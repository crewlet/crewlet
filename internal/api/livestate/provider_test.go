package livestate_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/livestate"
)

// parkedEnv is the engine's provider park for the Lead, naming the reset.
func parkedEnv(when, idv string) *livestate.Envelope {
	return env("seat_provider_parked", map[string]any{
		"agent_id": "Lead", "provider_keys": []any{"default"},
		"retry_at": "2026-10-27T23:30:00Z", "resets_at": "2026-10-27T23:30:00Z",
		"said":    "You've hit your 5-hour limit · resets 4:30pm (America/Vancouver)",
		"harness": "claude-code", "subscription": true,
	}, at(when), id(idv))
}

// A SEAT PARKED ON ITS MODELS' LIMIT READS ONE STEADY STATE THROUGH EVERY
// PROBE. The engine's probe is a call outside any turn, and one that meets the
// limit renews the park; a turn the seat already had in flight when it parked
// fails its phase on the same limit. None of that is a change a reader should
// see: the seat waited, and waits, for the same reset. Only a phase that
// completes says the limit lifted.
//
// Mutation: let a running turn outrank the park in stateOf, and the turn in
// flight reads as working; or clear the limit on a failed phase, and it reads
// idle.
func TestAParkedSeatReadsStoppedThroughEveryProbe(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	s.Apply(parkedEnv("2026-10-27T23:16:00Z", "park-1"))
	wantState(t, s, "Lead", livestate.ActivityStopped, livestate.StoppedProvider)
	first := overlayOf(t, s, "Lead").ProviderLimit
	if first == nil || !first.Parked || first.ResetsAt == nil ||
		*first.ResetsAt != "2026-10-27T23:30:00Z" ||
		first.Harness != "claude-code" || !first.Subscription {
		t.Fatalf("provider limit = %+v, want the parked limit naming its reset", first)
	}

	// A turn already in flight fails its phase on the same limit, and the
	// probe renews the park.
	probe := map[string]any{"agent_id": "Lead", "turn_id": "tn-2", "phase": "execute", "iteration": 0}
	for _, e := range []*livestate.Envelope{
		env("agent_turn_started", probe, at("2026-10-27T23:30:01Z"), id("ts")),
		env("agent_phase_started", probe, at("2026-10-27T23:30:01Z"), id("ps")),
		env("agent_phase_completed", with(probe, map[string]any{"failed": true,
			"provider_key": "default", "error": "rate_limit"}), at("2026-10-27T23:30:02Z"), id("pc")),
	} {
		s.Apply(e)
		wantState(t, s, "Lead", livestate.ActivityStopped, livestate.StoppedProvider)
	}
	change := s.Apply(parkedEnv("2026-10-27T23:30:02Z", "park-2"))
	wantState(t, s, "Lead", livestate.ActivityStopped, livestate.StoppedProvider)
	again := overlayOf(t, s, "Lead").ProviderLimit
	if again == nil || again.Since != first.Since {
		t.Errorf("the renewed park reads %+v, want the condition's start kept at %q", again, first.Since)
	}
	if _, moved := change.Agents["Lead"]; moved {
		t.Error("a renewal with the same instants pushed the seat: a screen redraws nothing changed")
	}

	// The limit lifts: the next phase completes, and the seat is working.
	s.Apply(env("agent_phase_completed", with(probe, map[string]any{"provider_key": "default"}),
		at("2026-10-27T23:31:00Z"), id("pc2")))
	if got := overlayOf(t, s, "Lead").ProviderLimit; got != nil {
		t.Errorf("provider limit = %+v after a phase answered, want none", got)
	}
}

