package scheduler

import (
	"context"
	"errors"
	"reflect"

	"github.com/valbaudo/dawn/value"
	"github.com/valbaudo/dawn/workflow"
)

// Scheduler executes canonical definitions with one runtime-wide external-leaf
// capacity limit.
type Scheduler struct {
	runner   LeafRunner
	boundary Boundary
	policy   Policy
	capacity chan struct{}
}

// New constructs a scheduler from its Task 3 ports and operator policy.
func New(runner LeafRunner, boundary Boundary, policy Policy) (*Scheduler, error) {
	if nilInterface(runner) || nilInterface(boundary) {
		return nil, errors.New("scheduler requires runner and boundary")
	}
	if policy.Capacity <= 0 {
		return nil, errors.New("scheduler capacity must be positive")
	}
	if policy.CancellationGrace < 0 {
		return nil, errors.New("scheduler cancellation grace must not be negative")
	}
	return &Scheduler{
		runner: runner, boundary: boundary, policy: policy,
		capacity: make(chan struct{}, policy.Capacity),
	}, nil
}

// Start validates the immutable root boundary before starting asynchronous
// execution. A rejected root input never enters the root boundary.
func (s *Scheduler) Start(ctx context.Context, definition workflow.Definition, input value.Value) (*Execution, error) {
	if ctx == nil {
		return nil, errors.New("scheduler start requires a context")
	}
	if s == nil || nilInterface(s.runner) || nilInterface(s.boundary) || s.policy.Capacity <= 0 || s.policy.CancellationGrace < 0 {
		return nil, errors.New("invalid scheduler dependencies")
	}
	root := definition.Root()
	if len(definition.Canonical()) == 0 || !root.Inputs().Valid() || !root.Outputs().Valid() {
		return nil, errors.New("scheduler start requires a compiled definition")
	}
	if err := root.Inputs().Validate(input); err != nil {
		return nil, err
	}

	runContext, cancel := context.WithCancel(context.WithoutCancel(ctx))
	control := newRunController(cancel)
	execution := &Execution{control: control, done: make(chan struct{})}
	run := &runState{scheduler: s, control: control}

	if err := ctx.Err(); err != nil {
		control.cancel(err)
	}
	go func() {
		select {
		case <-ctx.Done():
			control.cancel(ctx.Err())
		case <-control.finished:
		}
	}()
	go func() {
		result := run.runGraph(runContext, Path{}, root, input)
		execution.finish(result)
	}()
	return execution, nil
}

func nilInterface(value any) bool {
	if value == nil {
		return true
	}
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return reflected.IsNil()
	default:
		return false
	}
}

type runState struct {
	scheduler *Scheduler
	control   *runController
}

func (r *runState) runScope(ctx context.Context, path Path, scope workflow.Scope, input value.Value) (Result, bool) {
	switch scope.Kind() {
	case workflow.GraphScope:
		graph, present := scope.Graph()
		if !present {
			return Result{}, false
		}
		return r.runGraph(ctx, path, graph, input), true
	case workflow.BranchScope:
		branch, present := scope.Branch()
		if !present {
			return Result{}, false
		}
		return r.runBranch(ctx, path, branch, input), true
	case workflow.MapScope:
		mapped, present := scope.Map()
		if !present {
			return Result{}, false
		}
		return r.runMap(ctx, path, mapped, input), true
	case workflow.LoopScope:
		loop, present := scope.Loop()
		if !present {
			return Result{}, false
		}
		return r.runLoop(ctx, path, loop, input), true
	default:
		return Result{}, false
	}
}

func (r *runState) runNested(ctx context.Context, fn func(context.Context) Result) <-chan Result {
	done := make(chan Result, 1)
	go func() {
		done <- fn(ctx)
		close(done)
	}()
	return done
}

func cancellationAwareDiagnostic(ctx context.Context, path Path, kind FailureKind, err error) diagnostic {
	if ctx != nil && ctx.Err() != nil && errors.Is(err, ctx.Err()) {
		return parentCancelled(path)
	}
	return failed(path, kind, err)
}

func (r *runState) withExternalCancellation(result Result) Result {
	external := r.control.externalError()
	if external == nil || result.Status() == Succeeded {
		return result
	}
	return normalize(resultDiagnostics(result), external)
}
