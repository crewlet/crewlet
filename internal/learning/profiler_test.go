package learning_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

func profiler(t *testing.T, p llm.Provider, c *learning.Counterparties) *learning.Profiler {
	t.Helper()
	w, err := learning.NewProfiler(&stubModels{p: p}, c, learning.ProfilerOptions{})
	if err != nil {
		t.Fatalf("NewProfiler: %v", err)
	}
	return w
}

// said builds one interaction from a named sender.
func said(handle, external, name, body string) types.InboundInteraction {
	return types.InboundInteraction{
		Sender: types.CanonicalIdentity{
			Handle: handle, ExternalID: external,
			Platform: "mattermost", DisplayName: name,
		},
		Body: body, ChannelKind: types.ChannelPublic,
	}
}

// cpTurn is a turn woken by one identifiable person.
func cpTurn(interactions ...types.InboundInteraction) learning.Turn {
	if len(interactions) == 0 {
		interactions = []types.InboundInteraction{
			said("", "u-sam", "Sam", "can you send it as bullets please"),
		}
	}
	return learning.Turn{
		Role: &org.Role{Name: "Dev"},
		Event: types.TurnCompleted{
			Agent: "agent-uuid", AgentHandle: "dev", RoleName: "Dev",
			TurnID: "work-1", TaskSummary: "reply to Sam",
			ToolSequence: []string{"reply"}, ReviewOutcome: "done",
			Interactions: interactions,
		},
	}
}

func TestATraitsPatchIsMergedIntoTheProfile(t *testing.T) {
	t.Parallel()
	c := counterparties(t)
	w := profiler(t, says(`{"reply_style":"bullet points","timezone":"Europe/Berlin"}`), c)
	if _, err := w.Reflect(context.Background(), cpTurn()); err != nil {
		t.Fatalf("Reflect: %v", err)
	}

	got, ok, err := c.Get(context.Background(), "dev",
		learning.Subject{ExternalID: "u-sam", Platform: "mattermost", Name: "Sam"})
	if err != nil || !ok {
		t.Fatalf("Get: %v (found=%v)", err, ok)
	}
	if got.Traits["reply_style"] != "bullet points" {
		t.Errorf("traits = %v", got.Traits)
	}
	if got.InteractionCount != 1 {
		t.Errorf("interaction count = %d, want 1", got.InteractionCount)
	}
}

// AN EMPTY PATCH IS STILL AN OBSERVATION. The store separates "seen" from
// "learned something", and the prefetch demotes on the difference — so a
// counterparty seen daily whose traits have settled must still be counted.
func TestNothingNewStillCountsTheInteraction(t *testing.T) {
	t.Parallel()
	c := counterparties(t)
	if _, err := profiler(t, says(`{}`), c).Reflect(context.Background(), cpTurn()); err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	got, ok, _ := c.Get(context.Background(), "dev",
		learning.Subject{ExternalID: "u-sam", Platform: "mattermost", Name: "Sam"})
	if !ok || got.InteractionCount != 1 {
		t.Fatalf("profile = %+v (found=%v), want one counted interaction", got, ok)
	}
	if len(got.Traits) != 0 {
		t.Errorf("traits = %v, want none", got.Traits)
	}
}

// A FAILED CALL RECORDS NOTHING. Counting an interaction on the strength of
// a call that never happened makes an unreachable model look exactly like a
// counterparty whose traits have settled.
func TestAnUnreachableModelRecordsNothing(t *testing.T) {
	t.Parallel()
	c := counterparties(t)
	w := profiler(t, &auxProvider{err: errors.New("provider is down")}, c)
	if _, err := w.Reflect(context.Background(), cpTurn()); err == nil {
		t.Fatal("an unreachable model was reported as a successful pass")
	}
	if _, ok, _ := c.Get(context.Background(), "dev",
		learning.Subject{ExternalID: "u-sam", Platform: "mattermost", Name: "Sam"}); ok {
		t.Error("a profile was written from a call that never happened")
	}
}

