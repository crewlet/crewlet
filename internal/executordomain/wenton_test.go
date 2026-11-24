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

// A ROW KEPT UP TO A SESSION ITS TURN WENT PAST SAYS SO, and so does a lost one
// (package doc, "What a reader is told of the session a row keeps"): the note,
// asked of the row's shown form, names the unkept capture as it stands and
// redacts its reason; a lost turn is told only what its end can say; Kept holds
// the end's word and the loss apart, with CapturedAt. THE END'S WORD IS HELD TO
// THE ROW'S PLACE: a row at its LastWord holds all the turn did (no note, handed
// on and shown without went_on); an older one is one it went on past.
//
// Mutation: drop the lost outcome from WentOn; name the unkept capture as the
// end did; give a lost turn the definite note; join or drop WentOn, Lost or
// CapturedAt in Kept; read went_on off the ending alone, or compare launch or
// capture alone; or show the end's own went_on: a case goes red.
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
				Unkept: &executorkeep.Unkept{Loss: executorkeep.LossStoreRefused, LaunchSeq: 1, CaptureSeq: 2, Reason: said}, LastWord: placed(1, 2)}),
			wentOn: true, endSaid: true, shownSaid: true,
			says: []string{"kept up to a session taken while the turn still worked", "the turn went on past it",
				"what it did after is not in it", "its newer capture (launch 1, capture 2) was not kept: ",
				redact.Marker},
			never: []string{plantedToken, "may not hold"}},
		{name: "an end that said so, the capture it names unkept the row's own and no last word",
			row: row(object, &executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted, WentOn: true,
				Unkept: &executorkeep.Unkept{Loss: executorkeep.LossStoreRefused, LaunchSeq: 1, CaptureSeq: 1, Reason: said}}),
			wentOn: true, endSaid: true, shownSaid: true, says: []string{"the turn went on past it"},
			never: []string{"not kept", plantedToken}},
		{name: "an end that said so, the last word it names the row's own",
			row: row(object, &executorkeep.Ending{Outcome: executorkeep.OutcomeCompleted, WentOn: true,
				Unkept: &executorkeep.Unkept{Loss: executorkeep.LossStoreRefused, LaunchSeq: 1, CaptureSeq: 1, Reason: said}, LastWord: placed(1, 1)})},
		{name: "an end that said so, the last word it names a later launch's",
			row: row(object, &executorkeep.Ending{Outcome: executorkeep.OutcomeFailed, WentOn: true,
				Unkept: &executorkeep.Unkept{Loss: executorkeep.LossStoreRefused, LaunchSeq: 2, CaptureSeq: 1, Reason: said}, LastWord: placed(2, 1)}),
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
				Unkept: &executorkeep.Unkept{Loss: executorkeep.LossStoreRefused, LaunchSeq: 1, CaptureSeq: 1, Reason: said}, LastWord: placed(1, 1)}),
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
