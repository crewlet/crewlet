package engine_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerstore"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/chatlet"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/sandboxtest"
)

// A PERSON'S CHAT REPLY TO A PARKED RUN IS RECORDED AS WHAT THEY TYPED, AND THE
// RUN READS IT UNDER WHO WROTE IT.
//
// Ana answers the question swe's coding run stopped on, in the Chatlet thread
// it asked in, with "Use Postgres". The delivery reaches swe's inbox as the
// notification service builds every wake: its body is the seat's prompt, the
// message fenced under the wake's token among the surface's triage guidance
// and how to reply, and its salient body is the message. What the answer
// records is read back to Ana among her answered decisions (ADR-0068:
// `sandbox_run_answered`'s `answer` is the answer's body there), sealed onto
// the run's row, and handed to the run as her answer, so each must be exactly
// what she typed, with none of the prompt and no fence line: a fence is a
// model's. The run reads it fenced (runner.Asked), labelled as the trigger
// labels the message: read in a chat thread, written by ana, a colleague's.
//
// Mutation: offer the coordinator [engine.DescribeTrigger] of the delivery
// (as the dispatch did) and every assertion on the words is red; offer it
// unlabelled and the label's is.
func TestAChatReplyToAParkedRunIsRecordedAsWhatThePersonTyped(t *testing.T) {
	t.Parallel()
	const typed = "Use Postgres"
	rig := newChatAnswerRig(t)
	wake := rig.chatletWake(t, "ana", typed)
	if body := wakeBody(t, wake); !strings.Contains(body, typed) || body == typed {
		t.Fatalf("the wake's body is %q, want the seat's prompt around the message: "+
			"the case asserts the answer is the message and not the prompt", body)
	}
	rig.park(t, wake)

	if got := rig.dispatch.Dispatch(t.Context(), "swe", []*events.Event{wake}); got.Outcome != queue.OutcomeAck {
		t.Fatalf("the reply was not taken as the answer: %+v", got)
	}
	asked := rig.resumedWith(t)
	if asked.Answer != typed {
		t.Errorf("the run was handed %q as the answer, want %q", asked.Answer, typed)
	}
	if words, err := fleetAnswers().Open(rig.resumer.row()); err != nil || words.Text != typed {
		t.Errorf("the answer sealed onto the run opens to %q (%v), want %q", words.Text, err, typed)
	}
	answered := rig.announced(t)
	if answered.Answer != typed {
		t.Errorf("sandbox_run_answered says the answer was %q, want %q: it is the "+
			"answer's body among the person's answered decisions", answered.Answer, typed)
	}
	if answered.AnsweredBy != "ana" {
		t.Errorf("the answer is recorded as %q's, want ana's", answered.AnsweredBy)
	}
	want := provenance.Label{Source: provenance.SourceChatThread, Trust: provenance.TrustColleague,
		Authors: []provenance.Author{{Kind: iam.ActorHuman, Seat: "ana"}}}
	if asked.Label == nil || asked.Label.Source != want.Source || asked.Label.Trust != want.Trust ||
		!slices.Equal(asked.Label.Authors, want.Authors) {
		t.Errorf("the run reads the reply under %+v; want it read as the trigger labels "+
			"the message: %+v", asked.Label, want)
	}
}

// chatAnswerRig is a seat, swe, with a coding run parked on a question it
// asked in a Chatlet thread, a person, ana, who can answer it there, and the
// dispatch a reply reaches swe through.
type chatAnswerRig struct {
	org         *org.Organization
	queue       queue.EventQueue
	store       *sandbox.CoordStore
	resumer     *askedResumer
	coordinator *sandbox.Coordinator
	dispatch    *engine.Dispatcher

	mu         sync.Mutex
	announcing []*events.Event
}

