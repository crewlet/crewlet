package worklet_test

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/store/storetest"
	"github.com/crewlet/crewlet/internal/worklet"
)

// keyPath is a key's address in the scope alphabet, under its own project.
func keyPath(project, key string) statelog.ScopeSet {
	return statelog.ScopeSet{Paths: []string{
		worklet.ScopeTerm{Kind: worklet.TermKey, Container: project, ID: key}.Path(),
	}}
}

// EVERY RECORD THAT MOVES A KEY NAMES IT.
//
// Who holds a key is decided in log order (the comment atop keyscope.go), and
// records about different tasks reorder on a node that retained one unless
// their scopes meet. So a create, a re-keying move and a purge each state every
// key they claim, release or hand on: a purge named by its task alone overtakes
// a retained duplicate's create and hands the key elsewhere on that node, for
// good.
func TestEveryRecordThatMovesAKeyNamesIt(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	seedProject(t, r, worklet.Project{Key: "OPS", Name: "Operations"})
	task := r.createTask("re-keyed and purged")
	if task.Key != "ENG-1" {
		t.Fatalf("the premise: the task was filed as %s, want ENG-1", task.Key)
	}
	if _, err := r.writer.MoveTaskToProject(t.Context(), "op-move", task.ID, "OPS", nil); err != nil {
		t.Fatalf("move: %v", err)
	}
	r.drain()
	if moved := oneTask(t, r, task.ID); moved.Key != "OPS-1" {
		t.Fatalf("the premise: the task moved to %s, want OPS-1", moved.Key)
	}
	operator := r.writer.As("ops-1", worklet.AuthorOperator, worklet.Provenance{})
	if _, err := operator.PurgeTask(t.Context(), "op-purge", task.ID, "OPS",
		"a duplicate import"); err != nil {
		t.Fatalf("purge: %v", err)
	}
	r.drain()

	// EACH RECORD ON THE TASK'S SUBJECT, and the keys it must name.
	want := map[string][]statelog.ScopeSet{
		"the create": {keyPath("ENG", "ENG-1")},
		"the move":   {keyPath("ENG", "ENG-1"), keyPath("OPS", "OPS-1")},
		"the purge":  {keyPath("ENG", "ENG-1"), keyPath("OPS", "OPS-1")},
	}
	seen := map[string]bool{}
	for seq := uint64(1); seq <= r.logEnd(t); seq++ {
		rec := r.recordAt(t, seq)
		if rec.Subject != worklet.TaskSubject(task.ID) {
			continue
		}
		var which string
		switch rec.Op {
		case worklet.OpCreate:
			which = "the create"
		case worklet.OpPurge:
			which = "the purge"
		case worklet.OpPatch:
			var patch worklet.TaskPatch
			if err := json.Unmarshal(rec.Mutation, &patch); err != nil {
				t.Fatalf("decode the patch at %d: %v", seq, err)
			}
			if patch.Mint == nil {
				continue
			}
			which = "the move"
		default:
			continue
		}
		seen[which] = true
		scope := rec.Scope.Resolve(rec.Subject)
		for _, key := range want[which] {
			if !scope.Intersects(key) {
				t.Errorf("%s's scope %v does not meet %v — a record deferred under "+
					"that key would not hold it back", which, scope.Paths, key.Paths)
			}
		}
	}
	for which := range want {
		if !seen[which] {
			t.Errorf("found no record for %s on the task's subject", which)
		}
	}
}

