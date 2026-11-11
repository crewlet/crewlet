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
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
)

// conversationLog is ONE executor log as the cases below play it: every node's
// coordinator records on it and continues sessions from it, over the rows of a
// seat's conversation — each turn's row by its turn, the ended ones
// tombstoned, and every object's archive.
//
// IT KEEPS THE DOMAIN'S RULES THESE CASES TURN ON, and no others: a turn's
// first landed record keeps its read; a begin holds its base where the base's
// row still names the session the read saw; a live row that has landed no
// capture holds its base against an end's absorption; only a turn that landed
// a capture absorbs, at its end, the turns its first read handed out; and a
// head read now names the ended turn that read the conversation last
// ([conversationLog.head]). The domain's own suite holds those rules on the
// real log (internal/executordomain's TestNoReleaseTakesAHeldBase,
// TestATurnThatNeverCapturedAbsorbsNothing and
// TestAFireThatEndsAfterTheFiresBesideItIsToldNotContinued), and
// TestTheConversationLogsHeadIsTheTurnThatReadItLast holds this log to the
// last of them; these cases hold what the coordinators do with them.
type conversationLog struct {
	mu       sync.Mutex
	rows     map[string]logRow
	tombs    map[string]bool
	archives map[string][]byte
	seq      int64

	begins   []BeginRecord
	captures []CaptureRecord
}

// logRow is one turn's row on the conversationLog: what the restore reads of it
// ([executorkeep.TurnRow]) and what the log keeps beside that — the turn's
// first read and where its end landed.
type logRow struct {
	executorkeep.TurnRow
	WorkKey     string
	Ending      *executorkeep.Ending
	EndedSeq    int64
	ReadThrough int64
	Absorbs     []string
}

var (
	_ SessionWrites  = (*conversationLog)(nil)
	_ SessionRestore = (*conversationLog)(nil)
)

func newConversationLog() *conversationLog {
	return &conversationLog{rows: map[string]logRow{}, tombs: map[string]bool{},
		archives: map[string][]byte{}}
}

// nextLocked is the log's next position.
func (l *conversationLog) nextLocked() int64 {
	l.seq++
	return l.seq
}

// endedTurn seeds turn of the conversation `chat:D1` of seat swe as its capture
// and its end left it: ended, naming a session sessionID whose archive is body.
func (l *conversationLog) endedTurn(t *testing.T, turn, sessionID, body string) executorkeep.TurnRow {
	t.Helper()
	l.mu.Lock()
	defer l.mu.Unlock()
	key := objstore.KeyAt(time.Now())
	row := logRow{TurnRow: executorkeep.TurnRow{Handle: "swe", Turn: turn, ConversationKey: "chat:D1",
		Harness: "claude-code", HarnessVersion: "2.1.310", SessionID: sessionID,
		Object:     objstore.Object{Key: key, Hash: objstore.Hash(strings.Repeat("b", 64)), Size: int64(len(body))},
		ContentKey: "k1", ArchiveFormat: FakeArchiveFormat, LaunchSeq: 1, CaptureSeq: 1,
		Home: "/boxes/" + turn + "/home", ContextTokens: 42000, Ended: true, EndedAt: time.Now().UTC()},
		WorkKey: "wk-" + turn, Ending: &executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}}
	row.EndedSeq = l.nextLocked()
	l.rows[turn], l.tombs[turn] = row, true
	l.archives[key.String()] = []byte(body)
	return row.TurnRow
}

// readNow is the position a head read made now would be read at.
func (l *conversationLog) readNow() int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.seq
}

// errTurnEnded is the log's refusal of a record of a turn it tombstoned.
var errTurnEnded = errors.New("the turn has ended")

