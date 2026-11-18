package cache

import (
	"strings"
	"testing"
)

// A FINDING FOUND AGAIN STAYS AS IT WAS FOUND, AND ONLY NEWER DAMAGE REPLACES
// IT (recordCheck). A file failing at the identity the repair mark names is
// the same damage: the mark's finding stands, a scrub's above all
// (scrubWaits), and the round is handed it as held, under an admitted check
// this check's files alone. At another identity it is newer damage: were the
// old finding kept, the repair would act on neither (checked.still) and the
// round would serve the file. TestAFindingFoundAgainStaysAsItWasFound covers
// a request's whole repair.
//
// Mutations: a check's finding replaces what the mark holds (rows 1, 2, 4);
// the mark keeps what it holds (row 3); under a check, the round is handed
// this check's own findings as found (row 4).
func TestOnlyDamageAtAnotherIdentityReplacesAFinding(t *testing.T) {
	t.Parallel()
	// The build's declarations, as Open takes them: a finding's file is
	// told apart by them from a run of the index of pushes, whose finding
	// the backfill's look reads (backfillReads).
	im, err := newImage(DerivedFiles())
	if err != nil {
		t.Fatalf("the build's declarations: %v", err)
	}
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
			handed := (&Cache{image: im}).recordCheck(e, inc, 1, []checkedFile{tc.found})
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