func newChatAnswerRig(t *testing.T) *chatAnswerRig {
	t.Helper()
	ctx := t.Context()
	o := &org.Organization{Name: fixtureOrg, Roles: []*org.Role{
		{Name: "SWE", DeclaredHandle: "swe"},
		{Name: "Ana Lima", DeclaredHandle: "ana", Kind: org.KindHuman},
	}}
	o.Normalize()
	q := memory.New()
	if err := q.Start(ctx); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(ctx)) })
	rig := &chatAnswerRig{org: o, queue: q, store: sandbox.NewCoordStore(coordmemory.NewFleet())}
	if err := q.Subscribe(ctx, topics.Event(types.SandboxRunAnswered{}.EventType()), "answered",
		func(_ context.Context, ev *events.Event) queue.Result {
			rig.mu.Lock()
			rig.announcing = append(rig.announcing, ev)
			rig.mu.Unlock()
			return queue.Ack()
		}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	manager, err := sandbox.NewManager(sandbox.ManagerOptions{
		Providers: map[sandbox.Placement]sandbox.Provider{sandbox.Direct: sandbox.NewFakeProvider()},
		Runners:   map[string]sandbox.Runner{"claude-code": sandbox.NewFakeRunner("claude-code")},
	})
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	rig.resumer = &askedResumer{}
	coordinator, err := sandbox.NewCoordinator(sandbox.CoordinatorOptions{
		Answers:  fleetAnswers(),
		Audience: noAudience{}, Queue: q, Pending: rig.store, Manager: manager,
		Resume: rig.resumer, Hold: inboxHold{q: q}, After: (&handCranked{}).after,
		Lease: func(string) (sandbox.Fence, bool) {
			return sandbox.Fence{Owner: "node-a", Epoch: 1}, true
		},
	})
	if err != nil {
		t.Fatalf("NewCoordinator: %v", err)
	}
	t.Cleanup(coordinator.Stop)
	reg := notify.NewRegistry(o, nil)
	rig.dispatch = &engine.Dispatcher{
		// THE NODE'S PROMPTS AND PARTIES, which a reply is labelled by as its
		// trigger was.
		Prompts:     func() notify.Prompts { return notify.NewPrompts(chatlet.NewPrompt()) },
		Parties:     func() notify.Parties { return reg },
		Ledgered:    func(kind string) bool { return kind == notificationType },
		Completions: ledgerstore.NewMemoryCompletions(),
		Answer:      coordinator.TryResumeFromAnswer,
		Turn: func(_ context.Context, req engine.Request) (turn.Result, error) {
			t.Errorf("the reply was worked as a turn (%q): it answers the parked run",
				engine.DescribeTrigger(req.Events, ""))
			return turn.Result{}, nil
		},
		Conditions: func(handle string) inbox.Conditions {
			held, awaits := coordinator.SeatRuns(handle)
			return inbox.Conditions{Owned: true, TurnEngineReady: true, AdmitsTriggers: true,
				SeatHeldBySandbox: held, SandboxAwaitsAnswer: awaits}
		},
		Park:         func(context.Context, string, []*events.Event) error { return nil },
		NoteDeferred: func(string) {},
	}
	rig.coordinator = coordinator
	return rig
}

