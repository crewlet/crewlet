package host_test

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A CHANGE'S MERGE, BUILT ON ITS REPOSITORY'S GIT NODE (merge.go): a
// merge_change judged, built onto the target's tip in a scratch and landed as
// one merge record through the one landing — its commit authored as U43 (a)
// says, built again onto a target that moved under it, and enqueued where its
// target's merges land through a queue.

// featureRef is the branch pat's changes are opened from.
const featureRef = "refs/heads/feature"

// mergeCase is a write case whose person pat opens changes by pushing, and
// whose rows the case meets a change's merge gate on.
type mergeCase struct {
	*writeCase
	pat     *codetest.Person
	patWork string
}

// newMergeCase is a merge case over o and settings, main seeded by ceo with
// seed, pat holding a clone of it.
func newMergeCase(t *testing.T, o codetest.SoloOptions, settings code.RepoSettings,
	seed map[string]string) *mergeCase {

	t.Helper()
	c := newCase(t, o, settings)
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, seed, "seed")
	c.MustPush(p, work, "main")
	wc := &writeCase{Case: c, p: p, work: work, head: head, repo: c.Rows().Repo.ID}
	pat := codetest.NewPerson(t, c.Fleet, codetest.FixturePat)
	return &mergeCase{writeCase: wc, pat: pat, patWork: pat.Clone(c.URL(0))}
}

// solo is the case's fleet as the solo fleet it is.
func (c *mergeCase) solo() *codetest.SoloFleet { return c.Fleet.(*codetest.SoloFleet) }

// open pushes files onto a new branch ref off main as pat, opening a change
// from it into main, and answers the change's number.
func (c *mergeCase) open(ref string, files map[string]string, title string) int64 {
	c.T.Helper()
	c.pat.Git().Run(c.T, c.patWork, "fetch", "-q", "origin")
	c.pat.Git().Run(c.T, c.patWork, "checkout", "-q", "-B", strings.TrimPrefix(ref, "refs/heads/"), "origin/main")
	c.pat.Commit(c.patWork, files, title)
	res := c.pat.PushArgs(c.Ctx, c.patWork, []string{"-o", "change", "-o", "change.title=" + title},
		c.URL(0), "HEAD:"+ref)
	if res.Err != nil {
		c.T.Fatalf("pat's push of %s opening a change: %v\n%s", ref, res.Err, res.Stderr)
	}
	for _, ch := range c.changes() {
		if ch.SourceRef == ref && ch.State.Live() {
			return ch.Number
		}
	}
	c.T.Fatalf("pat's push of %s opened no change", ref)
	return 0
}

// changes is every change of the case's repository, read as pat.
func (c *mergeCase) changes() []code.ChangeSummary {
	c.T.Helper()
	ans, err := c.solo().Code(c.T).Read(c.Ctx, code.ReadQuery{Kind: code.ReadChanges, Asker: pat(),
		Repo: codetest.RepoName, Fresh: linear})
	if err != nil {
		c.T.Fatalf("read the changes: %v", err)
	}
	return ans.Changes
}

// change is change n, read as pat at the log's head.
func (c *mergeCase) change(n int64) *code.ChangeDetail {
	c.T.Helper()
	ans, err := c.solo().Code(c.T).Read(c.Ctx, code.ReadQuery{Kind: code.ReadChange, Asker: pat(),
		Repo: codetest.RepoName, Change: n, Fresh: linear})
	if err != nil || ans.Change == nil {
		c.T.Fatalf("read change %d: %+v %v", n, ans.Change, err)
	}
	return ans.Change
}

// meetGate makes change n's head revision meet a gate asking nothing beyond
// M4's: its facts recorded and an approval of it by approver — the
// conversation's asker, which names no run.
func (c *mergeCase) meetGate(n int64, approver code.Principal) {
	c.T.Helper()
	d := c.recordFacts(n)
	approver.Run = code.RunProvenance{}
	res, err := c.solo().Code(c.T).Converse(c.Ctx, code.ConversationWrite{OpID: opNow(), Asker: approver,
		Review: &code.Review{RepoID: c.repo, ChangeID: d.ID, Revision: d.Revision, Verdict: code.VerdictApprove,
			Summary: "Looks right."}})
	if err != nil || res.Refusal != "" {
		c.T.Fatalf("approve change %d: %+v %v", n, res, err)
	}
}

