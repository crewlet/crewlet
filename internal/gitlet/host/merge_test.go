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

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/gitlet/host"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// A CHANGE'S MERGE, BUILT ON ITS REPOSITORY'S GITLET NODE (merge.go): a
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
	pat     *gitlettest.Person
	patWork string
}

// newMergeCase is a merge case over o and settings, main seeded by ceo with
// seed, pat holding a clone of it.
func newMergeCase(t *testing.T, o gitlettest.SoloOptions, settings gitlet.RepoSettings,
	seed map[string]string) *mergeCase {

	t.Helper()
	c := newCase(t, o, settings)
	p := gitlettest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, seed, "seed")
	c.MustPush(p, work, "main")
	wc := &writeCase{Case: c, p: p, work: work, head: head, repo: c.Rows().Repo.ID}
	pat := gitlettest.NewPerson(t, c.Fleet, gitlettest.FixturePat)
	return &mergeCase{writeCase: wc, pat: pat, patWork: pat.Clone(c.URL(0))}
}

// solo is the case's fleet as the solo fleet it is.
func (c *mergeCase) solo() *gitlettest.SoloFleet { return c.Fleet.(*gitlettest.SoloFleet) }

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
func (c *mergeCase) changes() []gitlet.ChangeSummary {
	c.T.Helper()
	ans, err := c.solo().Code(c.T).Read(c.Ctx, gitlet.ReadQuery{Kind: gitlet.ReadChanges, Asker: pat(),
		Repo: gitlettest.RepoName, Fresh: linear})
	if err != nil {
		c.T.Fatalf("read the changes: %v", err)
	}
	return ans.Changes
}

// change is change n, read as pat at the log's head.
func (c *mergeCase) change(n int64) *gitlet.ChangeDetail {
	c.T.Helper()
	ans, err := c.solo().Code(c.T).Read(c.Ctx, gitlet.ReadQuery{Kind: gitlet.ReadChange, Asker: pat(),
		Repo: gitlettest.RepoName, Change: n, Fresh: linear})
	if err != nil || ans.Change == nil {
		c.T.Fatalf("read change %d: %+v %v", n, ans.Change, err)
	}
	return ans.Change
}

// meetGate makes change n's head revision meet a gate asking nothing beyond
// M4's: its facts recorded and an approval of it by approver — the
// conversation's asker, which names no run.
func (c *mergeCase) meetGate(n int64, approver gitlet.Principal) {
	c.T.Helper()
	d := c.recordFacts(n)
	approver.Run = gitlet.RunProvenance{}
	res, err := c.solo().Code(c.T).Converse(c.Ctx, gitlet.ConversationWrite{OpID: opNow(), Asker: approver,
		Review: &gitlet.Review{RepoID: c.repo, ChangeID: d.ID, Revision: d.Revision, Verdict: gitlet.VerdictApprove,
			Summary: "Looks right."}})
	if err != nil || res.Refusal != "" {
		c.T.Fatalf("approve change %d: %+v %v", n, res, err)
	}
}

// recordFacts records change n's head revision's facts against its target as
// its home records them — read through the git reads, at the tip and
// position the due listing names — and answers the change as that listing
// named it.
func (c *mergeCase) recordFacts(n int64) gitlet.ChangeDue {
	c.T.Helper()
	seam := c.solo().Code(c.T)
	due, err := seam.Read(c.Ctx, gitlet.ReadQuery{Kind: gitlet.ReadChangesDue, Fresh: linear,
		ChangesDue: &gitlet.ChangesDueQuery{RepoID: c.repo, RecheckBefore: time.Now()}})
	if err != nil {
		c.T.Fatalf("read the due changes: %v", err)
	}
	i := slices.IndexFunc(due.DueChanges, func(d gitlet.ChangeDue) bool { return d.Number == n })
	if i < 0 {
		c.T.Fatalf("change %d is not due its facts: %+v", n, due.DueChanges)
	}
	d := due.DueChanges[i]
	read, err := c.solo().GitRead(c.Ctx, gitlet.GitQuery{Kind: gitlet.GitRevisionFacts, RepoID: c.repo, Fresh: linear,
		RevisionFacts: &gitlet.RevisionFactsQuery{Head: d.Head, Target: d.Tip}})
	if err != nil || read.RevisionFacts == nil {
		c.T.Fatalf("read revision %d's facts: %+v %v", d.Revision, read, err)
	}
	f := read.RevisionFacts
	_, digest := gitlet.MatchOwners(due.PathOwners, f.Touched, f.TouchesAll)
	held, humanDigest, err := gitlet.MatchTargetHumanPaths(due.Gates, d.TargetRef, f.Touched, f.TouchesAll, f.Classes)
	if err != nil {
		c.T.Fatalf("match revision %d's human paths: %v", d.Revision, err)
	}
	res, err := seam.Converse(c.Ctx, gitlet.ConversationWrite{OpID: opNow(), Revise: &gitlet.Revise{RepoID: c.repo,
		ChangeID: d.ID, N: d.Revision, HeadOID: d.Head, TargetRef: d.TargetRef, TargetOID: d.Tip,
		TargetPosition: d.TipPosition, BaseOID: f.MergeBase, BaseTree: f.BaseTree, HeadTree: f.HeadTree,
		PatchID: f.PatchID, PatchIDOmitted: f.PatchIDOmission == gitlet.OmittedPastBound, Touched: f.Touched,
		TouchesAll: f.TouchesAll, Classes: f.Classes, Node: "solo-a", OwnersDigest: digest, HumanPaths: held,
		HumanPathsDigest: humanDigest}})
	if err != nil || res.Refusal != "" {
		c.T.Fatalf("record revision %d's facts: %+v %v", d.Revision, res, err)
	}
	return d
}

