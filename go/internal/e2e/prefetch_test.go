package e2e

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/learning"
)

// waitForSeat blocks until this node owns the seat: publishing before the
// claim is safe (the queue is durable) but makes a later failure read as a
// lost message rather than a slow claim.
func waitForSeat(t *testing.T, n *node, handle string) {
	t.Helper()
	waitFor(t, "the seat to be claimed", func() bool {
		return slices.Contains(n.engine.Node().Host().Held(), handle)
	})
}

// waitForTurn blocks until the seat has planned, which is the only phase
// these tests assert about.
func waitForTurn(t *testing.T, n *node) {
	t.Helper()
	waitFor(t, "the plan phase to run", func() bool {
		return slices.Contains(n.model.seen(), "plan")
	})
}

// The Plan-phase prefetch, end to end: a memory this seat wrote reaches the
// prompt of the turn it bears on.
//
// The claim being tested is NOT "the block rendered" — the prefetch suite
// covers that against fakes. It is that a real node, with a real store,
// resolves the seat, reads its diary, runs the filter on the seat's own
// auxiliary model and puts the result in front of the planner. Every one of
// those is a wire that was not connected before.

// remember writes a memory as this seat's own, the way the persist decider
// does after a turn.
func remember(t *testing.T, n *node, content string) {
	t.Helper()
	seat, ok := n.engine.Registry().ByHandle("ceo")
	if !ok {
		t.Fatal("no CEO seat")
	}
	diary := learning.NewDiary(n.engine.Backends().Store)
	err := diary.Write(t.Context(), learning.DiaryEntry{
		ID: "mem-" + content[:4], AgentID: seat.AgentID.String(),
		Kind: learning.DiaryLong, Content: content,
		Source: "test", CreatedAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatalf("write memory: %v", err)
	}
}

// planPrompt is the system prompt of the call that planned.
func planPrompt(t *testing.T, n *node) string {
	t.Helper()
	phases, systems := n.model.seen(), n.model.systemPrompts()
	for i, phase := range phases {
		if phase == "plan" && i < len(systems) {
			return systems[i]
		}
	}
	t.Fatalf("no plan call was made; phases = %v", phases)
	return ""
}

func TestASeatsOwnMemoryReachesThePlanPrompt(t *testing.T) {
	n := start(t)
	waitForSeat(t, n, "ceo")
	remember(t, n, "always use semantic commit messages on this repository")

	n.wake(t, "ceo", "How did the week go?")
	waitForTurn(t, n)

	system := planPrompt(t, n)
	if !strings.Contains(system, "## Personal memory") {
		t.Fatalf("the plan prompt has no memory section:\n%s", tail(system))
	}
	// The scripted model answers every auxiliary call with the same
	// canned response, so what reaches the prompt is whichever memory the
	// filter's answer selected — the point is that the seat's OWN store
	// was read and the result was rendered, not which one it picked.
	if !strings.Contains(system, "semantic commit") &&
		!strings.Contains(system, "no stored memories surfaced") {
		t.Fatalf("neither the memory nor the empty hint rendered:\n%s", tail(system))
	}
}

// A FRESH SEAT gets no memory section at all — not an empty one. A heading
// with nothing under it tells the planner it has a memory it cannot read.
func TestAFreshSeatGetsNoMemorySection(t *testing.T) {
	n := start(t)
	waitForSeat(t, n, "ceo")

	n.wake(t, "ceo", "How did the week go?")
	waitForTurn(t, n)

	if system := planPrompt(t, n); strings.Contains(system, "## Personal memory") {
		t.Fatalf("a seat with nothing stored got a memory section:\n%s", tail(system))
	}
}

// A SEAT THAT ONBOARDS ON THIS VERY TURN IS NOT THEN TOLD TO ONBOARD.
//
// The prefetch is frozen at turn start and the onboarding pass runs after
// it, so the hint is rendered against a seat that has not onboarded YET —
// which is true at that instant and false by the time Plan reads it. Without
// the suppression the first turn of every seat's life ends with the planner
// being told to go and read the pages it has just finished reading.
func TestASeatThatJustOnboardedIsNotToldToOnboard(t *testing.T) {
	n := start(t)
	waitForSeat(t, n, "ceo")

	n.wake(t, "ceo", "How did the week go?")
	waitForTurn(t, n)

	// The pass really did run — otherwise this asserts the absence of a
	// hint that was never going to be there.
	if phases := n.model.seen(); !slices.Contains(phases, "onboarding") {
		t.Fatalf("the onboarding pass did not run; phases = %v", phases)
	}
	if system := planPrompt(t, n); strings.Contains(system, "## First-turn onboarding") {
		t.Fatalf("a seat that just onboarded was nagged:\n%s", tail(system))
	}
}

// tail is the last of a long prompt, for a failure message that is readable.
func tail(s string) string {
	if len(s) <= 1200 {
		return s
	}
	return "…" + s[len(s)-1200:]
}
