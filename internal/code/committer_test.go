package code_test

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/store"
)

// THE COMMITTER, through the whole path: proposals queued, decided in one
// snapshot, published as one record on a real embedded broker and answered
// from the rows the shipped applier wrote.

// gatePublisher holds every publish at its gate until the case lets it
// through — one at a time with pass, or all at once with open — and then
// publishes through the node's own publisher.
type gatePublisher struct {
	inner code.Publisher
	gate  chan struct{}
	once  sync.Once

	// calls is how many publishes reached the gate; dead is how many went
	// on with a context that had already ended.
	calls atomic.Int32
	dead  atomic.Int32
}

func newGate(inner code.Publisher) *gatePublisher {
	return &gatePublisher{inner: inner, gate: make(chan struct{})}
}

func (g *gatePublisher) Publish(ctx context.Context, req statelog.Request) (statelog.Result, error) {
	g.calls.Add(1)
	<-g.gate
	if ctx.Err() != nil {
		g.dead.Add(1)
	}
	return g.inner.Publish(ctx, req)
}

// pass lets one held publish through.
func (g *gatePublisher) pass() { g.gate <- struct{}{} }

// open lets every publish through, now and later.
func (g *gatePublisher) open() { g.once.Do(func() { close(g.gate) }) }

// committerOn is a committer on n over pub, at the harness's clock, judging a
// seat's push by the grant tests' chart on UTC's calendar, with a recorder of
// its own. A gate is opened before the committer stops, so a case that fails
// while holding one still ends.
func committerOn(t testing.TB, n *node, pub code.Publisher,
	tune ...func(*code.CommitterOptions)) (*code.Committer, *metrics.Recorder) {

	t.Helper()
	rec, err := metrics.New()
	if err != nil {
		t.Fatal(err)
	}
	o := code.CommitterOptions{
		Publisher: pub, DB: n.db.Reader(), NodeID: n.id, Limits: code.DefaultLimits,
		Retention: func() time.Duration { return code.DefaultDeletedRetention },
		SeatRules: func() (code.SeatRules, bool) { return code.SeatRules{Roster: testChart(), Zone: time.UTC}, true },
		Now:       func() time.Time { return harnessAt }, Metrics: rec,
	}
	for _, f := range tune {
		f(&o)
	}
	c, err := code.NewCommitter(o)
	if err != nil {
		t.Fatalf("NewCommitter: %v", err)
	}
	t.Cleanup(func() {
		if g, ok := pub.(*gatePublisher); ok {
			g.open()
		}
		c.Stop(context.WithoutCancel(t.Context()))
	})
	return c, rec
}

// repoOn is a fresh repository on n, with main at a commit.
func repoOn(t testing.TB, n *node, name string) string {
	t.Helper()
	repo := n.ensure(name, suiteSettings(), t1).RepoID
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set("refs/heads/main", code.ZeroOID, oidOf("A"))))
	return repo
}

// proposal is one push to repo of undecided commands, with no pack.
func proposal(repo string, cmds ...code.Command) code.Proposal {
	return code.Proposal{
		PushID: uuid.Must(uuid.NewV7()).String(), RepoID: repo,
		Principal: code.Principal{Kind: code.PrincipalOperator, OperatorID: "op-1", Seat: "ada"},
		Via:       code.ViaGit, Node: "node-a", Commands: cmds, ReceivedAt: harnessAt,
	}
}

// update is an undecided command moving ref from old to new.
func update(ref, old, new string) code.Command { return code.Command{Ref: ref, Old: old, New: new} }

// packed is p carrying a one-object bundle whose key was minted at minted.
func packed(p code.Proposal, minted time.Time) code.Proposal {
	p.Pack = &code.PackRef{
		PackID: p.PushID, Object: objstore.KeyAt(minted), Hash: objstore.HashOf([]byte(p.PushID)),
		Size:         32 + code.IndexBytesFor(1) + code.ReverseBytesFor(1),
		IndexBytes:   code.IndexBytesFor(1),
		ReverseBytes: code.ReverseBytesFor(1), GitChecksum: oidOf("pack" + p.PushID), Objects: 1,
	}
	return p
}

// answer is one Propose's return.
type answer struct {
	res code.ProposeResult
	err error
}

// proposeAsync proposes p on its own goroutine and hands back its answer.
func proposeAsync(ctx context.Context, c *code.Committer, p code.Proposal) <-chan answer {
	out := make(chan answer, 1)
	go func() {
		res, err := c.Propose(ctx, p)
		out <- answer{res, err}
	}()
	return out
}

// await is the answer from ch, failing t if none comes in time.
func await(t *testing.T, ch <-chan answer) answer {
	t.Helper()
	select {
	case a := <-ch:
		return a
	case <-time.After(30 * time.Second):
		t.Fatal("no answer in 30 s")
		return answer{}
	}
}

// eventually fails t unless cond holds within ten seconds.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("%s did not happen in 10 s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// target is ref's value in repo on n, "" for none.
func (n *node) target(repo, ref string) string {
	n.t.Helper()
	var target string
	if err := n.db.Reader().Read(n.t.Context(), func(tx *sql.Tx) error {
		err := tx.QueryRowContext(n.t.Context(),
			`SELECT target FROM code_refs WHERE repo_id = ? AND ref = ?`, repo, ref).Scan(&target)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}); err != nil {
		n.t.Fatalf("read %s: %v", ref, err)
	}
	return target
}

// reading is one instrument's snapshot in rec.
func reading(rec *metrics.Recorder, name string) metrics.Snapshot {
	for _, s := range rec.Read() {
		if s.Name == name {
			return s
		}
	}
	return metrics.Snapshot{}
}

// requireOK fails t unless a is an applied push whose every command is ok.
func requireOK(t *testing.T, a answer) {
	t.Helper()
	if a.err != nil {
		t.Fatalf("Propose: %v", a.err)
	}
	if a.res.Outcome != statelog.OutcomeApplied || a.res.Position.IsZero() {
		t.Fatalf("outcome %q at %v, want applied at a position", a.res.Outcome, a.res.Position)
	}
	for _, c := range a.res.Answer.Commands {
		if c.Result != code.ResultOK {
			t.Fatalf("%s is %s %s (%q), want ok", c.Ref, c.Result, c.Reason, c.Detail)
		}
	}
}

// requireOnePlacement fails t unless a whole-push refusal's sentence is in
// ONE placement whichever path formed it: on the answer, and on every command
// the refusal covers — never on one and not the other.
func requireOnePlacement(t *testing.T, a code.PushAnswer) {
	t.Helper()
	if a.Refused == "" {
		return
	}
	if a.Detail == "" {
		t.Fatalf("the %s refusal of the whole push carries no sentence: %+v", a.Refused, a)
	}
	for _, c := range a.Commands {
		if c.Reason == a.Refused && c.Detail != a.Detail {
			t.Fatalf("%s is refused %s with %q, and the answer says %q — one refusal, two "+
				"sentences", c.Ref, c.Reason, c.Detail, a.Detail)
		}
	}
}

// decideOnly runs a batch's decide in a snapshot of n's rows and answers
// applied WITHOUT appending anything — a publish whose record no applier
// wrote a row for.
type decideOnly struct{ n *node }

func (d decideOnly) Publish(ctx context.Context, req statelog.Request) (statelog.Result, error) {
	var decision statelog.Decision
	if err := d.n.db.Reader().Read(ctx, func(tx *sql.Tx) error {
		var err error
		decision, err = req.Decide(tx, statelog.Stamp{Writer: d.n.id})
		return err
	}); err != nil {
		return statelog.Result{}, err
	}
	if decision.Empty() {
		return statelog.Result{}, errors.New("the decide formed no record")
	}
	return statelog.Result{Outcome: statelog.OutcomeApplied, OpID: req.OpID,
		Position: statelog.Position{Stream: code.Domain{}.Stream().Name, Seq: 1 << 20}}, nil
}

// AN OK IS ONLY EVER REPORTED FROM A ROW: a publish that answers applied for a
// record that wrote no row is an error, never an ok taken from the decision.
func TestAnOkComesOnlyFromARow(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo := repoOn(t, n, "web")
	c, _ := committerOn(t, n, decideOnly{n})
	res, err := c.Propose(t.Context(), proposal(repo, update("refs/heads/main", oidOf("A"), oidOf("B"))))
	if err == nil || !strings.Contains(err.Error(), "has no row") {
		t.Fatalf("Propose answered %+v, %v; want an error saying the push has no row", res, err)
	}
	for _, cmd := range res.Answer.Commands {
		if cmd.Result == code.ResultOK {
			t.Fatalf("%s was answered ok from a decision", cmd.Ref)
		}
	}
}

// lostAcknowledgement publishes a batch for real at the early instant — its
// record lands and applies — then decides it AGAIN at the late instant from a
// peer's rows that do not hold that record yet, as a round after a lost
// acknowledgement does, and answers the first round's record as the ledger
// finds it: applied, collapsed. The committer's last decision is then not the
// one that landed.
type lostAcknowledgement struct {
	n, peer *node
	clock   *fakeClock
	late    time.Time
}

func (l lostAcknowledgement) Publish(ctx context.Context, req statelog.Request) (statelog.Result, error) {
	res, err := l.n.pub.Publish(ctx, req)
	if err != nil {
		return res, err
	}
	l.clock.advance(l.late.Sub(l.clock.now()))
	if err := l.peer.db.Reader().Read(ctx, func(tx *sql.Tx) error {
		_, err := req.Decide(tx, statelog.Stamp{Writer: l.n.id})
		return err
	}); err != nil {
		return statelog.Result{}, err
	}
	return statelog.Result{Outcome: statelog.OutcomeApplied, Position: res.Position, OpID: req.OpID,
		Version: res.Version, Collapsed: true}, nil
}

