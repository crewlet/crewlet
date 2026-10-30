package engine

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/slack"
)

// A REPLY'S WORDS ARE ITS WRITERS', EACH UNDER THEIR OWN NAME WHERE THERE ARE
// SEVERAL. One writer's message is recorded exactly as it was typed, never the
// seat's prompt around it; a delivery holding two people's messages puts each
// under the name its writer posted as, since the answer is recorded as the
// newest writer's and must not carry the other's words under that name; and a
// coalesced burst, whose salient text already names each sender, is not named
// twice.
//
// Mutation: drop the writer's name and the two-writer case is red; name every
// line and the one-writer case is.
func TestAReplysWordsAreItsWritersEachUnderTheirOwnName(t *testing.T) {
	t.Parallel()
	message := func(sender, body string) *events.Event {
		salient := body
		return events.New(types.ExternalNotification{
			NotificationSource: "slack", SourceEventType: "message", Sender: sender,
			Subject: "Slack message", SubjectIsLabel: true,
			Body:        "A chat message was posted. **Message:** " + body + "\n## How to handle this",
			SalientBody: &salient,
		}, events.TraceContext{})
	}
	burst := func() *events.Event {
		salient := "ana: Use Postgres\n\nbo: agreed"
		return events.New(types.ExternalNotification{
			NotificationSource: "slack", SourceEventType: "message", Sender: "bo",
			Subject: "Slack message", SubjectIsLabel: true, Body: "2 messages",
			SalientBody: &salient,
			Messages: []types.CoalescedMessage{{Sender: "ana", Body: "Use Postgres"},
				{Sender: "bo", Body: "agreed"}},
		}, events.TraceContext{})
	}
	for name, tc := range map[string]struct {
		evs  []*events.Event
		want string
	}{
		"one writer's message, as typed": {
			evs:  []*events.Event{message("ana", "Use Postgres")},
			want: "Use Postgres",
		},
		"one writer's two messages": {
			evs:  []*events.Event{message("ana", "Use Postgres"), message("ana", "the managed one")},
			want: "Use Postgres\n\nthe managed one",
		},
		"two writers, each under their name": {
			evs:  []*events.Event{message("ana", "Use Postgres"), message("bo", "agreed")},
			want: "ana: Use Postgres\n\nbo: agreed",
		},
		"a coalesced burst, named once": {
			evs:  []*events.Event{burst()},
			want: "ana: Use Postgres\n\nbo: agreed",
		},
	} {
		if got := replyWords(tc.evs); got != tc.want {
			t.Errorf("%s: the reply records %q, want %q", name, got, tc.want)
		}
	}
}

// A REPLY IS LABELLED AS ITS TRIGGER LABELS THE MESSAGE, for the seat it was
// delivered to: read in a chat thread, every writer named once, at the least
// trust any of them earns, so a stranger's message in a burst of a
// colleague's makes the whole reply data.
//
// Mutation: take the newest message's trust alone and the stranger's case is
// red; name the writers by the newest message alone and it is too.
func TestAReplyIsLabelledAsItsTriggerLabelsTheMessage(t *testing.T) {
	t.Parallel()
	o := &org.Organization{Name: "Acme", Roles: []*org.Role{
		{Name: "SWE", DeclaredHandle: "swe"},
		{Name: "Ana Lima", DeclaredHandle: "ana", Kind: org.KindHuman},
	}}
	o.Normalize()
	reg := notify.NewRegistry(o, nil)
	if err := reg.Register(slack.Backend, "U0ANA", "ana"); err != nil {
		t.Fatal(err)
	}
	prompts := notify.NewPrompts(slack.Prompt())
	said := func(source, user, body string) *events.Event {
		salient := body
		return events.New(types.ExternalNotification{
			NotificationSource: source, SourceEventType: "message", Sender: user,
			Body: body, SalientBody: &salient,
			Metadata: map[string]string{notify.UserField: user, notify.ActorField: user,
				notify.RecipientField: "swe"},
		}, events.TraceContext{})
	}
	ana := provenance.Author{Kind: iam.ActorHuman, Seat: "ana"}
	for name, tc := range map[string]struct {
		evs  []*events.Event
		want provenance.Label
	}{
		"a colleague's reply": {
			evs: []*events.Event{said(slack.Backend, "U0ANA", "Use Postgres")},
			want: provenance.Label{Source: provenance.SourceChatThread,
				Trust: provenance.TrustColleague, Authors: []provenance.Author{ana}},
		},
		"a stranger's, then a colleague's": {
			evs: []*events.Event{said(slack.Backend, "U0STRANGER", "no, use mine"),
				said(slack.Backend, "U0ANA", "Use Postgres")},
			want: provenance.Label{Source: provenance.SourceChatThread,
				Trust: provenance.TrustExternal, Authors: []provenance.Author{{}, ana}},
		},
	} {
		got := replyLabel(prompts, reg, "swe", tc.evs)
		if got == nil || got.Source != tc.want.Source || got.Trust != tc.want.Trust ||
			!slices.Equal(got.Authors, tc.want.Authors) {
			t.Errorf("%s: the reply is labelled %+v; want %+v", name, got, tc.want)
		}
	}
}
