package notify_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// errFutureVersion is a domain's refusal of a record a newer build writes, as
// each native domain's decode answers one (internal/worklet's
// ErrFutureVersion and its kin).
var errFutureVersion = errors.New("record: written by a newer build")

// versionedParser reads a relayed record as the engine's native parsers do
// (internal/worklet's parse.go is the shape): the record's own version first —
// one past what this build reads refused with its domain's future-version
// error, which the parser answers as a deferral to a peer — and otherwise the
// record's wake for the seat it names, under the id derived from the change
// and the recipient.
type versionedParser struct {
	// reads is the newest record version this build decodes.
	reads int
}

func (versionedParser) Source() string { return "tracker" }

func (p versionedParser) Parse(_ context.Context, w types.RawWebhook, _ *notify.Registry) ([]notify.Routed, error) {
	stated, _ := w.Body["version"].(string)
	version, err := strconv.Atoi(stated)
	if err != nil {
		return nil, fmt.Errorf("tracker: the record states no version: %w", err)
	}
	if version > p.reads {
		return nil, notify.Defer(notify.DeferNewerRecord, fmt.Errorf(
			"tracker: the record is version %d and this build reads %d: %w",
			version, p.reads, errFutureVersion))
	}
	change, _ := w.Body["change"].(string)
	r := to(notify.Recipient{Handle: "engineering-lead"}, "the record's change")
	r.WakeID = changefeed.WakeID(change, "engineering-lead")
	return []notify.Routed{r}, nil
}

// relayed is the delivery a newer node's feed relays for one change: the
// record at version, under its change id.
func relayed(version, change string) *events.Event {
	ev := events.New(types.RawWebhook{
		Body: map[string]any{"version": version, "change": change}, Headers: map[string]string{},
	}, events.NewTrace())
	ev.Source = "tracker"
	return ev
}

// node is another node of h's fleet: a service on a client of its own on h's
// broker, reading records with parser, its queue wrapped by wrap where a case
// breaks one of its verbs.
func node(t *testing.T, h *harness, parser notify.Parser,
	wrap func(queue.EventQueue) queue.EventQueue) *notify.Service {

	t.Helper()
	client := h.q.Client()
	if err := client.Start(t.Context()); err != nil {
		t.Fatalf("start a peer's queue: %v", err)
	}
	var q queue.EventQueue = client
	if wrap != nil {
		q = wrap(client)
	}
	svc, err := notify.New(notify.Options{
		Queue:    q,
		Registry: func() *notify.Registry { return h.reg },
		Prompts:  prompts(),
		Parsers:  []notify.Parser{parser},
		Now:      func() time.Time { return t0 },
	})
	if err != nil {
		t.Fatalf("notify.New: %v", err)
	}
	return svc
}

// start starts svc and waits for the replay its start began to return, failing
// the case on an error, and stops svc when the case ends: a replay runs on a
// goroutine of its own, which the case must not leave running.
func start(t *testing.T, svc *notify.Service) {
	t.Helper()
	if err := svc.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		if err := svc.Stop(context.Background()); err != nil {
			t.Errorf("Stop: %v", err)
		}
	})
	<-svc.Replayed()
}

// inboundLetters is what one of the inbound group's dead-letter subjects
// holds.
func inboundLetters(t *testing.T, q queue.EventQueue, subject string) []queue.Kept {
	t.Helper()
	kept, err := q.DeadLettersOn(t.Context(), subject)
	if err != nil {
		t.Fatalf("DeadLettersOn: %v", err)
	}
	return kept
}

// inboundPoison is the inbound group's other dead-letter subject: what a
// failure, never a pass or a hand-back, retires there.
var inboundPoison = topics.DeadLetter(topics.NotificationsInbound, notify.InboundGroup)

// passedOff is the inbound group's one unhandled copy after an older node
// passed ev until its budget was spent: kept on the group's unhandled
// subject, its body ev's own bytes.
func passedOff(t *testing.T, h *harness, ev *events.Event) queue.Kept {
	t.Helper()
	passed := inboundLetters(t, h.q, notify.UnhandledSubject())
	if len(passed) != 1 {
		t.Fatalf("the inbound group holds %d unhandled copies, want the one the older "+
			"node's passes retired", len(passed))
	}
	frame, held, err := h.q.DeadLetterFrame(t.Context(), notify.UnhandledSubject(), passed[0].Seq)
	if err != nil || !held {
		t.Fatalf("DeadLetterFrame: held %v, %v", held, err)
	}
	var copied events.Event
	if err := json.Unmarshal(frame, &copied); err != nil || copied.ID != ev.ID {
		t.Fatalf("the passed copy reads back as %s (%v), want the delivery %s", copied.ID, err, ev.ID)
	}
	return passed[0]
}

