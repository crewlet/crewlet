package coord

import (
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
// key's write carries, which internal/coord/kv derives and holds this figure
// to. It binds a sandbox run's row and parts (internal/sandbox's RowBytes) and
// the activation pointer ([ActivationValue], held by internal/api/configapi);
// other records' own fields keep them far below it.
const MaxValueBytes = 6_290_542

// MaxValueCrossing is the longest a CHANGE of a run's value at [MaxValueBytes],
// a read-decide-write ([SandboxRuns.MutateSandboxRun]), takes to cross a node's
// connection to the store at the slowest link pace the engine allows
// (internal/objstore's MiBPace, both ways): the read, an 8 MiB reply
// (internal/jsapi's ReplyBytes), 240 s; the write, 6,290,731 bytes under a
// 43-byte key (a part's, a run's longest) behind a kibibyte protocol line,
// 180.008554458 s a send for up to three (internal/jsapi's Sending and
// WriteSends). internal/coord/kv derives and holds it. A budget waiting on
// another node's change counts this; one on its own node's,
// [SandboxRuns.ValueCrossing]. It is ONLY the crossing: a budget is its caller's
// own reason PLUS this, since a reason alone cuts the change while its bytes
// still cross, and counting only the write leaves the read's four minutes out.
const MaxValueCrossing = 780_025_663_374 * time.Nanosecond

// ErrValueTooLarge is a write refused because what it carries is past the
// store's bound on it: a key's value past [MaxValueBytes] ([HoldValue]), or a
// lease's Meta past [MaxLeaseMetaBytes] ([HoldLeaseMeta]). It is refused
// BEFORE ANYTHING IS SENT: a definite answer, never [ErrUnavailable] (no retry
// changes it) nor [ErrWriteUnknown]. The wrapping message names the write, its
// size and its bound, so this text names neither bound.
var ErrValueTooLarge = errors.New("coord: a write past what the store takes")

// HoldValue refuses a value of size bytes written to key past [MaxValueBytes],
// with [ErrValueTooLarge] naming the three; nil for one that fits. Every
// backend refuses a value by it.
func HoldValue(key string, size int) error {
	if size <= MaxValueBytes {
		return nil
	}
	return fmt.Errorf("%w: %d bytes for %s, past the %d one key holds — every read of a key "+
		"carries its value base64-encoded in one reply, which a leaf's link closes on past the "+
		"transport", ErrValueTooLarge, size, key, MaxValueBytes)
}
