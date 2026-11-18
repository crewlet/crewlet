package host_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitletfault"
	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitlet/host"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE ENGINE WRITE'S OWN CASES, beside the suite's (internal/gitlet/gitlettest's
// F32–F38): what Gitlet on one Gitlet node does with a write it is asked.

// linear is the freshness a write is judged at where a case does not say.
var linear = statelog.Freshness{Level: statelog.ReadLinearizable}

// alice is the fixture chart's full_access seat, as her write names her: with
// the run that made it.
func alice() gitlet.Principal {
	return gitlet.Principal{Kind: gitlet.PrincipalAgent, Seat: gitlettest.FixtureAlice,
		Run: gitlet.RunProvenance{TurnID: uuid.Must(uuid.NewV7()).String(), WorkKey: "ENG-7", Model: "fixture"}}
}

// pat is the fixture chart's person, through the token the fleet minted her.
func pat() gitlet.Principal {
	return gitlet.Principal{Kind: gitlet.PrincipalOperator, OperatorID: iam.TokenLogin(gitlettest.FixturePat),
		Seat: gitlettest.FixturePat}
}

// opNow is an operation minted now; opAt one minted at at.
func opNow() string            { return statelog.NewOpID(time.Now(), "") }
func opAt(at time.Time) string { return statelog.NewOpID(at, "") }

// text is a put of content at path.
func text(path, content string) gitlet.FileOp {
	return gitlet.FileOp{Op: gitlet.FileOpPut, Path: path, Content: &content}
}

// files is a commit_files by who onto ref, made for nobody but who: onto
// head, or creating ref from base where head is "".
func files(repo string, who gitlet.Principal, ref, head, base string, ops ...gitlet.FileOp) gitlet.EngineWrite {
	return gitlet.EngineWrite{Kind: gitlet.WriteCommitFiles, RepoID: repo, Principal: who,
		WrittenFor: types.ForNobody(), OpID: opNow(), Fresh: linear, Ref: ref, ExpectedHead: head, Base: base,
		Message: "a change made through a tool", Files: ops}
}

// writeCase is a solo fleet with RepoName's main seeded by ceo.
type writeCase struct {
	*gitlettest.Case
	p    *gitlettest.Person
	work string
	head string
	repo string
}

// newWriteCase is a write case over o, main holding seed.
func newWriteCase(t *testing.T, o gitlettest.SoloOptions, seed map[string]string) *writeCase {
	t.Helper()
	return seedWriteCase(newCase(t, o, gitlettest.Settings()), seed)
}

// seedWriteCase is the write case of c, main holding seed. ceo's push of it is
// c's first request, so whatever a case hands its host before this governs
// every request.
func seedWriteCase(c *gitlettest.Case, seed map[string]string) *writeCase {
	c.T.Helper()
	p := gitlettest.NewPerson(c.T, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, seed, "seed")
	c.MustPush(p, work, "main")
	return &writeCase{Case: c, p: p, work: work, head: head, repo: c.Rows().Repo.ID}
}

// server is Gitlet on the case's one Gitlet node.
func (c *writeCase) server() *host.Server { return c.Fleet.Nodes()[0].Host() }

// write asks w of Gitlet on the Gitlet node.
func (c *writeCase) write(w gitlet.EngineWrite) (gitlet.EngineWriteResult, error) {
	return c.server().Write(c.Ctx, w)
}

// land asks w and fails the case unless it applied — with its commit, every
// kind but a delete, which makes none.
func (c *writeCase) land(w gitlet.EngineWrite) gitlet.EngineWriteResult {
	c.T.Helper()
	res, err := c.write(w)
	deletes := w.Kind == gitlet.WriteDeleteBranch || w.Kind == gitlet.WriteDeleteTag
	if err != nil || res.Outcome != statelog.OutcomeApplied || (res.Commit == "") != deletes {
		c.T.Fatalf("the write (%s %s) did not apply: %+v %v", w.Kind, w.Ref, res, err)
	}
	return res
}

// clone is a fresh clone of the repository by ceo: every branch fetched.
func (c *writeCase) clone() string {
	c.T.Helper()
	return c.p.Clone(c.URL(0))
}

// show is `git log -1 --format=<format> <rev>` in dir.
func (c *writeCase) show(dir, format, rev string) string {
	c.T.Helper()
	return strings.TrimSpace(c.p.Git().Run(c.T, dir, "log", "-1", "--format="+format, rev))
}

// objects is every object name the case's store holds.
func (c *writeCase) objects() []string {
	c.T.Helper()
	var names []string
	if err := c.Fleet.Objects().List(c.Ctx, func(i objstore.Info) error {
		names = append(names, i.Name)
		return nil
	}); err != nil {
		c.T.Fatalf("list the object store: %v", err)
	}
	return names
}

