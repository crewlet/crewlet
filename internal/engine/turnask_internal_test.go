package engine

import (
	"reflect"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/execstate"
	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/sandbox"
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
			// A chat subject names the SURFACE, the same on every
			// message: leading the ask with it handed every relevance
			// judgement on the surface the same words.
			name: "a chat message is what was said, without the surface's name",
			event: events.New(types.ExternalNotification{
				NotificationSource: "slack", Sender: "U0FOUNDER",
				Subject: "Slack message", SubjectIsLabel: true, Body: scaffold, SalientBody: &salient,
			}, events.TraceContext{}),
			want:    []string{salient},
			wantNot: []string{"Slack message", "## Triage"},
		},
		{
			// A subject NOT marked a label is content and is kept, even
			// one that reads like a surface's name: only the stamp
			// decides, never the words.
			name: "a subject not marked a label is kept",
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
			// one to fall back to the scaffolding for — and with the
			// surface's name left out it is an EMPTY ask, which the
			// relevance passes are gated on.
			name: "an empty salient body does not fall back to the wrapping",
			event: events.New(types.ExternalNotification{
				NotificationSource: "slack", Subject: "Slack message", SubjectIsLabel: true,
				Body: scaffold, SalientBody: ptr(""),
			}, events.TraceContext{}),
			wantNot: []string{"## Triage", salient, "Slack message"},
		},
		{
			name: "a schedule's fire is its task without the fire's id",
			event: events.New(types.TaskAssigned{
				TaskID:   "unit:Engineering:weekly-report:2026-09-28T09:00:00Z:swe",
				Schedule: "weekly-report", Description: "Summarise the week's merged PRs.",
			}, events.TraceContext{}),
			want:    []string{"weekly-report", "Summarise the week's merged PRs."},
			wantNot: []string{"2026-09-28T09:00:00Z"},
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

// WHAT A TURN WAS ASKED REACHES ITS COMPLETED-TURN RECORD where nothing else
// carries it. A colleague's question woke a turn with no interactions, so the
// record — and the episode the reflection worker writes from it — knew the
// turn only as "lead asked a colleague on a2a-1".
func TestAColleaguesQuestionIsCarriedOnTheTurnsRecord(t *testing.T) {
	t.Parallel()
	_, _, done := turnRecords(t, events.New(types.A2ARequest{
		ChannelID: "a2a-1", Requester: "lead", SenderRole: "Lead",
		Content: "which test is flaky?",
	}, events.TraceContext{}))
	if !strings.Contains(done.Ask, "which test is flaky?") {
		t.Fatalf("ask = %q, want the colleague's question", done.Ask)
	}
	// AND WHOSE WORDS THEY ARE (ADR-0040): the record a learning worker reads
	// after the turn says the question is the asking colleague's, as the
	// trigger fenced it.
	want := provenance.LabelFor("", provenance.SourceA2AMessage,
		[]provenance.Author{{Kind: iam.ActorAgent, Seat: "lead"}})
	if done.AskProvenance == nil || !reflect.DeepEqual(*done.AskProvenance, want) {
		t.Fatalf("the ask is recorded under %+v, want %+v", done.AskProvenance, want)
	}
}

// AND NOT REPEATED where the interactions carry it: a notification's ask is
// what its senders said, which the record already holds once.
func TestANotificationsAskIsNotCarriedTwice(t *testing.T) {
	t.Parallel()
	_, _, done := turnRecords(t, chatTrigger("D0ANA"))
	if len(done.Interactions) == 0 {
		t.Fatal("the chat wake recorded no interaction, so this asserts nothing")
	}
	if done.Ask != "" || done.AskProvenance != nil {
		t.Fatalf("ask = %q under %+v beside %d interactions carrying it", done.Ask,
			done.AskProvenance, len(done.Interactions))
	}
	// EACH INTERACTION SAYS WHO WROTE ITS BODY, as the trigger fenced it: a
	// message from somebody the company does not know, which nothing vouches
	// for — never the zero label, which says nothing at all.
	for _, in := range done.Interactions {
		if !in.Provenance.Valid() || in.Provenance.Trust != provenance.TrustExternal {
			t.Errorf("an interaction's body is recorded under %+v, want a stranger's message",
				in.Provenance)
		}
	}
}

// A RESUMED SEGMENT RECORDS THE ASK ITS TURN PARKED WITH — never one read off
// the completion or the reply that resumed it, which is not what the turn was
// asked.
func TestAResumedTurnRecordsTheAskItParkedWith(t *testing.T) {
	t.Parallel()
	parked := provenance.Label{Source: provenance.SourceWorkComment, Trust: provenance.TrustExternal}
	e, p := starting(t, refusingModels(t))
	company := e.Company()
	if err := e.resumeTurn(t.Context(), resumeInput{
		Company: company,
		Run: sandbox.PendingRun{
			TurnID: "run-1", AgentHandle: "swe", Reply: "tool",
			TaskDescription: "fix the failing test", DelegationDepth: 3,
		},
		State: execstate.State{Ask: "the login test fails on main — can you fix it?",
			AskProvenance: &parked},
		Turn: &turnctx.Turn{RunID: "run-1", Seat: company.Org.AgentSeatByHandle("swe"),
			Org: company.Org},
		Answer:  "use the main branch",
		Trigger: chatTrigger("D0ANA"),
	}); err != nil {
		t.Fatalf("resumeTurn: %v", err)
	}
	done := only[*types.TurnCompleted](t, p, "turn_completed")
	if done.Ask != "the login test fails on main — can you fix it?" {
		t.Fatalf("ask = %q, want the one the turn parked with", done.Ask)
	}
	// UNDER THE LABEL IT PARKED WITH: nothing the resumed segment reads
	// could decide who wrote it again.
	if done.AskProvenance == nil || !reflect.DeepEqual(*done.AskProvenance, parked) {
		t.Fatalf("the ask is recorded under %+v, want the parked %+v", done.AskProvenance, parked)
	}
}
