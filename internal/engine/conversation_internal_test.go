package engine

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/period"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
)

// headLog is ONE executor log as these cases play it: the head a turn's dispatch
// reads off it, the rows its launches restore from and record on, and every
// object's archive. It keeps the domain's rules these cases turn on and no
// others — a turn's first landed record keeps its read, a begin holds its base
// where the base's row still names the session the read saw, a live row that
// landed no capture holds its base against an end's absorption, and only a turn
// that landed a capture absorbs what its read handed out — which the domain's
// own suite holds on the real log (internal/executordomain's
// TestNoReleaseTakesAHeldBase).
type headLog struct {
	mu       sync.Mutex
	rows     map[string]executordomain.Session
	tombs    map[string]bool
	archives map[string][]byte
	seq      uint64

	// holdNothing answers every begin unheld and records no base on its row,
	// as a log does where the base moved before the begin landed.
	holdNothing bool

	// headErr, where set, is what every head read answers in place of the
	// head: a log the estate router could not reach, or a copy the read
	// ladder refused.
	headErr error

	heads  int
	begins []executordomain.NewBegin
}

var (
	_ headReader             = (*headLog)(nil)
	_ sandbox.SessionWrites  = (*headLog)(nil)
	_ sandbox.SessionRestore = (*headLog)(nil)
)

func newHeadLog() *headLog {
	return &headLog{rows: map[string]executordomain.Session{}, tombs: map[string]bool{},
		archives: map[string][]byte{}}
}

func (l *headLog) nextLocked() statelog.Position {
	l.seq++
	return statelog.Position{Stream: executordomain.Domain{}.Stream().Name, Seq: l.seq}
}

// endedTurn seeds an ended turn of seat swe's conversation `chat:D1` whose row
// names a session sessionID holding body, whose last request carried
// contextTokens, its reviewer's account of it landed.
func (l *headLog) endedTurn(turn, sessionID, body string, contextTokens int64) executordomain.Session {
	l.mu.Lock()
	defer l.mu.Unlock()
	key := objstore.KeyAt(time.Now())
	row := executordomain.Session{Handle: "swe", Turn: turn, ConversationKey: "chat:D1", WorkKey: "wk-" + turn,
		Harness: "claude-code", HarnessVersion: "2.1.310", SessionID: sessionID,
		Object:     objstore.Object{Key: key, Hash: objstore.Hash(strings.Repeat("b", 64)), Size: int64(len(body))},
		ContentKey: "k1", ArchiveFormat: sandbox.FakeArchiveFormat, LaunchSeq: 1, CaptureSeq: 1,
		Home: "/boxes/" + turn + "/home", ContextTokens: contextTokens,
		AskExcerpt: "what " + turn + " was asked",
		Ending: &executordomain.Ending{Outcome: executordomain.OutcomeCompleted, Delivered: true,
			DeliveredTo: "slack", CompletedWork: "what " + turn + " landed"}}
	row.EndedAt, row.EndedAtPosition = time.Now().UTC(), l.nextLocked()
	l.rows[turn], l.tombs[turn] = row, true
	l.archives[key.String()] = []byte(body)
	return row
}

// endingOnly seeds an ended turn of the conversation that kept no session, its
// reviewer's account of what landed being account.
func (l *headLog) endingOnly(turn, account string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	row := executordomain.Session{Handle: "swe", Turn: turn, ConversationKey: "chat:D1", WorkKey: "wk-" + turn,
		Ending: &executordomain.Ending{Outcome: executordomain.OutcomeCompleted, CompletedWork: account}}
	row.EndedAt, row.EndedAtPosition = time.Now().UTC(), l.nextLocked()
	l.rows[turn], l.tombs[turn] = row, true
}

func (l *headLog) Head(_ context.Context, handle, conversationKey string,
	fresh statelog.Freshness) (executordomain.Head, error) {

	l.mu.Lock()
	defer l.mu.Unlock()
	l.heads++
	if l.headErr != nil {
		return executordomain.Head{}, l.headErr
	}
	if fresh.Level != statelog.ReadLinearizable {
		return executordomain.Head{}, errors.New("a head read below the level another node's end is seen at")
	}
	var ended []executordomain.Session
	for _, row := range l.rows {
		if row.Handle == handle && row.ConversationKey == conversationKey && row.Ended() {
			ended = append(ended, row)
		}
	}
	slices.SortFunc(ended, func(a, b executordomain.Session) int {
		return cmp.Compare(a.EndedAtPosition.Seq, b.EndedAtPosition.Seq)
	})
	var out executordomain.Head
	for i := len(ended) - 1; i >= 0; i-- {
		if ended[i].HasObject() {
			head := ended[i]
			out.Session = &head
			ended = slices.Delete(ended, i, i+1)
			break
		}
	}
	if len(ended) > executordomain.MaxEndings {
		out.EndingsOmitted = len(ended) - executordomain.MaxEndings
		ended = ended[len(ended)-executordomain.MaxEndings:]
	}
	out.Endings = ended
	for i := len(ended) - 1; i >= 0 && len(out.Siblings) < executordomain.MaxSiblings; i-- {
		if ended[i].HasObject() {
			out.Siblings = append(out.Siblings, ended[i])
		}
	}
	slices.Reverse(out.Siblings)
	out.Position = statelog.Position{Stream: executordomain.Domain{}.Stream().Name, Seq: l.seq}
	return out, nil
}

