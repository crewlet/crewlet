package slack_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/slack"
)

func trigger(extra map[string]string) notify.Inbound {
	meta := map[string]string{
		"transport": slack.Backend, "channel": "C0ENG", "channel_type": "channel",
		"ts": "1700000001.000100", "bot_user_id": botUser, "user": human,
	}
	for k, v := range extra {
		meta[k] = v
	}
	return notify.Inbound{
		Source: slack.Backend, EventType: "message",
		Subject: "Slack message", Body: "how is the fix going", Metadata: meta,
	}
}

// THE ONE THING AN AGENT GETS WRONG HERE MORE THAN ANYWHERE ELSE: Slack
// shows `@ana` and delivers `<@U…>`, so an agent that writes what it sees
// posts literal text that renders as itself and notifies nobody — silently,
// because the message looks right in the transcript.
func TestThePromptSaysHowToWriteAMention(t *testing.T) {
	t.Parallel()
	got := slack.Prompt().Build(trigger(nil), nil)
	if !strings.Contains(got, "<@USER_ID>") {
		t.Fatalf("the prompt does not say how to write a mention:\n%s", got)
	}
	if !strings.Contains(got, "notifies nobody") {
		t.Errorf("the prompt does not say what writing @name costs:\n%s", got)
	}
}

// THE AGENT IS TOLD ITS OWN ID, which is what lets it recognise its own
// replies when it reads a thread back — and, because Slack's mention markup
// is that same id, tell being named from a colleague being named.
func TestThePromptNamesTheAgentsOwnID(t *testing.T) {
	t.Parallel()
	got := slack.Prompt().Build(trigger(nil), nil)
	if !strings.Contains(got, botUser) {
		t.Fatalf("the prompt never names the agent's own id:\n%s", got)
	}
	// A seat whose identity has not resolved yet says nothing rather than
	// naming an empty id.
	bare := slack.Prompt().Build(trigger(map[string]string{"bot_user_id": ""}), nil)
	if strings.Contains(bare, "Your Slack user id is ``") {
		t.Errorf("an unresolved identity was rendered:\n%s", bare)
	}
}

// A THREAD REPLY IS A POINTER: the triggering message is usually thin —
// "yes", "+1" — and the thread is the context.
func TestAThreadReplyRequiresRecon(t *testing.T) {
	t.Parallel()
	p := slack.Prompt()
	if !p.RequiresRecon(trigger(map[string]string{"thread_ts": "1700000000.000000"})) {
		t.Error("a thread reply was not treated as a pointer")
	}
	if p.RequiresRecon(trigger(nil)) {
		t.Error("a top-level message was sent to fetch a thread it has no id for")
	}
}

// THE PROMPT ANSWERS FOR ITS OWN SOURCE, and the spine dispatches to it.
func TestThePromptIsRegisteredForItsSource(t *testing.T) {
	t.Parallel()
	if got := slack.Prompt().Source(); got != slack.Backend {
		t.Fatalf("source = %q", got)
	}
	prompts := notify.NewPrompts(slack.Prompt())
	if !strings.Contains(prompts.For(slack.Backend).Build(trigger(nil), nil), "<@USER_ID>") {
		t.Error("the spine did not dispatch to the Slack prompt")
	}
}

// A CHAT BACKEND KEEPS EVERY CONSTITUENT OF A DIGEST: a person typing four
// times has said four things, where a tracker re-emits one state four times.
func TestEveryMessageSurvivesADigest(t *testing.T) {
	t.Parallel()
	if got := slack.Prompt().DigestBody("message", "and another thing"); got != "and another thing" {
		t.Fatalf("digest body = %q", got)
	}
}
