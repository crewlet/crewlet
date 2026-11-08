package worklet_test

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A CREDENTIAL'S WRITE THAT WAKES SOMEBODY CARRIES THE APPS IT USED HERE, AND
// NOTHING ELSE DOES (ADR-0109). A task a pipeline's token files with its wake
// records, beside its login, the apps the node that took the write resolved
// for it — the turn the wake starts is bounded by them — at the version that
// carries them, which an older build retains rather than applying without
// them. The same token's write that wakes nobody carries none, and neither
// does a person's even when handed some: its record names the seat, whose
// holder the node running a turn reads. Both stay at the base version every
// build reads.
//
// Mutation: leave the apps off the record the writer builds — the token's
// task carries none; keep them on a record that wakes nobody — the token's
// quiet task carries Worklet; keep them for every kind in Writer.As — the
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
		{"t-by-token", "token:ci", worklet.AuthorOperator, true, worklets, 2},
		{"t-quiet-by-token", "token:ci", worklet.AuthorOperator, false, nil, 1},
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
		if record.Actor != c.actor || !slices.Equal(record.ActorApps, c.want) || record.V != c.version {
			t.Errorf("%s is recorded as %q carrying %v at version %d, want %v at %d",
				c.id, record.Actor, record.ActorApps, record.V, c.want, c.version)
		}
	}
}
