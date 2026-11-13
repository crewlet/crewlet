package notify_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"testing"
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

// start starts svc, failing the case on an error.
func start(t *testing.T, svc *notify.Service) {
	t.Helper()
	if err := svc.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
}

// inboundLetters is what the inbound group's dead-letter subject holds.
func inboundLetters(t *testing.T, q queue.EventQueue) []queue.Kept {
	t.Helper()
	kept, err := q.DeadLettersOn(t.Context(), notify.DeadLetterSubject())
	if err != nil {
		t.Fatalf("DeadLettersOn: %v", err)
	}
	return kept
}

// passedOff is the inbound group's one passed copy after an older node passed
// ev until its budget was spent: kept under the header naming the pass for a
// newer record, its body ev's own bytes.
func passedOff(t *testing.T, h *harness, ev *events.Event) queue.Kept {
	t.Helper()
	letters := inboundLetters(t, h.q)
	var passed []queue.Kept
	for _, k := range letters {
		if _, ok := queue.PassedReason(k.Header); ok {
			passed = append(passed, k)
		}
	}
	if len(passed) != 1 {
		t.Fatalf("the inbound group holds %d passed copies among %d dead letters, want the "+
			"one the older node's passes retired", len(passed), len(letters))
	}
	if reason, _ := queue.PassedReason(passed[0].Header); reason != string(notify.DeferNewerRecord) {
		t.Fatalf("the copy was passed for %q, want %q", reason, notify.DeferNewerRecord)
	}
	frame, held, err := h.q.DeadLetterFrame(t.Context(), notify.DeadLetterSubject(), passed[0].Seq)
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
// it, under the header naming the pass (queue.PassedLetter), and the replay a
// node's start makes of it (crewlet#993). End to end on one broker, through
// the real service and the queue's own budget:
//
//   - an older node, whose parser refuses the record as newer than it reads,
//     passes the delivery until the budget is spent, and the queue keeps its
//     copy under the pass's reason;
//   - another older node starting reads it no better, delivers nothing and
//     keeps the copy;
//   - a newer node starting delivers the wake to the seat it names, under the
//     id derived from the change, and drops the copy — and leaves the poison
//     beside it, a delivery whose failure, not a pass, retired it.
//
// Mutation: replay nothing at Start, and the seat is never woken; drop a copy
// this node still defers, and the second older node's start empties the
// subject; dead-letter a spent pass as a failure's, with no header, and no
// node replays it.
func TestAPassedWakeNoNodeTookIsReplayedByTheFirstNodeThatReadsIt(t *testing.T) {
	ctx := t.Context()
	older := newService(t, func(o *notify.Options, _ *harness) {
		o.Parsers = []notify.Parser{versionedParser{reads: 1}}
	})
	start(t, older.svc)
	lead := inbox("engineering-lead")

	// POISON BESIDE IT: a delivery a failure retired, which carries no
	// pass's header — a wake a newer node would deliver, were it replayed.
	poison, err := json.Marshal(relayed("1", "op-poison"))
	if err != nil {
		t.Fatalf("encode the poison: %v", err)
	}
	if _, err := older.q.KeepDeadLetter(ctx, notify.DeadLetterSubject(), "the poison", nil, poison); err != nil {
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
	letters := inboundLetters(t, older.q)
	if len(letters) != 1 || len(letters[0].Header) != 0 {
		t.Fatalf("after the replay the inbound group holds %d dead letter(s), want the "+
			"poison alone: the replayed copy dropped, the poison left", len(letters))
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
