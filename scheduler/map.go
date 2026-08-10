package scheduler

import (
	"context"
	"errors"
	"strconv"

	"github.com/valbaudo/dawn/value"
	"github.com/valbaudo/dawn/workflow"
)

type mapCompletion struct {
	index  int
	path   Path
	result Result
}

type activeMapItem struct {
	cancel context.CancelFunc
	path   Path
}

func (r *runState) runMap(ctx context.Context, path Path, mapped workflow.Map, input value.Value) Result {
	if err := mapped.Inputs().Validate(input); err != nil {
		return r.withExternalCancellation(resultFrom(failed(path, ContractFailure, err), nil))
	}
	instance, err := NewInstance(path)
	if err != nil {
		return r.withExternalCancellation(resultFrom(failed(path, MechanicalFailure, err), nil))
	}
	if err := r.scheduler.boundary.Enter(ctx, instance); err != nil {
		return r.withExternalCancellation(resultFrom(cancellationAwareDiagnostic(ctx, path, MechanicalFailure, err), nil))
	}

	collection, present := selectValue(input, []string{mapped.Collection()})
	if !present || collection.Kind() != value.ListKind {
		return r.settle(ctx, instance, resultFrom(failed(path, ContractFailure, errors.New("map collection is not a runtime list")), nil))
	}
	items := collection.Items()
	results := make([]value.Value, len(items))
	occurrences := make(map[string]uint64)
	completed := make(chan mapCompletion)
	active := make(map[int]activeMapItem)
	causes := make([]diagnostic, 0)
	failFast := false
	contextObserved := false

	cancelActive := func() {
		for _, activeItem := range active {
			activeItem.cancel()
		}
	}
	handleCompletion := func(completion mapCompletion) {
		activeItem, exists := active[completion.index]
		if !exists {
			return
		}
		delete(active, completion.index)
		activeItem.cancel()
		if comparePath(activeItem.path, completion.path) != 0 || !completion.result.Valid() {
			completion.result = resultFrom(failed(activeItem.path, MechanicalFailure, errors.New("map body returned a malformed result")), nil)
		}
		if completion.result.Status() != Succeeded {
			causes = append(causes, resultDiagnostics(completion.result)...)
			if !failFast {
				failFast = true
				cancelActive()
			}
			return
		}
		if failFast {
			return
		}
		output, ok := completion.result.Output()
		if !ok {
			causes = append(causes, failed(activeItem.path, MechanicalFailure, errors.New("successful map body omitted output")))
			failFast = true
			cancelActive()
			return
		}
		results[completion.index] = output
	}
	drainAvailable := func() {
		for {
			select {
			case completion := <-completed:
				handleCompletion(completion)
			default:
				return
			}
		}
	}

	for index, item := range items {
		drainAvailable()
		if failFast {
			break
		}
		if ctx.Err() != nil {
			contextObserved = true
			causes = append(causes, parentCancelled(path))
			failFast = true
			cancelActive()
			break
		}

		canonical := item.Canonical()
		if canonical == nil {
			causes = append(causes, failed(path, ContractFailure, errors.New("map item is invalid")))
			failFast = true
			cancelActive()
			break
		}
		key := string(canonical)
		occurrence := occurrences[key]
		occurrences[key] = occurrence + 1
		bodyPath, pathErr := path.MapItem(canonical, occurrence)
		if pathErr != nil {
			causes = append(causes, failed(path, MechanicalFailure, pathErr))
			failFast = true
			cancelActive()
			break
		}
		indexValue, indexErr := value.NewInteger(strconv.Itoa(index))
		if indexErr != nil {
			causes = append(causes, failed(bodyPath, MechanicalFailure, indexErr))
			failFast = true
			cancelActive()
			break
		}
		bodyInput, inputErr := objectValue(map[string]value.Value{"item": item, "index": indexValue})
		if inputErr == nil {
			inputErr = mapped.Body().Inputs().Validate(bodyInput)
		}
		if inputErr != nil {
			causes = append(causes, failed(bodyPath, ContractFailure, inputErr))
			failFast = true
			cancelActive()
			break
		}

		bodyContext, cancel := context.WithCancel(ctx)
		active[index] = activeMapItem{cancel: cancel, path: bodyPath}
		bodyIndex, nestedPath, nestedInput := index, bodyPath, bodyInput
		r.runNested(bodyContext, func(nestedContext context.Context) Result {
			result := r.runGraph(nestedContext, nestedPath, mapped.Body(), nestedInput)
			completed <- mapCompletion{index: bodyIndex, path: nestedPath, result: result}
			return result
		})
	}

	contextDone := ctx.Done()
	for len(active) > 0 {
		select {
		case completion := <-completed:
			handleCompletion(completion)
		case <-contextDone:
			contextDone = nil
			if !contextObserved {
				contextObserved = true
				causes = append(causes, parentCancelled(path))
			}
			if !failFast {
				failFast = true
				cancelActive()
			}
		}
	}
	if ctx.Err() != nil && !contextObserved {
		causes = append(causes, parentCancelled(path))
		failFast = true
	}

	if failFast || len(causes) > 0 {
		return r.settle(ctx, instance, normalize(causes, r.control.externalError()))
	}
	output, outputErr := objectValue(map[string]value.Value{mapped.Result(): value.NewList(results...)})
	if outputErr == nil {
		outputErr = mapped.Outputs().Validate(output)
	}
	if outputErr != nil {
		return r.settle(ctx, instance, resultFrom(failed(path, ContractFailure, outputErr), nil))
	}
	if commitErr := r.scheduler.boundary.Commit(ctx, instance, output); commitErr != nil {
		return r.settle(ctx, instance, resultFrom(cancellationAwareDiagnostic(ctx, path, MechanicalFailure, commitErr), nil))
	}
	result, constructErr := NewSucceededResult(output)
	if constructErr != nil {
		return r.settle(ctx, instance, resultFrom(failed(path, MechanicalFailure, constructErr), nil))
	}
	return result
}
