package code_test

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
)

// A SERVED HALF WITH NO COMMITTER REFUSES A PUSH, and a compaction, and
// answers every read: a node that does not publish — a maintenance mode —
// decides nothing and appends nothing, so its refusal is the one the router
// reads as a node that ran nothing and hands the proposal to the repository's
// runner-up under the same id; its reader still serves, since a read appends
// nothing.
func TestAServedHalfWithNoCommitterRefusesAPush(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.ensure("app", described("app"), t1)
	half := code.Served{Reader: n.reader}
	if _, err := half.Propose(t.Context(), code.Proposal{PushID: "p", RepoID: "r"}); !errors.Is(err,
		code.ErrNotServing) {
		t.Fatalf("a proposal to a half with no committer = %v, want ErrNotServing", err)
	}
	// A COMPACTION'S TOO, which is a write of the same committer's.
	if _, err := half.ProposeCompaction(t.Context(), code.CompactionProposal{}); !errors.Is(err,
		code.ErrNotServing) {
		t.Fatalf("a compaction to a half with no committer = %v, want ErrNotServing", err)
	}
	ref, err := half.Resolve(t.Context(), code.ResolveQuery{Name: "app", Fresh: stale})
	if err != nil || ref.Name != "app" {
		t.Fatalf("a read of a half with no committer = (%+v, %v), want the repository", ref, err)
	}
}

// A SERVED HALF WITH NO READER RUNS NOTHING rather than panicking on the
// serving goroutine: every operation answers ErrNotServing, which the router
// reads as a node that ran nothing and asks the next data node. A zero value
// the estate could be handed has to mean something; this is what it means.
func TestAServedHalfWithNoReaderRunsNothing(t *testing.T) {
	t.Parallel()
	var half code.Served
	ctx := t.Context()
	for name, call := range map[string]func() error{
		"propose": func() error { _, err := half.Propose(ctx, code.Proposal{}); return err },
		"compact": func() error {
			_, err := half.ProposeCompaction(ctx, code.CompactionProposal{})
			return err
		},
		"resolve": func() error { _, err := half.Resolve(ctx, code.ResolveQuery{}); return err },
		"snapshot": func() error {
			_, err := half.Snapshot(ctx, code.StateQuery{})
			return err
		},
		"push outcome": func() error {
			_, err := half.PushOutcome(ctx, code.OutcomeQuery{})
			return err
		},
		"read": func() error { _, err := half.Read(ctx, code.ReadQuery{}); return err },
		"resolve ref": func() error {
			_, err := half.ResolveRef(ctx, code.RefQuery{})
			return err
		},
	} {
		if err := call(); !errors.Is(err, code.ErrNotServing) {
			t.Errorf("%s on a zero served half = %v, want ErrNotServing", name, err)
		}
	}
}