// AN ENGINE COMMIT IS THE ACTING PRINCIPAL'S (ADR-0092): its author is the
// signer's seat at the address it names; its committer the engine,
// `Crewlet <engine@seats.invalid>` with code.identity_domain unset; its author
// date the operation's own second, its committer date too while the parent is
// older. It is a pure function of the write and its parent: built in two
// Gitlet nodes' scratches, it is one commit, byte for byte.
//
// Mutations: name the committer from the author (alice's line), red; read the
// author date from the node's clock (the op was minted an hour ago), red.
func TestAnEngineCommitIsTheActingPrincipals(t *testing.T) {
	t.Parallel()
	minted := time.Now().UTC().Add(-time.Hour).Truncate(time.Second).Add(300 * time.Millisecond)
	op := opAt(minted)
	who := alice()
	var commits []string
	for range 2 {
		c := newWriteCase(t, gitlettest.SoloOptions{}, map[string]string{"README.md": "one\n"})
		w := files(c.repo, who, "refs/heads/seats/alice/sign", "", c.head, text("signed.md", "signed\n"))
		w.OpID = op
		res := c.land(w)
		commits = append(commits, res.Commit)
		clone := c.clone()
		got := c.show(clone, "%an <%ae>|%cn <%ce>|%at|%ct", res.Commit)
		second := strconv.FormatInt(minted.Unix(), 10)
		want := fmt.Sprintf("%s <%s>|%s <%s>|%s|%s", gitlettest.FixtureAlice,
			gitlettest.FixtureAuthorEmail(gitlettest.FixtureAlice), gitlettest.FixtureCommitterName,
			gitlettest.FixtureCommitterEmail, second, second)
		if got != want {
			t.Errorf("the commit is %q, want %q", got, want)
		}
	}
	if commits[0] != commits[1] {
		t.Errorf("one write built in two scratches is two commits, %s and %s", commits[0], commits[1])
	}
}

// A COMMIT IS NEVER DATED BEFORE ITS PARENT: over a parent committed an hour
// after the operation was minted (a person's clock ahead), the author date is
// still the operation's second and the committer date the parent's, so no walk
// by date meets a child before its parent.
//
// Mutations: clamp the author date too (it reads the parent's), red; leave the
// committer date at the operation's (an hour before the parent), red.
func TestACommitIsNeverDatedBeforeItsParent(t *testing.T) {
	t.Parallel()
	c := newWriteCase(t, gitlettest.SoloOptions{}, map[string]string{"README.md": "one\n"})
	minted := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	ahead := minted.Add(time.Hour)
	stamp := fmt.Sprintf("@%d +0000", ahead.Unix())
	clock := c.p.Git().Env("GIT_AUTHOR_DATE="+stamp, "GIT_COMMITTER_DATE="+stamp)
	parent := clock.Commit(t, c.work, map[string]string{"later.md": "later\n"}, "dated ahead")
	c.MustPush(c.p, c.work, "main")
	w := files(c.repo, alice(), "refs/heads/seats/alice/dates", "", parent, text("child.md", "child\n"))
	w.OpID = opAt(minted)
	res := c.land(w)
	got := c.show(c.clone(), "%at|%ct", res.Commit)
	if want := fmt.Sprintf("%d|%d", minted.Unix(), ahead.Unix()); got != want {
		t.Errorf("the commit's author and committer dates are %s, want %s: the operation's second, and its "+
			"parent's", got, want)
	}
	author, committer := host.CommitDates(minted.Add(1500*time.Millisecond), ahead.Add(-2*time.Hour))
	if !author.Equal(minted.Add(time.Second)) || !committer.Equal(author) {
		t.Errorf("over an older parent the dates are %v and %v, want the operation's second twice", author,
			committer)
	}
}

// A RETRIED ENGINE WRITE IS ONE PUSH (ADR-0092's first Enforced-by): one write
// asked twice at once on one Gitlet node is built once, the second joining the
// first (which holds the write's one scratch) for its result; asked again once
// landed, it is answered from its push's row, nothing built. Every answer
// names WritePushID of its operation, a canonical UUIDv7.
//
// Mutations: a named derivation for the push id (the scratch refuses its
// name), red; drop the in-flight join (the second's own scratch of the same
// push id is refused), red.
func TestARetriedEngineWriteIsOnePush(t *testing.T) {
	t.Parallel()
	e := &recording{}
	c := newWriteCase(t, gitlettest.SoloOptions{Shape: gitlettest.Shape{WrapEstate: e.wrap}},
		map[string]string{"README.md": "one\n"})
	s := c.server()
	cmds := host.RecordGit(s)
	seeded := len(e.seen("propose"))
	w := files(c.repo, alice(), "refs/heads/seats/alice/once", "", c.head, text("once.md", "once\n"))
	pushID := gitlet.WritePushID(w.OpID)
	if id, err := uuid.Parse(pushID); err != nil || id.String() != pushID || id.Version() != 7 ||
		strings.Contains(pushID, ".") {
		t.Fatalf("the push id %q is no canonical UUIDv7: %v", pushID, err)
	}

	held, reached := make(chan struct{}), make(chan struct{})
	c.Fleet.Faults().Arm(gitletfault.HostAfterUpload, gitletfault.Action{Once: true, Block: held, Reached: reached})
	type answer struct {
		res gitlet.EngineWriteResult
		err error
	}
	first, second := make(chan answer, 1), make(chan answer, 1)
	go func() {
		res, err := c.write(w)
		first <- answer{res, err}
	}()
	reachedFirst(t, reached, first)
	go func() {
		res, err := c.write(w)
		second <- answer{res, err}
	}()
	joined := false
	//fixedwait:measure a poll's own deadline: a second ask that never joined fails the case after it
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		if host.WriteJoiners(s, pushID) == 1 {
			joined = true
			break
		}
		time.Sleep(5 * time.Millisecond) //fixedwait:liveness
	}
	close(held)
	a, b := <-first, <-second
	if !joined {
		t.Fatalf("the second ask never joined the first: %+v %v", b.res, b.err)
	}
	for i, x := range []answer{a, b} {
		if x.err != nil || x.res.Outcome != statelog.OutcomeApplied || x.res.PushID != pushID {
			t.Fatalf("ask %d: %+v %v, want applied under push %s", i+1, x.res, x.err, pushID)
		}
	}
	if b.res.Commit != a.res.Commit {
		t.Errorf("the joined ask answers %s, the first %s", b.res.Commit, a.res.Commit)
	}
	builds := func() int {
		n := 0
		for _, cmd := range cmds() {
			if cmd.Args[0] == "commit-tree" {
				n++
			}
		}
		return n
	}
	if n := builds(); n != 1 {
		t.Errorf("two asks at once built %d commits, want one", n)
	}
	if n := len(e.seen("propose")) - seeded; n != 1 {
		t.Errorf("two asks at once proposed %d times, want once", n)
	}

	again, err := c.write(w)
	if err != nil || again.Outcome != statelog.OutcomeApplied || again.Commit != a.res.Commit ||
		again.PushID != pushID {
		t.Errorf("the write asked again after it landed: %+v %v, want applied with %s", again, err, a.res.Commit)
	}
	if n := builds(); n != 1 {
		t.Errorf("the write asked after it landed built again: %d commits", n)
	}
	if n := len(e.seen("propose")) - seeded; n != 1 {
		t.Errorf("the write asked after it landed proposed again: %d proposals", n)
	}
}