// recordFacts records change n's head revision's facts against its target as
// its home records them — read through the git reads, at the tip and
// position the due listing names — and answers the change as that listing
// named it.
func (c *mergeCase) recordFacts(n int64) code.ChangeDue {
	c.T.Helper()
	seam := c.solo().Code(c.T)
	due, err := seam.Read(c.Ctx, code.ReadQuery{Kind: code.ReadChangesDue, Fresh: linear,
		ChangesDue: &code.ChangesDueQuery{RepoID: c.repo, RecheckBefore: time.Now()}})
	if err != nil {
		c.T.Fatalf("read the due changes: %v", err)
	}
	i := slices.IndexFunc(due.DueChanges, func(d code.ChangeDue) bool { return d.Number == n })
	if i < 0 {
		c.T.Fatalf("change %d is not due its facts: %+v", n, due.DueChanges)
	}
	d := due.DueChanges[i]
	read, err := c.solo().GitRead(c.Ctx, code.GitQuery{Kind: code.GitRevisionFacts, RepoID: c.repo, Fresh: linear,
		RevisionFacts: &code.RevisionFactsQuery{Head: d.Head, Target: d.Tip}})
	if err != nil || read.RevisionFacts == nil {
		c.T.Fatalf("read revision %d's facts: %+v %v", d.Revision, read, err)
	}
	f := read.RevisionFacts
	_, digest := code.MatchOwners(nil, f.Touched, f.TouchesAll)
	res, err := seam.Converse(c.Ctx, code.ConversationWrite{OpID: opNow(), Revise: &code.Revise{RepoID: c.repo,
		ChangeID: d.ID, N: d.Revision, HeadOID: d.Head, TargetRef: d.TargetRef, TargetOID: d.Tip,
		TargetPosition: d.TipPosition, BaseOID: f.MergeBase, BaseTree: f.BaseTree, HeadTree: f.HeadTree,
		PatchID: f.PatchID, PatchIDOmitted: f.PatchIDOmitted, Touched: f.Touched, TouchesAll: f.TouchesAll,
		Classes: f.Classes, Node: "solo-a", OwnersDigest: digest}})
	if err != nil || res.Refusal != "" {
		c.T.Fatalf("record revision %d's facts: %+v %v", d.Revision, res, err)
	}
	return d
}

// merge is who's merge_change of change n into main at head, by method.
func merge(repo string, who code.Principal, n int64, head string, method code.MergeMethod) code.EngineWrite {
	return code.EngineWrite{Kind: code.WriteMergeChange, RepoID: repo, Principal: who, OpID: opNow(),
		Fresh: linear, Ref: "refs/heads/main", ExpectedHead: head, Change: n, Method: method}
}

// lead is the fixture chart's ENG lead, as their write names them.
func lead() code.Principal {
	return code.Principal{Kind: code.PrincipalAgent, Seat: codetest.FixtureLead,
		Run: code.RunProvenance{TurnID: uuid.Must(uuid.NewV7()).String(), WorkKey: "ENG-9", Model: "fixture"}}
}

// requireMerged fails the case unless res is a merge applied onto main, its
// change merged at the commit main now holds, and answers that commit.
func (c *mergeCase) requireMerged(n int64, res code.EngineWriteResult, err error) string {
	c.T.Helper()
	if err != nil || res.Outcome != statelog.OutcomeApplied || res.Merge == nil || res.Merge.Refusal != "" ||
		res.Commit == "" {
		c.T.Fatalf("the merge of change %d did not apply: %+v %v (%+v)", n, res, err, res.Merge)
	}
	ch := c.change(n)
	if ch.State != code.ChangeStateMerged || ch.MergeOID != res.Commit || ch.MergedVia != code.MergedViaDirect {
		c.T.Fatalf("change %d is %s via %s at %s, want merged directly at %s", n, ch.State, ch.MergedVia,
			ch.MergeOID, res.Commit)
	}
	if main := strings.TrimSpace(c.p.Git().Run(c.T, c.clone(), "rev-parse", "main")); main != res.Commit {
		c.T.Fatalf("main is at %s after the merge, want its commit %s", main, res.Commit)
	}
	return res.Commit
}