func (l *headLog) Begin(_ context.Context, _ executordomain.Call,
	n executordomain.NewBegin) (executordomain.BeginResult, error) {

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.tombs[n.Turn] {
		return executordomain.BeginResult{}, fmt.Errorf("%w: %s", executordomain.ErrTurnEnded, n.Turn)
	}
	l.begins = append(l.begins, n)
	row, found := l.rows[n.Turn]
	if !found {
		row = executordomain.Session{Handle: n.Handle, Turn: n.Turn, ConversationKey: n.ConversationKey,
			WorkKey: n.WorkKey}
	}
	if row.ReadThrough.Seq == 0 {
		row.ReadThrough, row.Absorbs = n.ReadThrough, slices.Clone(n.Absorbs)
		if base, ok := l.rows[n.BaseTurn]; ok && !l.holdNothing && n.BaseTurn != "" && base.Ended() &&
			base.Object.Key == n.BaseObject.Key && base.Object.Hash == n.BaseObject.Hash {
			row.BaseTurn = n.BaseTurn
		}
	}
	l.rows[n.Turn] = row
	held := n.BaseTurn != "" && row.BaseTurn == n.BaseTurn && row.LaunchSeq == 0
	return executordomain.BeginResult{Result: statelog.Result{Outcome: statelog.OutcomeApplied}, Held: held}, nil
}

func (l *headLog) Capture(_ context.Context, _ executordomain.Call,
	n executordomain.NewCapture) (executordomain.CaptureResult, error) {

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.tombs[n.Turn] {
		return executordomain.CaptureResult{}, fmt.Errorf("%w: %s", executordomain.ErrTurnEnded, n.Turn)
	}
	row, found := l.rows[n.Turn]
	if !found {
		row = executordomain.Session{Handle: n.Handle, Turn: n.Turn, ConversationKey: n.ConversationKey,
			WorkKey: n.WorkKey}
	}
	if row.ReadThrough.Seq == 0 && n.ReadThrough.Seq > 0 {
		row.ReadThrough, row.Absorbs = n.ReadThrough, slices.Clone(n.Absorbs)
	}
	row.Harness, row.SessionID, row.Object = n.Harness, n.SessionID, n.Object
	row.ArchiveFormat, row.LaunchSeq, row.CaptureSeq = n.ArchiveFormat, n.LaunchSeq, n.CaptureSeq
	l.rows[n.Turn] = row
	return executordomain.CaptureResult{Result: statelog.Result{Outcome: statelog.OutcomeApplied},
		Recorded: true}, nil
}

func (l *headLog) End(_ context.Context, _ executordomain.Call,
	n executordomain.NewEnd) (statelog.Result, error) {

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.tombs[n.Turn] {
		return statelog.Result{}, fmt.Errorf("%w: %s", executordomain.ErrTurnEnded, n.Turn)
	}
	row, found := l.rows[n.Turn]
	if !found {
		row = executordomain.Session{Handle: n.Handle, Turn: n.Turn, ConversationKey: n.ConversationKey,
			WorkKey: n.WorkKey}
	}
	ending := n.Ending
	row.Ending, row.EndedAt, row.EndedAtPosition = &ending, time.Now().UTC(), l.nextLocked()
	l.rows[n.Turn], l.tombs[n.Turn] = row, true
	if row.LaunchSeq > 0 {
		for _, turn := range row.Absorbs {
			if absorbed, ok := l.rows[turn]; ok && absorbed.Ended() && !l.heldLocked(turn) {
				delete(l.rows, turn)
			}
		}
	}
	return statelog.Result{Outcome: statelog.OutcomeApplied}, nil
}

func (l *headLog) heldLocked(turn string) bool {
	for _, row := range l.rows {
		if row.BaseTurn == turn && !row.Ended() && row.LaunchSeq == 0 {
			return true
		}
	}
	return false
}

func (l *headLog) Session(_ context.Context, handle, turn string,
	fresh statelog.Freshness) (executordomain.Session, error) {

	l.mu.Lock()
	defer l.mu.Unlock()
	if fresh.Level != statelog.ReadLinearizable {
		return executordomain.Session{}, errors.New("a row read below the level another node's record is seen at")
	}
	row, ok := l.rows[turn]
	if !ok || row.Handle != handle {
		return executordomain.Session{}, executordomain.ErrNotFound
	}
	return row, nil
}

func (l *headLog) Fetch(_ context.Context, row executordomain.Session, dst io.Writer) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	body, ok := l.archives[row.Object.Key.String()]
	if !ok {
		return executordomain.ErrUnreadable
	}
	_, err := dst.Write(body)
	return err
}

