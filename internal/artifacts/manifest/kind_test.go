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
			if !ok || !s.Entry.Admits(typ.Essence()) {
				t.Errorf("%s defaults its entry to %q, which its own rule does not admit", s.Kind, d)
			}
		}
	}
}

// INFERENCE NAMES ONE KIND OR NONE: a default entry infers its own kind by its
// path, a type exactly one kind opens on infers that kind, and a type two kinds
// open on infers nothing, so a publish that names no kind becomes the kind
// whose default it used and is never picked by the table's order.
func TestAnEntryInfersExactlyOneKind(t *testing.T) {
	t.Parallel()
	opens := map[string][]artifacts.Kind{}
	for _, s := range artifacts.KindSpecs() {
		for _, e := range s.Entry.Essences {
			opens[e] = append(opens[e], s.Kind)
		}
		for _, d := range s.Entry.Defaults {
			typ, _ := mediatype.For(d)
			if got, ok := artifacts.KindFor(d, typ.Essence()); !ok || got != s.Kind {
				t.Errorf("%s's default entry %q infers %q", s.Kind, d, got)
			}
		}
	}
	for essence, kinds := range opens {
		got, ok := artifacts.KindFor("elsewhere/entry", essence)
		switch {
		case len(kinds) == 1 && (!ok || got != kinds[0]):
			t.Errorf("%s opens only %s and infers %q", essence, kinds[0], got)
		case len(kinds) > 1 && ok:
			t.Errorf("%s opens %q and infers %q by the table's order", essence, kinds, got)
		}
	}
	if k, ok := artifacts.KindFor("report.pdf", "application/pdf"); ok {
		t.Errorf("a PDF entry infers %q, and no kind opens on one", k)
	}
}
