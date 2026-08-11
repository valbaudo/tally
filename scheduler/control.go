package scheduler

import (
	"context"
	"sync"
	"sync/atomic"
)

// Execution is one asynchronously running workflow definition.
type Execution struct {
	control *runController
	done    chan struct{}

	mu     sync.Mutex
	result Result
}

// Cancel records the run's single external cancellation intent. It is
// idempotent and begins cooperative cancellation of active descendants.
func (e *Execution) Cancel() {
	if e == nil || e.control == nil {
		return
	}
	e.control.cancel(context.Canceled)
}

// ForceStop records external cancellation and immediately asks active leaf
// executions to stop. It is idempotent and stronger than Cancel.
func (e *Execution) ForceStop() {
	if e == nil || e.control == nil {
		return
	}
	e.control.forceStop(context.Canceled)
}

// Wait blocks until the complete run has quiesced and returns its immutable
// terminal result. Repeated and concurrent calls return the same result.
func (e *Execution) Wait() Result {
	if e == nil || e.done == nil {
		return Result{}
	}
	<-e.done
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.result
}

func (e *Execution) finish(result Result) {
	e.mu.Lock()
	e.result = result
	e.mu.Unlock()
	e.control.finish()
	close(e.done)
}

type runController struct {
	cancelContext context.CancelFunc
	cancelled     chan struct{}
	force         chan struct{}
	finished      chan struct{}
	root          *scopeTerminal

	cancelOnce sync.Once
	forceOnce  sync.Once
	finishOnce sync.Once
	mu         sync.Mutex
	external   error
	version    uint64
	epoch      uint64
	externalAt uint64

	beforeBodyOutcomeClaim    func(context.Context, Path)
	beforeTerminalClaim       func(context.Context, Path)
	graphCancellationObserved func(Path)
}

func newRunController(cancel context.CancelFunc) *runController {
	control := &runController{
		cancelContext: cancel,
		cancelled:     make(chan struct{}),
		force:         make(chan struct{}),
		finished:      make(chan struct{}),
	}
	control.root = &scopeTerminal{control: control}
	return control
}

func (c *runController) cancel(err error) {
	if c == nil {
		return
	}
	c.cancelOnce.Do(func() {
		if err == nil {
			err = context.Canceled
		}
		c.mu.Lock()
		c.external = err
		c.version++
		c.epoch++
		c.externalAt = c.epoch
		c.mu.Unlock()
		c.cancelContext()
		close(c.cancelled)
	})
}

func (c *runController) forceStop(err error) {
	if c == nil {
		return
	}
	c.cancel(err)
	c.forceOnce.Do(func() {
		// A force request is a new cancellation event. In particular, cleanup
		// deliberately ignores a cancellation captured while unwinding began,
		// but a later force request must still interrupt that cleanup.
		c.mu.Lock()
		c.version++
		c.epoch++
		c.externalAt = c.epoch
		c.mu.Unlock()
		close(c.force)
	})
}

func (c *runController) rootCancellationView() cancellationView {
	return cancellationView{control: c}
}

func (c *runController) finish() {
	c.finishOnce.Do(func() { close(c.finished) })
}

// scopeTerminal serializes cancellation with one scope's terminal transition.
// All state is protected by the owning run controller's mutex; no claim keeps
// that mutex while calling a Boundary implementation.
type scopeTerminal struct {
	control             *runController
	parent              *scopeTerminal
	watch               *scopeTerminal
	after               uint64
	cancelledAt         uint64
	terminal            bool
	claimedCancellation scopeCancellation
}

type scopeCancellation struct {
	external error
	parent   bool
}

type scopeSnapshot struct {
	epoch             uint64
	externalVersion   uint64
	parentCancelled   bool
	externalCancelled bool
}

type terminalClaim struct {
	external error
}

func (c *runController) childScope(parent *scopeTerminal) *scopeTerminal {
	after := uint64(0)
	if parent != nil {
		after = parent.after
	}
	return &scopeTerminal{control: c, parent: parent, after: after}
}

func (c *runController) cleanupScope(protected *scopeTerminal, after uint64) *scopeTerminal {
	return &scopeTerminal{control: c, watch: protected, after: after}
}

func (s *scopeTerminal) cancelParent() bool {
	if s == nil || s.control == nil {
		return false
	}
	c := s.control
	c.mu.Lock()
	defer c.mu.Unlock()
	if s.terminal || s.cancelledAt != 0 {
		return false
	}
	c.epoch++
	s.cancelledAt = c.epoch
	return true
}

func (s *scopeTerminal) cancellationLocked() scopeCancellation {
	if s == nil || s.control == nil {
		return scopeCancellation{}
	}
	cancellation := scopeCancellation{}
	if s.control.externalAt > s.after {
		cancellation.external = s.control.external
	}
	for current := s; current != nil; current = current.parent {
		if current.cancelledAt > s.after {
			cancellation.parent = true
		}
		for watched := current.watch; watched != nil; watched = watched.parent {
			if watched.cancelledAt > current.after {
				cancellation.parent = true
			}
		}
	}
	return cancellation
}

