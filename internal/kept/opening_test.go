package kept_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/kept"
)

// THE OPENING A CUT TEXT KEEPS IS READ BACK EXACTLY: for ASCII, escaped,
// multi-byte and invalid texts, and one that holds the marker's own opening,
// cut at every budget from none to one byte short of the whole, Opening of
// what Text kept is the text's own opening, followed in what was kept by
// nothing but the marker for that cut. A text Text kept whole, one that only
// ends like a marker, and a marker whose figures are not the cut's, are
// refused.
//
// Mutation: skip re-checking the marker's figure, or take its first opening
// `…\n(` rather than its last, and a case reads back another opening or
// accepts a marker Text never wrote.
func TestTheOpeningACutTextKeepsIsReadBackExactly(t *testing.T) {
	t.Parallel()
	for name, text := range map[string]string{
		"ascii":   strings.Repeat("the build passed. ", 20),
		"escaped": strings.Repeat("<li>&amp;\"\\\n\x01</li>", 12),
		"runes":   strings.Repeat("日本語😀 ", 12),
		"invalid": strings.Repeat("a\xff\xc3", 40),
		"markers": strings.Repeat("ok…\n(3 bytes of 1 KiB not kept on the run's record)", 6),
	} {
		cuts := 0
		for budget := range kept.Quoted(text) {
			got, whole := run.Text(text, budget)
			if whole == 0 {
				t.Fatalf("%s: Text kept %d bytes whole within %d", name, len(text), budget)
			}
			opening, ok := run.Opening(got, whole)
			marker := "…\n(" + kept.Size(whole-len(opening)) + " of " + kept.Size(whole) +
				" not kept on the run's record)"
			switch {
			case !ok:
				t.Errorf("%s at %d: the opening of %q is not read back", name, budget, got)
			case !strings.HasPrefix(text, opening) || got != opening+marker:
				t.Errorf("%s at %d: read back %q from %q", name, budget, opening, got)
			}
			cuts++
		}
		if cuts == 0 {
			t.Fatalf("%s: no budget cut the text", name)
		}
		if _, ok := run.Opening(text, 0); ok {
			t.Errorf("%s: a text kept whole reads as a cut", name)
		}
	}
	cut, whole := run.Text(strings.Repeat("x", 4096), 1024)
	for name, refused := range map[string]struct {
		kept  string
		whole int
	}{
		"kept whole":              {kept: cut, whole: 0},
		"another whole":           {kept: cut, whole: 2 * whole},
		"another record":          {kept: strings.Replace(cut, "the run's record", "the turn's record", 1), whole: whole},
		"a figure not the cut's":  {kept: strings.Replace(cut, "…\n(3 KiB", "…\n(2 KiB", 1), whole: whole},
		"a figure of another cut": {kept: "a…\n(3 bytes of 5 bytes not kept on the run's record)", whole: 5},
		"no marker":               {kept: strings.Repeat("x", 1000), whole: whole},
	} {
		if opening, ok := run.Opening(refused.kept, refused.whole); ok {
			t.Errorf("%s: %q read back the opening %q", name, refused.kept, opening[:min(len(opening), 40)])
		}
	}
}
