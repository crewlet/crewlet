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

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE ENGINE WRITE'S OWN CASES, beside the suite's (internal/code/codetest's
// F32–F38): what one git node's code host does with a write it is asked.

// linear is the freshness a write is judged at where a case does not say.
var linear = statelog.Freshness{Level: statelog.ReadLinearizable}

// alice is the fixture chart's full_access seat, as her write names her: with
// the run that made it.
func alice() code.Principal {
	return code.Principal{Kind: code.PrincipalAgent, Seat: codetest.FixtureAlice,
		Run: code.RunProvenance{TurnID: uuid.Must(uuid.NewV7()).String(), WorkKey: "ENG-7", Model: "fixture"}}
}

// pat is the fixture chart's person, through the token the fleet minted her.
func pat() code.Principal {
	return code.Principal{Kind: code.PrincipalOperator, OperatorID: iam.TokenLogin(codetest.FixturePat),
		Seat: codetest.FixturePat}
}

// opNow is an operation minted now; opAt one minted at at.
func opNow() string            { return statelog.NewOpID(time.Now(), "") }
func opAt(at time.Time) string { return statelog.NewOpID(at, "") }

// text is a put of content at path.
func text(path, content string) code.FileOp {
	return code.FileOp{Op: code.FileOpPut, Path: path, Content: &content}
}

// files is a commit_files by who onto ref: onto head, or creating ref from
// base where head is "".
func files(repo string, who code.Principal, ref, head, base string, ops ...code.FileOp) code.EngineWrite {
	return code.EngineWrite{Kind: code.WriteCommitFiles, RepoID: repo, Principal: who, OpID: opNow(),
		Fresh: linear, Ref: ref, ExpectedHead: head, Base: base, Message: "a change made through a tool", Files: ops}
}

// writeCase is a solo fleet with RepoName's main seeded by ceo.
type writeCase struct {
	*codetest.Case
	p    *codetest.Person
	work string
	head string
	repo string
}

// newWriteCase is a write case over o, main holding seed.
func newWriteCase(t *testing.T, o codetest.SoloOptions, seed map[string]string) *writeCase {
	t.Helper()
	c := newCase(t, o, codetest.Settings())
	p := codetest.NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	head := p.Commit(work, seed, "seed")
	c.MustPush(p, work, "main")
	return &writeCase{Case: c, p: p, work: work, head: head, repo: c.Rows().Repo.ID}
}

// server is the case's one git node's code host.
func (c *writeCase) server() *host.Server { return c.Fleet.Nodes()[0].Host() }

// write asks w of the git node's code host.
func (c *writeCase) write(w code.EngineWrite) (code.EngineWriteResult, error) {
	return c.server().Write(c.Ctx, w)
}

// land asks w and fails the case unless it applied — with its commit, every
// kind but a delete, which makes none.
func (c *writeCase) land(w code.EngineWrite) code.EngineWriteResult {
	c.T.Helper()
	res, err := c.write(w)
	if err != nil || res.Outcome != statelog.OutcomeApplied || (res.Commit == "") != (w.Kind == code.WriteDeleteBranch) {
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

// AN ENGINE COMMIT IS THE ACTING PRINCIPAL'S (ADR-0085): its author is the
// seat the signer names, at the address it names; its committer the engine,
// `Crewlet <engine@seats.invalid>` with code.identity_domain unset; its author
// date the operation's own second, and its committer date the same while the
// parent is older. And it is a pure function of the write and its parent: the
// same write built on two git nodes' scratches is one commit, byte for byte.
//
// Mutations: the committer named from the author — the committer line is
// alice's, red; the author date read from the node's clock — the operation
// was minted an hour ago, red.
func TestAnEngineCommitIsTheActingPrincipals(t *testing.T) {
	t.Parallel()
	minted := time.Now().UTC().Add(-time.Hour).Truncate(time.Second).Add(300 * time.Millisecond)
	op := opAt(minted)
	who := alice()
	var commits []string
	for range 2 {
		c := newWriteCase(t, codetest.SoloOptions{}, map[string]string{"README.md": "one\n"})
		w := files(c.repo, who, "refs/heads/seats/alice/sign", "", c.head, text("signed.md", "signed\n"))
		w.OpID = op
		res := c.land(w)
		commits = append(commits, res.Commit)
		clone := c.clone()
		got := c.show(clone, "%an <%ae>|%cn <%ce>|%at|%ct", res.Commit)
		second := strconv.FormatInt(minted.Unix(), 10)
		want := fmt.Sprintf("%s <%s>|%s <%s>|%s|%s", codetest.FixtureAlice,
			codetest.FixtureAuthorEmail(codetest.FixtureAlice), codetest.FixtureCommitterName,
			codetest.FixtureCommitterEmail, second, second)
		if got != want {
			t.Errorf("the commit is %q, want %q", got, want)
		}
	}
	if commits[0] != commits[1] {
		t.Errorf("one write built in two scratches is two commits, %s and %s", commits[0], commits[1])
	}
}

// A COMMIT IS NEVER DATED BEFORE ITS PARENT: over a parent committed an hour
// after the operation was minted — a person's clock ahead — the commit's
// author date is still the operation's second, and its committer date the
// parent's, so no walk by date meets a child before its parent.
//
// Mutations: clamp the author date too — it reads the parent's, red; leave the
// committer date at the operation's — it reads an hour before the parent, red.
func TestACommitIsNeverDatedBeforeItsParent(t *testing.T) {
	t.Parallel()
	c := newWriteCase(t, codetest.SoloOptions{}, map[string]string{"README.md": "one\n"})
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

// A RETRIED ENGINE WRITE IS ONE PUSH (ADR-0085's first Enforced-by): one write
// asked twice at once on one git node is built once — the second joins the
// first, which holds the write's one scratch, and is answered its result — and
// asked again once it landed is answered from its push's row with nothing
// built; every answer names WritePushID of its operation, a canonical UUIDv7.
//
// Mutations: a named derivation for the push id — the scratch refuses its
// name, red; drop the in-flight join — the second never joins, and its own
// scratch of one push id is refused, red.
func TestARetriedEngineWriteIsOnePush(t *testing.T) {
	t.Parallel()
	e := &recording{}
	c := newWriteCase(t, codetest.SoloOptions{Shape: codetest.Shape{WrapEstate: e.wrap}},
		map[string]string{"README.md": "one\n"})
	s := c.server()
	cmds := host.RecordGit(s)
	seeded := len(e.seen("propose"))
	w := files(c.repo, alice(), "refs/heads/seats/alice/once", "", c.head, text("once.md", "once\n"))
	pushID := code.WritePushID(w.OpID)
	if id, err := uuid.Parse(pushID); err != nil || id.String() != pushID || id.Version() != 7 ||
		strings.Contains(pushID, ".") {
		t.Fatalf("the push id %q is no canonical UUIDv7: %v", pushID, err)
	}

	held, reached := make(chan struct{}), make(chan struct{})
	c.Fleet.Faults().Arm(codefault.HostAfterUpload, codefault.Action{Once: true, Block: held, Reached: reached})
	type answer struct {
		res code.EngineWriteResult
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
	for deadline := time.Now().Add(10 * time.Second); time.Now().Before(deadline); {
		if host.WriteJoiners(s, pushID) == 1 {
			joined = true
			break
		}
		time.Sleep(5 * time.Millisecond)
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
// with nothing synced, built or proposed — so the walk asks a current git node
// — while a person's write, which reads no chart, lands there.
//
// Mutation: drop the check — the seat's write lands, red.
func TestAWriteBehindTheConfigurationIsUnready(t *testing.T) {
	t.Parallel()
	e := &recording{}
	c := newWriteCase(t, codetest.SoloOptions{Current: func() bool { return false },
		Shape: codetest.Shape{WrapEstate: e.wrap}}, map[string]string{"README.md": "one\n"})
	states, proposals := len(e.seen("state")), len(e.seen("propose"))
	_, err := c.write(files(c.repo, alice(), "refs/heads/seats/alice/x", "", c.head, text("a.md", "a\n")))
	var unready *code.ErrGitUnready
	if !errors.As(err, &unready) || unready.Reason != code.UnreadyBehindConfig {
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
// copy still holding a protection a revision has since removed, the write's
// first judgement refuses — and the copy, synced through the barrier, judges
// it again and lands it.
//
// Mutation: answer the first refusal — protected_direct, red.
func TestAWriteRefusalIsJudgedAtLinearizable(t *testing.T) {
	t.Parallel()
	e := &recording{}
	c := newWriteCase(t, codetest.SoloOptions{Shape: codetest.Shape{WrapEstate: e.wrap}},
		map[string]string{"README.md": "one\n"})
	open := codetest.Settings()
	open.Protections = nil
	c.Fleet.Declare(t, code.Declared{Name: codetest.RepoName, Settings: open})
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
// writing onto a head the branch is not at, over a copy read as it stands, is
// judged again at the log's head — one barrier a write — until its turn's
// sixteen on the node are spent; then the refusal is answered from its
// session, through no barrier, saying it was not judged again and when its
// re-syncs come back. A person's write, charged to nobody, is still judged
// again.
//
// Mutation: the re-judgement taken without the bucket (resync answering nil)
// — the seventeenth write reads through the barrier — red.
func TestASeatsWriteRefusalIsJudgedAgainWhileItsResyncsLast(t *testing.T) {
	t.Parallel()
	e := &recording{}
	c := newWriteCase(t, codetest.SoloOptions{Shape: codetest.Shape{WrapEstate: e.wrap}},
		map[string]string{"README.md": "one\n"})
	open := codetest.Settings()
	open.Protections = nil
	c.Fleet.Declare(t, code.Declared{Name: codetest.RepoName, Settings: open})
	stale := randomOID()
	write := func(who code.Principal) (code.Command, int) {
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
	for i := range code.ResyncBurst {
		if cmd, n := write(seat); n == 0 || cmd.Reason != code.RefusalStale {
			t.Fatalf("write %d of the turn's %d was refused %s through %d barriers, want stale through one", i+1,
				code.ResyncBurst, cmd.Reason, n)
		}
	}
	cmd, n := write(seat)
	if n != 0 || cmd.Reason != code.RefusalStale ||
		!strings.Contains(cmd.Detail, "(not re-judged at linearizable: this turn's re-syncs are spent until ") {
		t.Errorf("past its re-syncs the seat's write was refused %s %q through %d barriers, want stale from its "+
			"session, through none, saying so", cmd.Reason, cmd.Detail, n)
	}
	if cmd, n := write(pat()); n == 0 || cmd.Reason != code.RefusalStale {
		t.Errorf("a person's write was refused %s through %d barriers, want stale through one", cmd.Reason, n)
	}
}

// A SEAT'S WRITE IS HELD TO WHOEVER ITS TURN WORKS FOR, on the git node that
// builds it (code.EngineWrite.Ceiling, M3-decisions U40 (a)): alice, who
// writes the fixture repository, moves main for the unit's lead, who writes it
// too, and not for bob, who only reads it, nor for nobody — a stranger, or a
// person who may not act — each refused not_permitted in words saying whom the
// work is for, nothing built; and under every ceiling she still writes under
// her own namespace, her own work.
//
// Mutations: build with the seat's own grant alone (drop the ceiling in
// rules) — bob's and nobody's main lands, red; refuse the seat's own work under
// a ceiling (ownWork answering false) — the namespace write is refused, red.
func TestASeatsWriteIsHeldToWhoeverItsTurnWorksFor(t *testing.T) {
	t.Parallel()
	c := newWriteCase(t, codetest.SoloOptions{}, map[string]string{"README.md": "one\n"})
	open := codetest.Settings()
	open.Protections = nil
	c.Fleet.Declare(t, code.Declared{Name: codetest.RepoName, Settings: open})
	for _, tc := range []struct {
		name    string
		ceiling *code.WriteCeiling
		says    string
	}{
		{"a colleague who only reads", &code.WriteCeiling{Kind: code.WriteCeilingSeat, Seat: codetest.FixtureBob},
			"this work is for the seat whose ask started its turn"},
		{"nobody", &code.WriteCeiling{Kind: code.WriteCeilingNobody}, "may change nothing of this company's code"},
	} {
		w := files(c.repo, alice(), "refs/heads/main", c.head, "", text(tc.name+".md", "x\n"))
		w.Ceiling = tc.ceiling
		res, err := c.write(w)
		if err != nil {
			t.Fatalf("alice's write for %s: %v", tc.name, err)
		}
		if res.Command.Reason != code.RefusalNotPermitted || !strings.Contains(res.Command.Detail, tc.says) ||
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
	w.Ceiling = &code.WriteCeiling{Kind: code.WriteCeilingSeat, Seat: codetest.FixtureLead}
	c.land(w)
}

// A RETRIED WRITE THAT LANDED IS ANSWERED FROM ITS ROW: its branch moved on by
// a later push, the write asked again finds a head that is not the one it
// read, asks its own push's outcome before calling it stale, and answers
// applied with its first commit — building nothing.
//
// Mutation: answer stale before asking the outcome — stale, red.
func TestARetriedWriteThatLandedIsAnsweredFromItsRow(t *testing.T) {
	t.Parallel()
	c := newWriteCase(t, codetest.SoloOptions{}, map[string]string{"README.md": "one\n"})
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

// A WRITE WHOSE CALLER LEFT NO TIME FOR ITS PROPOSAL UPLOADS NOTHING: a caller
// whose deadline cannot hold the bundle's upload and the proposal after it is
// answered busy before a byte is stored — a slow bucket costs a refusal, never
// a PUT nobody proposes.
//
// Mutation: drop the rule — the bundle is stored, red.
func TestAWriteWithNoTimeForItsProposalUploadsNothing(t *testing.T) {
	t.Parallel()
	e := &recording{}
	c := newWriteCase(t, codetest.SoloOptions{Shape: codetest.Shape{WrapEstate: e.wrap}},
		map[string]string{"README.md": "one\n"})
	before, proposals := c.objects(), len(e.seen("propose"))
	ctx, cancel := context.WithTimeout(c.Ctx, code.MinProposeAttempt/2)
	defer cancel()
	res, err := c.server().Write(ctx, files(c.repo, alice(), "refs/heads/seats/alice/late", "", c.head,
		text("late.md", "late\n")))
	if err != nil || res.Command.Reason != code.RefusalBusy || !strings.Contains(res.Command.Detail, "upload") {
		t.Fatalf("a write with no time for its proposal: %+v %v, want busy naming its upload", res, err)
	}
	if after := c.objects(); !slices.Equal(after, before) {
		t.Errorf("the object store went from %v to %v", before, after)
	}
	if n := len(e.seen("propose")); n != proposals {
		t.Errorf("the write proposed %d times", n-proposals)
	}
}

// A CREATE AND A DELETE OF A BRANCH BUILD NOTHING: no git writer runs, no
// pack is stored and no unit of the seat share is taken — a create's target
// is checked present, the connectivity a receive-pack would have checked —
// and both land as pushes.
//
// Mutation: take a unit for them — the share holds one while the create
// lands, red.
func TestCreateAndDeleteBranchBuildNothing(t *testing.T) {
	t.Parallel()
	c := newWriteCase(t, codetest.SoloOptions{}, map[string]string{"README.md": "one\n"})
	cmds := host.RecordGit(c.server())
	share := c.Fleet.Nodes()[0].Share()
	held, reached := make(chan struct{}), make(chan struct{})
	c.Fleet.Faults().Arm(codefault.HostAfterPropose, codefault.Action{Once: true, Block: held, Reached: reached})
	create := code.EngineWrite{Kind: code.WriteCreateBranch, RepoID: c.repo, Principal: alice(), OpID: opNow(),
		Fresh: linear, Ref: "refs/heads/seats/alice/b", Commit: c.head}
	done := make(chan error, 1)
	var created code.EngineWriteResult
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
	deleted := c.land(code.EngineWrite{Kind: code.WriteDeleteBranch, RepoID: c.repo, Principal: alice(),
		OpID: opNow(), Fresh: linear, Ref: "refs/heads/seats/alice/b", ExpectedHead: c.head})
	if deleted.Command.New != code.ZeroOID {
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
	_, err := c.write(code.EngineWrite{Kind: code.WriteCreateBranch, RepoID: c.repo, Principal: alice(),
		OpID: opNow(), Fresh: linear, Ref: "refs/heads/seats/alice/c", Commit: strings.Repeat("ab", 20)})
	if !errors.Is(err, code.ErrObjectNotFound) {
		t.Errorf("a create at a commit the repository lacks = %v, want ErrObjectNotFound", err)
	}
}

// A WRITE'S BUILD HOLDS ONE UNIT OF THE SEAT SHARE, AND ITS LANDING NONE: while
// its git runs the node's seat git work holds its unit; given back before the
// upload, the proposal and the wait for its row, none of which is git.
//
// Mutation: give the unit back after the landing — it is held at the
// proposal, red.
func TestAWritesBuildHoldsTheShareAndItsLandingNone(t *testing.T) {
	t.Parallel()
	c := newWriteCase(t, codetest.SoloOptions{}, map[string]string{"README.md": "one\n"})
	share := c.Fleet.Nodes()[0].Share()
	building, release := host.HoldGit(c.server(), "commit-tree")
	held, reached := make(chan struct{}), make(chan struct{})
	c.Fleet.Faults().Arm(codefault.HostAfterPropose, codefault.Action{Once: true, Block: held, Reached: reached})
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
// its branch is, and its first cat-file — what every path it names and every
// directory above one is at that head, each asked whole as `<head>:<path>` —
// runs holding the build's unit of the seat share and charged to the build's
// budget of git. git walks each of those paths from the root, so their cost
// grows with the square of a path's depth, which no short check outside the
// share may run up.
//
// Mutation: read the paths in the check, before the build takes its unit —
// the read runs with no unit held, red.
func TestAWriteReadsItsPathsInItsBuild(t *testing.T) {
	t.Parallel()
	c := newWriteCase(t, codetest.SoloOptions{}, map[string]string{"docs/a/b.md": "one\n"})
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
// time: a commit_files putting a file one component deeper than a write may
// name (code.MaxTreePathDepth), or moving an entry there, is refused
// ErrInvalid naming the bound before any git process runs; and a put at the
// bound lands, its build running one mktree a level — as many as the bound,
// and no more. Every cost of a build that grows with depth is held down by
// that bound: the directories it rewrites, its mktree a level, and its read of
// what each named path and each directory above one is, which git walks from
// the root.
//
// Mutation: drop the depth bound — the deeper put runs git, red.
func TestAWritePastTheDepthBoundRunsNoGit(t *testing.T) {
	t.Parallel()
	c := newWriteCase(t, codetest.SoloOptions{}, map[string]string{"README.md": "one\n"})
	cmds := host.RecordGit(c.server())
	deep := strings.TrimSuffix(strings.Repeat("d/", code.MaxTreePathDepth), "/")
	for _, op := range []code.FileOp{
		text(deep+"/x.md", "x\n"),
		{Op: code.FileOpMove, Path: "README.md", To: deep + "/x.md"},
	} {
		res, err := c.write(files(c.repo, pat(), "refs/heads/main", c.head, "", op))
		if !errors.Is(err, code.ErrInvalid) || !strings.Contains(err.Error(), "code.MaxTreePathDepth") {
			t.Errorf("a %s %d components deep: %+v %v, want ErrInvalid naming code.MaxTreePathDepth", op.Op,
				code.MaxTreePathDepth+1, res, err)
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
	if mktrees != code.MaxTreePathDepth {
		t.Errorf("a put %d components deep ran %d mktree processes, want one a level, %d", code.MaxTreePathDepth,
			mktrees, code.MaxTreePathDepth)
	}
}

// A WRITE'S BUILD IS ONE READ'S DEADLINE: its git — every process of it,
// together — is given gitproc.ReadDeadline of run time, code.WriteBuildDeadline,
// which this package's constant names apart since code imports no git; and a
// build whose git runs the whole of it is too_large, a change no node builds
// faster.
func TestAWriteBuildIsOneReadsDeadline(t *testing.T) {
	t.Parallel()
	if code.WriteBuildDeadline != gitproc.ReadDeadline {
		t.Errorf("code.WriteBuildDeadline is %v and gitproc.ReadDeadline %v: a build is one read's deadline",
			code.WriteBuildDeadline, gitproc.ReadDeadline)
	}
	c := newWriteCase(t, codetest.SoloOptions{}, map[string]string{"README.md": "one\n"})
	for _, kind := range code.EngineWriteKinds() {
		want := code.WriteBuildDeadline
		switch kind {
		case code.WriteMergeChange:
			// A MERGE'S IS THE MERGEABILITY'S: three reads' git.
			want = 3 * gitproc.ReadDeadline
		case code.WriteRebaseChange:
			// A REBASE'S REPLAYS UP TO A THOUSAND COMMITS (BenchmarkEngineRebase).
			want = code.RebaseBuildDeadline
		}
		if got := host.BuildBudget(c.server(), kind); got != want || kind.BuildDeadline() != want {
			t.Errorf("a host gives a %s's build %v (its kind says %v), want %v", kind, got, kind.BuildDeadline(),
				want)
		}
	}
	host.SetBuildBudget(c.server(), time.Nanosecond)
	res, err := c.write(files(c.repo, alice(), "refs/heads/seats/alice/slow", "", c.head, text("s.md", "s\n")))
	if err != nil || res.Refusal != code.WriteTooLarge {
		t.Errorf("a build past its budget: %+v %v, want too_large", res, err)
	}
}

// A CHANGE IS JUDGED ON THE TREE AS THE CALL LEAVES IT, and built as it says:
// a path beneath one the call removes may be made anew there, while every
// other nesting of two paths a call names, a put over a directory, a move
// onto an entry, and an edit or a mode of what is no file is refused by name;
// what the change leaves is exactly the parent with the change made — a put
// keeping an executable's mode, an emptied directory dropped, an emptied root
// the empty tree — and a change that leaves the tree as it is makes nothing.
func TestAChangeIsJudgedOnTheTreeAsTheCallLeavesIt(t *testing.T) {
	t.Parallel()
	c := newWriteCase(t, codetest.SoloOptions{}, map[string]string{"a.txt": "a\n", "d/x.txt": "x\n",
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
	del := func(p string) code.FileOp { return code.FileOp{Op: code.FileOpDelete, Path: p} }
	edit := func(p string) code.FileOp { return code.FileOp{Op: code.FileOpEdit, Path: p, Old: "x", New: "z"} }
	n := 0
	for _, tc := range []struct {
		name    string
		ops     []code.FileOp
		refusal code.WriteRefusal
		tree    []string // what ls-tree -r lists of the commit, where it lands
	}{
		{"a directory removed and made anew", []code.FileOp{del("d"), text("d/z.txt", "z\n")}, "",
			[]string{"100644 a.txt", "100644 d/z.txt", "100755 exec.sh", "120000 link"}},
		{"a file removed and made a directory", []code.FileOp{del("a.txt"), text("a.txt/b", "b\n")}, "",
			[]string{"100644 a.txt/b", "100644 d/x.txt", "100644 d/y.txt", "100755 exec.sh", "120000 link"}},
		{"a put over a directory", []code.FileOp{text("d", "d\n")}, code.WritePathConflict, nil},
		{"a put beneath a file", []code.FileOp{text("a.txt/b", "b\n")}, code.WritePathConflict, nil},
		{"an edit beneath a removed directory", []code.FileOp{del("d"), edit("d/x.txt")}, code.WritePathConflict,
			nil},
		{"a move onto an entry", []code.FileOp{{Op: code.FileOpMove, Path: "a.txt", To: "d/x.txt"}},
			code.WritePathConflict, nil},
		{"an edit of nothing", []code.FileOp{edit("missing.txt")}, code.WriteNotFound, nil},
		{"an edit of a directory", []code.FileOp{edit("d")}, code.WriteNotAFile, nil},
		{"an edit of a link", []code.FileOp{edit("link")}, code.WriteNotAFile, nil},
		{"a mode of a link", []code.FileOp{{Op: code.FileOpMode, Path: "link", Mode: code.ModeExecutable}},
			code.WriteNotAFile, nil},
		{"a move", []code.FileOp{{Op: code.FileOpMove, Path: "a.txt", To: "b.txt"}}, "",
			[]string{"100644 b.txt", "100644 d/x.txt", "100644 d/y.txt", "100755 exec.sh", "120000 link"}},
		{"a directory emptied", []code.FileOp{del("d/x.txt"), del("d/y.txt")}, "",
			[]string{"100644 a.txt", "100755 exec.sh", "120000 link"}},
		{"a put keeping a mode", []code.FileOp{text("exec.sh", "#!/bin/sh\nexit 0\n")}, "",
			[]string{"100644 a.txt", "100644 d/x.txt", "100644 d/y.txt", "100755 exec.sh", "120000 link"}},
		{"a mode flipped", []code.FileOp{{Op: code.FileOpMode, Path: "exec.sh", Mode: code.ModeFile}}, "",
			[]string{"100644 a.txt", "100644 d/x.txt", "100644 d/y.txt", "100644 exec.sh", "120000 link"}},
		{"an edit", []code.FileOp{edit("d/x.txt")}, "",
			[]string{"100644 a.txt", "100644 d/x.txt", "100644 d/y.txt", "100755 exec.sh", "120000 link"}},
		{"a put of what is there", []code.FileOp{text("a.txt", "a\n")}, code.WriteNothingChanged, nil},
		{"everything removed", []code.FileOp{del("a.txt"), del("d"), del("exec.sh"), del("link")}, "", nil},
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
