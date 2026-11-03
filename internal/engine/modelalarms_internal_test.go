package engine

import (
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
)

// conditionOf is key's condition in conditions.
func conditionOf(t *testing.T, conditions []statelog.ModelCondition, key string) statelog.ModelCondition {
	t.Helper()
	for _, m := range conditions {
		if m.Key == key {
			return m
		}
	}
	t.Fatalf("no condition for %q in %+v", key, conditions)
	return statelog.ModelCondition{}
}

// A MODEL'S CONDITION IS THE FLEET'S, JOINED FROM EVERY NODE'S HEARTBEAT: a
// refusal on one node stands until an answer on ANY node is newer; the latest
// window and the earliest wait across nodes are the entry's; a token's expiry
// is computed from when the store wrote it; and "nothing signs it in" is this
// node's reading, and only for an entry some seat or the assistant runs.
//
// Mutation: read the refusal off one node, take any node's window rather than
// the latest, or raise "no sign-in" for an entry nothing runs, and this fails.
func TestAModelsConditionIsTheFleetsHeartbeats(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)
	c := &Company{Config: &config.Company{Providers: config.Providers{LLM: map[string]config.LLMProvider{
		"default":      {Harness: "claude-code", Model: "opus", Auth: config.HarnessAuth{Token: "${CLAUDE_CODE_OAUTH_TOKEN}"}},
		"recovered":    {Harness: "claude-code", Model: "opus", Auth: config.HarnessAuth{Token: "${RECOVERED_TOKEN}"}},
		"idle":         {Harness: "codex", Model: "gpt-6.1-sol"},
		"unsigned-run": {Harness: "codex", Model: "gpt-6.1-sol"},
	}}}}
	seat := uuid.MustParse("6f1c2a3b-4d5e-5f60-8172-839405a6b7c8")
	early, late := now.Add(-time.Hour), now.Add(-time.Minute)
	nodes := map[string][]coord.ModelStatus{
		"node-1": {
			{Key: "default", Resolves: true, LastAnswer: early,
				SittingOut: &coord.ModelSitOut{Until: now.Add(time.Hour), Window: "session"},
				Parked:     []coord.ParkedSeat{{Seat: seat, Since: late}}},
			{Key: "recovered", Resolves: true, LastFailure: &coord.ModelFailure{Kind: "auth", At: early, Said: "401"}},
			{Key: "idle"}, {Key: "unsigned-run"},
		},
		"node-2": {
			{Key: "default", Resolves: true,
				LastFailure: &coord.ModelFailure{Kind: "auth", At: late, Said: "Failed to authenticate"},
				SittingOut:  &coord.ModelSitOut{Until: now.Add(3 * time.Hour), Window: "weekly"},
				Parked:      []coord.ParkedSeat{{Seat: seat, Since: early}}},
			{Key: "recovered", Resolves: true, LastAnswer: late},
		},
	}
	stored := map[string]time.Time{"CLAUDE_CODE_OAUTH_TOKEN": now.Add(-360 * 24 * time.Hour)}
	lifetimes := map[string]time.Duration{"default": 365 * 24 * time.Hour, "recovered": 365 * 24 * time.Hour}
	named := map[string]bool{"default": true, "recovered": true, "unsigned-run": true}

	got := modelConditions(c, nodes, "node-1", stored, named, lifetimes, now)
	def := conditionOf(t, got, "default")
	if !def.RefusedAt.Equal(late) || def.RefusedSaid != "Failed to authenticate" {
		t.Errorf("default's refusal = %v %q, want node-2's, newer than any answer", def.RefusedAt, def.RefusedSaid)
	}
	if def.SittingOut == nil || def.SittingOut.Window != "weekly" {
		t.Errorf("default's window = %+v, want the latest across nodes (node-2's weekly)", def.SittingOut)
	}
	if !def.WaitingSince.Equal(early) {
		t.Errorf("default's wait = %v, want the earliest park across nodes", def.WaitingSince)
	}
	if def.Variable != "CLAUDE_CODE_OAUTH_TOKEN" || !def.ExpiresAt.Equal(stored["CLAUDE_CODE_OAUTH_TOKEN"].Add(365*24*time.Hour)) {
		t.Errorf("default's expiry = %v of %q, want a year after the store wrote it", def.ExpiresAt, def.Variable)
	}
	if rec := conditionOf(t, got, "recovered"); !rec.RefusedAt.IsZero() || !rec.ExpiresAt.IsZero() {
		t.Errorf("recovered = %+v, want no refusal (an answer on node-2 is newer) and no expiry "+
			"(its token is read from the environment, not the store)", rec)
	}
	if idle := conditionOf(t, got, "idle"); idle.NoSignIn {
		t.Error("an entry nothing runs raised no-sign-in")
	}
	if run := conditionOf(t, got, "unsigned-run"); !run.NoSignIn {
		t.Error("an entry a seat runs, with nothing signing it in on this node, is not reported")
	}

	// AND THE TABLE JUDGES THEM: default is refused and waits past the grace.
	alarms := statelog.Evaluate(statelog.Reading{Models: got, ModelsAt: now})
	kinds := map[statelog.Kind]bool{}
	for _, a := range alarms {
		kinds[a.Kind] = true
	}
	if !kinds[statelog.KindLLMSignInRefused] || !kinds[statelog.KindLLMLimitWaiting] || !kinds[statelog.KindLLMCredentialExpiresSoon] {
		t.Errorf("the table raised %v, want a refusal, a wait and a token expiring within seven days", kinds)
	}
}