// A MERGE COMMIT IS ITS MERGER'S (U43 a): a change pat opened, merged with a
// merge commit by the seat alice, is authored by alice — not by pat, who wrote
// the change — and committed by the engine, its two parents main's tip and the
// change's head; merged by the person pat, it is pat's. Each is the MERGE
// RECORD's: the change merged directly at the commit main moved to.
//
// Mutation: author a merge commit by the change's owner → alice's merge is
// pat's, red.
func TestAMergeCommitIsItsMergers(t *testing.T) {
	t.Parallel()
	c := newMergeCase(t, codetest.SoloOptions{}, codetest.Settings(), map[string]string{"README.md": "one\n"})
	for _, tc := range []struct {
		name   string
		merger code.Principal
		author string
	}{
		{"a seat", alice(), codetest.FixtureAlice},
		{"a person", pat(), codetest.FixturePat},
	} {
		ref := "refs/heads/" + strings.ReplaceAll(tc.name, " ", "-")
		n := c.open(ref, map[string]string{tc.author + ".md": "by " + tc.name + "\n"}, "Merged by "+tc.name)
		c.meetGate(n, lead())
		tip := strings.TrimSpace(c.p.Git().Run(c.T, c.clone(), "rev-parse", "main"))
		head := c.change(n).Head
		res, err := c.write(merge(c.repo, tc.merger, n, head, code.MergeMethodMerge))
		commit := c.requireMerged(n, res, err)
		got := c.show(c.clone(), "%an <%ae>|%cn <%ce>|%P", commit)
		want := fmt.Sprintf("%s <%s>|%s <%s>|%s %s", tc.author, codetest.FixtureAuthorEmail(tc.author),
			codetest.FixtureCommitterName, codetest.FixtureCommitterEmail, tip, head)
		if got != want {
			t.Errorf("%s's merge commit is %q, want %q: its merger's, committed by the engine, on main's tip "+
				"and the head", tc.name, got, want)
		}
		if subject := c.show(c.clone(), "%s", commit); subject != fmt.Sprintf("Merge %s!%d: Merged by %s",
			codetest.RepoName, n, tc.name) {
			t.Errorf("%s's merge commit is titled %q", tc.name, subject)
		}
	}
}

// A SQUASH NAMES ITS CO-AUTHORS (U43 a): a change pat opened and alice added
// a commit to, squashed by the seat lead, is ONE commit on main's tip
// authored by pat — the change's owner, not its merger — carrying a
// `Co-authored-by:` trailer for alice and none for pat, committed by the
// engine; its tree is the change's, and its message the change's title and
// reference.
//
// Mutation: drop the trailers → alice goes unnamed, red; author the squash by
// its merger → lead's, red.
func TestASquashNamesItsCoAuthors(t *testing.T) {
	t.Parallel()
	c := newMergeCase(t, codetest.SoloOptions{}, codetest.Settings(), map[string]string{"README.md": "one\n"})
	n := c.open(featureRef, map[string]string{"pat.md": "pat's\n"}, "Squash me")
	c.land(files(c.repo, alice(), featureRef, c.change(n).Head, "", text("alice.md", "alice's\n")))
	c.meetGate(n, lead())
	tip := strings.TrimSpace(c.p.Git().Run(c.T, c.clone(), "rev-parse", "main"))
	head := c.change(n).Head
	res, err := c.write(merge(c.repo, lead(), n, head, code.MergeMethodSquash))
	commit := c.requireMerged(n, res, err)
	clone := c.clone()
	got := c.show(clone, "%an <%ae>|%cn <%ce>|%P", commit)
	want := fmt.Sprintf("%s <%s>|%s <%s>|%s", codetest.FixturePat, codetest.FixtureAuthorEmail(codetest.FixturePat),
		codetest.FixtureCommitterName, codetest.FixtureCommitterEmail, tip)
	if got != want {
		t.Errorf("the squash is %q, want %q: its owner's, committed by the engine, one parent", got, want)
	}
	body := c.show(clone, "%B", commit)
	coauthor := fmt.Sprintf("Co-authored-by: %s <%s>", codetest.FixtureAlice,
		codetest.FixtureAuthorEmail(codetest.FixtureAlice))
	if !strings.HasPrefix(body, fmt.Sprintf("Squash me (%s!%d)", codetest.RepoName, n)) ||
		!strings.Contains(body, coauthor) || strings.Contains(body, "Co-authored-by: "+codetest.FixturePat) {
		t.Errorf("the squash's message is %q, want its title and reference and %q alone", body, coauthor)
	}
	if tree, want := c.show(clone, "%T", commit), c.show(clone, "%T", head); tree != want {
		t.Errorf("the squash's tree is %s, want the change's %s", tree, want)
	}
}

