package manifest

import (
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/mediatype"
)

// A KIND'S ENTRY RULE IS ONE A PUBLISH CAN MEET: every default entry is a file
// the media table serves as a type the kind's own rule admits, a kind that must
// name an entry admits at least one type, and a kind whose content is its
// datastore needs none.
func TestEveryEntryRuleCanBeMet(t *testing.T) {
	t.Parallel()
	for _, s := range artifacts.KindSpecs() {
		if !s.Entry.Optional && len(s.Entry.Essences) == 0 {
			t.Errorf("%s must name an entry and admits no type", s.Kind)
		}
		if s.Store == artifacts.StoreContent && !s.Entry.Optional {
			t.Errorf("%s's content is its datastore and it must still name an entry", s.Kind)
		}
		for _, d := range s.Entry.Defaults {
			typ, ok := mediatype.For(d)
			if !ok || !s.Entry.AdmitsAt(d, typ.Essence()) {
				t.Errorf("%s defaults its entry to %q, which its own rule does not admit", s.Kind, d)
			}
		}
	}
}

// INFERENCE NAMES ONE KIND: no two kinds that take any path open on the same
// type, no two kinds fix the same path, and a default entry infers its own
// kind, so a publish that names no kind becomes the kind whose default it used.
func TestAnEntryInfersExactlyOneKind(t *testing.T) {
	t.Parallel()
	seen := map[string]artifacts.Kind{}
	fixed := map[string]artifacts.Kind{}
	for _, s := range artifacts.KindSpecs() {
		if s.Entry.Fixed {
			for _, d := range s.Entry.Defaults {
				if other, dup := fixed[d]; dup {
					t.Errorf("%s and %s both fix their entry at %s", other, s.Kind, d)
				}
				fixed[d] = s.Kind
			}
		} else {
			for _, e := range s.Entry.Essences {
				if other, dup := seen[e]; dup {
					t.Errorf("%s and %s both open on %s, so inference picks by order", other, s.Kind, e)
				}
				seen[e] = s.Kind
			}
		}
		for _, d := range s.Entry.Defaults {
			typ, _ := mediatype.For(d)
			if got, ok := artifacts.KindFor(d, typ.Essence()); !ok || got != s.Kind {
				t.Errorf("%s's default entry %q infers %q", s.Kind, d, got)
			}
		}
	}
	if k, ok := artifacts.KindFor("report.pdf", "application/pdf"); ok {
		t.Errorf("a PDF entry infers %q, and no kind opens on one", k)
	}
	if k, ok := artifacts.KindFor("data.json", "application/json"); ok {
		t.Errorf("a JSON file that is no kind's fixed entry infers %q", k)
	}
}
