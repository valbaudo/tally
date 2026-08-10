package scheduler

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/valbaudo/dawn/value"
	"github.com/valbaudo/dawn/workflow"
)

func (r *runState) runLeaf(ctx context.Context, path Path, leaf workflow.Leaf, input value.Value) Result {
	if err := leaf.Inputs().Validate(input); err != nil {
		return resultFrom(failed(path, ContractFailure, err), nil)
	}
	instance, err := NewInstance(path)
	if err != nil {
		return resultFrom(failed(path, MechanicalFailure, err), nil)
	}
	if err := r.scheduler.boundary.Enter(ctx, instance); err != nil {
		return resultFrom(cancellationAwareDiagnostic(ctx, path, MechanicalFailure, err), nil)
	}

	select {
	case r.scheduler.capacity <- struct{}{}:
		defer func() { <-r.scheduler.capacity }()
	case <-ctx.Done():
		return r.settle(ctx, instance, resultFrom(parentCancelled(path), nil))
	case <-r.control.force:
		return r.settle(ctx, instance, resultFrom(parentCancelled(path), nil))
	}
	if ctx.Err() != nil {
		return r.settle(ctx, instance, resultFrom(parentCancelled(path), nil))
	}

	leafContext, cancel := context.WithCancel(ctx)
	defer cancel()
	request, err := NewLeafRequest(instance, input)
	if err != nil {
		return r.settle(ctx, instance, resultFrom(failed(path, MechanicalFailure, err), nil))
	}
	execution, err := r.scheduler.runner.Start(leafContext, request)
	if err != nil {
		return r.settle(ctx, instance, resultFrom(cancellationAwareDiagnostic(ctx, path, MechanicalFailure, err), nil))
	}
	if nilInterface(execution) || execution.Done() == nil {
		return r.settle(ctx, instance, resultFrom(failed(path, MechanicalFailure, errors.New("leaf runner returned an invalid execution")), nil))
	}

	for {
		select {
		case completion, ok := <-execution.Done():
			return r.completeLeaf(ctx, instance, leaf, completion, ok)
		case <-ctx.Done():
			return r.stopLeaf(ctx, instance, leaf, execution)
		case <-r.control.force:
			return r.forceLeaf(ctx, instance, leaf, execution)
		}
	}
}

func (r *runState) stopLeaf(ctx context.Context, instance Instance, leaf workflow.Leaf, execution LeafExecution) Result {
	if completion, ok, ready := availableCompletion(execution.Done()); ready {
		return r.completeLeaf(ctx, instance, leaf, completion, ok)
	}
	timer := time.NewTimer(r.scheduler.policy.CancellationGrace)
	defer timer.Stop()
	select {
	case completion, ok := <-execution.Done():
		return r.completeLeaf(ctx, instance, leaf, completion, ok)
	case <-timer.C:
		return r.forceLeaf(ctx, instance, leaf, execution)
	case <-r.control.force:
		return r.forceLeaf(ctx, instance, leaf, execution)
	}
}

func (r *runState) forceLeaf(ctx context.Context, instance Instance, leaf workflow.Leaf, execution LeafExecution) Result {
	if completion, ok, ready := availableCompletion(execution.Done()); ready {
		return r.completeLeaf(ctx, instance, leaf, completion, ok)
	}
	if err := execution.ForceStop(); err != nil {
		completion, channelOpen := <-execution.Done()
		result := r.leafCompletion(ctx, instance, leaf, completion, channelOpen)
		consequence := failed(instance.Path(), MechanicalFailure, fmt.Errorf("force stop leaf: %w", err))
		consequence.parentCancelled = true
		result = appendSecondaryConsequence(result, consequence)
		if result.Status() == Succeeded {
			return result
		}
		return r.settle(ctx, instance, result)
	}
	return r.settle(ctx, instance, resultFrom(parentCancelled(instance.Path()), nil))
}

func availableCompletion(done <-chan LeafCompletion) (LeafCompletion, bool, bool) {
	select {
	case completion, ok := <-done:
		return completion, ok, true
	default:
		return LeafCompletion{}, false, false
	}
}

