package pages_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/pages"
)

// The registry's contract, asserted where the type lives: a source that
// forgets a method is a compile error rather than a silent default.
var _ notify.Prompt = pages.Prompt{}

// A PAGE IS BOTH KEYS, AND THE ID IS BOTH OF THEM.
//
// The merge unit and the durable thread are one object here — a comment on a
// page is activity ON the page, not a thread of its own — so the identity
// delegates rather than deriving the address a second time. And it is the ID
// rather than the title on both sides: a rename would otherwise split the
// conversation in half, silently, each half looking ordinary.
func TestAPageIsBothThePartitionAndTheConversation(t *testing.T) {
	t.Parallel()
	renamed := map[string]string{
		pages.MetaPageID: "p-1", pages.MetaTitle: "Deploy runbook (2026)",
	}
	original := map[string]string{
		pages.MetaPageID: "p-1", pages.MetaTitle: "Deploy runbook",
	}
	for _, meta := range []map[string]string{original, renamed} {
		if got := (pages.Prompt{}).PartitionKey(meta, ""); got != "p-1" {
			t.Errorf("partition key = %q, want the page id", got)
		}
		if got := (pages.Prompt{}).ConversationIdentity(meta, ""); got != "p-1" {
			t.Errorf("conversation identity = %q, want the page id", got)
		}
	}
	// A wake naming no page derives NEITHER key, and both must agree about
	// that: one deriving a key where the other does not would let a turn be
	// recorded under an address no later change can reproduce.
	empty := map[string]string{pages.MetaTitle: "Deploy runbook"}
	if got := (pages.Prompt{}).PartitionKey(empty, "subject"); got != "" {
		t.Errorf("a page-less wake keyed on %q", got)
	}
	if got := (pages.Prompt{}).ConversationIdentity(empty, "subject"); got != "" {
		t.Errorf("a page-less wake resolved to %q", got)
	}
}
