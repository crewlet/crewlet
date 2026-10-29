package engine

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/runtoken"
)

// A CHECK DIAGNOSES THE ENTRY AS IT RUNS, OR THE DRAFT AS IT WOULD: the active
// entry by its key, and a draft as the document it would join, never the
// active entry under the draft's name.
//
// Mutation: diagnose the active entry when a draft is given, and this fails.
func TestACheckDiagnosesTheEntryOrItsDraft(t *testing.T) {
	t.Parallel()
	c, _ := chainCompany(t, chainMember{key: "plan", agent: "claude-code", model: "opus"})
	e := epochEngine(c)

	got, err := e.CheckModel(t.Context(), "plan", nil, nil, false)
	if err != nil || got.Provider != "plan" || got.Model != "opus" {
		t.Fatalf("CheckModel(plan) = %+v, %v; want the active entry's diagnosis", got, err)
	}
	if _, err := e.CheckModel(t.Context(), "nobody", nil, nil, false); !errors.Is(err, ErrNoModel) {
		t.Fatalf("checking an unknown entry = %v, want ErrNoModel", err)
	}
	draft := config.LLMProvider{Agent: "claude-code", Model: "haiku", StateDir: t.TempDir(),
		Overrides: map[string]any{"binary": "/nonexistent/claude"}}
	got, err = e.CheckModel(t.Context(), "plan", &draft, nil, false)
	if err != nil || got.Model != "haiku" {
		t.Fatalf("CheckModel(plan, draft) = %+v, %v; want the draft's diagnosis", got, err)
	}
}

// THE RUNS STILL OUT ON AN ENTRY ARE COUNTED from the runs' own rows, by the
// entry each job was launched on: what a credential write tells the person
// keeps the old credential until its segment ends.
func TestTheRunsOutOnAnEntryAreCounted(t *testing.T) {
	t.Parallel()
	c, seat := chainCompany(t, chainMember{key: "plan", agent: "claude-code", model: "opus"})
	e, _ := launchRig(t, c, bridgeFor(t, mcpbridge.Options{
		Material: runtoken.OneKey("k", "test-key"), BaseURL: "https://engine.example.com",
	}))
	l := launcherOn(e, c, seat, "t1")
	var handoffs []chain.Fallback
	if _, err := l.LaunchExecutor(t.Context(), launchRequest(l, &handoffs)); err != nil {
		t.Fatalf("LaunchExecutor: %v", err)
	}
	if n, err := e.RunningRunsOn(t.Context(), []string{"plan"}); err != nil || n != 1 {
		t.Errorf("RunningRunsOn(plan) = %d, %v; want the one run out on it", n, err)
	}
	if n, err := e.RunningRunsOn(t.Context(), []string{"other"}); err != nil || n != 0 {
		t.Errorf("RunningRunsOn(other) = %d, %v; want none", n, err)
	}
}

// A CANDIDATE IS TESTED IN A HOME THAT IS GONE AFTERWARD, AND IS NEVER KEPT.
// Whatever way the test ends (the CLI answered or failed, or the caller's
// context was already cancelled), the throwaway home the candidate ran in is
// removed, and the value reaches neither the fleet's secret store nor any log
// line: a credential nobody chose to store must not outlive its test.
//
// NOT PARALLEL: it reads the process's log output, which only a test running
// alone may install a sink for.
//
// Mutation: drop the home's removal, run the candidate in the entry's own
// state directory, or log the values, and this fails.
func TestACandidatesTestLeavesNothingBehind(t *testing.T) {
	var logs bytes.Buffer
	logging.Configure(slog.LevelDebug, logging.FormatText, &logs)
	t.Cleanup(func() { logging.Configure(slog.LevelInfo, logging.FormatConsole, os.Stderr) })

	const candidate = "sk-ant-api03-CANDIDATE-NEVER-STORED-4242"
	c, _ := chainCompany(t, chainMember{key: "plan", agent: "claude-code", model: "opus"})
	e := epochEngine(c)
	fleet := coordmemory.NewFleet()
	e.backends = &Backends{Fleet: fleet}
	e.checkHomes = t.TempDir()

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	for name, ctx := range map[string]context.Context{"a test that ran": t.Context(), "a cancelled test": cancelled} {
		if _, err := e.CheckModel(ctx, "plan", nil, map[string]string{"ANTHROPIC_API_KEY": candidate}, true); err != nil {
			t.Fatalf("%s: CheckModel: %v", name, err)
		}
		if left, _ := os.ReadDir(e.checkHomes); len(left) != 0 {
			t.Errorf("%s left its home behind: %v", name, left)
		}
	}
	if rows, err := fleet.SecretValues(t.Context()); err != nil || len(rows) != 0 {
		t.Errorf("the secret store holds %d row(s) (%v) after a candidate's test, want none", len(rows), err)
	}
	if strings.Contains(logs.String(), candidate) {
		t.Error("a log line carries the candidate credential")
	}
}
