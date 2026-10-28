package livestate_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/livestate"
)

// entryKey is a providers.llm key no reader but an admin may be sent.
const entryKey = "plan-secretkey"

// rawFailure is a chain error as the engine writes it: it names the entry.
const rawFailure = "llm chain exhausted after 1 providers (" + entryKey + "): usage limit"

// leakyState is a seat whose live state has met every field that can carry the
// company's configuration: a fallback off the entry, a CLI run it served, a
// phase that failed naming it, and the chain's exhaustion.
func leakyState() *livestate.LiveState {
	s := livestate.New()
	call := map[string]any{"agent_id": "Lead", "turn_id": "tn-1", "phase": "execute", "iteration": 0}
	s.Apply(env("agent_phase_started", call, at("2026-10-27T23:16:00Z"), id("ps")))
	s.Apply(env("provider_fallback", with(call, map[string]any{
		"from_provider_key": entryKey, "to_provider_key": "metered-" + entryKey,
		"error_kind": "rate_limit", "from_cli": "claude-code", "from_subscription": true,
	}), at("2026-10-27T23:16:01Z"), id("fb")))
	s.Apply(env("agent_phase_completed", with(call, map[string]any{
		"failed": true, "error_kind": "rate_limit", "error": rawFailure, "provider_key": entryKey,
		"invocations": []any{map[string]any{"provider_key": entryKey, "outcome": "failed",
			"first_round": 1, "last_round": 1}},
	}), at("2026-10-27T23:16:02Z"), id("pc")))
	return s
}

// wireOf is v as a reader receives it.
func wireOf(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	return string(raw)
}

// NO BROADCAST FIELD CARRIES THE COMPANY'S CONFIGURATION OR A RAW FAILURE, to
// any reader but an admin. Every shape a seat's live state reaches a reader in
// (its overlay, its live call whole and lean, its roster row) is read for the
// entry's key and for the failure's own words; a seat's lead reads its trail
// and is still not an admin. The failure's kind stays, which the dashboard
// words for a person, and an admin is sent the text.
//
// Mutation: leave the failure's text or a run's entry in any one of the
// masks, or broadcast it as status, and the lead's copy names the entry.
func TestNoBroadcastFieldCarriesAConfigKeyOrARawFailure(t *testing.T) {
	t.Parallel()
	s := leakyState()
	overlay := s.AgentOverlay("Lead")
	if overlay == nil || overlay.LiveCall == nil || overlay.LastError == nil {
		t.Fatalf("overlay = %+v, want a failed call and a last error", overlay)
	}
	shapes := func(r livestate.Reach) map[string]any {
		rows := s.MergeAgents([]map[string]any{{"agent_id": "Lead", "handle": "lead"}})
		return map[string]any{
			"the overlay":    overlay.For(r),
			"the call whole": overlay.LiveCall.For(r),
			"the call lean":  livestate.MaskCall(overlay.LiveCall.Without(livestate.CallVersions{}), r),
			"the roster row": livestate.MaskRow(rows[0], r),
			"the pushed row": livestate.MaskRow(s.OverlayRows([]string{"Lead"})[0], r),
		}
	}
	for reader, r := range map[string]livestate.Reach{
		"everybody":       {},
		"a tracker user":  {Tracker: true},
		"the seat's lead": {Body: true, Tracker: true},
	} {
		for shape, v := range shapes(r) {
			wire := wireOf(t, v)
			for _, leak := range []string{entryKey, "llm chain exhausted"} {
				if strings.Contains(wire, leak) {
					t.Errorf("%s is sent %s carrying %q: %s", reader, shape, leak, wire)
				}
			}
			if shape == "the overlay" && !strings.Contains(wire, `"kind":"rate_limit"`) {
				t.Errorf("%s is sent %s without the failure's kind: %s", reader, shape, wire)
			}
		}
	}
	admin := livestate.Reach{Body: true, Tracker: true, Config: true}
	for shape, v := range shapes(admin) {
		if wire := wireOf(t, v); !strings.Contains(wire, rawFailure) {
			t.Errorf("an admin is sent %s without the failure's own text: %s", shape, wire)
		}
	}
}
