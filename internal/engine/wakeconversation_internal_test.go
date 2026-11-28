package engine

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/changefeed"
	"github.com/crewlet/crewlet/internal/chatlet"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// prioritize is the words a person posted twice, in two threads of one room.
const prioritize = "please prioritize this work for me and set a due date for it in the next 8 hours"

// threadKey is the conversation identity of room's thread under root.
func threadKey(room, root string) string {
	return chatlet.MessageConversation(chatlet.Channel{ID: room, Kind: chatlet.KindPublic},
		chatlet.Message{ID: root + "-reply", ThreadRoot: root})
}

// chatReply publishes to swe's inbox author's reply in room's thread under
// root, reaching swe for reason, as the chat parser and the notification
// service deliver it; its message id is root-reason.
func (r *routerRig) chatReply(t *testing.T, author, room, root, words string, reason chatlet.Reason) *events.Event {
	t.Helper()
	record := chatlet.MutationRecord{
		RecordEnvelope: chatlet.RecordEnvelope{
			V: chatlet.RecordVersion, OpID: "op-" + room + root + string(reason) + words,
			Subject: chatlet.MessageSubject(room), Op: chatlet.OpPost, Writer: "node-a",
		},
		Actor: author, ActorKind: chatlet.AuthorHuman,
		Notify: &chatlet.Notify{
			MessageID: root + "-" + string(reason), ChannelID: room, ChannelName: "launch",
			ChannelKind: chatlet.KindPublic, ThreadRoot: root, Author: author,
			AuthorKind: chatlet.AuthorHuman, Excerpt: words,
			Recipients: []chatlet.Recipient{{Handle: "swe", Reason: reason}},
		},
	}
	payload, err := chatlet.Encode(record)
	if err != nil {
		t.Fatal(err)
	}
	delivery, wake, err := chatlet.NewTranslator(nil).Translate(t.Context(),
		changefeed.Record{Payload: payload, Key: record.Subject.Wire()})
	if err != nil || !wake {
		t.Fatalf("the chat record relayed nothing: %v", err)
	}
	ev := deliveredTo(t, r.e.Company().Org, r.e.notify.registry, "swe",
		rawDelivery(t, chatlet.Source, delivery.Body))
	if err := r.q.Publish(t.Context(), r.subject, ev); err != nil {
		t.Fatal(err)
	}
	return ev
}

// takenReceipts records each take the engine writes, or refuses every one,
// signalling each on landed.
type takenReceipts struct {
	mu     sync.Mutex
	took   []string
	refuse error
	landed chan struct{}
}

// await is every take once n have landed, or the test's end.
func (k *takenReceipts) await(t *testing.T, n int) []string {
	t.Helper()
	for len(k.all()) < n {
		select {
		case <-k.landed:
		case <-t.Context().Done():
			t.Fatalf("%d takes landed, want %d", len(k.all()), n)
		}
	}
	return k.all()
}

func (k *takenReceipts) TakeReceipt(_ context.Context, actor chatlet.Actor, room, message, handle string,
	as chatlet.Taken) (chatlet.Written, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.refuse != nil {
		return chatlet.Written{}, k.refuse
	}
	if actor.Kind != chatlet.AuthorSystem {
		return chatlet.Written{}, errors.New("a take written as somebody other than the engine")
	}
	k.took = append(k.took, handle+" "+string(as)+" "+room+"/"+message)
	k.landed <- struct{}{}
	return chatlet.Written{}, nil
}

func (k *takenReceipts) all() []string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.Clone(k.took)
}

// busyInThread is a triage rig whose run of swe works for boss, a person who
// leads it, in room-1's thread under root-a, the scripted triage playing
// runs, every take recorded through the engine's hand-over.
func busyInThread(t *testing.T, runs ...decided) (*routerRig, *takenReceipts) {
	t.Helper()
	r := newTriageRigFor(t, newRouterRigFor(t, "boss", threadKey("room-1", "root-a")), triageRuns(runs)...)
	takes := &takenReceipts{landed: make(chan struct{}, 8)}
	r.e.receipts = takes
	r.runs.telling(func(ctx context.Context, handle string, took tookWakes) {
		r.e.tookOver(ctx, handle, took)
		r.handed <- took
	})
	return r, takes
}

// decided is one triage's decision and reason, as the scripted model gives it.
type decided struct{ decision, reason string }

func triageRuns(runs []decided) []llmtest.Run {
	out := make([]llmtest.Run, 0, len(runs))
	for _, d := range runs {
		out = append(out, decisionRun(d.decision, d.reason))
	}
	return out
}

// runsAlone reports whether ev's delivery still runs as a turn of its own.
func runsAlone(t *testing.T, r *routerRig, ev *events.Event) bool {
	t.Helper()
	d := &Dispatcher{Completions: r.ledger, Ledgered: inbox.Ledgered}
	surviving, _ := d.dropWorked(t.Context(), "swe", []*events.Event{ev})
	return len(surviving) == 1
}

