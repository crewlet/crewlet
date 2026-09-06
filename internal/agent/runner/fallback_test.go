package runner_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// A provider chain that falls through has to say so ON THE TURN.
//
// The event type, its category and its documentation all existed while nothing
// wired [chain.Options.OnFallback], so no code path could produce one: every
// screen read a company whose providers never failed. These cases hold the
// wiring in place, and they hold the ADDRESSING in place too — `turn_id` is
// the promoted column the turn lookup selects on, so a hand-off published
// without one is invisible to the screen built to show a turn end to end.

// benched is a member that fails the way a spent key does — retryably, so the
// chain moves on rather than returning the error to the caller.
type benched struct{ model string }

func (b benched) Model() string { return b.model }

func (b benched) Complete(context.Context, llm.Request) (*llm.Completion, error) {
	return nil, &llm.Error{
		Kind: llm.KindRateLimit, Provider: "p", Model: b.model,
		Err: fmt.Errorf("rate limited"),
	}
}

// fallbacks returns every hand-off the phase published, in order.
func (c *capture) fallbacks() []*types.ProviderFallback {
	c.mu <- struct{}{}
	defer func() { <-c.mu }()
	var out []*types.ProviderFallback
	for _, ev := range c.events {
		if got, ok := events.DataAs[*types.ProviderFallback](ev); ok {
			out = append(out, got)
		}
	}
	return out
}

func TestAProviderHandOffIsPublishedAgainstTheTurnItHappenedIn(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	prov := &scriptedProvider{execute: deliver(t, "posted the weekly summary")}
	r, _ := buildWith(t, []phase.Entry{
		{Key: "benched", Provider: benched{model: "benched-model"}},
		{Key: "executor", Provider: prov},
	}, buildOpts{pub: pub, execChain: org.ProviderKeys{"benched", "executor"}})

	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	// ONE PER PROVIDER CALL, not one per phase: a chain is walked on every
	// round, so a benched member is a hand-off on every round. Counting them
	// per phase would report one flap where there were three, which is the
	// difference between a blip and a provider to take out of the chain.
	rounds := len(prov.requestsFor("execute"))
	got := pub.fallbacks()
	if rounds == 0 {
		t.Fatal("the executor never reached its provider")
	}
	if len(got) != rounds {
		t.Fatalf("published %d provider_fallback events over %d rounds, want one each",
			len(got), rounds)
	}

	f := got[0]
	// EVERY field, because each one is a different way for the row to be
	// useless: without the turn id the Turn screen cannot select it, without
	// the phase and iteration it cannot be attributed to the round it broke,
	// and without the two keys it names neither the provider that failed nor
	// the one that took over.
	for _, c := range []struct{ field, got, want string }{
		{"turn_id", f.TurnID, "t-1"},
		{"agent_id", f.Agent, "a-1"},
		{"role", f.RoleName, "CTO"},
		{"phase", string(f.Phase), "execute"},
		{"from_provider_key", f.FromProviderKey, "benched"},
		{"to_provider_key", f.ToProviderKey, "executor"},
		{"error_kind", f.ErrorKind, llm.KindRateLimit.String()},
	} {
		if c.got != c.want {
			t.Errorf("%s = %q, want %q", c.field, c.got, c.want)
		}
	}
	if f.Iteration != 1 {
		t.Errorf("iteration = %d, want 1", f.Iteration)
	}
}

func TestTheLastMemberOfAChainFallsBackToNothingAndSaysSo(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	r, _ := buildWith(t, []phase.Entry{
		{Key: "benched", Provider: benched{model: "benched-model"}},
		{Key: "also-benched", Provider: benched{model: "also-benched-model"}},
	}, buildOpts{pub: pub, execChain: org.ProviderKeys{"benched", "also-benched"}})

	// The phase fails: no member answered. That is the point — the hand-offs
	// are what tell an operator WHY, and they are published on the way down
	// rather than reconstructed from the failure afterwards.
	if _, _, err := r.Execute(context.Background(), 1, "", nil); err == nil {
		t.Fatal("Execute succeeded with every member benched")
	}

	got := pub.fallbacks()
	if len(got) != 2 {
		t.Fatalf("published %d provider_fallback events, want 2 — one per member",
			len(got))
	}
	if got[1].ToProviderKey != "" {
		t.Errorf("to_provider_key = %q on the last member, want empty: there is "+
			"nothing left to fall to, and naming one would name a provider that "+
			"does not exist", got[1].ToProviderKey)
	}
	// The summary has to READ as the end of the chain rather than as a line
	// that lost its second half.
	want := "CTO also-benched failed (rate_limit) — no provider left in the chain"
	if summary := got[1].SummaryFor("CTO"); summary != want {
		t.Errorf("summary = %q, want %q", summary, want)
	}
}

func TestAChainThatDoesNotFallThroughPublishesNothing(t *testing.T) {
	t.Parallel()
	pub := newCapture()
	prov := &scriptedProvider{execute: deliver(t, "posted the weekly summary")}
	r, _ := buildWith(t, []phase.Entry{{Key: "executor", Provider: prov}},
		buildOpts{pub: pub})

	if _, _, err := r.Execute(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	// A healthy turn is a SILENT one here. An event published per phase
	// regardless would make the Turn screen's anomaly section permanently
	// non-empty, which is the same as having no anomaly section.
	if got := pub.fallbacks(); len(got) != 0 {
		t.Errorf("published %d provider_fallback events on a chain that never "+
			"fell through, want 0", len(got))
	}
}