func (r *runState) completeLeaf(ctx context.Context, instance Instance, leaf workflow.Leaf, completion LeafCompletion, channelOpen bool) Result {
	result := r.leafCompletion(ctx, instance, leaf, completion, channelOpen)
	if result.Status() == Succeeded {
		return result
	}
	return r.settle(ctx, instance, result)
}

func (r *runState) leafCompletion(ctx context.Context, instance Instance, leaf workflow.Leaf, completion LeafCompletion, channelOpen bool) Result {
	path := instance.Path()
	if !channelOpen || !completion.Valid() {
		return resultFrom(failed(path, MechanicalFailure, errors.New("leaf runner returned a malformed completion")), nil)
	}
	switch completion.Status() {
	case Succeeded:
		output, _ := completion.Output()
		if !leaf.Outputs().Valid() {
			return resultFrom(failed(path, MechanicalFailure, errors.New("leaf completion lost its output contract")), nil)
		}
		if err := leaf.Outputs().Validate(output); err != nil {
			return resultFrom(failed(path, ContractFailure, err), nil)
		}
		if err := r.scheduler.boundary.Commit(ctx, instance, output); err != nil {
			return resultFrom(cancellationAwareDiagnostic(ctx, path, MechanicalFailure, err), nil)
		}
		result, err := NewSucceededResult(output)
		if err != nil {
			return resultFrom(failed(path, MechanicalFailure, err), nil)
		}
		return result
	case Failed:
		return resultFrom(failed(path, completion.FailureKind(), completion.Error()), nil)
	case Cancelled:
		if ctx.Err() != nil {
			return resultFrom(parentCancelled(path), nil)
		}
		return resultFrom(diagnostic{path: path, status: Cancelled, err: completion.Error()}, nil)
	default:
		return resultFrom(failed(path, MechanicalFailure, errors.New("leaf runner returned an unsupported completion")), nil)
	}
}

func (r *runState) runGate(ctx context.Context, path Path, leaf workflow.Leaf, input value.Value) Result {
	if err := leaf.Inputs().Validate(input); err != nil {
		return resultFrom(failed(path, ContractFailure, err), nil)
	}
	instance, err := NewInstance(path)
	if err != nil {
		return resultFrom(failed(path, MechanicalFailure, err), nil)
	}
	if err := r.scheduler.boundary.Enter(ctx, instance); err != nil {
		return resultFrom(cancellationAwareDiagnostic(ctx, path, MechanicalFailure, err), nil)
	}
	if ctx.Err() != nil {
		return r.settle(ctx, instance, resultFrom(parentCancelled(path), nil))
	}
	passedValue, ok := selectValue(input, []string{"passed"})
	if !ok {
		return r.settle(ctx, instance, resultFrom(failed(path, ContractFailure, errors.New("gate passed input is absent")), nil))
	}
	passed, ok := passedValue.Boolean()
	if !ok {
		return r.settle(ctx, instance, resultFrom(failed(path, ContractFailure, errors.New("gate passed input is not boolean")), nil))
	}
	if !passed {
		reason := ""
		if reasonValue, present := selectValue(input, []string{"reason"}); present {
			reason, _ = reasonValue.Text()
		}
		return r.settle(ctx, instance, resultFrom(rejected(path, reason), nil))
	}
	output, err := objectValue(nil)
	if err != nil {
		return r.settle(ctx, instance, resultFrom(failed(path, MechanicalFailure, err), nil))
	}
	if err := r.scheduler.boundary.Commit(ctx, instance, output); err != nil {
		return r.settle(ctx, instance, resultFrom(cancellationAwareDiagnostic(ctx, path, MechanicalFailure, err), nil))
	}
	result, err := NewSucceededResult(output)
	if err != nil {
		return r.settle(ctx, instance, resultFrom(failed(path, MechanicalFailure, err), nil))
	}
	return result
}

func appendSecondaryConsequence(result Result, consequence diagnostic) Result {
	if result.Status() == Succeeded || !consequence.Valid() {
		return result
	}
	result.secondary = append(result.Secondary(), consequence)
	sortDiagnostics(result.secondary)
	return result
}