// A PURGE IS HELD BACK BY A RECORD DEFERRED UNDER ANY KEY IT HANDS ON.
//
// The request's scope is both this writer's probe and the scope every other node
// defers the purge under, so a record retained under the task's current key, or
// under a former key in the project it left, must refuse it here as it would
// hold it back there.
func TestAPurgeIsHeldBackByARecordDeferredUnderAKeyItHandsOn(t *testing.T) {
	t.Parallel()
	for name, key := range map[string]struct{ project, key string }{
		"its current key":                      {"OPS", "OPS-1"},
		"a former key, in the project it left": {"ENG", "ENG-1"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			r := newRoundTrip(t)
			seedProject(t, r, worklet.Project{Key: "OPS", Name: "Operations"})
			task := r.createTask("moved and purged")
			if _, err := r.writer.MoveTaskToProject(t.Context(), "op-move", task.ID,
				"OPS", nil); err != nil {
				t.Fatalf("move: %v", err)
			}
			r.drain()
			// A DUPLICATE'S CREATE this node could not decode, filed
			// under the key it took.
			r.deferRecordAt("t-the-duplicate", keyPath(key.project, key.key).Paths[0])

			operator := r.writer.As("ops-1", worklet.AuthorOperator, worklet.Provenance{})
			_, err := operator.PurgeTask(t.Context(), "op-purge", task.ID, "OPS",
				"a duplicate import")
			var unavailable *statelog.Unavailable
			if !errors.As(err, &unavailable) || unavailable.Reason != statelog.ReasonDeferred {
				t.Fatalf("a purge handing on %s, which a deferred record covers = %v, "+
					"want it refused as deferred", key.key, err)
			}
		})
	}
}

// AN EDIT NEVER CLAIMS A KEY.
//
// Only a record that names a key may decide who holds it, and an edit names
// none. An apply that re-asserted a task's keys would let a duplicate that moved
// off a contested key (its alias claim never touched it) take that key on its
// first edit after the claimant's purge, under a scope naming no key, so a node
// that retained the edit handed the key elsewhere.
func TestAnEditNeverClaimsAKey(t *testing.T) {
	t.Parallel()
	h := newApplyHarness(t)
	at := time.Unix(1_700_000_100, 0).UTC()
	step := 0
	apply := func(rec worklet.MutationRecord) {
		t.Helper()
		step++
		if _, err := h.apply(rec, at.Add(time.Duration(step)*time.Second)); err != nil {
			t.Fatalf("apply %s: %v", rec.OpID, err)
		}
	}
	for _, id := range []string{"t-claimant", "t-duplicate"} {
		task := newTask(id)
		task.Key = "ENG-7"
		apply(taskRecord(id, worklet.OpCreate, task, nil))
	}
	// THE DUPLICATE MOVES OFF THE KEY it never held.
	former := []string{"ENG-7"}
	apply(taskRecord("t-duplicate", worklet.OpPatch, worklet.TaskPatch{
		Project: ptr("OPS"), FormerKeys: &former, Mint: &worklet.KeyMint{N: 1},
	}, nil))
	holder := func(key string) int64 {
		t.Helper()
		return h.value(`SELECT COUNT(*) FROM tracker_task_keys
			WHERE key = ? AND task_id = 't-duplicate'`, key)
	}
	if holder("OPS-1") != 1 {
		t.Fatal("the premise: the move did not claim the duplicate's new key")
	}
	purge := taskRecord("t-claimant", worklet.OpPurge,
		map[string]any{"reason": "the restored duplicate"}, nil)
	apply(purge)
	if got := h.value(`SELECT COUNT(*) FROM tracker_task_keys WHERE key = 'ENG-7'`); got != 0 {
		t.Fatalf("the premise: ENG-7 is claimed after its claimant's purge (%d rows), "+
			"although nothing holds it as its current key", got)
	}

	edit := taskRecord("t-duplicate", worklet.OpPatch,
		worklet.TaskPatch{Title: ptr("edited after the purge")}, nil)
	edit.OpID = "t-duplicate-edit"
	apply(edit)
	if holder("ENG-7") != 0 {
		t.Error("an edit to the duplicate claimed ENG-7 — a key it never held, " +
			"taken by a record whose scope names no key")
	}
	if holder("OPS-1") != 1 {
		t.Error("an edit to the duplicate lost the claim on its own key")
	}
}