// A WAKE NO NODE TOOK WHILE IT WAS PASSED ROUND IS DEAD-LETTERED WITH ITS COPY,
// AND THE FIRST NODE THAT READS IT REPLAYS IT AT ITS START.
//
// Nothing steers a pass to a node that can read it: every delivery of a newer
// record's wake can go to older nodes, early in a roll, until the group's
// budget is spent. So what delivers it is the dead letter the queue keeps of
// it, on the group's unhandled subject (queue.RetiredTo), and the replay a
// node's start makes of it (crewlet#993). End to end on one broker, through
// the real service and the queue's own budget:
//
//   - an older node, whose parser refuses the record as newer than it reads,
//     passes the delivery until the budget is spent, and the queue keeps its
//     copy with the group's unhandled deliveries;
//   - another older node starting reads it no better, delivers nothing and
//     keeps the copy;
//   - a newer node starting delivers the wake to the seat it names, under the
//     id derived from the change, and drops the copy — and leaves the poison
//     beside it, a delivery whose failure, not a pass, retired it.
//
// Mutation: replay nothing at Start, and the seat is never woken; drop a copy
// this node still defers, and the second older node's start empties the
// subject; dead-letter a spent pass as a failure's, with the poison, and no
// node replays it.
func TestAPassedWakeNoNodeTookIsReplayedByTheFirstNodeThatReadsIt(t *testing.T) {
	ctx := t.Context()
	older := newService(t, func(o *notify.Options, _ *harness) {
		o.Parsers = []notify.Parser{versionedParser{reads: 1}}
	})
	start(t, older.svc)
	lead := inbox("engineering-lead")

	// POISON BESIDE IT: a delivery a failure retired, kept on the group's
	// poison subject — a wake a newer node would deliver, were it replayed.
	poison, err := json.Marshal(relayed("1", "op-poison"))
	if err != nil {
		t.Fatalf("encode the poison: %v", err)
	}
	if _, err := older.q.KeepDeadLetter(ctx, inboundPoison, "the poison", nil, poison); err != nil {
		t.Fatalf("KeepDeadLetter: %v", err)
	}

	// The twin dispatches inline: the publish returns once the older node
	// has passed the delivery through the group's whole budget.
	rec := relayed("2", "op-7")
	if err := older.q.Publish(ctx, topics.NotificationsInbound, rec); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	passed := passedOff(t, older, rec)
	older.quiet(t, lead)

	// ANOTHER OLDER NODE starts: it cannot read the record either.
	start(t, node(t, older, versionedParser{reads: 1}, nil))
	if again := passedOff(t, older, rec); again.Seq != passed.Seq {
		t.Fatalf("an older node's start replaced the passed copy %d with %d", passed.Seq, again.Seq)
	}
	older.quiet(t, lead)

	// THE FIRST NEWER NODE starts, and delivers what the group never did.
	start(t, node(t, older, versionedParser{reads: 2}, nil))
	woken := older.settledN(t, lead, 1)
	if len(woken) != 1 || woken[0].ID != changefeed.WakeID("op-7", "engineering-lead") {
		t.Fatalf("the seat was woken %d time(s), want once, by the wake derived from op-7",
			len(woken))
	}
	if left := inboundLetters(t, older.q, notify.UnhandledSubject()); len(left) != 0 {
		t.Fatalf("after the replay the inbound group holds %d unhandled copies, want none: "+
			"the replayed copy is dropped", len(left))
	}
	if left := inboundLetters(t, older.q, inboundPoison); len(left) != 1 {
		t.Fatalf("after the replay the inbound group's poison subject holds %d entries, want "+
			"the poison, left alone", len(left))
	}
}