// A ROW IS ALWAYS THE ANSWER: a batch whose record landed in one round and was
// decided again in a later one — at a later instant, from rows that did not
// hold it — is answered from the row its record applied, never from the last
// decision, which here refuses the push's pack as expired while its ref moved.
func TestARowIsAlwaysTheAnswer(t *testing.T) {
	t.Parallel()
	a, b := newTwoNodes(t)
	repo := repoOn(t, a, "web")
	b.drain()
	clock := &fakeClock{at: harnessAt}
	c, _ := committerOn(t, a, lostAcknowledgement{n: a, peer: b, clock: clock,
		late: harnessAt.Add(objstore.RecordWithin)}, func(o *code.CommitterOptions) { o.Now = clock.now })
	p := packed(proposal(repo, update("refs/heads/main", oidOf("A"), oidOf("B"))), harnessAt)
	res, err := c.Propose(t.Context(), p)
	requireOK(t, answer{res, err})
	if res.Answer.Collapsed {
		t.Fatalf("the push answered collapsed onto its own batch's record: %+v", res)
	}
	if main := a.target(repo, "refs/heads/main"); main != oidOf("B") {
		t.Fatalf("main is at %s, want the push's commit", main)
	}
}

// countingCalls is a publisher that counts its calls and appends nothing.
type countingCalls struct{ calls atomic.Int32 }

func (c *countingCalls) Publish(context.Context, statelog.Request) (statelog.Result, error) {
	c.calls.Add(1)
	return statelog.Result{}, errors.New("nothing should be published")
}

// A PROPOSAL WHOSE SIZES LAY OUT NO BUNDLE IS REFUSED before it is published:
// a row naming such a bundle is an object no cache can install.
func TestAProposalWhoseSizesLayOutNoBundleIsRefused(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	pub := &countingCalls{}
	c, _ := committerOn(t, n, pub)
	for name, tc := range map[string]struct {
		mutate func(*code.PackRef)
		field  string
	}{
		"an index one byte long": {func(k *code.PackRef) { k.IndexBytes++; k.Size++ }, "pack.index_bytes"},
		"a reverse index short":  {func(k *code.PackRef) { k.ReverseBytes -= 4; k.Size -= 4 }, "pack.reverse_bytes"},
		"a pack under a header and a trailer": {func(k *code.PackRef) { k.Size = k.IndexBytes + k.ReverseBytes + 31 },
			"pack.size"},
		"past MaxPackBytes": {func(k *code.PackRef) { k.Size = code.MaxPackBytes + 1 }, "pack.size"},
		// The one rule a git node holds the row to (PackRef.Check): a
		// proposal it would refuse to install is refused here.
		"a trailer of the zero id": {func(k *code.PackRef) { k.GitChecksum = code.ZeroOID }, "pack.git_checksum"},
		"no objects":               {func(k *code.PackRef) { k.Objects = 0 }, "pack.objects"},
	} {
		t.Run(name, func(t *testing.T) {
			p := packed(proposal("0190f3c4-0000-7000-8000-00000000c0de",
				update("refs/heads/main", code.ZeroOID, oidOf("B"))), harnessAt)
			tc.mutate(p.Pack)
			_, err := c.Propose(t.Context(), p)
			if !errors.Is(err, code.ErrInvalidProposal) || !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("Propose: %v; want ErrInvalidProposal naming %s", err, tc.field)
			}
		})
	}
	if got := pub.calls.Load(); got != 0 {
		t.Fatalf("%d publishes, want none", got)
	}
}

// QUEUED PROPOSALS SHARE ONE RECORD: twenty pushes that arrive while one
// record is in flight are the next record, all twenty applied.
func TestQueuedProposalsShareOneRecord(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo := repoOn(t, n, "web")
	gate := newGate(n.pub)
	c, rec := committerOn(t, n, gate)
	before := n.end()

	lead := proposeAsync(t.Context(), c, proposal(repo, update("refs/heads/lead", code.ZeroOID, oidOf("L"))))
	eventually(t, "the first publish reaching the gate", func() bool { return gate.calls.Load() == 1 })
	var queued []<-chan answer
	for i := range 20 {
		queued = append(queued, proposeAsync(t.Context(), c, proposal(repo,
			update(fmt.Sprintf("refs/heads/b%02d", i), code.ZeroOID, oidOf(fmt.Sprint(i))))))
	}
	eventually(t, "twenty proposals queued", func() bool { return code.CommitterQueued(c, repo) == 20 })
	gate.open()

	requireOK(t, await(t, lead))
	var position statelog.Position
	for i, ch := range queued {
		a := await(t, ch)
		requireOK(t, a)
		if i == 0 {
			position = a.res.Position
		}
		if a.res.Position != position {
			t.Fatalf("push %d applied at %v and the first at %v — one record carries them", i,
				a.res.Position, position)
		}
	}
	if got := n.end() - before; got != 2 {
		t.Fatalf("%d records appended, want 2: the first push, then the twenty behind it", got)
	}
	if h := reading(rec, metrics.CodeCommitBatchSize); h.Count != 2 || h.Sum != 21 {
		t.Fatalf("batch sizes observed %d times summing %v, want 1 and 20", h.Count, h.Sum)
	}
}

// A BATCH IS DECIDED IN ORDER: each push against what the ones queued before
// it did.
func TestABatchIsDecidedInOrder(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo := repoOn(t, n, "web")
	gate := newGate(n.pub)
	c, _ := committerOn(t, n, gate)

	lead := proposeAsync(t.Context(), c, proposal(repo, update("refs/heads/lead", code.ZeroOID, oidOf("L"))))
	eventually(t, "the first publish reaching the gate", func() bool { return gate.calls.Load() == 1 })
	var queued []<-chan answer
	for _, u := range []code.Command{
		update("refs/heads/main", oidOf("A"), oidOf("B")),
		update("refs/heads/main", oidOf("B"), oidOf("C")),
		update("refs/heads/main", oidOf("A"), oidOf("D")),
	} {
		queued = append(queued, proposeAsync(t.Context(), c, proposal(repo, u)))
		want := len(queued)
		eventually(t, "the proposal queued", func() bool { return code.CommitterQueued(c, repo) == want })
	}
	gate.open()
	requireOK(t, await(t, lead))
	requireOK(t, await(t, queued[0]))
	requireOK(t, await(t, queued[1]))
	third := await(t, queued[2])
	if third.err != nil {
		t.Fatal(third.err)
	}
	got := third.res.Answer.Commands[0]
	if got.Reason != code.RefusalStale || !strings.Contains(got.Detail, oidOf("C")[:7]) {
		t.Fatalf("the third push is %s %s (%q), want stale naming the second's commit", got.Result,
			got.Reason, got.Detail)
	}
	if main := n.target(repo, "refs/heads/main"); main != oidOf("C") {
		t.Fatalf("main is at %s, want the second push's commit", main)
	}
}

// lockedRepoOn is a fresh repository on n whose rules let nobody push to
// refs/heads/locked, with main at a commit.
func lockedRepoOn(t *testing.T, n *node, name string) string {
	t.Helper()
	repo := n.ensure(name, lockedSettings(), t1).RepoID
	n.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set("refs/heads/main", code.ZeroOID, oidOf("A"))))
	return repo
}

// lockedSettings is the suite's settings with refs/heads/locked taking no
// direct push.
func lockedSettings() code.RepoSettings {
	s := suiteSettings()
	s.Protections = append([]code.Protection{{Pattern: "refs/heads/locked", DirectPush: code.DirectNone}},
		s.Protections...)
	return s
}

// roundsSeen records how many protection verdicts each of c's decide rounds
// formed inside the publisher's snapshot.
func roundsSeen(c *code.Committer) func() []int {
	var mu sync.Mutex
	var rounds []int
	code.ObserveDecideRounds(c, func(formed int) {
		mu.Lock()
		defer mu.Unlock()
		rounds = append(rounds, formed)
	})
	return func() []int {
		mu.Lock()
		defer mu.Unlock()
		return append([]int(nil), rounds...)
	}
}

// A FIRST ROUND FORMS NO VERDICT INSIDE ITS SNAPSHOT: the verdicts are formed
// before the publish, holding no connection of the reader pool, and a round
// whose snapshot holds the rules they were formed under only reads — at the
// costliest rules a repository may carry, forming them is seconds of a core,
// and inside the snapshot each batch held a reader for all of it.
func TestAFirstRoundFormsNoVerdictInsideItsSnapshot(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo := lockedRepoOn(t, n, "web")
	c, _ := committerOn(t, n, n.pub)
	rounds := roundsSeen(c)
	res, err := c.Propose(t.Context(), proposal(repo,
		update("refs/heads/main", oidOf("A"), oidOf("B")),
		update("refs/heads/locked", code.ZeroOID, oidOf("L")),
		update("refs/heads/fresh", code.ZeroOID, oidOf("N"))))
	if err != nil {
		t.Fatal(err)
	}
	for _, cmd := range res.Answer.Commands {
		want := map[bool]code.Refusal{true: code.RefusalProtectedDirect}[cmd.Ref == "refs/heads/locked"]
		if cmd.Reason != want || (want == "") != (cmd.Result == code.ResultOK) {
			t.Fatalf("%s was decided %s %q, want %q", cmd.Ref, cmd.Result, cmd.Reason, want)
		}
	}
	seen := rounds()
	if len(seen) == 0 {
		t.Fatal("the publish ran no decide round")
	}
	for i, formed := range seen {
		if formed != 0 {
			t.Fatalf("round %d formed %d protection verdicts inside its snapshot (rounds %v), want "+
				"none: the verdicts are formed before the publish", i+1, formed, seen)
		}
	}
}

