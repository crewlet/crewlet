package executordomain

import "github.com/crewlet/crewlet/internal/redact"

// Redacted is the row as every reader is shown it: what the turn was asked,
// and the ending's prose — what the reviewer said landed, what blocked a
// round, the reason a person gave for a stop, the engine's account of a
// failure, and why the turn's newest capture was not kept — each passed
// through [redact.Secrets], and every other field exactly as the row holds it.
//
// ONE REDACTION FOR EVERY READER of a row — a person's read surface, a seat's
// own reads of its sessions, a search of them — so a field somebody's words
// reach is redacted on all of them or on none: a second copy is how one
// surface hands out the field its neighbour redacts. Those six are the row's
// words that somebody else wrote, and each can quote a credential a run met:
// the ask is a person's or a colleague's message, the prose the reviewer's,
// the executor's or a person's, and a failure's account and an unkept
// capture's reason quote the tool or the error they tripped on, which a box or
// a store wrote. The rest are the engine's own: identifiers, a conversation's
// key as its surface names it, the harness's name and its release, which the
// sandbox holds to digits and dots, instants, ordinals and the object's name.
//
// OVER WORDS THE WRITE ALREADY REDACTED, and redacted again here: a record
// keeps somebody's words redacted before they are cut to its bounds
// (executorkeep.Excerpt), since a credential the bound runs across, cut first,
// leaves a head no rule recognises; a reader redacts them again, which a rule
// added since the record was written reaches.
//
// A COPY: the ending is the row's own pointer, so it is copied before its
// prose is replaced, and the row this is called on keeps what it was read
// with.
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
	if u := ending.Unkept; u != nil {
		unkept := *u
		unkept.Reason = redact.Secrets(unkept.Reason)
		ending.Unkept = &unkept
	}
	s.Ending = &ending
	return s
}