// AN UNREADY MERGE BUILDS NOTHING: a seat's merge of a change whose facts its
// home has not recorded yet is refused gate_unmet naming them — judged at the
// log's head before anything is built — and stores no object; a seat's
// override is refused not_person, likewise.
//
// Mutation: skip the readiness before the build → the merge's pack is stored
// for the record its decide refuses, red.
func TestAnUnreadyMergeBuildsNothing(t *testing.T) {
	t.Parallel()
	c := newMergeCase(t, codetest.SoloOptions{}, codetest.Settings(), map[string]string{"README.md": "one\n"})
	n := c.open(featureRef, map[string]string{"x.md": "x\n"}, "Not yet")
	head := c.change(n).Head
	before := len(c.objects())
	res, err := c.write(merge(c.repo, alice(), n, head, code.MergeMethodMerge))
	switch {
	case err != nil || res.Merge == nil || res.Merge.Refusal != code.MergeRefusalGateUnmet || res.Outcome != "":
		t.Fatalf("an unready merge answered %+v %v (%+v), want refused gate_unmet", res, err, res.Merge)
	case !slices.ContainsFunc(res.Merge.Unmet, func(p code.Precondition) bool {
		return p.Kind == code.PreconditionFacts
	}):
		t.Errorf("gate_unmet names %+v, not the facts it waits on", res.Merge.Unmet)
	}
	over := merge(c.repo, alice(), n, head, code.MergeMethodMerge)
	over.Override = "a seat's say-so"
	if res, err := c.write(over); err != nil || res.Merge == nil || res.Merge.Refusal != code.MergeRefusalNotPerson {
		t.Errorf("a seat's override answered %+v %v, want not_person", res, err)
	}
	if after := len(c.objects()); after != before {
		t.Errorf("refused merges stored %d objects: a refusal is decided before the bytes", after-before)
	}
	if ch := c.change(n); ch.State != code.ChangeStateOpen {
		t.Errorf("change %d is %s after refused merges", n, ch.State)
	}
}

// A SEAT'S MERGE IS HELD TO WHOEVER ITS TURN WORKS FOR, on the git node that
// judges it (code.EngineWrite.Ceiling, M3-decisions U40 (a)): alice, who may
// merge into main, merges a ready change for the unit's lead, who may too,
// and not for bob, who only reads the repository, nor for nobody — a
// stranger, or a person who may not act — each refused not_permitted in
// words saying whom the work is for, nothing built and the change left open.
//
// Mutation: mergeVerdict ignoring the write's ceiling — bob's and nobody's
// merges land, red.
func TestASeatsMergeIsHeldToWhoeverItsTurnWorksFor(t *testing.T) {
	t.Parallel()
	c := newMergeCase(t, codetest.SoloOptions{}, codetest.Settings(), map[string]string{"README.md": "one\n"})
	n := c.open(featureRef, map[string]string{"x.md": "x\n"}, "Merged for somebody")
	c.meetGate(n, lead())
	head := c.change(n).Head
	before := len(c.objects())
	for _, tc := range []struct {
		name    string
		ceiling *code.WriteCeiling
		says    string
	}{
		{"a colleague who only reads", &code.WriteCeiling{Kind: code.WriteCeilingSeat, Seat: codetest.FixtureBob},
			"this work is for seat bob"},
		{"nobody", &code.WriteCeiling{Kind: code.WriteCeilingNobody}, "may change nothing of this company's code"},
	} {
		w := merge(c.repo, alice(), n, head, code.MergeMethodMerge)
		w.Ceiling = tc.ceiling
		res, err := c.write(w)
		if err != nil || res.Merge == nil || res.Merge.Refusal != code.MergeRefusalNotPermitted ||
			!strings.Contains(res.Merge.Detail, tc.says) || res.Outcome != "" || res.Commit != "" {
			t.Errorf("alice's merge for %s answered %+v %v (%+v), want not_permitted saying %q", tc.name, res, err,
				res.Merge, tc.says)
		}
	}
	if after := len(c.objects()); after != before {
		t.Errorf("refused merges stored %d objects", after-before)
	}
	if ch := c.change(n); ch.State != code.ChangeStateOpen {
		t.Fatalf("change %d is %s after merges its requesters may not make", n, ch.State)
	}
	w := merge(c.repo, alice(), n, head, code.MergeMethodMerge)
	w.Ceiling = &code.WriteCeiling{Kind: code.WriteCeilingSeat, Seat: codetest.FixtureLead}
	res, err := c.write(w)
	c.requireMerged(n, res, err)
}

