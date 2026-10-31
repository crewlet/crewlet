package host_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A CHANGE REBASED ON ITS REPOSITORY'S GIT NODE (rebase.go): its commits its
// target does not hold, replayed onto the target's tip and landed as one
// forced push of its source — each keeping its author, the first conflict
// stopping it, a merge among them refusing it.

// authored is a commit on pat's checkout of files, by author at date — a
// commit as its author's own git wrote it — answering its id.
func (c *mergeCase) authored(files map[string]string, message, author, date string) string {
	c.T.Helper()
	c.pat.Commit(c.patWork, files, message)
	c.pat.Git().Run(c.T, c.patWork, "commit", "-q", "--amend", "--no-edit", "--author="+author, "--date="+date)
	return strings.TrimSpace(c.pat.Git().Run(c.T, c.patWork, "rev-parse", "HEAD"))
}

// branchOff checks out branch afresh at main's tip in pat's checkout.
func (c *mergeCase) branchOff(branch string) {
	c.T.Helper()
	c.pat.Git().Run(c.T, c.patWork, "fetch", "-q", "origin")
	c.pat.Git().Run(c.T, c.patWork, "checkout", "-q", "-B", branch, "origin/main")
}

// openPushed pushes pat's checkout's HEAD to ref, opening a change from it,
// and answers its number.
func (c *mergeCase) openPushed(ref, title string) int64 {
	c.T.Helper()
	res := c.pat.PushArgs(c.Ctx, c.patWork, []string{"-o", "change", "-o", "change.title=" + title},
		c.URL(0), "HEAD:"+ref)
	if res.Err != nil {
		c.T.Fatalf("pat's push of %s: %v\n%s", ref, res.Err, res.Stderr)
	}
	for _, ch := range c.changes() {
		if ch.SourceRef == ref && ch.State.Live() {
			return ch.Number
		}
	}
	c.T.Fatalf("pat's push of %s opened no change", ref)
	return 0
}

// moveMain moves main on by one commit of ceo's, touching path.
func (c *mergeCase) moveMain(path, content string) string {
	c.T.Helper()
	tip := c.p.Commit(c.work, map[string]string{path: content}, "main moves on")
	c.MustPush(c.p, c.work, "main")
	return tip
}

// rebase is who's rebase_change of change n's source ref at head.
func rebase(repo string, who code.Principal, n int64, ref, head string) code.EngineWrite {
	return code.EngineWrite{Kind: code.WriteRebaseChange, RepoID: repo, Principal: who, OpID: opNow(),
		Fresh: linear, Ref: ref, ExpectedHead: head, Change: n}
}

// A REBASE KEEPS EACH COMMIT'S AUTHOR (U43 a): a change of two commits by two
// authors — each at a date in a zone of its own — rebased by pat onto a main
// that moved, is two commits on main's new tip, each with ITS author's name,
// address and date, zone and all, and its message byte for byte, committed by
// the engine; its source moved to them by one push, and its change at a new
// revision of that head.
//
// Mutation: author a replayed commit by the rebaser → pat's name on Ada's,
// red; write the author date at +0000 → its zone lost, red.
func TestARebaseKeepsEachCommitsAuthor(t *testing.T) {
	t.Parallel()
	c := newMergeCase(t, codetest.SoloOptions{}, codetest.Settings(), map[string]string{"README.md": "one\n"})
	c.branchOff("feature")
	first := c.authored(map[string]string{"a.md": "a\n"}, "Add a\n\nWith a body.", "Ada Lovelace <ada@example.com>",
		"2020-01-02T03:04:05+0530")
	second := c.authored(map[string]string{"b.md": "b\n"}, "Add b", "Grace Hopper <grace@example.com>",
		"2021-06-07T08:09:10-0400")
	n := c.openPushed(featureRef, "Two authors")
	tip := c.moveMain("main.md", "moved\n")
	res, err := c.write(rebase(c.repo, pat(), n, featureRef, second))
	if err != nil || res.Outcome != statelog.OutcomeApplied || res.Commit == "" || res.Command.Old != second {
		t.Fatalf("the rebase did not land as a push of feature from %s: %+v %v", second, res, err)
	}
	clone := c.clone()
	engine := fmt.Sprintf("%s <%s>", codetest.FixtureCommitterName, codetest.FixtureCommitterEmail)
	for i, tc := range []struct{ was, now, date string }{
		{first, res.Commit + "~1", "1577914445 +0530"}, {second, res.Commit, "1623067750 -0400"},
	} {
		// THE AUTHOR AS GIT WROTE IT, its date raw: its seconds and its zone —
		// the original read in pat's own checkout, since no ref of the
		// repository reaches it once the branch moved.
		raw := func(dir, rev string) string {
			return strings.TrimSpace(c.pat.Git().Run(c.T, dir, "log", "-1", "--date=raw",
				"--format=%an <%ae> %ad", rev))
		}
		if got, want := raw(clone, tc.now), raw(c.patWork, tc.was); got != want || !strings.HasSuffix(got, tc.date) {
			t.Errorf("commit %d is authored %q, want its author's own %q, dated %s", i+1, got, want, tc.date)
		}
		if got, want := c.show(clone, "%B", tc.now), c.show(c.patWork, "%B", tc.was); got != want {
			t.Errorf("commit %d's message is %q, want its own %q", i+1, got, want)
		}
		if committer := c.show(clone, "%cn <%ce>", tc.now); committer != engine {
			t.Errorf("commit %d is committed by %q, want the engine %q", i+1, committer, engine)
		}
	}
	if base := c.show(clone, "%P", res.Commit+"~1"); base != tip {
		t.Errorf("the rebased commits sit on %s, want main's tip %s", base, tip)
	}
	if ch := c.change(n); ch.Head != res.Commit || ch.Revision != 2 {
		t.Errorf("change %d is at %s, revision %d, want its rebased head %s at revision 2", n, ch.Head, ch.Revision,
			res.Commit)
	}
}