// A ROUND UNDER RULES CHANGED SINCE THE FORMATION FORMS THEM AGAIN: the rules
// the formation read are this node's rows a moment before the snapshot, and
// only the snapshot's count — a lock lifted by an activation in between is
// lifted for the push, never answered from the verdict formed under the lock.
func TestARoundUnderRulesChangedSinceTheFormationFormsThemAgain(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo := lockedRepoOn(t, n, "web")
	gate := newGate(n.pub)
	c, _ := committerOn(t, n, gate)
	rounds := roundsSeen(c)
	push := proposeAsync(t.Context(), c, proposal(repo,
		update("refs/heads/main", oidOf("A"), oidOf("B")),
		update("refs/heads/locked", code.ZeroOID, oidOf("L"))))
	// THE FORMATION HAS READ THE LOCK once the publish reaches the gate.
	eventually(t, "the publish reaching the gate", func() bool { return gate.calls.Load() == 1 })
	if got := n.ensure("web", suiteSettings(), t2); !got.Changed {
		t.Fatalf("lifting the lock changed nothing: %+v", got)
	}
	gate.open()
	requireOK(t, await(t, push))
	if seen := rounds(); len(seen) == 0 || seen[0] != 2 {
		t.Fatalf("the rounds formed %v verdicts inside their snapshots, want the first to form "+
			"both commands' under the rules it read", seen)
	}
	if got := n.target(repo, "refs/heads/locked"); got != oidOf("L") {
		t.Fatalf("refs/heads/locked is at %q, want the push's commit", got)
	}
}

// A CALLER THAT HANGS UP NEVER CANCELS THE BATCH: the batch is every caller's
// in it, so it runs under the committer's own lifetime.
func TestACallerThatHangsUpNeverCancelsTheBatch(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo := repoOn(t, n, "web")
	gate := newGate(n.pub)
	c, _ := committerOn(t, n, gate)

	lead := proposeAsync(t.Context(), c, proposal(repo, update("refs/heads/lead", code.ZeroOID, oidOf("L"))))
	eventually(t, "the first publish reaching the gate", func() bool { return gate.calls.Load() == 1 })
	hangUp, cancel := context.WithCancel(t.Context())
	defer cancel()
	gone := proposal(repo, update("refs/heads/gone", code.ZeroOID, oidOf("G")))
	first := proposeAsync(hangUp, c, gone)
	eventually(t, "the first proposal queued", func() bool { return code.CommitterQueued(c, repo) == 1 })
	second := proposeAsync(t.Context(), c, proposal(repo, update("refs/heads/kept", code.ZeroOID, oidOf("K"))))
	eventually(t, "the second proposal queued", func() bool { return code.CommitterQueued(c, repo) == 2 })
	gate.pass()
	requireOK(t, await(t, lead))
	eventually(t, "the two proposals' batch reaching the gate", func() bool { return gate.calls.Load() == 2 })

	cancel()
	if a := await(t, first); !errors.Is(a.err, context.Canceled) {
		t.Fatalf("the caller that hung up got %+v, %v; want context.Canceled", a.res, a.err)
	}
	gate.open()
	requireOK(t, await(t, second))
	if dead := gate.dead.Load(); dead != 0 {
		t.Fatalf("%d publishes ran on an ended context", dead)
	}
	if got := n.count(`SELECT COUNT(*) FROM code_pushes WHERE push_id = ?`, gone.PushID); got != 1 {
		t.Fatalf("the abandoned push has %d rows, want 1: its batch went on without its caller", got)
	}
}

// fill proposes mk(i) for i = 0, 1, … to repo while c's first batch is held,
// each on its own goroutine, until one is answered rather than queued — and
// answers how many were queued before it, that answer, and the queued
// proposals' answers to come.
func fill(t *testing.T, c *code.Committer, repo string, mk func(i int) code.Proposal) (
	int, code.ProposeResult, []<-chan answer) {

	t.Helper()
	var queued []<-chan answer
	for i := 0; ; i++ {
		start := time.Now()
		ch := proposeAsync(t.Context(), c, mk(i))
		for {
			if code.CommitterQueued(c, repo) == len(queued)+1 {
				queued = append(queued, ch)
				break
			}
			select {
			case a := <-ch:
				if a.err != nil {
					t.Fatalf("proposal %d: %v", i, a.err)
				}
				return len(queued), a.res, queued
			default:
			}
			if time.Since(start) > 10*time.Second {
				t.Fatalf("proposal %d was neither queued nor answered in 10 s", i)
			}
			time.Sleep(50 * time.Microsecond)
		}
	}
}

// A FULL QUEUE IS BUSY, NOT BLOCKED: past CommitterInbox a proposal is
// answered busy at once.
func TestAFullInboxIsBusyNotBlocked(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo := repoOn(t, n, "web")
	gate := newGate(n.pub)
	c, rec := committerOn(t, n, gate)
	lead := proposeAsync(t.Context(), c, proposal(repo, update("refs/heads/lead", code.ZeroOID, oidOf("L"))))
	eventually(t, "the first publish reaching the gate", func() bool { return gate.calls.Load() == 1 })

	accepted, busy, queued := fill(t, c, repo, func(i int) code.Proposal {
		// STALE, so the queue's batches append nothing once released.
		return proposal(repo, update(fmt.Sprintf("refs/heads/q%04d", i), oidOf("X"), oidOf("Y")))
	})
	if accepted < code.CommitterInbox {
		t.Fatalf("busy after %d proposals, want at least CommitterInbox (%d)", accepted, code.CommitterInbox)
	}
	if busy.Answer.Refused != code.RefusalBusy || busy.Answer.Commands[0].Reason != code.RefusalBusy {
		t.Fatalf("the proposal past the queue answered %+v, want busy", busy.Answer)
	}
	requireOnePlacement(t, busy.Answer)
	// ANSWERED WHILE THE BATCH IS STILL HELD: the gate has let nothing
	// through, so an answer that waited on the queue could not have come —
	// which a deadline would only approximate, and flake on a loaded runner.
	if got := gate.calls.Load(); got != 1 {
		t.Fatalf("%d publishes reached the gate before the busy answer, want the held one", got)
	}
	if got := reading(rec, metrics.CodeCommitBusy).Total; got != 1 {
		t.Fatalf("the busy counter reads %d, want 1", got)
	}
	gate.open()
	requireOK(t, await(t, lead))
	for _, ch := range queued {
		if a := await(t, ch); a.err != nil || a.res.Answer.Commands[0].Reason != code.RefusalStale {
			t.Fatalf("a queued proposal answered %+v, %v; want stale", a.res.Answer, a.err)
		}
	}
}

// A QUEUE FULL OF BYTES IS BUSY: large pushes fill a repository's queue by
// what they weigh long before they reach its count.
func TestAnInboxFullOfBytesIsBusy(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo := repoOn(t, n, "web")
	gate := newGate(n.pub)
	c, _ := committerOn(t, n, gate)
	lead := proposeAsync(t.Context(), c, proposal(repo, update("refs/heads/lead", code.ZeroOID, oidOf("L"))))
	eventually(t, "the first publish reaching the gate", func() bool { return gate.calls.Load() == 1 })

	maximal := func(i int) code.Proposal {
		var cmds []code.Command
		for k := range code.MaxCommandsPerPush {
			ref := fmt.Sprintf("refs/heads/%04d-%04d-", i, k)
			cmds = append(cmds, update(ref+strings.Repeat("x", code.MaxRefNameBytes-len(ref)),
				oidOf("X"), oidOf("Y")))
		}
		return proposal(repo, cmds...)
	}
	bound, err := code.EntryBound(maximal(0))
	if err != nil {
		t.Fatal(err)
	}
	accepted, busy, queued := fill(t, c, repo, maximal)
	switch {
	case accepted >= code.CommitterInbox:
		t.Fatalf("%d maximal proposals queued — the count bound, not the bytes", accepted)
	case accepted*bound > code.CommitterInboxBytes:
		t.Fatalf("%d proposals of %d bytes queued, past CommitterInboxBytes (%d)", accepted, bound,
			code.CommitterInboxBytes)
	case (accepted+1)*bound <= code.CommitterInboxBytes:
		t.Fatalf("busy after %d proposals of %d bytes, with room for another", accepted, bound)
	case busy.Answer.Refused != code.RefusalBusy:
		t.Fatalf("the proposal past the bytes answered %+v, want busy", busy.Answer)
	}
	gate.open()
	requireOK(t, await(t, lead))
	for _, ch := range queued {
		if a := await(t, ch); a.err != nil {
			t.Fatalf("a queued proposal: %v", a.err)
		}
	}
}

