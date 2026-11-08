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
// turn was lost with its executor's box or host, which nobody saw end: either
// way the row is no account of all the turn did, and every reader is told — a
// note in a reader's words, shown through the row's one shown form, so the
// newer capture it names as not kept is the one that stands on the row and
// its reason is redacted; and the view the conversation's next turn continues
// the session by ([executordomain.Session.Kept]) carries it, with when the
// session was kept, so that turn is told where the session ends. A row that
// names no session, a turn still working and one that ended on its last word
// say nothing of the kind.
//
// Mutation: drop the lost outcome from WentOn, and the lost row reads as its
// turn's whole session; name the unkept capture as the end did rather than as
// it stands, and the row's own capture is told as not kept; tell a lost turn
// the definite note, and it is said to have gone on past a session it may not
// have; leave WentOn, Lost or CapturedAt out of Kept, and the next turn is told
// the session runs through its turn's end.
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
	for _, c := range []struct {
		name         string
		row          executordomain.Session
		wentOn, lost bool
		says, never  []string
	}{
		{name: "an end that said its turn went on past it, beside a newer capture not kept",
			row: row(object, &executorkeep.Ending{Outcome: executorkeep.OutcomeStopped, WentOn: true,
				Unkept: &executorkeep.Unkept{LaunchSeq: 1, CaptureSeq: 2, Reason: said}}),
			wentOn: true,
			says: []string{"kept up to a session taken while the turn still worked", "the turn went on past it",
				"what it did after is not in it", "its newer capture (launch 1, capture 2) was not kept: ",
				redact.Marker},
			never: []string{plantedToken, "may not hold"}},
		{name: "an end that said so, the capture it names unkept the row's own",
			row: row(object, &executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted, WentOn: true,
				Unkept: &executorkeep.Unkept{LaunchSeq: 1, CaptureSeq: 1, Reason: said}}),
			wentOn: true, says: []string{"the turn went on past it"}, never: []string{"not kept", plantedToken}},
		{name: "a turn that was lost",
			row:    row(object, &executorkeep.Ending{Outcome: executorkeep.OutcomeLost}),
			wentOn: true, lost: true,
			says: []string{"its turn may have gone on past", "lost with its executor's box or host",
				"may not hold the last of what it did"},
			never: []string{"taken while the turn still worked"}},
		{name: "a turn that was lost and whose end said it went on past the session",
			row:    row(object, &executorkeep.Ending{Outcome: executorkeep.OutcomeLost, WentOn: true}),
			wentOn: true, lost: true, says: []string{"taken while the turn still worked"},
			never: []string{"may have gone on past"}},
		{name: "a turn that ended on its last word",
			row: row(object, &executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted})},
		{name: "a row whose session went since",
			row: row(objstore.Object{}, &executorkeep.Ending{Outcome: executorkeep.OutcomeStopped, WentOn: true})},
		{name: "a turn still working", row: row(object, nil)},
	} {
		shown := c.row.Redacted()
		note := shown.WentOnNote()
		if shown.WentOn() != c.wentOn || (note != "") != c.wentOn {
			t.Errorf("%s: the row reads went on %v with the note %q, want %v", c.name, shown.WentOn(), note,
				c.wentOn)
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
		if kept.WentOn != c.wentOn || kept.Lost != c.lost || !kept.CapturedAt.Equal(at) {
			t.Errorf("%s: the next turn's view of the row went on %v, lost %v, kept at %v; want %v, %v, %v",
				c.name, kept.WentOn, kept.Lost, kept.CapturedAt, c.wentOn, c.lost, at)
		}
	}
}