func (l *headLog) OpenSpooled(_ executordomain.Session, sealed io.Reader) (io.Reader, error) {
	return sealed, nil
}

// headReads is how many times a head was read off the log.
func (l *headLog) headReads() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.heads
}

// begun is every begin recorded for turn.
func (l *headLog) begun(turn string) []executordomain.NewBegin {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []executordomain.NewBegin
	for _, n := range l.begins {
		if n.Turn == turn {
			out = append(out, n)
		}
	}
	return out
}

// row is turn's row, and whether the log holds one.
func (l *headLog) row(turn string) (executordomain.Session, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	row, ok := l.rows[turn]
	return row, ok
}

// forget drops the archive of turn's object, as a store that no longer holds it.
func (l *headLog) forget(turn string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.archives, l.rows[turn].Object.Key.String())
}

// conversingKeep is a node's keep as these cases need it: sessions kept, every
// turn's operations minted at one instant, and no session ever taken, since no
// case here collects a run.
type conversingKeep struct{}

func (conversingKeep) Policy(context.Context) (sandbox.SessionPolicy, error) {
	return sandbox.SessionPolicy{Keep: true, SeatBudget: 64 << 20}, nil
}

func (conversingKeep) Keeps() bool { return true }

func (conversingKeep) SealHanded(turnID string, handed []byte) (string, error) {
	return turnID + "|" + string(handed), nil
}

func (conversingKeep) OpenHanded(turnID, sealed string) ([]byte, error) {
	body, ok := strings.CutPrefix(sealed, turnID+"|")
	if !ok {
		return nil, errors.New("sealed for another turn")
	}
	return []byte(body), nil
}

func (conversingKeep) MintedAt(context.Context, string, string, time.Time) (time.Time, error) {
	return time.Date(2026, 8, 23, 11, 0, 0, 0, time.UTC), nil
}

func (conversingKeep) Seal(string, string, io.Reader, io.Writer) (executordomain.SealedSpool, error) {
	return executordomain.SealedSpool{}, errors.New("no case here keeps a session")
}

func (conversingKeep) Upload(context.Context, executordomain.SealedSpool,
	io.Reader) (executordomain.SealedObject, error) {
	return executordomain.SealedObject{}, errors.New("no case here keeps a session")
}

// conversingLaunch is a seat whose turns serve the conversation `chat:D1`, on
// an engine whose dispatch reads that conversation's head off one log, whose
// launches record on it and continue sessions from it, and the double's runner
// every launch starts its job on.
type conversingLaunch struct {
	e    *Engine
	c    *Company
	seat *org.Role
	log  *headLog
	jobs *sandbox.FakeRunner
	ctx  context.Context
}