// EVERY DISTINCT SENDER of a coalesced trigger is observed: a turn woken by
// three people is not a turn about the last of them.
func TestEverySenderOfACoalescedTriggerIsProfiled(t *testing.T) {
	t.Parallel()
	c := counterparties(t)
	w := profiler(t, says(`{"a":"1"}`, `{"b":"2"}`, `{"c":"3"}`), c)
	payloads, err := w.Reflect(context.Background(), cpTurn(
		said("", "u-sam", "Sam", "one"),
		said("", "u-ash", "Ash", "two"),
		said("", "u-kim", "Kim", "three"),
	))
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if len(payloads) != 3 {
		t.Fatalf("announced %d profiles, want one per sender", len(payloads))
	}
	seen := map[string]bool{}
	for _, p := range payloads {
		seen[p.(types.CounterpartyProfileUpdated).SubjectExternalID] = true
	}
	for _, id := range []string{"u-sam", "u-ash", "u-kim"} {
		if !seen[id] {
			t.Errorf("%s was never profiled", id)
		}
	}
}

// TWO MESSAGES FROM ONE PERSON ARE ONE CONVERSATION. Profiling them
// separately spends two calls on one question, and the second observation
// carries the same work key so its counter move is dropped anyway.
func TestOneSenderSpeakingTwiceIsOneObservation(t *testing.T) {
	t.Parallel()
	p := says(`{"reply_style":"short"}`)
	w := profiler(t, p, counterparties(t))
	payloads, err := w.Reflect(context.Background(), cpTurn(
		said("", "u-sam", "Sam", "first"),
		said("", "u-sam", "Sam", "second"),
	))
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if len(payloads) != 1 {
		t.Fatalf("announced %d profiles for one person, want 1", len(payloads))
	}
	if got := p.count(); got != 1 {
		t.Fatalf("made %d model calls for one person, want 1", got)
	}
	if body := p.prompt(t, 0); !strings.Contains(body, "first") || !strings.Contains(body, "second") {
		t.Errorf("the prompt dropped a message: %q", body)
	}
}

// A SEAT DOES NOT PROFILE ITSELF: its own echoed post reaches it as an
// interaction, and a self-profile is a bag of traits nothing ever reads.
func TestASeatDoesNotProfileItself(t *testing.T) {
	t.Parallel()
	w := profiler(t, says(`{}`), counterparties(t))
	turn := cpTurn(said("dev", "u-dev", "Dev", "here is the summary"))
	if got := w.Skip(turn); got != "no_counterparties" {
		t.Fatalf("skip = %q, want the seat's own echo to be no counterparty", got)
	}
}

func TestTheGatesThatKeepProfilesHonest(t *testing.T) {
	t.Parallel()
	w := profiler(t, says(`{}`), counterparties(t))
	for _, tc := range []struct {
		name, want string
		turn       learning.Turn
	}{
		{
			// A schedule, a sandbox completion, an A2A ask: nobody spoke.
			// Built by hand rather than through cpTurn, whose no-argument
			// form supplies a default sender — which is exactly the case
			// this is asserting the absence of.
			name: "an internal trigger", want: "no_counterparties",
			turn: internalTurn(),
		},
		{
			name: "a sender with no identity at all", want: "no_counterparties",
			turn: cpTurn(said("", "", "", "an engine-authored notice")),
		},
	} {
		if got := w.Skip(tc.turn); got != tc.want {
			t.Errorf("%s: skip = %q, want %q", tc.name, got, tc.want)
		}
	}

	turn := cpTurn()
	turn.Event.AgentHandle = ""
	if got := w.Skip(turn); got != "no_observer" {
		t.Errorf("a turn with no seat: skip = %q, want no_observer", got)
	}
}

