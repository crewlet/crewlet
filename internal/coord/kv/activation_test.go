package kv

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
)

// AN OLDER BUILD'S POINTER READS AS AN UNKNOWN AUTHOR, NEVER AS AN INVENTED ONE.
//
// A rolling upgrade leaves the pointer an older build published in the bucket
// until the next activation, and that record carries no origin at all. It must
// decode — a pointer this build cannot read is a fleet that stops converging
// on upgrade — and it must read as a ZERO origin, which the adopting node
// records as "not recorded" rather than as `peer` or as itself.
func TestAnOlderPointerReadsAsAnUnknownAuthor(t *testing.T) {
	store := openFleet(t, embeddedNATS(t))
	const older = `{"revision_id":"rev-old","at":"2026-09-01T10:00:00Z","summary":"from an older build"}`
	if _, err := store.config.Put(t.Context(), activationKey, []byte(older)); err != nil {
		t.Fatalf("write the older pointer: %v", err)
	}
	got, found, err := store.Target(t.Context())
	if err != nil || !found {
		t.Fatalf("Target = (%+v, %v, %v): an older build's pointer must still be read",
			got, found, err)
	}
	if got.RevisionID != "rev-old" || got.Summary != "from an older build" {
		t.Fatalf("target = %+v, want the older record's revision and summary", got)
	}
	if got.Origin != (coord.RevisionOrigin{}) {
		t.Fatalf("origin = %+v, want the zero origin: nothing in that record says who "+
			"wrote the revision", got.Origin)
	}
}

// THIS BUILD'S POINTER STAYS READABLE BY AN OLDER ONE. The origin is added
// beside the fields an older build decodes, never in place of one, so the
// record keeps every key it always had with the meaning it always had.
func TestAPointerKeepsTheFieldsAnOlderBuildReads(t *testing.T) {
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
