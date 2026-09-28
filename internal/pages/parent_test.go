package pages_test

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/pages"
)

// A PAGE IS FILED ONLY UNDER A PARENT IT CAN HAVE, and the write authority is
// what says so.
//
// The pointer was stored as given. A page filed under an id nobody holds, one
// in another space, one in the trash, itself or one of its own descendants
// was accepted and written — and each is a page no walk of its container ever
// reaches, since the tree is read from the top down: two pages filed under
// each other are reachable from nowhere at all. Every refusal names the field
// and the remedy, and answers both [pages.ErrParent] (which remedy) and
// [pages.ErrInvalid] (that it is a field refusal at all).
func TestAPageIsFiledOnlyUnderAParentItCanHave(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	top := r.write(author("jane"), pages.NewPage{Title: "Handbook", Body: "top"})
	child := r.write(author("jane"), pages.NewPage{
		Title: "Onboarding", Body: "under the handbook", ParentID: top.Page.ID,
	})
	elsewhere, err := r.store.Create(t.Context(), author("jane"), pages.NewPage{
		Container: "OPS", Title: "Pager", Body: "ops' own",
	})
	if err != nil {
		t.Fatalf("create in OPS: %v", err)
	}
	trashed := r.write(author("jane"), pages.NewPage{Title: "Old", Body: "bin"})
	if _, err := r.store.Trash(t.Context(), author("jane"), trashed.Page.ID); err != nil {
		t.Fatalf("trash: %v", err)
	}
	gone := r.write(author("jane"), pages.NewPage{Title: "Gone", Body: "soon"})
	if _, err := r.store.Purge(t.Context(), author("jane"), gone.Page.ID, "test"); err != nil {
		t.Fatalf("purge: %v", err)
	}
	r.drain()

	refused := func(what string, err error, says string) {
		t.Helper()
		if !errors.Is(err, pages.ErrParent) || !errors.Is(err, pages.ErrInvalid) {
			t.Fatalf("%s: %v, want a refusal of parent_id", what, err)
		}
		if !strings.Contains(err.Error(), "parent_id") || !strings.Contains(err.Error(), says) {
			t.Fatalf("%s: %q does not name the field and say %q", what, err, says)
		}
	}
	for _, c := range []struct {
		what, parent, says string
	}{
		{"a parent nobody holds", "no-such-page", "there is no page"},
		{"a parent in another container", elsewhere.Page.ID, "in container OPS"},
		{"a parent in the trash", trashed.Page.ID, "in the trash"},
		{"a purged parent", gone.Page.ID, "purged"},
	} {
		_, err := r.store.Create(t.Context(), author("jane"), pages.NewPage{
			Container: "ENG", Title: "New " + c.what, Body: "x", ParentID: c.parent,
		})
		refused("a create under "+c.what, err, c.says)
	}

	// A MOVE IS JUDGED THE SAME WAY, and adds the two a create cannot
	// reach: the page itself, and a page beneath it.
	for _, c := range []struct {
		what, parent, says string
	}{
		{"itself", top.Page.ID, "its own parent"},
		{"its own child", child.Page.ID, "beneath this page"},
		{"a parent in another container", elsewhere.Page.ID, "in container OPS"},
		{"a parent nobody holds", "no-such-page", "there is no page"},
	} {
		parent := c.parent
		_, err := r.store.SavePage(t.Context(), author("jane"), top.Page.ID,
			pages.Save{BaseVersion: 1, ParentID: &parent})
		refused("a move under "+c.what, err, c.says)
	}

	// AND A SOUND ONE LANDS: a sibling under the handbook, and the
	// handbook's child moved back to the top.
	sibling := r.write(author("jane"), pages.NewPage{
		Title: "Leave", Body: "under the handbook", ParentID: top.Page.ID,
	})
	toTop := ""
	if _, err := r.store.SavePage(t.Context(), author("jane"), child.Page.ID,
		pages.Save{BaseVersion: 1, ParentID: &toTop}); err != nil {
		t.Fatalf("move a page to its container's top: %v", err)
	}
	r.drain()
	if got := r.get(sibling.Page.ID).Page.ParentID; got != top.Page.ID {
		t.Fatalf("a page created under the handbook is under %q", got)
	}
	if got := r.get(child.Page.ID).Page.ParentID; got != "" {
		t.Fatalf("a page moved to the top is under %q", got)
	}
}

