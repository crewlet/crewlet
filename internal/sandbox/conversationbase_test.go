package sandbox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// conversationLog is ONE executor log as the cases below play it: every node's
// coordinator records on it and continues sessions from it, over the rows of a
// seat's conversation — each turn's row by its turn, the ended ones
// tombstoned, and every object's archive.
//
// IT KEEPS THE DOMAIN'S RULES THESE CASES TURN ON, and no others: a turn's
// first landed record keeps its read; a begin holds its base where the base's
// row still names the session the read saw; a live row that has landed no
// capture holds its base against an end's absorption; and only a turn that
// landed a capture absorbs, at its end, the turns its first read handed out.
// The domain's own suite holds those rules on the real log
// (internal/executordomain's TestNoReleaseTakesAHeldBase and
// TestATurnThatNeverCapturedAbsorbsNothing); these cases hold what the
// coordinators do with them.
type conversationLog struct {
	mu       sync.Mutex
	rows     map[string]executordomain.Session
	tombs    map[string]bool
	archives map[string][]byte
	seq      uint64

	begins   []executordomain.NewBegin
	captures []executordomain.NewCapture
}

var (
	_ SessionWrites  = (*conversationLog)(nil)
	_ SessionRestore = (*conversationLog)(nil)
)

func newConversationLog() *conversationLog {
	return &conversationLog{rows: map[string]executordomain.Session{}, tombs: map[string]bool{},
		archives: map[string][]byte{}}
}

// nextLocked is the log's next position.
func (l *conversationLog) nextLocked() statelog.Position {
	l.seq++
	return statelog.Position{Stream: executordomain.Domain{}.Stream().Name, Seq: l.seq}
}

// endedTurn seeds turn of the conversation `chat:D1` of seat swe as its capture
// and its end left it: ended, naming a session sessionID whose archive is body.
func (l *conversationLog) endedTurn(t *testing.T, turn, sessionID, body string) executordomain.Session {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	key := objstore.KeyAt(time.Now())
	row := executordomain.Session{Handle: "swe", Turn: turn, ConversationKey: "chat:D1", WorkKey: "wk-" + turn,
		Harness: "claude-code", HarnessVersion: "2.1.310", SessionID: sessionID,
		Object:     objstore.Object{Key: key, Hash: objstore.Hash(strings.Repeat("b", 64)), Size: int64(len(body))},
		ContentKey: "k1", ArchiveFormat: FakeArchiveFormat, LaunchSeq: 1, CaptureSeq: 1,
		Home: "/boxes/" + turn + "/home", ContextTokens: 42000,
		Ending: &executordomain.Ending{Outcome: executordomain.OutcomeCompleted}}
	row.EndedAt, row.EndedAtPosition = time.Now().UTC(), l.nextLocked()
	l.rows[turn], l.tombs[turn] = row, true
	l.archives[key.String()] = []byte(body)
	return row
}

// readNow is the position a head read made now would be read at.
func (l *conversationLog) readNow() statelog.Position {
	l.mu.Lock()
	defer l.mu.Unlock()
	return statelog.Position{Stream: executordomain.Domain{}.Stream().Name, Seq: l.seq}
}

func (l *conversationLog) Begin(_ context.Context, _ executordomain.Call,
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
	// THE TURN'S FIRST READ STANDS, whichever record carried it.
	if row.ReadThrough.Seq == 0 {
		row.ReadThrough, row.Absorbs = n.ReadThrough, slices.Clone(n.Absorbs)
		if base, ok := l.rows[n.BaseTurn]; ok && n.BaseTurn != "" && base.Ended() &&
			base.ConversationKey == n.ConversationKey && base.Object.Key == n.BaseObject.Key &&
			base.Object.Hash == n.BaseObject.Hash {
			row.BaseTurn = n.BaseTurn
		}
	}
	l.rows[n.Turn] = row
	held := n.BaseTurn != "" && row.BaseTurn == n.BaseTurn && row.LaunchSeq == 0
	why := ""
	if !held {
		why = "the base did not stand when the begin applied"
	}
	return executordomain.BeginResult{Result: statelog.Result{Outcome: statelog.OutcomeApplied},
		Held: held, Why: why}, nil
}

