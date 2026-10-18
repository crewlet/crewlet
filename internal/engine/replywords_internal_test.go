package engine

import (
	"testing"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
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