// A SEAT'S WRITE ON A NODE BEHIND THE CONFIGURATION IS UNREADY, behind_config,
// nothing synced, built or proposed, so the walk asks a current Gitlet node; a
// person's write, which reads no chart, lands there.
//
// Mutation: drop the check: the seat's write lands, red.
func TestAWriteBehindTheConfigurationIsUnready(t *testing.T) {
	t.Parallel()
	e := &recording{}
	c := newWriteCase(t, gitlettest.SoloOptions{Current: func() bool { return false },
		Shape: gitlettest.Shape{WrapEstate: e.wrap}}, map[string]string{"README.md": "one\n"})
	states, proposals := len(e.seen("state")), len(e.seen("propose"))
	_, err := c.write(files(c.repo, alice(), "refs/heads/seats/alice/x", "", c.head, text("a.md", "a\n")))
	var unready *gitlet.ErrGitUnready
	if !errors.As(err, &unready) || unready.Reason != gitlet.UnreadyBehindConfig {
		t.Fatalf("a seat's write behind the configuration = %v, want ErrGitUnready{behind_config}", err)
	}
	if n := len(e.seen("state")); n != states {
		t.Errorf("the refused write synced %d times", n-states)
	}
	if n := len(e.seen("propose")); n != proposals {
		t.Errorf("the refused write proposed %d times", n-proposals)
	}
	c.land(files(c.repo, pat(), "refs/heads/main", c.head, "", text("p.md", "p\n")))
}

// A WRITE'S REFUSAL IS JUDGED AT THE LOG'S HEAD before it is answered: over a
// copy still holding a protection a revision has since removed, the first
// judgement refuses; the copy, synced through the barrier, judges again and
// lands it.
//
// Mutation: answer the first refusal (protected_direct), red.
func TestAWriteRefusalIsJudgedAtLinearizable(t *testing.T) {
	t.Parallel()
	e := &recording{}
	c := newWriteCase(t, gitlettest.SoloOptions{Shape: gitlettest.Shape{WrapEstate: e.wrap}},
		map[string]string{"README.md": "one\n"})
	open := gitlettest.Settings()
	open.Protections = nil
	c.Fleet.Declare(t, gitlet.Declared{Name: gitlettest.RepoName, Settings: open})
	before := e.linearizable()
	w := files(c.repo, alice(), "refs/heads/main", c.head, "", text("a.md", "a\n"))
	w.Fresh = statelog.Freshness{Level: statelog.ReadStale}
	res := c.land(w)
	if e.linearizable() == before {
		t.Fatalf("the write landed with no read through the barrier: the copy was never behind the protection's "+
			"removal, so this case judged nothing (%+v)", res)
	}
}

// A SEAT'S WRITE REFUSAL IS JUDGED AGAIN ONLY WHILE ITS RE-SYNCS LAST: a seat
// writing onto a head the branch is not at, over a stale copy, is judged again
// at the log's head (one barrier a write) until its turn's gitlet.ResyncBurst
// on the node are spent; then the refusal is answered from its session,
// through no barrier, saying it was not judged again and when its re-syncs
// return. A person's write, charged to nobody, is still judged again.
//
// Mutation: re-judge without the bucket (resync answering nil): the write past
// the burst reads through the barrier, red.
func TestASeatsWriteRefusalIsJudgedAgainWhileItsResyncsLast(t *testing.T) {
	t.Parallel()
	e := &recording{}
	c := newWriteCase(t, gitlettest.SoloOptions{Shape: gitlettest.Shape{WrapEstate: e.wrap}},
		map[string]string{"README.md": "one\n"})
	open := gitlettest.Settings()
	open.Protections = nil
	c.Fleet.Declare(t, gitlet.Declared{Name: gitlettest.RepoName, Settings: open})
	stale := randomOID()
	write := func(who gitlet.Principal) (gitlet.Command, int) {
		t.Helper()
		before := e.linearizable()
		w := files(c.repo, who, "refs/heads/main", stale, "", text("a.md", "a\n"))
		w.Fresh = statelog.Freshness{Level: statelog.ReadStale}
		res, err := c.write(w)
		if err != nil {
			t.Fatalf("a write onto %s: %v", stale, err)
		}
		return res.Command, e.linearizable() - before
	}
	seat := alice()
	for i := range gitlet.ResyncBurst {
		if cmd, n := write(seat); n == 0 || cmd.Reason != gitlet.RefusalStale {
			t.Fatalf("write %d of the turn's %d was refused %s through %d barriers, want stale through one", i+1,
				gitlet.ResyncBurst, cmd.Reason, n)
		}
	}
	cmd, n := write(seat)
	if n != 0 || cmd.Reason != gitlet.RefusalStale ||
		!strings.Contains(cmd.Detail, "(not re-judged at linearizable: this turn's re-syncs are spent until ") {
		t.Errorf("past its re-syncs the seat's write was refused %s %q through %d barriers, want stale from its "+
			"session, through none, saying so", cmd.Reason, cmd.Detail, n)
	}
	if cmd, n := write(pat()); n == 0 || cmd.Reason != gitlet.RefusalStale {
		t.Errorf("a person's write was refused %s through %d barriers, want stale through one", cmd.Reason, n)
	}
}

