package kv

import (
	"errors"
	"strings"
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
	// A RESTORE CARRIES ITS MARK, and a choice carries none at all.
	if _, err = store.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: "rev-1", Summary: "s", Payload: []byte("{}"), At: at.Add(time.Hour),
		Expect: "rev-1", Restore: true,
		Origin: coord.RevisionOrigin{Author: "node-a", AuthorKind: iam.ActorSystem,
			Source: "file", CreatedAt: written},
	}); err != nil {
		t.Fatalf("Activate a restore: %v", err)
	}
	if entry, err = store.get(t.Context(), store.config, activationKey); err != nil {
		t.Fatalf("read the restored record: %v", err)
	}
	const restored = `{"revision_id":"rev-1","at":"2026-09-02T09:00:00Z","summary":"s",` +
		`"author":"node-a","author_kind":"system","source":"file",` +
		`"created_at":"2026-09-02T07:00:00Z","payload":{},"restored":true}`
	if string(entry.Value()) != restored {
		t.Fatalf("the restored pointer record is\n  %s\nwant\n  %s", entry.Value(), restored)
	}
}

// A POINTER REMOVED UNDER A WRITER IS A RACE THE WRITER LOST, whether it was
// deleted or purged, and whether the marker that removal left is still on the
// subject or has been swept since ([coord.MarkerRetention]): what an expecting
// write was built on is gone, so it edits a company the fleet no longer runs.
// A create-only write is the one that lands over each, and the suite cannot
// reach these cases: the memory twin has no way to remove its pointer, and
// nothing in the engine removes one — an operator or a rebuilt bucket does.
//
// THE SWEPT MARKER IS THE CASE THAT MATTERS. While a marker stands, the
// subject has a last sequence, so even a write that compared against nothing
// would be refused by the broker's compare-and-set. Once it is swept the
// subject holds no message at all, and only expectedSeq's own refusal stands
// between an expecting write and a pointer recreated over the fleet's loss.
// Every case also asserts the refusal is expectedSeq's, by its words.
//
// Mutation: answer an unheld pointer as "nothing to have raced with" in
// expectedSeq, as it once did, and every case fails — the swept ones by
// landing, the others by naming the broker's refusal instead.
func TestAnExpectationOverARemovedPointerIsARace(t *testing.T) {
	t.Parallel()
	origin := coord.RevisionOrigin{
		Author: "maya", AuthorKind: iam.ActorOperator, Source: "api",
		CreatedAt: time.Date(2026, 9, 2, 7, 0, 0, 0, time.UTC),
	}
	at := time.Date(2026, 9, 2, 8, 0, 0, 0, time.UTC)
	deleted := func(t *testing.T, store *FleetStore) {
		if err := store.config.Delete(t.Context(), activationKey); err != nil {
			t.Fatalf("delete the pointer: %v", err)
		}
	}
	purged := func(t *testing.T, store *FleetStore) {
		if err := store.config.Purge(t.Context(), activationKey); err != nil {
			t.Fatalf("purge the pointer: %v", err)
		}
	}
	swept := func(remove func(*testing.T, *FleetStore)) func(*testing.T, *FleetStore) {
		return func(t *testing.T, store *FleetStore) {
			remove(t, store)
			n, err := sweepMarkers(t.Context(), store.js, store.config, time.Now().Add(time.Hour))
			if err != nil || n == 0 {
				t.Fatalf("sweep the removal's marker: swept %d, err %v", n, err)
			}
		}
	}
	for _, removal := range []struct {
		name   string
		remove func(*testing.T, *FleetStore)
	}{
		{"deleted", deleted},
		{"purged", purged},
		{"deleted and swept", swept(deleted)},
		{"purged and swept", swept(purged)},
	} {
		t.Run(removal.name, func(t *testing.T) {
			t.Parallel()
			store := openFleet(t, embeddedNATS(t))
			ctx := t.Context()
			if _, err := store.Activate(ctx, coord.ActivationRequest{
				RevisionID: "rev-1", Payload: []byte("{}"), At: at, Origin: origin,
			}); err != nil {
				t.Fatalf("Activate: %v", err)
			}
			removal.remove(t, store)

			_, err := store.Activate(ctx, coord.ActivationRequest{
				RevisionID: "rev-2", Payload: []byte("{}"), At: at.Add(time.Minute),
				Expect: "rev-1", Origin: origin,
			})
			if !errors.Is(err, coord.ErrActivationRaced) ||
				!strings.Contains(err.Error(), "the fleet has no activation") {
				t.Fatalf("an expectation over a %s pointer: err = %v, want coord.ErrActivationRaced "+
					"saying the fleet has no activation", removal.name, err)
			}
			if target, found, err := store.Target(ctx); err != nil || found {
				t.Fatalf("target = %+v found=%v err=%v, want the refusal to leave no pointer",
					target, found, err)
			}

			published, err := store.Activate(ctx, coord.ActivationRequest{
				RevisionID: "rev-3", Payload: []byte("{}"), At: at.Add(2 * time.Minute),
				ExpectAbsent: true, Origin: origin,
			})
			if err != nil {
				t.Fatalf("a create-only write over a %s pointer: %v", removal.name, err)
			}
			if published.RevisionID != "rev-3" {
				t.Fatalf("published %+v, want rev-3", published)
			}
		})
	}
}
