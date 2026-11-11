package worklet_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A CREDENTIAL'S WRITE A LATER TURN READS CARRIES THE APPS IT USED HERE, AND NO
// PERSON'S DOES (ADR-0109, ADR-0112). A task a pipeline's token files records,
// beside its login, the apps the node that took the write resolved for it, at
// the version that marks them kept for the history row, which an older build
// retains rather than applying without them: whether or not the creation
// wakes anybody, because the work pickup reads an item's creation for the
// apps the turn it starts is bounded by, as it reads it for whom the work is
// for (Writer.carries, which Writer.recordedApps asks). A person's write
// carries none even when handed some: its record names the seat, whose holder
// the node running a turn reads, and it stays at the base version every build
// reads.
//
// Mutation: leave the apps off the record the writer builds — the token's
// task carries none; ask the apps only of what a record wakes — the token's
// quiet task carries none; keep them for every kind in Writer.As — the
// person's task carries Worklet.
func TestACredentialsWriteCarriesTheAppsItUsedHere(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	worklets := []iam.Grant{iam.GrantWorklet}
	for _, c := range []struct {
		id, actor string
		kind      worklet.AuthorKind
		woken     bool
		want      []iam.Grant
		version   int
	}{
		{"t-by-token", "token:ci", worklet.AuthorOperator, true, worklets, worklet.AppsKeptVersion},
		{"t-quiet-by-token", "token:ci", worklet.AuthorOperator, false, worklets, worklet.AppsKeptVersion},
		{"t-by-person", "jane-founder", worklet.AuthorHuman, true, nil, 1},
	} {
		w := r.writer.As(c.actor, c.kind, worklet.Provenance{OperatorID: c.actor, Apps: worklets})
		task := newTask(c.id)
		task.Key, task.Reporter = "", c.actor
		var wake *worklet.Notify
		if c.woken {
			wake = worklet.Wake{Kind: worklet.ChangeCreated, After: task}.Notify(nil)
		}
		if _, err := w.CreateTask(t.Context(), "op-"+c.id, task, wake); err != nil {
			t.Fatalf("%s files %s: %v", c.actor, c.id, err)
		}
		r.drain()
		record, err := worklet.Decode(r.lastRecord(t))
		if err != nil {
			t.Fatalf("decode %s: %v", c.id, err)
		}
		if record.Actor != c.actor || !slices.Equal(record.ActorApps, c.want) || record.V != c.version ||
			record.ActorAppsKept != (c.want != nil) {
			t.Errorf("%s is recorded as %q carrying %v (kept %v) at version %d, want %v at %d",
				c.id, record.Actor, record.ActorApps, record.ActorAppsKept, record.V, c.want, c.version)
		}
	}
}

// A CREDENTIAL'S ASSIGNMENT CARRIES ITS APPS ONLY WHERE IT MOVES THE ASSIGNEE
// (ADR-0109, ADR-0112): the work pickup reads the rows that moved an item's
// assignee, so a token's move keeps the apps it wrote under, at the version
// that marks them kept, while its reassertion to the seat that already holds
// the item, which moves nothing and which no read asks, carries none and stays
// at the base version every build reads. The reassertion is written with the
// wake the tool builds for it, which names nobody to wake.
//
// Mutation: ask the apps of every record of kind assignee again
// (Writer.recordedApps(carries || kind == ChangeAssignee)), and the reassertion
// carries Worklet at the kept version.
func TestACredentialsAssignmentCarriesItsAppsOnlyWhereItMovesTheAssignee(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	worklets := []iam.Grant{iam.GrantWorklet}
	token := r.writer.As("token:ci", worklet.AuthorOperator,
		worklet.Provenance{OperatorID: "token:ci", Apps: worklets})
	task := newTask("t-handed")
	task.Key, task.Reporter = "", "token:ci"
	if _, err := token.CreateTask(t.Context(), "op-t-handed", task, nil); err != nil {
		t.Fatalf("token:ci files t-handed: %v", err)
	}
	r.drain()
	for _, c := range []struct {
		name    string
		want    []iam.Grant
		version int
	}{
		{"a move of the assignee", worklets, worklet.AppsKeptVersion},
		{"a reassertion to the holder", nil, 1},
	} {
		after := task
		after.Assignee = "ana"
		to := after.Assignee
		wake := worklet.Wake{Kind: worklet.ChangeAssignee, Before: task, After: after}.Notify(nil)
		if _, err := token.UpdateTask(t.Context(), "op-"+c.name, "t-handed", "ENG", worklet.NoIfMatch,
			worklet.TaskPatch{Assignee: &to}, worklet.ChangeAssignee, wake); err != nil {
			t.Fatalf("token:ci writes %s: %v", c.name, err)
		}
		r.drain()
		record, err := worklet.Decode(r.lastRecord(t))
		if err != nil {
			t.Fatalf("decode %s: %v", c.name, err)
		}
		if !slices.Equal(record.ActorApps, c.want) || record.V != c.version ||
			record.ActorAppsKept != (c.want != nil) {
			t.Errorf("%s is recorded carrying %v (kept %v) at version %d, want %v at %d",
				c.name, record.ActorApps, record.ActorAppsKept, record.V, c.want, c.version)
		}
		task = after
	}
}
