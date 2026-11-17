package engine_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/inbox"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
)

// A turn's ask is the string every phase, prefetch and model round is built on,
// so these tests assert it CONTAINS the wake's real content, not an exact
// rendering: a brief's wording may change, the ask's presence may not.

func TestEveryWakeTypeHandsTheTurnItsAsk(t *testing.T) {
	t.Parallel()

	body := "Roll back service X before 5pm — runbook step 4 is wrong."
	cases := []struct {
		name  string
		event *events.Event
		want  []string
	}{
		{
			name: "an external notification carries its body, not its subject line",
			event: events.New(types.ExternalNotification{
				NotificationSource: "slack", SourceEventType: "message",
				Sender: "U0FOUNDER", Subject: "deploy", Body: body,
			}, events.TraceContext{}),
			want: []string{body},
		},
		{
			name: "an A2A ask carries the colleague's question",
			event: events.New(types.A2ARequest{
				ChannelID: "ch-1", Requester: "cto", SenderRole: "CTO",
				Content: "Is runbook step 4 wrong?",
			}, events.TraceContext{}),
			want: []string{"Is runbook step 4 wrong?", "CTO"},
		},
		{
			// Both halves: the channel is closed when the answer lands,
			// so without the question the reply has no antecedent.
			name: "an A2A answer carries the question it answers",
			event: events.New(types.A2AMessage{
				ChannelID: "ch-1", Sender: "sre", SenderRole: "SRE",
				Question: "Is runbook step 4 wrong?", Content: "Yes — it names the old cluster.",
			}, events.TraceContext{}),
			want: []string{"Is runbook step 4 wrong?", "Yes — it names the old cluster."},
		},
		{
			name: "a scheduled fire carries the schedule's task text",
			event: events.New(types.TaskAssigned{
				TaskID: "run-9", RoleName: "Engineer", Schedule: "weekly-report",
				Description: "Summarise the week's merged PRs and post to #eng.",
			}, events.TraceContext{}),
			want: []string{"Summarise the week's merged PRs and post to #eng.", "weekly-report"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := engine.DescribeTrigger([]*events.Event{tc.event}, "")
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("the turn's ask is missing %q\ngot: %q", want, got)
				}
			}
		})
	}
}

// Every type that runs a turn must state its ask: one without a Brief() wakes
// its seats with a type name. The assertion hangs off the ledger because adding
// a type there is the step nobody forgets.
func TestEveryLedgeredTriggerTypeStatesItsAsk(t *testing.T) {
	t.Parallel()
	for _, name := range inbox.LedgeredTypes() {
		payload, ok := events.PayloadFor(name)
		if !ok {
			t.Errorf("%s runs a turn but is not a registered payload type, so its "+
				"ask cannot travel typed", name)
			continue
		}
		if _, ok := payload.(events.Briefer); !ok {
			t.Errorf("%s runs a turn but does not implement events.Briefer, so a seat "+
				"woken by it is handed its type name instead of the ask", name)
		}
	}
}

// A coalesced partition is one conversation, so every constituent's ask must
// survive the merge: keeping only the first hands the seat a thread it cannot
// answer.
func TestACoalescedPartitionKeepsEveryConstituentsAsk(t *testing.T) {
	t.Parallel()
	first := events.New(types.ExternalNotification{
		NotificationSource: "slack", Sender: "alice", Subject: "deploy",
		Body: "Can you take the deploy today?",
	}, events.TraceContext{})
	second := events.New(types.ExternalNotification{
		NotificationSource: "slack", Sender: "bob", Subject: "deploy",
		Body: "Blocked on the migration — see PROJ-4.",
	}, events.TraceContext{})

	got := engine.DescribeTrigger([]*events.Event{first, second}, "")
	for _, want := range []string{"Can you take the deploy today?", "Blocked on the migration — see PROJ-4."} {
		if !strings.Contains(got, want) {
			t.Errorf("the coalesced ask lost %q\ngot: %q", want, got)
		}
	}
}

// A payload that states no ask still has to produce one: a blank task is a task
// a model answers by inventing one.
func TestATriggerWithNoAskStillNamesItself(t *testing.T) {
	t.Parallel()
	ev := events.New(types.TaskAssigned{RoleName: "Engineer"}, events.TraceContext{})
	if got := engine.DescribeTrigger([]*events.Event{ev}, ""); strings.TrimSpace(got) == "" {
		t.Error("a trigger with no readable ask produced a blank task")
	}
}