// A NODE FULL OF BYTES IS BUSY, whichever repository the next push is for:
// each repository's queue is bounded on its own, and the node bounds them
// together — from each push's queueing to its caller's answer, its batch
// included — because a node commits for every repository rendezvous ranks it
// first for, and a thousand of the largest pushes from as many
// repositories, each under its own repository's bound, held about a gibibyte
// of entries. Every push it held is released once its caller has its answer.
func TestANodeFullOfBytesIsBusy(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	web, api := repoOn(t, n, "web"), repoOn(t, n, "api")
	gate := newGate(n.pub)
	c, rec := committerOn(t, n, gate)
	// ONE REPOSITORY'S BYTES AND HALF ANOTHER'S: the first repository
	// fills its own queue, and the second meets the node's bound well
	// before its own.
	nodeBytes := code.CommitterInboxBytes + code.CommitterInboxBytes/2
	code.SetCommitterNodeBytes(c, nodeBytes)

	maximal := func(repo string) func(i int) code.Proposal {
		return func(i int) code.Proposal {
			cmds := make([]code.Command, 0, code.MaxCommandsPerPush)
			for k := range code.MaxCommandsPerPush {
				ref := fmt.Sprintf("refs/heads/%04d-%04d-", i, k)
				cmds = append(cmds, update(ref+strings.Repeat("x", code.MaxRefNameBytes-len(ref)),
					oidOf("X"), oidOf("Y")))
			}
			return proposal(repo, cmds...)
		}
	}
	bound, err := code.EntryBound(maximal(web)(0))
	if err != nil {
		t.Fatal(err)
	}
	// A BATCH HELD ON EACH, so every push after them queues — and each
	// lead is held too, in its batch.
	var held int
	var leads []<-chan answer
	for i, repo := range []string{web, api} {
		lead := proposal(repo, update("refs/heads/lead", code.ZeroOID, oidOf("L")))
		leadBound, err := code.EntryBound(lead)
		if err != nil {
			t.Fatal(err)
		}
		held += leadBound
		leads = append(leads, proposeAsync(t.Context(), c, lead))
		eventually(t, "a lead's publish reaching the gate", func() bool { return gate.calls.Load() == int32(i+1) })
	}

	acceptedWeb, busyWeb, queuedWeb := fill(t, c, web, maximal(web))
	if !strings.Contains(busyWeb.Answer.Detail, "for this repository") {
		t.Fatalf("the first repository's queue was refused %q — it was to meet its own bound, "+
			"under the node's", busyWeb.Answer.Detail)
	}
	held += acceptedWeb * bound
	if got := code.CommitterHeld(c); got != held {
		t.Fatalf("the node holds %d bytes, want %d: both leads and every queued push", got, held)
	}

	acceptedAPI, busyAPI, queuedAPI := fill(t, c, api, maximal(api))
	switch {
	case held+acceptedAPI*bound > nodeBytes:
		t.Fatalf("%d more proposals of %d bytes queued on top of %d held, past the node's %d",
			acceptedAPI, bound, held, nodeBytes)
	case held+(acceptedAPI+1)*bound <= nodeBytes:
		t.Fatalf("busy after %d more proposals of %d bytes on top of %d held, with room for "+
			"another under the node's %d", acceptedAPI, bound, held, nodeBytes)
	case (acceptedAPI+1)*bound > code.CommitterInboxBytes:
		t.Fatalf("the second repository's own bound (%d) was within reach at %d proposals, so "+
			"this case cannot tell the node's bound from it", code.CommitterInboxBytes, acceptedAPI)
	case busyAPI.Answer.Refused != code.RefusalBusy:
		t.Fatalf("the proposal past the node's bytes answered %+v, want busy", busyAPI.Answer)
	case !strings.Contains(busyAPI.Answer.Detail, "across this node's repositories"):
		t.Fatalf("the proposal past the node's bytes was refused %q, which does not say the "+
			"node was full rather than the repository", busyAPI.Answer.Detail)
	}
	requireOnePlacement(t, busyAPI.Answer)
	if got := reading(rec, metrics.CodeCommitBusy).Total; got != 2 {
		t.Fatalf("the busy counter reads %d, want 2: one per repository refused", got)
	}

	gate.open()
	for _, lead := range leads {
		requireOK(t, await(t, lead))
	}
	for _, ch := range append(queuedWeb, queuedAPI...) {
		if a := await(t, ch); a.err != nil {
			t.Fatalf("a queued proposal: %v", a.err)
		}
	}
	// RELEASED AS EACH CALLER LEFT, or the node would refuse every push
	// after its first full burst for as long as it ran.
	eventually(t, "every answered push released", func() bool { return code.CommitterHeld(c) == 0 })
}

// fakeClock is a clock a case moves by hand.
type fakeClock struct {
	mu sync.Mutex
	at time.Time
}

func (f *fakeClock) now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.at
}

func (f *fakeClock) advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.at = f.at.Add(d)
}

// A QUEUED PUSH IS NEVER DECIDED PAST ONE BUDGET: a deep queue's tail, asked
// with no deadline — the in-process path — and still waiting a whole
// CommitBudget after it arrived, behind a batch in flight and a full one after
// it, is answered busy, decided by nobody and appended by nobody: a push waits
// at most a budget before its own record is written. The full batch ahead of
// it, which waited half a budget, is committed. (A caller WITH a deadline is
// judged by it instead: TestAPushIsJudgedByItsCallersDeadline.)
func TestAQueuedPushIsNeverDecidedPastOneBudget(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo := repoOn(t, n, "web")
	gate := newGate(n.pub)
	c, rec := committerOn(t, n, gate)
	clock := &fakeClock{at: time.Unix(1_900_000_000, 0)}
	code.SetCommitterClock(c, clock.now)
	before := n.end()

	lead := proposeAsync(t.Context(), c, proposal(repo, update("refs/heads/lead", code.ZeroOID, oidOf("L"))))
	eventually(t, "the first publish reaching the gate", func() bool { return gate.calls.Load() == 1 })
	var head, tail []<-chan answer
	var tailIDs []string
	for i := range code.MaxPushesPerRecord + 3 {
		p := proposal(repo, update(fmt.Sprintf("refs/heads/q%03d", i), code.ZeroOID, oidOf(fmt.Sprint(i))))
		ch := proposeAsync(t.Context(), c, p)
		if i < code.MaxPushesPerRecord {
			head = append(head, ch)
		} else {
			tail, tailIDs = append(tail, ch), append(tailIDs, p.PushID)
		}
		want := i + 1
		eventually(t, "the proposal queued", func() bool { return code.CommitterQueued(c, repo) == want })
	}
	// HALF A BUDGET behind the lead: the next batch's pushes are decided.
	clock.advance(code.CommitBudget / 2)
	gate.pass()
	requireOK(t, await(t, lead))
	eventually(t, "the next batch reaching the gate", func() bool { return gate.calls.Load() == 2 })
	// A WHOLE BUDGET for the tail, still queued behind that batch.
	clock.advance(code.CommitBudget / 2)
	gate.open()
	for _, ch := range head {
		requireOK(t, await(t, ch))
	}
	for i, ch := range tail {
		a := await(t, ch)
		if a.err != nil || a.res.Outcome != "" || a.res.Answer.Refused != code.RefusalBusy ||
			a.res.Answer.Commands[0].Reason != code.RefusalBusy ||
			!strings.Contains(a.res.Answer.Detail, "waited") {
			t.Fatalf("the queue's tail answered %+v, %v; want busy, undecided, saying it waited",
				a.res, a.err)
		}
		if got := n.count(`SELECT COUNT(*) FROM code_pushes WHERE push_id = ?`, tailIDs[i]); got != 0 {
			t.Fatalf("a push answered busy has %d rows", got)
		}
	}
	if got := gate.calls.Load(); got != 2 {
		t.Fatalf("%d publishes, want 2: the lead and the full batch behind it", got)
	}
	if got := n.end() - before; got != 2 {
		t.Fatalf("%d records appended, want 2", got)
	}
	if got := reading(rec, metrics.CodeCommitBusy).Total; got != 3 {
		t.Fatalf("the busy counter reads %d, want the tail's 3", got)
	}
}

// A PUSH WHOSE CALLER WENT BEFORE ITS BATCH IS NEVER DECIDED: its asker may
// have handed it to the runner-up, so this node neither decides nor appends a
// push nobody here is waiting for — while the batch it would have joined
// commits the rest.
func TestAPushWhoseCallerWentBeforeItsBatchIsNeverDecided(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo := repoOn(t, n, "web")
	gate := newGate(n.pub)
	c, _ := committerOn(t, n, gate)
	before := n.end()

	lead := proposeAsync(t.Context(), c, proposal(repo, update("refs/heads/lead", code.ZeroOID, oidOf("L"))))
	eventually(t, "the first publish reaching the gate", func() bool { return gate.calls.Load() == 1 })
	hangUp, cancel := context.WithCancel(t.Context())
	defer cancel()
	gone := proposal(repo, update("refs/heads/gone", code.ZeroOID, oidOf("G")))
	first := proposeAsync(hangUp, c, gone)
	eventually(t, "the push queued", func() bool { return code.CommitterQueued(c, repo) == 1 })
	kept := proposeAsync(t.Context(), c, proposal(repo, update("refs/heads/kept", code.ZeroOID, oidOf("K"))))
	eventually(t, "the next push queued", func() bool { return code.CommitterQueued(c, repo) == 2 })
	cancel()
	if a := await(t, first); !errors.Is(a.err, context.Canceled) {
		t.Fatalf("the caller that went got %+v, %v; want context.Canceled", a.res, a.err)
	}
	gate.open()
	requireOK(t, await(t, lead))
	requireOK(t, await(t, kept))
	if got := n.count(`SELECT COUNT(*) FROM code_pushes WHERE push_id = ?`, gone.PushID); got != 0 {
		t.Fatalf("the push whose caller went has %d rows, want none", got)
	}
	if got := n.target(repo, "refs/heads/gone"); got != "" {
		t.Fatalf("refs/heads/gone is at %s, want no ref", got)
	}
	if got := n.end() - before; got != 2 {
		t.Fatalf("%d records appended, want 2: the lead and the push that was waited for", got)
	}
}

