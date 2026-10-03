package pages_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/pages"
	"github.com/crewlet/crewlet/internal/statelog"
)

// keyedSince is the instant the caller's operation key was minted: an hour
// before the test, as a request's own mint would be by the time its retry
// arrives.
var keyedSince = time.Now().Add(-time.Hour).UTC().Truncate(time.Millisecond)

// keyedAt is a person acting under an operation key minted at keyedSince, or
// under none when key is empty.
func keyedAt(key string) pages.Actor {
	actor := author("jane")
	actor.OpKey = key
	return actor
}

// A KEYED WRITE DERIVES ITS OPERATION FROM THE KEY, on every node alike.
//
// Asserted across two independent nodes' stores, because that is what a
// derivation is for: the retry may be served by a different node than the
// first attempt, in a later process, reading rows the first attempt already
// moved. Every keyed write here — a create, a remark, a save, a rename, an
// edit of the remark — is sent to both and must come back under ONE
// operation, and the create under one page id, while a different key, or none,
// is a different operation every time.
//
// Mutation: derive any of the five from anything but the key, the verb, the
// object and what the write says — a fresh id, the node, a row read at call
// time — and the one-key pair splits.
func TestAKeyedWriteDerivesItsOperationFromTheKey(t *testing.T) {
	t.Parallel()
	key := statelog.NewOpID(keyedSince, "")

	// One run of every keyed write against one node, answering the change
	// id each one wrote under and the id of the page it created.
	run := func(t *testing.T, actor pages.Actor) (string, []string) {
		t.Helper()
		r := newRoundTrip(t)
		created, err := r.store.Create(t.Context(), actor, pages.NewPage{
			Container: "ENG", Title: "Runbook", Body: "step one",
		})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		r.drain()
		comment, commented, err := r.store.Comment(t.Context(), actor,
			created.Page.ID, pages.NewComment{Body: "is this current?"})
		if err != nil {
			t.Fatalf("comment: %v", err)
		}
		r.drain()
		saved, err := r.store.SavePage(t.Context(), actor, created.Page.ID,
			pages.Save{BaseVersion: 1, Body: ptr("step two")})
		if err != nil {
			t.Fatalf("save: %v", err)
		}
		r.drain()
		renamed, err := r.store.Rename(t.Context(), actor, created.Page.ID,
			"Deploy Book", false)
		if err != nil {
			t.Fatalf("rename: %v", err)
		}
		r.drain()
		_, edited, err := r.store.EditComment(t.Context(), actor, created.Page.ID,
			comment.ID, "is this still current?")
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
		a, b string
		same bool
	}{
		"one key on two nodes is one operation": {key, key, true},
		"another key is another operation": {key,
			statelog.NewOpID(keyedSince, ""), false},
		"no key is another operation": {key, "", false},
		// AND WITH NO KEY, TWICE, is two operations: nothing will repeat
		// a call that named none, and a stable id would collapse every
		// later write into the first.
		"no key twice is two operations": {"", "", false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pageA, opsA := run(t, keyedAt(pair.a))
			pageB, opsB := run(t, keyedAt(pair.b))
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

// A KEYED WRITE'S OPERATION CARRIES THE INSTANT ITS KEY WAS MINTED — never the
// call's own clock.
//
// The state log reads an operation's instant out of its id to decide whether
// this node's ledger can vouch for a retry of it; an id minted before the
// ledger's last loss whose row is gone is answered `unknown` rather than
// decided again. A keyed id derived with no instant at all reads as minted
// before every loss, so on any node whose ledger ever lost a row every keyed
// write answered `unknown` and never published; one carrying the call's clock
// reads as minted after the loss, and a retry that lost its first attempt's
// row published the write a second time.
//
// Mutation: derive any of the five without the key's instant, or with the
// store's clock, and its write names the wrong one.
func TestAKeyedWriteCarriesItsKeysInstant(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	actor := keyedAt(statelog.NewOpID(keyedSince, ""))
	created, err := r.store.Create(t.Context(), actor, pages.NewPage{
		Container: "ENG", Title: "Runbook", Body: "step one",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	r.drain()
	comment, commented, err := r.store.Comment(t.Context(), actor,
		created.Page.ID, pages.NewComment{Body: "is this current?"})
	if err != nil {
		t.Fatalf("comment: %v", err)
	}
	r.drain()
	saved, err := r.store.SavePage(t.Context(), actor, created.Page.ID,
		pages.Save{BaseVersion: 1, Body: ptr("step two")})
	if err != nil {
		t.Fatalf("save: %v", err)
	}
	r.drain()
	renamed, err := r.store.Rename(t.Context(), actor, created.Page.ID,
		"Deploy Book", false)
	if err != nil {
		t.Fatalf("rename: %v", err)
	}
	r.drain()
	_, edited, err := r.store.EditComment(t.Context(), actor, created.Page.ID,
		comment.ID, "is this still current?")
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
				"a retry's clock is after the loss it must be judged against",
				write, minted, keyedSince)
		}
	}
}