// newConversingLaunch is the seat's engine, its day capped at ceiling (zero
// caps nothing) with spent already counted.
func newConversingLaunch(t *testing.T, ceiling, spent int) conversingLaunch {
	t.Helper()
	c, seat := executorCompany(t)
	if ceiling > 0 {
		seat.TokenBudget = org.TokenCeilings{period.Day: ceiling}
	}
	jobs := sandbox.NewFakeRunner("claude-code")
	jobs.Carries = true
	providers := map[sandbox.Placement]sandbox.Provider{}
	for _, p := range sandbox.Placements {
		providers[p] = sandbox.NewFakeProvider()
	}
	manager, err := sandbox.NewManager(sandbox.ManagerOptions{
		Providers: providers, DefaultPlacement: sandbox.Direct,
		Runners: map[string]sandbox.Runner{"claude-code": jobs},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	bridge := bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
	})
	_ = bridge.Handler()
	q := memory.New()
	fleet := coordmemory.NewFleet()
	pending := sandbox.NewCoordStore(fleet)
	heads := newHeadLog()
	e := &Engine{backends: &Backends{Queue: q, Fleet: fleet}, bridge: bridge}
	coordinator, err := sandbox.NewCoordinator(sandbox.CoordinatorOptions{
		Answers: testAnswers(), Audience: noAudience{},
		Queue: q, Pending: pending, Manager: manager, Resume: &resumer{engine: e},
		Restore: heads, Sessions: heads, Keep: conversingKeep{},
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	t.Cleanup(coordinator.Stop)
	e.useSandbox(pending, coordinator)
	e.executorHeads = heads
	c.catalogue = manager
	e.epoch.current.Store(c)
	ctx := t.Context()
	if spent > 0 {
		id, ok := c.Org.AgentIDFor(seat)
		if !ok {
			t.Fatal("the seat has no agent id")
		}
		if _, err := fleet.PostCharge(ctx, coord.AgentScope(id.String()), spent,
			coord.WindowsAt(time.Now(), c.Config.Location())); err != nil {
			t.Fatalf("PostCharge: %v", err)
		}
	}
	return conversingLaunch{e: e, c: c, seat: seat, log: heads, jobs: jobs, ctx: ctx}
}

// read is what turnID's dispatch reads of the conversation it serves.
func (k conversingLaunch) read(turnID string) *conversationRead {
	return k.e.readConversation(k.ctx, k.c, Request{Handle: k.seat.Handle(), RunID: turnID,
		ConversationKey: "chat:D1"}, provenance.NewBoundary())
}

// launcher is turnID's launcher: a dispatched segment's, handed what its
// dispatch read, or a resumed segment's, handed nil.
func (k conversingLaunch) launcher(turnID string, read *conversationRead) *agentLauncher {
	l := launcherOn(k.e, k.c, k.seat, turnID)
	l.turn.ConversationKey, l.turn.WorkKey = "chat:D1", "wk-"+turnID
	l.conversation = read
	return l
}

// freshBrief is the brief a turn's runner writes for a run of its own: how the
// conversation's earlier turns ended, then the task.
func freshBrief(endings string) string {
	return strings.TrimSpace(endings + "\n\n## Task\nfix the failing test")
}

// launch launches l's run continuing what basis names, with the turn's fresh
// brief — the endings in it — and the lead before it where basis names the
// conversation's session, answering how the launch decided.
func (k conversingLaunch) launch(t *testing.T, l *agentLauncher, basis runner.SessionBasis,
	endings string) ([]runner.SessionDecision, error) {
	t.Helper()
	req := launchRequest(l, nil)
	req.Brief = freshBrief(endings)
	if basis.ContinuesBase() {
		req.Session = &runner.SessionContinuation{Basis: basis,
			Replaced: "## A new turn of this conversation begins here\n\n" + req.Brief}
	}
	var decided []runner.SessionDecision
	req.Decided = func(d runner.SessionDecision) { decided = append(decided, d) }
	_, err := l.LaunchExecutor(k.ctx, req)
	return decided, err
}

// lastStarted is the job the newest launch started.
func (k conversingLaunch) lastStarted(t *testing.T) sandbox.RunRequest {
	t.Helper()
	started := k.jobs.Started()
	if len(started) == 0 {
		t.Fatal("no job was started")
	}
	return started[len(started)-1]
}

// A TURN READS ITS CONVERSATION ONCE, at its dispatch. Its first launch is
// answered which session it continues from that read with no other, and every
// later launch — a relaunch of the turn, on a segment that resumed it — from
// what the first one froze: none of them reads the conversation's head again,
// so a sibling that ended meanwhile never moves the turn's base or its account.
//
// Mutation: read the head in the probe, the launch or a relaunch, and the count
// goes past one.
func TestATurnReadsItsConversationOnce(t *testing.T) {
	t.Parallel()
	k := newConversingLaunch(t, 0, 0)
	k.log.endedTurn("b1", "s-b1", "B1's transcript", 42000)
	read := k.read("t2")
	if read.why != "" || read.frozen() == nil || read.frozen().Session == nil || read.frozen().Session.Turn != "b1" {
		t.Fatalf("the dispatch read %+v, want the conversation's head b1", read)
	}
	first := k.launcher("t2", read)
	if _, err := k.launch(t, first, first.ProbeSession(k.ctx), read.endingsBlock()); err != nil {
		t.Fatalf("the first launch: %v", err)
	}
	relaunch := k.launcher("t2", nil)
	if _, err := k.launch(t, relaunch, relaunch.ProbeSession(k.ctx), read.endingsBlock()); err != nil {
		t.Fatalf("the relaunch: %v", err)
	}
	if heads := k.log.headReads(); heads != 1 {
		t.Fatalf("the turn read its conversation's head %d times across its dispatch, its first launch and a "+
			"relaunch, want once", heads)
	}
}

// THE FROZEN BASE OUTLIVES A SIBLING'S END, as the engine wires it. Turn t2's
// dispatch read b1, the conversation's head; its first launch continued b1 and
// recorded its begin, which holds it. Its sibling t3, which read b1 too, captured
// and ended — and b1 is still there, t2 holding it — and t3 is the head now. A
// relaunch of t2 from a resumed segment, which reads no conversation, is
// answered b1 — never t3 — carried to the sandbox layer as the conversation's
// session, and continued in a box of its own.
//
// Mutation: answer a relaunch from a head read now, freeze nothing on the
// record, or carry the base as a kept capture, and a check goes red.
func TestTheFrozenBaseOutlivesASiblingsEnd(t *testing.T) {
	t.Parallel()
	k := newConversingLaunch(t, 0, 0)
	b1 := k.log.endedTurn("b1", "s-b1", "B1's transcript", 42000)
	read := k.read("t2")
	first := k.launcher("t2", read)
	basis := first.ProbeSession(k.ctx)
	if !basis.ContinuesBase() || basis.Base.Turn != "b1" || basis.SessionID != "s-b1" ||
		basis.Base.ContextTokens != 42000 {
		t.Fatalf("the first launch is answered %+v, want b1, as the dispatch read it", basis)
	}
	if _, err := k.launch(t, first, basis, read.endingsBlock()); err != nil {
		t.Fatalf("the first launch: %v", err)
	}
	if begins := k.log.begun("t2"); len(begins) != 1 || begins[0].BaseTurn != "b1" {
		t.Fatalf("the first launch recorded begins %+v, want one holding b1", begins)
	}

	// THE SIBLING, which read b1 too, captures and ends.
	if _, err := k.log.Capture(k.ctx, executordomain.Call{}, executordomain.NewCapture{Handle: "swe",
		Turn: "t3", ConversationKey: "chat:D1", WorkKey: "wk-t3", Harness: "claude-code", SessionID: "s-t3",
		Object: objstore.Object{Key: objstore.KeyAt(time.Now()), Hash: objstore.Hash(strings.Repeat("d", 64)),
			Size: 3},
		ArchiveFormat: sandbox.FakeArchiveFormat, LaunchSeq: 1, CaptureSeq: 1,
		ReadThrough: statelog.Unpack(executordomain.Domain{}.Stream().Name, read.frozen().ReadThrough),
		Absorbs:     []string{"b1"}}); err != nil {
		t.Fatalf("t3's capture: %v", err)
	}
	if _, err := k.log.End(k.ctx, executordomain.Call{}, executordomain.NewEnd{Handle: "swe", Turn: "t3",
		ConversationKey: "chat:D1", WorkKey: "wk-t3",
		Ending: executordomain.Ending{Outcome: executordomain.OutcomeCompleted}}); err != nil {
		t.Fatalf("t3's end: %v", err)
	}
	if row, ok := k.log.row("b1"); !ok || row.Object.Key != b1.Object.Key {
		t.Fatalf("t3's end took b1, which t2 holds: %+v (%v)", row, ok)
	}

	relaunch := k.launcher("t2", nil)
	again := relaunch.ProbeSession(k.ctx)
	if !again.ContinuesBase() || again.Base.Turn != "b1" || again.SessionID != "s-b1" {
		t.Fatalf("the relaunch is answered %+v, want b1, the base the turn froze", again)
	}
	decided, err := k.launch(t, relaunch, again, read.endingsBlock())
	if err != nil {
		t.Fatalf("the relaunch: %v", err)
	}
	last := k.lastStarted(t)
	if last.Resume != "s-b1" || len(decided) != 1 || !decided[0].Base || !decided[0].NewBox {
		t.Fatalf("the relaunch started session %q, deciding %+v; want b1's, in a box of its own", last.Resume,
			decided)
	}
	if carried := k.jobs.Carried(); len(carried) != 2 || string(carried[1].Body) != "B1's transcript" {
		t.Fatalf("the relaunch's box was handed %+v, want b1's archive", carried)
	}
	if heads := k.log.headReads(); heads != 1 {
		t.Errorf("the turn read its conversation's head %d times, want once", heads)
	}
}

// EVERY FRESH FALLBACK IS TOLD THE FROZEN ENDINGS. A launch that cannot continue
// the conversation's session — a box whose harness cannot take it up, an object
// the store no longer holds, a session no window can carry, a begin the log did
// not hold — starts its run on the turn's fresh brief, and a run that continues
// it carries that brief for its script to hand a session of its own: each is
// told how the conversation's earlier turns ended, as the dispatch rendered it.
//
// Mutation: hand a fallback any brief but the turn's fresh one, or leave the
// endings out of the brief a continued run carries, and a row goes red.
func TestEveryFreshFallbackIsToldTheFrozenEndings(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		context   int64
		ceiling   int
		spoil     func(k conversingLaunch)
		continues bool
	}{
		"its box cannot take the session up": {context: 42000, spoil: func(k conversingLaunch) {
			k.jobs.CarryWhy = "this box runs Claude Code 2.1.263"
		}},
		"its object is unreadable": {context: 42000, spoil: func(k conversingLaunch) { k.log.forget("b1") }},
		"no window can carry it":   {context: 150000, ceiling: 100000},
		"its begin was not held":   {context: 42000, spoil: func(k conversingLaunch) { k.log.holdNothing = true }},
		"it is continued":          {context: 42000, continues: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			k := newConversingLaunch(t, tc.ceiling, 0)
			k.log.endedTurn("b1", "s-b1", "B1's transcript", tc.context)
			if tc.spoil != nil {
				tc.spoil(k)
			}
			read := k.read("t2")
			endings := read.endingsBlock()
			if !strings.Contains(endings, "### Turn b1") {
				t.Fatalf("the dispatch rendered %q, want b1's ending", endings)
			}
			l := k.launcher("t2", read)
			if _, err := k.launch(t, l, l.ProbeSession(k.ctx), endings); err != nil {
				t.Fatalf("the first launch: %v", err)
			}
			last := k.lastStarted(t)
			switch {
			case tc.continues && (last.Resume != "s-b1" || !strings.Contains(last.FreshBrief, endings)):
				t.Fatalf("a continued run started on %q carrying fresh brief %q, want b1's with the endings",
					last.Resume, last.FreshBrief)
			case !tc.continues && (last.Resume != "" || last.Brief != freshBrief(endings)):
				t.Fatalf("a fallback started on session %q with brief %q, want the turn's fresh brief, its "+
					"endings in it", last.Resume, last.Brief)
			}
		})
	}
}