// A SEAT CARRIED BY ITS FALLBACK IS LIMITED BUT NOT STOPPED, until the head
// that was limited answers.
func TestASeatOnItsFallbackSaysSoWithoutStopping(t *testing.T) {
	t.Parallel()
	s := livestate.New()
	call := map[string]any{"agent_id": "Lead", "turn_id": "tn-1", "phase": "execute", "iteration": 0}
	s.Apply(env("agent_turn_started", call, at("2026-10-27T23:16:00Z"), id("ts")))
	s.Apply(env("agent_phase_started", call, at("2026-10-27T23:16:00Z"), id("ps")))
	s.Apply(env("provider_fallback", with(call, map[string]any{
		"from_provider_key": "plan", "to_provider_key": "key", "error_kind": "rate_limit",
		"resets_at": "2026-10-27T23:30:00Z", "from_harness": "claude-code", "from_subscription": true,
	}), at("2026-10-27T23:16:01Z"), id("fb")))
	wantState(t, s, "Lead", livestate.ActivityWorking, "")
	got := overlayOf(t, s, "Lead").ProviderLimit
	if got == nil || got.Parked || got.Harness != "claude-code" || !got.Subscription {
		t.Fatalf("provider limit = %+v, want the plan limited and the seat on its fallback", got)
	}

	// The fallback answers: the head is still limited.
	s.Apply(env("agent_phase_completed", with(call, map[string]any{"provider_key": "key"}),
		at("2026-10-27T23:17:00Z"), id("pc")))
	if overlayOf(t, s, "Lead").ProviderLimit == nil {
		t.Fatal("a phase the fallback answered cleared the head's limit")
	}
	// The head answers: it is back.
	s.Apply(env("agent_phase_completed", with(call, map[string]any{"provider_key": "plan"}),
		at("2026-10-27T23:31:00Z"), id("pc2")))
	if got := overlayOf(t, s, "Lead").ProviderLimit; got != nil {
		t.Errorf("provider limit = %+v after the head answered, want none", got)
	}
}

// A PARK RELEASED FOR ANOTHER REASON, OR A NEW INSTANCE, ENDS THE CONDITION.
func TestAReleaseOrARespawnEndsTheLimit(t *testing.T) {
	t.Parallel()
	for name, end := range map[string]*livestate.Envelope{
		"released": env("seat_provider_released", map[string]any{
			"agent_id": "Lead", "reason": "the seat's models changed",
		}, at("2026-10-27T23:20:00Z"), id("rel")),
		"respawned": env("agent_spawned", map[string]any{"agent_id": "Lead"},
			at("2026-10-27T23:20:00Z"), id("sp")),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := livestate.New()
			s.Apply(parkedEnv("2026-10-27T23:16:00Z", "park-1"))
			s.Apply(end)
			wantState(t, s, "Lead", livestate.ActivityIdle, "")
			if got := overlayOf(t, s, "Lead").ProviderLimit; got != nil {
				t.Errorf("provider limit = %+v, want none", got)
			}
		})
	}
}

// THE CONDITION REACHES EVERY READER, SO IT NAMES NO ENTRY. Which providers.llm
// keys a company configures, which of them is spent and which carries a seat
// meanwhile are an admin's to read; a person is told what the limit is of, when
// it lifts and whether the seat waits. The keys still move the condition (a
// phase the limited head answers ends it, one its fallback answers does not),
// so they are held and never encoded, and neither is the vendor's sentence.
//
// Mutation: tag the held entry keys for JSON, or carry the parked chain's keys
// or the vendor's sentence again, and the keys or the zone reach the wire.
func TestTheBroadcastLimitCarriesNoConfigKey(t *testing.T) {
	t.Parallel()
	for name, events := range map[string][]*livestate.Envelope{
		"parked": {parkedEnv("2026-10-27T23:16:00Z", "park-1")},
		"on its fallback": {env("provider_fallback", map[string]any{
			"agent_id": "Lead", "turn_id": "tn-1", "phase": "execute",
			"from_provider_key": "plan-secretkey", "to_provider_key": "metered-secretkey",
			"error_kind": "rate_limit", "resets_at": "2026-10-27T23:30:00Z",
			"from_harness": "claude-code", "from_subscription": true,
		}, at("2026-10-27T23:16:01Z"), id("fb"))},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := livestate.New()
			for _, e := range events {
				s.Apply(e)
			}
			limit := overlayOf(t, s, "Lead").ProviderLimit
			if limit == nil {
				t.Fatal("no provider limit to broadcast")
			}
			wire, err := json.Marshal(limit)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			var fields map[string]any
			if err := json.Unmarshal(wire, &fields); err != nil {
				t.Fatalf("Unmarshal: %v", err)
			}
			for key := range fields {
				switch key {
				case "parked", "resets_at", "since", "harness", "subscription":
				default:
					t.Errorf("the broadcast carries %q: %s", key, wire)
				}
			}
			for _, leak := range []string{"default", "secretkey", "Vancouver"} {
				if strings.Contains(string(wire), leak) {
					t.Errorf("the broadcast carries %q: %s", leak, wire)
				}
			}
		})
	}
}
