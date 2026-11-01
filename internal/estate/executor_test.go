package estate

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
)

// executorStream is the executor log's stream, as a floor names it.
var executorStream = floorStreams[executorDomain]

// The turns the cases speak of, run ids in their canonical spelling.
const (
	liveTurn  = "0192f3c4-1d2e-7a8b-9c0d-1e2f3a4b5c6d"
	endedTurn = "0192f3c4-1d2e-7a8b-9c0d-1e2f3a4b5c6e"
)

// conversation is the conversation the cases' turns are on.
const conversation = "C0FOUNDER/1700000000.000100"

// crossedAt is the instant the cases' rows and listings carry.
var crossedAt = time.Unix(1_700_000_000, 0).UTC()

// sessionKey is the object every case's session names: minted ONCE, because a
// key is random past its instant, and two answers that named two keys could
// not be compared.
var sessionKey = objstore.KeyAt(crossedAt)

// atLog is a position on the executor log.
func atLog(seq uint64) statelog.Position {
	return statelog.Position{Stream: executorStream, Generation: 1, Seq: seq}
}

// engineCall is a write's call as the engine makes it, under opID.
func engineCall(opID string) executordomain.Call {
	return executordomain.Call{
		By:   iam.Actor{Name: "executor", Kind: iam.ActorSystem, OperatorID: "node-1"},
		OpID: opID,
	}
}

// fullEnding is an ending with every field set.
func fullEnding() executordomain.Ending {
	return executordomain.Ending{Outcome: executordomain.OutcomeStopped, Decision: "done",
		Delivered: true, DeliveredTo: "chatlet", Answer: "answered", CompletedWork: "shipped",
		BlockedOn: "a review", StoppedBy: "jane", StopReason: "enough", Failure: "a guard"}
}

// sessionOf is one turn's row with every field set, so a field the wire
// dropped arrives as its zero value and is named.
func sessionOf(handle, turn, conversationKey string) executordomain.Session {
	ending := fullEnding()
	return executordomain.Session{
		Handle: handle, Turn: turn, ConversationKey: conversationKey, WorkKey: "wk-1",
		Harness: "claude-code", HarnessVersion: "2.1.0", SessionID: "sess-1",
		Object:     objstore.Object{Key: sessionKey, Hash: objstore.HashOf([]byte(turn)), Size: 42},
		ContentKey: "k1", ArchiveFormat: 1, LaunchSeq: 2, CaptureSeq: 3,
		Home: "/home/agent", Workspace: "/work", CapturedAt: crossedAt,
		ReadThrough: atLog(4), Absorbs: []string{endedTurn},
		EndedAt: crossedAt, EndedAtPosition: atLog(5),
		Ending:     &ending,
		AskExcerpt: "what " + handle + " was asked",
	}
}

// captureOf is a capture of one of swe's turns with every field set.
func captureOf(turn string) executordomain.NewCapture {
	return executordomain.NewCapture{
		Handle: "swe", Turn: turn, ConversationKey: conversation, WorkKey: "wk-1",
		Harness: "claude-code", HarnessVersion: "2.1.0", SessionID: "sess-1",
		Object:     objstore.Object{Key: sessionKey, Hash: objstore.HashOf([]byte(turn)), Size: 42},
		ContentKey: "k1", ArchiveFormat: 1, LaunchSeq: 1, CaptureSeq: 2,
		Home: "/home/agent", Workspace: "/work",
		ReadThrough: atLog(4), Absorbs: []string{endedTurn},
		SeatBudget: 64 << 20,
	}
}

// whole fails for every field of v, a struct, that arrived at its zero value:
// a field the wire dropped reads as zero on the far side, and says nothing.
func whole(t *testing.T, what string, v any) {
	t.Helper()
	rv := reflect.ValueOf(v)
	for i := range rv.NumField() {
		if rv.Field(i).IsZero() {
			t.Errorf("%s crossed without its %s", what, rv.Type().Field(i).Name)
		}
	}
}

// sameHere fails where a data node's own router answered a call with anything
// but what the routed call was answered with.
func sameHere[R any](t *testing.T, what string, routed, local R, err error) {
	t.Helper()
	if err != nil {
		t.Errorf("%s answered in-process: %v", what, err)
		return
	}
	if !reflect.DeepEqual(routed, local) {
		t.Errorf("%s answered in-process\n%+v\nand routed\n%+v\na consumer can tell the two "+
			"apart", what, local, routed)
	}
}

// serveExecutor makes local answer the fleet as the data node name, on q.
func serveExecutor(t *testing.T, q Server, name string, local LocalBackends) {
	t.Helper()
	stop, err := Serve(t.Context(), q, ServerOptions{Self: name, Local: local, Envelopes: fleetEnvelopes})
	if err != nil {
		t.Fatalf("serve %s: %v", name, err)
	}
	t.Cleanup(func() { _ = stop(context.Background()) })
}

// logbook is one data node's executor log as the crossing case reaches it:
// every read answers rows built from what it was asked, every write records the
// call and the input it was made with, and a few inputs answer the refusals a
// caller branches on.
type logbook struct {
	mu       sync.Mutex
	heard    []string
	calls    []executordomain.Call
	captured []executordomain.NewCapture
	ended    []executordomain.NewEnd
	writes   []string
	floors   []statelog.Position
	sealed   secrets.KeyCounts
}

