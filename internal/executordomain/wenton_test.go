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

// A ROW KEPT UP TO A SESSION ITS TURN WENT PAST SAYS SO, and so does a lost
// one. Its end said the turn went on past the session the row names, or the
// turn was lost, which no frame of it saw end: either way the row is no account
// of all the turn did, and a reader is told — a note in a reader's words, asked
// of the row's one shown form, so the newer capture it names as not kept is the
// one that stands on the row and its reason is redacted. And the view the
// conversation's next turn continues the session by
// ([executordomain.Session.Kept]) carries the two facts apart — what the end
// said, and whether the turn was lost — with when the session was kept, so
// that turn is told where the session ends, and of a lost turn only what its
// end can say. A row that names no session, a turn still working and one that
// ended on its last word say nothing of the kind.
//
// THE END'S WORD IS HELD TO THE ROW'S PLACE. An end names the turn's last word
// where it knows it ([executorkeep.Ending.LastWord]), and a row whose place is
// that last word's — its final capture, which the run's record held refused and
// the log took before the end all the same — holds all the turn did: it reads
// as no row its turn went on past, says no note of it, is handed the next turn
// as one through that turn, and is shown to every reader without went_on. A
// row older than the last word named, of its own launch or an earlier one, is
// one its turn went on past; and a lost turn's row at its last word is told
// only what a lost turn's end can say. Whichever form of the row a reader asks
// — as the row holds it, or as every reader is shown it, whose went_on is
// already the end's word held to the row's place — it is told the same: the
// row's own note, redacted, is its shown form's.
//
// Mutation: drop the lost outcome from WentOn, and the lost row reads as its
// turn's whole session; name the unkept capture as the end did rather than as
// it stands, and the row's own capture is told as not kept; tell a lost turn
// the definite note, and it is said to have gone on past a session it may not
// have; join the two facts in Kept, and a lost turn's next turn is told its
// session was kept while it still worked; leave WentOn, Lost or CapturedAt out
// of Kept, and the next turn is told the session runs through its turn's end.
// Read went_on off the ending alone (endWentPast ignoring the last word), and
// the row at its turn's last word reads, is noted, is handed on and is shown as
// one its turn went on past; take every row as its last word's once one is
// named, or hold the place to it at or after it, and the rows older than it, or
// at it, read wrong; compare its launch or its capture alone, and a row older
// than a later capture of its own launch, or than a later launch's, reads as
// its turn's last word; choose the note, hand the row on, or show it by the
// end's own went_on, and the lost row's note as the row holds it, the next turn
// or the shown form at the last word says the turn went on past it.
func TestARowKeptUpToASessionItsTurnWentPastSaysSo(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 11, 8, 9, 0, 0, 0, time.UTC)
	object := objstore.Object{Key: objstore.KeyAt(at), Hash: objstore.Hash(strings.Repeat("a", 64)), Size: 4096}
	row := func(kept objstore.Object, ending *executorkeep.Ending) executordomain.Session {
		s := executordomain.Session{Handle: "ana", Turn: "0193f0c3-0d5e-7a14-8b2f-61c9e2d0a7f3",
			ConversationKey: "chat:room", Object: kept, LaunchSeq: 1, CaptureSeq: 1, CapturedAt: at,
			Ending: ending}
		if ending != nil {
			s.EndedAt = at.Add(time.Hour)
		}
		return s
	}
	said := "the upload was refused while it read " + plantedToken
	placed := func(launch, capture int64) *executorkeep.CapturePlace {
		return &executorkeep.CapturePlace{LaunchSeq: launch, CaptureSeq: capture}
	}
	for _, c := range []struct {
		name                  string
		row                   executordomain.Session
		wentOn, endSaid, lost bool
		// shownSaid is the went_on every reader is shown on the row's
		// ending: the end's word held to the row's place, whether or not
		// the row still names a session.
		shownSaid   bool
		says, never []string
	}{
		{name: "an end that said its turn went on past it, beside its last word not kept",
			row: row(object, &executorkeep.Ending{Outcome: executorkeep.OutcomeStopped, WentOn: true,
				Unkept: &executorkeep.Unkept{LaunchSeq: 1, CaptureSeq: 2, Reason: said}, LastWord: placed(1, 2)}),
			wentOn: true, endSaid: true, shownSaid: true,
			says: []string{"kept up to a session taken while the turn still worked", "the turn went on past it",
				"what it did after is not in it", "its newer capture (launch 1, capture 2) was not kept: ",
				redact.Marker},
			never: []string{plantedToken, "may not hold"}},
		{name: "an end that said so, the capture it names unkept the row's own and no last word",
			row: row(object, &executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted, WentOn: true,
				Unkept: &executorkeep.Unkept{LaunchSeq: 1, CaptureSeq: 1, Reason: said}}),
			wentOn: true, endSaid: true, shownSaid: true, says: []string{"the turn went on past it"},
			never: []string{"not kept", plantedToken}},
		{name: "an end that said so, the last word it names the row's own",
			row: row(object, &executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted, WentOn: true,
				Unkept: &executorkeep.Unkept{LaunchSeq: 1, CaptureSeq: 1, Reason: said}, LastWord: placed(1, 1)})},
		{name: "an end that said so, the last word it names a later launch's",
			row: row(object, &executorkeep.Ending{Outcome: executorkeep.OutcomeFailed, WentOn: true,
				Unkept: &executorkeep.Unkept{LaunchSeq: 2, CaptureSeq: 1, Reason: said}, LastWord: placed(2, 1)}),
			wentOn: true, endSaid: true, shownSaid: true,
			says:  []string{"the turn went on past it", "its newer capture (launch 2, capture 1) was not kept: "},
			never: []string{plantedToken}},
		{name: "a turn that was lost",
			row:    row(object, &executorkeep.Ending{Outcome: executorkeep.OutcomeLost}),
			wentOn: true, lost: true,
			says: []string{"its turn may have gone on past", "the turn was lost",
				"may not hold the last of what it did"},
			never: []string{"taken while the turn still worked", "box or host"}},
		{name: "a turn that was lost and whose end said it went on past the session",
			row:    row(object, &executorkeep.Ending{Outcome: executorkeep.OutcomeLost, WentOn: true}),
			wentOn: true, endSaid: true, lost: true, shownSaid: true,
			says: []string{"taken while the turn still worked"}, never: []string{"may have gone on past"}},
		{name: "a turn that was lost, the last word its end names the row's own",
			row: row(object, &executorkeep.Ending{Outcome: executorkeep.OutcomeLost, WentOn: true,
				Unkept: &executorkeep.Unkept{LaunchSeq: 1, CaptureSeq: 1, Reason: said}, LastWord: placed(1, 1)}),
			wentOn: true, lost: true,
			says:  []string{"its turn may have gone on past", "may not hold the last of what it did"},
			never: []string{"taken while the turn still worked", "not kept"}},
		{name: "a turn that ended on its last word",
			row: row(object, &executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted})},
		{name: "a row whose session went since", shownSaid: true,
			row: row(objstore.Object{}, &executorkeep.Ending{Outcome: executorkeep.OutcomeStopped, WentOn: true})},
		{name: "a turn still working", row: row(object, nil)},
	} {
		shown := c.row.Redacted()
		note := shown.WentOnNote()
		if shown.WentOn() != c.wentOn || (note != "") != c.wentOn {
			t.Errorf("%s: the row reads went on %v with the note %q, want %v", c.name, shown.WentOn(), note,
				c.wentOn)
		}
		if c.row.WentOn() != shown.WentOn() {
			t.Errorf("%s: the row reads went on %v, and its shown form %v", c.name, c.row.WentOn(), shown.WentOn())
		}
		if own := redact.Secrets(c.row.WentOnNote()); own != note {
			t.Errorf("%s: the row as it is held is told %q, and its shown form %q", c.name, own, note)
		}
		if shown.Ending != nil && shown.Ending.WentOn != c.shownSaid {
			t.Errorf("%s: every reader is shown went_on %v, want %v", c.name, shown.Ending.WentOn, c.shownSaid)
		}
		for _, want := range c.says {
			if !strings.Contains(note, want) {
				t.Errorf("%s: the note %q does not say %q", c.name, note, want)
			}
		}
		for _, not := range c.never {
			if strings.Contains(note, not) {
				t.Errorf("%s: the note %q says %q", c.name, note, not)
			}
		}
		kept := c.row.Kept()
		if kept.WentOn != c.endSaid || kept.Lost != c.lost || !kept.CapturedAt.Equal(at) {
			t.Errorf("%s: the next turn's view of the row went on %v, lost %v, kept at %v; want %v, %v, %v",
				c.name, kept.WentOn, kept.Lost, kept.CapturedAt, c.endSaid, c.lost, at)
		}
	}
}
