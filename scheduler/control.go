package scheduler

import (
	"context"
	"sync"
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

	cancelOnce sync.Once
	forceOnce  sync.Once
	finishOnce sync.Once
	mu         sync.Mutex
	external   error
}

func newRunController(cancel context.CancelFunc) *runController {
	return &runController{
		cancelContext: cancel,
		cancelled:     make(chan struct{}),
		force:         make(chan struct{}),
		finished:      make(chan struct{}),
	}
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
	c.forceOnce.Do(func() { close(c.force) })
}

func (c *runController) externalError() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.external
}

func (c *runController) finish() {
	c.finishOnce.Do(func() { close(c.finished) })
}

// cleanupContext preserves execution values without inheriting a body
// cancellation that was already recorded when unwinding began. A cancellation
// first recorded after this snapshot still interrupts cleanup, and force-stop
// always interrupts it.
func (c *runController) cleanupContext(body context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.WithoutCancel(body))
	c.mu.Lock()
	previouslyCancelled := c.external != nil
	c.mu.Unlock()

	if channelClosed(c.force) || (!previouslyCancelled && channelClosed(c.cancelled)) {
		cancel()
		return ctx, cancel
	}
	if previouslyCancelled {
		go func() {
			select {
			case <-c.force:
				cancel()
			case <-ctx.Done():
			}
		}()
		return ctx, cancel
	}
	go func() {
		select {
		case <-c.cancelled:
			cancel()
		case <-c.force:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

func channelClosed(channel <-chan struct{}) bool {
	select {
	case <-channel:
		return true
	default:
		return false
	}
}