func (l *logbook) For(context.Context) (Backend, bool) {
	return Backend{
		Executor: ExecutorHalf{Reader: l, Writer: l},
		Committed: func(_ context.Context, at statelog.Position) error {
			l.mu.Lock()
			defer l.mu.Unlock()
			l.floors = append(l.floors, at)
			return nil
		},
	}, true
}

// asked records a read and the level it crossed at.
func (l *logbook) asked(op string, fresh statelog.Freshness) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.heard = append(l.heard, op+":"+string(fresh.Level))
}

// wrote records a write and answers it applied, at one fixed position, so the
// same write answers the same wherever it is asked from.
func (l *logbook) wrote(op string, c executordomain.Call) statelog.Result {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.writes = append(l.writes, op)
	l.calls = append(l.calls, c)
	return statelog.Result{Outcome: statelog.OutcomeApplied, OpID: c.OpID, Position: atLog(10)}
}

func (l *logbook) Head(_ context.Context, handle, conversationKey string,
	fresh statelog.Freshness) (executordomain.Head, error) {
	l.asked("head", fresh)
	head, ended := sessionOf(handle, liveTurn, conversationKey), sessionOf(handle, endedTurn, conversationKey)
	return executordomain.Head{Session: &head, Endings: []executordomain.Session{ended}, EndingsOmitted: 2,
		Siblings: []executordomain.Session{ended}, SiblingsOmitted: 1, Position: atLog(9)}, nil
}

func (l *logbook) Session(_ context.Context, handle, turn string,
	fresh statelog.Freshness) (executordomain.Session, error) {
	l.asked("session", fresh)
	if turn == endedTurn {
		return executordomain.Session{}, fmt.Errorf("%w: %s's turn %s", executordomain.ErrNotFound,
			handle, turn)
	}
	return sessionOf(handle, turn, conversation), nil
}

func (l *logbook) Sessions(_ context.Context, handle, cursor string, limit int,
	fresh statelog.Freshness) (executordomain.Page, error) {
	l.asked("sessions", fresh)
	return executordomain.Page{Sessions: []executordomain.Session{sessionOf(handle, liveTurn, conversation)},
		Next: fmt.Sprintf("%s+%d", cursor, limit), Position: atLog(8), Complete: true}, nil
}

func (l *logbook) Expired(_ context.Context, cutoff time.Time, limit int) ([]executordomain.Expiry, error) {
	return []executordomain.Expiry{{Handle: "swe", ConversationKey: fmt.Sprint(limit),
		Turn: endedTurn, At: cutoff}}, nil
}

func (l *logbook) Unended(_ context.Context, before time.Time, limit int) ([]executordomain.Expiry, error) {
	return []executordomain.Expiry{{Handle: "swe", ConversationKey: fmt.Sprint(limit),
		Turn: liveTurn, At: before}}, nil
}

func (l *logbook) Handles(_ context.Context, fresh statelog.Freshness) ([]string, error) {
	l.asked("handles", fresh)
	if fresh.Level == statelog.ReadStale {
		return nil, fmt.Errorf("%w: list the seats holding sessions", executordomain.ErrIncomplete)
	}
	return []string{"cto", "swe"}, nil
}

func (l *logbook) SealedKeys(context.Context) (executordomain.SealedCount, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return executordomain.SealedCount{Values: maps.Clone(l.sealed)}, nil
}

func (l *logbook) Capture(_ context.Context, c executordomain.Call,
	n executordomain.NewCapture) (executordomain.CaptureResult, error) {
	if n.Turn == endedTurn {
		return executordomain.CaptureResult{}, fmt.Errorf("%w: %s's turn %s", executordomain.ErrTurnEnded,
			n.Handle, n.Turn)
	}
	res := l.wrote("capture", c)
	l.mu.Lock()
	l.captured = append(l.captured, n)
	l.mu.Unlock()
	return executordomain.CaptureResult{Result: res, Recorded: true}, nil
}

func (l *logbook) End(_ context.Context, c executordomain.Call,
	n executordomain.NewEnd) (statelog.Result, error) {
	res := l.wrote("end", c)
	l.mu.Lock()
	l.ended = append(l.ended, n)
	l.mu.Unlock()
	return res, nil
}

func (l *logbook) EndLost(_ context.Context, c executordomain.Call, handle, conversationKey, turn string,
	before time.Time) (statelog.Result, error) {
	return l.wrote(fmt.Sprintf("end_lost:%s:%s:%s:%s", handle, conversationKey, turn,
		before.Format(time.RFC3339)), c), nil
}

func (l *logbook) DeleteExpired(_ context.Context, c executordomain.Call, handle, conversationKey,
	turn string, cutoff time.Time) (statelog.Result, error) {
	if turn == liveTurn {
		return statelog.Result{}, fmt.Errorf("%w: %s's turn %s was captured after it was listed",
			executordomain.ErrNotDue, handle, turn)
	}
	return l.wrote(fmt.Sprintf("delete_expired:%s:%s:%s:%s", handle, conversationKey, turn,
		cutoff.Format(time.RFC3339)), c), nil
}