// requireBusyUndecided fails t unless a is a push answered busy with no
// record, saying why in a sentence that contains says, and its push has no
// row on n.
func requireBusyUndecided(t *testing.T, n *node, a answer, pushID, says string) {
	t.Helper()
	if a.err != nil || a.res.Outcome != "" || !a.res.Position.IsZero() ||
		a.res.Answer.Refused != code.RefusalBusy || a.res.Answer.Commands[0].Reason != code.RefusalBusy ||
		!strings.Contains(a.res.Answer.Detail, says) {
		t.Fatalf("answered %+v, %v; want busy, undecided, saying %q", a.res, a.err, says)
	}
	requireOnePlacement(t, a.res.Answer)
	if got := n.count(`SELECT COUNT(*) FROM code_pushes WHERE push_id = ?`, pushID); got != 0 {
		t.Fatalf("a push answered busy has %d rows", got)
	}
}

// A PUSH ITS ASKER CANNOT WAIT FOR IS BUSY AT ONCE: a caller whose deadline is
// nearer than a batch and its answer may take — a routed proposal whose
// asker's attempt was mostly spent before it arrived — is answered busy
// without taking a place in the queue, decided by nobody and appended by
// nobody, because any answer a batch gave it would come after the asker had
// handed the push to the runner-up.
func TestAPushItsAskerCannotWaitForIsBusyAtOnce(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo := repoOn(t, n, "web")
	gate := newGate(n.pub)
	c, rec := committerOn(t, n, gate)
	before := n.end()

	// A BATCH IN FLIGHT holds the repository's goroutine, so a push that
	// took a place in the queue would still hold it when its answer came.
	lead := proposeAsync(t.Context(), c, proposal(repo, update("refs/heads/lead", code.ZeroOID, oidOf("L"))))
	eventually(t, "the first publish reaching the gate", func() bool { return gate.calls.Load() == 1 })
	ctx, cancel := context.WithTimeout(t.Context(), code.CommitBudget)
	defer cancel()
	p := proposal(repo, update("refs/heads/late", code.ZeroOID, oidOf("T")))
	res, err := c.Propose(ctx, p)
	requireBusyUndecided(t, n, answer{res, err}, p.PushID, "stops waiting")
	if got := code.CommitterQueued(c, repo); got != 0 {
		t.Fatalf("%d proposals queued, want none: a push nobody can wait for takes no place", got)
	}
	gate.open()
	requireOK(t, await(t, lead))
	if got := gate.calls.Load(); got != 1 {
		t.Fatalf("%d publishes, want the lead's alone", got)
	}
	if got := n.end() - before; got != 1 {
		t.Fatalf("%d records appended, want the lead's alone", got)
	}
	if got := reading(rec, metrics.CodeCommitBusy).Total; got != 1 {
		t.Fatalf("the busy counter reads %d, want 1", got)
	}
}

// A QUEUED PUSH IS ANSWERED BEFORE ITS ASKER GIVES UP: a push waiting behind a
// batch that runs long — a broker election holding the record ahead of it —
// is answered busy WHILE IT WAITS, the moment its caller's deadline leaves no
// more than a batch and its answer, rather than reaching the head of the queue
// after its asker had stopped waiting and taken the silence for a dead
// committer. It is withdrawn undecided: when the batch ahead of it ends, it is
// neither decided nor appended.
func TestAQueuedPushIsAnsweredBeforeItsAskerGivesUp(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo := repoOn(t, n, "web")
	gate := newGate(n.pub)
	c, rec := committerOn(t, n, gate)
	before := n.end()

	lead := proposeAsync(t.Context(), c, proposal(repo, update("refs/heads/lead", code.ZeroOID, oidOf("L"))))
	eventually(t, "the first publish reaching the gate", func() bool { return gate.calls.Load() == 1 })
	ctx, cancel := context.WithTimeout(t.Context(), code.CommitBudget+code.ReplyMargin+200*time.Millisecond)
	defer cancel()
	waiting := proposal(repo, update("refs/heads/waiting", code.ZeroOID, oidOf("W")))
	ch := proposeAsync(ctx, c, waiting)
	var got answer
	select {
	case got = <-ch:
	case <-time.After(10 * time.Second):
		t.Fatal("the queued push was not answered while the batch ahead of it held the gate")
	}
	requireBusyUndecided(t, n, got, waiting.PushID, "stops waiting")
	if calls := gate.calls.Load(); calls != 1 {
		t.Fatalf("%d publishes when the queued push was answered, want the lead's alone", calls)
	}

	gate.open()
	requireOK(t, await(t, lead))
	eventually(t, "the queue drained", func() bool { return code.CommitterQueued(c, repo) == 0 })
	if got := gate.calls.Load(); got != 1 {
		t.Fatalf("%d publishes, want the lead's alone: the withdrawn push started no batch", got)
	}
	if got := n.end() - before; got != 1 {
		t.Fatalf("%d records appended, want the lead's alone", got)
	}
	if got := n.count(`SELECT COUNT(*) FROM code_pushes WHERE push_id = ?`, waiting.PushID); got != 0 {
		t.Fatalf("the withdrawn push has %d rows, want none", got)
	}
	if got := n.target(repo, "refs/heads/waiting"); got != "" {
		t.Fatalf("refs/heads/waiting is at %s, want no ref", got)
	}
	if got := reading(rec, metrics.CodeCommitBusy).Total; got != 1 {
		t.Fatalf("the busy counter reads %d, want 1", got)
	}
}

// A PUSH IS JUDGED BY ITS CALLER'S DEADLINE, NEVER BY ITS WAIT: one that
// reaches its batch with too little of its asker's attempt left is answered
// busy undecided whatever it waited, and one whose asker gave it more than a
// whole budget's wait is decided — the asker's attempt is the bound, and a
// wait counted from arrival forgot whatever the asker spent before the push
// arrived.
func TestAPushIsJudgedByItsCallersDeadline(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo := repoOn(t, n, "web")
	gate := newGate(n.pub)
	c, rec := committerOn(t, n, gate)
	clock := &fakeClock{at: time.Unix(1_900_000_000, 0)}
	code.SetCommitterClock(c, clock.now)
	before := n.end()

	lead := proposeAsync(t.Context(), c, proposal(repo, update("refs/heads/lead", code.ZeroOID, oidOf("L"))))
	eventually(t, "the first publish reaching the gate", func() bool { return gate.calls.Load() == 1 })
	// Deadlines on the committer's clock: an hour more than a batch and its
	// answer, and ten hours — so no real timer fires while the case runs.
	shortCtx, cancelShort := context.WithDeadline(t.Context(),
		clock.now().Add(code.CommitBudget+code.ReplyMargin+time.Hour))
	defer cancelShort()
	longCtx, cancelLong := context.WithDeadline(t.Context(), clock.now().Add(10*time.Hour))
	defer cancelLong()
	short := proposal(repo, update("refs/heads/short", code.ZeroOID, oidOf("S")))
	shortCh := proposeAsync(shortCtx, c, short)
	eventually(t, "the push queued", func() bool { return code.CommitterQueued(c, repo) == 1 })
	long := proposeAsync(longCtx, c, proposal(repo, update("refs/heads/long", code.ZeroOID, oidOf("G"))))
	eventually(t, "the next push queued", func() bool { return code.CommitterQueued(c, repo) == 2 })

	// AN HOUR AND A SECOND behind the lead: the first push's asker stops
	// waiting inside a batch and its answer; the second's has hours left,
	// though it waited far past one budget.
	clock.advance(time.Hour + time.Second)
	gate.open()
	requireOK(t, await(t, lead))
	requireBusyUndecided(t, n, await(t, shortCh), short.PushID, "stops waiting")
	requireOK(t, await(t, long))
	if got := n.end() - before; got != 2 {
		t.Fatalf("%d records appended, want 2: the lead, and the push its asker still waits for", got)
	}
	if got := reading(rec, metrics.CodeCommitBusy).Total; got != 1 {
		t.Fatalf("the busy counter reads %d, want 1", got)
	}
}