func (l *conversationLog) Begin(_ context.Context, b BeginRecord) (BeginAnswer, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.tombs[b.Turn] {
		return BeginAnswer{Log: LogRefused}, fmt.Errorf("%w: %s", errTurnEnded, b.Turn)
	}
	l.begins = append(l.begins, b)
	row, found := l.rows[b.Turn]
	if !found {
		row = logRow{TurnRow: executorkeep.TurnRow{Handle: b.Handle, Turn: b.Turn,
			ConversationKey: b.ConversationKey}, WorkKey: b.WorkKey}
	}
	// THE TURN'S FIRST READ STANDS, whichever record carried it.
	if row.ReadThrough == 0 {
		row.ReadThrough, row.Absorbs = b.ReadThrough, slices.Clone(b.Absorbs)
		if base, ok := l.rows[b.BaseTurn]; ok && b.BaseTurn != "" && base.Ended &&
			base.ConversationKey == b.ConversationKey && base.Object.Key == b.BaseObject.Key &&
			base.Object.Hash == b.BaseObject.Hash {
			row.BaseTurn = b.BaseTurn
		}
	}
	l.rows[b.Turn] = row
	held := b.BaseTurn != "" && row.BaseTurn == b.BaseTurn && row.LaunchSeq == 0
	why := ""
	if !held {
		why = "the base did not stand when the begin applied"
	}
	return BeginAnswer{Log: LogLanded, Held: held, Why: why}, nil
}

func (l *conversationLog) Capture(_ context.Context, c CaptureRecord) (LogAnswer, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.tombs[c.Turn] {
		return LogRefused, fmt.Errorf("%w: %s", errTurnEnded, c.Turn)
	}
	l.captures = append(l.captures, c)
	row, found := l.rows[c.Turn]
	if !found {
		row = logRow{TurnRow: executorkeep.TurnRow{Handle: c.Handle, Turn: c.Turn,
			ConversationKey: c.ConversationKey}, WorkKey: c.WorkKey}
	}
	if row.ReadThrough == 0 && c.ReadThrough > 0 {
		row.ReadThrough, row.Absorbs = c.ReadThrough, slices.Clone(c.Absorbs)
	}
	n := c.Stamp
	row.Harness, row.HarnessVersion, row.SessionID = n.Harness, n.HarnessVersion, n.SessionID
	row.Object, row.ContentKey, row.ArchiveFormat = c.Object, n.ContentKey, n.ArchiveFormat
	row.LaunchSeq, row.CaptureSeq = n.LaunchSeq, n.CaptureSeq
	row.Home, row.Workspace = n.Home, n.Workspace
	row.ContextTokens, row.Interrupted = n.ContextTokens, n.Interrupted
	l.rows[c.Turn] = row
	return LogLanded, nil
}

func (l *conversationLog) End(_ context.Context, e EndRecord) (LogAnswer, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.tombs[e.Turn] {
		return LogLanded, nil
	}
	row, found := l.rows[e.Turn]
	if !found {
		row = logRow{TurnRow: executorkeep.TurnRow{Handle: e.Handle, Turn: e.Turn,
			ConversationKey: e.ConversationKey}, WorkKey: e.WorkKey}
	}
	ending := e.Ending
	row.Ending, row.Ended, row.EndedAt, row.EndedSeq = &ending, true, time.Now().UTC(), l.nextLocked()
	l.rows[e.Turn], l.tombs[e.Turn] = row, true
	// ONLY A TURN THAT LANDED A CAPTURE ABSORBS what its first read handed
	// out, and never a row a live turn holds.
	if row.LaunchSeq > 0 {
		for _, turn := range row.Absorbs {
			if absorbed, ok := l.rows[turn]; ok && absorbed.Ended && !l.heldLocked(turn) {
				delete(l.rows, turn)
			}
		}
	}
	return LogLanded, nil
}

