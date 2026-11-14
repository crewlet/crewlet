package atlas_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/atlas"
)

// WHAT A REPOSITORY'S FILES DECLARE IS LIKELY, AND STAYS SO UNTIL A PERSON
// CONFIRMS IT: the entry and what is said of it read as likely and
// discovered, are shown to an agent and in a default read (they are backed by
// a file, unlike an agent's unbacked report), and the person's own claim on
// the same entry makes it confirmed.
//
// Mutation: derive verified for a code source in Source.Confidence, and what
// one file says reads as confirmed with nobody having confirmed it.
func TestWhatCodeDeclaresIsLikelyUntilAPersonConfirmsIt(t *testing.T) {
	t.Parallel()
	m := must(t)
	code := atlas.CodeSource("shop")
	svc := atlas.MustRef(atlas.KindService, "shop-api")
	db := atlas.MustRef(atlas.KindDatastore, "shop/db")
	repo := atlas.MustRef(atlas.KindRepository, "gitlet/shop")
	declared := []atlas.Claim{
		m(atlas.Exists(svc, code, now)),
		m(atlas.Relates(svc, atlas.RelationBuiltFrom, repo, code, now)),
		m(atlas.Relates(svc, atlas.RelationDependsOn, db, code, now)),
		m(atlas.Exists(db, code, now)),
		m(atlas.Says(db, atlas.FactEngine, "postgres", code, now)),
		m(atlas.Exists(repo, atlas.RecordSource(atlas.RecordGitlet), now)),
	}
	view := atlas.Fold(declared, now)
	for _, r := range []atlas.Ref{svc, db} {
		e := entry(t, view, r)
		if e.Word != atlas.WordLikely || e.State != atlas.StateDiscovered || e.Confidence != atlas.ConfidenceInferred {
			t.Errorf("%s reads %s, %s (%s); want likely and discovered", r, e.Word, e.State, e.Confidence)
		}
	}
	if e := entry(t, atlas.Vouched(view), svc); len(e.Relations) != 2 {
		t.Errorf("an agent's read shows %d of the service's 2 declared relations", len(e.Relations))
	}
	if shown := view.Select(atlas.Query{Kind: atlas.KindDatastore}); len(shown) != 1 {
		t.Errorf("a default read shows %d databases, want the declared one", len(shown))
	}
	confirmed := atlas.Fold(append(declared, m(atlas.Exists(svc, ada, now))), now)
	if e := entry(t, confirmed, svc); e.Word != atlas.WordConfirmed || e.State != atlas.StateConfirmed {
		t.Errorf("after a person's claim the service reads %s, %s; want confirmed", e.Word, e.State)
	}
}

// A FILE NEVER OUTVOTES WHAT WAS SEEN: where an integration's read and a file
// disagree on a fact, the integration's value stands and the entry is not
// sent to review over what a file says.
func TestAFileNeverContestsWhatAnIntegrationRead(t *testing.T) {
	t.Parallel()
	m := must(t)
	code := atlas.CodeSource("shop")
	db := atlas.MustRef(atlas.KindDatastore, "shop/db")
	view := atlas.Fold([]atlas.Claim{
		m(atlas.Exists(db, aws, now)),
		m(atlas.Says(db, atlas.FactEngineVersion, "16.4", aws, now)),
		m(atlas.Says(db, atlas.FactEngineVersion, "15", code, now.Add(1))),
	}, now.Add(2))
	e := entry(t, view, db)
	if len(e.Facts) != 1 || e.Facts[0].Value.Text != "16.4" || e.Facts[0].Standing != atlas.StandingSettled || e.Review {
		t.Errorf("the database reads %+v (review %v); want the integration's 16.4, settled", e.Facts, e.Review)
	}
}

// A REPOSITORY'S FILES DECIDE NOTHING A PERSON DECIDES, AND REMOVE NOTHING:
// the vocabulary refuses a fact of meaning, an owner, a denial and a "not the
// same" in the code voice.
func TestAFileNeverSetsMeaningOrDeniesAnything(t *testing.T) {
	t.Parallel()
	code := atlas.CodeSource("shop")
	svc := atlas.MustRef(atlas.KindService, "shop-api")
	other := atlas.MustRef(atlas.KindService, "shop-web")
	exists, err := atlas.Exists(svc, code, now)
	if err != nil {
		t.Fatal(err)
	}
	denied := exists
	denied.Denied = true
	for name, refused := range map[string]error{
		"a purpose":      errOf(atlas.Says(svc, atlas.FactPurpose, "Takes payments", code, now)),
		"an environment": errOf(atlas.Says(svc, atlas.FactEnvironment, "production", code, now)),
		"an owner":       errOf(atlas.Names(svc, atlas.RelationOwnedBy, atlas.ExternalUnit, "payments", code, now)),
		"not the same":   errOf(atlas.Distinct(svc, other, code, now)),
		"a denial":       denied.Validate(),
		"a turn":         errOf(atlas.Exists(svc, atlas.Source{Kind: atlas.SourceCode, Name: "shop", Turn: "t-1"}, now)),
		"no repository":  errOf(atlas.Exists(svc, atlas.CodeSource(""), now)),
	} {
		if refused == nil {
			t.Errorf("%s in the code voice is admitted", name)
		}
	}
	if _, err := atlas.Denial(exists, code, now); err == nil || !strings.Contains(err.Error(), "only a person") {
		t.Errorf("a file's denial: %v, want it refused as a person's alone", err)
	}
}

func errOf(_ atlas.Claim, err error) error { return err }
