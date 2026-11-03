package host_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A MERGED CHANGE REVERTED ON ITS REPOSITORY'S GIT NODE (revert.go): what its
// merge brought into its target undone onto the target's tip, on a new
// branch whose push opens "Revert <repo>!<n>" in its own record.

// revertChange is who's revert_change of change n onto branch.
func revertChange(repo string, who code.Principal, n int64, branch string) code.EngineWrite {
	return code.EngineWrite{Kind: code.WriteRevertChange, RepoID: repo, Principal: who, OpID: opNow(),
		Fresh: linear, Ref: branch, Change: n}
}

// mainTip is main's commit as a fresh clone reads it.
func (c *mergeCase) mainTip() string {
	c.T.Helper()
	return strings.TrimSpace(c.p.Git().Run(c.T, c.clone(), "rev-parse", "main"))
}

// A REVERT OPENS A CHANGE (M5-D25): a change merged by each method — a merge
// commit, a squash, and a fast-forward of two commits — reverted by pat, is
// ONE commit on main's tip, on the new branch revert-<n>, whose tree is
// main's before the merge, whatever the method: the merge commit's first
// parent, the squash's parent, and the fast-forward's whole range from its
// recorded base (its head's first parent is the change's own first commit, so
// a plain revert would leave it in). The push's record opens "Revert
// <repo>!<n>" from that branch into main, at that commit; main does not move.
//
// Mutations: revert a fast-forward against its merge_oid's first parent →
// the first commit's file survives, red; open no change in the push → red.
func TestARevertOpensAChange(t *testing.T) {
	t.Parallel()
	c := newMergeCase(t, codetest.SoloOptions{}, codetest.Settings(), map[string]string{"README.md": "one\n"})
	for _, method := range []code.MergeMethod{code.MergeMethodMerge, code.MergeMethodSquash,
		code.MergeMethodFastForward} {
		ref := "refs/heads/by-" + string(method)
		c.branchOff("by-" + string(method))
		c.pat.Commit(c.patWork, map[string]string{string(method) + "-1.md": "one\n"}, "First of "+string(method))
		c.pat.Commit(c.patWork, map[string]string{string(method) + "-2.md": "two\n"}, "Second of "+string(method))
		n := c.openPushed(ref, "Merged by "+string(method))
		c.meetGate(n, lead())
		before := c.mainTip()
		res, err := c.write(merge(c.repo, pat(), n, c.change(n).Head, method))
		merged := c.requireMerged(n, res, err)

		branch := fmt.Sprintf("refs/heads/revert-%d", n)
		got, err := c.write(revertChange(c.repo, pat(), n, branch))
		if err != nil || got.Outcome != statelog.OutcomeApplied || got.Commit == "" || got.Change == nil ||
			!got.Change.Opened || got.Change.Number < 1 {
			t.Fatalf("%s: the revert answered %+v %v (change %+v), want applied, opening a change", method, got, err,
				got.Change)
		}
		clone := c.clone()
		if tree, want := c.show(clone, "%T", got.Commit), c.show(clone, "%T", before); tree != want {
			t.Errorf("%s: the revert's tree is %s, want main's before the merge, %s", method, tree, want)
		}
		if parent := c.show(clone, "%P", got.Commit); parent != merged {
			t.Errorf("%s: the revert sits on %s, want main's tip %s", method, parent, merged)
		}
		if subject := c.show(clone, "%s", got.Commit); subject != fmt.Sprintf("Revert \"Merged by %s\"", method) {
			t.Errorf("%s: the revert is titled %q", method, subject)
		}
		if tip := c.mainTip(); tip != merged {
			t.Errorf("%s: main moved to %s on a revert, want it at %s", method, tip, merged)
		}
		opened := c.change(got.Change.Number)
		if want := fmt.Sprintf("Revert %s!%d", codetest.RepoName, n); opened.Title != want ||
			opened.SourceRef != branch || opened.TargetRef != "refs/heads/main" || opened.Head != got.Commit ||
			!opened.State.Live() {
			t.Errorf("%s: the revert opened %+v, want %q from %s into main at %s", method, opened.ChangeRow, want,
				branch, got.Commit)
		}
	}
}

// A REVERT CONFLICT NAMES ITS PATHS: a merged change whose file main changed
// again since does not revert cleanly — refused conflict, naming the path,
// storing nothing, creating no branch and opening no change; and a change
// that is not merged has nothing to revert (not_merged).
//
// Mutation: answer a conflicting revert's merged tree → the revert lands, red.
func TestARevertConflictNamesPaths(t *testing.T) {
	t.Parallel()
	c := newMergeCase(t, codetest.SoloOptions{}, codetest.Settings(),
		map[string]string{"README.md": "one\n", "conf.yaml": "retries: 1\n"})
	n := c.open(featureRef, map[string]string{"conf.yaml": "retries: 3\n"}, "Retry thrice")
	c.meetGate(n, lead())
	res, err := c.write(merge(c.repo, pat(), n, c.change(n).Head, code.MergeMethodMerge))
	c.requireMerged(n, res, err)
	// MAIN CHANGES THE SAME LINE AGAIN, by pat's push.
	c.branchOff("again")
	c.pat.Commit(c.patWork, map[string]string{"conf.yaml": "retries: 5\n"}, "Retry five times")
	if push := c.pat.PushArgs(c.Ctx, c.patWork, nil, c.URL(0), "HEAD:refs/heads/main"); push.Err != nil {
		t.Fatalf("pat's push of main: %v\n%s", push.Err, push.Stderr)
	}
	open := len(c.changes())
	before := len(c.objects())
	branch := fmt.Sprintf("refs/heads/revert-%d", n)
	got, err := c.write(revertChange(c.repo, pat(), n, branch))
	switch {
	case err != nil || got.Refusal != code.WriteConflict || got.Outcome != "":
		t.Fatalf("a conflicting revert answered %+v %v, want refused conflict", got, err)
	case len(got.Conflicts) != 1 || got.Conflicts[0].Path != "conf.yaml":
		t.Errorf("the conflict names %+v, want conf.yaml", got.Conflicts)
	case !strings.Contains(got.Detail, fmt.Sprintf("%s!%d", codetest.RepoName, n)):
		t.Errorf("the conflict %q does not name the change it reverts", got.Detail)
	}
	if after := len(c.objects()); after != before {
		t.Errorf("a refused revert stored %d objects", after-before)
	}
	if refs := c.p.Git().Run(c.T, c.clone(), "branch", "-r", "--list", "origin/revert-*"); strings.TrimSpace(refs) != "" {
		t.Errorf("a refused revert created %q", refs)
	}
	if now := len(c.changes()); now != open {
		t.Errorf("a refused revert opened %d changes", now-open)
	}

	live := c.open("refs/heads/live", map[string]string{"live.md": "live\n"}, "Still open")
	got, err = c.write(revertChange(c.repo, pat(), live, fmt.Sprintf("refs/heads/revert-%d", live)))
	if err != nil || got.Refusal != code.WriteNotMerged {
		t.Errorf("a revert of an open change answered %+v %v, want not_merged", got, err)
	}
}
