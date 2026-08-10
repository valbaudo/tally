package scheduler

import (
	"context"
	"errors"
	"fmt"

	"github.com/valbaudo/dawn/value"
	"github.com/valbaudo/dawn/workflow"
)

type graphCompletion struct {
	index  int
	result Result
}

type activeNode struct {
	cancel context.CancelFunc
}

func (r *runState) runGraph(ctx context.Context, path Path, graph workflow.Graph, input value.Value) Result {
	if err := graph.Inputs().Validate(input); err != nil {
		return r.withExternalCancellation(resultFrom(failed(path, ContractFailure, err), nil))
	}
	instance, err := NewInstance(path)
	if err != nil {
		return r.withExternalCancellation(resultFrom(failed(path, MechanicalFailure, err), nil))
	}
	if err := r.scheduler.boundary.Enter(ctx, instance); err != nil {
		return r.withExternalCancellation(resultFrom(cancellationAwareDiagnostic(ctx, path, MechanicalFailure, err), nil))
	}
	state, err := applyGraphInputs(graph, input)
	if err != nil {
		return r.finishGraph(ctx, path, graph, input, instance, state, resultFrom(failed(path, ContractFailure, err), nil))
	}

	completed := make(chan graphCompletion)
	active := make(map[int]activeNode)
	causes := make([]diagnostic, 0)
	failFast := false
	contextObserved := false

	cancelActive := func() {
		for _, activeChild := range active {
			activeChild.cancel()
		}
	}

	launchReady := func() {
		for index := range state.nodes {
			nodeState := &state.nodes[index]
			if failFast || nodeState.started || nodeState.done || nodeState.remaining != 0 {
				continue
			}
			if ctx.Err() != nil {
				failFast = true
				break
			}
			nodeState.started = true
			childPath := path.AuthoredChild(nodeState.node.Name())
			childInput, buildErr := state.childInput(index)
			if buildErr == nil {
				contract, ok := nodeInputContract(nodeState.node)
				if !ok {
					buildErr = errors.New("node has no executable input contract")
				} else {
					buildErr = contract.Validate(childInput)
				}
			}
			if buildErr != nil {
				nodeState.done = true
				causes = append(causes, failed(childPath, ContractFailure, buildErr))
				failFast = true
				cancelActive()
				continue
			}

			childContext, cancel := context.WithCancel(ctx)
			active[index] = activeNode{cancel: cancel}
			node := nodeState.node
			done := r.runNested(childContext, func(nestedContext context.Context) Result {
				return r.runNode(nestedContext, childPath, node, childInput)
			})
			go func(index int, done <-chan Result) {
				result, ok := <-done
				if !ok {
					result = resultFrom(failed(childPath, MechanicalFailure, errors.New("nested execution closed without a result")), nil)
				}
				completed <- graphCompletion{index: index, result: result}
			}(index, done)
		}
	}

	for {
		launchReady()
		if len(active) == 0 {
			if failFast || ctx.Err() != nil {
				if ctx.Err() != nil && !contextObserved {
					causes = append(causes, parentCancelled(path))
				}
				return r.finishGraph(ctx, path, graph, input, instance, state, normalize(causes, r.control.externalError()))
			}
			allDone := true
			for _, node := range state.nodes {
				if !node.done {
					allDone = false
					break
				}
			}
			if allDone {
				output, outputErr := objectValue(state.outputs)
				if outputErr == nil {
					outputErr = graph.Outputs().Validate(output)
				}
				if outputErr != nil {
					return r.finishGraph(ctx, path, graph, input, instance, state, resultFrom(failed(path, ContractFailure, outputErr), nil))
				}
				result, constructErr := NewSucceededResult(output)
				if constructErr != nil {
					return r.finishGraph(ctx, path, graph, input, instance, state, resultFrom(failed(path, MechanicalFailure, constructErr), nil))
				}
				return r.finishGraph(ctx, path, graph, input, instance, state, result)
			}
			return r.finishGraph(ctx, path, graph, input, instance, state, resultFrom(failed(path, MechanicalFailure, errors.New("graph scheduler stalled")), nil))
		}

		select {
		case completion := <-completed:
			activeChild, exists := active[completion.index]
			if !exists {
				continue
			}
			delete(active, completion.index)
			activeChild.cancel()
			nodeState := &state.nodes[completion.index]
			nodeState.done = true
			if !completion.result.Valid() {
				completion.result = resultFrom(failed(path.AuthoredChild(nodeState.node.Name()), MechanicalFailure, errors.New("nested execution returned malformed result")), nil)
			}
			if completion.result.Status() != Succeeded {
				causes = append(causes, resultDiagnostics(completion.result)...)
				if !failFast {
					failFast = true
					cancelActive()
				}
				continue
			}
			output, ok := completion.result.Output()
			if !ok {
				causes = append(causes, failed(path.AuthoredChild(nodeState.node.Name()), MechanicalFailure, errors.New("successful child omitted output")))
				failFast = true
				cancelActive()
				continue
			}
			state.recordCommittedChild(nodeState.node.Name(), output)
			if failFast {
				continue
			}
			if applyErr := state.applyChildOutput(nodeState.node.Name(), output); applyErr != nil {
				causes = append(causes, failed(path.AuthoredChild(nodeState.node.Name()), ContractFailure, applyErr))
				failFast = true
				cancelActive()
			}
		case <-ctx.Done():
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
}

func (r *runState) runNode(ctx context.Context, path Path, node workflow.Node, input value.Value) Result {
	if leaf, ok := node.Leaf(); ok {
		if leaf.Kind() == workflow.Gate {
			return r.runGate(ctx, path, leaf, input)
		}
		return r.runLeaf(ctx, path, leaf, input)
	}
	if scope, ok := node.Scope(); ok {
		if result, handled := r.runScope(ctx, path, scope, input); handled {
			return result
		}
	}
	return resultFrom(failed(path, MechanicalFailure, fmt.Errorf("unsupported node scope %q", node.ScopeKind())), nil)
}

func resultDiagnostics(result Result) []diagnostic {
	primary, ok := result.Primary()
	if !ok {
		return nil
	}
	diagnostics := []diagnostic{primary}
	diagnostics = append(diagnostics, result.Secondary()...)
	return diagnostics
}

func (r *runState) settle(ctx context.Context, instance Instance, result Result) Result {
	result = r.withExternalCancellation(result)
	if err := r.scheduler.boundary.Settle(context.WithoutCancel(ctx), instance, result); err != nil {
		causes := resultDiagnostics(result)
		causes = append(causes, failed(instance.Path(), MechanicalFailure, err))
		return normalize(causes, r.control.externalError())
	}
	return result
}
