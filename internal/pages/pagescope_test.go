package pages_test

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A WRITE TO A PAGE PROBES THE SPACE THE PAGE IS IN.
//
// Every page gesture — a save, a retitle, a trash, a purge and the three
// comment writes — stated its request's scope from the page's subject alone,
// which resolves to a page in NO space, while the record it decided carried
// the real one. So a record this node could not decode, filed under the page's
// space, was never matched: the write decided over rows that were behind for
// that space and reported them applied. Each gesture here meets a deferral on
// its page's space and must refuse it; the control, a page in another space,
// must not — or the cases pass on a store that refuses every write while
// anything is deferred.
func TestAPageWriteProbesTheSpaceThePageIsIn(t *testing.T) {
	t.Parallel()
	body := func(s string) *string { return &s }
	for _, gesture := range []struct {
		name  string
		write func(r *roundTrip, page pages.Page, comment string) error
	}{
		{"save", func(r *roundTrip, page pages.Page, _ string) error {
			_, err := r.store.SavePage(r.t.Context(), author("jane"), page.ID,
				pages.Save{BaseVersion: page.Version, Body: body("edited")})
			return err
		}},
		{"retitle", func(r *roundTrip, page pages.Page, _ string) error {
			_, err := r.store.Rename(r.t.Context(), author("jane"), page.ID,
				"DEPLOY RUNBOOK", false)
			return err
		}},
		{"trash", func(r *roundTrip, page pages.Page, _ string) error {
			_, err := r.store.Trash(r.t.Context(), author("jane"), page.ID)
			return err
		}},
		{"purge", func(r *roundTrip, page pages.Page, _ string) error {
			_, err := r.store.Purge(r.t.Context(), author("jane"), page.ID, "test")
			return err
		}},
		{"comment", func(r *roundTrip, page pages.Page, _ string) error {
			_, _, err := r.store.Comment(r.t.Context(), author("jane"), page.ID,
				pages.NewComment{Body: "a remark"})
			return err
		}},
		{"edit a comment", func(r *roundTrip, page pages.Page, comment string) error {
			_, _, err := r.store.EditComment(r.t.Context(), author("jane"), page.ID,
				comment, "a better remark")
			return err
		}},
		{"remove a comment", func(r *roundTrip, page pages.Page, comment string) error {
			_, err := r.store.RemoveComment(r.t.Context(), author("jane"), page.ID, comment)
			return err
		}},
	} {
		t.Run(gesture.name, func(t *testing.T) {
			t.Parallel()
			r := newRoundTrip(t)
			affected := r.write(author("jane"), pages.NewPage{Container: "ENG", Title: "Deploy Runbook"})
			control := r.write(author("jane"), pages.NewPage{Container: "PROD", Title: "Deploy Runbook"})
			remark := func(page pages.Page) string {
				c, _, err := r.store.Comment(t.Context(), author("jane"), page.ID,
					pages.NewComment{Body: "the first remark"})
				if err != nil {
					t.Fatalf("comment: %v", err)
				}
				r.drain()
				return c.ID
			}
			affectedRemark, controlRemark := remark(affected.Page), remark(control.Page)
			// THE HEADS AS THEY STAND, read before anything covers them.
			affectedHead := r.get(affected.Page.ID).Page
			controlHead := r.get(control.Page.ID).Page
			r.deferRecordAt("a-settings-edit-this-build-cannot-read",
				pages.ScopeTerm{Kind: pages.TermContainer, ID: "ENG"}.Path())

			err := gesture.write(r, affectedHead, affectedRemark)
			var unavailable *statelog.Unavailable
			if !errors.As(err, &unavailable) || unavailable.Reason != statelog.ReasonDeferred {
				t.Fatalf("%s on a page whose space a deferred record covers = %v, "+
					"want a deferred refusal", gesture.name, err)
			}
			if err := gesture.write(r, controlHead, controlRemark); err != nil {
				t.Fatalf("%s on a page in a space nothing covers = %v, want it "+
					"written", gesture.name, err)
			}
		})
	}
}

// A PURGE PROBES ITS WHOLE SPACE, because it writes rows across it: each of
// its children is re-filed at the apply, against whatever children the page
// has at that position. The record said so and the request did not, so a
// record deferred on a sibling — a page this purge may re-file a child of the
// purged page beside — was never matched. The control is the trash of the same
// page, which touches that page alone and must go through.
func TestAPurgeProbesItsWholeSpace(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	doomed := r.write(author("jane"), pages.NewPage{Container: "ENG", Title: "Doomed"})
	sibling := r.write(author("jane"), pages.NewPage{Container: "ENG", Title: "Sibling"})
	r.deferRecordAt(sibling.Page.ID, pages.ScopeTerm{
		Kind: pages.TermObject, Container: "ENG", ID: sibling.Page.ID,
	}.Path())

	_, err := r.store.Purge(t.Context(), author("jane"), doomed.Page.ID, "test")
	var unavailable *statelog.Unavailable
	if !errors.As(err, &unavailable) || unavailable.Reason != statelog.ReasonDeferred {
		t.Fatalf("a purge in a space a deferred record touches = %v, want a "+
			"deferred refusal", err)
	}
	if _, err := r.store.Trash(t.Context(), author("jane"), doomed.Page.ID); err != nil {
		t.Fatalf("a trash of the same page = %v, want it written — it touches "+
			"that page alone", err)
	}
}