func (l *conversationLog) Capture(_ context.Context, _ executordomain.Call,
	n executordomain.NewCapture) (executordomain.CaptureResult, error) {

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.tombs[n.Turn] {
		return executordomain.CaptureResult{}, fmt.Errorf("%w: %s", executordomain.ErrTurnEnded, n.Turn)
	}
	l.captures = append(l.captures, n)
	row, found := l.rows[n.Turn]
	if !found {
		row = executordomain.Session{Handle: n.Handle, Turn: n.Turn, ConversationKey: n.ConversationKey,
			WorkKey: n.WorkKey}
	}
	if row.ReadThrough.Seq == 0 && n.ReadThrough.Seq > 0 {
		row.ReadThrough, row.Absorbs = n.ReadThrough, slices.Clone(n.Absorbs)
	}
	row.Harness, row.HarnessVersion, row.SessionID = n.Harness, n.HarnessVersion, n.SessionID
	row.Object, row.ContentKey, row.ArchiveFormat = n.Object, n.ContentKey, n.ArchiveFormat
	row.LaunchSeq, row.CaptureSeq = n.LaunchSeq, n.CaptureSeq
	row.Home, row.Workspace = n.Home, n.Workspace
	row.ContextTokens, row.Interrupted = n.ContextTokens, n.Interrupted
	l.rows[n.Turn] = row
	return executordomain.CaptureResult{Result: statelog.Result{Outcome: statelog.OutcomeApplied},
		Recorded: true}, nil
}

func (l *conversationLog) End(_ context.Context, _ executordomain.Call,
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
	// ONLY A TURN THAT LANDED A CAPTURE ABSORBS what its first read handed
	// out, and never a row a live turn holds.
	if row.LaunchSeq > 0 {
		for _, turn := range row.Absorbs {
			if absorbed, ok := l.rows[turn]; ok && absorbed.Ended() && !l.heldLocked(turn) {
				delete(l.rows, turn)
			}
		}
	}
	return statelog.Result{Outcome: statelog.OutcomeApplied}, nil
}

// heldLocked reports whether a live row that has landed no capture holds turn
// as its base.
func (l *conversationLog) heldLocked(turn string) bool {
	for _, row := range l.rows {
		if row.BaseTurn == turn && !row.Ended() && row.LaunchSeq == 0 {
			return true
		}
	}
	return false
}

// holds reports whether turn's row is held as some live turn's base.
func (l *conversationLog) holds(turn string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.heldLocked(turn)
}

// row is turn's row, and whether the log holds one.
func (l *conversationLog) row(turn string) (executordomain.Session, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	row, ok := l.rows[turn]
	return row, ok
}

// head is the conversation's head: its newest ended row that names a session.
func (l *conversationLog) head(conversationKey string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var head executordomain.Session
	for _, row := range l.rows {
		if row.ConversationKey == conversationKey && row.Ended() && row.HasObject() &&
			row.EndedAtPosition.Seq > head.EndedAtPosition.Seq {
			head = row
		}
	}
	return head.Turn
}

// begun is every begin recorded, in order.
func (l *conversationLog) begun() []executordomain.NewBegin {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.begins)
}

// capturesOf is every capture of turn recorded, in order.
func (l *conversationLog) capturesOf(turn string) []executordomain.NewCapture {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []executordomain.NewCapture
	for _, n := range l.captures {
		if n.Turn == turn {
			out = append(out, n)
		}
	}
	return out
}

func (l *conversationLog) Session(_ context.Context, handle, turn string,
	fresh statelog.Freshness) (executordomain.Session, error) {

	l.mu.Lock()
	defer l.mu.Unlock()
	// AN END ANOTHER NODE PUBLISHED is seen only at linearizable.
	if fresh.Level != statelog.ReadLinearizable {
		return executordomain.Session{}, errors.New("read below the level another node's record is seen at")
	}
	row, ok := l.rows[turn]
	if !ok || row.Handle != handle {
		return executordomain.Session{}, executordomain.ErrNotFound
	}
	return row, nil
}

func (l *conversationLog) Fetch(_ context.Context, row executordomain.Session, dst io.Writer) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	body, ok := l.archives[row.Object.Key.String()]
	if !ok {
		return executordomain.ErrUnreadable
	}
	_, err := dst.Write(body)
	return err
}

func (l *conversationLog) OpenSpooled(_ executordomain.Session, sealed io.Reader) (io.Reader, error) {
	return sealed, nil
}

// frozenReadOf is the read a turn's dispatch made of the conversation whose
// head is base, at the log's position now, the brief having told base: what
// the turn's first launch freezes.
func frozenReadOf(l *conversationLog, base executordomain.Session) *ConversationBase {
	return NewConversationBase(executordomain.Head{Session: &base, Position: l.readNow()},
		[]string{base.Turn})
}

// continuingBase is turnID's first launch, its conversation's read frozen as
// read, asked to continue the base it names — with the lead and the fresh brief
// told apart.
func continuingBase(turnID string, read *ConversationBase) LaunchRequest {
	req := launchReq(turnID)
	req.Brief = "the fresh brief, which tells how the conversation's earlier turns ended"
	req.Base = read
	req.Session = &SessionContinuation{Base: true,
		Replaced: "a new turn of this conversation begins here, then the fresh brief"}
	return req
}

// conversingRig is a keeping coordinator rig recording on, and continuing
// sessions from, one conversation log, whose runner restores what its harness
// wrote: node A of every case below.
func conversingRig(t *testing.T) (*coordRig, *conversationLog, *sessionKeep) {
	t.Helper()
	rig, _, keep := keepingRig(t)
	logbook := newConversationLog()
	rig.coordinator.sessions, rig.coordinator.restore = logbook, logbook
	rig.coordinator.beginBudget = 2 * time.Second
	rig.runner.Carries = true
	return rig, logbook, keep
}

