package statelog

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// VerifiedLog is a [LogReader] over a domain's own log that answers each
// record's BODY — its frame opened and its signature checked — for the readers
// that decode a record straight off the log rather than through an applier.
//
// # Why it exists
//
// Every record on a state log is a signed FRAME (ADR-0018), and the applier
// opens it with [Verifier.Open] before any domain decodes a byte. A reader
// that reads the log by position — whether a node was evicted, which node
// opened a generation, which records past a restore these rows do not hold —
// hands what it read to [Domain.Envelope] too, and handed the frame the
// envelope does not decode: an eviction read as unreadable, a generation
// record as somebody else's. Opening the frame at each of those call sites
// would be the rule written once per reader, and the next reader added would
// be the one that forgot; so it is written once, here, at the seam every such
// reader is handed.
//
// # Strict: only a VERIFIED record is answered
//
// These readers decide something from what they read — a standing, an
// authority, a history — so a record signed under a key this node does not
// hold is an error ([ErrRecordUnverified]) rather than a body: it is readable
// for FILING, which is the applier's use of it, and not for deciding. A
// tampered one is an error too ([ErrRecordTampered]), and never reaches a
// decoder. Either way the caller says it cannot tell, which is the answer a
// reader without the key has.
type VerifiedLog struct {
	Log      LogReader
	Verifier *Verifier
}

// VerifiedReader is a [LogReader] whose At answers each record's BODY — what
// every reader that decodes straight off the log must be handed
// ([EvictedOnLog], [GenerationOpeners], [OwnGeneration], [UnheldTail], a
// reanchor's [ReanchorStream]).
//
// ONLY A [VerifiedLog] SATISFIES IT, or a type embedding one: the unexported
// method is what keeps a domain's raw log out. A raw log's At answers the
// signed frame, and handed where a body is decoded it compiled, ran, and read
// every record as one that does not decode — an evicted node read as never
// evicted, a generation this node opened as somebody else's. A rule a caller
// has to remember is the rule the next caller forgets, so the compiler holds
// it instead.
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

// At reads the record at seq and answers its body, or an error saying why it
// cannot be read as one.
func (v VerifiedLog) At(ctx context.Context, seq uint64) (string, []byte, time.Time, bool, error) {
	if v.Log == nil || v.Verifier == nil {
		return "", nil, time.Time{}, false, fmt.Errorf("statelog: a verified log "+
			"needs both a log and a verifier: %w", ErrUnsigned)
	}
	subject, framed, storedAt, ok, err := v.Log.At(ctx, seq)
	if err != nil || !ok {
		return subject, nil, storedAt, ok, err
	}
	body, verdict := v.Verifier.Open(framed)
	switch verdict {
	case Verified:
		return subject, body, storedAt, true, nil
	case KeyUnknown:
		return subject, nil, storedAt, true, fmt.Errorf("%w (sequence %d; this node "+
			"holds %v)", ErrRecordUnverified, seq, v.Verifier.KeyIDs())
	}
	return subject, nil, storedAt, true, fmt.Errorf("%w (sequence %d)",
		ErrRecordTampered, seq)
}
