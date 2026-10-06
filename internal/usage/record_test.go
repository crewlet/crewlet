package usage_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/usage"
)

// newerKind is a record a later build writes about a kind this one has never
// heard of, stamped the way that build would stamp it: above this build's
// [usage.RecordVersion].
func newerKind(t *testing.T, v int) []byte {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"v":      v,
		"writer": "node-b",
		"subject": map[string]any{
			"kind": "a_kind_from_a_later_build", "node": "node-b", "day": "2026-09-23",
			"person": "alice",
		},
		"scope":  statelog.ScopeSet{Paths: []string{"u.2026-09-23.node-b.later.alice"}},
		"tokens": []map[string]any{{"phase": "auxiliary", "total": 42, "calls": 1}},
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	return body
}

// A NEWER BUILD'S KIND IS DEFERRED, NOT A STOP.
//
// The envelope is the half every build must read, and the framework stops a
// domain's whole applier on one it cannot (statelog's decode): no record after
// it is applied on that node until an operator intervenes. Kind's own doc
// promised that a kind a newer build publishes is DEFERRED, and the envelope
// refused every kind it did not know — so the first record of any kind a
// later build added would have stopped the usage domain on every node still
// running this one. A record stamped above this build's version is a newer
// build's, so its envelope is read and the record is retained to be applied by
// a build that knows it.
func TestANewerBuildsKindIsDeferredNotAStop(t *testing.T) {
	t.Parallel()
	payload := newerKind(t, usage.RecordVersion+1)
	env, err := usage.Domain{}.Envelope(payload)
	if err != nil {
		t.Fatalf("the envelope of a newer build's kind = %v — an unreadable envelope "+
			"stops this domain's applier on every node still on this build", err)
	}
	if env.Kind != "a_kind_from_a_later_build" || env.Subject.Kind != env.Kind {
		t.Fatalf("envelope = %+v, want the newer build's kind carried through", env)
	}
	_, err = usage.Decode(payload)
	var future *usage.ErrFutureVersion
	if !errors.As(err, &future) {
		t.Fatalf("decode = %v, want ErrFutureVersion: the record is retained for a "+
			"build that can apply it, never applied as something it is not", err)
	}

	// THE APPLIER SAYS THE SAME, which is the answer the framework defers on.
	db := openStore(t)
	rec := statelog.Record{
		Envelope: env,
		Position: statelog.Position{Stream: usage.Domain{}.Stream().Name, Generation: 1, Seq: 1},
		Payload:  payload,
	}
	if err := applyRecord(context.Background(), db, rec); !errors.As(err, &future) {
		t.Fatalf("apply = %v, want ErrFutureVersion", err)
	}
}

// AN UNKNOWN KIND AT A VERSION THIS BUILD READS IS A WRITER FAULT.
//
// A version this build reads is one whose kinds it knows, so a kind it does not
// know at that version was written wrong rather than by a later build — and
// retaining it would wait for a build that is never coming.
func TestAnUnknownKindAtAKnownVersionIsRefused(t *testing.T) {
	t.Parallel()
	if _, err := (usage.Domain{}).Envelope(newerKind(t, usage.RecordVersion)); err == nil {
		t.Fatal("an unknown kind at this build's own version was read as a record")
	}
}

// A NEWER BUILD'S KIND STILL NAMES ITS NODE AND ITS DAY: the two segments
// every kind's identity begins with are the only ones this build can check,
// and it checks them.
func TestANewerBuildsKindStillNamesItsNodeAndDay(t *testing.T) {
	t.Parallel()
	var body map[string]any
	if err := json.Unmarshal(newerKind(t, usage.RecordVersion+1), &body); err != nil {
		t.Fatal(err)
	}
	body["subject"].(map[string]any)["day"] = "the twenty-third"
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (usage.Domain{}).Envelope(payload); err == nil {
		t.Fatal("a newer build's record with no readable day was admitted")
	}
}