// A TURN'S FIRST LAUNCH CONTINUES ITS CONVERSATION'S HEAD. The dispatch read the
// head — the conversation's last turn, ended, its session kept — and the turn's
// first launch is handed it: in a box of its own, the base's archive restored
// there, the run started on the base's session with the lead that says where
// this turn begins (the fresh brief beside it, for its script to hand a session
// of its own), the launch's record and its caller told it is the conversation's
// session. The read is frozen on the record the launch created and recorded as
// the turn's begin, which holds the base; and every capture of the turn carries
// that read, which the turn's end absorbs, while the turn's first landed
// capture lifts the hold.
//
// Mutation: continue no base on a first launch, read the head again at the
// launch, freeze the read anywhere but the create, publish no begin, or leave
// the read off the capture, and a check goes red.
func TestATurnsFirstLaunchContinuesItsConversationsHead(t *testing.T) {
	t.Parallel()
	rig, logbook, _ := conversingRig(t)
	base := logbook.endedTurn(t, "b1", "s-base", "the base's transcript")
	read := frozenReadOf(logbook, base)
	req := continuingBase("t2", read)
	var decided []LaunchDecision
	req.Decided = func(d LaunchDecision) { decided = append(decided, d) }

	res, err := rig.coordinator.Launch(t.Context(), rig.manager, req)
	if err != nil {
		t.Fatalf("the turn's first launch: %v", err)
	}
	started := rig.runner.Started()
	last := started[len(started)-1]
	if res.Reused || last.Resume != "s-base" || last.Brief != req.Session.Replaced || last.FreshBrief != req.Brief {
		t.Fatalf("the first launch started session %q (reused %v) with brief %q and fresh brief %q; want the "+
			"base's session in a box of its own, the lead, and the fresh brief beside it",
			last.Resume, res.Reused, last.Brief, last.FreshBrief)
	}
	carried := rig.runner.Carried()
	if len(carried) != 1 || string(carried[0].Body) != "the base's transcript" ||
		carried[0].Archive.SessionID != "s-base" || carried[0].Replace {
		t.Fatalf("the box was handed %+v, want the base's archive", carried)
	}
	if res.Brief != BriefContinuedConversation || res.Continues != "s-base" ||
		rig.get("t2").Launch.Brief != BriefContinuedConversation {
		t.Errorf("the launch answered %+v and recorded brief %q, want the conversation's session continued",
			res, rig.get("t2").Launch.Brief)
	}
	if want := (LaunchDecision{Brief: BriefContinuedConversation, Kept: true, Base: true}); len(decided) != 1 ||
		decided[0] != want {
		t.Errorf("the launch's caller was told %+v, want once: %+v", decided, want)
	}
	frozen := rig.get("t2").Base
	if frozen == nil || frozen.Session == nil || frozen.Session.Turn != "b1" ||
		frozen.ReadThrough != read.ReadThrough || !slices.Equal(frozen.Absorbs, []string{"b1"}) {
		t.Fatalf("the record froze %+v, want the dispatch's read of b1", frozen)
	}
	begins := logbook.begun()
	if len(begins) != 1 || begins[0].Turn != "t2" || begins[0].BaseTurn != "b1" ||
		begins[0].ReadThrough.Packed() != read.ReadThrough || !slices.Equal(begins[0].Absorbs, []string{"b1"}) ||
		begins[0].BaseObject.Key != base.Object.Key {
		t.Fatalf("the executor log was handed begins %+v, want the turn's one first read naming b1", begins)
	}
	if !logbook.holds("b1") {
		t.Fatal("no row holds the base the turn continues")
	}

	// THE JOB'S CAPTURE CARRIES THE READ, and lifts the hold.
	rig.suspend("t2")
	session := rig.runner.Session
	rig.runner.Finish(Result{Success: true, Text: "done", SessionID: "s-base", HarnessVersion: "2.1.310",
		Captured: &session.Archive})
	collectHeld(t, rig, "t2")
	captures := logbook.capturesOf("t2")
	if len(captures) != 1 || captures[0].ReadThrough.Packed() != read.ReadThrough ||
		!slices.Equal(captures[0].Absorbs, read.Absorbs) {
		t.Fatalf("the turn's captures %+v carry another read than its first launch froze", captures)
	}
	if logbook.holds("b1") {
		t.Error("the turn's first landed capture left the base held")
	}
}