// A PASSED COPY WHOSE WAKE COULD NOT BE DELIVERED IS KEPT for a later start,
// never dropped: the wake never happened, and its copy is the only one left.
// A newer node replays it — its parser reads the record — and the wake's
// publish fails, which a live delivery would answer with a NAK.
//
// Mutation: drop a copy whose handling was not an acknowledgement, and the
// failed delivery's copy is gone.
func TestAPassedCopyWhoseWakeFailedIsKeptForALaterStart(t *testing.T) {
	ctx := t.Context()
	older := newService(t, func(o *notify.Options, _ *harness) {
		o.Parsers = []notify.Parser{versionedParser{reads: 1}}
	})
	start(t, older.svc)
	rec := relayed("2", "op-8")
	if err := older.q.Publish(ctx, topics.NotificationsInbound, rec); err != nil {
		t.Fatalf("Publish: %v", err)
	}
	passed := passedOff(t, older, rec)

	boom := errors.New("broker unreachable")
	start(t, node(t, older, versionedParser{reads: 2}, func(q queue.EventQueue) queue.EventQueue {
		return failingWakes{EventQueue: q, boom: boom}
	}))
	older.quiet(t, inbox("engineering-lead"))
	if kept := passedOff(t, older, rec); kept.Seq != passed.Seq {
		t.Fatalf("a replay whose wake failed replaced the copy %d with %d", passed.Seq, kept.Seq)
	}
}

// failingWakes is a queue whose wakes, and nothing else, fail to publish.
type failingWakes struct {
	queue.EventQueue
	boom error
}

func (q failingWakes) Publish(ctx context.Context, topic string, ev *events.Event) error {
	if _, wake := topics.SeatFromInbox(topic); wake {
		return q.boom
	}
	return q.EventQueue.Publish(ctx, topic, ev)
}

// staged keeps ev on the inbound group's unhandled subject, as the queue keeps
// a delivery no member handled, under id, and answers the entry's sequence.
func staged(t *testing.T, h *harness, id string, ev *events.Event) uint64 {
	t.Helper()
	frame, err := json.Marshal(ev)
	if err != nil {
		t.Fatalf("encode the copy: %v", err)
	}
	if _, err := h.q.KeepDeadLetter(t.Context(), notify.UnhandledSubject(), id, nil, frame); err != nil {
		t.Fatalf("KeepDeadLetter: %v", err)
	}
	kept := inboundLetters(t, h.q, notify.UnhandledSubject())
	return kept[len(kept)-1].Seq
}

// panickingParser reads a relayed record as versionedParser does, and panics
// on the one whose change is panicsOn.
type panickingParser struct {
	versionedParser
	panicsOn string
}

func (p panickingParser) Parse(ctx context.Context, w types.RawWebhook, r *notify.Registry) ([]notify.Routed, error) {
	if change, _ := w.Body["change"].(string); change == p.panicsOn {
		panic("the parser met a record it was never written for")
	}
	return p.versionedParser.Parse(ctx, w, r)
}

// A REPLAYED COPY WHOSE HANDLING PANICS IS KEPT, AND THE REPLAY GOES ON.
//
// A live delivery runs inside the queue's recover, where a panic is a Nak and
// a line; the replay is no queue handler, so it contains each copy itself.
// Uncontained, a panic on one copy ended the process at every start of a node
// of the build that reads it — the only build that gets that far — because the
// copy, never dropped, was handled again at the next start.
//
// The replay is run on the case's own goroutine (ReplayNow), as Start's
// goroutine runs it, so a panic it fails to contain fails this case by name.
//
// Mutation: drop the recover from handleContained, and the panic fails the
// case.
func TestAReplayedCopyThatPanicsIsKeptAndTheReplayGoesOn(t *testing.T) {
	newer := newService(t, func(o *notify.Options, _ *harness) {
		o.Parsers = []notify.Parser{panickingParser{versionedParser{reads: 2}, "op-panic"}}
	})
	broken := relayed("2", "op-panic")
	brokenAt := staged(t, newer, "the copy that panics", broken)
	staged(t, newer, "the copy after it", relayed("2", "op-9"))

	newer.svc.ReplayNow(t.Context())
	woken := newer.settledN(t, inbox("engineering-lead"), 1)
	if len(woken) != 1 || woken[0].ID != changefeed.WakeID("op-9", "engineering-lead") {
		t.Fatalf("the seat was woken %d time(s), want once, by the copy after the one that "+
			"panicked", len(woken))
	}
	left := inboundLetters(t, newer.q, notify.UnhandledSubject())
	if len(left) != 1 || left[0].Seq != brokenAt {
		t.Fatalf("after the replay the unhandled subject holds %d entries, want the copy that "+
			"panicked alone, kept for a later start", len(left))
	}
}

// blockingParser reads a relayed record as versionedParser does, and holds the
// first one it is handed until released, saying when it has it.
type blockingParser struct {
	versionedParser
	once             *sync.Once
	entered, release chan struct{}
}