// heldLocked reports whether a live row that has landed no capture holds turn
// as its base.
func (l *conversationLog) heldLocked(turn string) bool {
	for _, row := range l.rows {
		if row.BaseTurn == turn && !row.Ended && row.LaunchSeq == 0 {
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
func (l *conversationLog) row(turn string) (executorkeep.TurnRow, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	row, ok := l.rows[turn]
	return row.TurnRow, ok
}

// head is the conversation's head, as the domain's head statement orders it:
// of its ended rows that name a session, the one whose turn read the
// conversation last, the newest end among turns that read it alike.
func (l *conversationLog) head(conversationKey string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var head *logRow
	for _, row := range l.rows {
		if row.ConversationKey != conversationKey || !row.Ended || !row.HasObject() {
			continue
		}
		if head == nil || row.ReadThrough > head.ReadThrough ||
			(row.ReadThrough == head.ReadThrough && row.EndedSeq > head.EndedSeq) {
			chosen := row
			head = &chosen
		}
	}
	if head == nil {
		return ""
	}
	return head.Turn
}

// begun is every begin recorded, in order.
func (l *conversationLog) begun() []BeginRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.begins)
}

// capturesOf is every capture of turn recorded, in order.
func (l *conversationLog) capturesOf(turn string) []CaptureRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []CaptureRecord
	for _, c := range l.captures {
		if c.Turn == turn {
			out = append(out, c)
		}
	}
	return out
}

// Session answers a turn's row as the restore reads it: at the level a
// relaunch reads (the engine's adapter's), so an end another node published is
// seen.
func (l *conversationLog) Session(_ context.Context, handle, turn string) (executorkeep.TurnRow, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	row, ok := l.rows[turn]
	if !ok || row.Handle != handle {
		return executorkeep.TurnRow{}, ErrTurnRowNotFound
	}
	return row.TurnRow, nil
}

func (l *conversationLog) Fetch(_ context.Context, row executorkeep.TurnRow, dst io.Writer) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	body, ok := l.archives[row.Object.Key.String()]
	if !ok {
		return errors.New("the store holds no such object")
	}
	_, err := dst.Write(body)
	return err
}

func (l *conversationLog) OpenSpooled(_ executorkeep.TurnRow, sealed io.Reader) (io.Reader, error) {
	return sealed, nil
}

// frozenReadOf is the read a turn's dispatch made of the conversation whose
// head is base, at the log's position now, the brief having told base: what
// the turn's first launch freezes.
func frozenReadOf(l *conversationLog, base executorkeep.TurnRow) *ConversationBase {
	return NewConversationBase(executorkeep.ConversationHead{Session: &base, ReadThrough: l.readNow(),
		Absorbs: []string{base.Turn}}, []string{base.Turn})
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
//
// Built on [keepingRig], and so only in a bubble ([inBubble]).
//
//fixedwait:bubble
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
	inBubble(t, func(t *testing.T) {
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
			begins[0].ReadThrough != read.ReadThrough || !slices.Equal(begins[0].Absorbs, []string{"b1"}) ||
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
		if len(captures) != 1 || captures[0].ReadThrough != read.ReadThrough ||
			!slices.Equal(captures[0].Absorbs, read.Absorbs) {
			t.Fatalf("the turn's captures %+v carry another read than its first launch froze", captures)
		}
		if logbook.holds("b1") {
			t.Error("the turn's first landed capture left the base held")
		}
	})
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
	inBubble(t, func(t *testing.T) {
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
		if _, err := logbook.Capture(ctx, CaptureRecord{Handle: "swe", Turn: "t3",
			ConversationKey: "chat:D1", WorkKey: "wk-t3",
			Object: objstore.Object{Key: objstore.KeyAt(time.Now()), Hash: objstore.Hash(strings.Repeat("d", 64)),
				Size: 3},
			Stamp: SessionCapture{Harness: "claude-code", SessionID: "s-t3", ArchiveFormat: FakeArchiveFormat,
				LaunchSeq: 1, CaptureSeq: 1},
			ReadThrough: read.ReadThrough, Absorbs: []string{"b1"}}); err != nil {
			t.Fatalf("T3's capture: %v", err)
		}
		if _, err := logbook.End(ctx, EndRecord{Handle: "swe", Turn: "t3",
			ConversationKey: "chat:D1", WorkKey: "wk-t3",
			Ending: executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}}); err != nil {
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
			Answers: rigAnswers(), Queue: rig.queue, Pending: rig.pending, Manager: rig.manager, Pushes: rig.pushes,
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
		if len(captures) != 1 || captures[0].ReadThrough != read.ReadThrough ||
			!slices.Equal(captures[0].Absorbs, []string{"b1"}) {
			t.Fatalf("T2's captures %+v carry another read than its first launch froze", captures)
		}
		if logbook.holds("b1") {
			t.Fatal("T2's first capture left B1 held")
		}
		// AND T2'S END ABSORBS B1, the head it was handed.
		if _, err := logbook.End(ctx, EndRecord{Handle: "swe", Turn: "t2",
			ConversationKey: "chat:D1", WorkKey: "wk-1",
			Ending: executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}}); err != nil {
			t.Fatalf("T2's end: %v", err)
		}
		if row, ok := logbook.row("b1"); ok {
			t.Fatalf("T2's end left B1, the head it was handed: %+v", row)
		}
	})
}

