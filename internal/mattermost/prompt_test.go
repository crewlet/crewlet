package mattermost_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/mattermost"
	"github.com/crewlet/crewlet/internal/notify"
)

func note(mutate func(map[string]string)) notify.Inbound {
	m := map[string]string{
		"transport": "mattermost", "channel": "C1", "ts": "p1",
		"channel_type": "O", "user": "u-ana",
		"bot_username": "agent-swe", "bot_user_id": "bot-1",
		notify.RecipientField: "swe",
	}
	if mutate != nil {
		mutate(m)
	}
	return notify.Inbound{
		Source: mattermost.Backend, EventType: "posted", Sender: "ana",
		Body: "can you look", Metadata: m,
	}
}

// Both halves of the identity matter and they are not interchangeable: the
// ID is how a payload names this seat, which is what lets the agent
// recognise its own posts when it reads a thread back; the USERNAME is how a
// person addresses it.
func TestThePromptCarriesBothHalvesOfTheIdentity(t *testing.T) {
	got := mattermost.Prompt().Build(note(nil), nil)

	// The TRIAGE marker is the username, because that is what a person
	// types and therefore what the agent will see written about itself.
	// The id never appears in a message body on this backend, so a marker
	// built from it names something the agent will never encounter.
	if !strings.Contains(got, "names you — `@agent-swe` or `swe`,") {
		t.Errorf("the triage marker is not the username:\n%s", got)
	}
	if !strings.Contains(got, "`bot-1`") {
		t.Errorf("the prompt does not name the agent's own id:\n%s", got)
	}
	if !strings.Contains(got, "posts from that id in a thread are your own") {
		t.Errorf("the prompt does not say what the id is for:\n%s", got)
	}
	// Mattermost stores mentions as typed, so a model reaching for another
	// backend's markup notifies nobody.
	if !strings.Contains(got, "write them literally as `@username`") {
		t.Errorf("the prompt does not say how to write a mention:\n%s", got)
	}
	// All three collectives, since @all is this backend's own.
	if !strings.Contains(got, "`@all` / `@channel` / `@here`") {
		t.Errorf("the collectives are wrong:\n%s", got)
	}
}

// Either half can be missing — a seat whose id has not resolved, or a bot
// registered by a live apply — and the prompt must still read.
func TestAPartialIdentityStillReads(t *testing.T) {
	p := mattermost.Prompt()

	nameOnly := p.Build(note(func(m map[string]string) { delete(m, "bot_user_id") }), nil)
	if !strings.Contains(nameOnly, "Your Mattermost account is `@agent-swe`.") {
		t.Errorf("a name-only identity rendered:\n%s", nameOnly)
	}
	if strings.Contains(nameOnly, "user id ``") {
		t.Errorf("an empty id rendered as empty markup:\n%s", nameOnly)
	}

	idOnly := p.Build(note(func(m map[string]string) { delete(m, "bot_username") }), nil)
	if !strings.Contains(idOnly, "Your Mattermost user id is `bot-1`") {
		t.Errorf("an id-only identity rendered:\n%s", idOnly)
	}
	// With no username there is no mention to teach, so the hint is
	// omitted rather than rendered pointing at nothing.
	if strings.Contains(idOnly, "**Mentions:**") {
		t.Errorf("a mention hint appeared with no username:\n%s", idOnly)
	}

	neither := p.Build(note(func(m map[string]string) {
		delete(m, "bot_username")
		delete(m, "bot_user_id")
	}), nil)
	if strings.Contains(neither, "Your Mattermost") {
		t.Errorf("an unresolved identity claimed one:\n%s", neither)
	}
	if !strings.Contains(neither, "Your handle is `swe`") {
		t.Errorf("the handle went missing:\n%s", neither)
	}
}

// Mattermost ids are 26 opaque lowercase alphanumerics, so a prefix test
// would mark arbitrary public channels as direct messages and raise
// indicators for traffic nobody addressed to this agent.
func TestThisBackendUsesNoChannelIDPrefix(t *testing.T) {
	p := mattermost.Prompt()
	if p.DMPrefix != "" {
		t.Fatalf("a channel-id prefix is configured: %q", p.DMPrefix)
	}
	// A public channel whose id happens to start with D is still public.
	if p.IsDirect(map[string]string{"channel": "dxxxxxxxxxxxxxxxxxxxxxxxxx", "channel_type": "O"}) {
		t.Fatal("a public channel was read as a direct message")
	}
	for _, kind := range mattermost.DirectKinds {
		if !p.IsDirect(map[string]string{"channel_type": kind}) {
			t.Errorf("%q is not read as direct", kind)
		}
	}
}

func TestThePromptAnswersForThisBackend(t *testing.T) {
	p := mattermost.Prompt()
	if p.Source() != mattermost.Backend {
		t.Fatalf("Source = %q", p.Source())
	}
	var _ notify.Prompt = p

	// And the registry hands it back for this source rather than the
	// generic fallback.
	if got := notify.NewPrompts(p).For(mattermost.Backend); got.Source() != mattermost.Backend {
		t.Fatalf("the registry answered with %q", got.Source())
	}
}
