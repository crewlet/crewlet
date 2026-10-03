package pages_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
)

// keyedSince is the instant the unit of work behind every key here began: an
// hour before the test, as a turn's earliest trigger or a request's own mint
// would be by the time a retry arrives.
var keyedSince = time.Now().Add(-time.Hour).UTC().Truncate(time.Millisecond)

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
	key := pages.CallKey{Seed: "req-7c1d4e2a-5b3f-4a6c-9d8e-0f1a2b3c4d5e", Since: keyedSince}

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
		"another key is another operation": {key,
			pages.CallKey{Seed: "req-other", Since: keyedSince}, false},
		"no key is another operation": {key, pages.CallKey{}, false},
		// AND WITH NO KEY, TWICE, is two operations: nothing will repeat
		// a call that named none, and a stable id would collapse every
		// later write into the first.
		"no key twice is two operations": {pages.CallKey{}, pages.CallKey{}, false},
		// A REPEAT COUNT IS PART OF THE IDENTITY: the same seed's second
		// call after a different one is not the first call's retry.
		"another repeat is another operation": {key,
			pages.CallKey{Seed: key.Seed, Since: key.Since, Repeat: 1}, false},
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

// A KEYED WRITE'S OPERATION CARRIES THE INSTANT ITS UNIT OF WORK BEGAN — never
// the call's own clock.
//
// The state log reads an operation's instant out of its id to decide whether
// this node's ledger can vouch for a retry of it; an id minted before the
// ledger's last loss whose row is gone is answered `unknown` rather than
// decided again. A keyed id derived with no instant at all reads as minted
// before every loss, so on any node whose ledger ever lost a row every keyed
// write answered `unknown` and never published; one carrying the call's clock
// reads as minted after the loss, and a re-run that lost its first attempt's
// row published the write a second time.
func TestAKeyedWriteCarriesItsKeysInstant(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	key := pages.CallKey{Seed: "wk-instant", Since: keyedSince}
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
	for write, id := range map[string]string{
		"create": created.ChangeID, "comment": commented.ChangeID,
		"save": saved.ChangeID, "rename": renamed.ChangeID,
		"comment edit": edited.ChangeID,
	} {
		minted, ok := statelog.OpMintedAt(id)
		if !ok {
			t.Errorf("the %s wrote under %q, which carries no instant — every "+
				"node whose ledger lost a row answers it `unknown` for ever", write, id)
			continue
		}
		if !minted.Equal(keyedSince) {
			t.Errorf("the %s's operation carries %s, want the key's own %s — "+
				"a re-run's clock is after the loss it must be judged against",
				write, minted, keyedSince)
		}
	}
}

// AN EDIT MADE AGAIN AFTER A DIFFERENT ONE CHANGES THE COMMENT, and its own
// retry is still one operation.
//
// A keyed write's operation names WHICH write it is, not WHEN: editing a
// remark to "blocked", then "unblocked", then "blocked" again derived one
// operation for the first and the third, and the third was answered as the
// first's retry — applied, at the first's position — while the comment still
// read "unblocked". The caller hands the store the call's repeat count in its
// run, and every derived id carries it, the edit's as much as a comment's.
func TestAnEditMadeAgainAfterAnotherChangesTheComment(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	page := r.write(author("jane"), pages.NewPage{Container: "ENG", Title: "Runbook", Body: "prose"})
	comment, _, err := r.store.Comment(t.Context(), author("jane"), page.Page.ID,
		pages.NewComment{Body: "starting"})
	if err != nil {
		t.Fatalf("comment: %v", err)
	}
	r.drain()
	first := pages.CallKey{Seed: "wk-edit", Since: keyedSince}
	later := pages.CallKey{Seed: "wk-edit", Since: keyedSince, Repeat: 1}

	var ids []string
	for _, step := range []struct {
		body string
		key  pages.CallKey
	}{
		{"blocked", first},
		{"unblocked", later},
		{"blocked", later},
		// ITS RETRY, which a re-run makes under the same count.
		{"blocked", later},
	} {
		_, written, err := r.store.EditComment(t.Context(), author("jane"),
			page.Page.ID, comment.ID, step.body, step.key)
		if err != nil {
			t.Fatalf("edit to %q: %v", step.body, err)
		}
		ids = append(ids, written.ChangeID)
		r.drain()
	}
	if got := r.get(page.Page.ID).Comments[0].Body; got != "blocked" {
		t.Fatalf("the comment reads %q, want the edit made again after "+
			"\"unblocked\" to have changed it back", got)
	}
	if ids[0] == ids[2] {
		t.Errorf("the edit made again after another wrote under the first's "+
			"operation %q", ids[0])
	}
	if ids[2] != ids[3] {
		t.Errorf("a retry of the same call wrote under %q, want its first "+
			"attempt's %q", ids[3], ids[2])
	}
}
