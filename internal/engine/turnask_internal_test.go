package engine

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
)

// WHAT A TURN WAS ASKED, read off its trigger — never the integration's
// wrapping, which DescribeTrigger hands the executor and no relevance
// judgement may read.
func TestTheTurnsAskIsWhatWasAskedNotTheWrapping(t *testing.T) {
	t.Parallel()
	salient := "Roll back service X before 5pm — runbook step 4 is wrong."
	scaffold := "## Triage — decide BEFORE replying\n\n**Message:** " + salient
	for _, tc := range []struct {
		name    string
		event   *events.Event
		want    []string
		wantNot []string
	}{
		{
			name: "a notification is its subject and its salient body",
			event: events.New(types.ExternalNotification{
				NotificationSource: "slack", Sender: "U0FOUNDER",
				Subject: "Slack message", Body: scaffold, SalientBody: &salient,
			}, events.TraceContext{}),
			want:    []string{"Slack message", salient},
			wantNot: []string{"## Triage"},
		},
		{
			// A tracker comment names its topic only in the subject: the
			// body is what somebody said about it.
			name: "a tracker comment keeps the item it is about",
			event: events.New(types.ExternalNotification{
				NotificationSource: "tracker", Subject: "ENG-42 comment: Fix the login redirect",
				Body: "**ENG-42** … Can you look today?", SalientBody: ptr("Can you look today?"),
			}, events.TraceContext{}),
			want:    []string{"ENG-42 comment: Fix the login redirect", "Can you look today?"},
			wantNot: []string{"**ENG-42**"},
		},
		{
			// An empty salient body is a message with nothing in it, not
			// one to fall back to the scaffolding for.
			name: "an empty salient body does not fall back to the wrapping",
			event: events.New(types.ExternalNotification{
				NotificationSource: "slack", Subject: "Slack message",
				Body: scaffold, SalientBody: ptr(""),
			}, events.TraceContext{}),
			want:    []string{"Slack message"},
			wantNot: []string{"## Triage", salient},
		},
		{
			name: "a schedule's fire is its task without the run id",
			event: events.New(types.TaskAssigned{
				TaskID: "9f2c1a7e-0d4b-4a59-9e1f-2b6c3d8e7a10", Schedule: "weekly-report",
				Description: "Summarise the week's merged PRs.",
			}, events.TraceContext{}),
			want:    []string{"weekly-report", "Summarise the week's merged PRs."},
			wantNot: []string{"9f2c1a7e"},
		},
		{
			name: "a colleague's question names who asked",
			event: events.New(types.A2ARequest{
				ChannelID: "ch-1", Requester: "cto", SenderRole: "CTO",
				Content: "Is runbook step 4 wrong?",
			}, events.TraceContext{}),
			want:    []string{"Is runbook step 4 wrong?", "CTO"},
			wantNot: []string{"ch-1"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := turnAsk([]*events.Event{tc.event})
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Errorf("the ask is missing %q\ngot: %q", want, got)
				}
			}
			for _, not := range tc.wantNot {
				if strings.Contains(got, not) {
					t.Errorf("the ask carries %q\ngot: %q", not, got)
				}
			}
		})
	}
}

// AN EVENT WITH NOTHING TO SAY CONTRIBUTES NOTHING — not the type name
// DescribeTrigger hands the executor so it is never given a blank ask, which
// here would be a word every such turn is judged against.
func TestAnEventWithNoAskContributesNothing(t *testing.T) {
	t.Parallel()
	bare := &events.Event{Type: types.A2ARequestType}
	if got := turnAsk([]*events.Event{bare, nil}); got != "" {
		t.Fatalf("ask = %q, want nothing", got)
	}
}

func ptr(s string) *string { return &s }

// THE TURN'S SENDERS, for a tool that re-runs the memory filter: every distinct
// identifiable sender in the order they first spoke — the set the turn-start
// filter was told — and nobody with nothing to be identified by.
func TestATurnsSendersAreWhoSpokeInOrderOnce(t *testing.T) {
	t.Parallel()
	ana := types.CanonicalIdentity{ExternalID: "U1", Platform: "slack", DisplayName: "Ana"}
	bo := types.CanonicalIdentity{Handle: "cto"}
	got := sendersSpoken([]types.InboundInteraction{
		{Sender: ana}, {Sender: bo}, {Sender: ana},
		{Sender: types.CanonicalIdentity{DisplayName: "nobody we can name"}},
		{Sender: types.CanonicalIdentity{ExternalID: "U9"}},
	})
	if len(got) != 2 || got[0] != ana || got[1] != bo {
		t.Fatalf("senders = %+v, want Ana then the CTO", got)
	}
}