// THE FREEZE IS WRITTEN AT THE FIRST LAUNCH ONLY. The write that creates the
// turn's record writes its read of the conversation; every write that opens a
// later launch on the record — a relaunch carrying a read of its own, or none —
// keeps it, so a relaunch on whatever node continues the base the first launch
// was handed. And a later build's keys inside the read, the base and each
// sibling survive this build's writes.
//
// Mutation: write the read on the reset, or drop a section's Extra, and this
// goes red.
func TestTheFreezeIsWrittenAtTheFirstLaunchOnly(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	store := NewCoordStore(memory.NewFleet())
	future := json.RawMessage(`{"a":"later build"}`)
	first := &ConversationBase{ReadThrough: 7, Absorbs: []string{"b1", "s1"},
		Siblings: []BaseSibling{{Turn: "s1", Object: objstore.KeyAt(time.Now()).String(),
			ObjectHash: strings.Repeat("c", 64), ObjectSize: 9, Extra: map[string]json.RawMessage{"sib_later": future}}},
		Session: &BaseSession{Turn: "b1", Object: objstore.KeyAt(time.Now()).String(),
			ObjectHash: strings.Repeat("b", 64), ObjectSize: 14, Harness: "claude-code", SessionID: "s-base",
			ArchiveFormat: FakeArchiveFormat, Extra: map[string]json.RawMessage{"session_later": future}},
		Extra: map[string]json.RawMessage{"read_later": future}}
	if _, err := store.BeginLaunch(ctx, PendingRun{TurnID: "t2", AgentHandle: "swe", Base: first},
		rigLease); err != nil {
		t.Fatalf("the first launch: %v", err)
	}
	other := &ConversationBase{ReadThrough: 99, Absorbs: []string{"b9"}}
	for _, relaunch := range []*ConversationBase{other, nil} {
		if _, err := store.BeginLaunch(ctx, PendingRun{TurnID: "t2", AgentHandle: "swe", Base: relaunch},
			rigLease); err != nil {
			t.Fatalf("a relaunch: %v", err)
		}
	}
	if err := store.SetStatus(ctx, "t2", StatusRunning, rigLease); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}
	got, found, err := store.Get(ctx, "t2")
	switch {
	case err != nil || !found:
		t.Fatalf("Get: %v, %v", found, err)
	case got.LaunchSeq != 3:
		t.Fatalf("the record is at launch %d, want the first and two relaunches", got.LaunchSeq)
	case got.Base == nil || got.Base.ReadThrough != 7 || got.Base.Session == nil || got.Base.Session.Turn != "b1":
		t.Fatalf("the record holds read %+v, want the first launch's", got.Base)
	}
	if string(got.Base.Extra["read_later"]) != string(future) ||
		string(got.Base.Session.Extra["session_later"]) != string(future) ||
		len(got.Base.Siblings) != 1 || string(got.Base.Siblings[0].Extra["sib_later"]) != string(future) {
		t.Errorf("a later build's keys in the read did not survive this build's writes: %+v", got.Base)
	}
}