func (p blockingParser) Parse(ctx context.Context, w types.RawWebhook, r *notify.Registry) ([]notify.Routed, error) {
	p.once.Do(func() {
		close(p.entered)
		<-p.release
	})
	return p.versionedParser.Parse(ctx, w, r)
}

// A START RETURNS WHILE ITS REPLAY RUNS, AND A STOP ENDS THE REPLAY, KEEPING
// WHAT IT DID NOT REACH.
//
// The replay handles its copies one after another, as many as a paused roll
// left, and a start runs in the engine's boot and ahead of a first company's
// epoch: run inside Start, it held both for the whole backlog. So it runs on a
// goroutine of its own, which Stop ends — and a copy it never reached is still
// kept, for the next start. In a bubble, so a Start that waited for its replay
// is a deadlock the bubble reports, never a case that hangs.
//
// Mutation: replay inside Start, and every goroutine of the bubble is blocked
// on the held copy; end the replay at Stop without waiting for it, or never
// end it, and the second copy is replayed after the Stop returned.
func TestAStartReturnsWhileItsReplayRunsAndAStopEndsIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		parser := blockingParser{versionedParser: versionedParser{reads: 2}, once: &sync.Once{},
			entered: make(chan struct{}), release: make(chan struct{})}
		newer := newService(t, func(o *notify.Options, _ *harness) {
			o.Parsers = []notify.Parser{parser}
		})
		staged(t, newer, "the copy held", relayed("2", "op-10"))
		secondAt := staged(t, newer, "the copy never reached", relayed("2", "op-11"))

		if err := newer.svc.Start(t.Context()); err != nil {
			t.Fatalf("Start: %v", err)
		}
		<-parser.entered
		stopped := make(chan error, 1)
		go func() { stopped <- newer.svc.Stop(context.Background()) }()
		// THE STOP HAS ENDED THE REPLAY'S CONTEXT AND WAITS FOR IT, which is
		// held on its first copy until released.
		synctest.Wait()
		close(parser.release)
		if err := <-stopped; err != nil {
			t.Fatalf("Stop: %v", err)
		}
		left := inboundLetters(t, newer.q, notify.UnhandledSubject())
		if len(left) == 0 || left[len(left)-1].Seq != secondAt {
			t.Fatalf("after a stop mid-replay the unhandled subject holds %d entries, want the "+
				"copy the replay never reached among them", len(left))
		}
		newer.mu.Lock()
		woken := slices.Clone(newer.seen[inbox("engineering-lead")])
		newer.mu.Unlock()
		for _, ev := range woken {
			if ev.ID == changefeed.WakeID("op-11", "engineering-lead") {
				t.Fatal("the copy the stop came before was replayed after the stop returned")
			}
		}
	})
}

// windowValve counts each bucket in fixed windows a second wide, truncated from
// the zero time, as the fleet's counter behind the engine's valve does
// (internal/coord's Counter, at coord.RateWindow).
type windowValve struct {
	mu     sync.Mutex
	counts map[string]int
}

func (v *windowValve) Allow(_ context.Context, bucket string, limit int, now time.Time) (bool, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	key := bucket + "@" + now.Truncate(time.Second).String()
	v.counts[key]++
	return v.counts[key] <= limit, nil
}

func (v *windowValve) Reopens(now time.Time) time.Time {
	return now.Truncate(time.Second).Add(time.Second)
}

// A REPLAYED BURST IS PACED BY THE RATE VALVE, AND LOSES NO WAKE.
//
// A seat's copies piled up over a roll and are replayed one after another, a
// burst the live path would have spread over the roll; the valve drops what a
// live burst sends past its limit, so the replay waits each refused wake out to
// the window's end instead. Five copies for one seat limited to two a second
// reach it, all five, over three windows, and every copy is dropped. In a
// bubble, so the windows pass on the bubble's clock.
//
// Mutation: ask the valve once and skip the wake it refuses, as the live path
// does, and three wakes are lost with their copies; ask again without waiting
// for the window, and they are kept undelivered.
func TestAReplayedBurstIsPacedByTheValveAndLosesNoWake(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		newer := newService(t, func(o *notify.Options, _ *harness) {
			o.Parsers = []notify.Parser{versionedParser{reads: 2}}
			o.Valve = &windowValve{counts: map[string]int{}}
			// The bubble's clock, which its timers move.
			o.Now = nil
		})
		newer.limit = 2
		const copies = 5
		for i := range copies {
			staged(t, newer, fmt.Sprintf("copy %d", i), relayed("2", fmt.Sprintf("op-%d", i)))
		}

		start(t, newer.svc)
		woken := newer.settledN(t, inbox("engineering-lead"), copies)
		ids := map[string]bool{}
		for _, ev := range woken {
			ids[ev.ID.String()] = true
		}
		for i := range copies {
			if id := changefeed.WakeID(fmt.Sprintf("op-%d", i), "engineering-lead"); !ids[id.String()] {
				t.Errorf("the wake of op-%d never reached the seat: the valve's refusal lost it", i)
			}
		}
		if left := inboundLetters(t, newer.q, notify.UnhandledSubject()); len(left) != 0 {
			t.Errorf("after the replay the unhandled subject holds %d copies, want none: every "+
				"wake was delivered", len(left))
		}
		newer.quiet(t, skipTopic)
	})
}

