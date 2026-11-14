package coord

import (
	"errors"
	"fmt"
	"time"
)

// MaxValueBytes is the largest value one key of the coordination store holds,
// as the store writes it: 6,290,542 bytes. Every backend refuses a write of a
// larger value before anything is sent ([ErrValueTooLarge]), so a value one
// backend takes is one every backend takes, and a case certified on the twin
// certifies the store.
//
// # Why this figure
//
// On the KV backend (internal/coord/kv) every read of a key is its stream
// leader's message read, whose reply is JSON with the value base64-encoded
// inside it, a third larger than the value — and the server on a leaf's side
// of its link closes its WHOLE link to the fleet on any message past its
// max_payload, the transport's eight mebibytes (nats-server v2.15.2,
// leafnode.go, processLeafMsgArgs → maxPayloadViolation), every lease and
// mailbox the node holds across it cut off with it (crewlet/crewlet#1031). A
// node without `data` reads the coordination store across exactly such a
// link. So a value is held to the largest whose read's reply the transport
// carries: a message of 6,290,688 bytes (internal/jsapi's MaxReadable of
// internal/queue's MaxPayloadBytes), less the most header a write of a key
// carries beside its value — which internal/coord/kv derives from the client's
// own encoding and holds this figure to.
//
// # What it bounds
//
// Every record a caller writes as bytes it built — a sandbox run's row and its
// parts, which internal/sandbox budgets to it field by field (its RowBytes) —
// and the records the store builds from a request: the activation pointer,
// which carries the company's sealed document ([ActivationValue]), and which
// internal/api/configapi holds a document to before it stores the revision.
// The rest are bounded by their own fields far below it.
const MaxValueBytes = 6_290_542

// MaxValueCrossing is the longest a write of a run's value at [MaxValueBytes]
// takes to cross a node's connection to the store, for every time the store
// may send it: 540.025663374 seconds, nine minutes, on a node whose
// connection crosses a link at the slowest pace the engine allows one to be.
// A node reaches the store at no slower pace, so a budget that waits on a
// write a node elsewhere in the fleet makes — how long a read of a run's
// record stands by for a capture another node keeps — counts this; one that
// waits on its own node's write counts what that node's connection costs
// ([SandboxRuns.ValueCrossing]), none at all in the store's own process.
//
// # Why this figure
//
// On the KV backend a write is acknowledged once its last byte is in, and a
// node without `data`, or any node of an external cluster, reaches the store
// across a link the engine allows to carry as little as a mebibyte in thirty
// seconds (internal/objstore's MiBPace); the store gives a write the time its
// request takes to cross for each of the sends the client may make of it, up
// to three (internal/jsapi's Sending and WriteSends), counting the key it is
// sent under beside its value and the most header a write carries. A value
// at its bound, under the longest key a run's write is sent under — 43 bytes,
// a part's beside a turn id the engine mints — is 6,290,731 bytes sent behind
// its protocol line's kibibyte: 180.008554458 seconds a send, three sends.
// internal/coord/kv derives it from the one rule its own writes are given
// ([SandboxRuns.ValueCrossing]), and holds this figure to what its own budget
// gives such a write.
//
// ONLY THE CROSSING, never the store's own term: a caller's own reason for a
// bound — a store's slow moment, a leader's election outlasted — is the part a
// budget holds beside it, so a budget is its caller's reason and this
// ([SandboxRuns.ValueCrossing]), and a reason held alone, as a write of a
// value of megabytes was held to five seconds and to thirty, cuts the write
// while its bytes are still crossing (crewlet/crewlet#1076).
const MaxValueCrossing = 540_025_663_374 * time.Nanosecond

// ErrValueTooLarge is a write refused because its value is past
// [MaxValueBytes]. It is refused BEFORE ANYTHING IS SENT, so nothing was
// written: a definite answer, never [ErrUnavailable] — no retry changes it —
// and never [ErrWriteUnknown]. Its message names the key, the value's size and
// the bound.
var ErrValueTooLarge = errors.New("coord: a value past what one key holds")

// HoldValue refuses a value of size bytes written to key past [MaxValueBytes],
// with [ErrValueTooLarge] naming the three; nil for one that fits. The one
// rule every backend refuses a value by.
func HoldValue(key string, size int) error {
	if size <= MaxValueBytes {
		return nil
	}
	return fmt.Errorf("%w: %d bytes for %s, past the %d one key holds — every read of a key "+
		"carries its value base64-encoded in one reply, which a leaf's link closes on past the "+
		"transport", ErrValueTooLarge, size, key, MaxValueBytes)
}