// THE SAME WORDS FROM ANOTHER CONVERSATION ARE NEVER ABSORBED BY THE RUNNING
// WORK. A person who leads the seat posts, in another thread of the room (or
// in another room), the words the running turn was asked in its own thread;
// whatever the triage says (here, the incident's "repeats the running task's
// request word for word"), the engine keeps it for a turn of its own, as a
// reply in a thread the seat follows and as a mention alike, and the triage is
// told where it was written.
//
// Mutation: drop the conversation fact from absorbable, and the reply case
// goes red; tell the triage nothing of the place, and the prompt case does.
func TestTheSameWordsFromAnotherConversationAreNeverAbsorbed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, room string
		reason     chatlet.Reason
		place      string
	}{
		{"a reply in another thread the seat follows", "room-1", chatlet.ReasonReply,
			"wrote it elsewhere in the room the running task works in"},
		{"a mention in another thread", "room-1", chatlet.ReasonMention,
			"wrote it elsewhere in the room the running task works in"},
		{"a reply in another room", "room-2", chatlet.ReasonReply,
			"wrote it in another conversation than the one the running task works in"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r, takes := busyInThread(t, decided{"noise",
				"The message repeats the running task's request word for word."})
			other := r.chatReply(t, "boss", tc.room, "root-b", prioritize, tc.reason)
			memo := newRouteMemo()
			r.e.routeSeat(t.Context(), "swe", r.subject, r.group, memo)
			if !memo.done(r.job, other.ID.String()) {
				t.Fatal("the triaged wake is undecided, so every read weighs it again")
			}
			if !runsAlone(t, r, other) {
				t.Error("the message from another conversation was absorbed by the running work, " +
					"so its writer is never answered")
			}
			if got := takes.all(); len(got) != 0 {
				t.Errorf("its receipt says the running turn took it (%v), want it waiting for its own turn", got)
			}
			if got := triagePrompt(t, r); !strings.Contains(got, tc.place) {
				t.Errorf("the triage was not told where the message was written (%q):\n%s", tc.place, got)
			}
		})
	}
}

// A PERSON'S MESSAGE TO THE SEAT A RUNNING TURN TAKES SAYS SO ON ITS RECEIPT,
// AND ONE IT CANNOT SAY SO ON IS NEVER ABSORBED. A lead's mention triaged as a
// steer is filed as their note and its receipt says joined; another person's
// mention triaged as input says joined once the run reads it; a person's reply
// in the run's own thread, triaged noise, is absorbed only after its receipt
// says so, and stays for a turn of its own where the receipt cannot.
//
// Mutation: drop the take from the hand-over, absorb before the receipt says
// so, or absorb a person's reply whose receipt refused, and a case goes red.
func TestAPersonsMessageTakenByARunningTurnSaysSoOnItsReceipt(t *testing.T) {
	t.Parallel()
	t.Run("a lead's steer from another thread", func(t *testing.T) {
		t.Parallel()
		r, takes := busyInThread(t, decided{"steer", "it changes course"})
		stop := r.chatReply(t, "boss", "room-1", "root-b", "@swe stop the migration", chatlet.ReasonMention)
		r.e.routeSeat(t.Context(), "swe", r.subject, r.group, newRouteMemo())
		if runsAlone(t, r, stop) {
			t.Fatal("the lead's steer was not filed as their note")
		}
		if got := takes.await(t, 1); !slices.Equal(got, []string{"swe joined room-1/root-b-mention"}) {
			t.Errorf("the steered message's receipt says %v, want joined", got)
		}
	})
	t.Run("another person's input, once read", func(t *testing.T) {
		t.Parallel()
		r, takes := busyInThread(t, decided{"input", "a fact the work needs"})
		fact := r.chatReply(t, "ceo", "room-1", "root-a", "@swe the customer is on the EU region",
			chatlet.ReasonMention)
		r.e.routeSeat(t.Context(), "swe", r.subject, r.group, newRouteMemo())
		if got := takes.all(); len(got) != 0 {
			t.Fatalf("the receipt says %v before the run read the wake", got)
		}
		box := boundInbox(t, r.runs, r.store, "run-1", r.job)
		viewHolds(t, box, 1)
		read := box.Take(t.Context(), mcpbridge.SteerEventToolBatch, hookCall())
		settled, err := r.store.SettleHook(t.Context(), "run-1", sandbox.HookSettle{Launch: r.job,
			Held: []string{takeOf(t, read)}})
		if err != nil {
			t.Fatal(err)
		}
		r.runs.confirmed(t.Context(), mustRow(t, r.store, "run-1"), settled)
		<-r.handed
		if runsAlone(t, r, fact) {
			t.Error("the read input still runs as a turn of its own")
		}
		if got := takes.await(t, 1); !slices.Equal(got, []string{"swe joined room-1/root-a-mention"}) {
			t.Errorf("the read input's receipt says %v, want joined", got)
		}
	})
	t.Run("a person's thanks in the run's own thread", func(t *testing.T) {
		t.Parallel()
		r, takes := busyInThread(t, decided{"noise", "thanks"})
		thanks := r.chatReply(t, "ceo", "room-1", "root-a", "thanks!", chatlet.ReasonReply)
		r.e.routeSeat(t.Context(), "swe", r.subject, r.group, newRouteMemo())
		if runsAlone(t, r, thanks) {
			t.Error("noise in the run's own thread was not absorbed")
		}
		if got := takes.all(); !slices.Equal(got, []string{"swe absorbed room-1/root-a-reply"}) {
			t.Errorf("the absorbed message's receipt says %v, want absorbed", got)
		}
	})
	t.Run("a person's thanks whose receipt could not be reached", func(t *testing.T) {
		t.Parallel()
		r, takes := busyInThread(t, decided{"noise", "thanks"})
		takes.refuse = errors.New("the chat log could not be reached")
		thanks := r.chatReply(t, "ceo", "room-1", "root-a", "thanks!", chatlet.ReasonReply)
		memo := newRouteMemo()
		r.e.routeSeat(t.Context(), "swe", r.subject, r.group, memo)
		if !runsAlone(t, r, thanks) || memo.done(r.job, thanks.ID.String()) {
			t.Error("a message whose receipt could not say so was absorbed, or decided so no read asks again")
		}
	})
	t.Run("a person's thanks whose message keeps no receipt", func(t *testing.T) {
		t.Parallel()
		r, takes := busyInThread(t, decided{"noise", "thanks"})
		takes.refuse = chatlet.ErrNotFound
		thanks := r.chatReply(t, "ceo", "room-1", "root-a", "thanks!", chatlet.ReasonReply)
		memo := newRouteMemo()
		r.e.routeSeat(t.Context(), "swe", r.subject, r.group, memo)
		if !runsAlone(t, r, thanks) || !memo.done(r.job, thanks.ID.String()) {
			t.Error("a message with no receipt to say so was absorbed, or left undecided so every read asks again")
		}
	})
}

