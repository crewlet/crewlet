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