// A SEAT'S WRITE IS HELD TO WHOEVER ITS TURN WORKS FOR, on the Gitlet node that
// builds it (gitlet.EngineWrite.Ceiling, M3-decisions U40 (a)): alice moves
// main for the unit's lead, who writes the repository too, but not for bob,
// who only reads it, nor for nobody (a stranger, or a person who may not act):
// each refused not_permitted saying whom the work is for, nothing built. Under
// every ceiling she still writes her own namespace.
//
// Mutations: build with the seat's own grant alone (no ceiling in rules): bob's
// and nobody's main land, red; OwnWork answering false: the namespace write is
// refused, red.
func TestASeatsWriteIsHeldToWhoeverItsTurnWorksFor(t *testing.T) {
	t.Parallel()
	c := newWriteCase(t, gitlettest.SoloOptions{}, map[string]string{"README.md": "one\n"})
	open := gitlettest.Settings()
	open.Protections = nil
	c.Fleet.Declare(t, gitlet.Declared{Name: gitlettest.RepoName, Settings: open})
	for _, tc := range []struct {
		name    string
		ceiling *gitlet.WriteCeiling
		says    string
	}{
		{"a colleague who only reads", &gitlet.WriteCeiling{Kind: gitlet.WriteCeilingSeat, Seat: gitlettest.FixtureBob},
			"this work is for the seat whose ask started its turn"},
		{"nobody", &gitlet.WriteCeiling{Kind: gitlet.WriteCeilingNobody}, "may change nothing of this company's code"},
	} {
		w := files(c.repo, alice(), "refs/heads/main", c.head, "", text(tc.name+".md", "x\n"))
		w.Ceiling = tc.ceiling
		res, err := c.write(w)
		if err != nil {
			t.Fatalf("alice's write for %s: %v", tc.name, err)
		}
		if res.Command.Reason != gitlet.RefusalNotPermitted || !strings.Contains(res.Command.Detail, tc.says) ||
			res.Commit != "" {
			t.Errorf("alice's write of main for %s: %+v, want not_permitted saying %q, nothing built", tc.name,
				res, tc.says)
		}
		mine := files(c.repo, alice(), "refs/heads/seats/alice/"+strings.ReplaceAll(tc.name, " ", "-"), "",
			c.head, text("mine.md", "mine\n"))
		mine.Ceiling = tc.ceiling
		c.land(mine)
	}
	w := files(c.repo, alice(), "refs/heads/main", c.head, "", text("lead.md", "x\n"))
	w.Ceiling = &gitlet.WriteCeiling{Kind: gitlet.WriteCeilingSeat, Seat: gitlettest.FixtureLead}
	c.land(w)
}

// A RETRIED WRITE THAT LANDED IS ANSWERED FROM ITS ROW: its branch moved on by
// a later push, the write asked again asks its own push's outcome before
// calling the head stale, and answers applied with its first commit, building
// nothing.
//
// Mutation: answer stale before asking the outcome: stale, red.
func TestARetriedWriteThatLandedIsAnsweredFromItsRow(t *testing.T) {
	t.Parallel()
	c := newWriteCase(t, gitlettest.SoloOptions{}, map[string]string{"README.md": "one\n"})
	cmds := host.RecordGit(c.server())
	w := files(c.repo, alice(), "refs/heads/seats/alice/r", "", c.head, text("r.md", "r\n"))
	first := c.land(w)
	git := c.p.Git()
	git.Run(t, c.work, "fetch", "-q", c.URL(0), "seats/alice/r")
	git.Run(t, c.work, "checkout", "-q", "-B", "r", "FETCH_HEAD")
	c.p.Commit(c.work, map[string]string{"later.md": "later\n"}, "a later push")
	c.MustPush(c.p, c.work, "r:refs/heads/seats/alice/r")
	built := len(cmds())
	again, err := c.write(w)
	if err != nil || again.Outcome != statelog.OutcomeApplied || again.Commit != first.Commit {
		t.Fatalf("the write asked again over a branch moved on: %+v %v, want applied with %s", again, err,
			first.Commit)
	}
	for _, cmd := range cmds()[built:] {
		if slices.Contains([]string{"hash-object", "mktree", "commit-tree", "pack-objects", "index-pack"},
			cmd.Args[0]) {
			t.Errorf("the write answered from its row ran %s", cmd.Args[0])
		}
	}
}

// A WRITE WHOSE CALLER LEFT NO TIME FOR ITS PROPOSAL UPLOADS NOTHING: a
// deadline that cannot hold the bundle's upload and then the proposal is
// answered busy before a byte is stored, so a slow bucket costs a refusal,
// never a PUT nobody proposes.
//
// Mutation: drop the rule: the bundle is stored, red.
func TestAWriteWithNoTimeForItsProposalUploadsNothing(t *testing.T) {
	t.Parallel()
	e := &recording{}
	c := newWriteCase(t, gitlettest.SoloOptions{Shape: gitlettest.Shape{WrapEstate: e.wrap}},
		map[string]string{"README.md": "one\n"})
	before, proposals := c.objects(), len(e.seen("propose"))
	ctx, cancel := context.WithTimeout(c.Ctx, gitlet.MinProposeAttempt/2)
	defer cancel()
	res, err := c.server().Write(ctx, files(c.repo, alice(), "refs/heads/seats/alice/late", "", c.head,
		text("late.md", "late\n")))
	if err != nil || res.Command.Reason != gitlet.RefusalBusy || !strings.Contains(res.Command.Detail, "upload") {
		t.Fatalf("a write with no time for its proposal: %+v %v, want busy naming its upload", res, err)
	}
	if after := c.objects(); !slices.Equal(after, before) {
		t.Errorf("the object store went from %v to %v", before, after)
	}
	if n := len(e.seen("propose")); n != proposals {
		t.Errorf("the write proposed %d times", n-proposals)
	}
}