// movingMain moves main under every merge record it is asked to decide, the
// first moves times — a person's push landing between the merge's build and
// its decide — and counts the records asked.
type movingMain struct {
	host.Estate
	mu    sync.Mutex
	moves int
	asked int
	move  func(i int)
}

func (m *movingMain) wrap(inner host.Estate) host.Estate {
	m.Estate = inner
	return m
}

func (m *movingMain) Merge(ctx context.Context, p code.MergeProposal) (code.MergeResult, error) {
	m.mu.Lock()
	m.asked++
	i, moving := m.asked, m.asked <= m.moves
	m.mu.Unlock()
	if moving {
		m.move(i)
	}
	return m.Estate.Merge(ctx, p)
}

// A MERGE RECOMPUTES FOUR TIMES, THEN REFUSES (M5-D10): a merge whose target
// moves between its build and its decide is refused target_moved and built
// again onto the tip the target then holds — twice moved, it merges on its
// third build, answering its two recomputes, its commit on the tip the last
// push left; moved under its first build and under every one of
// code.MergeRecomputes rebuilds after it, it is answered target_moving after
// five records asked, and the change stays open.
//
// Mutation: rebuild only three times → four records asked, red; answer the
// first target_moved → nothing merges after two moves, red.
func TestAMergeRecomputesFourTimesThenRefuses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		moves  int
		merged bool
	}{
		{"moved twice", 2, true},
		{"moved every time", code.MergeRecomputes + 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := &movingMain{moves: tc.moves}
			c := newMergeCase(t, codetest.SoloOptions{Shape: codetest.Shape{WrapEstate: m.wrap}},
				codetest.Settings(), map[string]string{"README.md": "one\n"})
			m.move = func(i int) {
				c.p.Commit(c.work, map[string]string{fmt.Sprintf("main-%d.md", i): "moved\n"}, "move main")
				c.MustPush(c.p, c.work, "main")
			}
			n := c.open(featureRef, map[string]string{"x.md": "x\n"}, "Chase main")
			head := c.change(n).Head
			// A PERSON'S OVERRIDE, so no push of main's leaves the gate to
			// judge again: what is measured is the rebuild alone.
			w := merge(c.repo, pat(), n, head, code.MergeMethodMerge)
			w.Override = "chasing a moving main"
			res, err := c.write(w)
			if !tc.merged {
				if err != nil || res.Merge == nil || res.Merge.Refusal != code.MergeRefusalTargetMoving ||
					res.Merge.Recomputes != code.MergeRecomputes || m.asked != code.MergeRecomputes+1 {
					t.Fatalf("a merge chasing main answered %+v %v (%+v) after %d records, want target_moving "+
						"after %d recomputes", res, err, res.Merge, m.asked, code.MergeRecomputes)
				}
				if ch := c.change(n); ch.State != code.ChangeStateOpen {
					t.Errorf("change %d is %s, want open", n, ch.State)
				}
				return
			}
			commit := c.requireMerged(n, res, err)
			if res.Merge.Recomputes != tc.moves || m.asked != tc.moves+1 {
				t.Errorf("the merge answered %d recomputes after %d records, want %d after %d",
					res.Merge.Recomputes, m.asked, tc.moves, tc.moves+1)
			}
			if parents := c.show(c.clone(), "%P", commit); !strings.HasPrefix(parents, c.p.Git().Run(c.T, c.work,
				"rev-parse", "HEAD")[:40]) {
				t.Errorf("the merge sits on %s, not on the tip main's last push left", parents)
			}
		})
	}
}