// chatletWake is author's reply in the Chatlet thread swe's run asked in, as
// the notification service delivers it to swe: through Chatlet's own parser
// and prompt, over the company's registry.
func (rig *chatAnswerRig) chatletWake(t *testing.T, author, body string) *events.Event {
	t.Helper()
	post, err := json.Marshal(chatlet.MessagePost{V: 1, MessageID: "msg-" + author, Body: body,
		ThreadRoot: "msg-root"})
	if err != nil {
		t.Fatalf("encode the post: %v", err)
	}
	record := chatlet.MutationRecord{
		RecordEnvelope: chatlet.RecordEnvelope{
			V: chatlet.RecordVersion, OpID: "op-" + author,
			Subject: chatlet.MessageSubject("room-1"), Op: chatlet.OpPost, Writer: "node-a",
		},
		Mutation: post,
		Actor:    author, ActorKind: chatlet.AuthorHuman,
		Notify: &chatlet.Notify{
			MessageID: "msg-" + author, ChannelID: "room-1", ChannelName: "eng",
			ChannelKind: chatlet.KindPublic, ThreadRoot: "msg-root",
			Author: author, AuthorKind: chatlet.AuthorHuman, Excerpt: chatlet.Excerpt(body),
			Recipients: []chatlet.Recipient{{Handle: "swe", Reason: chatlet.ReasonFollow}},
		},
	}
	payload, err := chatlet.Encode(record)
	if err != nil {
		t.Fatalf("encode the record: %v", err)
	}
	delivery, wakes, err := chatlet.NewTranslator(nil).Translate(t.Context(),
		changefeed.Record{Payload: payload, Key: record.Subject.Wire()})
	if err != nil || !wakes {
		t.Fatalf("the record relayed nothing: %v", err)
	}
	raw := events.New(types.RawWebhook{Body: delivery.Body, Headers: map[string]string{}},
		events.NewTrace())
	raw.Source = chatlet.Source

	q := memory.New()
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
	var (
		mu    sync.Mutex
		woken []*events.Event
	)
	if err := q.Subscribe(t.Context(), fixtureInbox("swe"), "collect",
		func(_ context.Context, ev *events.Event) queue.Result {
			mu.Lock()
			woken = append(woken, ev)
			mu.Unlock()
			return queue.Ack()
		}); err != nil {
		t.Fatalf("Subscribe: %v", err)
	}
	reg := notify.NewRegistry(rig.org, nil)
	svc, err := notify.New(notify.Options{
		Queue:    q,
		Registry: func() *notify.Registry { return reg },
		Prompts:  notify.NewPrompts(chatlet.NewPrompt()),
		Parsers:  []notify.Parser{chatlet.NewParser()},
	})
	if err != nil {
		t.Fatalf("notify.New: %v", err)
	}
	if got := svc.Handle(t.Context(), raw); got.Outcome != queue.OutcomeAck {
		t.Fatalf("the delivery was not handled: %+v", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(woken) != 1 {
		t.Fatalf("swe was woken %d times, want once", len(woken))
	}
	return woken[0]
}

// park leaves swe's run parked on a question asked in the conversation wake
// arrived in, a minute before it was written.
func (rig *chatAnswerRig) park(t *testing.T, wake *events.Event) {
	t.Helper()
	ctx := t.Context()
	conv, _ := wake.Payload[notify.ConversationField].(string)
	part, _ := wake.Payload[notify.PartitionField].(string)
	if conv == "" || part == "" {
		t.Fatalf("the wake states no conversation (%q) or partition (%q)", conv, part)
	}
	run := sandbox.PendingRun{
		TurnID: "t1", AgentHandle: "swe", AgentID: fixtureSeat("swe").String(), Role: "SWE",
		CodingAgent: "claude-code", ConversationKey: conv, PartitionKey: part,
	}
	if _, err := rig.store.BeginLaunch(ctx, run, sandbox.Fence{}); err != nil {
		t.Fatalf("BeginLaunch: %v", err)
	}
	if _, err := rig.store.MarkSuspended(ctx, "t1",
		sandbox.Suspension{State: json.RawMessage(`{"messages":[]}`)}); err != nil {
		t.Fatalf("MarkSuspended: %v", err)
	}
	sandboxtest.Ask(t, rig.store, "t1", sandbox.Ask{Question: "which database?",
		To: sandbox.AskToRequester})
	if err := rig.store.MarkAwaiting(ctx, "t1",
		sandbox.Clarification{AskedAt: wake.Timestamp.Add(-time.Minute)}); err != nil {
		t.Fatalf("MarkAwaiting: %v", err)
	}
	// THE SEAT TAKEN WITH ITS RUN PARKED, as a node that holds it finds it.
	if err := rig.coordinator.RecoverSeat(ctx, "swe", "node-a", 1); err != nil {
		t.Fatalf("RecoverSeat: %v", err)
	}
}

// resumedWith is the answered question the run was resumed with.
func (rig *chatAnswerRig) resumedWith(t *testing.T) sandbox.AskedRun {
	t.Helper()
	eventually(t, "the run to be resumed with the answer", func() bool {
		return rig.resumer.resumed() != nil
	})
	return *rig.resumer.resumed()
}

// announced is the one sandbox_run_answered the answer was recorded with.
func (rig *chatAnswerRig) announced(t *testing.T) types.SandboxRunAnswered {
	t.Helper()
	eventually(t, "the answer to be announced", func() bool {
		rig.mu.Lock()
		defer rig.mu.Unlock()
		return len(rig.announcing) > 0
	})
	rig.mu.Lock()
	defer rig.mu.Unlock()
	got, ok := events.DataAs[*types.SandboxRunAnswered](rig.announcing[0])
	if !ok || got == nil {
		t.Fatalf("the announcement is %T, want sandbox_run_answered", rig.announcing[0].Data)
	}
	return *got
}

// askedResumer resumes a run as a turn would, keeping the answered question it
// was handed and the row it was resumed from, which holds the sealed answer
// until the resumed run settles.
type askedResumer struct {
	mu    sync.Mutex
	asked *sandbox.AskedRun
	run   sandbox.PendingRun
}

func (r *askedResumer) Resume(_ context.Context, req sandbox.ResumeRequest) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.run = req.Run
	if req.Asked != nil {
		asked := *req.Asked
		r.asked = &asked
	}
	return nil
}

func (r *askedResumer) row() sandbox.PendingRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.run
}

func (r *askedResumer) resumed() *sandbox.AskedRun {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.asked
}

// wakeBody is the prompt a wake hands the seat.
func wakeBody(t *testing.T, ev *events.Event) string {
	t.Helper()
	n, ok := events.DataAs[*types.ExternalNotification](ev)
	if !ok || n == nil {
		t.Fatalf("the wake is %T, want a notification", ev.Data)
	}
	return n.Body
}
