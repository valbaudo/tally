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
	scope  *scopeTerminal
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
	contextDone := ctx.Done()

	cancelActive := func() {
		for _, activeChild := range active {
			activeChild.scope.cancelParent()
			activeChild.cancel()
		}
	}

	launchReady := func() {
		for !failFast {
			index, ready := state.popReady()
			if !ready {
				return
			}
			nodeState := &state.nodes[index]
			if ctx.Err() != nil {
				failFast = true
				return
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
				state.markDone(index)
				causes = append(causes, failed(childPath, ContractFailure, buildErr))
				failFast = true
				cancelActive()
				continue
			}

			childContext, cancel := context.WithCancel(ctx)
			childRun := r.childRun()
			active[index] = activeNode{cancel: cancel, scope: childRun.scope}
			node := nodeState.node
			done := childRun.runNested(childContext, func(nestedContext context.Context) Result {
				return childRun.runNode(nestedContext, childPath, node, childInput)
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
				return r.finishGraph(ctx, path, graph, input, instance, state, normalize(causes, r.externalError()))
			}
			if state.allDone() {
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
			state.markDone(completion.index)
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
			if applyErr := state.applyChildOutput(completion.index, output); applyErr != nil {
				causes = append(causes, failed(path.AuthoredChild(nodeState.node.Name()), ContractFailure, applyErr))
				failFast = true
				cancelActive()
			}
		case <-contextDone:
			contextDone = nil
			if r.control.graphCancellationObserved != nil {
				r.control.graphCancellationObserved(path)
			}
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
	return r.withExternalCancellation(resultFrom(failed(path, MechanicalFailure, fmt.Errorf("unsupported node scope %q", node.ScopeKind())), nil))
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
	result, claim := r.claimTerminal(ctx, instance.Path(), result)
	return r.settleClaimed(ctx, instance, result, claim)
}

func (r *runState) settleClaimed(ctx context.Context, instance Instance, result Result, claim terminalClaim) Result {
	if err := r.scheduler.boundary.Settle(context.WithoutCancel(ctx), instance, result); err != nil {
		causes := resultDiagnostics(result)
		causes = append(causes, failed(instance.Path(), MechanicalFailure, err))
		return normalize(causes, claim.external)
	}
	return result
}

func (r *runState) commit(ctx context.Context, instance Instance, output value.Value) Result {
	result, err := NewSucceededResult(output)
	if err != nil {
		return r.settle(ctx, instance, resultFrom(failed(instance.Path(), MechanicalFailure, err), nil))
	}
	result, claim := r.claimTerminal(ctx, instance.Path(), result)
	if result.Status() != Succeeded {
		return r.settleClaimed(ctx, instance, result, claim)
	}
	if err := r.scheduler.boundary.Commit(context.WithoutCancel(ctx), instance, output); err != nil {
		failure := resultFrom(failed(instance.Path(), MechanicalFailure, err), nil)
		return r.settleClaimed(ctx, instance, failure, claim)
	}
	return result
}
