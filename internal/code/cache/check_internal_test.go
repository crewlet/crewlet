package cache

import (
	"strings"
	"testing"
)

// A FINDING FOUND AGAIN STAYS AS IT WAS FOUND, AND ONLY NEWER DAMAGE REPLACES
// IT (recordCheck). A check that finds a file failing at the identity a
// finding on the repair mark names has found the same damage: the finding the
// mark holds stands, whatever found it — a scrub's above all, which answers a
// request's scrub and covers a report while it waits (scrubWaits) — and so
// does what the round is handed to act on, the repair logged as what found the
// damage first; while a check of the repository is admitted the round is
// handed this check's files alone, each as the mark holds it. A file failing
// at ANOTHER identity is newer damage in a file put there since, which the
// finding the mark holds no longer names: kept, the round's repair acted on
// neither (checked.still acts only on a file still at its finding's identity),
// and the round served the file its own check had just found failing.
// TestAFindingFoundAgainStaysAsItWasFound is the same rule through a request's
// whole repair.
//
// Mutations: a check's finding replaces whatever the mark holds (the first,
// second and fourth rows); the mark keeps whatever it holds (the third); the
// round, under a check of the repository, is handed this check's own findings
// as it found them (the fourth).
func TestOnlyDamageAtAnotherIdentityReplacesAFinding(t *testing.T) {
	t.Parallel()
	rel := packFileRel(placeLive, strings.Repeat("5e", hashTrailer), ".rev")
	at := func(ino uint64) fileID { return fileID{present: true, dev: 1, ino: ino, size: 1068, modified: 1} }
	scrubbed := checkedFile{kind: fileRev, rel: rel, id: at(1), trigger: triggerReport, scrubbed: true}
	unvouched := checkedFile{kind: fileRev, rel: rel, id: at(1), trigger: triggerUnvouched}
	forced := func(id fileID) checkedFile {
		return checkedFile{kind: fileRev, rel: rel, id: id, trigger: triggerForced}
	}
	for _, tc := range []struct {
		name      string
		held      checkedFile
		verifying bool
		found     checkedFile
		want      checkedFile
	}{
		{"a scrub's finding found again by a forced check", scrubbed, false, forced(at(1)), scrubbed},
		{"a check's finding found again by a forced check", unvouched, false, forced(at(1)), unvouched},
		{"a scrub's finding, its file failing at another identity", scrubbed, false, forced(at(2)), forced(at(2))},
		{"a scrub's finding found again under a check of the repository", scrubbed, true, forced(at(1)), scrubbed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			e, inc := &entry{}, &incarnation{}
			if tc.verifying {
				e.verifying = 1
			}
			inc.repair.found = map[string]checkedFile{rel: tc.held}
			inc.repair.forces = 1
			handed := (&Cache{}).recordCheck(e, inc, 1, []checkedFile{tc.found})
			if have := inc.repair.found[rel]; len(inc.repair.found) != 1 || have != tc.want {
				t.Errorf("the repair mark holds %+v, want %+v alone", inc.repair.found, tc.want)
			}
			if len(handed) != 1 || handed[0] != tc.want {
				t.Errorf("the round is handed %+v to repair, want %+v", handed, tc.want)
			}
			if inc.repair.forces != 0 {
				t.Errorf("the check left %d forces on the mark, want its one spent", inc.repair.forces)
			}
		})
	}
}
