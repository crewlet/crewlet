package changefeed_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/doclet"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"github.com/crewlet/crewlet/internal/worklet"
)

// ---- the fixture ------------------------------------------------------ //

// settled is what a feed did with the messages one case handed it: its acks,
// its naks with their delays, and — the order the dead-letter path promises —
// whether the record's dead letter was already kept at each ack.
type settled struct {
	mu        sync.Mutex
	acks      int
	naks      []time.Duration
	keptAtAck []bool
}

func (s *settled) read() (int, []time.Duration, []bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.acks, append([]time.Duration(nil), s.naks...), append([]bool(nil), s.keptAtAck...)
}

// node is one feed under a case, settled synchronously ([changefeed.Handle]):
// its translator, what it published, its recorder and its clock, over a
// dead-letter store and claims a case may share between nodes.
type node struct {
	feed    *changefeed.Feed
	tr      *probe
	docs    *estate
	pub     *capture
	rec     *metrics.Recorder
	clock   *clock
	letters *memory.Queue
}

// fleet is what the nodes of one case share: the dead-letter store — the
// queue's own, the memory twin the conformance suite certifies beside the
// broker the engine runs — the claims, and how many frames the store has
// been asked for.
type fleet struct {
	letters *memory.Queue
	claims  *claims
	frames  *atomic.Int64
}

func newFleet(t *testing.T) fleet {
	t.Helper()
	q := memory.New()
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("start the dead-letter store: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
	return fleet{letters: q, claims: newClaims(), frames: new(atomic.Int64)}
}

// framesRead is the queue's dead-letter verbs, counting every frame read: the
// one read that moves an entry's body, which only a replay makes.
type framesRead struct {
	*memory.Queue
	frames *atomic.Int64
}

func (f framesRead) DeadLetterFrame(ctx context.Context, subject string, seq uint64) ([]byte, bool, error) {
	f.frames.Add(1)
	return f.Queue.DeadLetterFrame(ctx, subject, seq)
}

// join builds one node of f, keeping its dead letters in f's store unless
// keeps is false.
func (f fleet) join(t *testing.T, keeps bool) *node {
	t.Helper()
	var letters changefeed.DeadLetters
	if keeps {
		letters = framesRead{Queue: f.letters, frames: f.frames}
	}
	return f.joinWith(t, letters, f.claims)
}

// joinWith builds one node of f keeping its dead letters through letters and
// taking its claims in cl, either of them nil for none.
func (f fleet) joinWith(t *testing.T, letters changefeed.DeadLetters, cl changefeed.Claims) *node {
	t.Helper()
	rec, err := metrics.New()
	if err != nil {
		t.Fatalf("metrics: %v", err)
	}
	n := &node{tr: newProbe(), docs: newEstate(), pub: &capture{}, rec: rec, clock: newClock(),
		letters: f.letters}
	if n.feed, err = changefeed.New(changefeed.Options{
		Opener: n.docs, Publisher: n.pub, Claims: cl, Translator: n.tr,
		Metrics: rec, Now: n.clock.now, DeadLetters: letters,
	}); err != nil {
		t.Fatalf("new feed: %v", err)
	}
	return n
}

// subject is where every node of these cases keeps its dead letters: the
// probe's source.
var subject = changefeed.DeadLetterSubject(newProbe().Source())

// kept is what the store holds on the probe's subject.
func (n *node) kept(t *testing.T) []queue.Kept {
	t.Helper()
	got, err := n.letters.DeadLettersOn(context.Background(), subject)
	if err != nil {
		t.Fatalf("list the dead letters: %v", err)
	}
	return got
}

// handle hands the feed one delivery of rec carrying frame, settled into s.
func (n *node) handle(t *testing.T, rec changefeed.Record, frame []byte, deferred *changefeed.Deferral,
	s *settled) {

	t.Helper()
	changefeed.Handle(t.Context(), n.feed, &changefeed.Message{
		Record: rec, Frame: frame, Deferred: deferred,
		Ack: func() error {
			kept := len(n.kept(t)) > 0
			s.mu.Lock()
			defer s.mu.Unlock()
			s.acks++
			s.keptAtAck = append(s.keptAtAck, kept)
			return nil
		},
		Nak: func(d time.Duration) error {
			s.mu.Lock()
			defer s.mu.Unlock()
			s.naks = append(s.naks, d)
			return nil
		},
	})
}

// counted is a counter's total for one series.
func (n *node) counted(name string, attrs map[string]string) uint64 {
	for _, s := range n.rec.Read() {
		if s.Name != name || len(s.Attrs) != len(attrs) {
			continue
		}
		match := true
		for k, v := range attrs {
			if s.Attrs[k] != v {
				match = false
			}
		}
		if match {
			return s.Total
		}
	}
	return 0
}

// aRecord is one tracker record, stored at, delivered for the delivered-th time.
func aRecord(stored time.Time, delivered uint64) changefeed.Record {
	return changefeed.Record{ID: "op-1", Key: "item/u1", Position: 7, Stream: "CREWLET_TRACKER_LOG",
		Gen: 2, StoredAt: stored, Delivered: delivered}
}

// aFrame is a sealed frame as the broker hands one over: bytes a dead letter
// must keep exactly, a newline and a NUL among them.
var aFrame = []byte("CRWL\x01k2\x00sealed\nbody\xff")

// ---- deferral --------------------------------------------------------- //

// A RECORD A PEER CAN READ AND THIS NODE CANNOT IS HANDED BACK WITH A BACKOFF,
// NEVER ACKED (#838).
//
// The feed's consumer is the fleet's, so an ack here is the fleet's: a record
// this node acked is one no node on the newer build, and no node holding the
// key, would ever see. Each way a node meets such a record — the opener's
// unknown key and newer envelope, the translator's newer record — is handed
// back once, at the backoff for its delivery count, and counted by its reason.
//
// Mutation: ack a deferral below the age (holdBack), and every row is red.
func TestARecordAPeerCanReadIsHandedBackWithABackoffNeverAcked(t *testing.T) {
	t.Parallel()
	newer := changefeed.Defer(changefeed.DeferNewerRecord, errors.New("worklet: record version 3, this build reads 2"))
	for name, tc := range map[string]struct {
		reason     changefeed.DeferReason
		opener     *changefeed.Deferral
		translator error
	}{
		"a frame sealed under a key this node does not hold": {reason: changefeed.DeferUnknownKey,
			opener: &changefeed.Deferral{Reason: changefeed.DeferUnknownKey, Err: errors.New("key k2")}},
		"an envelope a newer build writes": {reason: changefeed.DeferNewerEnvelope,
			opener: &changefeed.Deferral{Reason: changefeed.DeferNewerEnvelope, Err: errors.New("envelope v2")}},
		"a record version a newer build writes": {reason: changefeed.DeferNewerRecord, translator: newer},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if !tc.reason.Valid() {
				t.Fatalf("%q is no reason this build names, so its series is an open set", tc.reason)
			}
			n := newFleet(t).join(t, true)
			n.tr.set(true, tc.translator)
			var s settled
			// TEN MINUTES OLD, a roll's ordinary age.
			n.handle(t, aRecord(n.clock.now().Add(-10*time.Minute), 3), aFrame, tc.opener, &s)

			acks, naks, _ := s.read()
			if acks != 0 {
				t.Fatalf("a record this node decided nothing about was acked %d time(s): "+
					"no peer would ever see it", acks)
			}
			if len(naks) != 1 || naks[0] != changefeed.DeferDelay(3) {
				t.Fatalf("it was handed back %v, want once at the third delivery's backoff %v",
					naks, changefeed.DeferDelay(3))
			}
			if n.pub.count() != 0 || len(n.kept(t)) != 0 {
				t.Errorf("a deferral published %d wake(s) and kept %d dead letter(s)", n.pub.count(), len(n.kept(t)))
			}
			if got := n.counted(metrics.TrackerFeedDeferred,
				map[string]string{"source": "work", "reason": string(tc.reason)}); got != 1 {
				t.Errorf("the deferral was counted %d time(s) under %s, want once", got, tc.reason)
			}
		})
	}
}

