package notify_test

import (
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// A WAKE THAT OWES A PERSON AN ANSWER RIDES THE SEAT'S PRIORITY LANE, AND
// NOTHING ELSE DOES.
//
// On a live company a person's direct message reached its seat in a second
// and was answered hours later, after the seat had worked through every
// tracker notice queued ahead of it. The lane is what takes the message ahead
// of that backlog, and this is the rule that decides which wakes ride it: the
// seat owes an answer by the backend's own address rule, and a person wrote
// what it answers. A colleague's message waits its turn with the rest of the
// seat's work, and so does a person's remark that asked the seat nothing.
func TestOnlyAWakeOwedToAPersonRidesThePriorityLane(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		sender string
		kind   iam.ActorKind
		meta   map[string]string
		want   bool
	}{{
		name: "a person's direct message", sender: "dana-founder", kind: iam.ActorHuman,
		meta: map[string]string{"channel": "D1", "channel_type": "D", "ts": "p1"},
		want: true,
	}, {
		name: "an operator's direct message", sender: "", kind: iam.ActorOperator,
		meta: map[string]string{"channel": "D1", "channel_type": "D", "ts": "p2"},
		want: true,
	}, {
		name: "an account nobody in the company holds", sender: "U-OUTSIDE",
		meta: map[string]string{"channel": "D1", "channel_type": "D", "ts": "p3"},
		want: true,
	}, {
		name: "a colleague's direct message", sender: "backend-engineer", kind: iam.ActorAgent,
		meta: map[string]string{"channel": "D1", "channel_type": "D", "ts": "p4"},
		want: false,
	}, {
		name:   "a person's remark in a room that asks the seat nothing",
		sender: "dana-founder", kind: iam.ActorHuman,
		meta: map[string]string{"channel": "C1", "channel_type": "C", "ts": "p5"},
		want: false,
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newService(t, func(o *notify.Options, _ *harness) {
				o.Prompts = o.Prompts.With(notify.ChatPrompt{
					Backend: "chat", Label: "Chat",
					Address: notify.AddressRule{
						DirectKinds: []string{"D"},
						Follows:     notify.AddressingFollows(),
					},
				})
			})
			r := to(notify.Recipient{Handle: "engineering-lead"}, "can you look at this?")
			r.Inbound.Source, r.Inbound.EventType, r.Inbound.Sender = "chat", "message", tc.sender
			r.Inbound.Metadata = tc.meta
			if tc.kind != "" {
				r.Inbound.Metadata[notify.BodyAuthorKindField] = string(tc.kind)
			}
			h.parser.out = []notify.Routed{r}
			if got := h.svc.Handle(t.Context(), delivery("tracker")); got.Outcome != queue.OutcomeAck {
				t.Fatalf("Handle = %+v, want an ack", got)
			}
			woken := h.inbox(t, "engineering-lead")
			if len(woken) != 1 {
				t.Fatalf("the seat was woken %d times, want once", len(woken))
			}
			if got := queue.Prioritized(woken[0]); got != tc.want {
				t.Fatalf("the wake rides the priority lane = %v, want %v", got, tc.want)
			}
		})
	}
}

// A FIRST-PARTY NOTICE'S STATED WRITER DECIDES WHETHER A PERSON WROTE IT.
//
// Gitlet's wakes state who wrote a review comment or a check's verdict
// on Inbound.Writer and nowhere else. Read past it, the sender is a seat's
// handle in a namespace the registry handed here does not place, which the
// rule calls somebody outside the company and so a person: an agent's remark
// on a change jumped its colleague's lane. Each case names a sender nobody
// holds, so only the stated kind can turn the answer.
func TestAFirstPartyNoticesStatedWriterDecidesWhetherAPersonWroteIt(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name   string
		writer *types.NotificationWriter
		meta   map[string]string
		want   bool
	}{{
		name:   "a seat's review comment",
		writer: notify.FirstPartyWriter(iam.ActorAgent, "backend-engineer", nil),
		want:   false,
	}, {
		name:   "the engine's own record",
		writer: notify.FirstPartyWriter(iam.ActorSystem, "", nil),
		want:   false,
	}, {
		name:   "a person's review comment",
		writer: notify.FirstPartyWriter(iam.ActorHuman, "dana-founder", nil),
		want:   true,
	}, {
		name:   "an operator credential's write",
		writer: notify.FirstPartyWriter(iam.ActorOperator, "ops-bot", nil),
		want:   true,
	}, {
		name:   "the stated writer outranks the metadata",
		writer: notify.FirstPartyWriter(iam.ActorAgent, "backend-engineer", nil),
		meta:   map[string]string{notify.BodyAuthorKindField: string(iam.ActorHuman)},
		want:   false,
	}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			n := notify.Inbound{Source: "code", Sender: "nobody-holds-this", Writer: tc.writer, Metadata: tc.meta}
			if got := notify.WrittenByPerson(nil, n); got != tc.want {
				t.Fatalf("WrittenByPerson = %v, want %v", got, tc.want)
			}
		})
	}
}