// A REBASE STOPS AT THE FIRST CONFLICT: of a change's two commits, the second
// rewrites a line main has rewritten since — the first replays cleanly, the
// second conflicts — so the rebase is refused conflict, naming the second
// commit and the path, stores nothing, and leaves the branch where it was.
//
// Mutation: skip a conflicting commit and go on → the rebase lands, red.
func TestARebaseStopsAtTheFirstConflict(t *testing.T) {
	t.Parallel()
	c := newMergeCase(t, codetest.SoloOptions{}, codetest.Settings(),
		map[string]string{"README.md": "one\n", "conf.yaml": "retries: 1\n"})
	c.branchOff("feature")
	c.authored(map[string]string{"a.md": "a\n"}, "Add a", "Ada Lovelace <ada@example.com>", "2020-01-02T03:04:05Z")
	second := c.authored(map[string]string{"conf.yaml": "retries: 3\n"}, "Retry thrice",
		"Ada Lovelace <ada@example.com>", "2020-01-02T03:05:05Z")
	n := c.openPushed(featureRef, "Retries")
	c.moveMain("conf.yaml", "retries: 2\n")
	before := len(c.objects())
	res, err := c.write(rebase(c.repo, pat(), n, featureRef, second))
	switch {
	case err != nil || res.Refusal != code.WriteConflict || res.Outcome != "":
		t.Fatalf("a conflicting rebase answered %+v %v, want refused conflict", res, err)
	case !strings.Contains(res.Detail, second[:7]) || !strings.Contains(res.Detail, "2 of 2"):
		t.Errorf("the conflict %q does not name the second commit %s", res.Detail, second[:7])
	case len(res.Conflicts) != 1 || res.Conflicts[0].Path != "conf.yaml":
		t.Errorf("the conflict names %+v, want conf.yaml", res.Conflicts)
	}
	if after := len(c.objects()); after != before {
		t.Errorf("a refused rebase stored %d objects", after-before)
	}
	if head := strings.TrimSpace(c.p.Git().Run(c.T, c.clone(), "rev-parse", "origin/feature")); head != second {
		t.Errorf("feature is at %s after a refused rebase, want %s", head, second)
	}
}

// A REBASE OF A MERGE IS REFUSED: a change whose history holds a merge
// commit is refused merge_commit naming it — a merge needs a mainline to
// replay — and nothing moves.
//
// Mutation: replay a merge onto its first parent → the rebase lands, red.
func TestARebaseOfAMergeIsRefused(t *testing.T) {
	t.Parallel()
	c := newMergeCase(t, codetest.SoloOptions{}, codetest.Settings(), map[string]string{"README.md": "one\n"})
	c.branchOff("side")
	c.authored(map[string]string{"side.md": "s\n"}, "Side", "Ada Lovelace <ada@example.com>", "2020-01-02T03:04:05Z")
	c.pat.Git().Run(c.T, c.patWork, "checkout", "-q", "-B", "feature", "origin/main")
	c.authored(map[string]string{"a.md": "a\n"}, "Add a", "Ada Lovelace <ada@example.com>", "2020-01-02T03:05:05Z")
	c.pat.Git().Run(c.T, c.patWork, "merge", "-q", "--no-ff", "--no-edit", "side")
	head := strings.TrimSpace(c.pat.Git().Run(c.T, c.patWork, "rev-parse", "HEAD"))
	n := c.openPushed(featureRef, "Merged in")
	c.moveMain("main.md", "moved\n")
	res, err := c.write(rebase(c.repo, pat(), n, featureRef, head))
	if err != nil || res.Refusal != code.WriteMergeCommit || !strings.Contains(res.Detail, head[:7]) {
		t.Fatalf("the rebase of a merge answered %+v %v, want merge_commit naming %s", res, err, head[:7])
	}
}

// A REBASE_CHANGE IS HELD TO ITS SHAPE: the change by its number and the head
// its caller read, its source as its ref — and nothing a merge or a commit
// takes.
func TestARebaseChangeIsHeldToItsShape(t *testing.T) {
	t.Parallel()
	ok := rebase(uuid.Must(uuid.NewV7()).String(), pat(), 2, featureRef, strings.Repeat("b", 40))
	if err := ok.Validate(); err != nil || ok.Kind.BuildDeadline() != code.RebaseBuildDeadline {
		t.Fatalf("a whole rebase_change: %v, its build %v", err, ok.Kind.BuildDeadline())
	}
	for _, tc := range []struct {
		name  string
		edit  func(w *code.EngineWrite)
		field string
	}{
		{"no change", func(w *code.EngineWrite) { w.Change = 0 }, "change"},
		{"no head", func(w *code.EngineWrite) { w.ExpectedHead = "" }, "expected_head"},
		{"a method", func(w *code.EngineWrite) { w.Method = code.MergeMethodSquash }, "method"},
		{"a message", func(w *code.EngineWrite) { w.Message = "mine" }, "message"},
		{"an override", func(w *code.EngineWrite) { w.Override = "now" }, "override"},
	} {
		w := ok
		tc.edit(&w)
		if err := w.Validate(); err == nil || !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: %v, want a refusal naming %s", tc.name, err, tc.field)
		}
	}
}