func (l *logbook) ExpireRemoval(_ context.Context, c executordomain.Call, handle string,
	cutoff time.Time) (statelog.Result, error) {
	return l.wrote(fmt.Sprintf("expire_removal:%s:%s", handle, cutoff.Format(time.RFC3339)), c), nil
}

func (l *logbook) DeleteSeat(_ context.Context, c executordomain.Call, handle string) (statelog.Result, error) {
	return l.wrote("delete_seat:"+handle, c), nil
}

// EVERY EXECUTOR OPERATION CROSSES WHOLE, AND A CONSUMER CANNOT TELL LOCAL FROM
// ROUTED.
//
// A node without `data` reaches a data node's executor log with every argument
// (the engine's call, its author and operation id included) and every answer
// (a row's object, its ending, the position a head was read at), each refusal
// keeping the identity the engine branches on, and each read crossing at the
// level its caller asked. And a data node's own router, answering the same
// calls in-process from the same copy, answers each exactly as the routed call
// was answered: a session count that is empty included, which the wire leaves
// out.
//
// Mutation: drop a field from an operation's args type, drop the empty-map
// answer from ExecutorReads.SealedKeys, or a sentinel from the registry, and a
// row goes red.
func TestEveryExecutorOperationCrossesWhole(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	book := &logbook{sealed: secrets.KeyCounts{"k0": 1, "k1": 2}}
	serveExecutor(t, f.start(t), "data-x", book)
	f.placement.set(func(p *fakePlacement) { p.nodes = []string{"data-x"} })
	// THE SAME COPY ANSWERED IN-PROCESS, by a data node that asks nobody
	// else: a walk that left it would find no node and fail.
	local, err := NewRouter(RouterOptions{Self: "data-self", Queue: f.start(t),
		Placement: &fakePlacement{}, Session: NewSession(), Envelopes: fleetEnvelopes, Local: book})
	if err != nil {
		t.Fatal(err)
	}
	ctx := t.Context()
	reads, here := f.client.ExecutorReads(), local.ExecutorReads()

	linearizable := statelog.Freshness{Level: statelog.ReadLinearizable}
	head, err := reads.Head(ctx, "swe", conversation, linearizable)
	if err != nil || head.Session == nil || len(head.Endings) != 1 || len(head.Siblings) != 1 ||
		head.Position != atLog(9) {
		t.Fatalf("head = (%+v, %v)", head, err)
	}
	whole(t, "a head", head)
	whole(t, "the head's session", *head.Session)
	whole(t, "the head's ending", *head.Session.Ending)
	whole(t, "an ending", head.Endings[0])
	hereHead, err := here.Head(ctx, "swe", conversation, linearizable)
	sameHere(t, "head", head, hereHead, err)

	sessionLevel := statelog.Freshness{Level: statelog.ReadSession}
	row, err := reads.Session(ctx, "swe", liveTurn, sessionLevel)
	if err != nil || row.Turn != liveTurn || row.Handle != "swe" {
		t.Errorf("session = (%+v, %v)", row, err)
	}
	whole(t, "a session", row)
	hereRow, err := here.Session(ctx, "swe", liveTurn, sessionLevel)
	sameHere(t, "session", row, hereRow, err)
	if _, err := reads.Session(ctx, "swe", endedTurn, sessionLevel); !errors.Is(err,
		executordomain.ErrNotFound) {
		t.Errorf("a turn with no row crossed as %v", err)
	}

	prefix := statelog.Freshness{Level: statelog.ReadConsistentPrefix}
	page, err := reads.Sessions(ctx, "swe", "c", 5, prefix)
	if err != nil || page.Next != "c+5" || len(page.Sessions) != 1 {
		t.Errorf("sessions = (%+v, %v)", page, err)
	}
	whole(t, "a page", page)
	herePage, err := here.Sessions(ctx, "swe", "c", 5, prefix)
	sameHere(t, "sessions", page, herePage, err)

	expired, err := reads.Expired(ctx, crossedAt, 7)
	if err != nil || len(expired) != 1 || !expired[0].At.Equal(crossedAt) ||
		expired[0].ConversationKey != "7" || expired[0].Turn != endedTurn {
		t.Errorf("expired = (%+v, %v)", expired, err)
	}
	hereExpired, err := here.Expired(ctx, crossedAt, 7)
	sameHere(t, "expired", expired, hereExpired, err)

	unended, err := reads.Unended(ctx, crossedAt, 3)
	if err != nil || len(unended) != 1 || !unended[0].At.Equal(crossedAt) ||
		unended[0].ConversationKey != "3" || unended[0].Turn != liveTurn {
		t.Errorf("unended = (%+v, %v)", unended, err)
	}
	hereUnended, err := here.Unended(ctx, crossedAt, 3)
	sameHere(t, "unended", unended, hereUnended, err)

	handles, err := reads.Handles(ctx, linearizable)
	if err != nil || !slices.Equal(handles, []string{"cto", "swe"}) {
		t.Errorf("handles = (%v, %v)", handles, err)
	}
	if _, err := reads.Handles(ctx, statelog.Freshness{Level: statelog.ReadStale}); !errors.Is(err,
		executordomain.ErrIncomplete) {
		t.Errorf("a listing a node cannot answer whole crossed as %v, which a retirement would "+
			"read as a shorter list", err)
	}

	count, err := reads.SealedKeys(ctx)
	if err != nil || count.Values["k0"] != 1 || count.Values["k1"] != 2 || count.Outside("k1") != 1 {
		t.Errorf("sealed keys = (%+v, %v)", count, err)
	}
	book.mu.Lock()
	book.sealed = secrets.KeyCounts{}
	book.mu.Unlock()
	none, err := reads.SealedKeys(ctx)
	if err != nil || none.Values == nil || len(none.Values) != 0 {
		t.Errorf("an empty count crossed as (%+v, %v), want an empty map as the domain answers it", none, err)
	}
	hereNone, err := here.SealedKeys(ctx)
	sameHere(t, "an empty count", none, hereNone, err)

	writes, hereWrites := f.client.ExecutorWrites(), local.ExecutorWrites()
	call := engineCall("op-capture")
	capture := captureOf(liveTurn)
	whole(t, "the case's capture", capture)
	captured, err := writes.Capture(ctx, call, capture)
	if err != nil || !captured.Recorded || captured.Outcome != statelog.OutcomeApplied ||
		captured.OpID != call.OpID || captured.Position != atLog(10) {
		t.Errorf("capture = (%+v, %v)", captured, err)
	}
	hereCaptured, err := hereWrites.Capture(ctx, call, capture)
	sameHere(t, "capture", captured, hereCaptured, err)
	if _, err := writes.Capture(ctx, call, captureOf(endedTurn)); !errors.Is(err,
		executordomain.ErrTurnEnded) {
		t.Errorf("a capture of an ended turn's refusal crossed as %v", err)
	}

	ending := fullEnding()
	whole(t, "the case's ending", ending)
	end := executordomain.NewEnd{Handle: "swe", Turn: liveTurn, ConversationKey: conversation,
		WorkKey: "wk-1", Ask: "ship it", Ending: ending}
	endCall := engineCall("op-end")
	ended, err := writes.End(ctx, endCall, end)
	if err != nil || ended.OpID != endCall.OpID || ended.Outcome != statelog.OutcomeApplied {
		t.Errorf("end = (%+v, %v)", ended, err)
	}
	hereEnded, err := hereWrites.End(ctx, endCall, end)
	sameHere(t, "end", ended, hereEnded, err)

	for name, write := range map[string]func() (statelog.Result, error){
		"end_lost:swe:" + conversation + ":" + liveTurn + ":2023-11-14T22:13:20Z": func() (statelog.Result, error) {
			return writes.EndLost(ctx, call, "swe", conversation, liveTurn, crossedAt)
		},
		"delete_expired:swe:" + conversation + ":" + endedTurn + ":2023-11-14T22:13:20Z": func() (statelog.Result, error) {
			return writes.DeleteExpired(ctx, call, "swe", conversation, endedTurn, crossedAt)
		},
		"expire_removal:cto:2023-11-14T22:13:20Z": func() (statelog.Result, error) {
			return writes.ExpireRemoval(ctx, call, "cto", crossedAt)
		},
		"delete_seat:cto": func() (statelog.Result, error) { return writes.DeleteSeat(ctx, call, "cto") },
	} {
		if res, err := write(); err != nil || res.Outcome != statelog.OutcomeApplied {
			t.Errorf("%s = (%+v, %v)", name, res, err)
		}
		book.mu.Lock()
		ran := slices.Contains(book.writes, name)
		book.mu.Unlock()
		if !ran {
			t.Errorf("the serving node never ran %s with the arguments the engine gave it", name)
		}
	}
	if _, err := writes.DeleteExpired(ctx, call, "swe", conversation, liveTurn, crossedAt); !errors.Is(err,
		executordomain.ErrNotDue) {
		t.Errorf("a delete no longer due crossed as %v", err)
	}

	book.mu.Lock()
	defer book.mu.Unlock()
	for _, want := range []string{"head:linearizable", "session:session", "sessions:consistent_prefix",
		"handles:linearizable", "handles:stale"} {
		if !slices.Contains(book.heard, want) {
			t.Errorf("the serving node never read %s, as the caller asked: heard %v", want, book.heard)
		}
	}
	for i, got := range book.captured {
		if !reflect.DeepEqual(got, capture) {
			t.Errorf("capture %d ran as %+v, want the engine's %+v", i, got, capture)
		}
	}
	for i, got := range book.ended {
		if !reflect.DeepEqual(got, end) {
			t.Errorf("end %d ran as %+v, want the engine's %+v", i, got, end)
		}
	}
	for i, got := range book.calls {
		if got.By != call.By || (got.OpID != call.OpID && got.OpID != endCall.OpID) {
			t.Errorf("write %s ran under %+v, want the engine's own call", book.writes[i], got)
		}
	}
}

