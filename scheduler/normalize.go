package scheduler

import (
	"context"
	"errors"
	"sort"
)

func failed(path Path, kind FailureKind, err error) diagnostic {
	if !validFailureKind(kind) || err == nil {
		return diagnostic{}
	}
	return diagnostic{path: path, status: Failed, failure: kind, err: err, cleanup: kind == CleanupFailure}
}

func cleanupFailed(path Path, err error) diagnostic { return failed(path, CleanupFailure, err) }

func rejected(path Path, reason string) diagnostic {
	return diagnostic{path: path, status: Rejected, reason: reason}
}

func intrinsicCancelled(path Path) diagnostic {
	return diagnostic{path: path, status: Cancelled, err: context.Canceled}
}

func parentCancelled(path Path) diagnostic {
	return diagnostic{path: path, status: Cancelled, err: context.Canceled, parentCancelled: true}
}

func externalCancelled(err error) diagnostic {
	if err == nil {
		err = context.Canceled
	}
	return diagnostic{status: Cancelled, err: err}
}

// normalize produces a deterministic outcome after all concurrent causes have
// quiesced. Parent-induced cancellations remain secondary when an intrinsic
// cause exists; an external cancellation always wins.
func normalize(causes []diagnostic, external error) Result {
	valid := make([]diagnostic, 0, len(causes))
	for _, cause := range causes {
		if cause.Valid() {
			valid = append(valid, cause)
			continue
		}
		valid = append(valid, failed(Path{}, MechanicalFailure, errors.New("malformed scheduler diagnostic")))
	}
	sortDiagnostics(valid)

	if external != nil {
		return resultFrom(externalCancelled(external), valid)
	}
	if len(valid) == 0 {
		return resultFrom(failed(Path{}, MechanicalFailure, errors.New("missing terminal cause")), nil)
	}

	primaryIndex := 0
	for i, cause := range valid {
		if !cause.parentCancelled {
			primaryIndex = i
			break
		}
	}
	primary := valid[primaryIndex]
	secondary := make([]diagnostic, 0, len(valid)-1)
	secondary = append(secondary, valid[:primaryIndex]...)
	secondary = append(secondary, valid[primaryIndex+1:]...)
	return resultFrom(primary, secondary)
}

func resultFrom(primary diagnostic, secondary []diagnostic) Result {
	return Result{
		status:     primary.status,
		primary:    primary,
		hasPrimary: true,
		secondary:  append([]diagnostic(nil), secondary...),
	}
}

func sortDiagnostics(diagnostics []diagnostic) {
	sort.SliceStable(diagnostics, func(i, j int) bool {
		left, right := diagnostics[i], diagnostics[j]
		if leftRank, rightRank := diagnosticRank(left), diagnosticRank(right); leftRank != rightRank {
			return leftRank < rightRank
		}
		return comparePath(left.path, right.path) < 0
	})
}

func diagnosticRank(diagnostic diagnostic) int {
	switch {
	case diagnostic.cleanup:
		return 4
	case diagnostic.parentCancelled:
		return 5
	case diagnostic.status == Failed:
		return 1
	case diagnostic.status == Rejected:
		return 2
	case diagnostic.status == Cancelled:
		return 3
	default:
		return 6
	}
}
