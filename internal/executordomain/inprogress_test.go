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

// A live row naming a session reads as in progress, in the one sentence every
// surface carries ([executordomain.Session.InProgressNote]): not ended, the
// newest session its runs kept, captured when (in UTC), its run may have gone
// on past it, its last call may have no result yet. Never as an ended turn's
// session, nor as one saved as its run works. A live row naming no session is
// said by why; an ended row by how its turn left it (went-on note or none), and
// neither is told as the other. The engine's words alone, it reads alike held
// and shown; the went-on note's shown form redacts what refused a capture.
//
// Mutation: read InProgress off the object or the ending alone, answer either
// note of the other kind of row, or drop, mis-zone or zero-date the instant,
// and a case fails.
func TestALiveRowNamingASessionReadsAsInProgress(t *testing.T) {
	t.Parallel()
	// An instant outside UTC, as a node in another zone reads the broker's
	// clock: the sentence words it in UTC.
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
		Unkept: &executorkeep.Unkept{Loss: executorkeep.LossStoreRefused, LaunchSeq: 1, CaptureSeq: 2, Reason: "refused while it read " + plantedToken}}
	for _, c := range []struct {
		name string
		row  executordomain.Session
		// progress is the in-progress sentence ("" for none), and wentOn
		// whether the row is told its turn went on past its session.
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
		// A lost turn's row at its last word is still told it may not hold
		// the last of what the turn did, as every lost turn's row is.
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