// A SEATS-ONLY NODE'S CAPTURE IS RECORDED THROUGH A DATA NODE, and its
// position is that node's next floor on the executor log.
//
// A node without `data` holds no copy and no publisher, and a seat it holds ran
// its box there: with no route, the session's object is uploaded, nothing
// names it, and the collector deletes it a day later. The capture runs on the
// data node under the engine's call whole — its author and the operation the
// domain derived — and the next read this node sends, on whichever data node
// answers it, waits to have applied the capture.
//
// Mutation: drop the capture's Observe, or declare executor.session with no
// domain, and the next read carries no floor.
func TestASeatsOnlyNodesCaptureIsRecordedThroughADataNode(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	book := &logbook{}
	serveExecutor(t, f.start(t), "data-a", book)
	f.placement.set(func(p *fakePlacement) { p.nodes = []string{"data-a"} })
	call := engineCall(executordomain.CaptureOpID(crossedAt, "swe", liveTurn, "launch-1", 1))

	got, err := f.client.ExecutorWrites().Capture(t.Context(), call, captureOf(liveTurn))
	if err != nil || !got.Recorded || got.Outcome != statelog.OutcomeApplied {
		t.Fatalf("the capture answered (%+v, %v), want it recorded", got, err)
	}
	book.mu.Lock()
	calls := slices.Clone(book.calls)
	book.mu.Unlock()
	if len(calls) != 1 || calls[0] != call {
		t.Fatalf("the data node ran %+v, want the one capture under the engine's call %+v", calls, call)
	}

	if _, err := f.client.ExecutorReads().Session(t.Context(), "swe", liveTurn,
		statelog.Freshness{Level: statelog.ReadSession}); err != nil {
		t.Fatalf("the read after the capture: %v", err)
	}
	book.mu.Lock()
	floors := slices.Clone(book.floors)
	book.mu.Unlock()
	if !slices.Contains(floors, got.Position) {
		t.Errorf("the read after the capture waited for %v, want the capture's own %v", floors, got.Position)
	}
}