// THE BACKOFF DOUBLES FROM THE FEED'S OWN DELAY TO THE CONSUMER'S ACK WINDOW,
// and the feed hands a record back at the step its delivery count names.
//
// Never an immediate redelivery: a moment with only older peers up would spin
// the consumer, every node handing the record to every other as fast as the
// broker delivers. Never slower than the ack window either: a record handed
// back would come round after one nobody answered at all.
//
// Mutation: drop the doubling, or the cap, and the table is red.
func TestTheDeferralBacksOffFromTwoSecondsToTheAckWindow(t *testing.T) {
	t.Parallel()
	for delivered, want := range map[uint64]time.Duration{
		0: 2 * time.Second, 1: 2 * time.Second, 2: 4 * time.Second, 3: 8 * time.Second,
		4: 16 * time.Second, 5: 30 * time.Second, 6: 30 * time.Second, 50: 30 * time.Second,
		math.MaxUint64: 30 * time.Second,
	} {
		if got := changefeed.DeferDelay(delivered); got != want {
			t.Errorf("delivery %d waits %v, want %v", delivered, got, want)
		}
	}
	if changefeed.DeferCap != 30*time.Second {
		t.Errorf("the cap is %v, want the domain consumer's 30 s ack window", changefeed.DeferCap)
	}
}

// ---- the dead letter -------------------------------------------------- //

// A RECORD HANDED BACK FOR AN HOUR IS KEPT WHOLE AS A DEAD LETTER, AND ONLY
// THEN ACKED.
//
// The broker keeps a delayed nak in the consumer's pending set, and a thousand
// pending records stall every new wake of the domain, so the feed stops
// handing back a record no node read in its hour. It keeps it first — its
// sealed frame, byte for byte, and where it came from — under the feed's own
// subject, and acks it only once the keep returned: an ack before it is a wake
// lost between the two. A record a second short of the hour is still handed
// back.
//
// Mutation: ack before the keep, and the ack finds nothing kept; dead-letter
// below the age, and the younger record is kept.
func TestARecordHandedBackForAnHourIsKeptWholeAndOnlyThenAcked(t *testing.T) {
	t.Parallel()
	n := newFleet(t).join(t, true)
	now := n.clock.now()

	var young settled
	n.handle(t, aRecord(now.Add(-changefeed.DeadLetterAge+time.Second), 120), aFrame,
		&changefeed.Deferral{Reason: changefeed.DeferUnknownKey}, &young)
	if acks, naks, _ := young.read(); acks != 0 || len(naks) != 1 || len(n.kept(t)) != 0 {
		t.Fatalf("a record a second short of the hour was acked %d time(s), handed back %v and "+
			"%d kept: it is still a roll's, and a peer may yet read it", acks, naks, len(n.kept(t)))
	}

	var old settled
	rec := aRecord(now.Add(-changefeed.DeadLetterAge), 121)
	n.handle(t, rec, aFrame, &changefeed.Deferral{Reason: changefeed.DeferUnknownKey,
		Err: errors.New("the frame is sealed under a key this node does not hold")}, &old)
	acks, naks, keptAtAck := old.read()
	if acks != 1 || len(naks) != 0 {
		t.Fatalf("a record an hour old was acked %d time(s) and handed back %v, want it let go once", acks, naks)
	}
	if !keptAtAck[0] {
		t.Fatal("the record was acked before its dead letter was kept: a wake lost between the two")
	}
	entries := n.kept(t)
	if len(entries) != 1 {
		t.Fatalf("%d dead letters kept, want 1", len(entries))
	}
	letter, err := changefeed.DecodeDeadLetter(entries[0].Header)
	if err != nil {
		t.Fatalf("the entry's header does not read back: %v", err)
	}
	want := changefeed.DeadLetter{V: changefeed.DeadLetterVersion, Source: "work", Group: testGroup,
		Stream: rec.Stream, Seq: rec.Position, Gen: rec.Gen, Key: rec.Key, OpID: rec.ID,
		Reason: changefeed.DeferUnknownKey, StoredAt: rec.StoredAt, Delivered: 121, DeadLetteredAt: now}
	got := letter
	got.Detail = ""
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("the entry is %+v, want %+v", got, want)
	}
	frame, held, err := n.letters.DeadLetterFrame(t.Context(), subject, entries[0].Seq)
	if err != nil || !held || !bytes.Equal(frame, aFrame) {
		t.Errorf("the entry keeps the frame %q (held %v, %v), want %q byte for byte: a replay "+
			"re-reads it", frame, held, err, aFrame)
	}
	if !strings.Contains(letter.Detail, "does not hold") {
		t.Errorf("the entry's detail %q does not say what the node met", letter.Detail)
	}
	if got := n.counted(metrics.TrackerFeedDeadLettered,
		map[string]string{"source": "work", "reason": string(changefeed.DeferUnknownKey)}); got != 1 {
		t.Errorf("the dead-lettering was counted %d time(s), want once", got)
	}
	// AND ONLY THE YOUNG RECORD WAS HANDED BACK: the one let go is a dead
	// letter, not a deferral, or a roll's count would carry what outlasted it.
	if got := n.counted(metrics.TrackerFeedDeferred,
		map[string]string{"source": "work", "reason": string(changefeed.DeferUnknownKey)}); got != 1 {
		t.Errorf("%d deferral(s) counted, want the young record's alone", got)
	}
}