// A BATCH IS DECIDED PAST WHAT ITS PUSHES WERE CHECKED AGAINST: the receiving
// node formed a push's commands against the state its own sync read, and the
// repository's committer — another node — may be behind it. Decided from below
// it, a command whose old value the client was shown is refused stale and a
// repository it was shown is refused deleted: definite refusals that are
// false, and an empty decision the broker never arbitrates. So the committer
// reaches the push's Seen position before it decides — and, where it cannot
// within its budget, runs nothing (ErrNotServing), so the router asks the
// runner-up.
func TestABatchIsDecidedPastWhatItsPushesWereCheckedAgainst(t *testing.T) {
	t.Parallel()
	t.Run("a ref the receiver saw move", func(t *testing.T) {
		t.Parallel()
		a, b := newTwoNodes(t)
		repo := repoOn(t, a, "web")
		b.drain()
		a.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set("refs/heads/main", oidOf("A"), oidOf("B"))))
		c, _ := committerOn(t, b, b.pub)
		p := proposal(repo, update("refs/heads/main", oidOf("B"), oidOf("C")))
		p.Seen = a.waiter.Committed()
		res, err := c.Propose(t.Context(), p)
		requireOK(t, answer{res, err})
		if got := b.target(repo, "refs/heads/main"); got != oidOf("C") {
			t.Fatalf("refs/heads/main is at %s on the committer, want %s", got, oidOf("C"))
		}
	})
	t.Run("a repository the receiver saw created", func(t *testing.T) {
		t.Parallel()
		a, b := newTwoNodes(t)
		repo := repoOn(t, a, "web")
		c, _ := committerOn(t, b, b.pub)
		p := proposal(repo, update("refs/heads/feature", code.ZeroOID, oidOf("F")))
		p.Seen = a.waiter.Committed()
		res, err := c.Propose(t.Context(), p)
		requireOK(t, answer{res, err})
	})
	t.Run("past what the committer reaches in its budget", func(t *testing.T) {
		t.Parallel()
		a, b := newTwoNodes(t)
		repo := repoOn(t, a, "web")
		b.drain()
		a.push(repo, entry(uuid.Must(uuid.NewV7()).String(), set("refs/heads/main", oidOf("A"), oidOf("B"))))
		b.hold()
		c, _ := committerOn(t, b, b.pub)
		before := b.end()
		p := proposal(repo, update("refs/heads/main", oidOf("B"), oidOf("C")))
		p.Seen = a.waiter.Committed()
		res, err := c.Propose(t.Context(), p)
		if !errors.Is(err, code.ErrNotServing) {
			t.Fatalf("a committer behind the push's Seen answered %+v, %v; want ErrNotServing",
				res, err)
		}
		if got := b.end() - before; got != 0 {
			t.Fatalf("%d records appended, want none", got)
		}
	})
}

// A PROPOSAL'S SEEN IS ON THE CODE LOG: a position on another log names no
// state of the code host the decide could wait for, so a receiver that formed
// one is refused by name rather than waited on.
func TestAProposalSeenOnAnotherLogIsRefused(t *testing.T) {
	t.Parallel()
	p := proposal("0190f3c4-0000-7000-8000-00000000c0de", update("refs/heads/main", code.ZeroOID, oidOf("B")))
	p.Seen = statelog.Position{Stream: "CREWLET_TRACKER_LOG", Generation: 1, Seq: 3}
	if err := p.Validate(); !errors.Is(err, code.ErrInvalidProposal) || !strings.Contains(err.Error(), "seen") {
		t.Fatalf("Validate: %v; want ErrInvalidProposal naming seen", err)
	}
	p.Seen.Stream = code.Domain{}.Stream().Name
	if err := p.Validate(); err != nil {
		t.Fatalf("a Seen on the code log: %v", err)
	}
}

// A RETRIED PUSH COLLAPSES ON ITS ROW, proposed again after it landed or
// twice in one batch.
func TestARetriedPushCollapsesOnItsRow(t *testing.T) {
	t.Parallel()
	t.Run("again after it landed", func(t *testing.T) {
		t.Parallel()
		n := newNode(t)
		repo := repoOn(t, n, "web")
		c, _ := committerOn(t, n, n.pub)
		p := proposal(repo, update("refs/heads/main", oidOf("A"), oidOf("B")))
		first, err := c.Propose(t.Context(), p)
		requireOK(t, answer{first, err})
		again, err := c.Propose(t.Context(), p)
		requireOK(t, answer{again, err})
		if !again.Answer.Collapsed || again.Position != first.Position {
			t.Fatalf("the retry answered %+v, want collapsed at %v", again, first.Position)
		}
		if got := n.count(`SELECT COUNT(*) FROM code_pushes WHERE push_id = ?`, p.PushID); got != 1 {
			t.Fatalf("%d rows for one push", got)
		}
	})
	t.Run("twice in one batch", func(t *testing.T) {
		t.Parallel()
		n := newNode(t)
		repo := repoOn(t, n, "web")
		gate := newGate(n.pub)
		c, _ := committerOn(t, n, gate)
		lead := proposeAsync(t.Context(), c, proposal(repo, update("refs/heads/lead", code.ZeroOID, oidOf("L"))))
		eventually(t, "the first publish reaching the gate", func() bool { return gate.calls.Load() == 1 })
		p := proposal(repo, update("refs/heads/main", oidOf("A"), oidOf("B")))
		a := proposeAsync(t.Context(), c, p)
		eventually(t, "the push queued", func() bool { return code.CommitterQueued(c, repo) == 1 })
		b := proposeAsync(t.Context(), c, p)
		eventually(t, "its repeat queued", func() bool { return code.CommitterQueued(c, repo) == 2 })
		gate.open()
		requireOK(t, await(t, lead))
		first, second := await(t, a), await(t, b)
		requireOK(t, first)
		requireOK(t, second)
		if first.res.Position != second.res.Position {
			t.Fatalf("the repeat answered at %v and the push at %v", second.res.Position, first.res.Position)
		}
		if got := n.count(`SELECT COUNT(*) FROM code_pushes WHERE push_id = ?`, p.PushID); got != 1 {
			t.Fatalf("%d rows for one push", got)
		}
	})
}

// A PUSH ANSWERED WITH NO RECORD WRITES NOTHING: an --atomic push with one
// refusal, and a push whose every command is noop or ng.
func TestAPushAnsweredWithNoRecordWritesNothing(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		atomic bool
		cmds   []code.Command
		want   []code.Refusal
	}{
		"atomic with one stale": {true, []code.Command{
			update("refs/heads/new", code.ZeroOID, oidOf("N")),
			update("refs/heads/main", oidOf("Z"), oidOf("B")),
		}, []code.Refusal{code.RefusalAtomic, code.RefusalStale}},
		"every command noop or ng": {false, []code.Command{
			update("refs/heads/main", oidOf("Z"), oidOf("A")),
			update("refs/heads/main2", oidOf("Z"), oidOf("B")),
		}, []code.Refusal{"", code.RefusalStale}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			n := newNode(t)
			repo := repoOn(t, n, "web")
			c, _ := committerOn(t, n, n.pub)
			before := n.end()
			p := proposal(repo, tc.cmds...)
			p.Atomic = tc.atomic
			res, err := c.Propose(t.Context(), p)
			if err != nil {
				t.Fatal(err)
			}
			if res.Outcome != "" {
				t.Fatalf("outcome %q, want none", res.Outcome)
			}
			for i, cmd := range res.Answer.Commands {
				if cmd.Reason != tc.want[i] || cmd.Result == code.ResultOK {
					t.Errorf("command %d is %s %s, want %q", i, cmd.Result, cmd.Reason, tc.want[i])
				}
			}
			if got := n.end(); got != before {
				t.Fatalf("the log moved from %d to %d", before, got)
			}
		})
	}
}

// A PUSH IS REFUSED WHOLE FOR WHAT ITS PACK OR ITS REPOSITORY IS: a pack
// named too long ago, one past the repository's limit, and a repository
// archived or deleted — the deleted one told, through the committer's own
// Retention, when its purge may begin: the deletion's instant on the rows
// plus the deciding epoch's code.deleted_retention_days, which this case sets
// to something other than the default so that a committer deciding with any
// other retention names the wrong instant and fails it.
func TestAPushIsRefusedForItsPackOrItsRepository(t *testing.T) {
	t.Parallel()
	size := 32 + code.IndexBytesFor(1) + code.ReverseBytesFor(1)
	const retention = 11 * 24 * time.Hour
	for name, tc := range map[string]struct {
		minted  time.Time
		limit   int64
		settle  func(t *testing.T, n *node, repo string)
		reason  code.Refusal
		refused code.Refusal
		// says is what the refusal's sentence must say, given the
		// repository's id and the node holding its rows.
		says func(t *testing.T, n *node, repo string) string
	}{
		"an expired upload":          {minted: harnessAt.Add(-objstore.RecordWithin), reason: code.RefusalUploadExpired},
		"past the repository's size": {minted: harnessAt, limit: size - 1, reason: code.RefusalRepoSize},
		"archived": {minted: harnessAt, reason: code.RefusalRepoArchived, refused: code.RefusalRepoArchived,
			settle: func(t *testing.T, n *node, repo string) {
				s := suiteSettings()
				s.Archived = true
				n.ensure("web", s, t2)
			},
			says: func(*testing.T, *node, string) string { return "repository web is archived" }},
		"deleted": {minted: harnessAt, reason: code.RefusalRepoDeleted, refused: code.RefusalRepoDeleted,
			settle: func(t *testing.T, n *node, repo string) {
				if _, err := n.store.DeleteRepo(t.Context(), n.activate(t2), repo); err != nil {
					t.Fatal(err)
				}
				n.drain()
			},
			says: func(t *testing.T, n *node, repo string) string {
				var deleted int64
				if err := n.db.Reader().Read(t.Context(), func(tx *sql.Tx) error {
					return tx.QueryRowContext(t.Context(),
						`SELECT deleted_at FROM code_repos WHERE id = ?`, repo).Scan(&deleted)
				}); err != nil {
					t.Fatal(err)
				}
				return "repository web is deleted; declaring it again in code.repos restores it as " +
					"it was, until its purge, which may begin from " +
					store.DecodeTime(deleted).Add(retention).UTC().Format(time.RFC3339)
			}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			n := newNode(t)
			repo := repoOn(t, n, "web")
			if tc.settle != nil {
				tc.settle(t, n, repo)
			}
			c, _ := committerOn(t, n, n.pub, func(o *code.CommitterOptions) {
				if tc.limit > 0 {
					o.Limits = func() code.Limits {
						l := code.DefaultLimits()
						l.RepoMaxBytes = tc.limit
						return l
					}
				}
				o.Retention = func() time.Duration { return retention }
			})
			before := n.end()
			res, err := c.Propose(t.Context(), packed(proposal(repo,
				update("refs/heads/new", code.ZeroOID, oidOf("N"))), tc.minted))
			if err != nil {
				t.Fatal(err)
			}
			if got := res.Answer.Commands[0]; got.Reason != tc.reason || got.Detail == "" {
				t.Fatalf("the push is %s %s (%q), want %s with a detail", got.Result, got.Reason,
					got.Detail, tc.reason)
			}
			if tc.says != nil {
				if want := tc.says(t, n, repo); !strings.Contains(res.Answer.Detail, want) {
					t.Fatalf("the refusal says %q, want it to say %q", res.Answer.Detail, want)
				}
			}
			if res.Answer.Refused != tc.refused {
				t.Fatalf("the push's refusal is %q, want %q", res.Answer.Refused, tc.refused)
			}
			requireOnePlacement(t, res.Answer)
			if got := n.end(); got != before {
				t.Fatalf("the log moved from %d to %d", before, got)
			}
		})
	}
}