// A CONVERSATION NO WINDOW CAN CARRY STARTS FRESH; ONE A WINDOW CAN IS
// CONTINUED. A turn's first launch that would continue its conversation's
// session weighs that session's context — the base's own, which its row
// recorded — with the brief that continues it against the smallest ceiling of
// the seat's capped windows: past it no window turning over ever leaves room,
// so the run starts a session of its own on the static floor; within it, the
// session is continued.
//
// Mutation: weigh the first launch's floor by anything but the base's context,
// or wait for a floor no window can meet, and a row goes red.
func TestAConversationNoWindowCanCarryStartsFresh(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		context   int64
		continues bool
	}{
		"past the smallest ceiling":   {context: 150000},
		"within the smallest ceiling": {context: 60000, continues: true},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			k := newConversingLaunch(t, 100000, 0)
			k.log.endedTurn("b1", "s-b1", "B1's transcript", tc.context)
			read := k.read("t2")
			l := k.launcher("t2", read)
			decided, err := k.launch(t, l, l.ProbeSession(k.ctx), read.endingsBlock())
			if err != nil {
				t.Fatalf("the first launch: %v", err)
			}
			last := k.lastStarted(t)
			if continued := last.Resume == "s-b1"; continued != tc.continues || len(decided) != 1 ||
				decided[0].Continues != tc.continues {
				t.Fatalf("the first launch started session %q, deciding %+v; want continuing %v", last.Resume,
					decided, tc.continues)
			}
		})
	}
}