// A RECORD WHOSE DEAD LETTER WAS ALREADY KEPT IS ACKNOWLEDGED, AND NEITHER
// KEPT NOR COUNTED AGAIN.
//
// A keep waits as long as its frame takes to cross the node's link, so it can
// outlast the delivery's acknowledgement window, and the record is then handed
// to a peer past its hour too. Both keeps name the record's identity, so the
// queue keeps one entry and answers the keep that lands second already kept:
// the record is settled there as well, and the one record let go is counted
// once, by whichever node takes its dead-letter claim — here the node whose
// keep stored the entry, which took the claim first.
//
// Mutation: count every keep answered stored or already kept without taking
// the claim, and the peer counts the record dead-lettered a second time; name
// a keep by its delivery rather than its record, and the store holds two
// entries.
func TestARecordWhoseDeadLetterWasAlreadyKeptIsAcknowledgedAndNotCountedAgain(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	first, peer := f.join(t, true), f.join(t, true)
	stored := first.clock.now().Add(-changefeed.DeadLetterAge - time.Minute)
	reason := map[string]string{"source": "work", "reason": string(changefeed.DeferUnknownKey)}
	var kept, again settled
	first.handle(t, aRecord(stored, 120), aFrame, &changefeed.Deferral{Reason: changefeed.DeferUnknownKey}, &kept)
	peer.handle(t, aRecord(stored, 121), aFrame, &changefeed.Deferral{Reason: changefeed.DeferUnknownKey}, &again)
	for name, s := range map[string]*settled{"the first node": &kept, "the peer": &again} {
		if acks, naks, _ := s.read(); acks != 1 || len(naks) != 0 {
			t.Errorf("%s acked the record %d time(s) and handed it back %v, want it settled once",
				name, acks, naks)
		}
	}
	if entries := first.kept(t); len(entries) != 1 {
		t.Errorf("the store holds %d entries of the one record, want one", len(entries))
	}
	if got := first.counted(metrics.TrackerFeedDeadLettered, reason); got != 1 {
		t.Errorf("the node that kept the record counted it dead-lettered %d time(s), want once", got)
	}
	if got := peer.counted(metrics.TrackerFeedDeadLettered, reason); got != 0 {
		t.Errorf("the peer whose keep was answered already kept counted the record dead-lettered "+
			"%d time(s), want none: the record was let go once", got)
	}
}

// answerLost is a dead-letter store whose first keep stores its entry and
// answers an error, as a keep whose answer is lost on its way back does — to
// an election, a dropped connection, or a link slower than its budget.
type answerLost struct {
	changefeed.DeadLetters
	mu   sync.Mutex
	lost bool
}

func (a *answerLost) KeepDeadLetter(ctx context.Context, subject, id string, header, frame []byte) (bool, error) {
	already, err := a.DeadLetters.KeepDeadLetter(ctx, subject, id, header, frame)
	a.mu.Lock()
	defer a.mu.Unlock()
	if err != nil || a.lost {
		return already, err
	}
	a.lost = true
	return false, fmt.Errorf("keep a dead letter on %s: the answer was lost: %w", subject, queue.ErrUnavailable)
}

// syncBuffer is a log sink the logger may write to from any goroutine.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// detailsOf is the detail of each log line named msg whose key is key, in the
// order they were logged.
func (b *syncBuffer) detailsOf(t *testing.T, msg, key string) []string {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	var details []string
	for line := range bytes.Lines(b.buf.Bytes()) {
		var l struct {
			Msg    string `json:"msg"`
			Key    string `json:"key"`
			Detail string `json:"detail"`
		}
		if err := json.Unmarshal(line, &l); err != nil {
			t.Fatalf("a log line is not JSON: %v (%q)", err, line)
		}
		if l.Msg == msg && l.Key == key {
			details = append(details, l.Detail)
		}
	}
	return details
}

// captureLogs points the process's log at a buffer for one test, which must not
// be parallel — the log is the process's — and puts logging's own default back
// after it.
func captureLogs(t *testing.T) *syncBuffer {
	t.Helper()
	buf := &syncBuffer{}
	logging.Configure(slog.LevelInfo, logging.FormatJSON, buf)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatConsole, os.Stderr) })
	return buf
}

