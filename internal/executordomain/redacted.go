package executordomain

import "github.com/crewlet/crewlet/internal/redact"

// Redacted is THE SHOWN FORM of a row, the one form every reader is shown it
// in, which does two things and nothing else. Its words somebody else wrote are
// redacted: what the turn was asked, and the ending's prose — what the reviewer
// said landed, what blocked a round, the reason a person gave for a stop, the
// engine's account of a failure, and why the turn's newest capture was not
// kept — each passed through [redact.Secrets]. And what the end said of the
// row's session is shown as it stands on the row: a capture the run's stamp
// called unkept but the row keeps is shown as kept, the ending naming its
// unkept capture only where that capture is newer than the row's own place
// ([Session.Unkept]); and an end that said its turn went on is shown saying so
// only where the row's session is older than the last word it named, or it
// named none ([Session.endWentPast]), so a row that holds its turn's last word
// is shown as one. Every other field is exactly as the row holds it, the
// ending's last word among them; what a reader is told of the session a row
// keeps — that it is in progress ([Session.InProgressNote]), or that its turn
// went on past it ([Session.WentOnNote]) — is asked of this form, so the
// unkept capture's reason the second quotes is the redacted one.
//
// THE UNKEPT CAPTURE AND WENT_ON AS THEY STAND ON THE ROW, never as the end
// said them, for every reader by the same token: an end is decided off the
// run's record, which can name as not kept a capture whose record the log took
// before the end, and that capture is then the row's own, kept — the turn's
// last word, where it was its last launch's final capture. A reader shown the
// end's word for it — a person's read surface, a seat's own read, a search's
// hit, the account a conversation's next turn is told — would tell the session
// the row keeps as one that was lost, or as one its turn went on past; and
// none of them is shown the row's place to hold the end's word to.
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
	ending.Unkept = s.Unkept()
	if ending.Unkept != nil {
		ending.Unkept.Reason = redact.Secrets(ending.Unkept.Reason)
	}
	ending.WentOn = s.endWentPast()
	s.Ending = &ending
	return s
}
