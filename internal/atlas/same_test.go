package atlas_test

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
)

// A PERSON'S "THE SAME" JOINS TWO ENTRIES NO KEY JOINS: a declared server and
// the cloud instance it is, whose private addresses never merge anything.
func TestAPersonsTheSameJoinsTwoEntries(t *testing.T) {
	t.Parallel()
	m := must(t)
	web := atlas.MustRef(atlas.KindHost, "ssh/web-1")
	cloud := atlas.MustRef(atlas.KindHost, "aws/123456789012/eu-west-1/i-0abc")
	claims := []atlas.Claim{
		m(atlas.Exists(web, settings, now)), m(atlas.Says(web, atlas.FactAddress, "10.0.0.5", settings, now)),
		m(atlas.Exists(cloud, aws, now)), m(atlas.Says(cloud, atlas.FactAddress, "10.0.0.5", aws, now)),
	}
	if v := atlas.Fold(claims, now); len(v.Entries) != 2 {
		t.Fatalf("%d entries before anybody joined them, want 2: a private address merges nothing", len(v.Entries))
	}
	same := m(atlas.Same(cloud, web, ada, now.Add(-time.Hour)))
	v := atlas.Fold(append(claims, same), now)
	if len(v.Entries) != 1 {
		t.Fatalf("%d entries after a person said they are the same, want 1", len(v.Entries))
	}
	if e := entry(t, v, cloud); len(e.Members) != 2 {
		t.Errorf("the joined entry holds %v, want both members", e.Members)
	}
	undo := m(atlas.Denial(same, ada, now.Add(-time.Minute)))
	if v := atlas.Fold(append(claims, same, undo), now); len(v.Entries) != 2 {
		t.Errorf("%d entries after the person took it back, want 2", len(v.Entries))
	}
}

// "NOT THE SAME" ALWAYS WINS over "the same", whoever said either and in
// whichever order.
func TestNotTheSameWinsOverTheSame(t *testing.T) {
	t.Parallel()
	m := must(t)
	web := atlas.MustRef(atlas.KindHost, "ssh/web-1")
	cloud := atlas.MustRef(atlas.KindHost, "aws/123456789012/eu-west-1/i-0abc")
	grace := atlas.PersonSource("grace")
	base := []atlas.Claim{m(atlas.Exists(web, settings, now)), m(atlas.Exists(cloud, aws, now))}
	for name, pair := range map[string][2]atlas.Claim{
		"the same said last":     {m(atlas.Distinct(web, cloud, ada, now.Add(-time.Hour))), m(atlas.Same(cloud, web, grace, now))},
		"the same said first":    {m(atlas.Same(cloud, web, grace, now.Add(-time.Hour))), m(atlas.Distinct(web, cloud, ada, now))},
		"one person, both words": {m(atlas.Same(web, cloud, ada, now)), m(atlas.Distinct(cloud, web, ada, now))},
	} {
		if v := atlas.Fold(append(append([]atlas.Claim{}, base...), pair[0], pair[1]), now); len(v.Entries) != 2 {
			t.Errorf("%s: %d entries, want 2: a person's \"not the same\" always wins", name, len(v.Entries))
		}
	}
}

// ONLY A PERSON SAYS TWO ENTRIES ARE THE SAME, and only of two entries of one
// kind: a merge is a person's decision, never an inference.
func TestOnlyAPersonSaysTwoEntriesAreTheSame(t *testing.T) {
	t.Parallel()
	web := atlas.MustRef(atlas.KindHost, "ssh/web-1")
	cloud := atlas.MustRef(atlas.KindHost, "aws/123456789012/eu-west-1/i-0abc")
	api := atlas.MustRef(atlas.KindService, "api")
	if _, err := atlas.Same(web, cloud, sre, now); err == nil {
		t.Error("an agent said two entries are the same")
	}
	if _, err := atlas.Same(web, cloud, aws, now); err == nil {
		t.Error("an integration said two entries are the same")
	}
	if _, err := atlas.Same(web, api, ada, now); err == nil {
		t.Error("a server and a service were said to be the same")
	}
	if _, err := atlas.Same(web, web, ada, now); err == nil {
		t.Error("an entry was said to be the same as itself")
	}
}

// EACH PAIR IS ITS OWN SLOT: "not the same as one entry" does not replace
// "not the same as another", so a store keeps both.
func TestEachPairIsItsOwnSlot(t *testing.T) {
	t.Parallel()
	m := must(t)
	web := atlas.MustRef(atlas.KindHost, "ssh/web-1")
	one := atlas.MustRef(atlas.KindHost, "ssh/web-2")
	two := atlas.MustRef(atlas.KindHost, "ssh/web-3")
	a, b := m(atlas.Distinct(web, one, ada, now)), m(atlas.Distinct(web, two, ada, now))
	if a.Slot() == b.Slot() {
		t.Errorf("two pairs share the slot %q", a.Slot())
	}
	if s := m(atlas.Same(web, one, ada, now)); s.Slot() == a.Slot() {
		t.Errorf("the same and not the same share the slot %q", a.Slot())
	}
	if d := m(atlas.Denial(a, ada, now)); d.Slot() != a.Slot() || d.Source != a.Source {
		t.Error("a denial is not on the slot and the voice of the claim it takes back")
	}
}

// THE FACETS ARE A CLOSED SET, as every vocabulary of the atlas is.
func TestEveryFacetKindIsClosed(t *testing.T) {
	t.Parallel()
	for _, k := range atlas.FacetKinds {
		if !k.Valid() {
			t.Errorf("facet %s is listed and not valid", k)
		}
	}
	if atlas.FacetKind("merge").Valid() || atlas.FacetKind("").Valid() {
		t.Error("a facet outside the set is valid")
	}
}