// A SELF-ITERATE ROUND IS STILL OBSERVED, unlike for the persist decider:
// who spoke to this seat does not depend on whether the seat finished.
func TestAnUnsettledTurnIsStillObserved(t *testing.T) {
	t.Parallel()
	turn := cpTurn()
	turn.Event.ReviewOutcome = "self_iterate"
	if got := profiler(t, says(`{}`), counterparties(t)).Skip(turn); got != "" {
		t.Fatalf("skipped an unsettled turn: %s", got)
	}
}

// THE KNOWN TRAITS REACH THE PROMPT. Without them the model re-derives the
// same preference every turn, and the store then rewrites an identical value
// while the corroboration stamp moves — reporting a party as freshly learned
// about when nothing was learned.
func TestWhatIsAlreadyKnownIsPutInFrontOfTheModel(t *testing.T) {
	t.Parallel()
	c := counterparties(t)
	p := says(`{"timezone":"Europe/Berlin"}`, `{}`)
	w := profiler(t, p, c)
	if _, err := w.Reflect(context.Background(), cpTurn()); err != nil {
		t.Fatalf("first Reflect: %v", err)
	}
	turn := cpTurn()
	turn.Event.TurnID = "work-2"
	if _, err := w.Reflect(context.Background(), turn); err != nil {
		t.Fatalf("second Reflect: %v", err)
	}
	if body := p.prompt(t, 1); !strings.Contains(body, "Europe/Berlin") {
		t.Errorf("the second prompt did not carry the known traits: %q", body)
	}
}

// PROSE IS NOT A PATCH, and it must not fail the observation either: a model
// that stopped honouring the contract has still told us nothing new.
func TestAProseAnswerRecordsTheInteractionAndNoTraits(t *testing.T) {
	t.Parallel()
	c := counterparties(t)
	w := profiler(t, says("Sam seems to prefer short replies."), c)
	if _, err := w.Reflect(context.Background(), cpTurn()); err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	got, ok, _ := c.Get(context.Background(), "dev",
		learning.Subject{ExternalID: "u-sam", Platform: "mattermost", Name: "Sam"})
	if !ok || got.InteractionCount != 1 || len(got.Traits) != 0 {
		t.Fatalf("profile = %+v (found=%v)", got, ok)
	}
}

// NESTED VALUES ARE DROPPED. The store merges whatever it is given, so a map
// that slipped through would render as Go's default formatting of a map in
// the prefetch's block, permanently, on every turn that party appears in.
func TestOnlyFlatScalarTraitsAreKept(t *testing.T) {
	t.Parallel()
	c := counterparties(t)
	w := profiler(t, says(`{"reply_style":"short","prefs":{"a":1},"tags":["x"],`+
		`"blank":"  ","hours":9,"remote":true}`), c)
	if _, err := w.Reflect(context.Background(), cpTurn()); err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	got, _, _ := c.Get(context.Background(), "dev",
		learning.Subject{ExternalID: "u-sam", Platform: "mattermost", Name: "Sam"})
	for _, dropped := range []string{"prefs", "tags", "blank"} {
		if _, ok := got.Traits[dropped]; ok {
			t.Errorf("%q survived as %v", dropped, got.Traits[dropped])
		}
	}
	for _, kept := range []string{"reply_style", "hours", "remote"} {
		if _, ok := got.Traits[kept]; !ok {
			t.Errorf("%q was dropped", kept)
		}
	}
}

func TestAProfilerNeedsAModelAndAStore(t *testing.T) {
	t.Parallel()
	if _, err := learning.NewProfiler(nil, counterparties(t),
		learning.ProfilerOptions{}); err == nil {
		t.Error("a profiler with no models was accepted")
	}
	if _, err := learning.NewProfiler(&stubModels{p: says(`{}`)}, nil,
		learning.ProfilerOptions{}); err == nil {
		t.Error("a profiler with nowhere to write was accepted")
	}
}

// internalTurn is a turn nobody spoke to: a schedule, a sandbox completion,
// an agent-to-agent ask.
func internalTurn() learning.Turn {
	turn := cpTurn()
	turn.Event.Interactions = nil
	return turn
}

