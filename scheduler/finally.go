package scheduler

import (
	"context"
	"errors"
	"fmt"

	"github.com/valbaudo/dawn/value"
	"github.com/valbaudo/dawn/workflow"
)

func (r *runState) finishGraph(ctx context.Context, path Path, graph workflow.Graph, protectedInput value.Value, instance Instance, state *graphState, body Result) Result {
	result := body
	if cleanup, present := graph.Finally(); present {
		cleanupPath := path.Cleanup()
		cleanupInput, inputErr := assembleCleanupInput(cleanup, protectedInput, state, body.Status())
		if inputErr != nil {
			result = applyCleanupPrecedence(body, resultFrom(cleanupFailed(cleanupPath, inputErr), nil), cleanupPath)
		} else {
			cleanupContext, cancel := r.control.cleanupContext(ctx)
			done := r.runNested(cleanupContext, func(nestedContext context.Context) Result {
				return r.runGraph(nestedContext, cleanupPath, cleanup.Graph(), cleanupInput)
			})
			cleanupResult, open := <-done
			cancel()
			if !open || !cleanupResult.Valid() {
				cleanupResult = resultFrom(cleanupFailed(cleanupPath, errors.New("cleanup graph returned a malformed result")), nil)
			}
			result = applyCleanupPrecedence(body, cleanupResult, cleanupPath)
		}
	}

	if result.Status() != Succeeded {
		return r.settle(ctx, instance, result)
	}
	if ctx.Err() != nil {
		return r.settle(ctx, instance, normalize([]diagnostic{parentCancelled(path)}, r.control.externalError()))
	}
	output, present := result.Output()
	if !present {
		return r.settle(ctx, instance, resultFrom(failed(path, MechanicalFailure, errors.New("successful graph omitted its output")), nil))
	}
	if err := r.scheduler.boundary.Commit(ctx, instance, output); err != nil {
		return r.settle(ctx, instance, resultFrom(cancellationAwareDiagnostic(ctx, path, MechanicalFailure, err), nil))
	}
	committed, err := NewSucceededResult(output)
	if err != nil {
		return r.settle(ctx, instance, resultFrom(failed(path, MechanicalFailure, err), nil))
	}
	return committed
}

func assembleCleanupInput(cleanup workflow.Finally, protectedInput value.Value, state *graphState, status Status) (value.Value, error) {
	fields := make(map[string]value.Value, len(cleanup.Bindings()))
	for _, binding := range cleanup.Bindings() {
		source := binding.From()
		var selected value.Value
		var present bool
		switch source.Kind() {
		case workflow.CleanupInput:
			selected, present = selectValue(protectedInput, source.Path())
		case workflow.CleanupOutcome:
			outcome, err := cleanupOutcome(status)
			if err != nil {
				return value.Value{}, err
			}
			selected, present = outcome, true
		case workflow.CleanupChild:
			if committed, available := state.committedChild(source.Child()); available {
				selected, present = selectValue(committed, source.Path())
			}
		default:
			return value.Value{}, fmt.Errorf("unknown cleanup source kind %d", source.Kind())
		}
		if present {
			fields[binding.To()] = selected
		}
	}
	input, err := objectValue(fields)
	if err != nil {
		return value.Value{}, fmt.Errorf("assemble cleanup input: %w", err)
	}
	if err := cleanup.Graph().Inputs().Validate(input); err != nil {
		return value.Value{}, fmt.Errorf("validate cleanup input: %w", err)
	}
	return input, nil
}

func cleanupOutcome(status Status) (value.Value, error) {
	switch status {
	case Succeeded:
		return value.NewString("succeeded"), nil
	case Rejected:
		return value.NewString("rejected"), nil
	case Failed:
		return value.NewString("failed"), nil
	case Cancelled:
		return value.NewString("cancelled"), nil
	default:
		return value.Value{}, fmt.Errorf("invalid protected outcome %d", status)
	}
}

func applyCleanupPrecedence(body, cleanup Result, cleanupPath Path) Result {
	if cleanup.Status() == Succeeded {
		return body
	}
	failure := cleanupFailed(cleanupPath, cleanupResultError(cleanup))
	if body.Status() == Succeeded {
		return resultFrom(failure, nil)
	}
	primary, present := body.Primary()
	if !present {
		return resultFrom(failure, nil)
	}
	secondary := append(body.Secondary(), failure)
	sortDiagnostics(secondary)
	return resultFrom(primary, secondary)
}

func cleanupResultError(result Result) error {
	primary, present := result.Primary()
	if !present {
		return errors.New("cleanup ended without a terminal diagnostic")
	}
	if err := primary.Error(); err != nil {
		return fmt.Errorf("cleanup failed: %w", err)
	}
	if reason, ok := primary.Reason(); ok {
		return fmt.Errorf("cleanup rejected: %s", reason)
	}
	return fmt.Errorf("cleanup ended with status %d", result.Status())
}
