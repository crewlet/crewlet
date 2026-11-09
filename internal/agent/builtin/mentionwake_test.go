package builtin_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/worklet"
)

// AN AGENT'S MENTION IN A WORK ITEM COMMENT WAKES ONLY WHEN IT ASKS (ADR-0113).
// Measured on a live company, agents' status notes naming each other ("my
// approval still stands, nothing for @qa") woke the named agent, who replied
// naming the first back: about 25 turns on one item in 12 minutes. The named
// seat is still told; `asks: true` wakes it, and an agent must say which.
func TestAnAgentsCommentMentionWakesOnlyWhenItAsks(t *testing.T) {
	t.Parallel()
	const body = "My approval still stands, nothing for @qa."
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as, Mentions: fakeMentions{"qa"}})
	got := callWork(t, reg, builtin.CommentOnWorkTool, map[string]any{"item": "ENG-1", "body": body})
	if !got.Failed || !strings.Contains(got.Output, "needs `asks`") || len(trk.notified) != 0 {
		t.Fatalf("an agent's comment without asks answered %q and wrote %d, want it refused",
			got.Output, len(trk.notified))
	}
	for _, asks := range []bool{false, true} {
		got := callWork(t, reg, builtin.CommentOnWorkTool,
			map[string]any{"item": "ENG-1", "body": body, "asks": asks})
		if got.Failed {
			t.Fatalf("a comment with asks=%v failed: %s", asks, got.Output)
		}
		notify := trk.notified[len(trk.notified)-1]
		heard, told := worklet.CandidateFor(notify, "qa")
		if !told || heard.Reason != worklet.ReasonMention {
			t.Fatalf("asks=%v: qa was not told of the mention: %+v", asks, heard)
		}
		if heard.Wakes != asks || heard.Addressed != asks {
			t.Errorf("asks=%v: qa woken %v, asked %v; want both %v", asks, heard.Wakes,
				heard.Addressed, asks)
		}
	}
}