// expungingBuilds answers the first refusals merges it is asked refused
// expunging, deciding nothing — as the committer refuses a merge whose
// builder's copy held another drop epoch than the repository's, an expunge
// having begun between its build and its decide — and hands every later one
// to the estate.
type expungingBuilds struct {
	host.Estate
	mu       sync.Mutex
	refusals int
	asked    int
}

func (e *expungingBuilds) wrap(inner host.Estate) host.Estate {
	e.Estate = inner
	return e
}

func (e *expungingBuilds) Merge(ctx context.Context, p code.MergeProposal) (code.MergeResult, error) {
	e.mu.Lock()
	e.asked++
	refused := e.asked <= e.refusals
	e.mu.Unlock()
	if refused {
		return code.MergeResult{MergeID: p.Merge.MergeID, Refusal: code.MergeRefusalExpunging,
			Detail: code.ExpungingBuiltDetail(codetest.RepoName, "merge", "its builder", p.DropEpoch,
				p.DropEpoch+1)}, nil
	}
	return e.Estate.Merge(ctx, p)
}

// A MERGE REFUSED EXPUNGING IS BUILT AGAIN (M6-D4): a merge an expunge's
// begin overtook between its build and its decide is refused expunging,
// nothing decided, and its builder builds it again from a copy synced past the
// begin, as it does a merge whose target moved — the rebuild counted with the
// target_moved ones, so a merge refused under its fifth build too is answered
// expunging in the committer's own sentence, the change left open.
//
// Mutation: answer the first expunging refusal as the merge's answer, rather
// than building it again, and the merge refused once is never landed; answer
// target_moving at the limit, and the last case's refusal is misnamed.
func TestAMergeRefusedExpungingIsBuiltAgain(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name     string
		refusals int
		merged   bool
	}{
		{"refused once", 1, true},
		{"refused every time", code.MergeRecomputes + 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := &expungingBuilds{refusals: tc.refusals}
			c := newMergeCase(t, codetest.SoloOptions{Shape: codetest.Shape{WrapEstate: e.wrap}},
				codetest.Settings(), map[string]string{"README.md": "one\n"})
			n := c.open(featureRef, map[string]string{"x.md": "x\n"}, "Outlive an expunge")
			w := merge(c.repo, pat(), n, c.change(n).Head, code.MergeMethodMerge)
			w.Override = "merging through an expunge"
			res, err := c.write(w)
			if !tc.merged {
				if err != nil || res.Merge == nil || res.Merge.Refusal != code.MergeRefusalExpunging ||
					!strings.Contains(res.Merge.Detail, "its builder builds it again") ||
					res.Merge.Recomputes != code.MergeRecomputes || e.asked != code.MergeRecomputes+1 {
					t.Fatalf("a merge every expunge overtook answered %+v %v (%+v) after %d records, want "+
						"expunging after %d recomputes", res, err, res.Merge, e.asked, code.MergeRecomputes)
				}
				if ch := c.change(n); ch.State != code.ChangeStateOpen {
					t.Errorf("change %d is %s, want open", n, ch.State)
				}
				return
			}
			c.requireMerged(n, res, err)
			if res.Merge.Recomputes != 1 || e.asked != 2 {
				t.Errorf("the merge answered %d recomputes after %d records, want 1 after 2",
					res.Merge.Recomputes, e.asked)
			}
		})
	}
}