// reKeyedBehind is two nodes on one log where the PEER has moved a task out of
// ENG and back, re-keying it twice, and THIS node has applied neither move: its
// read before a request names the first key alone, while its decide waits for
// the peer's appends and sees the keys held now. It answers this node, the task
// as this node read it, and the task as the peer left it.
func reKeyedBehind(t *testing.T) (*roundTrip, worklet.Task, worklet.Task) {
	t.Helper()
	a := newRoundTrip(t)
	seedProject(t, a, worklet.Project{Key: "OPS", Name: "Operations"})
	task := a.createTask("re-keyed behind this node")

	peerDBNode, peerDB := storetest.OpenEstate(t, filepath.Join(t.TempDir(), "peer.db"), store.Options{}, 1)
	t.Cleanup(func() { _ = peerDBNode.Close() })
	peer := newRoundTripOn(t, a.broker, a.log, peerDBNode, peerDB, "node-b")
	peer.drain()
	peer.applyWhileWriting()
	for i, target := range []string{"OPS", "ENG"} {
		if _, err := peer.writer.MoveTaskToProject(t.Context(),
			"op-peer-move-"+target, task.ID, target, nil); err != nil {
			t.Fatalf("the peer's move %d into %s: %v", i, target, err)
		}
		peer.drain()
	}
	moved := oneTask(t, peer, task.ID)
	if moved.Key == task.Key || len(moved.FormerKeys) != 2 {
		t.Fatalf("the premise: the peer re-keyed the task to %s with former keys %v",
			moved.Key, moved.FormerKeys)
	}
	if held := oneTask(t, a, task.ID); held.Key != task.Key {
		t.Fatalf("the premise: this node already holds the task as %s", held.Key)
	}
	a.applyWhileWriting()
	return a, task, moved
}

// A PURGE OF A TASK RE-KEYED AFTER ITS SCOPE WAS READ IS REFUSED, NAMING A KEY.
//
// The task is back in the project the caller named, so nothing else about the
// purge is stale — but it holds keys this node's read never saw, and its apply
// hands every one of them on. Published under the scope it read, a record
// deferred under one of those keys would not hold it back.
func TestAPurgeOfATaskReKeyedAfterItsScopeWasReadIsRefused(t *testing.T) {
	t.Parallel()
	a, task, _ := reKeyedBehind(t)
	operator := a.writer.As("ops-1", worklet.AuthorOperator, worklet.Provenance{})
	_, err := operator.PurgeTask(t.Context(), "op-purge", task.ID, "ENG",
		"a duplicate import")
	if !errors.Is(err, statelog.ErrConflict) || !strings.Contains(err.Error(), "key ") {
		t.Fatalf("a purge of a task re-keyed since its scope was read answered %v, "+
			"want a conflict naming a key it hands on", err)
	}
	a.drain()
	if got := a.strings(`SELECT id FROM tracker_tasks WHERE id = ?`, task.ID); len(got) != 1 {
		t.Fatal("the refused purge purged the task")
	}
}

// A MOVE KEEPS EVERY KEY ITS TASK HOLDS WHERE THE MOVE DECIDES.
//
// The former-key list travels whole on the record, so one formed from the
// walk's starting read would drop the keys a task gained by moving away and
// back since (the move's project condition lets that through): they would go on
// opening it unrecorded, and its purge would never hand them on.
func TestAMoveKeepsEveryKeyItsTaskHoldsWhereTheMoveDecides(t *testing.T) {
	t.Parallel()
	a, task, peer := reKeyedBehind(t)
	if _, err := a.writer.MoveTaskToProject(t.Context(), "op-move", task.ID, "OPS", nil); err != nil {
		t.Fatalf("move: %v", err)
	}
	a.drain()
	held := oneTask(t, a, task.ID)
	if held.Project != "OPS" {
		t.Fatalf("the premise: the task is in %s after the move", held.Project)
	}
	if want := append(slices.Clone(peer.FormerKeys), peer.Key); !slices.Equal(held.FormerKeys, want) {
		t.Errorf("the moved task's former keys are %v, want %v — the keys it held "+
			"when the move decided, the one it left last", held.FormerKeys, want)
	}
	for _, key := range held.FormerKeys {
		if got := a.strings(`SELECT task_id FROM tracker_task_keys WHERE key = ?`,
			key); len(got) != 1 || got[0] != task.ID {
			t.Errorf("former key %s opens %v, want the task", key, got)
		}
	}
}

