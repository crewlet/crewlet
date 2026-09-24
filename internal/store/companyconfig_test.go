package store_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/store"
)

// THE TWO WRITES THAT ARE NOT APPENDS, and the one thing that bounds each.
//
// Every other write to company_config appends: importing writes a new row,
// activating appends to the pointer, and that is what makes the history a
// record rather than a claim. The scrub rewrites a row in place and the sweep
// deletes rows — so each needs a bound that holds in the database rather than
// in whatever remembered to check.

func configsFor(t *testing.T) *store.Configs {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "c.db"), store.Options{})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db.Configs()
}

// write stores a revision at a given age, returning its id.
func write(t *testing.T, c *store.Configs, parent, body string, age time.Duration, active bool) string {
	t.Helper()
	rev := store.Revision{
		ParentID: parent, CreatedAt: time.Now().UTC().Add(-age),
		CreatedBy: "test", Source: "test", Summary: body,
		Payload: json.RawMessage(fmt.Sprintf(`{"name":%q}`, body)),
	}
	var id string
	var err error
	if active {
		id, err = c.InsertActive(t.Context(), rev)
	} else {
		id, err = c.Insert(t.Context(), rev)
	}
	if err != nil {
		t.Fatalf("write %s: %v", body, err)
	}
	return id
}

// THE ACTIVE REVISION CANNOT BE SCRUBBED, AND THE STATEMENT IS WHAT REFUSES.
//
// The fleet is serving that document and every node is holding it. Rewriting
// it underneath them would be a configuration change nothing activated — no
// epoch, no apply, no event — so the company would run on a document its
// operator never wrote. The clause is in the UPDATE rather than in a check
// before it, because asking first and writing after is a race with an
// activation and the clause is what actually holds.
func TestTheScrubIsRefusedOnTheActiveRevision(t *testing.T) {
	t.Parallel()
	c := configsFor(t)

	old := write(t, c, "", "first", 2*time.Hour, false)
	live := write(t, c, old, "second", time.Hour, true)

	err := c.Scrub(t.Context(), live, json.RawMessage(`{"name":"rewritten"}`), time.Now().UTC())
	if !errors.Is(err, store.ErrRevisionIsActive) {
		t.Fatalf("scrubbing the active revision answered %v, want the refusal", err)
	}
	// AND IT WROTE NOTHING. A refusal that had already rewritten the row
	// would be the worse half of the failure it is here to prevent.
	rev, found, err := c.Get(t.Context(), live)
	if err != nil || !found {
		t.Fatalf("read back: %v (found=%v)", err, found)
	}
	if string(rev.Payload) != `{"name":"second"}` {
		t.Errorf("the active payload is %s, want the original", rev.Payload)
	}
	if !rev.ScrubbedAt.IsZero() {
		t.Error("a refused scrub stamped scrubbed_at")
	}
}

// A SUPERSEDED REVISION IS REWRITTEN AND STAMPED.
//
// The stamp is what tells the next reader that a diff showing a tombstone is
// a scrub rather than corruption — which is the whole reason the narrowed
// immutability is recorded in a column instead of happening quietly.
func TestASupersededRevisionIsScrubbedAndStamped(t *testing.T) {
	t.Parallel()
	c := configsFor(t)

	old := write(t, c, "", "first", 2*time.Hour, false)
	write(t, c, old, "second", time.Hour, true)

	at := time.Now().UTC().Truncate(time.Millisecond)
	if err := c.Scrub(t.Context(), old, json.RawMessage(`{"name":"__scrubbed__"}`), at); err != nil {
		t.Fatalf("scrub: %v", err)
	}
	rev, found, err := c.Get(t.Context(), old)
	if err != nil || !found {
		t.Fatalf("read back: %v (found=%v)", err, found)
	}
	if string(rev.Payload) != `{"name":"__scrubbed__"}` {
		t.Errorf("payload = %s", rev.Payload)
	}
	if rev.ScrubbedAt.IsZero() {
		t.Error("the row carries no scrub time, so a diff across it reads as " +
			"corruption rather than as an erasure somebody ran")
	}
}

// AND A REVISION THAT IS NOT THERE IS SAID SO, not reported as active.
//
// Both causes answer with the same zero rows affected, and telling them apart
// is what makes the message actionable: one is a typo in an id, the other is
// a refusal with a procedure behind it.
func TestScrubbingARevisionThatIsNotThere(t *testing.T) {
	t.Parallel()
	c := configsFor(t)

	err := c.Scrub(t.Context(), "no-such-revision", json.RawMessage(`{}`), time.Now().UTC())
	if !errors.Is(err, store.ErrNoRevision) {
		t.Fatalf("answered %v, want the missing-revision error", err)
	}
}

