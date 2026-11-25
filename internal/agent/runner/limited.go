package runner

import (
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/provenance"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// Limited is a run whose job met its model's usage limit before it did any
// work: no tool call and no output. Its round is relaunched once, fresh, on the
// next executor member that is not sitting out ([Resume.Limited]); a follow-up
// limited again is collected, and every member sitting out parks the seat.
type Limited struct {
	// BoxKept is whether the limited job's box is still there.
	BoxKept bool
}

// Reseed is what the fresh run carrying the round on is told beside its brief.
func (l Limited) Reseed() string {
	out := "## A run before you on this task could not start its work\n\n" +
		"Its model had reached its usage limit, so it did nothing. You run on another model, " +
		"in a new session: what earlier turns did reaches you through this brief.\n\n"
	if l.BoxKept {
		out += "If a working tree is in front of you, read `git status`, `git diff` and `git log` " +
			"before anything else.\n\n"
	}
	return out + "Do the task as it asks."
}

// A limited round continues no session (the next member may be another harness),
// and its words are the engine's own: it did no work for a later brief to retell.
func (l *Limited) session() string                         { return "" }
func (l *Limited) boxKept() bool                           { return l.BoxKept }
func (l *Limited) continuation(provenance.Boundary) string { return l.Reseed() }
func (l *Limited) reseed(provenance.Boundary) string       { return l.Reseed() }
func (l *Limited) tellsHeld() (id, tool string)            { return "", "" }
func (l *Limited) tellsCalled() []types.CallReturned       { return nil }
func (l *Limited) lead() *sandbox.Lead                     { return nil }
func (l *Limited) keeps() bool                             { return false }
func (l *Limited) branch() string                          { return "" }
func (l *Limited) refusal() error                          { return nil }
func (l *Limited) what() string {
	return "carrying on the round whose model met its usage limit before any work"
}

// isLimited reports whether a carried round is one a limit cut off before it began.
func isLimited(c carried) bool {
	_, limited := c.(*Limited)
	return limited
}
