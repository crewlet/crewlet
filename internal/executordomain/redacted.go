package executordomain

import (
	"slices"

	"github.com/crewlet/crewlet/internal/redact"
)

// Redacted is THE ONE FORM every reader is shown a row in (package doc, "What
// a reader is told of the session a row keeps"), so a field is redacted on
// every surface or on none. It changes two things. The fields of words
// somebody else wrote, any of which can quote a credential a run met (the ask,
// the ending's four prose fields, the unkept capture's reason, each verdict's
// detail), pass through [redact.Secrets] again although the write redacted
// them (executorkeep.Excerpt), so a rule added since reaches them. And the
// unkept capture and WentOn are shown as they stand on the row
// ([Session.Unkept], [Session.endWentPast]), never as the end said them: an end
// decided off the run's record can call unkept a capture the log took first,
// which the row keeps. The ending is copied first, so the receiver keeps what
// it was read with.
func (s Session) Redacted() Session {
	s.AskExcerpt = redact.Secrets(s.AskExcerpt)
	if s.Ending == nil {
		return s
	}
	ending := *s.Ending
	ending.CompletedWork = redact.Secrets(ending.CompletedWork)
	ending.BlockedOn = redact.Secrets(ending.BlockedOn)
	ending.StopReason = redact.Secrets(ending.StopReason)
	ending.Failure = redact.Secrets(ending.Failure)
	ending.Unkept = s.Unkept()
	if len(ending.NotContinued) > 0 {
		said := slices.Clone(ending.NotContinued)
		for i := range said {
			said[i].Detail = redact.Secrets(said[i].Detail)
		}
		ending.NotContinued = said
	}
	if ending.Unkept != nil {
		ending.Unkept.Reason = redact.Secrets(ending.Unkept.Reason)
	}
	ending.WentOn = s.endWentPast()
	s.Ending = &ending
	return s
}