// AN ENDING LEFT OUT OF THE BRIEF IS NOT ABSORBED. A conversation whose ended
// turns are more than the brief's bound tells the newest of them whole and
// counts the rest; the turn's first launch freezes, and records as its begin,
// only the turns the brief told — so the turn's end, which absorbs what its
// first read handed out, leaves the ones it was never told for the next turn,
// whose brief tells them.
//
// Mutation: freeze the whole read rather than the told part of it, and the
// end absorbs turns nobody was told.
func TestAnEndingLeftOutOfTheBriefIsNotAbsorbed(t *testing.T) {
	t.Parallel()
	k := newConversingLaunch(t, 0, 0)
	account := strings.Repeat("landed a fix. ", 4<<10/14)
	for i := range 8 {
		k.log.endingOnly(fmt.Sprintf("e%d", i), account)
	}
	k.log.endedTurn("b1", "s-b1", "B1's transcript", 42000)
	read := k.read("t2")
	frozen := read.frozen()
	if frozen == nil || !slices.Contains(frozen.Absorbs, "b1") || len(frozen.Absorbs) >= 9 {
		t.Fatalf("the dispatch froze %+v, want the head and only the endings its brief told", frozen)
	}
	if !sameTurns(frozen.Absorbs, read.told) {
		t.Fatalf("the read absorbs %v, want exactly the turns its brief told: %v", frozen.Absorbs, read.told)
	}
	left := slices.DeleteFunc([]string{"e0", "e1", "e2", "e3", "e4", "e5", "e6", "e7"}, func(turn string) bool {
		return slices.Contains(read.told, turn)
	})
	if len(left) == 0 || !strings.Contains(read.endingsBlock(), "are not shown here") {
		t.Fatalf("the brief told every ending (%v), so the case shows nothing", read.told)
	}
	l := k.launcher("t2", read)
	if _, err := k.launch(t, l, l.ProbeSession(k.ctx), read.endingsBlock()); err != nil {
		t.Fatalf("the first launch: %v", err)
	}
	if begins := k.log.begun("t2"); len(begins) != 1 || !sameTurns(begins[0].Absorbs, read.told) {
		t.Fatalf("the begin recorded %+v, want the told turns alone", begins)
	}
	// THE TURN CAPTURES AND ENDS: what it was told goes, what it was not stays.
	if _, err := k.log.Capture(k.ctx, executordomain.Call{}, executordomain.NewCapture{Handle: "swe",
		Turn: "t2", ConversationKey: "chat:D1", WorkKey: "wk-t2", Harness: "claude-code", SessionID: "s-t2",
		Object: objstore.Object{Key: objstore.KeyAt(time.Now()), Hash: objstore.Hash(strings.Repeat("e", 64)),
			Size: 3},
		ArchiveFormat: sandbox.FakeArchiveFormat, LaunchSeq: 1, CaptureSeq: 1}); err != nil {
		t.Fatalf("t2's capture: %v", err)
	}
	if _, err := k.log.End(k.ctx, executordomain.Call{}, executordomain.NewEnd{Handle: "swe", Turn: "t2",
		ConversationKey: "chat:D1", WorkKey: "wk-t2",
		Ending: executordomain.Ending{Outcome: executordomain.OutcomeCompleted}}); err != nil {
		t.Fatalf("t2's end: %v", err)
	}
	for _, turn := range left {
		if _, ok := k.log.row(turn); !ok {
			t.Errorf("t2's end absorbed %s, which its brief never told", turn)
		}
	}
	for _, turn := range read.told {
		if _, ok := k.log.row(turn); ok {
			t.Errorf("t2's end left %s, which its brief told", turn)
		}
	}
}

