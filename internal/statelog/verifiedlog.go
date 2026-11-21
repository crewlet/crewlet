package statelog

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/words"
)

// VerifiedLog is a [LogReader] over a domain's own log that answers each
// record's BODY, its frame opened and signature checked
// ([recordseal.Verifier.Open], ADR-0031), for readers that decode straight off
// the log rather than through an applier. Opening it here, at the seam every
// such reader is handed, keeps the next reader from forgetting to and
// decoding the frame as an unreadable record.
//
// It is strict, because these readers decide from what they read: a record
// under a key this node does not hold is [ErrRecordUnverified] (fit for the
// applier's filing, not for deciding) and a tampered one [ErrRecordTampered],
// so the caller says it cannot tell.
type VerifiedLog struct {
	Log      LogReader
	Verifier *recordseal.Verifier
}

// VerifiedReader is a [LogReader] whose At answers each record's BODY, what
// every reader that decodes straight off the log must be handed
// ([EvictedOnLog], [GenerationOpeners], [OwnGeneration], [UnheldTail], a
// reanchor's [ReanchorStream]).
//
// Only a [VerifiedLog], or a type embedding one, satisfies it: the unexported
// method keeps a raw log out at compile time, whose At answers the signed
// frame and so reads every record as undecodable (an evicted node as never
// evicted).
type VerifiedReader interface {
	LogReader
	verifiedBodies()
}

func (VerifiedLog) verifiedBodies() {}

// ErrRecordUnverified is a record signed under a key this node does not hold:
// retained by the applier and reprocessed once the key arrives, and not a
// record anything may decide from until then.
var ErrRecordUnverified = errors.New("statelog: the record is signed under a key this node does not hold")

// ErrRecordTampered is a record whose frame fails under the keys this node
// holds, or bytes that are not a frame at all: not written by this fleet.
var ErrRecordTampered = errors.New("statelog: the record is not signed by this fleet")

// At answers the body of the record at seq, or why it cannot.
func (v VerifiedLog) At(ctx context.Context, seq uint64) (string, []byte, time.Time, bool, error) {
	if v.Log == nil || v.Verifier == nil {
		return "", nil, time.Time{}, false, fmt.Errorf("statelog: a verified log "+
			"needs both a log and a verifier: %w", recordseal.ErrUnsigned)
	}
	subject, framed, storedAt, ok, err := v.Log.At(ctx, seq)
	if err != nil || !ok {
		return subject, nil, storedAt, ok, err
	}
	body, verdict := v.Verifier.Open(framed)
	switch verdict {
	case recordseal.Verified:
		return subject, body, storedAt, true, nil
	case recordseal.KeyUnknown:
		return subject, nil, storedAt, true, fmt.Errorf("%w (sequence %d; this node "+
			"holds %s)", ErrRecordUnverified, seq, words.And(v.Verifier.KeyIDs()))
	}
	return subject, nil, storedAt, true, fmt.Errorf("%w (sequence %d)",
		ErrRecordTampered, seq)
}