// A PARENT A PAGE CANNOT HOLD AT THE APPLY IS SALVAGED, never refused and
// never written.
//
// The decide sees one snapshot and the broker arbitrates each page on its own
// subject, so two moves that are each sound — A under B on one node, B under
// A on another — are both accepted, and so is a move decided just before its
// parent's purge. Refusing inside the apply would stop the whole log on every
// node over one page, which is why the applier salvages; writing the pointer
// would leave the pages reachable from nowhere. So every node files the page
// where it can, from the same rows at the same position.
func TestAParentAPageCannotHoldIsSalvagedAtTheApply(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	for _, c := range []struct{ id, container, title string }{
		{"page-a", "ENG", "A"}, {"page-b", "ENG", "B"}, {"page-p", "ENG", "P"},
		{"page-o", "OPS", "O"},
	} {
		if _, _, err := h.apply(create(c.id, c.container, c.title, "x")); err != nil {
			t.Fatalf("create %s: %v", c.id, err)
		}
	}
	move := func(opID, page, parent string) {
		t.Helper()
		rec := record(pages.PageSubject(page), pages.OpPatch, opID,
			pages.PagePatch{V: pages.DocumentVersion, ParentID: &parent},
			pages.ScopeSet{Subject: true, Container: "ENG"})
		if _, gate, err := h.apply(rec); err != nil || gate != "" {
			t.Fatalf("apply the move of %s under %s: %v (gate %q) — the "+
				"applier refused a record the broker committed, which stops "+
				"the log on every node", page, parent, err, gate)
		}
	}
	parentOf := func(page string) string {
		t.Helper()
		column := h.scalar(`SELECT parent_id FROM pages_heads WHERE id = ?`, page)
		document := h.scalar(`SELECT json_extract(document, '$.parent_id') FROM pages_heads WHERE id = ?`, page)
		if column != document {
			t.Fatalf("%s's parent column says %q and its document %q", page, column, document)
		}
		return column
	}

	// TWO MOVES THAT EACH MAKE THE OTHER AN ANCESTOR. The first is sound
	// and lands; the second would close the cycle and leaves B where it was.
	move("op-a-under-b", "page-a", "page-b")
	move("op-b-under-a", "page-b", "page-a")
	if got := parentOf("page-a"); got != "page-b" {
		t.Fatalf("A is under %q, want B — the first move was sound", got)
	}
	if got := parentOf("page-b"); got != "" {
		t.Fatalf("B is under %q: the second move closed a cycle, and a page "+
			"in one is reachable from nowhere", got)
	}

	// A MOVE UNDER A PAGE PURGED AFTER IT WAS DECIDED, and one under a page
	// in another container (a record no sound writer produces).
	purge := record(pages.PageSubject("page-p"), pages.OpPurge, "op-purge-p",
		pages.StatusPayload{V: pages.DocumentVersion, Reason: "gone"},
		pages.ScopeSet{Terms: []pages.ScopeTerm{{Kind: pages.TermContainer, ID: "ENG"}}})
	if _, _, err := h.apply(purge); err != nil {
		t.Fatalf("purge: %v", err)
	}
	move("op-a-under-purged", "page-a", "page-p")
	move("op-a-under-ops", "page-a", "page-o")
	if got := parentOf("page-a"); got != "page-b" {
		t.Fatalf("A is under %q after moves under a purged page and a page "+
			"elsewhere, want B, where it was", got)
	}

	// A CREATE UNDER A PARENT IT CANNOT HOLD lands at its container's top.
	orphan := create("page-c", "ENG", "C", "x")
	orphan.Mutation = mustJSON(t, pages.CreatePayload{
		V: pages.DocumentVersion, PageID: "page-c", Container: "ENG",
		Title: "C", Body: "x", Status: pages.StatusPublished, Author: "ada",
		ParentID: "page-p",
	})
	if _, _, err := h.apply(orphan); err != nil {
		t.Fatalf("create under a purged page: %v", err)
	}
	if got := parentOf("page-c"); got != "" {
		t.Fatalf("a page created under a purged one is filed under %q, which "+
			"no walk of its container reaches", got)
	}

	// A TRASHED PARENT IS NOT A BROKEN POINTER: the trash is reversible,
	// and trashing a page with children leaves this same state.
	trash := record(pages.PageSubject("page-b"), pages.OpTombstone, "op-trash-b",
		pages.StatusPayload{V: pages.DocumentVersion},
		pages.ScopeSet{Subject: true, Container: "ENG"})
	if _, _, err := h.apply(trash); err != nil {
		t.Fatalf("trash: %v", err)
	}
	move("op-c-under-trashed", "page-c", "page-b")
	if got := parentOf("page-c"); got != "page-b" {
		t.Fatalf("a move under a trashed page was salvaged to %q — the trash "+
			"is reversible, and only the decide refuses it", got)
	}
}