func (s *scopeTerminal) capture(path Path, result Result) (Result, scopeSnapshot) {
	if s == nil || s.control == nil {
		return result, scopeSnapshot{}
	}
	c := s.control
	c.mu.Lock()
	cancellation := s.cancellationLocked()
	snapshot := scopeSnapshot{
		epoch: c.epoch, externalVersion: c.version,
		parentCancelled: cancellation.parent, externalCancelled: cancellation.external != nil,
	}
	result = applyScopeCancellation(result, path, cancellation)
	c.mu.Unlock()
	return result, snapshot
}

func (s *scopeTerminal) claim(path Path, result Result) (Result, terminalClaim) {
	if s == nil || s.control == nil {
		return result, terminalClaim{}
	}
	c := s.control
	c.mu.Lock()
	cancellation := s.cancellationLocked()
	s.terminal = true
	s.claimedCancellation = cancellation
	result = applyScopeCancellation(result, path, cancellation)
	c.mu.Unlock()
	return result, terminalClaim{external: cancellation.external}
}

func applyScopeCancellation(result Result, path Path, cancellation scopeCancellation) Result {
	if cancellation.external == nil && !cancellation.parent {
		return result
	}
	causes := resultDiagnostics(result)
	if cancellation.external != nil {
		return normalize(causes, cancellation.external)
	}
	// An ancestor cancellation revokes an unclaimed success. Once a scope has
	// an intrinsic non-success cause, that cause already explains why it did
	// not publish; adding a scheduling-dependent cancellation diagnostic would
	// make quiescent arbitration depend on which completion was consumed first.
	if result.Status() != Succeeded {
		return result
	}
	return resultFrom(parentCancelled(path), nil)
}

// cleanupContext preserves execution values without inheriting a body
// cancellation that was already recorded when unwinding began. A cancellation
// first recorded after this snapshot still interrupts cleanup, and force-stop
// always interrupts it.
func (c *runController) cleanupContext(body context.Context, protected *scopeTerminal, snapshot scopeSnapshot) (context.Context, context.CancelFunc, cancellationView, *scopeTerminal) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(body))
	scope := c.cleanupScope(protected, snapshot.epoch)
	c.mu.Lock()
	view := cancellationView{control: c, after: snapshot.externalVersion, scope: scope}
	parentAlreadyCancelled := snapshot.parentCancelled || snapshot.externalCancelled
	externalAlreadyCancelled := snapshot.externalCancelled
	if !parentAlreadyCancelled {
		view.parent = &parentCancellationFact{done: body.Done()}
	}
	interrupted := scope.cancellationLocked()
	c.mu.Unlock()

	if interrupted.external != nil || interrupted.parent || channelClosed(c.force) || (!externalAlreadyCancelled && channelClosed(c.cancelled)) {
		scope.cancelParent()
		cancel()
		return ctx, cancel, view, scope
	}
	var parentDone <-chan struct{}
	if !parentAlreadyCancelled {
		parentDone = body.Done()
	}
	var externalDone <-chan struct{}
	if !externalAlreadyCancelled {
		externalDone = c.cancelled
	}
	go func() {
		select {
		case <-parentDone:
			scope.cancelParent()
			view.observeParentCancellation()
			cancel()
		case <-externalDone:
			cancel()
		case <-c.force:
			scope.cancelParent()
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel, view, scope
}

// cancellationView exposes only external cancellation facts recorded after a
// semantic execution boundary. Cleanup snapshots the current generation so a
// historical protected-body cancellation cannot rewrite cleanup's own result.
type cancellationView struct {
	control *runController
	after   uint64
	parent  *parentCancellationFact
	scope   *scopeTerminal
}

type parentCancellationFact struct {
	done     <-chan struct{}
	observed atomic.Bool
}

func (v cancellationView) externalError() error {
	if v.control == nil {
		return nil
	}
	v.control.mu.Lock()
	defer v.control.mu.Unlock()
	if v.control.version <= v.after {
		return nil
	}
	return v.control.external
}

func (v cancellationView) observeParentCancellation() {
	if v.parent == nil || v.externalError() != nil {
		return
	}
	v.parent.observed.Store(true)
}

func (v cancellationView) parentCancellationObserved() bool {
	if v.scope != nil && v.scope.control != nil {
		v.scope.control.mu.Lock()
		claimed := v.scope.terminal && v.scope.claimedCancellation.parent && v.scope.claimedCancellation.external == nil
		v.scope.control.mu.Unlock()
		if claimed {
			return true
		}
	}
	if v.parent == nil {
		return false
	}
	if !v.parent.observed.Load() && channelClosed(v.parent.done) {
		v.observeParentCancellation()
	}
	return v.parent.observed.Load()
}

func channelClosed(channel <-chan struct{}) bool {
	select {
	case <-channel:
		return true
	default:
		return false
	}
}
