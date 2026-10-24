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

	// A CREATE's object is its address, so another body at the same address
	// under the key is the one case the address cannot tell apart: bound to
	// the key alone it was the first create's operation, answered as made
	// with the page still reading the first body. Bound to what it says it
	// is another create of a held address, and refused as one.
	t.Run("a create", func(t *testing.T) {
		asked := pages.NewPage{Container: "ENG", Title: "Handbook", Body: "one"}
		first, err := r.store.Create(t.Context(), keyed, asked)
		if err != nil {
			t.Fatalf("first create: %v", err)
		}
		r.drain()
		again, err := r.store.Create(t.Context(), keyed, asked)
		if err != nil || again.Page.ID != first.Page.ID {
			t.Errorf("the same create under the same key answered page %q (%v), "+
				"want the first one's %q: a retry would file a second page",
				again.Page.ID, err, first.Page.ID)
		}
		other := asked
		other.Body = "two"
		if got, err := r.store.Create(t.Context(), keyed, other); err == nil {
			t.Errorf("another create at the address under the key was answered "+
				"as made (page %q, the first was %q) — the ledger answered it as "+
				"the first and wrote nothing", got.Page.ID, first.Page.ID)
		}
		r.drain()
		if got := r.get(first.Page.ID).Page.Body; got != "one" {
			t.Errorf("the first page reads %q, want one", got)
		}
	})

	// A PURGE SAYS WHY, and the reason is what it records: another reason
	// under the key is another purge — of a page already gone, which is
	// refused — rather than the first one answered back as though the
	// second reason had been written.
	t.Run("a purge", func(t *testing.T) {
		doomed := r.write(author("jane"), pages.NewPage{Title: "Scratch"}).Page.ID
		first, err := r.store.Purge(t.Context(), keyed, doomed, "a duplicate")
		if err != nil {
			t.Fatalf("first purge: %v", err)
		}
		r.drain()
		again, err := r.store.Purge(t.Context(), keyed, doomed, "a duplicate")
		if err != nil || again.ChangeID != first.ChangeID {
			t.Errorf("the same purge under the same key answered %q (%v), want "+
				"the first one's %q", again.ChangeID, err, first.ChangeID)
		}
		if other, err := r.store.Purge(t.Context(), keyed, doomed,
			"out of date"); err == nil {
			t.Errorf("another purge reason under the key was answered as made "+
				"(%q, the first was %q) — the ledger answered it as the first",
				other.ChangeID, first.ChangeID)
		}
	})
}