// THE LOG'S HEAD IS THE DOMAIN'S, so a case that asks it which session a head
// read made now would name is answered as the domain's head statement answers:
// of a conversation's ended rows that name a session, the one whose turn read
// the conversation last, never simply the one that ended last. Turn p read B1
// and parked; turn a read B1 too, beside it, and ended, absorbing B1; turn b
// read the conversation after a's end, continued a and ended; and p, answered,
// ended last of all. The head is b, and p is not. The domain's own suite holds
// its statement to the same order (internal/executordomain's
// TestAFireThatEndsAfterTheFiresBesideItIsToldNotContinued).
//
// Mutation: order conversationLog.head by EndedSeq alone and p is named.
func TestTheConversationLogsHeadIsTheTurnThatReadItLast(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	logbook := newConversationLog()
	logbook.endedTurn(t, "b1", "s-b1", "B1's transcript")
	landCapture := func(turn string, read int64, absorbs ...string) {
		t.Helper()
		if _, err := logbook.Capture(ctx, CaptureRecord{Handle: "swe", Turn: turn,
			ConversationKey: "chat:D1", WorkKey: "wk-" + turn,
			Object: objstore.Object{Key: objstore.KeyAt(time.Now()), Hash: objstore.Hash(strings.Repeat("e", 64)),
				Size: 3},
			Stamp: SessionCapture{Harness: "claude-code", SessionID: "s-" + turn, ArchiveFormat: FakeArchiveFormat,
				LaunchSeq: 1, CaptureSeq: 1},
			ReadThrough: read, Absorbs: absorbs}); err != nil {
			t.Fatalf("%s's capture: %v", turn, err)
		}
	}
	landEnd := func(turn string) {
		t.Helper()
		if _, err := logbook.End(ctx, EndRecord{Handle: "swe", Turn: turn, ConversationKey: "chat:D1",
			WorkKey: "wk-" + turn, Ending: executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted}}); err != nil {
			t.Fatalf("%s's end: %v", turn, err)
		}
	}
	first := logbook.readNow()
	landCapture("p", first, "b1")
	landCapture("a", first, "b1")
	landEnd("a")
	landCapture("b", logbook.readNow(), "a")
	landEnd("b")
	landEnd("p")
	if head := logbook.head("chat:D1"); head != "b" {
		t.Fatalf("the log names %q the head, want b, which read the conversation last, not p, which "+
			"ended last", head)
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
	holding := &executorkeep.TurnRow{Handle: "swe", Turn: "t2", BaseTurn: "b1"}
	captured := *holding
	captured.LaunchSeq, captured.CaptureSeq = 1, 1
	ended := *holding
	ended.Ended = true
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
		own       *executorkeep.TurnRow
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
		inBubble(t, func(t *testing.T) {
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
	})
	t.Run("the turn kept a session of its own", func(t *testing.T) {
		t.Parallel()
		inBubble(t, func(t *testing.T) {
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
	})
}

// A RELAUNCH STARTS WITHOUT A CAPTURE ITS RUN'S RECORD SETTLED REFUSED, WHENEVER
// ITS RECORD LANDS, AND SAYS WHERE THE LOG KEEPS IT. A turn that froze its
// conversation's base kept nothing before its job's final capture, which its
// run's record holds settled refused — its taker's lease lapsed, and the
// turn's row named no such capture when the settle read it — while the record
// that taker had already handed on may land after. A relaunch continues only
// what its run's record keeps, so it continues that capture in neither order.
// Where the log has taken the record by the time the relaunch reads the
// turn's row, that row holds the base no longer: the relaunch continues
// neither, its reason saying the log keeps the capture and naming no session a
// launch continues instead. Where the log has not, the relaunch continues the
// base, as any relaunch of a turn that has kept nothing does.
//
// Either way the relaunch's run is told what its read found there to read: the
// turn's own capture, the log having taken it, or else the base its row holds.
//
// Mutation: continue the base whatever the turn's row names, word the base's
// refusal as a session the relaunch continues instead, leave what the log
// keeps out of the reason, or name the run the turn's own off the record's
// stamp alone, and a case goes red.
func TestARelaunchStartsWithoutACaptureItsRecordSettledRefused(t *testing.T) {
	t.Parallel()
	for name, landed := range map[string]bool{
		"its record landed before the relaunch read the turn's row": true,
		"its record not yet landed as the relaunch read the row":    false,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				rig, logbook, _ := conversingRig(t)
				read := frozenReadOf(logbook, logbook.endedTurn(t, "b1", "s-base", "the base's transcript"))
				ctx := t.Context()
				if _, err := rig.coordinator.Launch(ctx, rig.manager, continuingBase("t2", read)); err != nil {
					t.Fatalf("the first launch: %v", err)
				}
				job := rig.get("t2")
				settled := SessionCapture{LaunchSeq: job.LaunchSeq, CaptureSeq: 1, Launch: job.LaunchID,
					State: CaptureRefused, Reason: takerStoppedWhy(takingProcess), Final: true,
					At: rig.coordinator.now(), Through: new(0)}
				if took, err := rig.pending.StampCapture(ctx, "t2", settled); err != nil || !took {
					t.Fatalf("stamping the settled capture = %v, %v", took, err)
				}
				if landed {
					late := CaptureRecord{Handle: "swe", Turn: "t2", ConversationKey: "chat:D1", WorkKey: "wk-1",
						Object: objstore.Object{Key: objstore.KeyAt(time.Now()),
							Hash: objstore.Hash(strings.Repeat("e", 64)), Size: 5},
						Stamp: SessionCapture{LaunchSeq: settled.LaunchSeq, CaptureSeq: settled.CaptureSeq,
							Harness: "claude-code", SessionID: "s-own", ArchiveFormat: FakeArchiveFormat}}
					if answer, err := logbook.Capture(ctx, late); err != nil || answer != LogLanded {
						t.Fatalf("the late record = %s, %v; want the log to take it", answer, err)
					}
				}
				continued, why := rig.coordinator.ContinuedCapture(ctx, "t2", rig.runner)
				if continued.Capture.CaptureSeq != 0 {
					t.Fatalf("the relaunch continues %+v, a capture its run's record settled refused",
						continued.Capture)
				}
				if !landed {
					if why != "" || continued.Base == nil || continued.Base.Turn != "b1" {
						t.Fatalf("the relaunch continues base %+v (%q), want the base the turn froze",
							continued.Base, why)
					}
					if continued.KeptTurn != "b1" {
						t.Errorf("the relaunch names %q to its run as a session to read, want the base its row "+
							"holds", continued.KeptTurn)
					}
					return
				}
				if continued.KeptTurn != "t2" {
					t.Errorf("the relaunch names %q to its run as a session to read, want the turn's own, whose "+
						"capture the log keeps", continued.KeptTurn)
				}
				switch {
				case continued.Base != nil:
					t.Fatalf("the relaunch continues base %+v beside a capture of the turn's own the log took",
						*continued.Base)
				case !strings.Contains(why, "the executor log keeps it all the same"):
					t.Errorf("the relaunch says %q, want it saying the log keeps the capture", why)
				case !strings.Contains(why, "landed a capture of its own, which lifted its hold") ||
					!strings.Contains(why, "no launch of the turn continues that one any more"):
					t.Errorf("the relaunch says %q, want why the base is not continued, naming no session "+
						"continued instead", why)
				}
			})
		})
	}
}

