package scheduler

import (
	"context"
	"errors"
	"time"

	"github.com/valbaudo/dawn/value"
)

var (
	errInvalidMapOccurrence = errors.New("map occurrence must be one-based")
	errInvalidLoopIteration = errors.New("loop iteration must be one-based")
)

// Status is the closed terminal outcome of one scheduler scope.
type Status uint8

const (
	Succeeded Status = iota + 1
	Rejected
	Failed
	Cancelled
)

// FailureKind classifies a failed outcome.
type FailureKind uint8

const (
	MechanicalFailure FailureKind = iota + 1
	ContractFailure
	TimeoutFailure
	CleanupFailure
)

// Diagnostic describes one immutable non-success cause.
type Diagnostic struct {
	path            Path
	status          Status
	failure         FailureKind
	err             error
	reason          string
	parentCancelled bool
	cleanup         bool
}

// diagnostic remains the scheduler's internal construction spelling.
type diagnostic = Diagnostic

// Path returns the stable path at which the cause occurred.
func (d Diagnostic) Path() Path { return d.path }

// Status returns the diagnostic's terminal outcome category.
func (d Diagnostic) Status() Status { return d.status }

// Failure returns the failure classification when d represents a failure.
func (d Diagnostic) Failure() (FailureKind, bool) {
	if d.status != Failed {
		return 0, false
	}
	return d.failure, true
}

// Error returns the underlying failure or cancellation error, when present.
func (d Diagnostic) Error() error { return d.err }

// Reason returns the scheduler-owned gate rejection reason, when d is rejected.
func (d Diagnostic) Reason() (string, bool) {
	if d.status != Rejected {
		return "", false
	}
	return d.reason, true
}

// Valid reports whether d is a closed well-formed non-success diagnostic.
func (d Diagnostic) Valid() bool {
	if !d.path.valid() {
		return false
	}
	switch d.status {
	case Failed:
		return validFailureKind(d.failure) && d.err != nil && d.reason == ""
	case Rejected:
		return d.failure == 0 && d.err == nil
	case Cancelled:
		return d.failure == 0 && d.err != nil && d.reason == ""
	default:
		return false
	}
}

// Result is an immutable closed terminal outcome. Only successful results can
// expose a committed output.
type Result struct {
	status     Status
	output     value.Value
	primary    Diagnostic
	hasPrimary bool
	secondary  []Diagnostic
}

// NewSucceededResult constructs a successful result with its committed output.
func NewSucceededResult(output value.Value) (Result, error) {
	if !output.Valid() {
		return Result{}, errors.New("successful result requires a valid output")
	}
	return Result{status: Succeeded, output: output}, nil
}

// Status returns r's closed terminal status.
func (r Result) Status() Status { return r.status }

// Output returns the committed output only for a successful result.
func (r Result) Output() (value.Value, bool) {
	if r.status != Succeeded {
		return value.Value{}, false
	}
	return r.output, true
}

// Primary returns the deterministic primary diagnostic for a non-success result.
func (r Result) Primary() (Diagnostic, bool) { return r.primary, r.hasPrimary }

// Secondary returns ordered secondary diagnostics as a defensive copy.
func (r Result) Secondary() []Diagnostic { return append([]Diagnostic(nil), r.secondary...) }

// Valid reports whether r is a closed well-formed result.
func (r Result) Valid() bool {
	if r.status == Succeeded {
		return r.output.Valid() && !r.hasPrimary && len(r.secondary) == 0
	}
	if r.status != Rejected && r.status != Failed && r.status != Cancelled {
		return false
	}
	if r.output.Valid() || !r.hasPrimary || !r.primary.Valid() || r.primary.status != r.status {
		return false
	}
	for _, diagnostic := range r.secondary {
		if !diagnostic.Valid() {
			return false
		}
	}
	return true
}

// Instance identifies one semantic execution instance.
type Instance struct{ path Path }

// NewInstance constructs an instance at path.
func NewInstance(path Path) (Instance, error) {
	if !path.valid() {
		return Instance{}, errors.New("invalid instance path")
	}
	return Instance{path: path}, nil
}

// Path returns the instance's stable semantic path.
func (i Instance) Path() Path { return i.path }

// Valid reports whether i is well formed.
func (i Instance) Valid() bool { return i.path.valid() }