// sessionLog is the executor log as the cases' data nodes share it: the
// operation ledger a repeat is answered from, every operation id any node ran,
// and the turns ended on it in log order — what every copy applies.
type sessionLog struct {
	mu     sync.Mutex
	ledger map[string]statelog.Result
	ran    []string
	ended  []string
}

func newSessionLog() *sessionLog { return &sessionLog{ledger: map[string]statelog.Result{}} }

// record runs one write under opID as the log's ledger does: the first copy
// appends a record — effect is what it does to the log, run under the log's
// lock — and a repeat is answered with the first copy's result, collapsed.
func (l *sessionLog) record(opID string, effect func()) statelog.Result {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ran = append(l.ran, opID)
	if first, ok := l.ledger[opID]; ok {
		first.Collapsed = true
		return first
	}
	if effect != nil {
		effect()
	}
	res := statelog.Result{Outcome: statelog.OutcomeApplied, OpID: opID,
		Position: atLog(uint64(len(l.ledger) + 1))}
	l.ledger[opID] = res
	return res
}

// replica is one data node's copy of the log, applied up to nothing yet.
func (l *sessionLog) replica() *sessionCopy {
	return &sessionCopy{log: l, applying: make(chan struct{})}
}

// sessionCopy is one data node's copy of a [sessionLog]: the executor half it
// serves, how far it has applied the log, and how it misbehaves.
type sessionCopy struct {
	// The reads and writes no case here asks: a call to one is a case
	// asking what this fake does not model, and panics on the nil.
	ExecutorReader
	ExecutorWriter

	log *sessionLog

	mu sync.Mutex
	// heard is every operation this copy ran, as "<op>:<detail>".
	heard []string
	// applied is how many of the log's ends this copy has applied, and
	// applying is closed and replaced each time it moves.
	applied  int
	applying chan struct{}
	// unvouched answers every write `unknown`, unvouched by its ledger, and
	// runs nothing.
	unvouched bool
	// losing loses every reply this copy's server sends ([lossy]).
	losing bool
}

func (c *sessionCopy) For(context.Context) (Backend, bool) {
	return Backend{Executor: ExecutorHalf{Reader: c, Writer: c}}, true
}

func (c *sessionCopy) note(what string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.heard = append(c.heard, what)
}

func (c *sessionCopy) asked() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.heard)
}

func (c *sessionCopy) set(change func(*sessionCopy)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	change(c)
}

func (c *sessionCopy) vouches() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.unvouched
}

func (c *sessionCopy) Capture(_ context.Context, call executordomain.Call,
	_ executordomain.NewCapture) (executordomain.CaptureResult, error) {
	c.note("capture:" + call.OpID)
	if !c.vouches() {
		return executordomain.CaptureResult{Result: statelog.Result{Outcome: statelog.OutcomeUnknown,
			OpID: call.OpID, Unvouched: true}}, nil
	}
	return executordomain.CaptureResult{Result: c.log.record(call.OpID, nil), Recorded: true}, nil
}

func (c *sessionCopy) End(_ context.Context, call executordomain.Call,
	n executordomain.NewEnd) (statelog.Result, error) {
	c.note("end:" + call.OpID)
	if !c.vouches() {
		return statelog.Result{Outcome: statelog.OutcomeUnknown, OpID: call.OpID, Unvouched: true}, nil
	}
	return c.log.record(call.OpID, func() { c.log.ended = append(c.log.ended, n.Turn) }), nil
}

