package executordomain

import (
	"slices"

	"github.com/crewlet/crewlet/internal/redact"
)

// Redacted is THE ONE FORM every reader is shown a row in (package doc, "What
// a reader is told of the session a row keeps"), so a field is redacted on
// every surface or on none. The fields of words somebody else wrote (the ask,
// the ending's four prose fields, the unkept capture's reason, each verdict's
// detail) pass through [redact.Secrets] again although the write redacted them
// (executorkeep.Excerpt), so a rule added since reaches them. The unkept
// capture and WentOn are shown as they stand on the row ([Session.Unkept],
// [Session.endWentPast]), never as an end decided off the run's record said
// them; the end's name of a summary is left off, the row's own
// ([Session.Summary]) being what a reader is told. The ending is copied first,
// so the receiver keeps what it was read with.
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
	// THE ROW SAYS WHETHER IT KEEPS A SUMMARY ([Session.Summary]), never the
	// end's name of one, which a release outlives.
	ending.Summary = nil
	s.Ending = &ending
	return s
}