// A CREATE AND A DELETE OF A BRANCH BUILD NOTHING: no git writer runs, no pack
// is stored, no unit of the seat share is taken; a create's target is checked
// present (receive-pack's connectivity check); both land as pushes.
//
// Mutation: take a unit for them: one is held while the create lands, red.
func TestCreateAndDeleteBranchBuildNothing(t *testing.T) {
	t.Parallel()
	c := newWriteCase(t, gitlettest.SoloOptions{}, map[string]string{"README.md": "one\n"})
	cmds := host.RecordGit(c.server())
	share := c.Fleet.Nodes()[0].Share()
	held, reached := make(chan struct{}), make(chan struct{})
	c.Fleet.Faults().Arm(gitletfault.HostAfterPropose, gitletfault.Action{Once: true, Block: held, Reached: reached})
	create := gitlet.EngineWrite{Kind: gitlet.WriteCreateBranch, RepoID: c.repo, Principal: alice(),
		WrittenFor: types.ForNobody(), OpID: opNow(), Fresh: linear, Ref: "refs/heads/seats/alice/b", Commit: c.head}
	done := make(chan error, 1)
	var created gitlet.EngineWriteResult
	go func() {
		var err error
		created, err = c.write(create)
		done <- err
	}()
	reachedFirst(t, reached, done)
	if n := share.Held(); n != 0 {
		t.Errorf("a create holds %d units of the seat share while it lands", n)
	}
	close(held)
	if err := <-done; err != nil || created.Outcome != statelog.OutcomeApplied || created.Commit != c.head {
		t.Fatalf("the create: %+v %v", created, err)
	}
	deleted := c.land(gitlet.EngineWrite{Kind: gitlet.WriteDeleteBranch, RepoID: c.repo, Principal: alice(),
		WrittenFor: types.ForNobody(), OpID: opNow(), Fresh: linear, Ref: "refs/heads/seats/alice/b",
		ExpectedHead: c.head})
	if deleted.Command.New != gitlet.ZeroOID {
		t.Errorf("the delete's command is %+v, want its new the zero id", deleted.Command)
	}
	if _, ok := c.Rows().Refs["refs/heads/seats/alice/b"]; ok {
		t.Error("the deleted branch is still a row")
	}
	for _, cmd := range cmds() {
		if !slices.Contains([]string{"for-each-ref", "cat-file"}, cmd.Args[0]) {
			t.Errorf("a create or a delete ran %s", cmd.Args)
		}
	}
	_, err := c.write(gitlet.EngineWrite{Kind: gitlet.WriteCreateBranch, RepoID: c.repo, Principal: alice(),
		WrittenFor: types.ForNobody(), OpID: opNow(), Fresh: linear, Ref: "refs/heads/seats/alice/c",
		Commit: strings.Repeat("ab", 20)})
	if !errors.Is(err, gitlet.ErrObjectNotFound) {
		t.Errorf("a create at a commit the repository lacks = %v, want ErrObjectNotFound", err)
	}
}

// A TAG IS WRITTEN AS A PUSHED TAG IS: lightweight, or annotated by a message with the write's
// author as tagger; never moved, deleted only at the value read, protected where a rule says.
// Mutations: build no object for a message, tag as the committer, or read it as a commit: red.
func TestATagIsWrittenAsAPushedTagIs(t *testing.T) {
	t.Parallel()
	settings := gitlettest.Settings()
	settings.Protections = append(settings.Protections, gitlet.Protection{PushRule: gitlet.PushRule{
		Pattern: "refs/tags/release/*", DirectPush: gitlet.DirectPersons}})
	c := seedWriteCase(newCase(t, gitlettest.SoloOptions{}, settings), map[string]string{"README.md": "one\n"})
	tag := func(name, message string) gitlet.EngineWrite {
		return gitlet.EngineWrite{Kind: gitlet.WriteCreateTag, RepoID: c.repo, Principal: pat(),
			WrittenFor: types.ForNobody(), OpID: opNow(), Fresh: linear, Ref: "refs/tags/" + name, Commit: c.head,
			Message: message}
	}
	drop := func(name, value string) gitlet.EngineWrite {
		return gitlet.EngineWrite{Kind: gitlet.WriteDeleteTag, RepoID: c.repo, Principal: pat(),
			WrittenFor: types.ForNobody(), OpID: opNow(), Fresh: linear, Ref: "refs/tags/" + name, ExpectedHead: value}
	}
	if light := c.land(tag("v1", "")); light.Commit != c.head || c.Rows().Refs["refs/tags/v1"] != c.head {
		t.Fatalf("the lightweight tag answered %+v, its row %q, want %s", light, c.Rows().Refs["refs/tags/v1"], c.head)
	}
	annotated := tag("v2", "The second release")
	made := c.land(annotated)
	if again, err := c.write(annotated); err != nil || again.Commit != made.Commit {
		t.Errorf("the same annotated tag asked again answered %+v %v, want %s", again, err, made.Commit)
	}
	authored := c.land(files(c.repo, pat(), "refs/heads/pat-side", "", c.head, text("side.md", "side\n")))
	clone := c.clone()
	got := strings.TrimSpace(c.p.Git().Run(c.T, clone, "for-each-ref",
		"--format=%(objectname) %(objecttype) %(*objectname)|%(taggername) %(taggeremail)|%(contents)",
		"refs/tags/v2"))
	want := fmt.Sprintf("%s tag %s|%s|The second release", made.Commit, c.head,
		c.show(clone, "%an <%ae>", authored.Commit))
	if got != want {
		t.Errorf("the annotated tag reads %q, want %q", got, want)
	}
	if res, err := c.write(tag("v1", "")); err != nil || res.Command.Reason != gitlet.RefusalStale ||
		!strings.Contains(res.Command.Detail, "a tag never moves") {
		t.Errorf("creating v1 again answered %+v %v, want stale: a tag never moves", res, err)
	}
	if res, err := c.write(drop("v2", c.head)); err != nil || res.Command.Reason != gitlet.RefusalStale {
		t.Errorf("deleting v2 at its commit, not its tag object, answered %+v %v, want stale", res, err)
	}
	if deleted := c.land(drop("v2", made.Commit)); deleted.Command.New != gitlet.ZeroOID {
		t.Errorf("the delete's command is %+v, want its new the zero id", deleted.Command)
	}
	if _, ok := c.Rows().Refs["refs/tags/v2"]; ok {
		t.Error("the deleted tag is still a row")
	}
	c.land(tag("release/1", ""))
	if res, err := c.write(drop("release/1", c.head)); err != nil ||
		res.Command.Reason != gitlet.RefusalProtectedDelete {
		t.Errorf("deleting a protected tag answered %+v %v, want protected_delete", res, err)
	}
}