// THE ACTIVE REVISION AND ITS WHOLE ANCESTRY SURVIVE THE SWEEP, HOWEVER OLD.
//
// A revert re-activates an older revision by id and `crewlet config diff`
// walks the chain, so a deleted ancestor turns both into an error naming a
// row that used to exist. A company that has not changed its configuration
// for a year also has an ACTIVE revision older than any horizon worth
// setting, which is why the chain is excluded by id rather than by date.
func TestTheSweepKeepsTheActiveChainWhateverItsAge(t *testing.T) {
	t.Parallel()
	c := configsFor(t)

	const old = 500 * 24 * time.Hour
	first := write(t, c, "", "first", old+2*time.Hour, false)
	second := write(t, c, first, "second", old+time.Hour, false)
	live := write(t, c, second, "third", old, true)

	n, err := c.Purge(t.Context(), time.Now().UTC().Add(-400*24*time.Hour))
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 0 {
		t.Errorf("the sweep deleted %d rows of a three-revision chain, all of "+
			"which a revert or a diff can still reach", n)
	}
	for _, id := range []string{first, second, live} {
		if _, found, err := c.Get(t.Context(), id); err != nil || !found {
			t.Errorf("revision %s went (found=%v, err=%v)", id, found, err)
		}
	}
}

// AN OFF-CHAIN BRANCH PAST THE HORIZON GOES, AND ITS FOREIGN KEYS DO NOT
// STOP IT.
//
// # Why this case exists
//
// parent_revision_id is a real foreign key and this database runs with
// `PRAGMA foreign_keys = ON`, so deleting a row something still points at
// FAILS. Off-chain revisions point at each other — a reverted branch is
// exactly that shape — so a bare delete aborts the whole tick on the first
// row whose child has not gone yet, and the table never shrinks while the
// sweep's log line says it ran.
func TestAnAbandonedBranchIsSweptDespiteItsParentPointers(t *testing.T) {
	t.Parallel()
	c := configsFor(t)

	const old = 500 * 24 * time.Hour
	base := write(t, c, "", "base", old+3*time.Hour, false)
	// THE ABANDONED BRANCH: two revisions chained to each other, neither
	// reachable from the active one.
	branch1 := write(t, c, base, "branch-1", old+2*time.Hour, false)
	branch2 := write(t, c, branch1, "branch-2", old+time.Hour, false)
	// THE REVERT: the fleet goes back to base, and everything after
	// chains from there.
	live := write(t, c, base, "reverted", old, true)

	n, err := c.Purge(t.Context(), time.Now().UTC().Add(-400*24*time.Hour))
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 2 {
		t.Errorf("the sweep deleted %d rows, want the two abandoned ones", n)
	}
	for _, id := range []string{branch1, branch2} {
		if _, found, _ := c.Get(t.Context(), id); found {
			t.Errorf("abandoned revision %s survived", id)
		}
	}
	for _, id := range []string{base, live} {
		if _, found, _ := c.Get(t.Context(), id); !found {
			t.Errorf("chain revision %s went", id)
		}
	}
}

// A SURVIVOR WHOSE PARENT WAS SWEPT POINTS AT NOTHING RATHER THAN AT A ROW
// THAT IS GONE.
//
// Nulling the pointer is the truthful repair rather than a way round the
// constraint: the ancestor is gone, so a pointer at it is a lie, and the
// column is already nullable because the first revision ever written has no
// parent.
func TestASurvivorsParentPointerIsClearedWhenItsParentGoes(t *testing.T) {
	t.Parallel()
	c := configsFor(t)

	const old = 500 * 24 * time.Hour
	ancient := write(t, c, "", "ancient", old, false)
	// RECENT AND OFF-CHAIN: inside the horizon, so it stays, while its
	// parent is outside it and goes.
	survivor := write(t, c, ancient, "survivor", time.Hour, false)
	write(t, c, "", "live", time.Minute, true)

	if _, err := c.Purge(t.Context(), time.Now().UTC().Add(-400*24*time.Hour)); err != nil {
		t.Fatalf("purge: %v", err)
	}
	rev, found, err := c.Get(t.Context(), survivor)
	if err != nil || !found {
		t.Fatalf("the survivor went: %v (found=%v)", err, found)
	}
	if rev.ParentID != "" {
		t.Errorf("the survivor still points at %s, which no longer exists — "+
			"a diff or a chain walk from here names a row nobody can open",
			rev.ParentID)
	}
}

