package pages_test

import (
	"database/sql"
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A PAGE READ REFUSES WHERE ITS PAGE IS FILED, by id and by address alike.
//
// One page is filed under the space its rows name. The read used to form its
// scope from the reference itself — an id with the id standing in for the
// space, an address with the whole reference standing in for the id — so it
// probed paths no record is filed under, and a page a newer build had already
// rewritten was served as though nothing covered it.
func TestAPageReadRefusesWhereItsPageIsFiled(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	affected := r.write(author("ana"), pages.NewPage{Container: "ENG", Title: "Deploy Runbook"})
	other := r.write(author("ana"), pages.NewPage{Container: "ENG", Title: "Somebody Else's"})
	r.deferRecordAt(affected.Page.ID, pages.ScopeTerm{
		Kind: pages.TermObject, Container: "ENG", ID: affected.Page.ID,
	}.Path())

	fresh := statelog.Freshness{Level: statelog.ReadSession}
	for _, ref := range []string{affected.Page.ID, "ENG/Deploy Runbook", "eng/deploy runbook"} {
		_, err := r.reader.Get(t.Context(), ref, fresh)
		var refusal *statelog.Refused
		if !errors.As(err, &refusal) || refusal.Code != statelog.RefuseDeferred {
			t.Errorf("Get(%q) on a page a deferred record covers = %v, want a "+
				"deferred refusal", ref, err)
		}
	}

	// THE CONTROL: a page nothing covers is served, or the cases above pass
	// on a reader that refuses every read while anything is deferred.
	if _, err := r.reader.Get(t.Context(), other.Page.ID, fresh); err != nil {
		t.Fatalf("Get on an unaffected page = %v, want it served", err)
	}
}

// AN ADDRESS NOBODY HOLDS IS STILL ABOUT THAT ADDRESS.
//
// "No such page" is a claim about rows, and a deferred create is precisely a
// row this node has not written — filed under the title it claimed. A read of
// that address refuses rather than answering not found.
func TestAnUnheldAddressIsProbedAtItsTitle(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	r.deferRecordAt("a-page-this-build-cannot-read", pages.ScopeTerm{
		Kind: pages.TermTitle, Container: "ENG", ID: pages.TitleToken("Not Yet Here"),
	}.Path())

	_, err := r.reader.Get(t.Context(), "ENG/Not Yet Here",
		statelog.Freshness{Level: statelog.ReadSession})
	var refusal *statelog.Refused
	if !errors.As(err, &refusal) || refusal.Code != statelog.RefuseDeferred {
		t.Fatalf("Get on an address a deferred create claimed = %v, want a "+
			"deferred refusal rather than not found", err)
	}
	if _, err := r.reader.Get(t.Context(), "ENG/Somewhere Else",
		statelog.Freshness{Level: statelog.ReadSession}); !errors.Is(err, pages.ErrNotFound) {
		t.Fatalf("Get on an address nothing claimed = %v, want not found", err)
	}
}

// deferRecordAt files one undecodable record about pageID under scope, which
// is how a newer peer's record looks to this build.
func (r *roundTrip) deferRecordAt(pageID, scope string) {
	r.t.Helper()
	position := int64(1)<<40 | 9_000_000
	if err := r.db.Replicated().Tx(r.t.Context(), func(tx *sql.Tx) error {
		if _, err := tx.ExecContext(r.t.Context(), `
			INSERT INTO pages_log_deferred
				(position, subject, subject_kind, subject_id, version, payload, stored_at)
			VALUES (?, ?, 'page', ?, ?, x'00', 0)`,
			position, "page."+pageID, pageID, pages.RecordVersion+1); err != nil {
			return err
		}
		_, err := tx.ExecContext(r.t.Context(), `
			INSERT INTO pages_log_deferred_scope (position, path) VALUES (?, ?)`,
			position, scope)
		return err
	}); err != nil {
		r.t.Fatalf("defer a record: %v", err)
	}
}