// A PURGED PAGE'S CHILDREN ARE RE-FILED UNDER ITS OWN PARENT, not destroyed
// and not orphaned.
//
// A purge destroys one page. Its children used to go on pointing at the row it
// deleted, which is filed nowhere a walk of the container reaches, with
// nothing on the row to say so. Each is moved to the grandparent — or the
// container's top — through `scoped_through` rather than `version`, because
// the write comes from another subject, and a redelivery of the purge finds
// nothing left to move.
func TestAPurgedPagesChildrenAreRefiledUnderItsOwnParent(t *testing.T) {
	t.Parallel()
	h := newHarness(t, nil)
	for _, id := range []string{"page-g", "page-p", "page-c1", "page-c2"} {
		if _, _, err := h.apply(create(id, "ENG", strings.ToUpper(id), "x")); err != nil {
			t.Fatalf("create %s: %v", id, err)
		}
	}
	file := func(opID, page, parent string) {
		t.Helper()
		rec := record(pages.PageSubject(page), pages.OpPatch, opID,
			pages.PagePatch{V: pages.DocumentVersion, ParentID: &parent},
			pages.ScopeSet{Subject: true, Container: "ENG"})
		if _, _, err := h.apply(rec); err != nil {
			t.Fatalf("file %s under %s: %v", page, parent, err)
		}
	}
	file("op-p", "page-p", "page-g")
	file("op-c1", "page-c1", "page-p")
	file("op-c2", "page-c2", "page-p")
	versionBefore := h.scalar(`SELECT version FROM pages_heads WHERE id = ?`, "page-c1")

	purge := record(pages.PageSubject("page-p"), pages.OpPurge, "op-purge",
		pages.StatusPayload{V: pages.DocumentVersion, Reason: "superseded"},
		pages.ScopeSet{Terms: []pages.ScopeTerm{{Kind: pages.TermContainer, ID: "ENG"}}})
	if _, _, err := h.apply(purge); err != nil {
		t.Fatalf("purge: %v", err)
	}
	for _, child := range []string{"page-c1", "page-c2"} {
		column := h.scalar(`SELECT parent_id FROM pages_heads WHERE id = ?`, child)
		document := h.scalar(`SELECT json_extract(document, '$.parent_id') FROM pages_heads WHERE id = ?`, child)
		if column != "page-g" || document != "page-g" {
			t.Fatalf("%s is filed under %q (document %q) after its parent's "+
				"purge, want the grandparent", child, column, document)
		}
	}
	if got := h.scalar(`SELECT version FROM pages_heads WHERE id = ?`, "page-c1"); got != versionBefore {
		t.Fatalf("the re-file stamped c1's version %s -> %s, poisoning its own "+
			"broker expectation — a write from another subject moves "+
			"scoped_through", versionBefore, got)
	}
	if got := h.scalar(`SELECT scoped_through FROM pages_heads WHERE id = ?`, "page-c1"); got == "0" {
		t.Fatal("the re-file did not stamp c1's scoped_through, so a read " +
			"barrier on c1 cannot see a change the purge made to it")
	}
	// A REDELIVERY OF THE PURGE MOVES NOTHING MORE.
	if rows, _, err := h.applyAt(purge, h.seq+1); err != nil {
		t.Fatalf("redeliver the purge: %v", err)
	} else if got := h.scalar(`SELECT parent_id FROM pages_heads WHERE id = ?`,
		"page-c2"); got != "page-g" {
		t.Fatalf("a redelivered purge moved c2 to %q (%d rows)", got, rows)
	}
}

// mustJSON encodes a payload for a hand-built record.
func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	body, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode %T: %v", v, err)
	}
	return body
}
