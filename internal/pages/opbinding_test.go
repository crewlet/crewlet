package pages_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A KEY SENT WITH OTHER CONTENT IS ANOTHER OPERATION, AND SENT WITH THE SAME
// CONTENT IS THE SAME ONE.
//
// The ledger answers an operation it already holds before the write is
// decided, so a write's id derived from the key, the verb and the page alone
// made the same key sent with another body, another title or another remark
// the first write's operation: answered from the ledger, with nothing of the
// second written — a change reported as made and silently dropped. What the
// write says is in the id now, so the retry (the same input again) is still
// one operation and anything else under the key lands as asked.
//
// Mutation: drop the content from [pages.Store]'s derivation and every second
// write is answered as the first, leaving the page as it was.
func TestAKeySentWithOtherContentIsAnotherOperation(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	keyed := author("jane")
	keyed.OpKey = statelog.NewOpID(time.Now(), "")

	created := r.write(author("jane"), pages.NewPage{Title: "Runbook", Body: "v1"})
	id := created.Page.ID

	t.Run("a save", func(t *testing.T) {
		first, err := r.store.SavePage(t.Context(), keyed, id,
			pages.Save{BaseVersion: 1, Body: ptr("v2")})
		if err != nil {
			t.Fatalf("first save: %v", err)
		}
		r.drain()
		again, err := r.store.SavePage(t.Context(), keyed, id,
			pages.Save{BaseVersion: 1, Body: ptr("v2")})
		if err != nil {
			t.Fatalf("the retry: %v", err)
		}
		if again.ChangeID != first.ChangeID {
			t.Errorf("the same save under the same key was two operations, %q and "+
				"%q: a retry would land twice", first.ChangeID, again.ChangeID)
		}
		other, err := r.store.SavePage(t.Context(), keyed, id,
			pages.Save{BaseVersion: 2, Body: ptr("v3")})
		if err != nil {
			t.Fatalf("another save under the key: %v", err)
		}
		r.drain()
		if other.ChangeID == first.ChangeID {
			t.Errorf("another save under the key was the first one's operation %q",
				first.ChangeID)
		}
		if got := r.get(id).Page.Body; got != "v3" {
			t.Errorf("the page reads %q after the second save, want v3 — the "+
				"ledger answered it as the first and wrote nothing", got)
		}
	})

	t.Run("a rename", func(t *testing.T) {
		first, err := r.store.Rename(t.Context(), keyed, id, "Runbook two", false)
		if err != nil {
			t.Fatalf("first rename: %v", err)
		}
		r.drain()
		other, err := r.store.Rename(t.Context(), keyed, id, "Runbook three", false)
		if err != nil {
			t.Fatalf("another rename under the key: %v", err)
		}
		r.drain()
		if other.ChangeID == first.ChangeID {
			t.Errorf("another rename under the key was the first one's operation %q",
				first.ChangeID)
		}
		if got := r.get(id).Page.Title; got != "Runbook three" {
			t.Errorf("the page is titled %q after the second rename, want "+
				"\"Runbook three\"", got)
		}
	})

	t.Run("a remark's edit", func(t *testing.T) {
		comment, _, err := r.store.Comment(t.Context(), author("jane"), id,
			pages.NewComment{Body: "first"})
		if err != nil {
			t.Fatalf("comment: %v", err)
		}
		r.drain()
		first, written, err := r.store.EditComment(t.Context(), keyed, id,
			comment.ID, "second")
		if err != nil {
			t.Fatalf("first edit: %v", err)
		}
		r.drain()
		_, other, err := r.store.EditComment(t.Context(), keyed, id,
			comment.ID, "third")
		if err != nil {
			t.Fatalf("another edit under the key: %v", err)
		}
		r.drain()
		if other.ChangeID == written.ChangeID {
			t.Errorf("another edit under the key was the first one's operation %q",
				written.ChangeID)
		}
		for _, c := range r.get(id).Comments {
			if c.ID == first.ID && c.Body != "third" {
				t.Errorf("the remark reads %q after the second edit, want third",
					c.Body)
			}
		}
	})
}