// A DEAD LETTER WHOSE STORING KEEP ANSWERED AN ERROR IS COUNTED ONCE, BY THE
// KEEP ANSWERED NEXT.
//
// A keep can store its entry and still answer an error, its answer lost on the
// way back, and its node then hands the record back, claiming and counting
// nothing. The record's next keep is answered already kept, and it is the keep
// that takes the record's dead-letter claim: it counts the record and logs its
// line. Counted by the answer instead, the record would sit in CREWLET_DLQ
// with no count and no line at all. The line says an earlier keep stored the
// entry and no more, since an operator's discard or a feed start's replay may
// have dropped it by then.
//
// NOT PARALLEL: it reads the process's log, which [logging.Configure] owns.
//
// Mutation: count only a keep answered stored, and the record is counted
// nowhere and its line never logged; give the line a stored keep's detail, and
// it promises an entry that may be gone.
func TestADeadLetterWhoseStoringKeepAnsweredAnErrorIsCountedOnce(t *testing.T) {
	logs := captureLogs(t)
	f := newFleet(t)
	n := f.joinWith(t, &answerLost{DeadLetters: framesRead{Queue: f.letters, frames: f.frames}}, f.claims)
	rec := aRecord(n.clock.now().Add(-changefeed.DeadLetterAge-time.Minute), 120)
	rec.Key = "item/answer-lost"
	reason := map[string]string{"source": "work", "reason": string(changefeed.DeferUnknownKey)}

	var lost settled
	n.handle(t, rec, aFrame, &changefeed.Deferral{Reason: changefeed.DeferUnknownKey}, &lost)
	if acks, naks, _ := lost.read(); acks != 0 || len(naks) != 1 {
		t.Fatalf("a keep answered with an error acked the record %d time(s) and handed it back %v, "+
			"want it handed back once", acks, naks)
	}
	if entries := n.kept(t); len(entries) != 1 {
		t.Fatalf("the store holds %d entries, want the one the lost answer's keep stored", len(entries))
	}
	if got := n.counted(metrics.TrackerFeedDeadLettered, reason); got != 0 {
		t.Fatalf("a keep answered with an error counted the record %d time(s), want none", got)
	}

	var again settled
	rec.Delivered = 121
	n.handle(t, rec, aFrame, &changefeed.Deferral{Reason: changefeed.DeferUnknownKey}, &again)
	if acks, naks, _ := again.read(); acks != 1 || len(naks) != 0 {
		t.Errorf("the keep answered already kept acked the record %d time(s) and handed it back %v, "+
			"want it settled once", acks, naks)
	}
	if entries := n.kept(t); len(entries) != 1 {
		t.Errorf("the store holds %d entries of the one record, want one", len(entries))
	}
	if got := n.counted(metrics.TrackerFeedDeadLettered, reason); got != 1 {
		t.Errorf("the record whose storing keep's answer was lost was counted dead-lettered %d "+
			"time(s), want once", got)
	}
	details := logs.detailsOf(t, "changefeed_record_dead_lettered", rec.Key)
	if len(details) != 1 {
		t.Fatalf("the record's dead-lettering was logged %d time(s), want once", len(details))
	}
	if !strings.Contains(details[0], "an earlier keep stored its entry") ||
		strings.Contains(details[0], "so it is kept in") {
		t.Errorf("the line of a record counted by a keep answered already kept says %q, "+
			"want it to say an earlier keep stored the entry, which may be gone by now", details[0])
	}
}

// A DEAD LETTER WHOSE CLAIM CANNOT BE DECIDED IS COUNTED ANYWAY.
//
// The count is the one reading an operator has of how many records were let
// go, and a record nobody counted is never noticed, where one two nodes count
// reads one too many. So a claims store that answers an error, and a feed
// given no claims at all, count the record they kept.
//
// Mutation: fail closed on a claim the store could not decide, and the record
// is counted by nobody.
func TestADeadLetterWhoseClaimCannotBeDecidedIsCountedAnyway(t *testing.T) {
	t.Parallel()
	for name, broken := range map[string]bool{
		"a claims store that answers an error": true,
		"a feed given no claims":               false,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFleet(t)
			var cl changefeed.Claims
			if broken {
				f.claims.breakWith(errors.New("the coordination store is unreachable"))
				cl = f.claims
			}
			n := f.joinWith(t, framesRead{Queue: f.letters, frames: f.frames}, cl)
			var s settled
			n.handle(t, aRecord(n.clock.now().Add(-changefeed.DeadLetterAge-time.Minute), 120), aFrame,
				&changefeed.Deferral{Reason: changefeed.DeferNewerRecord}, &s)
			if acks, naks, _ := s.read(); acks != 1 || len(naks) != 0 {
				t.Errorf("the record was acked %d time(s) and handed back %v, want it settled once", acks, naks)
			}
			if got := n.counted(metrics.TrackerFeedDeadLettered,
				map[string]string{"source": "work", "reason": string(changefeed.DeferNewerRecord)}); got != 1 {
				t.Errorf("a record whose dead-letter claim could not be decided was counted %d time(s), "+
					"want once", got)
			}
		})
	}
}

// THE DEAD-LETTER CLAIM FITS THE CLAIMS STORE. It is held for the queue's
// window, and the coordination store refuses a claim longer than it keeps one
// (coord.MaxClaimTTL) rather than clamp it: every keep would then be counted,
// each beside a warning that the claim could not be decided.
//
// Mutation: a window longer than coord.MaxClaimTTL, and this is red.
func TestTheDeadLetterClaimFitsTheClaimsStore(t *testing.T) {
	t.Parallel()
	if queue.DeadLetterWindow > coord.MaxClaimTTL {
		t.Errorf("queue.DeadLetterWindow %v is longer than the coordination store keeps a claim "+
			"(coord.MaxClaimTTL %v), so every record's dead-letter claim is refused",
			queue.DeadLetterWindow, coord.MaxClaimTTL)
	}
}

// A DEAD LETTER THAT COULD NOT BE KEPT LEAVES ITS RECORD HANDED BACK, NEVER
// ACKED BARE.
//
// An entry that was not kept is a wake no node could ever replay, so the ack —
// which is the fleet's — waits for a keep that lands: a store that refuses, a
// feed given no store at all, and a delivery with no frame to keep each leave
// the record circling with the backoff.
//
// Mutation: ack when the keep fails, and every row is red.
func TestADeadLetterThatCannotBeKeptLeavesItsRecordHandedBack(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		keeps   bool
		frame   []byte
		refuses bool
	}{
		"a store that refuses the keep":    {keeps: true, frame: aFrame, refuses: true},
		"a feed given no store":            {keeps: false, frame: aFrame},
		"a delivery with no frame to keep": {keeps: true, frame: nil},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			f := newFleet(t)
			n := f.join(t, tc.keeps)
			if tc.refuses {
				f.letters.Broker().Fail()
			}
			var s settled
			n.handle(t, aRecord(n.clock.now().Add(-2*changefeed.DeadLetterAge), 200), tc.frame,
				&changefeed.Deferral{Reason: changefeed.DeferNewerEnvelope}, &s)
			acks, naks, _ := s.read()
			if acks != 0 {
				t.Fatalf("a record whose dead letter was not kept was acked %d time(s)", acks)
			}
			if len(naks) != 1 || naks[0] != changefeed.DeferDelay(200) {
				t.Errorf("it was handed back %v, want once at the backoff %v", naks, changefeed.DeferDelay(200))
			}
			if got := n.counted(metrics.TrackerFeedDeadLettered,
				map[string]string{"source": "work", "reason": string(changefeed.DeferNewerEnvelope)}); got != 0 {
				t.Errorf("a dead letter that was not kept was counted %d time(s)", got)
			}
		})
	}
}