// NO WRITE DELETES THE DEFAULT BRANCH: code_delete_branch, a person's from the
// dashboard included, is judged by the push decide's own rule
// (gitlet.DefaultBranchRefusal) and refused default_branch with its sentence,
// the branch still a row, even where no protection rule names the branch.
//
// Mutation: drop the rule from the write's judge: the deletion lands, red.
func TestNoWriteDeletesTheDefaultBranch(t *testing.T) {
	t.Parallel()
	settings := gitlettest.Settings()
	settings.Protections = nil
	c := seedWriteCase(newCase(t, gitlettest.SoloOptions{}, settings), map[string]string{"README.md": "one\n"})
	res, err := c.write(gitlet.EngineWrite{Kind: gitlet.WriteDeleteBranch, RepoID: c.repo, Principal: pat(),
		WrittenFor: types.ForNobody(), OpID: opNow(), Fresh: linear, Ref: "refs/heads/main", ExpectedHead: c.head})
	if err != nil || res.Command.Result != gitlet.ResultNG || res.Command.Reason != gitlet.RefusalDefaultBranch ||
		!strings.Contains(res.Command.Detail, "make another branch the default first") {
		t.Fatalf("deleting the default branch answered %+v %v, want ng default_branch", res, err)
	}
	if got := c.Rows().Refs["refs/heads/main"]; got != c.head {
		t.Errorf("main is at %q after its deletion was refused, want %s", got, c.head)
	}
}