// A QUEUE TARGET'S MERGE IS AN ENQUEUE (M5-D9): a target whose gate requires a
// check takes a change through its queue, so a merge_change of a change whose
// head passed it builds nothing and enqueues the change, answering its entry
// and its place; main does not move; and a person's override merges it
// directly, past the queue.
//
// Mutation: build the merge on a queue target → the record is refused
// queue_target, red.
func TestAQueueTargetsMergeIsEnqueued(t *testing.T) {
	t.Parallel()
	settings := codetest.Settings()
	settings.Protections[0].Gate = &code.MergeGate{MergeBy: code.MergeByWriters, Approvals: code.ApprovalsNone,
		Checks: []string{"unit"}, Landing: code.Landing{Depth: 4, Batch: 4}, DeleteSource: code.DeleteNone}
	c := newMergeCase(t, codetest.SoloOptions{}, settings, map[string]string{"README.md": "one\n"})
	n := c.open(featureRef, map[string]string{"x.md": "x\n"}, "Queue me")
	c.meetGate(n, lead())
	head := c.change(n).Head
	// THE HEAD'S REQUIRED CHECK PASSED, for main: the gate an enqueue is
	// decided by holds a change's head to it.
	unit := code.CheckKey{RepoID: c.repo, OID: head, Target: "refs/heads/main", Name: "unit", Attempt: 1}
	queueCheck(t, c.solo(), unit)
	endCheck(t, c.solo(), unit)
	before := len(c.objects())
	w := merge(c.repo, alice(), n, head, code.MergeMethodSquash)
	res, err := c.write(w)
	if err != nil || res.Outcome != statelog.OutcomeApplied || res.Merge == nil || !res.Merge.Queued ||
		res.Merge.EntryID != code.QueueEntryID(w.OpID) || res.Merge.Place != 1 || res.Commit != "" {
		t.Fatalf("a merge onto a queue target answered %+v %v (%+v), want enqueued first", res, err, res.Merge)
	}
	if after := len(c.objects()); after != before {
		t.Errorf("an enqueue stored %d objects: the queue builds its candidates itself", after-before)
	}
	if ch := c.change(n); ch.State != code.ChangeStateOpen {
		t.Errorf("an enqueued change is %s", ch.State)
	}
	n2 := c.open("refs/heads/urgent", map[string]string{"y.md": "y\n"}, "Merge me now")
	over := merge(c.repo, pat(), n2, c.change(n2).Head, code.MergeMethodMerge)
	over.Override = "the release cannot wait for the queue"
	res, err = c.write(over)
	c.requireMerged(n2, res, err)
	if ch := c.change(n2); ch.OverrideReason != over.Override || ch.MergedBy != codetest.FixturePat {
		t.Errorf("the override merge is %+v, want pat's with its reason", ch.ChangeRow)
	}
}

// A MERGE_CHANGE IS HELD TO ITS SHAPE by the rule every party holds a write
// to: a change by its number, a method this build knows, the head its caller
// read, and none of the fields another kind takes — and no other kind takes a
// merge's.
func TestAMergeChangeIsHeldToItsShape(t *testing.T) {
	t.Parallel()
	repo := uuid.Must(uuid.NewV7()).String()
	ok := merge(repo, pat(), 3, strings.Repeat("a", 40), code.MergeMethodSquash)
	if err := ok.Validate(); err != nil {
		t.Fatalf("a whole merge_change is refused: %v", err)
	}
	long := strings.Repeat("x", code.MaxOverrideReasonBytes+1)
	for _, tc := range []struct {
		name  string
		edit  func(w *code.EngineWrite)
		field string
	}{
		{"no change", func(w *code.EngineWrite) { w.Change = 0 }, "change"},
		{"no method", func(w *code.EngineWrite) { w.Method = "" }, "method"},
		{"a method nobody knows", func(w *code.EngineWrite) { w.Method = "octopus" }, "method"},
		{"no head", func(w *code.EngineWrite) { w.ExpectedHead = "" }, "expected_head"},
		{"a message", func(w *code.EngineWrite) { w.Message = "mine" }, "message"},
		{"files", func(w *code.EngineWrite) { w.Files = []code.FileOp{text("a", "a")} }, "files"},
		{"a long override", func(w *code.EngineWrite) { w.Override = long }, "override"},
	} {
		w := ok
		tc.edit(&w)
		if err := w.Validate(); err == nil || !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: %v, want a refusal naming %s", tc.name, err, tc.field)
		}
	}
	other := files(repo, pat(), "refs/heads/x", "", "", text("a", "a"))
	other.Method = code.MergeMethodMerge
	if err := other.Validate(); err == nil || !strings.Contains(err.Error(), "method") {
		t.Errorf("a commit_files carrying a merge's method: %v, want refused naming method", err)
	}
}