// A COMMITTER IS REFUSED WITHOUT WHAT IT DECIDES AND ANSWERS FROM: each
// required option unset is a constructor error naming it, never a default a
// decision would read as the company's own.
func TestACommitterRefusesWhatItCannotWorkWithout(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	for name, tc := range map[string]struct {
		unset func(*code.CommitterOptions)
		names string
	}{
		"no publisher": {func(o *code.CommitterOptions) { o.Publisher = nil }, "write authority"},
		"no estate":    {func(o *code.CommitterOptions) { o.DB = store.ReplicatedReader{} }, "replicated estate"},
		"no node id":   {func(o *code.CommitterOptions) { o.NodeID = "" }, "deciding node's id"},
		"no limits":    {func(o *code.CommitterOptions) { o.Limits = nil }, "code.limits"},
		"no retention": {func(o *code.CommitterOptions) { o.Retention = nil }, "code.deleted_retention_days"},
		"no chart":     {func(o *code.CommitterOptions) { o.SeatRules = nil }, "SeatRules"},
	} {
		t.Run(name, func(t *testing.T) {
			o := code.CommitterOptions{
				Publisher: n.pub, DB: n.db.Reader(), NodeID: n.id, Limits: code.DefaultLimits,
				Retention: func() time.Duration { return code.DefaultDeletedRetention },
				SeatRules: func() (code.SeatRules, bool) { return code.SeatRules{}, false },
			}
			tc.unset(&o)
			c, err := code.NewCommitter(o)
			if err == nil {
				c.Stop(t.Context())
				t.Fatal("NewCommitter took options it cannot work without")
			}
			if !strings.Contains(err.Error(), tc.names) {
				t.Fatalf("NewCommitter: %v; want it to name %q", err, tc.names)
			}
		})
	}
}

// purgedRepo is a repository named name on n, deleted past the retention and
// purged by a retention pass to its deletion marker — the one the applier
// writes, holding the name — and its id.
func purgedRepo(t *testing.T, n *node, name string) string {
	t.Helper()
	n.ensure(name, described(name), t1)
	id := n.mustID(name)
	n.deleteAt(id, harnessAt.Add(-2*keep))
	var p purged
	if report, err := passOver(n.store, n.reader, &p).Pass(t.Context()); err != nil ||
		!slices.Equal(report.Purged, []string{name}) {
		t.Fatalf("Pass = %+v, %v; want %s purged", report, err, name)
	}
	if marked := n.count(`SELECT COUNT(*) FROM code_deletions WHERE repo_id = ? AND name = ?`,
		id, name); marked != 1 {
		t.Fatalf("the purge of %s left %d markers naming it, want one", name, marked)
	}
	return id
}

// refusing is a publisher that refuses every write with err.
type refusing struct{ err error }

func (r refusing) Publish(context.Context, statelog.Request) (statelog.Result, error) {
	return statelog.Result{}, r.err
}

// A REFUSAL BEFORE THE APPEND IS NOT SERVING: a refusal about this node moves
// the push to another node with the same id; one about every node, or a
// record of the batch that landed and applies nowhere, is the push's answer.
//
// EVERY SHAPE IS ONE THE PUBLISHER FORMS, positions included: `behind` names
// the point it needs this node to have applied, `deferred` the record this
// node cannot decode, `skew` the sequence the broker answered and `op_reused`
// the record the id already names — none of them a record of this push, so a
// committer that read a position as "landed" told the client its push was
// durable and lost when nothing was sent and the runner-up could commit it.
//
// AND A GATE'S REFUSAL AT ANOTHER OPERATION'S RECORD: the batch's answer was
// lost and the newest record on the repository's subject was somebody else's,
// at which this node's eviction — or the repository's marker — holds it. The
// record there may be a healthy peer's, and the batch's own append may never
// have landed: an eviction there is about this node, and the runner-up
// commits the push under the same id (no node holds a row of it); a marker
// there is the repository's deletion, naming no record of the push.
//
// A MARKER'S REFUSAL NAMES THE REPOSITORY BY THE MARKER'S NAME: the marker
// cases run against a repository a retention pass purged, so the marker is
// the one the applier wrote, and a client is told "repository web was
// purged" — the sentence the decide forms for the same state — rather than an
// id it never typed; only where this node holds no marker to read (its
// estate replaced since the gate read one) does the id name it. Mutation:
// drop the committer's read of the marker and the three cases with one name
// the id and fail.
func TestARefusalBeforeTheAppendIsNotServing(t *testing.T) {
	t.Parallel()
	at := statelog.Position{Stream: code.Domain{}.Stream().Name, Generation: 1, Seq: 42}
	none := statelog.Position{}
	refusal := func(r statelog.Reason, p statelog.Position) error {
		return &statelog.Unavailable{Reason: r, Position: p}
	}
	// dropped is a gate's refusal of the BATCH'S OWN record at p, as the
	// publisher's resolution forms it when the record there carries the
	// batch's operation.
	dropped := func(r statelog.Reason, p statelog.Position) error {
		return &statelog.Unavailable{Reason: r, Position: p, Dropped: true}
	}
	for name, tc := range map[string]struct {
		err        error
		notServing bool
		refused    code.Refusal
		position   statelog.Position
		// purged runs the case against a repository whose purge completed,
		// its deletion marker written by the applier.
		purged bool
	}{
		"behind the point the write needs":   {err: refusal(statelog.ReasonBehind, at), notServing: true},
		"holding a record it cannot decode":  {err: refusal(statelog.ReasonDeferred, at), notServing: true},
		"a store and a stream out of step":   {err: refusal(statelog.ReasonSkew, at), notServing: true},
		"below the trim floor":               {err: refusal(statelog.ReasonBelowFloor, none), notServing: true},
		"evicted before the append":          {err: refusal(statelog.ReasonEvicted, none), notServing: true},
		"a full log":                         {err: refusal(statelog.ReasonLogFull, none), refused: code.RefusalUnavailable},
		"an operation id that names another": {err: refusal(statelog.ReasonOpReused, at), refused: code.RefusalUnavailable},
		"evicted after the append landed": {err: dropped(statelog.ReasonEvicted, at),
			refused: code.RefusalNotApplied, position: at},
		"written in an abandoned generation": {err: dropped(statelog.ReasonAbandoned, at),
			refused: code.RefusalNotApplied, position: at},
		"evicted at another operation's record": {err: refusal(statelog.ReasonEvicted, at),
			notServing: true},
		"a purged repository": {err: refusal(statelog.ReasonDeleted, none),
			refused: code.RefusalRepoDeleted, purged: true},
		"a purged repository's marker dropped the record": {err: dropped(statelog.ReasonDeleted, at),
			refused: code.RefusalRepoDeleted, position: at, purged: true},
		"a purged repository's marker at another operation's record": {
			err: refusal(statelog.ReasonDeleted, at), refused: code.RefusalRepoDeleted, purged: true},
		"a marker this node no longer holds": {err: refusal(statelog.ReasonDeleted, none),
			refused: code.RefusalRepoDeleted},
		"a lost race": {err: fmt.Errorf("publish: %w", statelog.ErrConflict), refused: code.RefusalBusy},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			n := newNode(t)
			repo := "0190f3c4-0000-7000-8000-00000000c0de"
			if tc.purged {
				repo = purgedRepo(t, n, "web")
			}
			c, _ := committerOn(t, n, refusing{tc.err})
			res, err := c.Propose(t.Context(), proposal(repo,
				update("refs/heads/main", code.ZeroOID, oidOf("B"))))
			if tc.notServing {
				var refusal *statelog.Unavailable
				if !errors.Is(err, code.ErrNotServing) || !errors.As(err, &refusal) {
					t.Fatalf("Propose: %+v, %v; want ErrNotServing wrapping the refusal", res, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Propose: %v; want an answer", err)
			}
			if res.Answer.Refused != tc.refused || res.Answer.Commands[0].Reason != tc.refused {
				t.Fatalf("answered %+v, want refused %s", res.Answer, tc.refused)
			}
			requireOnePlacement(t, res.Answer)
			if res.Position != tc.position {
				t.Fatalf("answered at %v, want %v", res.Position, tc.position)
			}
			if tc.refused == code.RefusalNotApplied &&
				!strings.Contains(res.Answer.Detail, tc.position.String()) {
				t.Fatalf("the detail %q does not name where the record landed", res.Answer.Detail)
			}
			// A DELETION MARKER IS A PURGE THAT COMPLETED, which nothing
			// undoes: told to declare the repository again to restore it,
			// a client is told the one remedy false here — and told it of
			// the repository by the name the marker holds.
			named := "repository " + repo + " was purged"
			if tc.purged {
				named = "repository web was purged"
			}
			if tc.refused == code.RefusalRepoDeleted && (!strings.Contains(res.Answer.Detail, named) ||
				strings.Contains(res.Answer.Detail, "restores")) {
				t.Fatalf("the detail %q does not say %q for good", res.Answer.Detail, named)
			}
		})
	}
}

