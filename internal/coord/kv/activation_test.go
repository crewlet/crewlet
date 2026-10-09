package kv

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// THE POINTER RECORD IS PINNED ON THE WIRE. Its bytes are a contract between
// builds sharing one bucket in a rolling upgrade: this build reads a later
// build's pointer, so a later build must keep every key here with the meaning
// it has here, and a change to this record cannot go unnoticed.
func TestThePointerRecordIsPinnedOnTheWire(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 2, 8, 0, 0, 0, time.UTC)
	written := at.Add(-time.Hour)
	store := openFleet(t, embeddedNATS(t))
	if _, err := store.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: "rev-1", Summary: "s", Payload: []byte("{}"), At: at,
		Origin: coord.RevisionOrigin{
			Author: "maya", AuthorKind: "operator", Source: "api", CreatedAt: written,
		},
	}); err != nil {
		t.Fatalf("Activate: %v", err)
	}
	entry, err := store.config.Get(t.Context(), activationKey)
	if err != nil {
		t.Fatalf("read the record: %v", err)
	}
	const want = `{"revision_id":"rev-1","at":"2026-09-02T08:00:00Z","summary":"s",` +
		`"author":"maya","author_kind":"operator","source":"api",` +
		`"created_at":"2026-09-02T07:00:00Z"}`
	if string(entry.Value()) != want {
		t.Fatalf("the pointer record is\n  %s\nwant\n  %s", entry.Value(), want)
	}
}
