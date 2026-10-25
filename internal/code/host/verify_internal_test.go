package host

import (
	"testing"
	"time"

	"github.com/google/uuid"
)

// A NODE AGES WHAT IT KEPT WITHOUT WALKING IT: a sweep looks at the
// verifications that ended longest ago and stops at the first one still
// kept, so the cost of every verify asked of a node, and of every read of
// one, does not grow with the verifications it keeps — a sweep of every
// repository a node holds keeps one for each, and with a walk of the whole
// registry under its lock that sweep was quadratic in them. What it ages is
// what a walk would: every one that ended more than VerifyKept ago, the
// repository's latest pointer with it, and never one a newer verification
// of its repository replaced, which was dropped when it was.
//
// Mutation: sweep the whole map, and the count of what it looked at grows
// with what is kept.
func TestAVerifySweepLooksOnlyAtWhatItAges(t *testing.T) {
	t.Parallel()
	var v verifications
	start := time.Date(2026, 10, 14, 12, 0, 0, 0, time.UTC)
	const kept = 10_000
	ids := make([]uuid.UUID, kept)
	for i := range kept {
		ids[i] = uuid.Must(uuid.NewV7())
		repo := uuid.NewSHA1(uuid.NameSpaceOID, []byte{byte(i), byte(i >> 8)}).String()
		at := start.Add(time.Duration(i) * time.Millisecond)
		v.begin(&Verification{ID: ids[i], RepoID: repo, State: VerifyRunning, Started: at})
		v.end(ids[i], at, func(got *Verification) { got.State = VerifyDone })
	}
	if n := v.sweep(start.Add(VerifyKept)); n > 1 {
		t.Errorf("a sweep with nothing past VerifyKept looked at %d of %d kept verifications, want 1", n, kept)
	}
	// Half of them past VerifyKept: those are what it looks at, and one more.
	if n := v.sweep(start.Add(VerifyKept + kept/2*time.Millisecond)); n > kept/2+1 {
		t.Errorf("a sweep aging half looked at %d, want %d", n, kept/2+1)
	}
	if len(v.byID) != kept/2 || len(v.latest) != kept/2 {
		t.Errorf("after aging half the registry holds %d and %d latest, want %d", len(v.byID), len(v.latest),
			kept/2)
	}
	if _, ok := v.byID[ids[kept/2-1]]; ok {
		t.Error("a verification past VerifyKept was kept")
	}
	if _, ok := v.byID[ids[kept/2+1]]; !ok {
		t.Error("a verification within VerifyKept was dropped")
	}
	// A NEWER VERIFICATION OF A REPOSITORY replaces its last at once, and the
	// one replaced is passed over when the sweep reaches it.
	last := v.byID[ids[kept-1]]
	newer := &Verification{ID: uuid.Must(uuid.NewV7()), RepoID: last.RepoID, State: VerifyRunning,
		Started: start.Add(VerifyKept)}
	v.begin(newer)
	if _, ok := v.byID[ids[kept-1]]; ok || v.latest[last.RepoID] != newer.ID {
		t.Error("a newer verification of a repository did not replace its last")
	}
	v.sweep(start.Add(3 * VerifyKept))
	if _, ok := v.byID[newer.ID]; !ok || v.latest[last.RepoID] != newer.ID {
		t.Error("a sweep dropped a running verification")
	}
}