// A RE-KEY OF A TASK RE-KEYED AFTER ITS KEYS WERE READ IS REFUSED, NAMING A KEY.
//
// A minting write names the keys it read before the request and the key it
// takes. The task holds others by the time the decide runs, and the apply
// turns one of them into a former key, which changes who a purge hands it to;
// published under the scope it read, a record deferred under that key would
// not hold it back.
func TestAReKeyOfATaskReKeyedAfterItsKeysWereReadIsRefused(t *testing.T) {
	t.Parallel()
	a, task, _ := reKeyedBehind(t)
	ops := "OPS"
	_, err := a.writer.UpdateTask(t.Context(), "op-rekey", task.ID, "ENG", worklet.NoIfMatch,
		worklet.TaskPatch{Project: &ops, Mint: &worklet.KeyMint{N: 99}},
		worklet.ChangeMoved, nil)
	if !errors.Is(err, statelog.ErrConflict) || !strings.Contains(err.Error(), "key ") {
		t.Fatalf("a re-key of a task re-keyed since its keys were read answered %v, "+
			"want a conflict naming a key it would move", err)
	}
	a.drain()
	if held := oneTask(t, a, task.ID); held.Project != "ENG" {
		t.Errorf("the refused re-key moved the task to %s", held.Project)
	}

	// AND A CALLER NEVER NAMES THE FORMER KEYS: the decide forms them.
	former := []string{"ENG-1"}
	if _, err := a.writer.UpdateTask(t.Context(), "op-former", task.ID, "ENG",
		worklet.NoIfMatch, worklet.TaskPatch{FormerKeys: &former},
		worklet.ChangeFields, nil); !errors.Is(err, worklet.ErrInvalid) {
		t.Errorf("a patch naming its task's former keys answered %v, want it "+
			"refused as invalid", err)
	}
}

// A WRITE TO A BLOCKER SOMEBODY FILLED STAYS UNDER THE SCOPE CAP.
//
// Every write to a task names its dependents (its apply rewrites their edges),
// and a full blocker's [worklet.MaxDependents] plus its own term is one past
// [worklet.MaxScopeTerms]: a scope enumerating them is refused in every decide,
// so the blocker could never be written again. The cap's own rule folds the
// enumeration into the project instead.
func TestAWriteToAFilledBlockerStaysUnderTheScopeCap(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	blocker := r.createTask("waited on by everybody")
	dependents := make([]string, 0, worklet.MaxDependents)
	for i := range worklet.MaxDependents {
		dependents = append(dependents, r.createTask("waits "+itoa(i)).ID)
	}
	if _, err := r.writer.Depend(t.Context(), "op-fill", worklet.DependencyChange{
		Task: blocker.ID, Project: blocker.Project, BlockingAdd: dependents,
	}, fixedLeads{project: "eng-lead"}); err != nil {
		t.Fatalf("fill the blocker: %v", err)
	}
	r.drain()
	if held := oneTask(t, r, blocker.ID); len(held.Dependents) != worklet.MaxDependents {
		t.Fatalf("the premise: the blocker lists %d dependents, want %d",
			len(held.Dependents), worklet.MaxDependents)
	}

	title := "still waited on by everybody"
	if _, err := r.writer.UpdateTask(t.Context(), "op-edit", blocker.ID, blocker.Project,
		worklet.NoIfMatch, worklet.TaskPatch{Title: &title}, worklet.ChangeFields, nil); err != nil {
		t.Fatalf("an edit to a blocker with %d dependents answered %v",
			worklet.MaxDependents, err)
	}
	r.drain()
	if held := oneTask(t, r, blocker.ID); held.Title != title {
		t.Errorf("the edit did not land: the blocker is titled %q", held.Title)
	}
}
