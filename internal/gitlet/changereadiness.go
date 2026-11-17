package gitlet

import "slices"

// A LISTING OF CHANGES BY READINESS keeps the open changes standing in one bucket: ready ones walk
// their stored answer (replicated "The changes ready to merge are listed"), the rest the open walk.

// ReadinessBucket is where an open change's merge stands, as a listing narrows to it; a closed set
// ([ReadinessBuckets]). Waiting on a person is [NeedPerson].
type ReadinessBucket string

const (
	// BucketReady is a change whose every precondition but its merger's authority is met.
	BucketReady ReadinessBucket = "ready"
	// BucketApproval is a change standing at an approval, a person's or its path owners'.
	BucketApproval ReadinessBucket = "approval"
	// BucketChecks is a change standing at its target's required checks.
	BucketChecks ReadinessBucket = "checks"
	// BucketBlockers is a change standing at a blocker that stands.
	BucketBlockers ReadinessBucket = "blockers"
)

// ReadinessBuckets is the closed set, in this order.
func ReadinessBuckets() []ReadinessBucket {
	return []ReadinessBucket{BucketReady, BucketApproval, BucketChecks, BucketBlockers}
}

// Valid reports a bucket this build knows.
func (b ReadinessBucket) Valid() bool { return slices.Contains(ReadinessBuckets(), b) }

// Bucket is the bucket r stands in: its first unmet precondition's, else its first computing one's,
// the merger's authority aside, so it is the same whoever asks; BucketReady where none is unmet or
// computing, and "" where the one it stands at is no bucket's (a draft, its facts, a conflict, its queue).
func (r Readiness) Bucket() ReadinessBucket {
	for _, state := range []PreconditionState{PreconditionUnmet, PreconditionComputing} {
		at := slices.IndexFunc(r.Preconditions, func(p Precondition) bool {
			return p.State == state && p.Kind != PreconditionMergeAuthority
		})
		if at >= 0 {
			return r.Preconditions[at].Kind.bucket()
		}
	}
	if len(r.Preconditions) == 0 {
		return ""
	}
	return BucketReady
}

// bucket is the bucket a change standing at k is in, "" for a kind no bucket names.
func (k PreconditionKind) bucket() ReadinessBucket {
	switch k {
	case PreconditionApproval, PreconditionHumanApproval, PreconditionOwnersApproval:
		return BucketApproval
	case PreconditionBlocker:
		return BucketBlockers
	case PreconditionCheck:
		return BucketChecks
	}
	return ""
}

// holds reports a judged readiness standing in b, every one where b is none; nil is no live change's.
func (b ReadinessBucket) holds(r *Readiness) bool { return b == "" || r != nil && r.Bucket() == b }

// readying is q as a listing narrowed to a bucket walks it: open where it names no state.
func (q ReadQuery) readying() ReadQuery {
	if q.Readiness != "" && q.State == "" {
		q.State = ChangeStateOpen
	}
	return q
}

// readyKey is code_mergeability_ready_idx, (ready, change_id) WHERE ready = 1: the changes whose
// stored readiness is ready, newest change first.
const readyKey = "code_mergeability_ready_idx"

// The changes whose stored readiness is ready, a first page and a page after a cursor, the index's
// partial predicate repeated verbatim (changesWaitingSQL's reason).
const (
	changesReadySQL = `SELECT change_id FROM code_mergeability INDEXED BY ` + readyKey + `
		WHERE ready = 1 ORDER BY change_id DESC LIMIT ?`
	changesReadyAfterSQL = `SELECT change_id FROM code_mergeability INDEXED BY ` + readyKey + `
		WHERE ready = 1 AND change_id < ? ORDER BY change_id DESC LIMIT ?`
)