// A REDELIVERY MUST NOT COUNT TWICE. The counter is what "seen daily" is
// read off, and every backend may redeliver — so an observation carrying no
// work key inflates the cadence of exactly the counterparties a seat deals
// with most.
func TestARedeliveredTurnDoesNotCountTwice(t *testing.T) {
	t.Parallel()
	c := counterparties(t)
	w := profiler(t, says(`{"reply_style":"short"}`, `{"reply_style":"short"}`), c)
	subject := learning.Subject{ExternalID: "u-sam", Platform: "mattermost", Name: "Sam"}

	payloads, err := w.Reflect(context.Background(), cpTurn())
	if err != nil {
		t.Fatalf("first Reflect: %v", err)
	}
	if len(payloads) != 1 {
		t.Fatalf("the first pass announced %d profiles, want 1", len(payloads))
	}

	// The same turn again — a redelivery, or a peer that raced it.
	again, err := w.Reflect(context.Background(), cpTurn())
	if err != nil {
		t.Fatalf("redelivered Reflect: %v", err)
	}
	if len(again) != 0 {
		t.Errorf("the redelivery announced %d profiles; nothing was counted "+
			"to announce", len(again))
	}
	profile, ok, _ := c.Get(context.Background(), "dev", subject)
	if !ok {
		t.Fatal("the profile is gone")
	}
	if profile.InteractionCount != 1 {
		t.Fatalf("interaction count = %d after a redelivery, want 1",
			profile.InteractionCount)
	}
}

// A DIFFERENT TURN DOES count again — the guard is keyed on the unit of
// work, not on the pair, so a counterparty spoken to twice is seen twice.
func TestASecondTurnCountsAgain(t *testing.T) {
	t.Parallel()
	c := counterparties(t)
	w := profiler(t, says(`{}`, `{}`), c)
	if _, err := w.Reflect(context.Background(), cpTurn()); err != nil {
		t.Fatalf("first Reflect: %v", err)
	}
	second := cpTurn()
	second.Event.TurnID = "work-2"
	if _, err := w.Reflect(context.Background(), second); err != nil {
		t.Fatalf("second Reflect: %v", err)
	}
	profile, _, _ := c.Get(context.Background(), "dev",
		learning.Subject{ExternalID: "u-sam", Platform: "mattermost", Name: "Sam"})
	if profile.InteractionCount != 2 {
		t.Fatalf("interaction count = %d after two turns, want 2",
			profile.InteractionCount)
	}
}

// THE SUBJECT LIST IS BOUNDED. Each party costs an auxiliary call, so an
// unbounded list makes a heavily coalesced trigger — a busy channel merged
// over a batching window — spend a call per speaker while the seat's next
// turn waits behind it.
func TestOnlySoManyPartiesAreProfiledPerTurn(t *testing.T) {
	t.Parallel()
	p := says(`{}`)
	w := profiler(t, p, counterparties(t))

	var spoke []types.InboundInteraction
	for i := range 30 {
		id := fmt.Sprintf("u-%02d", i)
		spoke = append(spoke, said("", id, "Person "+id, "a line"))
	}
	payloads, err := w.Reflect(context.Background(), cpTurn(spoke...))
	if err != nil {
		t.Fatalf("Reflect: %v", err)
	}
	if len(payloads) >= len(spoke) {
		t.Fatalf("profiled %d of %d speakers — the bound did not apply",
			len(payloads), len(spoke))
	}
	if got := p.count(); got != len(payloads) {
		t.Fatalf("made %d calls for %d profiles", got, len(payloads))
	}
	// ORDER IS FIRST-SPOKEN, so the bound trims the people who
	// contributed least rather than an arbitrary subset.
	first := payloads[0].(types.CounterpartyProfileUpdated)
	if first.SubjectExternalID != "u-00" {
		t.Errorf("first profile = %q, want the first speaker", first.SubjectExternalID)
	}
}