// A PERSON'S MESSAGE ON THE PRIORITY LANE IS ANNOUNCED ON ITS SEAT'S WAITING
// CHANNEL, with the wake that carries it and the conversation's keys and
// nothing else; a background wake is not announced at all.
//
// The announcement is how the node running the seat learns a person is
// waiting, so it can show them their message is queued behind what the seat
// is doing. It names no person and carries no words: the message itself
// travels on the seat's durable inbox.
func TestAPersonsWaitingMessageIsAnnouncedToItsSeat(t *testing.T) {
	t.Parallel()
	h := newService(t, func(o *notify.Options, _ *harness) {
		o.Prompts = o.Prompts.With(notify.ChatPrompt{
			Backend: "chat", Label: "Chat",
			Address: notify.AddressRule{DirectKinds: []string{"D"}, Follows: notify.AddressingFollows()},
		})
	})
	var (
		mu    sync.Mutex
		heard []notify.WaitingNotice
	)
	stop, err := h.q.Listen(t.Context(), topics.SeatWaitingAll, func(subject string, payload []byte) {
		id, _ := org.DeriveAgentID(company().Name, "engineering-lead")
		if subject != topics.SeatWaiting(id) {
			t.Errorf("announced on %q, want the seat's own waiting channel", subject)
		}
		n, err := notify.DecodeWaitingNotice(payload)
		if err != nil {
			t.Errorf("decode the notice: %v", err)
		}
		mu.Lock()
		heard = append(heard, n)
		mu.Unlock()
	})
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = stop(t.Context()) })

	send := func(kind iam.ActorKind, ts string) {
		r := to(notify.Recipient{Handle: "engineering-lead"}, "can you look at this?")
		r.Inbound.Source, r.Inbound.EventType, r.Inbound.Sender = "chat", "message", "dana-founder"
		r.Inbound.Metadata = map[string]string{
			"channel": "D1", "channel_type": "D", "ts": ts, "transport": "chat",
			notify.BodyAuthorKindField: string(kind),
		}
		h.parser.out = []notify.Routed{r}
		if got := h.svc.Handle(t.Context(), delivery("tracker")); got.Outcome != queue.OutcomeAck {
			t.Fatalf("Handle = %+v, want an ack", got)
		}
	}
	send(iam.ActorHuman, "p1")
	send(iam.ActorAgent, "p2")

	woken := h.settledN(t, inbox("engineering-lead"), 2)
	deadline := time.Now().Add(2 * time.Second)
	for {
		mu.Lock()
		n := len(heard)
		mu.Unlock()
		if n >= 1 || time.Now().After(deadline) {
			break
		}
		time.Sleep(time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(heard) != 1 {
		t.Fatalf("%d notices, want the person's message alone", len(heard))
	}
	if heard[0].Wake != woken[0].ID.String() {
		t.Errorf("the notice names wake %s, want the person's %s", heard[0].Wake, woken[0].ID)
	}
	want := map[string]string{"channel": "D1", "channel_type": "D", "ts": "p1", "transport": "chat"}
	if len(heard[0].Metadata) != len(want) {
		t.Errorf("the notice carries %v, want the conversation's keys alone", heard[0].Metadata)
	}
	for k, v := range want {
		if heard[0].Metadata[k] != v {
			t.Errorf("the notice's %s is %q, want %q", k, heard[0].Metadata[k], v)
		}
	}
}
