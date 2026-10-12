package kv

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
)

// THE POINTER RECORD IS PINNED ON THE WIRE. Its bytes are a contract between
// builds sharing one bucket in a rolling upgrade: this build reads a later
// build's pointer, so a later build must keep every key here with the meaning
// it has here, and a change to this record cannot go unnoticed. The origin —
// its author, the author's kind, the credential, the source and the creation
// instant — and the body ride in it, the body as the JSON object it is.
func TestThePointerRecordIsPinnedOnTheWire(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 2, 8, 0, 0, 0, time.UTC)
	written := at.Add(-time.Hour)
	store := openFleet(t, embeddedNATS(t))
	if _, err := store.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: "rev-1", Summary: "s", Payload: []byte("{}"), At: at,
		Origin: coord.RevisionOrigin{
			Author: "maya", AuthorKind: iam.ActorOperator, OperatorID: "pat:0192f00d-0000-7000-8000-00000000000a",
			Source: "api", CreatedAt: written,
		},
	}); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	entry, err := store.get(t.Context(), store.config, activationKey)
	if err != nil {
		t.Fatalf("read the record: %v", err)
	}
	const want = `{"revision_id":"rev-1","at":"2026-09-02T08:00:00Z","summary":"s",` +
		`"author":"maya","author_kind":"operator",` +
		`"operator_id":"pat:0192f00d-0000-7000-8000-00000000000a","source":"api",` +
		`"created_at":"2026-09-02T07:00:00Z","payload":{}}`
	if string(entry.Value()) != want {
		t.Fatalf("the pointer record is\n  %s\nwant\n  %s", entry.Value(), want)
	}
}
