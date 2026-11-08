package executordomain_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/executordomain"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/redact"
)

// A LIVE ROW NAMING A SESSION READS AS IN PROGRESS. A turn that has not ended
// and whose row names a session — the newest its runs have kept, a job's final
// capture its collection took while the turn goes on, or one a loss left its
// box to take — is read as a session in progress, in the one sentence every
// surface carries ([executordomain.Session.InProgressNote]): that the turn has
// not ended, that this is the newest session its runs have kept and when it
// was captured, in UTC as every instant the engine words is, that its run may
// have gone on past it and that its last call may have no result yet. Never as
// an ended turn's session, and never as one saved as its run works, which
// nothing on the row says. A live row naming no session yet is said by why it
// names none; an ended row by how its turn left it — the note of one its turn
// went on past, a lost turn's at its last word among them, or none — and a row
// in progress is never told as one its turn went on past, nor an ended row as
// one in progress. The sentence is the engine's words alone, so the row as held
// and its shown form, which every reader is shown, say it alike; the went-on
// note quotes what refused a capture, and its shown form is its redaction.
//
// Mutation: read InProgress off the row's object alone, and the ended rows read
// in progress; off the ending alone, and the live row naming no session does;
// answer InProgressNote of an ended row, and the ended rows say they have not
// ended; let WentOnNote answer of a live row, and the row in progress is told
// its turn went on past it; drop the instant, word it in the capture's own
// zone, or word a row's zero instant as a date, and a sentence reads wrong.
func TestALiveRowNamingASessionReadsAsInProgress(t *testing.T) {
	t.Parallel()
	// AN INSTANT OUTSIDE UTC, as the broker's clock is read on a node in
	// another zone: the sentence words it in UTC.
	at := time.Date(2026, 11, 8, 10, 30, 0, 0, time.FixedZone("CET", 3600))
	object := objstore.Object{Key: objstore.KeyAt(at), Hash: objstore.Hash(strings.Repeat("a", 64)), Size: 4096}
	row := func(kept objstore.Object, captured time.Time, ending *executorkeep.Ending) executordomain.Session {
		s := executordomain.Session{Handle: "ana", Turn: "0193f0c3-0d5e-7a14-8b2f-61c9e2d0a7f3",
			ConversationKey: "chat:room", Object: kept, LaunchSeq: 1, CaptureSeq: 1, CapturedAt: captured,
			AskExcerpt: "deploy with " + plantedToken, Ending: ending}
		if ending != nil {
			s.EndedAt = at.Add(time.Hour)
		}
		return s
	}
	const inProgress = "This turn has not ended: this is the newest session its runs have kept, captured " +
		"at 2026-11-08T09:30:00Z; its run may have gone on past it, and its last call may have no result yet."
	wentOn := &executorkeep.Ending{Outcome: executorkeep.OutcomeStopped, WentOn: true,
		Unkept: &executorkeep.Unkept{LaunchSeq: 1, CaptureSeq: 2, Reason: "refused while it read " + plantedToken}}
	for _, c := range []struct {
		name string
		row  executordomain.Session
		// progress is the row's in-progress sentence, "" for a row not in
		// progress, and wentOn whether it is told it is kept up to a session
		// its turn went on past.
		progress string
		wentOn   bool
	}{
		{name: "a live turn's row naming a session", row: row(object, at, nil), progress: inProgress},
		{name: "a live turn's row naming a session whose capture holds no instant",
			row: row(object, time.Time{}, nil),
			progress: "This turn has not ended: this is the newest session its runs have kept; its run may " +
				"have gone on past it, and its last call may have no result yet."},
		{name: "a live turn's row naming no session yet", row: row(objstore.Object{}, time.Time{}, nil)},
		{name: "an ended turn's row naming its last word",
			row: row(object, at, &executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted})},
		{name: "an ended turn's row naming a session it went on past", row: row(object, at, wentOn),
			wentOn: true},
		// A LOST TURN'S ROW AT ITS LAST WORD is told it may not hold the last
		// of what the turn did all the same, as every lost turn's row is.
		{name: "a lost turn's row at its last word", row: row(object, at, &executorkeep.Ending{
			Outcome: executorkeep.OutcomeLost, WentOn: true,
			LastWord: &executorkeep.CapturePlace{LaunchSeq: 1, CaptureSeq: 1}}), wentOn: true},
	} {
		for form, s := range map[string]executordomain.Session{"as held": c.row, "as shown": c.row.Redacted()} {
			said := s.InProgressNote()
			if s.InProgress() != (c.progress != "") || said != c.progress {
				t.Errorf("%s, %s: reads in progress %v with the sentence %q, want %q", c.name, form,
					s.InProgress(), said, c.progress)
			}
			if strings.Contains(strings.ToLower(said), "save") {
				t.Errorf("%s, %s: is told %q, which says the session is saved", c.name, form, said)
			}
			if went := s.WentOnNote(); (went != "") != c.wentOn || (said != "" && went != "") {
				t.Errorf("%s, %s: is told it went on past its session in %q, want %v", c.name, form, went,
					c.wentOn)
			}
		}
		if own, shown := redact.Secrets(c.row.WentOnNote()), c.row.Redacted().WentOnNote(); own != shown {
			t.Errorf("%s: the row as held is told %q, and its shown form %q", c.name, own, shown)
		}
		if c.wentOn && strings.Contains(c.row.Redacted().WentOnNote(), plantedToken) {
			t.Errorf("%s: its shown form is told %q, the unkept capture's reason unredacted", c.name,
				c.row.Redacted().WentOnNote())
		}
	}
	if begun := row(objstore.Object{}, time.Time{}, nil); begun.NotKept() !=
		"nothing of this turn's session has been captured yet" {
		t.Errorf("a live row naming no session says %q, want that nothing of it has been captured yet",
			begun.NotKept())
	}
}