// A RECORD NO BUILD HERE CAN TRANSLATE BACKS OFF AND IS DEAD-LETTERED PAST THE
// AGE, under its own reason and still counted as unreadable.
//
// It used to come back every two seconds for ever, holding its slot in the
// consumer with nothing to end it. It is folded into the deferral's bound: the
// same backoff, the same hour, kept as `untranslatable` — and `feed_unreadable`
// still counts every attempt, since the node that met it has a build to fix.
//
// Mutation: hand it back at the fixed two-second delay, and the first half is
// red; never dead-letter it, and the second.
func TestARecordNoBuildHereCanTranslateBacksOffAndIsDeadLetteredPastTheAge(t *testing.T) {
	t.Parallel()
	n := newFleet(t).join(t, true)
	n.tr.set(true, errors.New("worklet: read the record on item/u1: unexpected end of JSON input"))
	unreadable := map[string]string{"source": "work"}

	var young settled
	n.handle(t, aRecord(n.clock.now().Add(-5*time.Minute), 4), aFrame, nil, &young)
	if acks, naks, _ := young.read(); acks != 0 || len(naks) != 1 || naks[0] != changefeed.DeferDelay(4) {
		t.Fatalf("an untranslatable record was acked %d time(s) and handed back %v, want once at "+
			"the fourth delivery's backoff %v", acks, naks, changefeed.DeferDelay(4))
	}
	if got := n.counted(metrics.TrackerFeedUnreadable, unreadable); got != 1 {
		t.Errorf("it was counted unreadable %d time(s), want once", got)
	}
	if got := n.counted(metrics.TrackerFeedDeferred,
		map[string]string{"source": "work", "reason": string(changefeed.DeferUntranslatable)}); got != 0 {
		t.Errorf("a record no build here reads was counted %d time(s) as a deferral to a peer, "+
			"which is a roll's traffic", got)
	}

	var old settled
	n.handle(t, aRecord(n.clock.now().Add(-changefeed.DeadLetterAge-time.Minute), 130), aFrame, nil, &old)
	acks, naks, keptAtAck := old.read()
	if acks != 1 || len(naks) != 0 || !keptAtAck[0] {
		t.Fatalf("an untranslatable record past the hour was acked %d time(s) (kept first: %v) and "+
			"handed back %v, want it kept and then let go", acks, keptAtAck, naks)
	}
	letter, err := changefeed.DecodeDeadLetter(n.kept(t)[0].Header)
	if err != nil || letter.Reason != changefeed.DeferUntranslatable {
		t.Fatalf("the entry reads %+v (%v), want it kept as untranslatable", letter, err)
	}
	if got := n.counted(metrics.TrackerFeedUnreadable, unreadable); got != 2 {
		t.Errorf("it was counted unreadable %d time(s) over two attempts", got)
	}
}

// A RECORD ITS ESTATE DATES NOTHING FOR IS NEVER DEAD-LETTERED: nothing says
// how long it has been handed back, so it goes on being handed back.
//
// Mutation: read a zero instant as the epoch, and the record is let go at once.
func TestARecordItsEstateDatesNothingForIsNeverDeadLettered(t *testing.T) {
	t.Parallel()
	n := newFleet(t).join(t, true)
	var s settled
	n.handle(t, aRecord(time.Time{}, 900), aFrame, &changefeed.Deferral{Reason: changefeed.DeferUnknownKey}, &s)
	if acks, naks, _ := s.read(); acks != 0 || len(naks) != 1 || len(n.kept(t)) != 0 {
		t.Errorf("an undated record was acked %d time(s), handed back %v and kept %d time(s)",
			acks, naks, len(n.kept(t)))
	}
}

// A DEAD LETTER'S HEADER IS ONE BOUNDED LINE OF ASCII, so a record at its
// log's ceiling is still kept, frame and all.
//
// The tracker's and the code log's records may be as large as the transport
// carries, and the entry's body is the record's frame whole, so what the
// header may cost is what the transport leaves beside a frame at the ceiling
// once the queue has framed the keep. Its fields are bounded however long the
// error a node met, every byte past the ASCII escaped — the header crosses the
// wire as a header of the message, one line of ASCII — and it reads back as
// what was kept. A header a newer build kept, one with no version, none at all
// and bytes that are no header are refused.
//
// Mutation: stop cutting the detail, and the header outgrows the room beside
// the frame; leave a rune past the ASCII unescaped, and the queue refuses the
// header; keep the frame inside the header, and nothing fits.
func TestADeadLettersHeaderIsOneBoundedLineOfASCII(t *testing.T) {
	t.Parallel()
	// THE LARGEST FRAME A LOG ADMITS: its largest record and the seal, which
	// statelog's appendOverhead bounds under three hundred bytes.
	frame := bytes.Repeat([]byte{0xff}, statelog.MaxTransportRecordBytes+300)
	letter := changefeed.DeadLetter{
		Source: "work", Group: worklet.FeedGroup, Stream: "CREWLET_TRACKER_LOG", Seq: math.MaxUint64,
		Gen: math.MaxUint64, Key: "crewlet.tracker.log.task." + strings.Repeat("k", 512),
		OpID: "op-01JABCDEF", Reason: changefeed.DeferUntranslatable,
		Detail:   strings.Repeat("\x01\x7f\"<>é\xff", 4096),
		StoredAt: time.Unix(1_700_000_000, 0).UTC(), Delivered: math.MaxUint64,
		DeadLetteredAt: time.Unix(1_700_003_600, 0).UTC(),
	}
	header, err := changefeed.EncodeDeadLetter(letter)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if err := queue.LetterHeader(header); err != nil {
		t.Fatalf("the header is no line the queue keeps: %v", err)
	}
	if room := queue.MaxPayloadBytes - queue.LetterFraming - len(frame); len(header) > room {
		t.Fatalf("the header of a record at its log's ceiling is %d bytes, past the %d the "+
			"transport leaves beside its frame: it could never be kept", len(header), room)
	}
	// AND THE QUEUE KEEPS IT, through the twin that refuses exactly what
	// the broker refuses.
	letters := newFleet(t).letters
	if _, err := letters.KeepDeadLetter(t.Context(), subject, "the largest", header, frame); err != nil {
		t.Fatalf("a record at its log's ceiling was not kept: %v", err)
	}
	back, err := changefeed.DecodeDeadLetter(header)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if back.Seq != letter.Seq || back.Key != letter.Key || back.Delivered != letter.Delivered ||
		!back.StoredAt.Equal(letter.StoredAt) || back.Detail == "" {
		t.Errorf("the header did not read back as it was kept: %+v", back)
	}

	for name, bad := range map[string][]byte{
		"a newer build's header":     []byte(`{"v":2,"source":"work"}`),
		"a header with no version":   []byte(`{"source":"work"}`),
		"no header at all":           nil,
		"bytes that are no header":   []byte("frame"),
		"an old header line, framed": []byte(`{"v":1,"source":"work"}` + "\nframe"),
	} {
		if _, err := changefeed.DecodeDeadLetter(bad); err == nil {
			t.Errorf("%s was read as a dead letter", name)
		}
	}
}