// sameTurns reports whether a and b name the same turns, in whatever order.
func sameTurns(a, b []string) bool {
	return slices.Equal(slices.Sorted(slices.Values(a)), slices.Sorted(slices.Values(b)))
}

// THE SWITCH OFF CARRIES NOTHING BETWEEN TURNS, and still records every end.
// `turn_engine.conversation_session.enabled: false` reads no head of the turn's
// conversation and renders no account of its earlier turns; the turn's first
// launch freezes no read, records no begin and continues no session — and the
// turn's end is recorded on the executor log all the same, which is the turn's
// account rather than its session: by the frame that ends a turn of a
// conversation refused at the delegation cap, and by the one that ends a turn
// whose first launch was abandoned once its run was recorded, each of a company
// with the switch off.
//
// Mutation: read the head with the switch off, or gate a turn's end in its
// frame on the switch, and a check goes red.
func TestTheSwitchOffCarriesNothingBetweenTurns(t *testing.T) {
	t.Parallel()
	k := newConversingLaunch(t, 0, 0)
	k.c.Config.TurnEngine.ConversationSession.Enabled = org.Off()
	k.log.endedTurn("b1", "s-b1", "B1's transcript", 42000)
	read := k.read("t2")
	if read.why != sandbox.NoKeptSession || read.endingsBlock() != "" || read.frozen() != nil ||
		k.log.headReads() != 0 {
		t.Fatalf("with the switch off the dispatch read %+v (%d head reads), want nothing, said",
			read, k.log.headReads())
	}
	l := k.launcher("t2", read)
	if basis := l.ProbeSession(k.ctx); basis.ContinuesBase() || basis.Continues() {
		t.Fatalf("with the switch off the first launch is answered %+v, want no session", basis)
	}
	if _, err := k.launch(t, l, runner.SessionBasis{}, ""); err != nil {
		t.Fatalf("the first launch: %v", err)
	}
	pending := k.e.sandbox.Load().pending
	if run, _, err := pending.Get(k.ctx, "t2"); err != nil || run.Base != nil {
		t.Fatalf("the record froze %+v (%v), want no read", run.Base, err)
	}
	if begins := k.log.begun("t2"); len(begins) != 0 || k.lastStarted(t).Resume != "" {
		t.Fatalf("with the switch off the launch recorded begins %+v and continued %q", begins,
			k.lastStarted(t).Resume)
	}

	capped, cappedLog := loggingEngine(t)
	capped.Company().Config.TurnEngine.ConversationSession.Enabled = org.Off()
	rigRuns(t, capped, halfwayPlay)
	res, err := capped.runTurn(k.ctx, Request{
		Handle: "swe", WorkKey: "wk-1", RunID: "run-1", WorkSince: time.Now().UTC(), Depth: 2,
		ConversationKey: "chat:D1", Events: []*events.Event{taskWake("task-9", "ENG-9")},
	})
	if err != nil || res.Suspended || res.Breach == nil || res.Breach.Kind != types.GuardDepthCap {
		t.Fatalf("runTurn = %+v, %v; want the turn refused at the delegation cap", res, err)
	}
	if ends := awaitEnds(t, cappedLog, "run-1", 1); len(ends) != 1 || ends[0].ConversationKey != "chat:D1" ||
		ends[0].Ending.Outcome != executordomain.OutcomeFailed {
		t.Fatalf("with the switch off a turn refused at the delegation cap ended %+v, want its one end, failed",
			ends)
	}

	abandoned, abandonedLog := loggingEngine(t)
	abandoned.Company().Config.TurnEngine.ConversationSession.Enabled = org.Off()
	rig := rigRuns(t, abandoned, halfwayPlay)
	rig.runners["claude-code"].StartErr = errors.New("the box refused to start its job")
	if _, err := abandoned.runTurn(k.ctx, Request{
		Handle: "swe", WorkKey: "wk-1", RunID: "run-1", WorkSince: time.Now().UTC(),
		ConversationKey: "chat:D1", Events: []*events.Event{taskWake("task-9", "ENG-9")},
	}); err == nil {
		t.Fatal("a turn whose job would not start ran")
	}
	if ends := awaitEnds(t, abandonedLog, "run-1", 1); len(ends) != 1 ||
		ends[0].Ending.Outcome != executordomain.OutcomeFailed {
		t.Fatalf("with the switch off a turn whose first launch was abandoned ended %+v, want its one end, "+
			"failed", ends)
	}
}

