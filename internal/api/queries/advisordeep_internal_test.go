package queries

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/advisor/modelpass"
	"github.com/crewlet/crewlet/internal/advisordomain"
	"github.com/crewlet/crewlet/internal/iam"
)

// THE DEEP RUN ASKS AS THE ADVISOR: the engine's own principal holding an
// administrator's reads, never a person's grant. Mutation: ask as a person,
// and a check fails.
func TestTheDeepRunAsksAsTheAdvisor(t *testing.T) {
	t.Parallel()
	var asked iam.Principal
	reads := deepReads{ask: func(ctx context.Context, _ string, _ Params) (any, error) {
		asked, _ = iam.From(ctx)
		return map[string]any{}, nil
	}}
	if _, err := reads.Answer(t.Context(), "advisor_snapshot", nil); err != nil {
		t.Fatal(err)
	}
	if asked.Kind != iam.KindEngine || !asked.Can(iam.GrantAdmin) || asked.Seat != "" {
		t.Errorf("asked as %+v, want the engine's Advisor holding an administrator's reads", asked)
	}
}

// A DEEP RUN'S SUMMARY SAYS WHAT IT DID: its kind, its progress lines, what it
// spent of its bounds and the bound it stopped at. Mutation: read its record as
// a daily pass's, and the progress is lost.
func TestADeepRunsSummarySaysWhatItDid(t *testing.T) {
	t.Parallel()
	var run modelpass.DeepRun
	run.Status, run.InputTokens, run.OutputTokens, run.StoppedBy, run.Minutes = "done", 900_000, 100_000, "done", 12.4
	run.Bounds.Tokens, run.Bounds.Minutes = 2_000_000, 20
	run.Progress = []modelpass.Progress{{Text: "reading capacity"}}
	raw, err := json.Marshal(run)
	if err != nil {
		t.Fatal(err)
	}
	got, err := runSummary(advisordomain.RunRow{ID: "r", Status: advisordomain.RunDone, Kind: advisordomain.RunKindDeep,
		Classes: json.RawMessage(`[]`), Model: raw})
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != "deep" || len(got.Progress) != 1 || got.Budget == nil || got.Budget.Tokens != 1_000_000 ||
		got.Budget.MinutesCeiling != 20 || got.Budget.Minutes != 13 || got.StoppedBy == nil || *got.StoppedBy != "done" {
		t.Errorf("summary = %+v, budget %+v", got, got.Budget)
	}
}

// A PREMISE IS ASKED AGAIN AS THE ADVISOR: its read answered through the
// registry, the number at its path compared as the finding said, a number no
// longer there not holding, and a read off the allowlist refused. Mutation:
// read a missing number as holding, and a finding about an item gone stays.
func TestAPremiseIsAskedAgainAsTheAdvisor(t *testing.T) {
	t.Parallel()
	hits := 4.0
	o := advisorObserver{ask: func(context.Context, string, Params) (any, error) {
		if hits < 0 {
			return map[string]any{"entries": []any{}}, nil
		}
		return map[string]any{"entries": []any{map[string]any{"hits": hits}}}, nil
	}}
	p := advisor.Premise{Read: "work_workload", Path: "entries[0].hits", Op: advisor.PremiseGe, Value: 3}
	if held, err := o.Holds(t.Context(), p); err != nil || !held {
		t.Errorf("4 hits held %v (%v), want held", held, err)
	}
	hits = 2
	if held, _ := o.Holds(t.Context(), p); held {
		t.Error("2 hits held a premise of 3 or more")
	}
	hits = -1
	if held, err := o.Holds(t.Context(), p); err != nil || held {
		t.Errorf("a number no longer there held %v (%v)", held, err)
	}
	if _, err := o.Holds(t.Context(), advisor.Premise{Read: "iam_sessions", Path: "n", Op: advisor.PremiseGe}); err == nil {
		t.Error("a premise off the allowlist was asked")
	}
}
