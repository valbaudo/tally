package scheduler

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"unicode/utf8"

	"github.com/valbaudo/dawn/value"
	"github.com/valbaudo/dawn/workflow"
)

func (r *runState) runBranch(ctx context.Context, path Path, branch workflow.Branch, input value.Value) Result {
	if err := branch.Inputs().Validate(input); err != nil {
		return r.withExternalCancellation(resultFrom(failed(path, ContractFailure, err), nil))
	}
	instance, err := NewInstance(path)
	if err != nil {
		return r.withExternalCancellation(resultFrom(failed(path, MechanicalFailure, err), nil))
	}
	if err := r.scheduler.boundary.Enter(ctx, instance); err != nil {
		return r.withExternalCancellation(resultFrom(cancellationAwareDiagnostic(ctx, path, MechanicalFailure, err), nil))
	}
	if ctx.Err() != nil {
		return r.settle(ctx, instance, resultFrom(parentCancelled(path), nil))
	}

	caseName, err := resolveBranchCase(branch, input)
	if err != nil {
		return r.settle(ctx, instance, resultFrom(failed(path, ContractFailure, err), nil))
	}
	selected, found := findBranchCase(branch.Cases(), caseName)
	if !found {
		return r.settle(ctx, instance, resultFrom(failed(path, MechanicalFailure, fmt.Errorf("compiled branch has no selected case %q", caseName)), nil))
	}

	casePath := path.BranchCase(caseName)
	caseRun := r.childRun()
	done := caseRun.runNested(ctx, func(nestedContext context.Context) Result {
		return caseRun.runGraph(nestedContext, casePath, selected.Graph(), input)
	})
	result, ok := <-done
	if !ok || !result.Valid() {
		return r.settle(ctx, instance, resultFrom(failed(casePath, MechanicalFailure, errors.New("selected branch case returned a malformed result")), nil))
	}
	if result.Status() != Succeeded {
		return r.settle(ctx, instance, result)
	}

	output, ok := result.Output()
	if !ok {
		return r.settle(ctx, instance, resultFrom(failed(casePath, MechanicalFailure, errors.New("selected branch case omitted its output")), nil))
	}
	if err := branch.Outputs().Validate(output); err != nil {
		return r.settle(ctx, instance, resultFrom(failed(path, ContractFailure, err), nil))
	}
	return r.commit(ctx, instance, output)
}

func resolveBranchCase(branch workflow.Branch, input value.Value) (string, error) {
	selectorType, ok := branch.Inputs().Resolve(branch.Selector())
	if !ok {
		return "", fmt.Errorf("branch selector %q is not an input", branch.Selector())
	}
	selector, ok := selectValue(input, []string{branch.Selector()})
	if !ok {
		return "", fmt.Errorf("branch selector %q is absent", branch.Selector())
	}

	switch selectorType.Kind() {
	case value.BooleanKind:
		selected, ok := selector.Boolean()
		if !ok {
			return "", fmt.Errorf("branch selector %q is not boolean", branch.Selector())
		}
		if selected {
			return "true", nil
		}
		return "false", nil
	case value.EnumKind:
		ordinary, ok := scalarOrdinaryJSON(selector)
		if !ok {
			return "", fmt.Errorf("branch selector %q is not an ordinary scalar", branch.Selector())
		}
		for _, member := range selectorType.EnumValues() {
			materialized, err := value.MaterializeLiteral(member, selectorType)
			if err != nil {
				return "", fmt.Errorf("materialize branch enum member: %w", err)
			}
			if materialized.Kind() == selector.Kind() && bytes.Equal(member.Bytes(), ordinary) {
				return string(member.Bytes()), nil
			}
		}
		return "", fmt.Errorf("branch selector %q is not an enum member", branch.Selector())
	default:
		return "", fmt.Errorf("branch selector %q must be boolean or enum", branch.Selector())
	}
}

func scalarOrdinaryJSON(selected value.Value) ([]byte, bool) {
	switch selected.Kind() {
	case value.StringKind:
		text, _ := selected.Text()
		if !utf8.ValidString(text) {
			return nil, false
		}
		encoded, err := json.Marshal(text)
		return encoded, err == nil
	case value.IntegerKind, value.NumberKind:
		number, _ := selected.Number()
		return []byte(number), true
	case value.BooleanKind:
		boolean, _ := selected.Boolean()
		if boolean {
			return []byte("true"), true
		}
		return []byte("false"), true
	case value.NullKind:
		return []byte("null"), true
	default:
		return nil, false
	}
}

func findBranchCase(cases []workflow.Case, name string) (workflow.Case, bool) {
	for _, branchCase := range cases {
		if branchCase.Name() == name {
			return branchCase, true
		}
	}
	return workflow.Case{}, false
}
