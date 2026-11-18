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

// The prefetched context, as the phases receive it. The runner takes strings,
// so it cannot re-fetch (the freeze); these pin which phase sees which block.

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

// launchedSystem is the system text the executor's run was launched with over
// the given blocks: what its harness appends to its own.
func launchedSystem(t *testing.T, blocks prefetch.Blocks) string {
	t.Helper()
	_, req := launch(t, []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}},
		buildOpts{context: blocks}, 1, "", nil)
	return req.System
}

// Every block the prefetch renders reaches the executor's launched system text
// under its own heading: one prompt, so the frame that decides is the frame
// that acts and nothing is forwarded between them.
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

// THE REVIEWER GETS NONE OF IT: memory, docs and the requester's traits served
// the executor doing the work, and in front of a reviewer they compete with the
// evidence it judges.
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

// AN EMPTY BLOCK RENDERS NO HEADING, which would tell the agent it has a
// memory or knowledge base it cannot read.
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
// back relaunches from the collecting segment, a fresh runner handed no
// prefetch or conversation read, so the freeze must survive the park. The
// endings reach the second brief byte for byte as the first carried them: the
// turn's end absorbs the read its first brief told; a sibling that ended since
// is the next turn's.
//
// Mutation: leave the context or the endings off the park, or render the
// endings again on the resume, and the second launch lacks them or differs.
func TestALaterLaunchIsBuiltFromTheContextTheTurnStartedWith(t *testing.T) {
	t.Parallel()
	entries := []phase.Entry{{Key: "default", Provider: &scriptedProvider{}}}
	opts := buildOpts{
		context: prefetch.Blocks{
			ThreadContext:  "- **Ana Ruiz (ana)**: staging redirects in a loop",
			PersonalMemory: "- always use semantic commits",
		},
		endings: "## How this conversation's earlier turns ended\n\n### Turn t-0\nIt completed.\n" +
			"Its reply reached the party waiting on `slack`.",
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
	if !strings.Contains(first.Brief, opts.endings) || !strings.Contains(second.Brief, opts.endings) {
		t.Errorf("the second run's brief does not tell the endings the first did, byte for byte:\n%s",
			second.Brief)
	}
}
