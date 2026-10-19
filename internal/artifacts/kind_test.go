package artifacts

import (
	"slices"
	"testing"
)

// EVERY ROW ANSWERS EVERY FACET with a value this build knows: a zero facet is
// a question a surface would answer with its own default, which is the switch
// the table exists to remove.
func TestEveryKindAnswersEveryFacet(t *testing.T) {
	t.Parallel()
	specs := KindSpecs()
	if len(specs) == 0 {
		t.Fatal("the kind table is empty")
	}
	for _, s := range specs {
		switch {
		case !s.Kind.Valid():
			t.Errorf("%q is a row and not a valid kind", s.Kind)
		case s.Summary == "":
			t.Errorf("%s has no summary for the agent tools to list", s.Kind)
		case !s.Render.Valid():
			t.Errorf("%s renders %q", s.Kind, s.Render)
		case !s.Store.Valid():
			t.Errorf("%s keeps store %q", s.Kind, s.Store)
		case !s.Format.Valid():
			t.Errorf("%s validates format %q", s.Kind, s.Format)
		case s.Entry.Fixed && len(s.Entry.Defaults) == 0:
			t.Errorf("%s fixes its entry's path and names none", s.Kind)
		case s.Search == SearchFormat && s.Format == FormatNone:
			t.Errorf("%s searches its format's text and validates no format", s.Kind)
		case !s.Search.Valid():
			t.Errorf("%s searches by %q", s.Kind, s.Search)
		case !s.Documents.Valid():
			t.Errorf("%s serves documents by %q", s.Kind, s.Documents)
		case !s.Format.Valid():
			t.Errorf("%s validates by format %q", s.Kind, s.Format)
		}
	}
	if !slices.Equal(slices.Compact(slices.Clone(Kinds)), Kinds) ||
		len(Kinds) != len(specs) {
		t.Errorf("Kinds %q are not the table's rows, once each", Kinds)
	}
}

// ONLY A FRAMED KIND IS SERVED ON THE ARTIFACT ORIGIN OR DECLARES A
// CAPABILITY: a native kind has no page to grant one to, and a kind this build
// does not serve is never framed.
func TestOnlyAFramedKindRunsAPage(t *testing.T) {
	t.Parallel()
	for _, s := range KindSpecs() {
		caps := s.Capabilities()
		switch {
		case s.Framed() && !slices.Equal(caps, Capabilities):
			t.Errorf("framed %s may declare %q, not every capability", s.Kind, caps)
		case !s.Framed() && len(caps) > 0:
			t.Errorf("native %s may declare %q", s.Kind, caps)
		case s.Kind.Framed() != s.Framed():
			t.Errorf("%s.Framed() disagrees with its row", s.Kind)
		case s.Framed() == (s.Documents == DocumentsNone):
			t.Errorf("%s renders %s and serves documents by %s: only a framed kind, and "+
				"every framed kind, has documents on the artifact origin", s.Kind, s.Render,
				s.Documents)
		case s.OpensEntry() && !s.Entry.Admits("text/html"):
			t.Errorf("%s opens its frame on its entry, which is never a page", s.Kind)
		}
	}
	if Kind("whiteboard").Framed() {
		t.Error("a kind this build does not serve is framed")
	}
}

// THE TABLE IS NOT SHARED STATE: a caller that edits its copy of a row edits
// nothing anyone else reads.
func TestASpecIsACopy(t *testing.T) {
	t.Parallel()
	s, ok := KindHTML.Spec()
	if !ok {
		t.Fatal("html has no row")
	}
	s.Entry.Essences[0] = "text/plain"
	s.Entry.Defaults[0] = "page.txt"
	again, _ := KindHTML.Spec()
	if again.Entry.Essences[0] != "text/html" || again.Entry.Defaults[0] != "index.html" {
		t.Fatalf("editing a copy changed the table: %+v", again.Entry)
	}
}
