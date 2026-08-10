package scheduler

import (
	"context"
	"errors"
	"strconv"

	"github.com/valbaudo/dawn/value"
	"github.com/valbaudo/dawn/workflow"
)

func (r *runState) runLoop(ctx context.Context, path Path, loop workflow.Loop, input value.Value) Result {
	if err := loop.Inputs().Validate(input); err != nil {
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
		return r.settle(ctx, instance, normalize([]diagnostic{parentCancelled(path)}, r.control.externalError()))
	}

	var previous value.Value
	for iteration := 1; iteration <= loop.Maximum(); iteration++ {
		if ctx.Err() != nil {
			return r.settle(ctx, instance, normalize([]diagnostic{parentCancelled(path)}, r.control.externalError()))
		}
		iterationPath, pathErr := path.LoopIteration(uint64(iteration))
		if pathErr != nil {
			return r.settle(ctx, instance, resultFrom(failed(path, MechanicalFailure, pathErr), nil))
		}
		iterationValue, integerErr := value.NewInteger(strconv.Itoa(iteration))
		if integerErr != nil {
			return r.settle(ctx, instance, resultFrom(failed(iterationPath, MechanicalFailure, integerErr), nil))
		}
		fields := map[string]value.Value{
			"initial":   input,
			"iteration": iterationValue,
		}
		if previous.Valid() {
			fields["previous"] = previous
		}
		bodyInput, inputErr := objectValue(fields)
		if inputErr == nil {
			inputErr = loop.Body().Inputs().Validate(bodyInput)
		}
		if inputErr != nil {
			return r.settle(ctx, instance, resultFrom(failed(iterationPath, ContractFailure, inputErr), nil))
		}

		done := r.runNested(ctx, func(nestedContext context.Context) Result {
			return r.runGraph(nestedContext, iterationPath, loop.Body(), bodyInput)
		})
		result, ok := <-done
		if !ok || !result.Valid() {
			return r.settle(ctx, instance, resultFrom(failed(iterationPath, MechanicalFailure, errors.New("loop body returned a malformed result")), nil))
		}
		if result.Status() != Succeeded {
			return r.settle(ctx, instance, result)
		}
		if ctx.Err() != nil {
			return r.settle(ctx, instance, normalize([]diagnostic{parentCancelled(path)}, r.control.externalError()))
		}

		output, present := result.Output()
		if !present {
			return r.settle(ctx, instance, resultFrom(failed(iterationPath, MechanicalFailure, errors.New("successful loop body omitted output")), nil))
		}
		verdictValue, present := selectValue(output, loop.Termination())
		if !present {
			return r.settle(ctx, instance, resultFrom(failed(iterationPath, ContractFailure, errors.New("loop termination verdict is absent")), nil))
		}
		verdict, boolean := verdictValue.Boolean()
		if !boolean {
			return r.settle(ctx, instance, resultFrom(failed(iterationPath, ContractFailure, errors.New("loop termination verdict is not boolean")), nil))
		}
		if !verdict && iteration < loop.Maximum() {
			previous = output
			continue
		}

		if outputErr := loop.Outputs().Validate(output); outputErr != nil {
			return r.settle(ctx, instance, resultFrom(failed(path, ContractFailure, outputErr), nil))
		}
		if ctx.Err() != nil {
			return r.settle(ctx, instance, normalize([]diagnostic{parentCancelled(path)}, r.control.externalError()))
		}
		if commitErr := r.scheduler.boundary.Commit(ctx, instance, output); commitErr != nil {
			return r.settle(ctx, instance, resultFrom(cancellationAwareDiagnostic(ctx, path, MechanicalFailure, commitErr), nil))
		}
		committed, constructErr := NewSucceededResult(output)
		if constructErr != nil {
			return r.settle(ctx, instance, resultFrom(failed(path, MechanicalFailure, constructErr), nil))
		}
		return committed
	}

	return r.settle(ctx, instance, resultFrom(failed(path, MechanicalFailure, errors.New("compiled loop executed no iterations")), nil))
}
