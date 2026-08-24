package runner_test

import (
	"context"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prefetch"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/tools"
)

// The prefetched context, as the phases receive it.
//
// The runner takes STRINGS: it cannot re-fetch, which is the freeze. What is
// left to get wrong is which phase is shown which block, and that is exactly
// what these pin.

// withContext builds a runner over the given blocks and recon seam.
func withContext(t *testing.T, prov *scriptedProvider, blocks prefetch.Blocks,
	recon func(context.Context, string) string,
) *runner.Runner {
	t.Helper()
	models, err := phase.NewRegistry([]phase.Entry{{Key: "default", Provider: prov}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	role := &org.Role{Name: "CTO", DeclaredHandle: "cto"}
	r, err := runner.New(runner.Config{
		Seat:     prompts.Seat{Org: &org.Organization{Name: "Acme", Roles: []*org.Role{role}}, Role: role},
		Registry: tools.NewRegistry(),
		Models:   models,
		Caps:     runner.Caps{PlanRounds: 4, ExecuteRounds: 4, ReviewRounds: 3},
		Task:     "post the weekly summary",
		Context:  blocks, Recon: recon,
	})
	if err != nil {
		t.Fatalf("runner.New: %v", err)
	}
	return r
}

func systemOf(t *testing.T, prov *scriptedProvider, phaseName string) string {
	t.Helper()
	requests := prov.requestsFor(phaseName)
	if len(requests) == 0 {
		t.Fatalf("no %s call was made", phaseName)
	}
	for _, m := range requests[0].Messages {
		if m.Role == llm.RoleSystem {
			return m.Content
		}
	}
	t.Fatalf("the %s call had no system message", phaseName)
	return ""
}

// Every block the prefetch renders reaches the PLAN prompt under its own
// heading. A block rendered and not shown is work spent on nothing.
func TestEveryPrefetchedBlockReachesThePlanPrompt(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{plan: []llm.Completion{text("planning")}}
	r := withContext(t, prov, prefetch.Blocks{
		PersonalMemory:      "- always use semantic commits",
		RelevantKnowledge:   "- **Staging runbook**: how the proxy is wired",
		EpisodeRecall:       "- fixed a redirect loop before",
		CounterpartyProfile: "Subject: Ana Ruiz",
		SynthesizedSkills:   "- **ship-a-fix**: the release checklist",
	}, nil)
	if _, _, err := r.Plan(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Plan: %v", err)
	}

	system := systemOf(t, prov, "plan")
	for _, want := range []string{
		"## Personal memory", "always use semantic commits",
		"## Relevant knowledge", "Staging runbook",
		"## Similar prior work", "fixed a redirect loop before",
		"## Known counterparty", "Subject: Ana Ruiz",
		"## Synthesized skills", "ship-a-fix",
	} {
		if !strings.Contains(system, want) {
			t.Fatalf("the plan prompt is missing %q", want)
		}
	}
}

// THE COUNTERPARTY PROFILE IS FORWARDED TO EXECUTE. The executor needs the
// requester's observed traits even where the plan describes the action
// abstractly — "reply in the counterparty's preferred register" is a plan
// step that cannot be carried out by somebody who cannot see the register.
func TestTheCounterpartyProfileReachesTheExecutor(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{execute: []llm.Completion{text("posted")}}
	r := withContext(t, prov, prefetch.Blocks{
		CounterpartyProfile: "Subject: Ana Ruiz\nObserved by you:\n  - tone: terse",
		// The memory block is NOT forwarded, and that is the contrast:
		// Execute is running a decided plan, and a paragraph of standing
		// memory in front of it competes with the plan for attention.
		PersonalMemory: "- always use semantic commits",
	}, nil)
	if _, _, err := r.Execute(context.Background(), 1,
		turn.Plan{Decision: turn.PlanDirect, Summary: "reply to Ana"}, nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	system := systemOf(t, prov, "execute")
	if !strings.Contains(system, "tone: terse") {
		t.Fatalf("the executor was not given the counterparty:\n%s", system)
	}
	if strings.Contains(system, "always use semantic commits") {
		t.Fatalf("the executor was handed the Plan-phase memory block:\n%s", system)
	}
}

// THE RECON SEAM is the one mid-turn fetch, and it is keyed on the plan
// summary — which does not exist until Plan has run.
func TestTheReconSeamIsKeyedOnThePlanSummary(t *testing.T) {
	t.Parallel()
	var asked []string
	prov := &scriptedProvider{execute: []llm.Completion{text("posted")}}
	r := withContext(t, prov, prefetch.Blocks{}, func(_ context.Context, summary string) string {
		asked = append(asked, summary)
		return "- **Staging runbook**: recovered after recon"
	})
	if _, _, err := r.Execute(context.Background(), 1,
		turn.Plan{Decision: turn.PlanRun, Summary: "fix the staging redirect"}, nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}

	if len(asked) != 1 || asked[0] != "fix the staging redirect" {
		t.Fatalf("the recon seam was asked %v", asked)
	}
	if system := systemOf(t, prov, "execute"); !strings.Contains(system, "recovered after recon") {
		t.Fatalf("the recovered block did not reach the executor:\n%s", system)
	}
}

// A runner with no recon seam is the ordinary case — a company with no
// knowledge backend — and Execute must not care.
func TestExecuteRunsWithNoReconSeam(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{execute: []llm.Completion{text("posted")}}
	r := withContext(t, prov, prefetch.Blocks{}, nil)
	if _, _, err := r.Execute(context.Background(), 1,
		turn.Plan{Decision: turn.PlanDirect}, nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if system := systemOf(t, prov, "execute"); strings.Contains(system, "## Relevant knowledge") {
		t.Fatalf("an empty recovery rendered a heading:\n%s", system)
	}
}

// AN EMPTY BLOCK RENDERS NO HEADING. A heading with nothing under it tells
// the planner it has a memory, or a knowledge base, that it cannot read.
func TestAnEmptyBlockRendersNoHeading(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{plan: []llm.Completion{text("planning")}}
	r := withContext(t, prov, prefetch.Blocks{}, nil)
	if _, _, err := r.Plan(context.Background(), 1, "", nil); err != nil {
		t.Fatalf("Plan: %v", err)
	}
	system := systemOf(t, prov, "plan")
	for _, heading := range []string{
		"## Personal memory", "## Relevant knowledge", "## Similar prior work",
		"## Known counterparty", "## Synthesized skills", "## First-turn onboarding",
	} {
		if strings.Contains(system, heading) {
			t.Fatalf("an empty block rendered %q", heading)
		}
	}
}
