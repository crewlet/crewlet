package runner_test

import (
	"context"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/prefetch"
	"github.com/crewlet/crewlet/internal/agent/prompts"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/llmtest"
	"github.com/crewlet/crewlet/internal/tools"
)

// The prefetched context, as the phases receive it.
//
// The runner takes STRINGS: it cannot re-fetch, which is the freeze. What is
// left to get wrong is which phase is shown which block, and that is exactly
// what these pin.

// withContext builds a runner over the given blocks, for the reviewer's case.
func withContext(t *testing.T, prov *scriptedProvider, blocks prefetch.Blocks) *runner.Runner {
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
		Launcher: &playLauncher{},
		Task:     "post the weekly summary",
		Context:  blocks,
		Sessions: sessions,
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

// launchedSystem is the system text the executor's run was launched with, on
// a runner over the given blocks: what its coding CLI appends to its own.
func launchedSystem(t *testing.T, blocks prefetch.Blocks) string {
	t.Helper()
	_, req := launch(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
		buildOpts{context: blocks}, 1, "", nil)
	return req.System
}

// Every block the prefetch renders reaches the executor's prompt under its own
// heading — the system text its run is launched with. A block rendered and not
// shown is work spent on nothing — and there is only ONE prompt to reach, which
// is the point: the frame that decides is the frame that acts, so nothing has
// to be forwarded from one to another.
func TestEveryPrefetchedBlockReachesTheExecutorsPrompt(t *testing.T) {
	t.Parallel()
	system := launchedSystem(t, prefetch.Blocks{
		ThreadContext:       "- **Ana Ruiz (ana)**: staging redirects in a loop",
		PersonalMemory:      "- always use semantic commits",
		RelevantKnowledge:   "- **Staging runbook**: how the proxy is wired",
		EpisodeRecall:       "- fixed a redirect loop before",
		CounterpartyProfile: "Subject: Ana Ruiz\nObserved by you:\n  - tone: terse",
		SynthesizedSkills:   "- **ship-a-fix**: the release checklist",
	})
	for _, want := range []string{
		"## The thread so far", "staging redirects in a loop",
		"## Personal memory", "always use semantic commits",
		"## Relevant knowledge", "Staging runbook",
		"## Similar prior work", "fixed a redirect loop before",
		"## Known counterparty", "tone: terse",
		"## Synthesized skills", "ship-a-fix",
	} {
		if !strings.Contains(system, want) {
			t.Fatalf("the executor's prompt is missing %q", want)
		}
	}
}

// THE REVIEWER GETS NONE OF IT. Its question is whether this round's work is
// right, and standing memory, the team's docs and the requester's traits are
// what the executor needed to DO the work — in front of a reviewer they
// compete with the evidence it is meant to judge.
func TestTheReviewerIsNotHandedThePrefetch(t *testing.T) {
	t.Parallel()
	prov := &scriptedProvider{
		review: []llmtest.Response{submitCall(t, runner.SubmitReviewTool, `{"decision":"done"}`)},
	}
	r := withContext(t, prov, prefetch.Blocks{
		PersonalMemory:      "- always use semantic commits",
		RelevantKnowledge:   "- **Staging runbook**: how the proxy is wired",
		CounterpartyProfile: "Subject: Ana Ruiz",
	})
	if _, err := r.Review(context.Background(), 1, workFor("reply to Ana"), nil); err != nil {
		t.Fatalf("Review: %v", err)
	}
	system := systemOf(t, prov, "review")
	for _, unwanted := range []string{
		"always use semantic commits", "Staging runbook", "Subject: Ana Ruiz",
	} {
		if strings.Contains(system, unwanted) {
			t.Fatalf("the reviewer was handed the executor's context: %q", unwanted)
		}
	}
}

// AN EMPTY BLOCK RENDERS NO HEADING. A heading with nothing under it tells the
// agent it has a memory, or a knowledge base, that it cannot read.
func TestAnEmptyBlockRendersNoHeading(t *testing.T) {
	t.Parallel()
	system := launchedSystem(t, prefetch.Blocks{})
	for _, heading := range []string{
		"## The thread so far",
		"## Personal memory", "## Relevant knowledge", "## Similar prior work",
		"## Known counterparty", "## Synthesized skills",
	} {
		if strings.Contains(system, heading) {
			t.Fatalf("an empty block rendered %q", heading)
		}
	}
}

// A LATER LAUNCH IS BUILT FROM THE CONTEXT THE TURN STARTED WITH. A turn sent
// back for another iteration launches again from the segment that collected
// its run — a fresh runner, which the engine hands no prefetch — so the freeze
// has to survive the park, or the second run is briefed without the seat's
// memory, its company's pages and the conversation it is part of, on a system
// prompt that moved under work already in hand.
//
// Mutation: leave the context off the park, and the second launch lacks it.
func TestALaterLaunchIsBuiltFromTheContextTheTurnStartedWith(t *testing.T) {
	t.Parallel()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	opts := buildOpts{
		context: prefetch.Blocks{
			ThreadContext:  "- **Ana Ruiz (ana)**: staging redirects in a loop",
			PersonalMemory: "- always use semantic commits",
		},
		conversation: "### Turn 1\nYou replied: shipped the proxy fix",
	}
	launched, first := launch(t, entries, opts, 1, "", nil)
	bridged := play(t, first, deliver(t, "posted"))

	relauncher := &playLauncher{}
	collecting := opts
	collecting.launcher = relauncher
	collector, _, _ := collect(t, entries, collecting, launched, nil, bridged, "posted", runner.RunRecord{})
	if _, _, err := collector.Execute(context.Background(), 2, "try again", nil); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	second := relauncher.last(t)
	if second.System != first.System {
		t.Errorf("the second run's system prompt moved:\nfirst:  %q\nsecond: %q", first.System, second.System)
	}
	if !strings.Contains(second.Brief, "You replied: shipped the proxy fix") {
		t.Errorf("the second run's brief lost the conversation the turn is part of:\n%s", second.Brief)
	}
}