// merge is who's merge_change of change n into main at head, by method.
func merge(repo string, who gitlet.Principal, n int64, head string, method gitlet.MergeMethod) gitlet.EngineWrite {
	return gitlet.EngineWrite{Kind: gitlet.WriteMergeChange, RepoID: repo, Principal: who, OpID: opNow(),
		Fresh: linear, Ref: "refs/heads/main", ExpectedHead: head, Change: n, Method: method}
}

// lead is the fixture chart's ENG lead, as their write names them.
func lead() gitlet.Principal {
	return gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: gitlettest.FixtureLead,
		Run: gitlet.RunProvenance{TurnID: uuid.Must(uuid.NewV7()).String(), WorkKey: "ENG-9", Model: "fixture"}}
}

// requireMerged fails the case unless res is a merge applied onto main, its
// change merged at the commit main now holds, and answers that commit.
func (c *mergeCase) requireMerged(n int64, res gitlet.EngineWriteResult, err error) string {
	c.T.Helper()
	if err != nil || res.Outcome != statelog.OutcomeApplied || res.Merge == nil || res.Merge.Refusal != "" ||
		res.Commit == "" {
		c.T.Fatalf("the merge of change %d did not apply: %+v %v (%+v)", n, res, err, res.Merge)
	}
	ch := c.change(n)
	if ch.State != gitlet.ChangeStateMerged || ch.MergeOID != res.Commit || ch.MergedVia != gitlet.MergedViaDirect {
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
	c := newMergeCase(t, gitlettest.SoloOptions{}, gitlettest.Settings(), map[string]string{"README.md": "one\n"})
	for _, tc := range []struct {
		name   string
		merger gitlet.Principal
		author string
	}{
		{"a seat", alice(), gitlettest.FixtureAlice},
		{"a person", pat(), gitlettest.FixturePat},
	} {
		ref := "refs/heads/" + strings.ReplaceAll(tc.name, " ", "-")
		n := c.open(ref, map[string]string{tc.author + ".md": "by " + tc.name + "\n"}, "Merged by "+tc.name)
		c.meetGate(n, lead())
		tip := strings.TrimSpace(c.p.Git().Run(c.T, c.clone(), "rev-parse", "main"))
		head := c.change(n).Head
		res, err := c.write(merge(c.repo, tc.merger, n, head, gitlet.MergeMethodMerge))
		commit := c.requireMerged(n, res, err)
		got := c.show(c.clone(), "%an <%ae>|%cn <%ce>|%P", commit)
		want := fmt.Sprintf("%s <%s>|%s <%s>|%s %s", tc.author, gitlettest.FixtureAuthorEmail(tc.author),
			gitlettest.FixtureCommitterName, gitlettest.FixtureCommitterEmail, tip, head)
		if got != want {
			t.Errorf("%s's merge commit is %q, want %q: its merger's, committed by the engine, on main's tip "+
				"and the head", tc.name, got, want)
		}
		if subject := c.show(c.clone(), "%s", commit); subject != fmt.Sprintf("Merge %s!%d: Merged by %s",
			gitlettest.RepoName, n, tc.name) {
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
	c := newMergeCase(t, gitlettest.SoloOptions{}, gitlettest.Settings(), map[string]string{"README.md": "one\n"})
	n := c.open(featureRef, map[string]string{"pat.md": "pat's\n"}, "Squash me")
	c.land(files(c.repo, alice(), featureRef, c.change(n).Head, "", text("alice.md", "alice's\n")))
	c.meetGate(n, lead())
	tip := strings.TrimSpace(c.p.Git().Run(c.T, c.clone(), "rev-parse", "main"))
	head := c.change(n).Head
	res, err := c.write(merge(c.repo, lead(), n, head, gitlet.MergeMethodSquash))
	commit := c.requireMerged(n, res, err)
	clone := c.clone()
	got := c.show(clone, "%an <%ae>|%cn <%ce>|%P", commit)
	want := fmt.Sprintf("%s <%s>|%s <%s>|%s", gitlettest.FixturePat, gitlettest.FixtureAuthorEmail(gitlettest.FixturePat),
		gitlettest.FixtureCommitterName, gitlettest.FixtureCommitterEmail, tip)
	if got != want {
		t.Errorf("the squash is %q, want %q: its owner's, committed by the engine, one parent", got, want)
	}
	body := c.show(clone, "%B", commit)
	coauthor := fmt.Sprintf("Co-authored-by: %s <%s>", gitlettest.FixtureAlice,
		gitlettest.FixtureAuthorEmail(gitlettest.FixtureAlice))
	if !strings.HasPrefix(body, fmt.Sprintf("Squash me (%s!%d)", gitlettest.RepoName, n)) ||
		!strings.Contains(body, coauthor) || strings.Contains(body, "Co-authored-by: "+gitlettest.FixturePat) {
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
	c := newMergeCase(t, gitlettest.SoloOptions{}, gitlettest.Settings(), map[string]string{"README.md": "one\n"})
	n := c.open(featureRef, map[string]string{"x.md": "x\n"}, "Not yet")
	head := c.change(n).Head
	before := len(c.objects())
	res, err := c.write(merge(c.repo, alice(), n, head, gitlet.MergeMethodMerge))
	switch {
	case err != nil || res.Merge == nil || res.Merge.Refusal != gitlet.MergeRefusalGateUnmet || res.Outcome != "":
		t.Fatalf("an unready merge answered %+v %v (%+v), want refused gate_unmet", res, err, res.Merge)
	case !slices.ContainsFunc(res.Merge.Unmet, func(p gitlet.Precondition) bool {
		return p.Kind == gitlet.PreconditionFacts
	}):
		t.Errorf("gate_unmet names %+v, not the facts it waits on", res.Merge.Unmet)
	}
	over := merge(c.repo, alice(), n, head, gitlet.MergeMethodMerge)
	over.Override = "a seat's say-so"
	if res, err := c.write(over); err != nil || res.Merge == nil || res.Merge.Refusal != gitlet.MergeRefusalNotPerson {
		t.Errorf("a seat's override answered %+v %v, want not_person", res, err)
	}
	if after := len(c.objects()); after != before {
		t.Errorf("refused merges stored %d objects: a refusal is decided before the bytes", after-before)
	}
	if ch := c.change(n); ch.State != gitlet.ChangeStateOpen {
		t.Errorf("change %d is %s after refused merges", n, ch.State)
	}
}

// A MERGE INTO A PULL MIRROR BUILDS NOTHING, and neither does one into a
// repository being imported: a ready change's merge — its gate met — on a
// repository made a pull mirror is refused repo_importing while the mirror's
// import runs, and repo_mirrored once it is done, each in the domain's one
// sentence for the state (gitlet.ImportingDetail, gitlet.MirroredDetail), by the
// Gitlet node from the row it synced, BEFORE anything is built: no object is
// stored, and the change stays open. The decide refuses either from the same
// row, but only once a scratch, a share unit and a stored pack were spent on
// a merge it could never take.
//
// Mutation: drop the importing arm, or the mirror arm, of the Gitlet node's
// status mapping — that merge is built and its pack stored before its decide
// refuses it, red.
func TestAMergeIntoAPullMirrorBuildsNothing(t *testing.T) {
	t.Parallel()
	c := newMergeCase(t, gitlettest.SoloOptions{}, gitlettest.Settings(), map[string]string{"README.md": "one\n"})
	n := c.open(featureRef, map[string]string{"x.md": "x\n"}, "Into a mirror")
	c.meetGate(n, lead())
	head := c.change(n).Head
	mirrored := gitlettest.Settings()
	mirrored.MirrorFrom = &gitlet.MirrorSource{URL: "https://forge.example.com/acme/web.git"}
	c.Fleet.Declare(t, gitlet.Declared{Name: gitlettest.RepoName, Settings: mirrored})
	before := len(c.objects())
	refused := func(state string, want gitlet.MergeRefusal, words string) {
		t.Helper()
		res, err := c.write(merge(c.repo, alice(), n, head, gitlet.MergeMethodMerge))
		if err != nil || res.Merge == nil || res.Merge.Refusal != want || res.Merge.Detail != words ||
			res.Outcome != "" || res.Commit != "" {
			t.Fatalf("a merge into a repository %s answered %+v %v (%+v), want %s saying %q", state, res, err,
				res.Merge, want, words)
		}
		if after := len(c.objects()); after != before {
			t.Errorf("a merge into a repository %s stored %d objects: its row refuses it before anything is "+
				"built", state, after-before)
		}
	}
	refused("being imported", gitlet.MergeRefusalRepoImporting, gitlet.ImportingDetail(gitlettest.RepoName))
	done := gitlet.ImportDone{RepoID: c.repo, Node: c.Fleet.Nodes()[0].ID()}
	if res, err := c.solo().Code(t).Import(c.Ctx, gitlet.ImportProposal{Done: &done}); err != nil ||
		res.Outcome != statelog.OutcomeApplied {
		t.Fatalf("end the pull mirror's import: %+v, %v", res, err)
	}
	refused("that is a pull mirror", gitlet.MergeRefusalRepoMirrored,
		gitlet.MirroredDetail(gitlettest.RepoName, gitlet.ManagedConfig))
	if ch := c.change(n); ch.State != gitlet.ChangeStateOpen {
		t.Errorf("change %d is %s after merges a pull mirror refused", n, ch.State)
	}
}

// A SEAT'S MERGE IS HELD TO WHOEVER ITS TURN WORKS FOR, on the Gitlet node that
// judges it (gitlet.EngineWrite.Ceiling, M3-decisions U40 (a)): alice, who may
// merge into main, merges a ready change for the unit's lead, who may too,
// and not for bob, who only reads the repository, nor for nobody — a
// stranger, or a person who may not act — each refused not_permitted in
// words saying whom the work is for, nothing built and the change left open.
//
// Mutation: mergeVerdict ignoring the write's ceiling — bob's and nobody's
// merges land, red.
func TestASeatsMergeIsHeldToWhoeverItsTurnWorksFor(t *testing.T) {
	t.Parallel()
	c := newMergeCase(t, gitlettest.SoloOptions{}, gitlettest.Settings(), map[string]string{"README.md": "one\n"})
	n := c.open(featureRef, map[string]string{"x.md": "x\n"}, "Merged for somebody")
	c.meetGate(n, lead())
	head := c.change(n).Head
	before := len(c.objects())
	for _, tc := range []struct {
		name    string
		ceiling *gitlet.WriteCeiling
		says    string
	}{
		{"a colleague who only reads", &gitlet.WriteCeiling{Kind: gitlet.WriteCeilingSeat, Seat: gitlettest.FixtureBob},
			"this work is for seat bob"},
		{"nobody", &gitlet.WriteCeiling{Kind: gitlet.WriteCeilingNobody}, "may change nothing of this company's code"},
	} {
		w := merge(c.repo, alice(), n, head, gitlet.MergeMethodMerge)
		w.Ceiling = tc.ceiling
		res, err := c.write(w)
		if err != nil || res.Merge == nil || res.Merge.Refusal != gitlet.MergeRefusalNotPermitted ||
			!strings.Contains(res.Merge.Detail, tc.says) || res.Outcome != "" || res.Commit != "" {
			t.Errorf("alice's merge for %s answered %+v %v (%+v), want not_permitted saying %q", tc.name, res, err,
				res.Merge, tc.says)
		}
	}
	if after := len(c.objects()); after != before {
		t.Errorf("refused merges stored %d objects", after-before)
	}
	if ch := c.change(n); ch.State != gitlet.ChangeStateOpen {
		t.Fatalf("change %d is %s after merges its requesters may not make", n, ch.State)
	}
	w := merge(c.repo, alice(), n, head, gitlet.MergeMethodMerge)
	w.Ceiling = &gitlet.WriteCeiling{Kind: gitlet.WriteCeilingSeat, Seat: gitlettest.FixtureLead}
	res, err := c.write(w)
	c.requireMerged(n, res, err)
}

// AN UNSAID SOURCE DELETION IS THE TARGET'S GATE'S, decided on the Gitlet node
// that judges the merge: a change from a seat's branch merged with nothing
// said of its branch deletes it where no gate over the target keeps it, and
// keeps it where one does (delete_source: none) — each answered as decided
// and done by the merge's own record — while a branch outside every seat's
// namespace is kept, and a caller's own word wins over the gate's.
//
// Mutations: the Gitlet node ignoring the gate's KeepsSource → the kept seat
// branch is deleted, red; defaulting to keep → the seat branch outlives its
// merge, red; ignoring the caller's word → red.
func TestAnUnsaidSourceDeletionIsTheTargetGates(t *testing.T) {
	t.Parallel()
	yes := true
	for _, tc := range []struct {
		name    string
		keeps   bool
		source  string
		said    *bool
		deleted bool
	}{
		{"a seat's branch, no gate keeping it", false, "refs/heads/seats/alice/fix", nil, true},
		{"a seat's branch a gate keeps", true, "refs/heads/seats/alice/fix", nil, false},
		{"a branch of nobody's namespace", false, featureRef, nil, false},
		{"a branch a gate keeps, deleted as said", true, "refs/heads/seats/alice/fix", &yes, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			settings := gitlettest.Settings()
			if tc.keeps {
				settings.Protections[0].Gate = &gitlet.MergeGate{MergeBy: gitlet.MergeByWriters,
					Approvals: gitlet.ApprovalsNone, Landing: gitlet.Landing{Depth: 4, Batch: 4},
					DeleteSource: gitlet.DeleteNone}
			}
			c := newMergeCase(t, gitlettest.SoloOptions{}, settings, map[string]string{"README.md": "one\n"})
			n := c.open(tc.source, map[string]string{"x.md": "x\n"}, "Merge my branch")
			c.meetGate(n, lead())
			w := merge(c.repo, pat(), n, c.change(n).Head, gitlet.MergeMethodMerge)
			w.DeleteSource = tc.said
			res, err := c.write(w)
			c.requireMerged(n, res, err)
			if res.Merge.DeleteSource != tc.deleted {
				t.Errorf("the merge answered delete_source %v, want %v", res.Merge.DeleteSource, tc.deleted)
			}
			if _, kept := c.Rows().Refs[tc.source]; kept == tc.deleted {
				t.Errorf("%s is kept %v after the merge, want deleted %v", tc.source, kept, tc.deleted)
			}
		})
	}
}

// changeReads records every change read the host asks of its estate.
type changeReads struct {
	host.Estate
	mu    sync.Mutex
	asked []gitlet.ReadQuery
}

func (r *changeReads) wrap(inner host.Estate) host.Estate {
	r.Estate = inner
	return r
}

func (r *changeReads) Read(ctx context.Context, q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
	if q.Kind == gitlet.ReadChange {
		r.mu.Lock()
		r.asked = append(r.asked, q)
		r.mu.Unlock()
	}
	return r.Estate.Read(ctx, q)
}

// of is how many times change n was read.
func (r *changeReads) of(n int64) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	read := 0
	for _, q := range r.asked {
		if q.Change == n {
			read++
		}
	}
	return read
}

// since is every change read r recorded from the from-th on, and the count to
// read the next from.
func (r *changeReads) since(from int) ([]gitlet.ReadQuery, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Clone(r.asked[from:]), len(r.asked)
}

// A COLLEAGUE-STEERED MERGE READS ITS CHANGE ONCE: a seat's merge made for
// the colleague whose ask woke its turn is held to the colleague's merge
// authority by the one rule of that term (gitlet.MergeAuthority), judged on the
// Gitlet node from its own chart, the synced row and the target's gate — so the
// node reads the change once, as the merger, and never again as the
// colleague; and the merge the colleague may make lands.
//
// Mutation: mergeCeiling reading the change again as the colleague → two
// reads, red.
func TestAColleagueSteeredMergeReadsItsChangeOnce(t *testing.T) {
	t.Parallel()
	reads := &changeReads{}
	c := newMergeCase(t, gitlettest.SoloOptions{Shape: gitlettest.Shape{WrapEstate: reads.wrap}},
		gitlettest.Settings(), map[string]string{"README.md": "one\n"})
	n := c.open(featureRef, map[string]string{"x.md": "x\n"}, "Merged for the lead")
	c.meetGate(n, lead())
	head := c.change(n).Head
	before := reads.of(n)
	w := merge(c.repo, alice(), n, head, gitlet.MergeMethodMerge)
	w.Ceiling = &gitlet.WriteCeiling{Kind: gitlet.WriteCeilingSeat, Seat: gitlettest.FixtureLead}
	res, err := c.write(w)
	read := reads.of(n) - before
	c.requireMerged(n, res, err)
	if read != 1 {
		t.Errorf("the Gitlet node read change %d %d times to merge it for the lead, want once: the lead's merge "+
			"authority is judged from what the node holds", n, read)
	}
}

// A CHANGE'S WRITES READ NO REVISION THEY DROP: a change rebased by pat onto
// a main that moved, merged by the seat alice for the unit's lead — its change
// read as alice alone, the lead's merge authority judged from what the Gitlet
// node holds (TestAColleagueSteeredMergeReadsItsChangeOnce) — and reverted
// by pat. The
// rebase and the merge read the change as its detail alone
// (gitlet.ReadQuery.DetailOnly), never with a page of its revisions beside it:
// neither reads a revision, a merge reads its change again at the log's head
// before a refusal and on every rebuild, and a first page of revisions is up
// to 200 of them, each carrying its facts' touched paths, shipped from a data
// node to the Gitlet node to be dropped. The revert reads its detail and its
// HEAD REVISION alone — a first page one revision long — whose facts place a
// fast-forward's range.
//
// Mutations: read the merge's change or the rebase's with its
// revisions → that read asks for them, red; read the revert's with a whole
// page → red.
func TestAChangesWritesReadNoRevisionTheyDrop(t *testing.T) {
	t.Parallel()
	r := &changeReads{}
	c := newMergeCase(t, gitlettest.SoloOptions{Shape: gitlettest.Shape{WrapEstate: r.wrap}}, gitlettest.Settings(),
		map[string]string{"README.md": "one\n"})
	c.branchOff("feature")
	c.pat.Commit(c.patWork, map[string]string{"x.md": "x\n"}, "Add x")
	head := strings.TrimSpace(c.pat.Git().Run(c.T, c.patWork, "rev-parse", "HEAD"))
	n := c.openPushed(featureRef, "Read alone")
	c.moveMain("main.md", "moved\n")
	_, from := r.since(0)
	rebased, err := c.write(rebase(c.repo, pat(), n, featureRef, head))
	if err != nil || rebased.Outcome != statelog.OutcomeApplied {
		t.Fatalf("the rebase did not land: %+v %v", rebased, err)
	}
	rebaseReads, from := r.since(from)
	c.meetGate(n, lead())
	w := merge(c.repo, alice(), n, c.change(n).Head, gitlet.MergeMethodMerge)
	w.Ceiling = &gitlet.WriteCeiling{Kind: gitlet.WriteCeilingSeat, Seat: gitlettest.FixtureLead}
	res, err := c.write(w)
	c.requireMerged(n, res, err)
	mergeReads, from := r.since(from)
	if got, err := c.write(revertChange(c.repo, pat(), n, "refs/heads/revert-read-alone")); err != nil ||
		got.Outcome != statelog.OutcomeApplied {
		t.Fatalf("the revert did not land: %+v %v", got, err)
	}
	revertReads, _ := r.since(from)

	for _, tc := range []struct {
		write  string
		reads  []gitlet.ReadQuery
		askers []string
		limit  int
		alone  bool
	}{
		{"rebase", rebaseReads, []string{gitlettest.FixturePat}, 0, true},
		{"merge", mergeReads, []string{gitlettest.FixtureAlice}, 0, true},
		{"revert", revertReads, []string{gitlettest.FixturePat}, 1, false},
	} {
		askers := map[string]bool{}
		for _, q := range tc.reads {
			askers[q.Asker.Seat] = true
			if q.DetailOnly != tc.alone || q.Limit != tc.limit || q.Cursor != "" || q.Part != "" {
				t.Errorf("the %s read change %d as %s as %+v, want detail_only %v and limit %d", tc.write,
					q.Change, q.Asker.Seat, q, tc.alone, tc.limit)
			}
		}
		// EVERY READ IT MAKES was seen: each write's as its asker.
		for _, seat := range tc.askers {
			if !askers[seat] {
				t.Errorf("the %s asked no change read as %s among its %d", tc.write, seat, len(tc.reads))
			}
		}
	}
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

func (m *movingMain) Merge(ctx context.Context, p gitlet.MergeProposal) (gitlet.MergeResult, error) {
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
// gitlet.MergeRecomputes rebuilds after it, it is answered target_moving after
// five records asked, and the change stays open. Each merge's rebuilds are
// observed once as it ends, on crewlet.code.merge.recomputes under how it
// ended: 2 merged, 4 target_moving.
//
// Mutation: rebuild only three times → four records asked, red; answer the
// first target_moved → nothing merges after two moves, red; drop
// recordRecomputes' observation → no merge observed, red.
func TestAMergeRecomputesFourTimesThenRefuses(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		moves  int
		merged bool
	}{
		{"moved twice", 2, true},
		{"moved every time", gitlet.MergeRecomputes + 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := &movingMain{moves: tc.moves}
			c := newMergeCase(t, gitlettest.SoloOptions{Shape: gitlettest.Shape{WrapEstate: m.wrap}},
				gitlettest.Settings(), map[string]string{"README.md": "one\n"})
			m.move = func(i int) {
				c.p.Commit(c.work, map[string]string{fmt.Sprintf("main-%d.md", i): "moved\n"}, "move main")
				c.MustPush(c.p, c.work, "main")
			}
			n := c.open(featureRef, map[string]string{"x.md": "x\n"}, "Chase main")
			head := c.change(n).Head
			// A PERSON'S OVERRIDE, so no push of main's leaves the gate to
			// judge again: what is measured is the rebuild alone.
			w := merge(c.repo, pat(), n, head, gitlet.MergeMethodMerge)
			w.Override = "chasing a moving main"
			res, err := c.write(w)
			// THE REBUILDS ARE OBSERVED ONCE, as the merge ends, on the Gitlet
			// node that built it: the count its answer carries, under how it
			// ended — never once a round, nor left unrecorded. (A node that
			// answered unready and handed the walk on records its own, which
			// is no merge built.)
			outcome, rebuilt := "merged", tc.moves
			if !tc.merged {
				outcome, rebuilt = string(gitlet.MergeRefusalTargetMoving), gitlet.MergeRecomputes
			}
			var merges, under uint64
			var sum float64
			for _, node := range c.Fleet.Nodes() {
				n, _ := observed(node.Metrics(), metrics.CodeMergeRecomputes, nil)
				handed, _ := observed(node.Metrics(), metrics.CodeMergeRecomputes,
					map[string]string{"outcome": "unready"})
				u, s := observed(node.Metrics(), metrics.CodeMergeRecomputes, map[string]string{"outcome": outcome})
				merges, under, sum = merges+n-handed, under+u, sum+s
			}
			if merges != 1 || under != 1 || sum != float64(rebuilt) {
				t.Errorf("crewlet.code.merge.recomputes observed %d merges, %d of them %s summing %v; want one "+
					"%s merge of %d rebuilds", merges, under, outcome, sum, outcome, rebuilt)
			}
			if !tc.merged {
				if err != nil || res.Merge == nil || res.Merge.Refusal != gitlet.MergeRefusalTargetMoving ||
					res.Merge.Recomputes != gitlet.MergeRecomputes || m.asked != gitlet.MergeRecomputes+1 {
					t.Fatalf("a merge chasing main answered %+v %v (%+v) after %d records, want target_moving "+
						"after %d recomputes", res, err, res.Merge, m.asked, gitlet.MergeRecomputes)
				}
				if ch := c.change(n); ch.State != gitlet.ChangeStateOpen {
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

// A MERGE IS BUILT AGAIN ONLY WHERE ITS ONE BUDGET AND ITS ATTEMPT HOLD A
// ROUND: its git is gitlet.MergeBuildDeadline across every round together — the
// one build the router's attempt holds — so a merge whose target moves under
// every build is answered target_moving, nothing more asked of the committer,
// once what is left of that budget is less than the git its last round ran:
// its merge-tree charged four seconds of a ten-second budget, it is built
// twice, and the six seconds left after the first hold a rebuild while the two
// after the second do not. And where what its caller's deadline leaves cannot
// hold another round's waits, its git and the upload and proposal its first
// landing was held to, it is built once. Never a round started to end unready
// during its build, which sends the walk's runner-up Gitlet node to merge it
// from scratch, or busy at its landing. The change stays open.
//
// Mutations: give every round the whole budget again → four rebuilds, five
// records asked, red; start a rebuild the budget cannot hold → its git runs
// out the budget on a second rebuild, red; start one the attempt cannot hold
// → five records asked, red.
func TestAMergeIsBuiltAgainOnlyWithinItsOneBudgetAndAttempt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// budget scales the merge's budget of git and its merge-tree's
		// charge, and deadline the caller's attempt; zero leaves each as it
		// is.
		budget, mergeTree time.Duration
		deadline          bool
		// rebuilds is how many times the merge is built again, and says
		// what its answer names as having stopped it.
		rebuilds int
		says     string
	}{
		{name: "its budget of git", budget: 10 * time.Second, mergeTree: 4 * time.Second, rebuilds: 1,
			says: "of the 10s of git a merge is given"},
		{name: "its attempt", deadline: true, says: "its caller left"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			m := &movingMain{moves: gitlet.MergeRecomputes + 1}
			c := newMergeCase(t, gitlettest.SoloOptions{Shape: gitlettest.Shape{WrapEstate: m.wrap}},
				gitlettest.Settings(), map[string]string{"README.md": "one\n"})
			m.move = func(i int) {
				c.p.Commit(c.work, map[string]string{fmt.Sprintf("main-%d.md", i): "moved\n"}, "move main")
				c.MustPush(c.p, c.work, "main")
			}
			if tc.budget > 0 {
				host.SetBuildBudget(c.server(), tc.budget)
				host.SlowGit(c.server(), tc.mergeTree, "merge-tree")
			}
			n := c.open(featureRef, map[string]string{"x.md": "x\n"}, "Chase main")
			head := c.change(n).Head
			w := merge(c.repo, pat(), n, head, gitlet.MergeMethodMerge)
			w.Override = "chasing a moving main"
			ctx := c.Ctx
			if tc.deadline {
				// ROOM FOR ONE LANDING AND NOT ONE MORE ROUND: what the deadline
				// rule holds the first landing to — its proposal and its
				// upload's allowance, which a bundle of a few hundred bytes
				// reaches whole (gitlet.UploadDeadline's first byte alone is
				// past it) — and four seconds for the round to reach it, short
				// of the eight seconds of the share's and three reads' waits a
				// second round needs on top of them.
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(c.Ctx, gitlet.MinProposeAttempt+gitlet.WriteUploadAllowance+
					4*time.Second)
				defer cancel()
			}
			res, err := c.server().Write(ctx, w)
			if err != nil || res.Merge == nil || res.Merge.Refusal != gitlet.MergeRefusalTargetMoving ||
				res.Merge.Recomputes != tc.rebuilds || !strings.Contains(res.Merge.Detail, tc.says) {
				t.Fatalf("a merge with no room to build again answered %+v %v (%+v), want target_moving after %d "+
					"rebuilds, saying %q", res, err, res.Merge, tc.rebuilds, tc.says)
			}
			m.mu.Lock()
			asked := m.asked
			m.mu.Unlock()
			if asked != tc.rebuilds+1 {
				t.Errorf("the merge asked %d records, want %d: one a build", asked, tc.rebuilds+1)
			}
			if ch := c.change(n); ch.State != gitlet.ChangeStateOpen {
				t.Errorf("change %d is %s, want open", n, ch.State)
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

func (e *expungingBuilds) Merge(ctx context.Context, p gitlet.MergeProposal) (gitlet.MergeResult, error) {
	e.mu.Lock()
	e.asked++
	refused := e.asked <= e.refusals
	e.mu.Unlock()
	if refused {
		return gitlet.MergeResult{MergeID: p.Merge.MergeID, Refusal: gitlet.MergeRefusalExpunging,
			Detail: gitlet.ExpungingBuiltDetail(gitlettest.RepoName, "merge", "its builder", p.DropEpoch,
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
		{"refused every time", gitlet.MergeRecomputes + 1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e := &expungingBuilds{refusals: tc.refusals}
			c := newMergeCase(t, gitlettest.SoloOptions{Shape: gitlettest.Shape{WrapEstate: e.wrap}},
				gitlettest.Settings(), map[string]string{"README.md": "one\n"})
			n := c.open(featureRef, map[string]string{"x.md": "x\n"}, "Outlive an expunge")
			w := merge(c.repo, pat(), n, c.change(n).Head, gitlet.MergeMethodMerge)
			w.Override = "merging through an expunge"
			res, err := c.write(w)
			if !tc.merged {
				if err != nil || res.Merge == nil || res.Merge.Refusal != gitlet.MergeRefusalExpunging ||
					!strings.Contains(res.Merge.Detail, "its builder builds it again") ||
					res.Merge.Recomputes != gitlet.MergeRecomputes || e.asked != gitlet.MergeRecomputes+1 {
					t.Fatalf("a merge every expunge overtook answered %+v %v (%+v) after %d records, want "+
						"expunging after %d recomputes", res, err, res.Merge, e.asked, gitlet.MergeRecomputes)
				}
				if ch := c.change(n); ch.State != gitlet.ChangeStateOpen {
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
	settings := gitlettest.Settings()
	settings.Protections[0].Gate = &gitlet.MergeGate{MergeBy: gitlet.MergeByWriters, Approvals: gitlet.ApprovalsNone,
		Checks: []string{"unit"}, Landing: gitlet.Landing{Depth: 4, Batch: 4}, DeleteSource: gitlet.DeleteNone}
	c := newMergeCase(t, gitlettest.SoloOptions{}, settings, map[string]string{"README.md": "one\n"})
	n := c.open(featureRef, map[string]string{"x.md": "x\n"}, "Queue me")
	c.meetGate(n, lead())
	head := c.change(n).Head
	// THE HEAD'S REQUIRED CHECK PASSED, for main: the gate an enqueue is
	// decided by holds a change's head to it.
	unit := gitlet.CheckKey{RepoID: c.repo, OID: head, Target: "refs/heads/main", Name: "unit", Attempt: 1}
	queueCheck(t, c.solo(), unit)
	endCheck(t, c.solo(), unit)
	before := len(c.objects())
	w := merge(c.repo, alice(), n, head, gitlet.MergeMethodSquash)
	res, err := c.write(w)
	if err != nil || res.Outcome != statelog.OutcomeApplied || res.Merge == nil || !res.Merge.Queued ||
		res.Merge.EntryID != gitlet.QueueEntryID(w.OpID) || res.Merge.Place != 1 || res.Commit != "" {
		t.Fatalf("a merge onto a queue target answered %+v %v (%+v), want enqueued first", res, err, res.Merge)
	}
	if after := len(c.objects()); after != before {
		t.Errorf("an enqueue stored %d objects: the queue builds its candidates itself", after-before)
	}
	if ch := c.change(n); ch.State != gitlet.ChangeStateOpen {
		t.Errorf("an enqueued change is %s", ch.State)
	}
	n2 := c.open("refs/heads/urgent", map[string]string{"y.md": "y\n"}, "Merge me now")
	over := merge(c.repo, pat(), n2, c.change(n2).Head, gitlet.MergeMethodMerge)
	over.Override = "the release cannot wait for the queue"
	res, err = c.write(over)
	c.requireMerged(n2, res, err)
	if ch := c.change(n2); ch.OverrideReason != over.Override || ch.MergedBy != gitlettest.FixturePat {
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
	ok := merge(repo, pat(), 3, strings.Repeat("a", 40), gitlet.MergeMethodSquash)
	if err := ok.Validate(); err != nil {
		t.Fatalf("a whole merge_change is refused: %v", err)
	}
	long := strings.Repeat("x", gitlet.MaxOverrideReasonBytes+1)
	for _, tc := range []struct {
		name  string
		edit  func(w *gitlet.EngineWrite)
		field string
	}{
		{"no change", func(w *gitlet.EngineWrite) { w.Change = 0 }, "change"},
		{"no method", func(w *gitlet.EngineWrite) { w.Method = "" }, "method"},
		{"a method nobody knows", func(w *gitlet.EngineWrite) { w.Method = "octopus" }, "method"},
		{"no head", func(w *gitlet.EngineWrite) { w.ExpectedHead = "" }, "expected_head"},
		{"a message", func(w *gitlet.EngineWrite) { w.Message = "mine" }, "message"},
		{"files", func(w *gitlet.EngineWrite) { w.Files = []gitlet.FileOp{text("a", "a")} }, "files"},
		{"a long override", func(w *gitlet.EngineWrite) { w.Override = long }, "override"},
	} {
		w := ok
		tc.edit(&w)
		if err := w.Validate(); err == nil || !strings.Contains(err.Error(), tc.field) {
			t.Errorf("%s: %v, want a refusal naming %s", tc.name, err, tc.field)
		}
	}
	other := files(repo, pat(), "refs/heads/x", "", "", text("a", "a"))
	other.Method = gitlet.MergeMethodMerge
	if err := other.Validate(); err == nil || !strings.Contains(err.Error(), "method") {
		t.Errorf("a commit_files carrying a merge's method: %v, want refused naming method", err)
	}
}