// A RUNE ABOVE U+FFFF IS SPELLED AS ITS SURROGATE PAIR, and read back as itself.
//
// The header is ASCII, so every rune past it is a JSON escape — and a `\u`
// escape names four hex digits, so a rune above U+FFFF has only one spelling:
// its UTF-16 surrogate pair, two escapes. Spelled as its own code point it
// would be read back as another rune and a stray digit.
//
// Mutation: escape the rune as `\u%04x` of itself, and the detail reads back
// as U+1F60 and a 0.
func TestARuneAboveTheBMPIsSpelledAsItsSurrogatePair(t *testing.T) {
	t.Parallel()
	detail := "met \U0001F600 and é"
	header, err := changefeed.EncodeDeadLetter(changefeed.DeadLetter{Source: "work", Group: testGroup,
		Stream: "CREWLET_TRACKER_LOG", Seq: 7, Key: "item/u1", Reason: changefeed.DeferUnknownKey,
		Detail: detail})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !bytes.Contains(header, []byte(`\ud83d\ude00`)) {
		t.Errorf("the header %s does not spell U+1F600 as its surrogate pair", header)
	}
	back, err := changefeed.DecodeDeadLetter(header)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if back.Detail != detail {
		t.Errorf("the detail reads back as %q, want %q", back.Detail, detail)
	}
}

// EVERY FEED KEEPS ITS DEAD LETTERS ON A SUBJECT OF ITS OWN, in the dead-letter
// namespace and under the head an operator greps.
//
// One subject per domain is what a feed's start replays and what the alarm
// reads per log; two feeds on one subject would each replay the other's
// records through a translator that cannot read them.
//
// Mutation: derive the subject from the source's name alone, or from a shared
// topic, and two feeds collide or the head names no estate.
func TestEveryFeedKeepsItsDeadLettersOnASubjectOfItsOwn(t *testing.T) {
	t.Parallel()
	seen := map[string]string{}
	for _, src := range []changefeed.Source{
		worklet.NewTranslator().Source(), doclet.NewTranslator(nil).Source(),
		chatlet.NewTranslator(nil).Source(), gitlet.NewTranslator().Source(),
	} {
		subject := changefeed.DeadLetterSubject(src)
		if !strings.HasPrefix(subject, topics.DeadLetterPrefix+"changefeed."+src.Name+".") {
			t.Errorf("the %s feed keeps its dead letters on %q, outside dlq.changefeed.%s", src.Name,
				subject, src.Name)
		}
		if other, clash := seen[subject]; clash {
			t.Errorf("the %s and %s feeds share %q", src.Name, other, subject)
		}
		seen[subject] = src.Name
	}
}

// ---- the replay ------------------------------------------------------- //

// deadLetterOf keeps rec's dead letter, as a node that gave up on it would.
func deadLetterOf(t *testing.T, f fleet, rec changefeed.Record) {
	t.Helper()
	n := f.join(t, true)
	var s settled
	n.handle(t, rec, rec.Payload, &changefeed.Deferral{Reason: changefeed.DeferUnknownKey}, &s)
	if acks, _, kept := s.read(); acks != 1 || !kept[0] {
		t.Fatalf("the record was not dead-lettered (acked %d, kept first %v)", acks, kept)
	}
}

// keptRecord is a record a dead letter keeps: its frame is its payload, which
// the fixture's estate reads back as the record.
func keptRecord(clk *clock) changefeed.Record {
	rec := aRecord(clk.now().Add(-2*changefeed.DeadLetterAge), 240)
	rec.Payload = []byte(`{"frame":"u1"}`)
	return rec
}

// A REPLAY DERIVES A KEPT RECORD'S WAKE ONCE, AND DROPS ITS ENTRY.
//
// Two entries can hold the same record, under its one identity (keepID), only
// when its keeps are further apart than the window the broker holds the
// identity for: a restart rebuilds the window from the entries still held, so
// it forgets only a dropped entry's identity and never leaves two. So the case
// keeps the record twice that far apart on the twin's clock, which a bubble
// moves. The feed's start publishes the first entry's
// wake through the claim a delivery passes and drops the entry; the second is
// the same record, whose wake this replay has just settled, so it is dropped
// with nothing published again — never left as though the claim this replay
// took were a peer's — and its frame never read. The wake carries the record's
// own place, as a delivered one does.
//
// Mutation: settle each entry alone, and the second is left as though a peer
// held the claim this replay took; leave an entry whose wake was published,
// and the store keeps it; read a frame before the header says the record is
// settled, and two are read; leave the replayed wake unmarked, and it is
// skipped past the seat's rate limit like a live one.
func TestAReplayDerivesAKeptRecordsWakeOnceAndDropsItsEntry(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		f := newFleet(t)
		rec := keptRecord(newClock())
		deadLetterOf(t, f, rec)
		// PAST THE QUEUE'S WINDOW on the twin's clock, the bubble's, so the
		// record's second keep stores an entry of its own.
		time.Sleep(queue.DeadLetterWindow + time.Second)
		deadLetterOf(t, f, rec)

		newer := f.join(t, true)
		if len(newer.kept(t)) != 2 {
			t.Fatalf("the store holds %d entries, want the record kept twice", len(newer.kept(t)))
		}
		if !changefeed.Replay(t.Context(), newer.feed, newer.docs) {
			t.Fatal("the replay's listing did not answer")
		}
		if got := newer.pub.count(); got != 1 {
			t.Fatalf("the replay published %d wake(s) for one record kept twice, want one", got)
		}
		// ONE FRAME READ, the entry it derived the wake from: the second
		// entry of the same record is settled by its header alone.
		if got := f.frames.Load(); got != 1 {
			t.Errorf("the replay read %d frames, want the one it derived the wake from", got)
		}
		w, _ := events.DataAs[*types.RawWebhook](newer.pub.first(t))
		at, err := statelog.ParsePosition(w.Trigger)
		if want := (statelog.Position{Stream: rec.Stream, Generation: uint32(rec.Gen), Seq: rec.Position}); err != nil ||
			at != want || w.Body["key"] != rec.Key {
			t.Errorf("the replayed wake carries %q at %q (%v), want %s at %v", w.Body["key"], w.Trigger, err, rec.Key, want)
		}
		// MARKED AS A REPLAY, so the notification service paces it by the
		// seat's rate valve rather than skip it: the entry it came from
		// is dropped once the wake is published, so a skipped wake was
		// lost with it.
		if !w.Replayed {
			t.Error("the replayed wake is not marked replayed")
		}
		if left := newer.kept(t); len(left) != 0 {
			t.Errorf("%d entries are still kept after their wake was published", len(left))
		}
	})
}