// Head answers the conversation as this copy has applied it — at
// `linearizable` only once it has applied every end the log held when the read
// began, which is what the read index's barrier holds a read to.
func (c *sessionCopy) Head(ctx context.Context, handle, conversationKey string,
	fresh statelog.Freshness) (executordomain.Head, error) {
	c.note("head:" + string(fresh.Level))
	if fresh.Level == statelog.ReadLinearizable {
		c.log.mu.Lock()
		barrier := len(c.log.ended)
		c.log.mu.Unlock()
		if err := c.reach(ctx, barrier); err != nil {
			return executordomain.Head{}, err
		}
	}
	c.mu.Lock()
	applied := c.applied
	c.mu.Unlock()
	c.log.mu.Lock()
	ended := slices.Clone(c.log.ended[:applied])
	c.log.mu.Unlock()
	out := executordomain.Head{Position: atLog(uint64(applied))}
	for _, turn := range ended {
		out.Endings = append(out.Endings, executordomain.Session{Handle: handle, Turn: turn,
			ConversationKey: conversationKey,
			Ending:          &executordomain.Ending{Outcome: executordomain.OutcomeCompleted}})
	}
	return out, nil
}

// reach waits until this copy has applied n of the log's ends.
func (c *sessionCopy) reach(ctx context.Context, n int) error {
	for {
		c.mu.Lock()
		applied, applying := c.applied, c.applying
		c.mu.Unlock()
		if applied >= n {
			return nil
		}
		select {
		case <-applying:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// apply brings this copy level with the log.
func (c *sessionCopy) apply() {
	c.log.mu.Lock()
	n := len(c.log.ended)
	c.log.mu.Unlock()
	c.mu.Lock()
	defer c.mu.Unlock()
	c.applied = n
	close(c.applying)
	c.applying = make(chan struct{})
}

// lossy is a data node's server that runs every request and, while its copy is
// losing, loses the reply on its way back: the write ran, and the asking node
// never heard.
type lossy struct {
	q    *memory.Queue
	node *sessionCopy
}

func (l lossy) Serve(ctx context.Context, subject string, h queue.AnswerFunc) (queue.Unsubscribe, error) {
	return l.q.Serve(ctx, subject, func(ctx context.Context, raw []byte) ([]byte, error) {
		answer, err := h(ctx, raw)
		l.node.mu.Lock()
		losing := l.node.losing
		l.node.mu.Unlock()
		if losing {
			return nil, errors.New("the reply was lost on its way back")
		}
		return answer, err
	})
}

// twoCopies is a fleet whose two data nodes, data-a and data-b, serve copies
// of one shared log through lossy servers, and the copy the fleet's client
// asks first.
func twoCopies(t *testing.T) (f *fleet, shared *sessionLog, copies map[string]*sessionCopy, first *sessionCopy) {
	t.Helper()
	f = newFleet(t)
	shared = newSessionLog()
	copies = map[string]*sessionCopy{}
	for _, name := range []string{"data-a", "data-b"} {
		copies[name] = shared.replica()
		serveExecutor(t, lossy{q: f.start(t), node: copies[name]}, name, copies[name])
	}
	f.placement.set(func(p *fakePlacement) { p.nodes = []string{"data-a", "data-b"} })
	firstName, _ := f.first(t, f.client)
	return f, shared, copies, copies[firstName]
}

// AN UNANSWERED CAPTURE IS ASKED AGAIN UNDER THE SAME OPERATION, AND LANDS
// ONCE.
//
// The first data node runs the capture and its reply is lost: it landed. The
// router asks the next data node under the same operation id — the one the
// domain derived from the turn, its launch and the capture's ordinal — and
// that node's ledger answers it with the first copy's outcome rather than
// appending a second record. A once-write would have stopped at the silent
// node, unknown; a write asked again under a fresh id would have landed twice.
//
// Mutation: declare executor.capture an opOnceWrite and the capture answers
// ErrOutcomeUnknown, the second node never asked.
func TestAnUnansweredCaptureIsAskedAgainUnderTheSameOperationAndLandsOnce(t *testing.T) {
	t.Parallel()
	f, shared, copies, first := twoCopies(t)
	first.set(func(c *sessionCopy) { c.losing = true })
	call := engineCall(executordomain.CaptureOpID(crossedAt, "swe", liveTurn, "launch-1", 1))

	got, err := f.client.ExecutorWrites().Capture(t.Context(), call, captureOf(liveTurn))
	if err != nil {
		t.Fatalf("a capture whose first node lost its reply failed: %v", err)
	}
	if got.Outcome != statelog.OutcomeApplied || !got.Recorded || !got.Collapsed ||
		got.Position != atLog(1) {
		t.Errorf("the capture answered %+v, want the first copy's record, collapsed onto it", got.Result)
	}
	for name, c := range copies {
		if asked := c.asked(); !slices.Equal(asked, []string{"capture:" + call.OpID}) {
			t.Errorf("%s ran %v, want the one capture under %s", name, asked, call.OpID)
		}
	}
	shared.mu.Lock()
	records, ran := len(shared.ledger), slices.Clone(shared.ran)
	shared.mu.Unlock()
	if records != 1 || !slices.Equal(ran, []string{call.OpID, call.OpID}) {
		t.Errorf("the log holds %d records from the operations %v, want one record from %s asked twice",
			records, ran, call.OpID)
	}
}

// AN UNVOUCHED EXECUTOR WRITE MOVES ON, UNDER THE SAME OPERATION.
//
// A data node whose ledger cannot say whether a write landed answers it
// `unknown`, unvouched, and another node's ledger may hold it: so the router
// asks the next under the same operation id, and the vouching node's answer is
// the one the engine gets. Where no node vouches, the answer is that unknown —
// never an estate nobody served, which says nothing ran and invites a second
// write under a fresh id.
//
// Mutation: declare executor.capture an opOnceWrite and the first node's
// unvouched unknown is the answer.
func TestAnUnvouchedExecutorWriteMovesOnUnderTheSameOperation(t *testing.T) {
	t.Parallel()
	f, _, copies, first := twoCopies(t)
	first.set(func(c *sessionCopy) { c.unvouched = true })
	call := engineCall(executordomain.CaptureOpID(crossedAt, "swe", liveTurn, "launch-1", 2))

	got, err := f.client.ExecutorWrites().Capture(t.Context(), call, captureOf(liveTurn))
	if err != nil || got.Outcome != statelog.OutcomeApplied || got.Unvouched || !got.Recorded {
		t.Fatalf("the capture answered (%+v, %v), want the vouching node's applied record", got, err)
	}
	for name, c := range copies {
		if !slices.Contains(c.asked(), "capture:"+call.OpID) {
			t.Errorf("%s ran %v, want the capture under %s", name, c.asked(), call.OpID)
		}
	}

	// NOBODY VOUCHES: the answer is the unknown, never an unserved estate.
	for _, c := range copies {
		c.set(func(c *sessionCopy) { c.unvouched = true })
	}
	endCall := engineCall(executordomain.EndOpID(crossedAt, "swe", liveTurn))
	res, err := f.client.ExecutorWrites().End(t.Context(), endCall, executordomain.NewEnd{
		Handle: "swe", Turn: liveTurn, ConversationKey: conversation,
		Ending: executordomain.Ending{Outcome: executordomain.OutcomeCompleted}})
	var unserved *ErrUnserved
	if err != nil || errors.As(err, &unserved) || res.Outcome != statelog.OutcomeUnknown ||
		!res.Unvouched || res.OpID != endCall.OpID {
		t.Fatalf("with no node vouching the end answered (%+v, %v), want its unvouched unknown", res, err)
	}
	for name, c := range copies {
		if !slices.Contains(c.asked(), "end:"+endCall.OpID) {
			t.Errorf("%s ran %v, want the end under %s", name, c.asked(), endCall.OpID)
		}
	}
}

// THE CONVERSATION READ WAITS FOR AN END ANOTHER NODE MADE.
//
// A seat that moved holds no floor for the end its previous turn published
// through another node: this node never observed it. So a launch reads its
// conversation at `linearizable`, and the level crosses whole to whichever data
// node answers, whose read index's barrier holds the read until its copy has
// applied every record the log held, the end among them. Read at `session`,
// with no floor to wait for, the same copy answers from before the end — the
// older head a launch must never continue.
//
// Mutation: drop Fresh from executorHeadArgs and the linearizable read answers
// at once, without the end.
func TestAConversationReadWaitsForAnEndAnotherNodeMade(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	shared := newSessionLog()
	data := shared.replica()
	serveExecutor(t, f.start(t), "data-a", data)
	f.placement.set(func(p *fakePlacement) { p.nodes = []string{"data-a"} })
	// THE NODE THE SEAT LEFT ends its turn; the node it moved to launches.
	left, moved := f.router(t, "agent-left", nil), f.router(t, "agent-moved", nil)
	moved.readBudget = 10 * time.Second
	ctx := t.Context()
	if _, err := left.ExecutorWrites().End(ctx, engineCall(executordomain.EndOpID(crossedAt, "swe", liveTurn)),
		executordomain.NewEnd{Handle: "swe", Turn: liveTurn, ConversationKey: conversation,
			Ending: executordomain.Ending{Outcome: executordomain.OutcomeCompleted}}); err != nil {
		t.Fatalf("end: %v", err)
	}

	behind, err := moved.ExecutorReads().Head(ctx, "swe", conversation,
		statelog.Freshness{Level: statelog.ReadSession})
	if err != nil || len(behind.Endings) != 0 {
		t.Fatalf("a session read with no floor answered (%+v, %v), want the copy as it stands, "+
			"before the end — without that this case proves nothing", behind, err)
	}

	type answer struct {
		head executordomain.Head
		err  error
	}
	read := make(chan answer, 1)
	go func() {
		head, err := moved.ExecutorReads().Head(ctx, "swe", conversation,
			statelog.Freshness{Level: statelog.ReadLinearizable})
		read <- answer{head, err}
	}()
	select {
	case got := <-read:
		t.Fatalf("the linearizable read answered (%+v, %v) before the copy applied the end",
			got.head, got.err)
	case <-time.After(100 * time.Millisecond):
	}
	data.apply()
	select {
	case got := <-read:
		if got.err != nil || len(got.head.Endings) != 1 || got.head.Endings[0].Turn != liveTurn {
			t.Errorf("the linearizable read answered (%+v, %v), want the end another node made",
				got.head, got.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the linearizable read never answered once the copy applied the end")
	}
	if heard := data.asked(); !slices.Contains(heard, "head:linearizable") {
		t.Errorf("the data node read %v, never at the level the launch asked", heard)
	}
}

// THE FACADES ARE THE DOMAIN'S OWN METHOD SETS, EXACTLY: the reader's and the
// writer's, method for method and signature for signature, and so are the
// halves a data node serves the log with. A consumer cannot tell local from
// routed only while it can make through the facade every call the domain's
// own side answers: a method the domain gained and the facade lacks is a call
// the engine makes in-process on a data node and cannot make on a node
// without `data`.
//
// Mutation: add a method to executordomain.Reader, or drop one from
// ExecutorReads, and the reader's row names it.
func TestTheExecutorFacadesAreTheDomainsMethodSets(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		domain reflect.Type
		others []reflect.Type
	}{
		{reflect.TypeFor[*executordomain.Reader](),
			[]reflect.Type{reflect.TypeFor[ExecutorReader](), reflect.TypeFor[ExecutorReads]()}},
		{reflect.TypeFor[*executordomain.Writer](),
			[]reflect.Type{reflect.TypeFor[ExecutorWriter](), reflect.TypeFor[ExecutorWrites]()}},
	} {
		want := methodSet(tc.domain)
		if len(want) == 0 {
			t.Fatalf("%s has no exported method, so this compares nothing", tc.domain)
		}
		for _, other := range tc.others {
			got := methodSet(other)
			for _, name := range slices.Sorted(maps.Keys(want)) {
				if got[name] != want[name] {
					t.Errorf("%s.%s is %q, want the domain's %q", other, name, got[name], want[name])
				}
			}
			for _, name := range slices.Sorted(maps.Keys(got)) {
				if _, ok := want[name]; !ok {
					t.Errorf("%s.%s is no method of %s", other, name, tc.domain)
				}
			}
		}
	}
}

// methodSet is a type's exported methods, each by its signature without the
// receiver.
func methodSet(ty reflect.Type) map[string]string {
	out := map[string]string{}
	for i := range ty.NumMethod() {
		m := ty.Method(i)
		first := 1 // the receiver
		if ty.Kind() == reflect.Interface {
			first = 0
		}
		var in, results []string
		for j := first; j < m.Type.NumIn(); j++ {
			in = append(in, m.Type.In(j).String())
		}
		for j := range m.Type.NumOut() {
			results = append(results, m.Type.Out(j).String())
		}
		out[m.Name] = "(" + strings.Join(in, ", ") + ") (" + strings.Join(results, ", ") + ")"
	}
	return out
}

// THE FACADES ASK EXACTLY THE EXECUTOR LOG'S OPERATIONS, each method one:
// every executor read through the reader facade, every write the engine
// originates through the writer facade, and nothing else through either. The
// method sets match the domain's (above); this holds each method to an
// operation of its own class, so a facade method wired to a surface's
// operation, or one that asks nothing, is named.
//
// Mutation: point ExecutorWrites.End at opExecutorEndLost and the writer's
// operations lack executor.end.
func TestTheExecutorFacadesAskExactlyTheExecutorOperations(t *testing.T) {
	t.Parallel()
	asker := &recordingAsker{answer: reply{Node: "data-a", Unserved: unservedNoBackend,
		Detail: "data-a runs no native backend for it"}}
	r, err := NewRouter(RouterOptions{Self: "agent-1", Queue: asker,
		Placement: &fakePlacement{nodes: []string{"data-a"}}, Session: NewSession(),
		Envelopes: fleetEnvelopes})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		facade any
		want   func(*opSpec) bool
	}{
		{r.ExecutorReads(), func(s *opSpec) bool { return s.domain == executorDomain && s.class == opRead }},
		{r.ExecutorWrites(), func(s *opSpec) bool { return s.origin == originEngine }},
	} {
		v := reflect.ValueOf(tc.facade)
		got := map[string]bool{}
		for i := range v.NumMethod() {
			method, m := v.Type().Method(i).Name, v.Method(i)
			in := make([]reflect.Value, m.Type().NumIn())
			in[0] = reflect.ValueOf(t.Context())
			for j := 1; j < len(in); j++ {
				in[j] = reflect.Zero(m.Type().In(j))
			}
			before := len(asker.requests())
			m.Call(in)
			sent := asker.requests()[before:]
			if len(sent) != 1 {
				t.Errorf("%s.%s asked %d operations, want one", v.Type().Name(), method, len(sent))
				continue
			}
			got[opened(sent[0].payload).Op] = true
		}
		want := map[string]bool{}
		for name, spec := range registry {
			if tc.want(spec) {
				want[name] = true
			}
		}
		if len(want) == 0 || !maps.Equal(got, want) {
			t.Errorf("%s asks %v, want exactly %v", v.Type().Name(),
				slices.Sorted(maps.Keys(got)), slices.Sorted(maps.Keys(want)))
		}
	}
}
