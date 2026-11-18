package gitlet_test

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet"
)

// A SERVED HALF WITH NO COMMITTER REFUSES A PUSH and a compaction with
// ErrNotServing, which the router hands to the repository's runner-up under
// the same id, and still answers every read, since a read appends nothing.
func TestAServedHalfWithNoCommitterRefusesAPush(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	n.ensure("app", described("app"), t1)
	half := gitlet.Served{Reader: n.reader}
	if _, err := half.Propose(t.Context(), gitlet.Proposal{PushID: "p", RepoID: "r"}); !errors.Is(err,
		gitlet.ErrNotServing) {
		t.Fatalf("a proposal to a half with no committer = %v, want ErrNotServing", err)
	}
	// A COMPACTION'S TOO: a write of the same committer.
	if _, err := half.ProposeCompaction(t.Context(), gitlet.CompactionProposal{}); !errors.Is(err,
		gitlet.ErrNotServing) {
		t.Fatalf("a compaction to a half with no committer = %v, want ErrNotServing", err)
	}
	ref, err := half.Resolve(t.Context(), gitlet.ResolveQuery{Name: "app", Fresh: stale})
	if err != nil || ref.Name != "app" {
		t.Fatalf("a read of a half with no committer = (%+v, %v), want the repository", ref, err)
	}
}

// A SERVED HALF WITH NO READER RUNS NOTHING rather than panicking on the
// serving goroutine: the zero value answers every operation ErrNotServing, and
// the router asks the next data node.
func TestAServedHalfWithNoReaderRunsNothing(t *testing.T) {
	t.Parallel()
	var half gitlet.Served
	ctx := t.Context()
	for name, call := range map[string]func() error{
		"propose": func() error { _, err := half.Propose(ctx, gitlet.Proposal{}); return err },
		"compact": func() error {
			_, err := half.ProposeCompaction(ctx, gitlet.CompactionProposal{})
			return err
		},
		"resolve": func() error { _, err := half.Resolve(ctx, gitlet.ResolveQuery{}); return err },
		"snapshot": func() error {
			_, err := half.Snapshot(ctx, gitlet.StateQuery{})
			return err
		},
		"push outcome": func() error {
			_, err := half.PushOutcome(ctx, gitlet.OutcomeQuery{})
			return err
		},
		"read": func() error { _, err := half.Read(ctx, gitlet.ReadQuery{}); return err },
		"resolve ref": func() error {
			_, err := half.ResolveRef(ctx, gitlet.RefQuery{})
			return err
		},
	} {
		if err := call(); !errors.Is(err, gitlet.ErrNotServing) {
			t.Errorf("%s on a zero served half = %v, want ErrNotServing", name, err)
		}
	}
}