// A PERSON'S MESSAGE TO THE SEAT WHOSE MESSAGE KEEPS NO RECEIPT IS NEVER
// ABSORBED, and noise that is no person's message to it still is. Decided by
// the engine's facts alone.
//
// Mutation: absorb a person's message to the seat whatever its receipt, or
// none from the run's own conversation, and a case goes red.
func TestOnlyNoiseFromTheRunsOwnConversationThatCanSaySoIsAbsorbable(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		f    wakeFacts
		want bool
	}{
		{"chatter in the run's conversation", wakeFacts{here: true}, true},
		{"a person's reply its receipt says", wakeFacts{here: true, person: true, addresses: true, marked: true}, true},
		{"a person's reply with no receipt", wakeFacts{here: true, person: true, addresses: true}, false},
		{"chatter from another conversation", wakeFacts{}, false},
		{"a person's reply from another conversation", wakeFacts{person: true, addresses: true, marked: true}, false},
		{"a message owing an answer", wakeFacts{here: true, owes: true, marked: true}, false},
	} {
		if got := absorbable(tc.f); got != tc.want {
			t.Errorf("%s: absorbable %v, want %v", tc.name, got, tc.want)
		}
	}
}

// A SAME-THREAD DUPLICATE FROM THE RUN'S REQUESTER STILL JOINS AS TODAY: the
// fixed rule files it on the run's record with no triage, and once read it is
// claimed and its delivery skipped joined; its receipt now says so too.
//
// Mutation: triage the requester's own follow-up, or leave it running beside
// the run, and this goes red.
func TestASameThreadDuplicateStillJoinsTheRunningRun(t *testing.T) {
	t.Parallel()
	r, takes := busyInThread(t)
	again := r.chatReply(t, "boss", "room-1", "root-a", "@swe "+prioritize, chatlet.ReasonMention)
	r.e.routeSeat(t.Context(), "swe", r.subject, r.group, newRouteMemo())
	if len(r.model.Seen()) != 0 {
		t.Fatal("the requester's same-thread duplicate was triaged")
	}
	if got := r.filed(t); !slices.Equal(got, []string{again.ID.String()}) {
		t.Fatalf("the record holds wakes %v, want the duplicate", got)
	}
	box := boundInbox(t, r.runs, r.store, "run-1", r.job)
	viewHolds(t, box, 1)
	read := box.Take(t.Context(), mcpbridge.SteerEventToolBatch, hookCall())
	settled, err := r.store.SettleHook(t.Context(), "run-1", sandbox.HookSettle{Launch: r.job,
		Held: []string{takeOf(t, read)}})
	if err != nil {
		t.Fatal(err)
	}
	r.runs.confirmed(t.Context(), mustRow(t, r.store, "run-1"), settled)
	<-r.handed
	if runsAlone(t, r, again) {
		t.Error("the read duplicate still runs as a turn of its own")
	}
	if got := takes.await(t, 1); !slices.Equal(got, []string{"swe joined room-1/root-a-mention"}) {
		t.Errorf("the duplicate's receipt says %v, want joined", got)
	}
}

// triagePrompt is everything the rig's triage model was handed.
func triagePrompt(t *testing.T, r *routerRig) string {
	t.Helper()
	var b strings.Builder
	for _, req := range r.model.Seen() {
		for _, m := range req.Messages {
			b.WriteString(m.Content)
		}
	}
	return b.String()
}