// THE LOG REFUSING THIS NODE IS NOT SERVING, through the shipped publisher: a
// node whose applier is held behind a peer's record on the repository's
// subject, and one holding a record on the repository's scope it cannot
// decode, each refuse before anything is appended — WITH a position — and the
// push goes to the runner-up rather than being answered lost.
func TestTheLogRefusingThisNodeIsNotServing(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		reason statelog.Reason
		settle func(t *testing.T, a, b *node, repo string)
	}{
		"behind a peer's record": {statelog.ReasonBehind, func(t *testing.T, a, b *node, repo string) {
			b.drain()
			a.hold()
			b.push(repo, entry(uuid.Must(uuid.NewV7()).String(),
				set("refs/heads/main", oidOf("A"), oidOf("P"))))
		}},
		"holding a record it cannot decode": {statelog.ReasonDeferred, func(t *testing.T, a, _ *node, repo string) {
			a.exec(`INSERT INTO code_log_deferred (position, subject, subject_kind, subject_id,
				version, payload, stored_at) VALUES (999999, ?, 'repo', ?, 9, x'7b7d', 0)`,
				"crewlet.code.log.repo."+repo, repo)
			a.exec(`INSERT INTO code_log_deferred_scope (position, path) VALUES (999999, ?)`,
				code.RepoPath(repo))
		}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			a, b := newTwoNodes(t)
			repo := repoOn(t, a, "web")
			tc.settle(t, a, b, repo)
			before := a.end()
			c, _ := committerOn(t, a, a.pub)
			p := proposal(repo, update("refs/heads/main", oidOf("A"), oidOf("B")))
			res, err := c.Propose(t.Context(), p)
			var refusal *statelog.Unavailable
			switch {
			case !errors.Is(err, code.ErrNotServing) || !errors.As(err, &refusal):
				t.Fatalf("Propose: %+v, %v; want ErrNotServing wrapping the log's refusal", res, err)
			case refusal.Reason != tc.reason || refusal.Position.IsZero():
				t.Fatalf("the log refused %s at %v; want %s naming a position — the shape this "+
					"case is about", refusal.Reason, refusal.Position, tc.reason)
			}
			if got := a.end(); got != before {
				t.Fatalf("the log moved from %d to %d: the refusal came before the append", before, got)
			}
			if got := a.count(`SELECT COUNT(*) FROM code_pushes WHERE push_id = ?`, p.PushID); got != 0 {
				t.Fatalf("the push has %d rows on the refusing node", got)
			}
		})
	}
}

// A STOPPING COMMITTER ANSWERS NOT SERVING: the batch in flight finishes and
// is answered, the queued proposals are handed back, a new one is refused,
// and no goroutine is left.
func TestAStoppingCommitterAnswersNotServing(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo := repoOn(t, n, "web")
	gate := newGate(n.pub)
	c, _ := committerOn(t, n, gate)
	lead := proposeAsync(t.Context(), c, proposal(repo, update("refs/heads/lead", code.ZeroOID, oidOf("L"))))
	eventually(t, "the first publish reaching the gate", func() bool { return gate.calls.Load() == 1 })
	var queued []<-chan answer
	for i := range 2 {
		queued = append(queued, proposeAsync(t.Context(), c, proposal(repo,
			update(fmt.Sprintf("refs/heads/q%d", i), code.ZeroOID, oidOf(fmt.Sprint(i))))))
		want := i + 1
		eventually(t, "the proposal queued", func() bool { return code.CommitterQueued(c, repo) == want })
	}

	stopped := make(chan struct{})
	go func() {
		c.Stop(t.Context())
		close(stopped)
	}()
	// WAITED FOR, NOT PROBED: a proposal sent before Stop is called queues
	// behind the held batch and waits for an answer only the gate releases.
	eventually(t, "Stop to be called", func() bool { return code.CommitterStopping(c) })
	if _, err := c.Propose(t.Context(), proposal(repo,
		update("refs/heads/late", code.ZeroOID, oidOf("Z")))); !errors.Is(err, code.ErrNotServing) {
		t.Fatalf("a proposal to a stopping committer: %v; want ErrNotServing", err)
	}
	gate.open()
	requireOK(t, await(t, lead))
	for _, ch := range queued {
		if a := await(t, ch); !errors.Is(a.err, code.ErrNotServing) {
			t.Fatalf("a queued proposal answered %+v, %v; want ErrNotServing", a.res, a.err)
		}
	}
	<-stopped
	if got := code.CommitterRepos(c); got != 0 {
		t.Fatalf("%d repositories still held after Stop", got)
	}
}

// A PUSH A STOPPING COMMITTER HANDED BACK IS ANSWERED NOT SERVING, even when
// its caller's context ends before the caller reads the answer.
//
// That is the order a node stops in: the committer first, which hands every
// queued push back before its Stop returns, then the estate's serving
// registration, whose withdrawal cancels the context of every answer in
// flight — so by the time a caller's goroutine runs, its answer and its
// cancellation are both there. A select between them picks at random, and the
// cancellation won half the time: the refusal that sends the push to its
// repository's runner-up under the same push id was answered `context
// canceled` instead, a final failure for a push nothing decided. Run many
// times, because a coin that lands right once proves nothing.
func TestAPushHandedBackIsAnsweredWhenItsCallerHasGone(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	c, _ := committerOn(t, n, n.pub)
	refused := fmt.Errorf("%w: the committer stopped before deciding the push", code.ErrNotServing)
	for range 64 {
		if res, err := code.AwaitAnsweredAndGone(c, refused); !errors.Is(err, code.ErrNotServing) {
			t.Fatalf("a push handed back while its caller's context ended answered %+v, %v; "+
				"want the refusal it was handed back with", res, err)
		}
	}
}

// AN IDLE REPOSITORY'S GOROUTINE EXITS, and a push after that starts another.
func TestAnIdleRepositoryGoroutineExits(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo := repoOn(t, n, "web")
	c, _ := committerOn(t, n, n.pub)
	code.SetCommitterIdle(c, 50*time.Millisecond)
	res, err := c.Propose(t.Context(), proposal(repo, update("refs/heads/one", code.ZeroOID, oidOf("1"))))
	requireOK(t, answer{res, err})
	eventually(t, "the idle goroutine exiting", func() bool { return code.CommitterRepos(c) == 0 })
	res, err = c.Propose(t.Context(), proposal(repo, update("refs/heads/two", code.ZeroOID, oidOf("2"))))
	requireOK(t, answer{res, err})
}

// A RECORD STAYS UNDER THE ROW BUDGET: four proposals of MaxCommandsPerPush
// creates queued behind one in flight are two records — three pushes, then
// one — each applied within MaxRecordRows and MaxRecordBytes.
func TestARecordStaysUnderTheRowBudget(t *testing.T) {
	t.Parallel()
	n := newNode(t)
	repo := repoOn(t, n, "web")
	var mu sync.Mutex
	rows := map[uint64]int{}
	n.applied = func(seq uint64, applied int) {
		mu.Lock()
		rows[seq] = applied
		mu.Unlock()
	}
	gate := newGate(n.pub)
	c, _ := committerOn(t, n, gate)
	before := n.end()
	lead := proposeAsync(t.Context(), c, proposal(repo, update("refs/heads/lead", code.ZeroOID, oidOf("L"))))
	eventually(t, "the first publish reaching the gate", func() bool { return gate.calls.Load() == 1 })
	var queued []<-chan answer
	for i := range 4 {
		var cmds []code.Command
		for k := range code.MaxCommandsPerPush {
			cmds = append(cmds, update(fmt.Sprintf("refs/heads/p%d/%04d", i, k), code.ZeroOID,
				oidOf(fmt.Sprint(i, k))))
		}
		queued = append(queued, proposeAsync(t.Context(), c, proposal(repo, cmds...)))
		want := i + 1
		eventually(t, "the proposal queued", func() bool { return code.CommitterQueued(c, repo) == want })
	}
	gate.open()
	requireOK(t, await(t, lead))
	for _, ch := range queued {
		requireOK(t, await(t, ch))
	}
	last := n.end()
	if got := last - before; got != 3 {
		t.Fatalf("%d records, want 3: the lead, three maximal pushes, then the fourth", got)
	}
	for seq := before + 2; seq <= last; seq++ {
		_, payload, _, _, err := n.records.At(t.Context(), seq)
		if err != nil {
			t.Fatal(err)
		}
		rec, err := code.Decode(payload)
		if err != nil {
			t.Fatal(err)
		}
		batch, err := code.DecodeMutation(rec)
		if err != nil {
			t.Fatal(err)
		}
		pushes := len(batch.(code.PushBatch).Pushes)
		mu.Lock()
		applied := rows[seq]
		mu.Unlock()
		switch {
		case pushes > 3:
			t.Errorf("record %d carries %d maximal pushes, past the three a record's rows hold", seq, pushes)
		case applied > code.MaxRecordRows:
			t.Errorf("record %d applied %d rows, past MaxRecordRows (%d)", seq, applied, code.MaxRecordRows)
		case len(payload) > code.MaxRecordBytes:
			t.Errorf("record %d is %d bytes, past MaxRecordBytes (%d)", seq, len(payload), code.MaxRecordBytes)
		}
	}
}