// A RELAUNCH OF A TURN THAT FROZE NO BASE READS ITS ROW FOR A CAPTURE ITS RECORD
// REFUSED. The turn continued no conversation's session, and its job's final
// capture is settled refused on its run's record — its taker's lease lapsed,
// the row naming no such capture as the settle read it — while the record that
// taker had handed on may land after. The relaunch continues it in neither
// order, as its record stamps it; but with no base to read the row for, it
// reads the row for that capture alone. Where the log has taken the record, its
// reason says the log keeps the capture, and its run is told the turn's own
// session is there to read; where the log has not, its reason is the record's
// own, and its run is told of nothing to read.
//
// Mutation: read the turn's row only for a base the turn froze, and the
// relaunch says a capture the log keeps was not kept, naming its run nothing
// to read.
func TestARelaunchOfATurnThatFrozeNoBaseReadsItsRowForACaptureItsRecordRefused(t *testing.T) {
	t.Parallel()
	for name, landed := range map[string]bool{
		"its record landed before the relaunch read the turn's row": true,
		"its record not yet landed as the relaunch read the row":    false,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				rig, logbook, _ := conversingRig(t)
				ctx := t.Context()
				if _, err := rig.coordinator.Launch(ctx, rig.manager, launchReq("t2")); err != nil {
					t.Fatalf("the first launch: %v", err)
				}
				job := rig.get("t2")
				if job.Base != nil {
					t.Fatalf("the premise: the first launch froze base %+v, want none", *job.Base)
				}
				settled := SessionCapture{LaunchSeq: job.LaunchSeq, CaptureSeq: 1, Launch: job.LaunchID,
					State: CaptureRefused, Reason: takerStoppedWhy(takingProcess), Final: true,
					At: rig.coordinator.now(), Through: new(0)}
				if took, err := rig.pending.StampCapture(ctx, "t2", settled); err != nil || !took {
					t.Fatalf("stamping the settled capture = %v, %v", took, err)
				}
				if landed {
					late := CaptureRecord{Handle: "swe", Turn: "t2", ConversationKey: "chat:D1", WorkKey: "wk-1",
						Object: objstore.Object{Key: objstore.KeyAt(time.Now()),
							Hash: objstore.Hash(strings.Repeat("e", 64)), Size: 5},
						Stamp: SessionCapture{LaunchSeq: settled.LaunchSeq, CaptureSeq: settled.CaptureSeq,
							Harness: "claude-code", SessionID: "s-own", ArchiveFormat: FakeArchiveFormat}}
					if answer, err := logbook.Capture(ctx, late); err != nil || answer != LogLanded {
						t.Fatalf("the late record = %s, %v; want the log to take it", answer, err)
					}
				}
				continued, why := rig.coordinator.ContinuedCapture(ctx, "t2", rig.runner)
				if continued.Capture.CaptureSeq != 0 || continued.Base != nil {
					t.Fatalf("the relaunch continues %+v, base %+v; want neither: its record settled the capture "+
						"refused, and the turn froze no base", continued.Capture, continued.Base)
				}
				if !strings.Contains(why, "was not kept: refused") {
					t.Errorf("the relaunch says %q, want the record's own account of the capture first", why)
				}
				keeps := strings.Contains(why, "the executor log keeps it all the same")
				switch {
				case landed && (!keeps || continued.KeptTurn != "t2"):
					t.Errorf("the relaunch says %q and names %q to its run to read; want the log keeping the "+
						"capture said, and the turn's own named", why, continued.KeptTurn)
				case !landed && (keeps || continued.KeptTurn != ""):
					t.Errorf("the relaunch says %q and names %q to its run to read; want neither, the log "+
						"keeping no capture of the turn", why, continued.KeptTurn)
				}
			})
		})
	}
}