// A WRITE'S BUILD HOLDS ONE UNIT OF THE SEAT SHARE, AND ITS LANDING NONE: the
// unit is held while its git runs and given back before the upload, the
// proposal and the wait for its row, none of which is git.
//
// Mutation: give the unit back after the landing: it is held at the proposal,
// red.
func TestAWritesBuildHoldsTheShareAndItsLandingNone(t *testing.T) {
	t.Parallel()
	c := newWriteCase(t, gitlettest.SoloOptions{}, map[string]string{"README.md": "one\n"})
	share := c.Fleet.Nodes()[0].Share()
	building, release := host.HoldGit(c.server(), "commit-tree")
	held, reached := make(chan struct{}), make(chan struct{})
	c.Fleet.Faults().Arm(gitletfault.HostAfterPropose, gitletfault.Action{Once: true, Block: held, Reached: reached})
	done := make(chan error, 1)
	go func() {
		_, err := c.write(files(c.repo, alice(), "refs/heads/seats/alice/s", "", c.head, text("s.md", "s\n")))
		done <- err
	}()
	reachedFirst(t, building, done)
	if n := share.Held(); n != 1 {
		t.Errorf("the build holds %d units of the seat share, want one", n)
	}
	release()
	reachedFirst(t, reached, done)
	if n := share.Held(); n != 0 {
		t.Errorf("the landing holds %d units of the seat share, want none", n)
	}
	close(held)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// A WRITE READS WHAT ITS PATHS ARE IN ITS BUILD: a commit_files onto the head
// it read, naming no object by id, asks git nothing before its build but where
// its branch is; its first cat-file, of every named path and each directory
// above one as `<head>:<path>`, runs holding the build's share unit and
// charged to the build's git budget. git walks each path from the root, a cost
// growing with the square of depth, which no check outside the share may run up.
//
// Mutation: read the paths before the build takes its unit: the read runs
// with none held, red.
func TestAWriteReadsItsPathsInItsBuild(t *testing.T) {
	t.Parallel()
	c := newWriteCase(t, gitlettest.SoloOptions{}, map[string]string{"docs/a/b.md": "one\n"})
	share := c.Fleet.Nodes()[0].Share()
	inputs := host.RecordGitInputs(c.server())
	reading, release := host.HoldGit(c.server(), "cat-file")
	done := make(chan error, 1)
	go func() {
		_, err := c.write(files(c.repo, pat(), "refs/heads/main", c.head, "", text("docs/a/c.md", "c\n")))
		done <- err
	}()
	reachedFirst(t, reading, done)
	if n := share.Held(); n != 1 {
		t.Errorf("the read of what the write's paths are holds %d units of the seat share, want the build's one", n)
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	var asked []string
	for _, in := range inputs() {
		if in.Args[0] == "cat-file" {
			asked = append(asked, strings.Join(in.Args, " ")+"\x00"+string(in.Stdin))
		}
	}
	want := "cat-file --batch-check=%(objectname) %(objecttype) %(objectsize) %(objectmode) -Z\x00" +
		c.head + ":docs\x00" + c.head + ":docs/a\x00" + c.head + ":docs/a/c.md\x00"
	if len(asked) == 0 || asked[0] != want {
		t.Errorf("the write's first cat-file asked %q, want what its paths are at its head, %q", asked, want)
	}
}

// A WRITE PAST THE DEPTH BOUND RUNS NO GIT, and one at it builds a level at a
// time: a commit_files putting or moving an entry one component past
// gitlet.MaxTreePathDepth is refused ErrInvalid naming the bound before any
// git runs; a put at the bound lands with one mktree a level, no more. The
// bound holds down every build cost that grows with depth: the directories
// rewritten, the mktrees, and the read of each named path, walked from the root.
//
// Mutation: drop the depth bound: the deeper put runs git, red.
func TestAWritePastTheDepthBoundRunsNoGit(t *testing.T) {
	t.Parallel()
	c := newWriteCase(t, gitlettest.SoloOptions{}, map[string]string{"README.md": "one\n"})
	cmds := host.RecordGit(c.server())
	deep := strings.TrimSuffix(strings.Repeat("d/", gitlet.MaxTreePathDepth), "/")
	for _, op := range []gitlet.FileOp{
		text(deep+"/x.md", "x\n"),
		{Op: gitlet.FileOpMove, Path: "README.md", To: deep + "/x.md"},
	} {
		res, err := c.write(files(c.repo, pat(), "refs/heads/main", c.head, "", op))
		if !errors.Is(err, gitlet.ErrInvalid) || !strings.Contains(err.Error(), "gitlet.MaxTreePathDepth") {
			t.Errorf("a %s %d components deep: %+v %v, want ErrInvalid naming gitlet.MaxTreePathDepth", op.Op,
				gitlet.MaxTreePathDepth+1, res, err)
		}
		if ran := cmds(); len(ran) != 0 {
			t.Fatalf("a %s past the depth bound ran %d git processes, the first %s", op.Op, len(ran), ran[0].Args)
		}
	}
	c.land(files(c.repo, pat(), "refs/heads/main", c.head, "", text(deep, "at the bound\n")))
	mktrees := 0
	for _, cmd := range cmds() {
		if cmd.Args[0] == "mktree" {
			mktrees++
		}
	}
	if mktrees != gitlet.MaxTreePathDepth {
		t.Errorf("a put %d components deep ran %d mktree processes, want one a level, %d", gitlet.MaxTreePathDepth,
			mktrees, gitlet.MaxTreePathDepth)
	}
}

// A WRITE'S BUILD IS ONE READ'S DEADLINE: all its git together gets
// gitproc.ReadDeadline of run time, named apart as gitlet.WriteBuildDeadline
// since gitlet imports no git; a build whose git runs the whole of it is
// too_large, a change no node builds faster.
func TestAWriteBuildIsOneReadsDeadline(t *testing.T) {
	t.Parallel()
	if gitlet.WriteBuildDeadline != gitproc.ReadDeadline {
		t.Errorf("gitlet.WriteBuildDeadline is %v and gitproc.ReadDeadline %v: a build is one read's deadline",
			gitlet.WriteBuildDeadline, gitproc.ReadDeadline)
	}
	c := newWriteCase(t, gitlettest.SoloOptions{}, map[string]string{"README.md": "one\n"})
	for _, kind := range gitlet.EngineWriteKinds() {
		want := gitlet.WriteBuildDeadline
		switch kind {
		case gitlet.WriteMergeChange:
			// A MERGE'S IS THE MERGEABILITY'S: three reads' git.
			want = 3 * gitproc.ReadDeadline
		case gitlet.WriteRebaseChange:
			// A REBASE'S REPLAYS UP TO A THOUSAND COMMITS (BenchmarkEngineRebase).
			want = gitlet.RebaseBuildDeadline
		}
		if got := host.BuildBudget(c.server(), kind); got != want || kind.BuildDeadline() != want {
			t.Errorf("a host gives a %s's build %v (its kind says %v), want %v", kind, got, kind.BuildDeadline(),
				want)
		}
	}
	host.SetBuildBudget(c.server(), time.Nanosecond)
	res, err := c.write(files(c.repo, alice(), "refs/heads/seats/alice/slow", "", c.head, text("s.md", "s\n")))
	if err != nil || res.Refusal != gitlet.WriteTooLarge {
		t.Errorf("a build past its budget: %+v %v, want too_large", res, err)
	}
}

// A CHANGE IS JUDGED ON THE TREE AS THE CALL LEAVES IT, and built as it says:
// a path beneath one the call removes may be made anew; every other nesting of
// two named paths, a put over a directory, a move onto an entry, and an edit
// or mode of a non-file is refused by name. The result is exactly the parent
// with the change made (a put keeps an executable's mode, an emptied directory
// drops, an emptied root is the empty tree); a no-op change makes nothing.
func TestAChangeIsJudgedOnTheTreeAsTheCallLeavesIt(t *testing.T) {
	t.Parallel()
	c := newWriteCase(t, gitlettest.SoloOptions{}, map[string]string{"a.txt": "a\n", "d/x.txt": "x\n",
		"d/y.txt": "y\n", "exec.sh": "#!/bin/sh\n"})
	git := c.p.Git()
	git.Run(t, c.work, "update-index", "--chmod=+x", "exec.sh")
	git.Run(t, c.work, "commit", "-q", "-m", "executable")
	target, stderr, err := git.Try(t, c.work, strings.NewReader("a.txt"), "hash-object", "-w", "--stdin")
	if err != nil {
		t.Fatalf("a link's target: %v\n%s", err, stderr)
	}
	git.Run(t, c.work, "update-index", "--add", "--cacheinfo", "120000,"+strings.TrimSpace(target)+",link")
	git.Run(t, c.work, "commit", "-q", "-m", "a link")
	head := strings.TrimSpace(git.Run(t, c.work, "rev-parse", "HEAD"))
	c.MustPush(c.p, c.work, "main")
	del := func(p string) gitlet.FileOp { return gitlet.FileOp{Op: gitlet.FileOpDelete, Path: p} }
	edit := func(p string) gitlet.FileOp { return gitlet.FileOp{Op: gitlet.FileOpEdit, Path: p, Old: "x", New: "z"} }
	n := 0
	for _, tc := range []struct {
		name    string
		ops     []gitlet.FileOp
		refusal gitlet.WriteRefusal
		tree    []string // what ls-tree -r lists of the commit, where it lands
	}{
		{"a directory removed and made anew", []gitlet.FileOp{del("d"), text("d/z.txt", "z\n")}, "",
			[]string{"100644 a.txt", "100644 d/z.txt", "100755 exec.sh", "120000 link"}},
		{"a file removed and made a directory", []gitlet.FileOp{del("a.txt"), text("a.txt/b", "b\n")}, "",
			[]string{"100644 a.txt/b", "100644 d/x.txt", "100644 d/y.txt", "100755 exec.sh", "120000 link"}},
		{"a put over a directory", []gitlet.FileOp{text("d", "d\n")}, gitlet.WritePathConflict, nil},
		{"a put beneath a file", []gitlet.FileOp{text("a.txt/b", "b\n")}, gitlet.WritePathConflict, nil},
		{"an edit beneath a removed directory", []gitlet.FileOp{del("d"), edit("d/x.txt")}, gitlet.WritePathConflict,
			nil},
		{"a move onto an entry", []gitlet.FileOp{{Op: gitlet.FileOpMove, Path: "a.txt", To: "d/x.txt"}},
			gitlet.WritePathConflict, nil},
		{"an edit of nothing", []gitlet.FileOp{edit("missing.txt")}, gitlet.WriteNotFound, nil},
		{"an edit of a directory", []gitlet.FileOp{edit("d")}, gitlet.WriteNotAFile, nil},
		{"an edit of a link", []gitlet.FileOp{edit("link")}, gitlet.WriteNotAFile, nil},
		{"a mode of a link", []gitlet.FileOp{{Op: gitlet.FileOpMode, Path: "link", Mode: gitlet.ModeExecutable}},
			gitlet.WriteNotAFile, nil},
		{"a move", []gitlet.FileOp{{Op: gitlet.FileOpMove, Path: "a.txt", To: "b.txt"}}, "",
			[]string{"100644 b.txt", "100644 d/x.txt", "100644 d/y.txt", "100755 exec.sh", "120000 link"}},
		{"a directory emptied", []gitlet.FileOp{del("d/x.txt"), del("d/y.txt")}, "",
			[]string{"100644 a.txt", "100755 exec.sh", "120000 link"}},
		{"a put keeping a mode", []gitlet.FileOp{text("exec.sh", "#!/bin/sh\nexit 0\n")}, "",
			[]string{"100644 a.txt", "100644 d/x.txt", "100644 d/y.txt", "100755 exec.sh", "120000 link"}},
		{"a mode flipped", []gitlet.FileOp{{Op: gitlet.FileOpMode, Path: "exec.sh", Mode: gitlet.ModeFile}}, "",
			[]string{"100644 a.txt", "100644 d/x.txt", "100644 d/y.txt", "100644 exec.sh", "120000 link"}},
		{"an edit", []gitlet.FileOp{edit("d/x.txt")}, "",
			[]string{"100644 a.txt", "100644 d/x.txt", "100644 d/y.txt", "100755 exec.sh", "120000 link"}},
		{"a put of what is there", []gitlet.FileOp{text("a.txt", "a\n")}, gitlet.WriteNothingChanged, nil},
		{"everything removed", []gitlet.FileOp{del("a.txt"), del("d"), del("exec.sh"), del("link")}, "", nil},
	} {
		n++
		ref := fmt.Sprintf("refs/heads/seats/alice/t%d", n)
		res, err := c.write(files(c.repo, alice(), ref, "", head, tc.ops...))
		switch {
		case err != nil:
			t.Errorf("%s: %v", tc.name, err)
			continue
		case tc.refusal != "":
			if res.Refusal != tc.refusal || res.Detail == "" {
				t.Errorf("%s: %+v, want %s with its sentence", tc.name, res, tc.refusal)
			}
			continue
		case res.Outcome != statelog.OutcomeApplied:
			t.Errorf("%s: %+v, want applied", tc.name, res)
			continue
		}
		clone := c.clone()
		var got []string
		for line := range strings.Lines(git.Run(t, clone, "ls-tree", "-r", res.Commit)) {
			fields := strings.Fields(line)
			got = append(got, fields[0]+" "+fields[3])
		}
		if !slices.Equal(got, tc.tree) {
			t.Errorf("%s: the commit holds %v, want %v", tc.name, got, tc.tree)
		}
		if tc.tree == nil {
			if tree := c.show(clone, "%T", res.Commit); tree != "4b825dc642cb6eb9a060e54bf8d69288fbee4904" {
				t.Errorf("%s: the emptied root is %s, want the empty tree", tc.name, tree)
			}
		}
	}
}