// A CONVERSATION THAT CANNOT BE READ IS LESS CONTEXT, NOT A FAILED TURN. A head
// read that fails — the estate router unable to reach a data node, a copy the
// read ladder refuses — fails open: the turn is told nothing of how its
// conversation's earlier turns ended, its first launch freezes no read, records
// no begin and continues no session, and it launches all the same, on its fresh
// brief, the probe saying why it continued nothing.
//
// Mutation: fail or panic on a head that cannot be read, or freeze a read
// nobody made, and a check goes red.
func TestAnUnreadableConversationIsLessContextNotAFailedTurn(t *testing.T) {
	t.Parallel()
	k := newConversingLaunch(t, 0, 0)
	k.log.endedTurn("b1", "s-b1", "B1's transcript", 42000)
	k.log.headErr = errors.New("no data node answered the estate router")
	read := k.read("t2")
	if read.endingsBlock() != "" || read.frozen() != nil || !strings.Contains(read.why, "could not be read") ||
		!strings.Contains(read.why, "no data node answered") {
		t.Fatalf("a head read that failed left %+v, want nothing read, saying why", read)
	}
	l := k.launcher("t2", read)
	basis := l.ProbeSession(k.ctx)
	if basis.ContinuesBase() || basis.Continues() || !strings.Contains(basis.Why, "could not be read") {
		t.Fatalf("the first launch is answered %+v, want no session, the failed read its reason", basis)
	}
	if _, err := k.launch(t, l, basis, read.endingsBlock()); err != nil {
		t.Fatalf("a turn whose conversation could not be read did not launch: %v", err)
	}
	pending := k.e.sandbox.Load().pending
	if run, _, err := pending.Get(k.ctx, "t2"); err != nil || run.Base != nil {
		t.Fatalf("the record froze %+v (%v), want no read", run.Base, err)
	}
	last := k.lastStarted(t)
	if begins := k.log.begun("t2"); len(begins) != 0 || last.Resume != "" || last.Brief != freshBrief("") {
		t.Fatalf("the launch recorded begins %+v and started session %q on brief %q, want none, on the "+
			"fresh brief", begins, last.Resume, last.Brief)
	}
}

// A TURN THAT NEVER LAUNCHED TELLS THE NEXT TURN HOW IT ENDED. A turn refused
// before its first round — at the delegation cap — ends in its own frame with
// no run, and its end is recorded on the executor log under its conversation:
// failed, saying which guard, so the conversation's next turn is told how it
// ended, as of every turn of a conversation.
//
// Mutation: record an end only for a turn that launched, and this one ends
// nowhere.
func TestATurnThatNeverLaunchedTellsTheNextTurnHowItEnded(t *testing.T) {
	t.Parallel()
	e, spy := loggingEngine(t)
	rigRuns(t, e, halfwayPlay)
	res, err := e.runTurn(t.Context(), Request{
		Handle: "swe", WorkKey: "wk-1", RunID: "run-1", WorkSince: time.Now().UTC(), Depth: 2,
		ConversationKey: "chat:D1", Events: []*events.Event{taskWake("task-9", "ENG-9")},
	})
	if err != nil || res.Suspended || res.Breach == nil || res.Breach.Kind != types.GuardDepthCap {
		t.Fatalf("runTurn = %+v, %v; want the turn refused at the delegation cap", res, err)
	}
	ends := awaitEnds(t, spy, "run-1", 1)
	if len(ends) != 1 {
		t.Fatalf("recorded %d ends of the turn, want 1", len(ends))
	}
	if end := ends[0]; end.ConversationKey != "chat:D1" || end.Ending.Outcome != executordomain.OutcomeFailed ||
		!strings.Contains(end.Ending.Failure, string(types.GuardDepthCap)) {
		t.Fatalf("the end is %+v, want the conversation's turn failed at the delegation cap", end)
	}
}

// AN ABANDONED FIRST LAUNCH ENDS ITS TURN ON THE LOG. A first launch whose job
// would not start once its run's record was written abandons that record, so no
// ending of the run owes the turn's end: the frame that ends the turn records it
// — failed, saying the launch was abandoned, and redacted, since the launch's
// error can quote what a box said — which lifts the hold the turn's begin took
// and tells the conversation's next turn how this one ended.
//
// Mutation: record an abandoned launch's end only where a stop abandoned it, or
// quote its error unredacted, and a check goes red.
func TestAnAbandonedFirstLaunchEndsItsTurnOnTheLog(t *testing.T) {
	t.Parallel()
	e, spy := loggingEngine(t)
	rig := rigRuns(t, e, halfwayPlay)
	rig.runners["claude-code"].StartErr = errors.New("the box refused to start its job: " + leakedToken)
	if _, err := e.runTurn(t.Context(), Request{
		Handle: "swe", WorkKey: "wk-1", RunID: "run-1", WorkSince: time.Now().UTC(),
		ConversationKey: "chat:D1", Events: []*events.Event{taskWake("task-9", "ENG-9")},
	}); err == nil {
		t.Fatal("a turn whose job would not start ran")
	}
	ends := awaitEnds(t, spy, "run-1", 1)
	if len(ends) != 1 {
		t.Fatalf("recorded %d ends of the turn, want 1", len(ends))
	}
	failure := ends[0].Ending.Failure
	if ends[0].Ending.Outcome != executordomain.OutcomeFailed ||
		!strings.Contains(failure, "abandoned after its run was recorded") ||
		strings.Contains(failure, leakedToken) {
		t.Fatalf("the end is %+v, want the turn failed on its abandoned launch, redacted", ends[0].Ending)
	}
}
