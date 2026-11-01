package livestate_test

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/livestate"
)

// entryKey is a providers.llm key no reader but an admin may be sent.
const entryKey = "plan-secretkey"

// rawFailure is a chain error as the engine writes it: it names the entry.
const rawFailure = "llm chain exhausted after 1 providers (" + entryKey + "): usage limit"

// leakyState is a seat whose live state has met every field that can carry the
// company's configuration: a fallback off the entry, a CLI run it served, a
// phase that failed naming it, the chain's exhaustion, and an executor
// launched on it.
func leakyState() *livestate.LiveState {
	s := livestate.New()
	call := map[string]any{"agent_id": "Lead", "turn_id": "tn-1", "phase": "execute", "iteration": 0}
	s.Apply(env("executor_launched", map[string]any{"agent_id": "Lead", "turn_id": "tn-0",
		"role": "Lead", "agent_handle": "lead", "coding_agent": "claude-code",
		"provider_key": entryKey, "model": "opus", "sandbox_id": "sb-1", "task": "fix it",
	}, at("2026-10-27T23:15:00Z"), id("sr")))
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
// (its overlay, its live call whole and lean, its roster row, and the executor
// it launched, which the `executors` push and snapshot carry) is read for
// the entry's key and for the failure's own words; a seat's lead reads its
// trail and is still not an admin. The failure's kind stays, which the
// dashboard words for a person, and so does the model an executor runs
// under; an admin is sent the text and the run's entry.
//
// Mutation: leave the failure's text, a CLI run's entry or an executor's in
// any one of the masks, or broadcast it as status, and the lead's copy names
// the entry.
func TestNoBroadcastFieldCarriesAConfigKeyOrARawFailure(t *testing.T) {
	t.Parallel()
	s := leakyState()
	overlay := s.AgentOverlay("Lead")
	if overlay == nil || overlay.LiveCall == nil || overlay.LastError == nil {
		t.Fatalf("overlay = %+v, want a failed call and a last error", overlay)
	}
	runs := s.ActiveExecutors()
	if len(runs) != 1 || runs[0].ProviderKey != entryKey {
		t.Fatalf("runs = %+v, want the executor launched on the entry", runs)
	}
	shapes := func(r livestate.Reach) map[string]any {
		rows := s.MergeAgents([]map[string]any{{"agent_id": "Lead", "handle": "lead"}})
		return map[string]any{
			"the overlay":    overlay.For(r),
			"the call whole": overlay.LiveCall.For(r),
			"the call lean":  livestate.MaskCall(overlay.LiveCall.Without(livestate.CallVersions{}), r),
			"the roster row": livestate.MaskRow(rows[0], r),
			"the pushed row": livestate.MaskRow(s.OverlayRows([]string{"Lead"})[0], r),
			"the executor":   runs[0].For(r),
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
		want := rawFailure
		if shape == "the executor" {
			want = `"provider_key":"` + entryKey + `"`
		}
		if wire := wireOf(t, v); !strings.Contains(wire, want) {
			t.Errorf("an admin is sent %s without %s: %s", shape, want, wire)
		}
	}
	// THE MODEL A RUN RUNS UNDER STAYS EVERYBODY'S: it is what the run is
	// doing, as a call's model is.
	if wire := wireOf(t, runs[0].For(livestate.Reach{})); !strings.Contains(wire, `"model":"opus"`) {
		t.Errorf("everybody is sent the executor without its model: %s", wire)
	}
}

// A SPEND RECORD WITH NO MODEL IS NOT NAMED BY ITS ENTRY. The live window read
// a phase's model and fell back to its `provider_key`, so a phase that named no
// model reached every reader's spend by model under the company's entry key,
// where the store's rows (which a named window folds) carry none.
//
// Mutation: fall back to `provider_key` for the model again, and the record is
// named by the entry.
func TestASpendRecordIsNeverNamedByItsEntry(t *testing.T) {
	t.Parallel()
	now, _ := time.Parse(time.RFC3339, "2026-10-27T23:20:00Z")
	s := livestate.New(livestate.WithClock(func() time.Time { return now }))
	s.Apply(env("agent_phase_completed", map[string]any{
		"agent_id": "Lead", "turn_id": "tn-1", "phase": "execute", "iteration": 0,
		"provider_key": entryKey, "input_tokens": 10, "total_tokens": 10,
	}, at("2026-10-27T23:16:02Z"), id("pc")))
	records := s.SpendRecords()
	if len(records) != 1 {
		t.Fatalf("%d spend records, want the phase's", len(records))
	}
	if records[0].Model == entryKey {
		t.Errorf("the phase's spend is filed under model %q, its entry's key", records[0].Model)
	}
}