// A RELAUNCH THAT WILL NOT WAIT OUT A KEEP CONTINUES THE BASE THE TURN STILL
// HOLDS. A capture still being kept once the relaunch's short wait has run out
// is no kept capture, and the relaunch goes as one whose job's keep was refused
// goes: a turn that has kept nothing, whose row still holds the base its first
// launch froze, continues that base, which holds everything a session of its
// own would and the conversation besides, and names it to its run as the
// session its read found. Where the keep reached the log ahead of its stamp,
// the row holds the base no longer: the relaunch continues none, saying why,
// and names its run the turn's own capture, which the row names, to read.
//
// Mutation: return before the frozen base where the capture is still being
// kept, and the relaunch of a turn that kept nothing starts a session of its
// own beside the base its row holds.
func TestARelaunchThatWillNotWaitOutAKeepContinuesTheBaseTheTurnStillHolds(t *testing.T) {
	t.Parallel()
	for name, landed := range map[string]bool{
		"the keep not yet on the log":            false,
		"the keep on the log ahead of its stamp": true,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			inBubble(t, func(t *testing.T) {
				rig, logbook, _ := conversingRig(t)
				rig.coordinator.relaunchWait = 50 * time.Millisecond
				read := frozenReadOf(logbook, logbook.endedTurn(t, "b1", "s-base", "the base's transcript"))
				ctx := t.Context()
				if _, err := rig.coordinator.Launch(ctx, rig.manager, continuingBase("t2", read)); err != nil {
					t.Fatalf("the first launch: %v", err)
				}
				job := rig.get("t2")
				// NO TAKER NAMED, as an older build's stamp: read by its age
				// alone, it is in flight for a capture's whole budget.
				begun := SessionCapture{LaunchSeq: job.LaunchSeq, CaptureSeq: 1, Launch: job.LaunchID,
					State: CaptureBegun, Final: true, At: rig.coordinator.now(), Through: new(0)}
				if took, err := rig.pending.StampCapture(ctx, "t2", begun); err != nil || !took {
					t.Fatalf("stamping the capture still being kept = %v, %v", took, err)
				}
				if landed {
					early := CaptureRecord{Handle: "swe", Turn: "t2", ConversationKey: "chat:D1", WorkKey: "wk-1",
						Object: objstore.Object{Key: objstore.KeyAt(time.Now()),
							Hash: objstore.Hash(strings.Repeat("e", 64)), Size: 5},
						Stamp: SessionCapture{LaunchSeq: begun.LaunchSeq, CaptureSeq: begun.CaptureSeq,
							Harness: "claude-code", SessionID: "s-own", ArchiveFormat: FakeArchiveFormat}}
					if answer, err := logbook.Capture(ctx, early); err != nil || answer != LogLanded {
						t.Fatalf("the keep's record = %s, %v; want the log to take it", answer, err)
					}
				}
				continued, why := rig.coordinator.ContinuedCapture(ctx, "t2", rig.runner)
				if continued.Capture.CaptureSeq != 0 {
					t.Fatalf("the relaunch continues %+v, a capture still being kept", continued.Capture)
				}
				if !landed {
					if why != "" || continued.Base == nil || continued.Base.Turn != "b1" || continued.KeptTurn != "b1" {
						t.Fatalf("the relaunch continues base %+v (%q), naming %q to read; want the base its row "+
							"still holds, continued and named", continued.Base, why, continued.KeptTurn)
					}
					return
				}
				switch {
				case continued.Base != nil:
					t.Fatalf("the relaunch continues base %+v beside a capture of the turn's own the log took",
						*continued.Base)
				case !strings.Contains(why, "still being kept") ||
					!strings.Contains(why, "landed a capture of its own, which lifted its hold"):
					t.Errorf("the relaunch says %q, want the keep still under way and why the base is not "+
						"continued", why)
				case continued.KeptTurn != "t2":
					t.Errorf("the relaunch names %q to its run as a session to read, want the turn's own, which "+
						"its row names", continued.KeptTurn)
				}
			})
		})
	}
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
			inBubble(t, func(t *testing.T) {
				rig, logbook, _ := conversingRig(t)
				read := frozenReadOf(logbook, logbook.endedTurn(t, "b1", "s-base", "the base's transcript"))
				if _, err := logbook.Begin(t.Context(), BeginRecord{Handle: "swe",
					Turn: "t2", ConversationKey: "chat:D1", WorkKey: "wk-1",
					ReadThrough: read.ReadThrough, Absorbs: read.Absorbs, BaseTurn: "b1",
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
