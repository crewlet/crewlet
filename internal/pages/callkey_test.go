package pages_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/pages"
)

// A KEYED WRITE DERIVES ITS OPERATION FROM THE KEY, and from nothing a retry
// could see differently. Before the key reached these writes every create,
// save, rename and comment edit minted a fresh operation id, so a request sent
// twice — a redelivered turn, a person pressing retry on an `unknown` — was
// two operations, and nothing downstream of the id could ever tell them apart.
//
// Asserted across two independent nodes' stores, because that is what a
// derivation is for: the retry may be served by a different node than the
// first attempt, in a later process, reading rows the first attempt already
// moved. Each keyed write here is sent to both and must come back under ONE
// operation — and a create under one page id — while a different key, or none,
// is a different operation every time.
func TestAKeyedWriteDerivesItsOperationFromTheKey(t *testing.T) {
	t.Parallel()
	const key = pages.CallKey("req-7c1d4e2a-5b3f-4a6c-9d8e-0f1a2b3c4d5e")

	// One run of every keyed write against one node, answering the change
	// id each one wrote under and the id of the page it created.
	run := func(t *testing.T, key pages.CallKey) (string, []string) {
		t.Helper()
		r := newRoundTrip(t)
		created, err := r.store.Create(t.Context(), author("jane"), pages.NewPage{
			Container: "ENG", Title: "Runbook", Body: "step one", CallKey: key,
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		r.drain()
		comment, commented, err := r.store.Comment(t.Context(), author("jane"),
			created.Page.ID, pages.NewComment{Body: "is this current?", CallKey: key})
		if err != nil {
			t.Fatalf("comment: %v", err)
		}
		r.drain()
		saved, err := r.store.SavePage(t.Context(), author("jane"), created.Page.ID,
			pages.Save{BaseVersion: 1, Body: ptr("step two"), CallKey: key})
		if err != nil {
			t.Fatalf("save: %v", err)
		}
		r.drain()
		renamed, err := r.store.Rename(t.Context(), author("jane"), created.Page.ID,
			"Deploy Book", false, key)
		if err != nil {
			t.Fatalf("rename: %v", err)
		}
		r.drain()
		_, edited, err := r.store.EditComment(t.Context(), author("jane"), created.Page.ID,
			comment.ID, "is this still current?", key)
		if err != nil {
			t.Fatalf("edit: %v", err)
		}
		return created.Page.ID, []string{created.ChangeID, commented.ChangeID,
			saved.ChangeID, renamed.ChangeID, edited.ChangeID}
	}
	writes := []string{"create", "comment", "save", "rename", "comment edit"}

	// EACH PAIR ON ITS OWN, in parallel: every write waits out the resolve
	// budget in this harness, and a sequence of runs would be minutes.
	for name, pair := range map[string]struct {
		a, b pages.CallKey
		same bool
	}{
		"one key on two nodes is one operation": {key, key, true},
		"another key is another operation":      {key, "req-other", false},
		"no key is another operation":           {key, "", false},
		// AND WITH NO KEY, TWICE, is two operations: nothing will repeat
		// a call that named none, and a stable id would collapse every
		// later write into the first.
		"no key twice is two operations": {"", "", false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pageA, opsA := run(t, pair.a)
			pageB, opsB := run(t, pair.b)
			if (pageA == pageB) != pair.same {
				t.Errorf("the creates made pages %q and %q, want the same page = %v "+
					"— a retry answering an id nothing holds, or two gestures one page",
					pageA, pageB, pair.same)
			}
			for i, write := range writes {
				if (opsA[i] == opsB[i]) != pair.same {
					t.Errorf("the %s wrote under %q and then %q, want one operation = %v",
						write, opsA[i], opsB[i], pair.same)
				}
			}
		})
	}
}
