package executor

import (
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
)

// EVERY HARNESS THAT READS NOTES CAN CONFIRM A TAKE: by its hooks' receipts on
// its main thread, or by its plugin's own delivery event. One that reads notes
// but cannot confirm leaves its wakes unclaimed, run again as turns of their
// own, and the requester answered twice: such a harness takes no notes until it
// has a confirm path.
//
// Mutation: take OpenCode's inbox delivery, or Claude Code's hook receipts, away
// and this goes red; so does a harness that reads notes by hooks it cannot read
// back (the stand-in below).
func TestEveryHarnessThatReadsNotesCanConfirmATake(t *testing.T) {
	t.Parallel()
	for _, h := range harnesses {
		if _, reads := h.(noteReader); reads && !confirms(h) {
			t.Errorf("%s reads notes but can confirm no take: give it hook receipts or a poll of its own, "+
				"or take its notes away", h.Name())
		}
	}
	if confirms(unconfirmedHooks{}) {
		t.Error("a harness answered by hooks whose thread shows no receipts was taken for one that confirms")
	}
}

// unconfirmedHooks is Claude Code's hooks with a thread that shows no receipts.
type unconfirmedHooks struct{ ClaudeCode }

func (unconfirmedHooks) threadTraits() sandbox.ThreadTraits { return sandbox.ThreadTraits{} }
