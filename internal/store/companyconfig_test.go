package store_test

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
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
	db := storetest.OpenNode(t, filepath.Join(t.TempDir(), "c.db"), store.Options{})
	t.Cleanup(func() { _ = db.Close() })
	return db.Configs()
}

// write stores a revision at a given age, returning its id. A revision this
// node boots on is served at its creation instant, as the reconciler serves
// the one it applied.
func write(t *testing.T, c *store.Configs, parent, body string, age time.Duration, boots bool) string {
	t.Helper()
	rev := store.Revision{
		ParentID: parent, CreatedAt: time.Now().UTC().Add(-age),
		CreatedBy: "test", CreatedByKind: iam.ActorSystem, Source: "test", Summary: body,
		Payload: json.RawMessage(fmt.Sprintf(`{"name":%q}`, body)),
	}
	id, err := c.Insert(t.Context(), rev)
	if err != nil {
		t.Fatalf("write %s: %v", body, err)
	}
	if boots {
		rev.ID, rev.ActivatedAt = id, rev.CreatedAt
		if err = c.Serve(t.Context(), rev); err != nil {
			t.Fatalf("serve %s: %v", body, err)
		}
	}
	return id
}

// THE BOOT ROW AND ITS WHOLE ANCESTRY SURVIVE THE SWEEP, HOWEVER OLD.
//
// A revert re-activates an older revision by id and `crewlet config diff`
// walks the chain, so a deleted ancestor turns both into an error naming a
// row that used to exist. A company that has not changed its configuration
// for a year also boots on a revision older than any horizon worth setting,
// which is why the chain is excluded by id rather than by date.
func TestTheSweepKeepsTheBootChainWhateverItsAge(t *testing.T) {
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
	// reachable from the boot row.
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

// WHAT THE NODE BOOTS ON, WHAT IT LAST SAW THE FLEET ON, BOTH CHAINS AND ANY
// REVISION ACTIVATED INSIDE THE WINDOW SURVIVE THE SWEEP; nothing else past the
// horizon does.
//
// The fleet's row is usually the boot row, and the case this guards is the one
// where it is not: a node that refused the fleet's revision runs an older one,
// and the fleet's is what it restores when the pointer is gone — swept, the
// node would restore the older revision it runs over every node that runs the
// newer. And an old revision the fleet re-activated yesterday is the company,
// whatever its creation says, before this node's reconciler has recorded it.
//
// Mutation: drop the fleet's row from the kept set, or the activation term
// from the sweep's predicate, and a kept row here goes.
func TestTheSweepKeepsWhatTheNodeBootsOnAndWhatItSawTheFleetOn(t *testing.T) {
	t.Parallel()
	c := configsFor(t)

	const old = 500 * 24 * time.Hour
	bootParent := write(t, c, "", "boot-parent", old+time.Hour, false)
	write(t, c, bootParent, "boot", old, true)
	fleetsParent := write(t, c, "", "fleets-parent", old+2*time.Hour, false)
	fleets := write(t, c, fleetsParent, "fleets", old+time.Hour, false)
	kept, _, err := c.Get(t.Context(), fleets)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	kept.ActivatedAt = kept.CreatedAt
	if err = c.SawFleet(t.Context(), kept); err != nil {
		t.Fatalf("saw: %v", err)
	}
	reactivated := write(t, c, "", "reactivated", old, false)
	if err = c.NoteActivation(t.Context(), reactivated,
		time.Now().UTC().Add(-24*time.Hour)); err != nil {
		t.Fatalf("note: %v", err)
	}
	doomed := write(t, c, "", "doomed", old, false)

	n, err := c.Purge(t.Context(), time.Now().UTC().Add(-400*24*time.Hour))
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 1 {
		t.Errorf("the sweep deleted %d rows, want only the unrelated one", n)
	}
	if _, found, _ := c.Get(t.Context(), doomed); found {
		t.Error("an unrelated revision past the horizon survived")
	}
	for _, id := range []string{bootParent, fleetsParent, fleets, reactivated} {
		if _, found, err := c.Get(t.Context(), id); err != nil || !found {
			t.Errorf("revision %s went (found=%v, err=%v)", id, found, err)
		}
	}
}

// A RE-ACTIVATION THIS NODE REFUSED STILL DATES THE REVISION IT RAN.
//
// A node serves R, the fleet re-activates R to rotate a credential, and this
// node's apply of it fails: R stays what it boots on, at the instant it
// applied. The fleet then moves to Q, which this node serves, and R is history
// — re-activated inside the horizon, so the sweep must keep it. With one
// column for both instants the boot row could not record the re-activation,
// R went into history dated by the old one, and a horizon between the two
// deleted a revision the fleet activated yesterday.
//
// Mutation: put `is_active = 0` back on the raise, and R goes.
func TestASweepDatesTheBootRowByTheFleetsLatestActivation(t *testing.T) {
	t.Parallel()
	c := configsFor(t)

	now := time.Now().UTC().Truncate(time.Microsecond)
	applied, reactivated := now.Add(-500*24*time.Hour), now.Add(-24*time.Hour)
	r := store.Revision{
		ID: "r", CreatedAt: applied, CreatedBy: "test", CreatedByKind: iam.ActorSystem,
		Source: "test", Summary: "r", Payload: json.RawMessage(`{"name":"r"}`),
		ActivatedAt: applied,
	}
	if err := c.Serve(t.Context(), r); err != nil {
		t.Fatalf("serve r: %v", err)
	}
	r.ActivatedAt = reactivated
	if err := c.SawFleet(t.Context(), r); err != nil {
		t.Fatalf("saw r re-activated: %v", err)
	}
	q := r
	q.ID, q.Summary, q.Payload, q.ActivatedAt = "q", "q", json.RawMessage(`{"name":"q"}`), now
	if err := c.SawFleet(t.Context(), q); err != nil {
		t.Fatalf("saw q: %v", err)
	}
	if err := c.Serve(t.Context(), q); err != nil {
		t.Fatalf("serve q: %v", err)
	}
	if _, err := c.Purge(t.Context(), now.Add(-400*24*time.Hour)); err != nil {
		t.Fatalf("purge: %v", err)
	}
	got, found, err := c.Get(t.Context(), "r")
	if err != nil || !found {
		t.Fatalf("r was swept (found=%v err=%v), though the fleet re-activated it a day ago", found, err)
	}
	if !got.ActivatedAt.Equal(reactivated) {
		t.Errorf("r is dated %s, want the re-activation's %s", got.ActivatedAt, reactivated)
	}
}

// A REVISION KEEPS ITS AUTHOR AND THE CREDENTIAL APART, through every write
// and every read — including the copy a node keeps of its FLEET's revision,
// which is the only copy every node but the writer's ever holds. Mutation: drop
// either column from the insert or the scan and a case here fails.
func TestARevisionKeepsItsAuthorAndTheCredentialApart(t *testing.T) {
	t.Parallel()
	c := configsFor(t)
	const pat = "pat:0192f00d-0000-7000-8000-00000000000a"
	id, err := c.Offer(t.Context(), store.Offer{}, store.Revision{
		CreatedBy: "jane.doe", CreatedByKind: "operator", OperatorID: pat,
		Source: "api", Summary: "written", CreatedAt: time.Now().UTC(),
		Payload: json.RawMessage(`{"name":"written"}`),
	})
	if err != nil {
		t.Fatalf("offer: %v", err)
	}
	adopted := uuid.NewString()
	if err := c.SawFleet(t.Context(), store.Revision{
		ID: adopted, Source: "fleet", Summary: "adopted",
		CreatedBy: "sam", CreatedByKind: "human",
		OperatorID: "session:0192f00d-0000-7000-8000-0000000000b0",
		CreatedAt:  time.Now().UTC(), ActivatedAt: time.Now().UTC(),
		Payload: json.RawMessage(`{"name":"adopted"}`),
	}); err != nil {
		t.Fatalf("saw: %v", err)
	}
	for _, want := range []struct {
		id, by   string
		kind     iam.ActorKind
		operator string
	}{
		{id, "jane.doe", iam.ActorOperator, pat},
		{adopted, "sam", iam.ActorHuman, "session:0192f00d-0000-7000-8000-0000000000b0"},
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

// AND A NODE WITH NOTHING TO BOOT ON SWEEPS WITHOUT IT.
//
// No boot row is a real state — the engine boots unconfigured, the API
// serves /config, and the first import populates the company — so a sweep
// that needed a boot row to protect would fail every tick on a node waiting
// for its first import.
func TestTheSweepRunsWithNothingToBootOn(t *testing.T) {
	t.Parallel()
	c := configsFor(t)

	write(t, c, "", "orphan", 500*24*time.Hour, false)
	n, err := c.Purge(t.Context(), time.Now().UTC().Add(-400*24*time.Hour))
	if err != nil {
		t.Fatalf("purge with nothing to boot on: %v", err)
	}
	if n != 1 {
		t.Errorf("deleted %d rows, want the one past the horizon", n)
	}
}

// AND A REVISION THAT NAMES ITSELF AS ITS PARENT TERMINATES THE SWEEP.
//
// # Why this is reachable rather than paranoia
//
// A copy of a fleet revision takes BOTH the id and the parent id from its
// caller — the pointer's record, not a row this node minted — and nothing in
// the schema forbids a row from naming itself. A self-referencing row
// satisfies the foreign key, because the row it points at exists.
//
// What that costs without a visited set is not a wrong answer: it is an
// infinite loop inside a maintenance tick, holding the sweep for ever on a
// node that otherwise looks healthy. Nothing writes one today, which is
// exactly why nothing would notice it.
func TestTheSweepTerminatesOnARevisionThatIsItsOwnParent(t *testing.T) {
	t.Parallel()
	c := configsFor(t)

	const id = "11111111-1111-1111-1111-111111111111"
	now := time.Now().UTC()
	if err := c.Serve(t.Context(), store.Revision{
		ID: id, ParentID: id, CreatedBy: "peer", CreatedByKind: iam.ActorOperator,
		Source: "fleet", CreatedAt: now, ActivatedAt: now,
		Payload: json.RawMessage(`{}`),
	}); err != nil {
		t.Fatalf("serve a self-parented revision: %v", err)
	}

	// A DEADLINE RATHER THAN A PLAIN CALL, because the failure this
	// guards is a hang: without the visited set the assertion below is
	// never reached and the case times out with the whole package's
	// output attached to it.
	done := make(chan error, 1)
	go func() {
		_, err := c.Purge(t.Context(), time.Now().UTC().Add(-400*24*time.Hour))
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("purge: %v", err)
		}
		if _, found, err := c.Get(t.Context(), id); err != nil || !found {
			t.Errorf("the boot row went (found=%v, err=%v)", found, err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the sweep did not terminate on a revision that is its own " +
			"parent, which hangs the maintenance sweep for ever on a node " +
			"that otherwise looks healthy")
	}
}