// A REPLAYED WAKE THE VALVE STILL REFUSES IS KEPT, NEVER SKIPPED.
//
// Once its window has reopened, a refusal is the seat's own live mail filling
// its limit — the storm the valve is for — and the replay neither joins it nor
// waits it out: it keeps the copy for a later start, records no skip, and goes
// on. The valve is asked twice, before the window reopens and after.
//
// Mutation: skip a replayed wake the valve refuses, as the live path does, and
// its copy is dropped with the wake never delivered.
func TestAReplayedWakeTheValveStillRefusesIsKept(t *testing.T) {
	newer := newService(t, func(o *notify.Options, _ *harness) {
		o.Parsers = []notify.Parser{versionedParser{reads: 2}}
	})
	newer.limit = 1
	newer.valve.allow = false
	at := staged(t, newer, "the copy refused", relayed("2", "op-20"))

	start(t, newer.svc)
	newer.quiet(t, inbox("engineering-lead"))
	newer.quiet(t, skipTopic)
	if left := inboundLetters(t, newer.q, notify.UnhandledSubject()); len(left) != 1 || left[0].Seq != at {
		t.Fatalf("after the replay the unhandled subject holds %d copies, want the one the "+
			"valve refused, kept", len(left))
	}
	if asked := newer.valve.seen(); asked != 2 {
		t.Errorf("the valve was asked %d time(s), want twice: once, and once its window "+
			"had reopened", asked)
	}
}

// A REPLAYED COPY THAT READS AS NO EVENT IS KEPT, never dropped by a node that
// could not judge it: the bytes are a delivery's copy as the queue kept it,
// and a build that reads them as one may yet come.
//
// Mutation: drop a copy whose body does not decode, and it is gone.
func TestAReplayedCopyThatReadsAsNoEventIsKept(t *testing.T) {
	newer := newService(t, func(o *notify.Options, _ *harness) {
		o.Parsers = []notify.Parser{versionedParser{reads: 2}}
	})
	if _, err := newer.q.KeepDeadLetter(t.Context(), notify.UnhandledSubject(), "no event", nil,
		[]byte("this is no event")); err != nil {
		t.Fatalf("KeepDeadLetter: %v", err)
	}

	start(t, newer.svc)
	if left := inboundLetters(t, newer.q, notify.UnhandledSubject()); len(left) != 1 {
		t.Fatalf("after the replay the unhandled subject holds %d entries, want the copy that "+
			"reads as no event, kept", len(left))
	}
	newer.quiet(t, inbox("engineering-lead"))
}

// A REPLAYED COPY A SHEDDING NODE HANDS BACK IS KEPT, for a start whose node
// admits work: a node whose config posture sheds defers every delivery before
// it parses anything, the replay's copies among them, and a copy it handed back
// is one whose wake nobody delivered.
//
// Mutation: drop a copy the node deferred, and its wake is lost with it.
func TestAReplayedCopyASheddingNodeDefersIsKept(t *testing.T) {
	newer := newService(t, func(o *notify.Options, _ *harness) {
		o.Parsers = []notify.Parser{versionedParser{reads: 2}}
	})
	newer.admits = false
	at := staged(t, newer, "the copy deferred", relayed("2", "op-30"))

	start(t, newer.svc)
	newer.quiet(t, inbox("engineering-lead"))
	if left := inboundLetters(t, newer.q, notify.UnhandledSubject()); len(left) != 1 || left[0].Seq != at {
		t.Fatalf("after a shedding node's replay the unhandled subject holds %d entries, want "+
			"the copy it deferred, kept", len(left))
	}
}