// A REBUILT LOG'S RECORD AT A REUSED SEQUENCE IS REPLAYED ON ITS OWN.
//
// A deleted and rebuilt log numbers its records from one again while the dead
// letters keep their entries for the event stream's retention, so an old
// incarnation's entry and a new one's can name the same stream and sequence.
// The broker stored the two at different instants, which both entries carry:
// they are two records, kept under two identities, and the replay derives each
// one's wake from its own frame. Neither is dropped unread as a copy of the
// other.
//
// Mutation: name a record's place without its stored instant, and the second
// entry is dropped unread as the first one's copy, its wake lost.
func TestARebuiltLogsRecordAtAReusedSequenceIsReplayedOnItsOwn(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	old := keptRecord(newClock())
	deadLetterOf(t, f, old)
	rebuilt := old
	rebuilt.StoredAt = old.StoredAt.Add(10 * time.Minute)
	rebuilt.ID, rebuilt.Key, rebuilt.Payload = "op-2", "item/u2", []byte(`{"frame":"u2"}`)
	deadLetterOf(t, f, rebuilt)

	newer := f.join(t, true)
	if len(newer.kept(t)) != 2 {
		t.Fatalf("the store holds %d entries, want one for each incarnation's record", len(newer.kept(t)))
	}
	if !changefeed.Replay(t.Context(), newer.feed, newer.docs) {
		t.Fatal("the replay's listing did not answer")
	}
	if got := newer.pub.count(); got != 2 {
		t.Fatalf("the replay published %d wake(s) for two records at one sequence, want one each", got)
	}
	if got := f.frames.Load(); got != 2 {
		t.Errorf("the replay read %d frames, want each record's own", got)
	}
	if left := newer.kept(t); len(left) != 0 {
		t.Errorf("%d entries are still kept after both wakes were published", len(left))
	}
}

// TWO NODES KEEP ONE RECORD ONCE, WHETHER OR NOT THEY READ ITS ENVELOPE.
//
// A node that cannot open a record's frame — a key it does not hold, an
// envelope a newer build writes — defers it knowing only what the delivery
// says: its log, its position and the broker's instant, and no generation. A
// node that read the envelope also knows the log's generation, one or more on
// a log reanchored since it began. Both keep the one record, the second once
// the first's delivery window lapsed, and the queue keeps it once: the
// identity is what both read alike.
//
// Mutation: key the identity on the generation rather than the stored instant,
// and the store holds two entries of the one record.
func TestTwoNodesKeepOneRecordOnceWhetherOrNotTheyReadItsEnvelope(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	read, blind := f.join(t, true), f.join(t, true)
	rec := aRecord(read.clock.now().Add(-changefeed.DeadLetterAge-time.Minute), 120)
	if rec.Gen < 1 {
		t.Fatalf("the record is at generation %d, want a reanchored log's", rec.Gen)
	}
	var first, second settled
	read.handle(t, rec, aFrame, &changefeed.Deferral{Reason: changefeed.DeferNewerRecord}, &first)
	unread := rec
	unread.ID, unread.Gen, unread.Delivered = "", 0, 121
	blind.handle(t, unread, aFrame, &changefeed.Deferral{Reason: changefeed.DeferUnknownKey}, &second)
	for name, s := range map[string]*settled{"the node that read it": &first, "the node that could not": &second} {
		if acks, naks, _ := s.read(); acks != 1 || len(naks) != 0 {
			t.Errorf("%s acked the record %d time(s) and handed it back %v, want it settled once",
				name, acks, naks)
		}
	}
	if entries := read.kept(t); len(entries) != 1 {
		t.Errorf("the store holds %d entries of the one record, want one", len(entries))
	}
	counted := read.counted(metrics.TrackerFeedDeadLettered,
		map[string]string{"source": "work", "reason": string(changefeed.DeferNewerRecord)}) +
		blind.counted(metrics.TrackerFeedDeadLettered,
			map[string]string{"source": "work", "reason": string(changefeed.DeferUnknownKey)})
	if counted != 1 {
		t.Errorf("the one record was counted dead-lettered %d time(s) across the two nodes, want once", counted)
	}
}