// LeafRequest is the immutable resolved input for one external leaf.
type LeafRequest struct {
	instance Instance
	input    value.Value
}

// NewLeafRequest constructs a leaf request with a valid instance and input.
func NewLeafRequest(instance Instance, input value.Value) (LeafRequest, error) {
	if !instance.Valid() || !input.Valid() {
		return LeafRequest{}, errors.New("leaf request requires a valid instance and input")
	}
	return LeafRequest{instance: instance, input: input}, nil
}

// Instance returns the request's semantic instance.
func (r LeafRequest) Instance() Instance { return r.instance }

// Input returns the leaf's immutable resolved input.
func (r LeafRequest) Input() value.Value { return r.input }

// Valid reports whether r is well formed.
func (r LeafRequest) Valid() bool { return r.instance.Valid() && r.input.Valid() }

// LeafCompletion is a closed immutable runner response. It cannot represent a
// scheduler-owned rejection or a cleanup classification.
type LeafCompletion struct {
	status  Status
	output  value.Value
	failure FailureKind
	err     error
}

// NewLeafSucceeded constructs a successful external candidate.
func NewLeafSucceeded(output value.Value) (LeafCompletion, error) {
	if !output.Valid() {
		return LeafCompletion{}, errors.New("successful leaf completion requires a valid output")
	}
	return LeafCompletion{status: Succeeded, output: output}, nil
}

// NewLeafFailed constructs a mechanical or contract leaf failure.
func NewLeafFailed(kind FailureKind, err error) (LeafCompletion, error) {
	if (kind != MechanicalFailure && kind != ContractFailure) || err == nil {
		return LeafCompletion{}, errors.New("invalid leaf failure")
	}
	return LeafCompletion{status: Failed, failure: kind, err: err}, nil
}

// NewLeafTimeout constructs a timeout leaf failure.
func NewLeafTimeout(err error) (LeafCompletion, error) {
	if err == nil {
		return LeafCompletion{}, errors.New("timeout requires an error")
	}
	return LeafCompletion{status: Failed, failure: TimeoutFailure, err: err}, nil
}

// NewLeafCancelled constructs an intrinsic backend cancellation.
func NewLeafCancelled(err error) (LeafCompletion, error) {
	if err == nil {
		return LeafCompletion{}, errors.New("cancellation requires an error")
	}
	return LeafCompletion{status: Cancelled, err: err}, nil
}

// Status returns the completion's closed terminal status.
func (c LeafCompletion) Status() Status { return c.status }

// Output returns the candidate output only for a successful completion.
func (c LeafCompletion) Output() (value.Value, bool) {
	if c.status != Succeeded {
		return value.Value{}, false
	}
	return c.output, true
}

// FailureKind returns the failure classification, or zero for non-failures.
func (c LeafCompletion) FailureKind() FailureKind { return c.failure }

// Error returns the failure or cancellation error, when present.
func (c LeafCompletion) Error() error { return c.err }

// Valid reports whether c is a closed well-formed runner response.
func (c LeafCompletion) Valid() bool {
	switch c.status {
	case Succeeded:
		return c.output.Valid() && c.failure == 0 && c.err == nil
	case Failed:
		return !c.output.Valid() && (c.failure == MechanicalFailure || c.failure == ContractFailure || c.failure == TimeoutFailure) && c.err != nil
	case Cancelled:
		return !c.output.Valid() && c.failure == 0 && c.err != nil
	default:
		return false
	}
}

// LeafExecution is an in-flight leaf that can settle or be force-stopped.
type LeafExecution interface {
	Done() <-chan LeafCompletion
	ForceStop() error
}

// LeafRunner starts external leaves. Gate rejections never reach this port.
type LeafRunner interface {
	Start(context.Context, LeafRequest) (LeafExecution, error)
}

// Boundary publishes semantic lifecycle events. It intentionally exposes no
// persistence, retry, or adapter operations.
type Boundary interface {
	Enter(context.Context, Instance) error
	Commit(context.Context, Instance, value.Value) error
	Settle(context.Context, Instance, Result) error
}

// Policy is operator-owned runtime policy.
type Policy struct {
	Capacity          int
	CancellationGrace time.Duration
}

func validFailureKind(kind FailureKind) bool {
	return kind == MechanicalFailure || kind == ContractFailure || kind == TimeoutFailure || kind == CleanupFailure
}
