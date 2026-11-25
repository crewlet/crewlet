package coord

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// MaxValueBytes is the largest value one key of the coordination store holds.
// Every backend refuses a larger write before sending anything ([HoldValue],
// [ErrValueTooLarge]), so a case certified on the twin certifies the store. On
// the KV backend a read's reply carries the value base64 in JSON, a third
// larger, and a leaf's server closes its WHOLE link to the fleet, every lease
// and mailbox across it, on a message past the transport's eight-mebibyte
// max_payload (nats-server leafnode.go, maxPayloadViolation). So a value fits a
// 6,290,688-byte read (internal/jsapi's MaxReadable) beside the most header a
// write carries, a run's standing in it ([RunStanding]), which internal/coord/kv
// derives and holds this figure to. It binds a run's row and parts
// (internal/sandbox's RowBytes) and the activation's body ([BodyRef], held by
// internal/api/configapi); other records keep far below it.
const MaxValueBytes = 6_290_346

// MaxValueCrossing is the longest a CHANGE of a run's value at [MaxValueBytes],
// a read-decide-write ([SandboxRuns.MutateSandboxRun]), takes to cross a node's
// connection to the store at the slowest link pace the engine allows
// (internal/objstore's MiBPace, both ways): the read, an 8 MiB reply
// (internal/jsapi's ReplyBytes), 240 s ([MaxValueReadCrossing]); the write, 6,290,731 bytes under a
// 43-byte key (a part's, a run's longest) behind a kibibyte protocol line,
// 180.008554458 s a send for up to three (internal/jsapi's Sending and
// WriteSends). internal/coord/kv derives and holds it. A budget waiting on
// another node's change counts this; one on its own node's,
// [SandboxRuns.ValueCrossing]. It is ONLY the crossing: a budget is its caller's
// own reason PLUS this, since a reason alone cuts the change while its bytes
// still cross, and counting only the write leaves the read's four minutes out.
const MaxValueCrossing = 780_025_663_374 * time.Nanosecond

// MaxValueReadCrossing is the longest a READ of a run's value at
// [MaxValueBytes] takes to cross a node's connection to the store at the
// slowest link pace the engine allows: the leader's reply alone, 8 MiB
// (internal/jsapi's ReplyBytes) at internal/objstore's MiBPace, 240 s, the read
// [MaxValueCrossing] counts beside its write's sends. internal/coord/kv derives
// and holds it. A budget waiting on another node's read of a run's value counts
// this, never a change's crossing, since a read sends nothing; one on its own
// node's, [SandboxRuns.ValueReadCrossing]. A budget is its reason PLUS this.
const MaxValueReadCrossing = 240 * time.Second

// ErrValueTooLarge is a write refused because what it carries is past the
// store's bound on it: a key's value past [MaxValueBytes] ([HoldValue]), or a
// lease's Meta past [MaxLeaseMetaBytes] ([HoldLeaseMeta]). It is refused
// BEFORE ANYTHING IS SENT: a definite answer, never [ErrUnavailable] (no retry
// changes it) nor [ErrWriteUnknown]. The wrapping message names the write, its
// size and its bound, so this text names neither bound.
var ErrValueTooLarge = errors.New("coord: a write past what the store takes")

// HoldValue refuses a value of size bytes written to key past bound, the most
// a value of key's class may be ([MaxValueBytes], [MaxPointerBytes]), with
// [ErrValueTooLarge] naming all three; nil for one that fits. Every backend
// refuses a value by it, with the bound its store declares for the key.
func HoldValue(key string, size, bound int) error {
	if size <= bound {
		return nil
	}
	return fmt.Errorf("%w: %d bytes for %s, past the %d its key's class may be, since every read "+
		"of the key carries its value whole in one reply: store less under it, or store the bytes "+
		"under a key of their own and name them by size and digest (ADR-0175)",
		ErrValueTooLarge, size, key, bound)
}

// SmallValueBytes is the most a declared class read at the read term alone,
// given no crossing of its own, may hold (internal/coord/kv's key classes,
// the activation pointer's among them): at this bound a read's reply
// (internal/jsapi's ReplyBytes, the most write header included) is 23,328
// bytes, 0.67 s at the floor, inside internal/jsprovision's ReadTerm of a
// second. internal/coord/kv holds the figure to it.
const SmallValueBytes = 16 << 10

// MaxPointerBytes is the most the activation pointer may be ([ActivationValue]):
// every probe, heartbeat and tick on every node reads it, so its read must
// cross a link at the slowest pace allowed inside a probe's 1.875 s. At this
// bound a read's reply (internal/jsapi's ReplyBytes, the most write header
// included) is 12,404 bytes: 0.35 s at the floor. Its fixed fields take under 600 bytes and
// [MaxSummaryBytes] 4 KiB, so the rest is room for an origin no valid one
// approaches. The company document rides its own key, named by the pointer.
const MaxPointerBytes = 8 << 10

// MaxSummaryBytes is the most a revision's summary may take in the activation
// pointer, measured as the pointer encodes it ([SummaryBytes]). Half the
// pointer, and above the longest summary a writer composes (crewlet-operator's,
// at 2,722 bytes). A caller's summary past it is refused where it enters
// ([HoldSummary]), never cut: it is that person's words.
const MaxSummaryBytes = 4 << 10

// BodyRetention is how long the activation's body a pointer no longer names
// stays before its sweep removes it ([ActivationBodies]). A write flips the
// pointer to its body no later than half of this after it began writing the
// body, so the sweep never removes a body a flip still in flight will name
// (internal/coord/kv's Activate, ADR-0175). Half an hour for that half: over
// twice [MaxValueCrossing], which bounds the body's write at its bound across a
// link at the slowest pace, resends included, so the flip behind it has as
// long again. The other half is for clock skew. Every superseded body is kept
// this long, so no longer than the flip needs (ADR-0175 counts the bytes).
const BodyRetention = time.Hour

// ErrSummaryTooLong is a revision summary past [MaxSummaryBytes], refused
// before anything is written: a definite answer that names the summary, since
// only a shorter summary changes it.
var ErrSummaryTooLong = errors.New("coord: a revision summary past what the activation pointer carries")

// SummaryBytes is how many bytes summary takes in the activation pointer: its
// JSON encoding without the quotes, so an escaped byte counts as written (a
// control byte is six).
func SummaryBytes(summary string) int {
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	// A string always encodes.
	_ = enc.Encode(summary)
	return out.Len() - len("\"\"\n")
}

// HoldSummary refuses a summary past [MaxSummaryBytes] with [ErrSummaryTooLong],
// naming its size as the pointer encodes it; nil for one that fits.
func HoldSummary(summary string) error {
	if n := SummaryBytes(summary); n > MaxSummaryBytes {
		return fmt.Errorf("%w: %d bytes as the pointer encodes it, past the %d it may take",
			ErrSummaryTooLong, n, MaxSummaryBytes)
	}
	return nil
}