// A RELAUNCH ON ANOTHER NODE AFTER A SIBLING ENDED CONTINUES THE FROZEN BASE.
// Turn T2's first launch, on node A, froze B1, the conversation's head, and held
// it. Its job was lost with its box before anything of it was captured. Sibling
// T3, which read B1 too, captured and ended — and its end, which absorbs what
// its read handed out, passed B1 over, since T2's row holds it; T3 is the
// conversation's head now. The seat moved to node B, over the same record and
// the same log. T2's relaunch there is asked which session it continues and
// answers B1 — never T3, which a read of the head now would name — and continues
// it in a box of its own; B1 still names its object. T2's first capture there
// carries the read its first launch made, and lifts the hold, so B1 is
// releasable; and T2's end absorbs B1, the head it was handed.
//
// Mutation: read the conversation's head again on a relaunch, drop the freeze
// on the record's reset, hold nothing on the log, or carry a read made now on
// the capture, and a check goes red.
func TestARelaunchOnAnotherNodeAfterASiblingEndedContinuesTheFrozenBase(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	rig, logbook, keep := conversingRig(t)
	b1 := logbook.endedTurn(t, "b1", "s-b1", "B1's transcript")
	read := frozenReadOf(logbook, b1)
	if _, err := rig.coordinator.Launch(ctx, rig.manager, continuingBase("t2", read)); err != nil {
		t.Fatalf("T2's first launch on node A: %v", err)
	}
	rig.suspend("t2")
	if !logbook.holds("b1") {
		t.Fatal("T2's first launch holds no base")
	}

	// T3, which read B1 too, captures and ends.
	if _, err := logbook.Capture(ctx, executordomain.Call{}, executordomain.NewCapture{Handle: "swe", Turn: "t3",
		ConversationKey: "chat:D1", WorkKey: "wk-t3", Harness: "claude-code", SessionID: "s-t3",
		Object: objstore.Object{Key: objstore.KeyAt(time.Now()), Hash: objstore.Hash(strings.Repeat("d", 64)),
			Size: 3},
		ArchiveFormat: FakeArchiveFormat, LaunchSeq: 1, CaptureSeq: 1,
		ReadThrough: statelog.Unpack(executordomain.Domain{}.Stream().Name, read.ReadThrough),
		Absorbs:     []string{"b1"}}); err != nil {
		t.Fatalf("T3's capture: %v", err)
	}
	if _, err := logbook.End(ctx, executordomain.Call{}, executordomain.NewEnd{Handle: "swe", Turn: "t3",
		ConversationKey: "chat:D1", WorkKey: "wk-t3",
		Ending: executordomain.Ending{Outcome: executordomain.OutcomeCompleted}}); err != nil {
		t.Fatalf("T3's end: %v", err)
	}
	if row, ok := logbook.row("b1"); !ok || row.Object.Key != b1.Object.Key {
		t.Fatalf("T3's end took B1, which T2 holds: %+v (%v)", row, ok)
	}
	if head := logbook.head("chat:D1"); head != "t3" {
		t.Fatalf("the conversation's head is %q, want T3, which ended since", head)
	}

	// THE SEAT MOVES: node B holds it under a newer lease, over the same
	// record and the same log.
	resumer := &resumeSpy{}
	nodeB, err := NewCoordinator(CoordinatorOptions{
		Answers: rigAnswers(), Queue: rig.queue, Pending: rig.pending, Manager: rig.manager,
		Resume: resumer, Audience: &audienceSpy{},
		Lease: leased(Fence{Owner: "node-b:1", Epoch: 2}), Node: "node-b",
		Sessions: logbook, Keep: keep, Restore: logbook, SpoolDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	t.Cleanup(nodeB.Stop)
	continued, why := nodeB.ContinuedCapture(ctx, "t2", rig.runner)
	if why != "" || continued.Base == nil || continued.Base.Turn != "b1" {
		t.Fatalf("node B's relaunch continues %+v (%q), want B1, the base T2 froze", continued.Base, why)
	}
	relaunch := launchReq("t2")
	relaunch.Brief = "the reseed, which tells the same endings"
	relaunch.Session = &SessionContinuation{Base: true,
		Replaced: "a new turn of this conversation begins here, then the reseed"}
	res, err := nodeB.Launch(ctx, rig.manager, relaunch)
	if err != nil {
		t.Fatalf("T2's relaunch on node B: %v", err)
	}
	started := rig.runner.Started()
	last := started[len(started)-1]
	if res.Reused || res.Brief != BriefContinuedConversation || last.Resume != "s-b1" ||
		last.Brief != relaunch.Session.Replaced || last.FreshBrief != relaunch.Brief {
		t.Fatalf("node B's relaunch started session %q in a reused box %v, brief %q (%s); want B1's in a box "+
			"of its own", last.Resume, res.Reused, last.Brief, res.Brief)
	}
	if carried := rig.runner.Carried(); len(carried) != 2 || string(carried[1].Body) != "B1's transcript" {
		t.Fatalf("node B's box was handed %+v, want B1's archive", carried)
	}
	if frozen := rig.get("t2").Base; frozen == nil || frozen.Session == nil || frozen.Session.Turn != "b1" ||
		frozen.ReadThrough != read.ReadThrough {
		t.Fatalf("the relaunch's record holds read %+v, want the first launch's", frozen)
	}
	if begins := logbook.begun(); len(begins) != 1 {
		t.Errorf("the relaunch recorded begins %+v: a turn's first read is recorded once", begins)
	}
	if row, ok := logbook.row("b1"); !ok || !row.HasObject() {
		t.Fatalf("B1 names no object any more: %+v (%v)", row, ok)
	}

	// T2'S FIRST CAPTURE, ON NODE B, carries the first launch's read and lifts
	// the hold.
	rig.suspend("t2")
	session := rig.runner.Session
	rig.runner.Finish(Result{Success: true, Text: "done", SessionID: "s-b1", HarnessVersion: "2.1.310",
		Captured: &session.Archive})
	resumer.failWith(errors.New("held for the case to read the row"))
	payload, ev := rig.completion("t2")
	if err := nodeB.OnCompleted(ctx, payload, ev); err == nil {
		t.Fatal("the refused resume was acknowledged")
	}
	nodeB.keeping.Wait()
	captures := logbook.capturesOf("t2")
	if len(captures) != 1 || captures[0].ReadThrough.Packed() != read.ReadThrough ||
		!slices.Equal(captures[0].Absorbs, []string{"b1"}) {
		t.Fatalf("T2's captures %+v carry another read than its first launch froze", captures)
	}
	if logbook.holds("b1") {
		t.Fatal("T2's first capture left B1 held")
	}
	// AND T2'S END ABSORBS B1, the head it was handed.
	if _, err := logbook.End(ctx, executordomain.Call{}, executordomain.NewEnd{Handle: "swe", Turn: "t2",
		ConversationKey: "chat:D1", WorkKey: "wk-1",
		Ending: executordomain.Ending{Outcome: executordomain.OutcomeCompleted}}); err != nil {
		t.Fatalf("T2's end: %v", err)
	}
	if row, ok := logbook.row("b1"); ok {
		t.Fatalf("T2's end left B1, the head it was handed: %+v", row)
	}
}

// WHETHER A BASE IS CONTINUED IS ONE DECISION, of the base the turn froze, the
// turn's own row on the executor log and its run's record. A base naming a
// session, the turn live, not ending and having kept nothing, its row holding
// that base, and a runner that restores it: continued. A base naming none, a
// run whose ending is decided, a turn that kept a capture of its own — on its
// record, or on its row — an ended row, a row holding another base or none, no
// runner, and one that refuses the archive: not, each saying why. Asked of no
// row and no record, as a turn's first launch is before either exists, it is the
// base and the runner alone.
//
// Mutation: drop any one condition and its row goes red.
func TestWhetherABaseIsContinuedIsOneDecision(t *testing.T) {
	t.Parallel()
	key := objstore.KeyAt(time.Now()).String()
	base := BaseSession{Turn: "b1", Object: key, ObjectHash: strings.Repeat("b", 64), ObjectSize: 14,
		Harness: "claude-code", SessionID: "s-base", ArchiveFormat: FakeArchiveFormat}
	holding := &executordomain.Session{Handle: "swe", Turn: "t2", BaseTurn: "b1"}
	captured := *holding
	captured.LaunchSeq, captured.CaptureSeq = 1, 1
	ended := *holding
	ended.Ending = &executordomain.Ending{Outcome: executordomain.OutcomeCompleted}
	other := *holding
	other.BaseTurn = "b0"
	none := *holding
	none.BaseTurn = ""
	live := &PendingRun{TurnID: "t2", LaunchSeq: 2}
	kept := &PendingRun{TurnID: "t2", LaunchSeq: 2, Kept: &SessionCapture{LaunchSeq: 1, CaptureSeq: 1,
		State: CaptureLanded}}
	ending := &PendingRun{TurnID: "t2", LaunchSeq: 2, OwedEnd: []byte(`{"outcome":"completed"}`)}
	for name, tc := range map[string]struct {
		base      BaseSession
		own       *executordomain.Session
		run       *PendingRun
		restorer  ArchiveRestorer
		continues bool
		why       string
	}{
		"a first launch, before its row and its record": {base: base, restorer: fakeRestorer{}, continues: true},
		"a relaunch of a turn that kept nothing":        {base: base, own: holding, run: live, restorer: fakeRestorer{}, continues: true},
		"a base naming no session":                      {base: BaseSession{Turn: "b1"}, restorer: fakeRestorer{}, why: "left no session"},
		"a run whose ending is decided":                 {base: base, own: holding, run: ending, restorer: fakeRestorer{}, why: "is ending"},
		"a turn that kept a capture of its own":         {base: base, own: holding, run: kept, restorer: fakeRestorer{}, why: "kept a session of its own"},
		"a row that landed a capture":                   {base: base, own: &captured, run: live, restorer: fakeRestorer{}, why: "landed a capture of its own"},
		"an ended row":                                  {base: base, own: &ended, run: live, restorer: fakeRestorer{}, why: "has ended"},
		"a row holding another base":                    {base: base, own: &other, run: live, restorer: fakeRestorer{}, why: "does not hold turn b1's"},
		"a row holding none":                            {base: base, own: &none, run: live, restorer: fakeRestorer{}, why: "does not hold turn b1's"},
		"no runner restores it":                         {base: base, own: holding, run: live, why: "no runner of this build restores"},
		"its runner refuses it":                         {base: base, own: holding, run: live, restorer: fakeRestorer{why: "older than 2.1.300"}, why: "older than 2.1.300"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			got := BaseContinuityOf(tc.base, tc.own, tc.run, tc.restorer)
			if got.Continues != tc.continues || !strings.Contains(got.Why, tc.why) || (tc.continues && got.Why != "") {
				t.Fatalf("BaseContinuityOf = %+v, want continues %v saying %q", got, tc.continues, tc.why)
			}
		})
	}
}

// A RELAUNCH CONTINUES THE BASE ONLY WHILE THE TURN HAS KEPT NONE. A turn whose
// first job kept nothing is relaunched into the base its first launch froze,
// which its row still holds. Once the turn has kept a session of its own —
// here an earlier job's, the job the relaunch follows having left none — that
// session holds the base and everything after it, so the relaunch continues no
// base, saying why, and a launch asked to continue it all the same starts a
// session of its own.
//
// Mutation: continue the frozen base whatever the turn kept, at the probe or at
// the launch, and a case goes red.
func TestARelaunchContinuesTheBaseOnlyWhileTheTurnHasKeptNone(t *testing.T) {
	t.Parallel()
	t.Run("the turn kept nothing", func(t *testing.T) {
		t.Parallel()
		rig, logbook, _ := conversingRig(t)
		read := frozenReadOf(logbook, logbook.endedTurn(t, "b1", "s-base", "the base's transcript"))
		if _, err := rig.coordinator.Launch(t.Context(), rig.manager, continuingBase("t2", read)); err != nil {
			t.Fatalf("the first launch: %v", err)
		}
		continued, why := rig.coordinator.ContinuedCapture(t.Context(), "t2", rig.runner)
		if why != "" || continued.Base == nil || continued.Base.Turn != "b1" || continued.Capture.CaptureSeq != 0 {
			t.Fatalf("a relaunch of a turn that kept nothing continues %+v (%q), want the frozen base",
				continued, why)
		}
	})
	t.Run("the turn kept a session of its own", func(t *testing.T) {
		t.Parallel()
		rig, logbook, _ := conversingRig(t)
		read := frozenReadOf(logbook, logbook.endedTurn(t, "b1", "s-base", "the base's transcript"))
		ctx := t.Context()
		if _, err := rig.coordinator.Launch(ctx, rig.manager, continuingBase("t2", read)); err != nil {
			t.Fatalf("the first launch: %v", err)
		}
		own := SessionCapture{LaunchSeq: 1, CaptureSeq: 1, State: CaptureLanded, Final: true,
			Object: objstore.KeyAt(time.Now()).String(), ObjectHash: strings.Repeat("e", 64), ObjectSize: 5}
		if _, err := rig.pending.StampCapture(ctx, "t2", own); err != nil {
			t.Fatalf("StampCapture: %v", err)
		}
		// THE JOB AFTER IT, which kept nothing: the relaunch follows that one.
		if _, err := rig.pending.BeginLaunch(ctx, PendingRun{TurnID: "t2", AgentHandle: "swe",
			AgentID: sweID, Role: "SWE", Harness: "claude-code"}, rigLease); err != nil {
			t.Fatalf("the second job: %v", err)
		}
		continued, why := rig.coordinator.ContinuedCapture(ctx, "t2", rig.runner)
		if continued.Base != nil || !strings.Contains(why, "kept a session of its own") {
			t.Fatalf("a relaunch of a turn that kept a session continues base %+v (%q), want none, said",
				continued.Base, why)
		}
		req := launchReq("t2")
		req.Brief = "the fresh brief"
		req.Session = &SessionContinuation{Base: true, Replaced: "a new turn begins here"}
		if _, err := rig.coordinator.Launch(ctx, rig.manager, req); err != nil {
			t.Fatalf("the relaunch: %v", err)
		}
		started := rig.runner.Started()
		if last := started[len(started)-1]; last.Resume == "s-base" || last.Brief != "the fresh brief" {
			t.Fatalf("the relaunch started session %q with brief %q, want a session of its own", last.Resume,
				last.Brief)
		}
	})
}

// A BASE IS CONTINUED ONLY IN A BOX OF ITS OWN. A box the turn's job before ran
// in holds that job's working tree and its session, which are the turn's to
// continue in place — never the conversation's last turn's session restored
// over them. So the launch that reuses that box carries the job before's own
// session on there, or starts fresh, and the base is continued only in a box of
// the launch's own.
//
// Mutation: continue the base in a reused box, and the in-place rows go red.
func TestABaseIsContinuedOnlyInABoxOfItsOwn(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		reused, inPlace bool
		want            BriefKind
		session         string
		carried         bool
	}{
		"a box of its own":                 {want: BriefContinuedConversation, session: "s-base", carried: true},
		"its box, with the job's session":  {reused: true, inPlace: true, want: BriefContinued, session: "sess-own"},
		"its box, with no session to keep": {reused: true, want: BriefFresh},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			rig, logbook, _ := conversingRig(t)
			read := frozenReadOf(logbook, logbook.endedTurn(t, "b1", "s-base", "the base's transcript"))
			if _, err := logbook.Begin(t.Context(), executordomain.Call{}, executordomain.NewBegin{Handle: "swe",
				Turn: "t2", ConversationKey: "chat:D1", WorkKey: "wk-1",
				ReadThrough: statelog.Unpack(executordomain.Domain{}.Stream().Name, read.ReadThrough),
				Absorbs:     read.Absorbs, BaseTurn: "b1",
				BaseObject: objstore.Object{Key: mustKey(t, read.Session.Object),
					Hash: objstore.Hash(read.Session.ObjectHash), Size: read.Session.ObjectSize}}); err != nil {
				t.Fatalf("Begin: %v", err)
			}
			opened := PendingRun{TurnID: "t2", AgentHandle: "swe", LaunchSeq: 2, Base: read}
			req := launchReq("t2")
			req.Brief = "the fresh brief"
			req.Session = &SessionContinuation{Base: true, Replaced: "a new turn begins here"}
			if tc.inPlace {
				req.Continue = &Continuation{SessionID: "sess-own", Brief: "carry on in the box you left"}
			}
			got := rig.coordinator.continueSession(t.Context(), req, opened, NewFakeSandbox("box-1"),
				rig.runner, tc.reused, false)
			t.Cleanup(got.release)
			if got.kind != tc.want || got.session != tc.session || (got.carried != nil) != tc.carried {
				t.Fatalf("the launch hands %s on session %q, carrying %v; want %s on %q, carrying %v",
					got.kind, got.session, got.carried != nil, tc.want, tc.session, tc.carried)
			}
		})
	}
}