// A REPLAY WHOSE WAKE A PEER HAS CLAIMED LEAVES THE ENTRY FOR THAT PEER — and a
// peer whose publish failed leaves it for the next start.
//
// Two nodes starting together replay the same entry, and the claim lets one of
// them publish. The other must not drop the entry on the claim's word: the
// claim's holder may yet fail to publish, release the claim and leave the
// entry for the next start, and an entry dropped meanwhile is a wake nobody
// has.
//
// Mutation: drop an entry whose claim a peer holds, and the wake is lost once
// the peer's publish fails; publish past the claim, and a second wake goes out
// beside the peer's.
func TestAReplayWhoseWakeAPeerHasClaimedLeavesTheEntryForThatPeer(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	deadLetterOf(t, f, keptRecord(newClock()))

	// THE PEER HAS CLAIMED THE WAKE, and is publishing it.
	key := changefeed.ClaimKey(newProbe().Source().Name, "u1")
	if won, err := f.claims.Claim(t.Context(), key, changefeed.ClaimTTL, newClock().now()); !won || err != nil {
		t.Fatalf("the peer's claim: won %v, %v", won, err)
	}
	n := f.join(t, true)
	if !changefeed.Replay(t.Context(), n.feed, n.docs) {
		t.Fatal("the replay's listing did not answer")
	}
	if n.pub.count() != 0 || len(n.kept(t)) != 1 {
		t.Fatalf("a replay beside a peer holding the claim published %d wake(s) and left %d entries, "+
			"want nothing published and the entry left for the peer", n.pub.count(), len(n.kept(t)))
	}

	// THE PEER'S PUBLISH FAILED: it released the claim and left the entry,
	// which the next start replays.
	if err := f.claims.Release(t.Context(), key); err != nil {
		t.Fatalf("release the peer's claim: %v", err)
	}
	next := f.join(t, true)
	changefeed.Replay(t.Context(), next.feed, next.docs)
	if next.pub.count() != 1 || len(next.kept(t)) != 0 {
		t.Errorf("the next start published %d wake(s) and left %d entries, want the wake once and "+
			"the entry dropped", next.pub.count(), len(next.kept(t)))
	}
}

// AN ENTRY THIS NODE STILL CANNOT READ IS LEFT FOR A NODE THAT CAN — and the
// node that can then replays it.
//
// A node whose start replays the dead letters may be an older one, or one still
// without the key: whatever it cannot read — a deferral at the opener or the
// translator, a record it cannot translate — it leaves, and publishes nothing.
// The control at the end is a node that reads it, which proves the entry was
// replayable all along.
//
// Mutation: drop every replayed entry whatever became of it, and the first
// node's replay loses the wake.
func TestAnEntryThisNodeStillCannotReadIsLeftForANodeThatCan(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	deadLetterOf(t, f, keptRecord(newClock()))
	for name, unable := range map[string]func(n *node){
		"a node without the key": func(n *node) {
			n.docs.reopen = func(frame []byte, at changefeed.Record) (*changefeed.Message, error) {
				return &changefeed.Message{Record: at, Frame: frame,
					Deferred: &changefeed.Deferral{Reason: changefeed.DeferUnknownKey}}, nil
			}
		},
		"a node on the older build": func(n *node) {
			n.tr.set(true, changefeed.Defer(changefeed.DeferNewerRecord, errors.New("version 3")))
		},
		"a node that cannot translate it": func(n *node) {
			n.tr.set(true, errors.New("unexpected end of JSON input"))
		},
	} {
		n := f.join(t, true)
		unable(n)
		if !changefeed.Replay(t.Context(), n.feed, n.docs) {
			t.Fatalf("%s: the replay's listing did not answer", name)
		}
		if n.pub.count() != 0 || len(n.kept(t)) != 1 {
			t.Fatalf("%s published %d wake(s) and left %d entries, want nothing published and "+
				"the entry kept for a node that can read it", name, n.pub.count(), len(n.kept(t)))
		}
	}

	able := f.join(t, true)
	changefeed.Replay(t.Context(), able.feed, able.docs)
	if able.pub.count() != 1 || len(able.kept(t)) != 0 {
		t.Errorf("a node that reads the entry published %d wake(s) and left %d entries, want "+
			"its wake once and the entry dropped", able.pub.count(), len(able.kept(t)))
	}
}

// A FRAME THIS FLEET DID NOT WRITE IS DROPPED FROM THE DEAD LETTERS AND WAKES
// NOBODY: no build and no keyring will ever make it one to act on.
//
// Mutation: leave it, and it is replayed at every start until it expires;
// derive a wake from it, and the engine acts on bytes it cannot attribute.
func TestAForeignFrameInTheDeadLettersIsDroppedAndWakesNobody(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	deadLetterOf(t, f, keptRecord(newClock()))
	n := f.join(t, true)
	n.docs.reopen = func([]byte, changefeed.Record) (*changefeed.Message, error) {
		return nil, fmt.Errorf("%w: the frame is tampered under this node's keyring", changefeed.ErrForeignFrame)
	}
	changefeed.Replay(t.Context(), n.feed, n.docs)
	if n.pub.count() != 0 || len(n.kept(t)) != 0 {
		t.Errorf("a foreign frame published %d wake(s) and left %d entries, want none and none",
			n.pub.count(), len(n.kept(t)))
	}
}

// AN ENTRY A NEWER BUILD KEPT IS LEFT UNREAD: this build cannot be sure it
// reads its header whole, so it judges nothing about it.
//
// Mutation: read past the version, or drop what does not decode, and the
// newer build's entry is lost.
func TestAnEntryANewerBuildKeptIsLeft(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	if _, err := f.letters.KeepDeadLetter(t.Context(), subject, "a newer build's record",
		[]byte(`{"v":2,"source":"work","seq":7}`), []byte("frame")); err != nil {
		t.Fatalf("keep: %v", err)
	}
	n := f.join(t, true)
	opened := false
	n.docs.reopen = func(frame []byte, at changefeed.Record) (*changefeed.Message, error) {
		opened = true
		return &changefeed.Message{Record: at, Frame: frame}, nil
	}
	changefeed.Replay(t.Context(), n.feed, n.docs)
	if opened || n.pub.count() != 0 || len(n.kept(t)) != 1 {
		t.Errorf("a newer build's entry was opened (%v), published %d wake(s) and left %d entries, "+
			"want it left untouched", opened, n.pub.count(), len(n.kept(t)))
	}
}

// A REPLAY WHOSE LISTING FAILS SAYS SO, so the feed lists again at its next
// open rather than taking an unread store for an empty one.
//
// Mutation: report a failed listing as a replay done, and a feed whose broker
// blinked at its start never replays what it kept.
func TestAReplayWhoseListingFailsSaysSo(t *testing.T) {
	t.Parallel()
	f := newFleet(t)
	n := f.join(t, true)
	f.letters.Broker().Fail()
	if changefeed.Replay(t.Context(), n.feed, n.docs) {
		t.Error("a replay whose listing failed reported itself done")
	}
}
