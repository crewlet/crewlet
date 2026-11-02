package notify_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/queue"
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