// mustKey is the object key s spells.
func mustKey(t *testing.T, s string) objstore.Key {
	t.Helper()
	key, err := objstore.ParseKey(s)
	if err != nil {
		t.Fatalf("ParseKey(%q): %v", s, err)
	}
	return key
}

// A FROZEN READ FITS ITS BOUND, whatever the log admits: the largest read a head
// can answer — the most turns it hands out and the most siblings it offers, each
// a turn and an object, and a base whose every free-text field is at the log's
// bound and escapes at JSON's worst — encodes within [ConversationBaseMaxBytes],
// the share of the run's record the read is held to. And only what the brief
// told is frozen: a turn its bound left out is neither absorbed nor offered,
// and stays for the next read.
//
// Mutation: lower the bound below the largest read, or freeze what the brief
// did not tell, and this goes red.
func TestAFrozenReadFitsItsBound(t *testing.T) {
	t.Parallel()
	turn := func(i int) string { return fmt.Sprintf("0190f3a0-0000-7000-8000-%012d", i) }
	// A CONTROL CHARACTER, which JSON escapes as six bytes: every free-text
	// field at its bound, at the encoding's worst.
	const worst = "\x01"
	object := func() objstore.Object {
		return objstore.Object{Key: objstore.KeyAt(time.Now()), Hash: objstore.Hash(strings.Repeat("f", 64)),
			Size: executordomain.SealedMaxBytes}
	}
	head := executordomain.Session{Handle: "swe", Turn: turn(0), Object: object(),
		ContentKey:     strings.Repeat(worst, executordomain.ContentKeyMaxBytes),
		Harness:        strings.Repeat(worst, executordomain.HarnessMaxBytes),
		HarnessVersion: strings.Repeat(worst, executordomain.HarnessVersionMaxBytes),
		SessionID:      strings.Repeat(worst, executordomain.SessionIDMaxBytes),
		Home:           strings.Repeat(worst, executordomain.PathMaxBytes),
		Workspace:      strings.Repeat(worst, executordomain.PathMaxBytes),
		ContextTokens:  1 << 40, Interrupted: true, ArchiveFormat: 1 << 20,
		EndedAt: time.Now().UTC(), Ending: &executordomain.Ending{Outcome: executordomain.OutcomeCompleted}}
	var endings []executordomain.Session
	told := []string{head.Turn}
	for i := 1; i <= executordomain.MaxEndings; i++ {
		ended := executordomain.Session{Handle: "swe", Turn: turn(i),
			Ending: &executordomain.Ending{Outcome: executordomain.OutcomeCompleted}}
		if i <= executordomain.MaxSiblings {
			ended.Object = object()
		}
		endings = append(endings, ended)
		told = append(told, ended.Turn)
	}
	var siblings []executordomain.Session
	for _, e := range endings {
		if e.HasObject() {
			siblings = append(siblings, e)
		}
	}
	read := executordomain.Head{Session: &head, Endings: endings, Siblings: siblings,
		Position: statelog.Position{Stream: executordomain.Domain{}.Stream().Name, Generation: 1 << 22,
			Seq: 1 << 39}}
	largest := NewConversationBase(read, told)
	raw, err := json.Marshal(largest)
	if err != nil {
		t.Fatalf("encoding the read: %v", err)
	}
	switch {
	case len(largest.Absorbs) != executordomain.MaxAbsorbs || len(largest.Siblings) != executordomain.MaxSiblings:
		t.Fatalf("the largest read absorbs %d and offers %d, want %d and %d", len(largest.Absorbs),
			len(largest.Siblings), executordomain.MaxAbsorbs, executordomain.MaxSiblings)
	case len(raw) > ConversationBaseMaxBytes:
		t.Fatalf("the largest read encodes in %d bytes, past its bound of %d", len(raw), ConversationBaseMaxBytes)
	}
	t.Logf("the largest frozen read is %d bytes of %d", len(raw), ConversationBaseMaxBytes)

	// ONLY WHAT THE BRIEF TOLD: the head and the newest ending, the rest left
	// out by the brief's bound.
	partial := NewConversationBase(read, []string{head.Turn, endings[len(endings)-1].Turn})
	if !slices.Equal(partial.Absorbs, []string{head.Turn, endings[len(endings)-1].Turn}) ||
		len(partial.Siblings) != 0 || partial.Session == nil {
		t.Fatalf("a read whose brief told two turns froze %+v", partial)
	}
	if untold := NewConversationBase(read, nil); untold.Session != nil || len(untold.Absorbs) != 0 {
		t.Fatalf("a read whose brief told nothing froze %+v", untold)
	}
}
