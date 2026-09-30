package pages_test

import (
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// retain files a record this node "could not decode" at position, scoped to
// paths — the rows the framework's own loop writes when it retains one — and
// hands back a reader over a node that reports it holds one, which is what
// makes the framework consult the index at all.
func (r *roundTrip) retain(position int64, paths ...string) *pages.Reader {
	r.t.Helper()
	if err := r.db.Replicated().Tx(r.t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(r.t.Context(), `
			INSERT INTO pages_log_deferred
				(position, subject, subject_kind, subject_id, version, payload, stored_at)
			VALUES (?, 'crewlet.pages.page.x', 'page', 'x', 99, x'00', ?)`,
			position, store.EncodeTime(wednesday)); err != nil {
			return err
		}
		for _, path := range paths {
			if _, err := tx.ExecContext(r.t.Context(),
				`INSERT INTO pages_log_deferred_scope (position, path) VALUES (?, ?)`,
				position, path); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		r.t.Fatalf("retain a record: %v", err)
	}
	authority, err := statelog.NewReader(statelog.ReaderDeps{
		Domain: pages.Domain{},
		DB:     r.db.Replicated(),
		Waiter: r.waiter,
		Health: func() statelog.Health {
			at := r.waiter.Committed()
			behind, first, floor := uint64(0), uint64(1), uint64(0)
			return statelog.Health{
				Position: at, AppliedThrough: at.Seq, Drained: true,
				Floor:     statelog.Floor{State: statelog.FloorOK, ReadAt: time.Now()},
				Lag:       &behind,
				FirstSeq:  &first,
				TrimFloor: &floor,
				Deferred:  1,
			}
		},
		Drain: func() float64 { return 2000 },
	})
	if err != nil {
		r.t.Fatalf("read authority: %v", err)
	}
	reader, err := pages.NewReader(pages.ReaderOptions{
		DB: r.db, Log: authority, Committed: r.waiter.Committed,
	})
	if err != nil {
		r.t.Fatalf("pages reader: %v", err)
	}
	return reader
}

// objectPath is the scope path a record about one page is filed under.
func objectPath(container, id string) string {
	return pages.ScopeTerm{Kind: pages.TermObject, Container: container, ID: id}.Path()
}

// A LISTING IS SCOPED TO THE CONTAINER ITS ROWS ARE IN, HOWEVER THE CALLER
// SPELLED IT.
//
// The rows a listing of `eng` returns are ENG's — every read's SQL compares
// the canonical key — while the deferral scope was formed from the container
// as passed, which no record is ever filed under. A space holding a record
// this node cannot decode was then listed as complete, so a page that record
// created, moved in or trashed was missing or present with nothing saying so.
//
// Mutation: form the scope from the container as passed and the listing and
// the feed of `eng` claim to be complete.
func TestAReadIsScopedToTheCanonicalContainer(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.write(author("jane"), pages.NewPage{Container: "ENG", Title: "Runbook"})
	// A RECORD ABOUT THE SPACE ITSELF — its settings, an archive — which is
	// matched by the path exactly: a scope naming another spelling of the
	// container shares no path with it.
	reader := r.retain(1_000_000,
		pages.ScopeTerm{Kind: pages.TermContainer, ID: "ENG"}.Path())
	stale := statelog.Freshness{Level: statelog.ReadStale}

	for _, spelled := range []string{"ENG", "eng", " Eng "} {
		listed, err := reader.List(t.Context(), pages.Filter{Container: spelled}, stale)
		if err != nil {
			t.Fatalf("list %q: %v", spelled, err)
		}
		if len(listed.Pages) != 1 || listed.Complete {
			t.Errorf("listing %q served %d page(s), complete %v — want ENG's "+
				"one page and the gap the retained record leaves", spelled,
				len(listed.Pages), listed.Complete)
		}
		feed, err := reader.Activity(t.Context(), pages.PageActivityQuery{
			Container: spelled, Freshness: stale,
		})
		if err != nil {
			t.Fatalf("activity of %q: %v", spelled, err)
		}
		if feed.Complete {
			t.Errorf("the feed of %q claims to be complete over a retained "+
				"record in ENG", spelled)
		}
	}
	// THE CONTROL: another space holds nothing retained, so its listing
	// is complete — the rows above are not incomplete for every container.
	other, err := reader.List(t.Context(), pages.Filter{Container: "ops"}, stale)
	if err != nil {
		t.Fatalf("list ops: %v", err)
	}
	if !other.Complete {
		t.Error("a space no retained record is about was listed as incomplete")
	}
}

// A PAGE READ REFUSES A PAGE A RETAINED RECORD IS ABOUT — BY ITS ID AND BY ITS
// ADDRESS — AND ONE NO ROW HOLDS WHILE A RETAINED RECORD MAY CREATE IT.
//
// A point read's refusal is decided on the page's own scope, and that scope
// was formed from the reference: an id became the container AND the id, an
// address the id, so neither matched the object path any record about the
// page is filed under, and a page a record this node cannot decode had saved,
// moved or trashed was served as current. The scope is the resolved page's,
// in the snapshot that read it — and a page no row holds is decided the same
// way, since "no such page" read from rows a retained create never wrote is
// the same stale answer.
//
// Mutation: go back to the reference's own scope, or drop the absent page's
// probe, and a row goes red.
func TestAPageReadRefusesWhatARetainedRecordCovers(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	held := r.write(author("jane"), pages.NewPage{Container: "ENG", Title: "Runbook"})
	clear := r.write(author("jane"), pages.NewPage{Container: "ENG", Title: "Onboarding"})
	unwritten := uuid.Must(uuid.NewV7()).String()
	reader := r.retain(1_000_000, objectPath("ENG", held.Page.ID),
		objectPath("OPS", unwritten))
	stale := statelog.Freshness{Level: statelog.ReadStale}

	for _, ref := range []string{held.Page.ID, "eng/Runbook", "ENG/runbook", unwritten} {
		_, err := reader.Get(t.Context(), ref, stale)
		var refusal *statelog.Refused
		if !errors.As(err, &refusal) || refusal.Code != statelog.RefuseDeferred {
			t.Errorf("reading %q answered %v, want the `deferred` refusal a "+
				"point read gives when a retained record covers it", ref, err)
		}
	}
	// THE CONTROLS: a page in the same space that nothing retained is
	// about is served, and an id no record is about is not found.
	if got, err := reader.Get(t.Context(), clear.Page.ID, stale); err != nil ||
		got.Page.ID != clear.Page.ID {
		t.Errorf("a page nothing retained is about answered %v", err)
	}
	if _, err := reader.Get(t.Context(), uuid.Must(uuid.NewV7()).String(), stale); !errors.Is(err, pages.ErrNotFound) {
		t.Errorf("an id no row holds and no record is about answered %v, want "+
			"not found", err)
	}
}
