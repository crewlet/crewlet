package builtin_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/colleague"
	"github.com/crewlet/crewlet/internal/worklet"
)

// AN AGENT'S MENTION IN A WORK ITEM COMMENT WAKES ONLY WHEN IT ASKS (ADR-0113).
// Measured on a live company, agents' status notes naming each other ("my
// approval still stands, nothing for @qa") woke the named agent, who replied
// naming the first back: about 25 turns on one item in 12 minutes. The named
// seat is still told; `asks: true` wakes it, and an agent must say which.
func TestAnAgentsCommentMentionWakesOnlyWhenItAsks(t *testing.T) {
	t.Parallel()
	const body = "My approval still stands, nothing for @eng or @jane."
	trk := newFakeTracker()
	roster := func() []colleague.Seat {
		return append(askable(), colleague.Seat{Handle: "jane", Name: "Jane", Kind: "human"})
	}
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as,
		Mentions: fakeMentions{"eng", "jane"}, Seats: roster})
	got := callWork(t, reg, builtin.CommentOnWorkTool, map[string]any{"item": "ENG-1", "body": body})
	if !got.Failed || !strings.Contains(got.Output, "needs `asks`") || len(trk.notified) != 0 {
		t.Fatalf("an agent's comment without asks answered %q and wrote %d, want it refused",
			got.Output, len(trk.notified))
	}
	// An agent asking a person in words is refused (ADR-0100), so the comment
	// that asks names the agent alone.
	for _, c := range []struct {
		asks  bool
		body  string
		named fakeMentions
	}{{false, body, fakeMentions{"eng", "jane"}}, {true, "Over to @eng for the merge.", fakeMentions{"eng"}}} {
		asks := c.asks
		reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as, Mentions: c.named, Seats: roster})
		got := callWork(t, reg, builtin.CommentOnWorkTool,
			map[string]any{"item": "ENG-1", "body": c.body, "asks": asks})
		if got.Failed {
			t.Fatalf("a comment with asks=%v failed: %s", asks, got.Output)
		}
		notify := trk.notified[len(trk.notified)-1]
		heard, told := worklet.CandidateFor(notify, "eng")
		if !told || heard.Reason != worklet.ReasonMention {
			t.Fatalf("asks=%v: eng was not told of the mention: %+v", asks, heard)
		}
		if heard.Wakes != asks || heard.Addressed != asks {
			t.Errorf("asks=%v: eng woken %v, asked %v; want both %v", asks, heard.Wakes,
				heard.Addressed, asks)
		}
		if person, _ := worklet.CandidateFor(notify, "jane"); !asks && !person.Addressed {
			t.Errorf("asks=%v: the person named is not addressed", asks)
		}
	}
}