// THE CHAIN STOPS AT A BREAK RATHER THAN FAILING.
//
// A node that joins a running fleet fetches the active revision alone, so its
// chain is broken from the start. That is an ordinary state rather than
// damage: the sweep protects what it can reach and the horizon covers the
// rest.
func TestTheChainStopsAtAParentThisNodeNeverAdopted(t *testing.T) {
	t.Parallel()
	c := configsFor(t)

	id := write(t, c, "", "adopted-alone", time.Hour, true)
	// A PARENT NOTHING HOLDS, which is what a fetch of the active revision
	// alone produces on a joining node.
	if err := c.Adopt(t.Context(), store.Revision{
		ID: id, ParentID: "", CreatedBy: "peer", Source: "fleet",
		Payload: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	chain, err := c.Chain(t.Context())
	if err != nil {
		t.Fatalf("chain: %v", err)
	}
	if len(chain) != 1 || chain[0].ID != id {
		t.Errorf("chain = %+v, want the one revision this node holds", chain)
	}
}

// A REVISION KEEPS ITS AUTHOR AND THE CREDENTIAL APART, through every write
// and every read — including the copy a node ADOPTS from its fleet, which is
// the only copy every node but the writer's ever holds. Mutation: drop either
// column from the insert, the adopt or the scan and a case here fails.
func TestARevisionKeepsItsAuthorAndTheCredentialApart(t *testing.T) {
	t.Parallel()
	c := configsFor(t)
	const pat = "pat:0192f00d-0000-7000-8000-00000000000a"
	id, err := c.InsertActive(t.Context(), store.Revision{
		CreatedBy: "jane.doe", CreatedByKind: "operator", OperatorID: pat,
		Source: "api", Summary: "written", CreatedAt: time.Now().UTC(),
		Payload: json.RawMessage(`{"name":"written"}`),
	})
	if err != nil {
		t.Fatalf("insert: %v", err)
	}
	adopted := uuid.NewString()
	if err := c.Adopt(t.Context(), store.Revision{
		ID: adopted, ParentID: id, Source: "fleet", Summary: "adopted",
		CreatedBy: "sam", CreatedByKind: "human",
		OperatorID: "session:0192f00d-0000-7000-8000-0000000000b0",
		CreatedAt:  time.Now().UTC(), Payload: json.RawMessage(`{"name":"adopted"}`),
	}); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	for _, want := range []struct {
		id, by, kind, operator string
	}{
		{id, "jane.doe", "operator", pat},
		{adopted, "sam", "human", "session:0192f00d-0000-7000-8000-0000000000b0"},
	} {
		got, found, err := c.Get(t.Context(), want.id)
		if err != nil || !found {
			t.Fatalf("get %s: %v (found=%v)", want.id, err, found)
		}
		if got.CreatedBy != want.by || got.CreatedByKind != want.kind ||
			got.OperatorID != want.operator {
			t.Errorf("%s reads back %q/%q/%q, want %q/%q/%q", want.id, got.CreatedBy,
				got.CreatedByKind, got.OperatorID, want.by, want.kind, want.operator)
		}
	}
	listed, err := c.List(t.Context(), 0, 0)
	if err != nil || len(listed) != 2 || listed[0].OperatorID == "" ||
		listed[1].OperatorID == "" {
		t.Errorf("the listing reads %+v (%v), want both credentials", listed, err)
	}
}

// AND A COMPANY WITH NO ACTIVE REVISION SWEEPS WITHOUT ONE.
//
// Zero rows active is a real state — the engine boots, the API serves
// /config, and the first import populates the company — so a sweep that
// needed an active revision to protect would fail every tick on a node
// waiting for its first import.
func TestTheSweepRunsWithNoActiveRevision(t *testing.T) {
	t.Parallel()
	c := configsFor(t)

	write(t, c, "", "orphan", 500*24*time.Hour, false)
	n, err := c.Purge(t.Context(), time.Now().UTC().Add(-400*24*time.Hour))
	if err != nil {
		t.Fatalf("purge with nothing active: %v", err)
	}
	if n != 1 {
		t.Errorf("deleted %d rows, want the one past the horizon", n)
	}
}

// AND A REVISION THAT NAMES ITSELF AS ITS PARENT TERMINATES THE WALK.
//
// # Why this is reachable rather than paranoia
//
// [Configs.Adopt] takes BOTH the id and the parent id from its caller — it is
// the peer path, recording a revision this node fetched from the fleet — so
// the pointer is not minted here and nothing in the schema forbids a row from
// naming itself. A self-referencing row satisfies the foreign key, because
// the row it points at exists.
//
// What that costs without a visited set is not a wrong answer: it is an
// infinite loop inside a maintenance tick, holding the sweep for ever on a
// node that otherwise looks healthy. Nothing writes one today, which is
// exactly why nothing would notice it.
func TestTheChainTerminatesOnARevisionThatIsItsOwnParent(t *testing.T) {
	t.Parallel()
	c := configsFor(t)

	const id = "11111111-1111-1111-1111-111111111111"
	if err := c.Adopt(t.Context(), store.Revision{
		ID: id, ParentID: id, CreatedBy: "peer", Source: "fleet",
		CreatedAt: time.Now().UTC(), Payload: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("adopt a self-parented revision: %v", err)
	}
	if _, err := c.Activate(t.Context(), id, time.Now().UTC()); err != nil {
		t.Fatalf("activate: %v", err)
	}

	// A DEADLINE RATHER THAN A PLAIN CALL, because the failure this
	// guards is a hang: without the visited set the assertion below is
	// never reached and the case times out with the whole package's
	// output attached to it.
	done := make(chan []store.Revision, 1)
	go func() {
		chain, err := c.Chain(t.Context())
		if err != nil {
			t.Errorf("chain: %v", err)
		}
		done <- chain
	}()
	select {
	case chain := <-done:
		if len(chain) != 1 {
			t.Errorf("chain = %d revisions, want the one row that exists", len(chain))
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the chain walk did not terminate on a revision that is its " +
			"own parent, which hangs the maintenance sweep for ever on a node " +
			"that otherwise looks healthy")
	}
}
