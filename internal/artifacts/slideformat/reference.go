package slideformat

import (
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/jsonform"
	"github.com/crewlet/crewlet/internal/toolname"
)

// Reference is the slide format as an agent reads it before writing a deck:
// every key of `deck.json` with its type ([IndexForm], the strict decode's own
// tree), and the bounds and closed sets a slide is held to, each the value the
// reader checks against.
func Reference() string {
	themes := make([]string, 0, len(themes)+1)
	themes = append(themes, BrandTheme)
	for _, th := range Themes() {
		themes = append(themes, th.ID)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "# A slide deck: %s and its slides\n\n", IndexPath)
	fmt.Fprintf(&b, "A deck is `%s` at the version's root and one HTML file per slide under "+
		"`%s` (`%sintro.html` is the slide `intro`). A key not listed is refused, and so is "+
		"a value of another type. Begin a deck with %s `starter` rather than "+
		"writing this by hand where you can.\n\n", IndexPath, SlidesDir, SlidesDir,
		toolname.CreateArtifact)
	b.WriteString("## Every key of " + IndexPath + "\n\n")
	b.WriteString(IndexForm.Outline(map[string]string{
		"title": fmt.Sprintf("required, at most %d bytes", MaxTitleBytes),
		"order": fmt.Sprintf("required: the slide ids in order, 1 to %d, each once", MaxSlides),
		"theme": jsonform.OneOf(themes) + ", or absent",
		"faces": "typefaces by key: each a `family` and the `src` of a woff2, woff, ttf or otf " +
			"file of the version (never an `href` to a font service)",
		"designSystems": "refused when it names any: a Crewlet deck's look is the company's artifact kit",
	}))
	b.WriteString("\n## Each slide\n\n")
	fmt.Fprintf(&b, "- Elements: %s. At most %d elements, %d nested `div`s, %d characters "+
		"of text and %d of speaker notes (one `<aside>`, the section's last child).\n",
		strings.Join(Elements(), ", "), MaxElements, MaxDivDepth, MaxTextRunes, MaxNotesRunes)
	fmt.Fprintf(&b, "- Tables: at most %d rows, %d cells a row, %d cells a slide.\n",
		MaxTableRows, MaxTableColumns, MaxTableCells)
	fmt.Fprintf(&b, "- `x-shape kind`: %s. `x-icon name`: %s.\n",
		jsonform.OneOf(ShapeKinds), jsonform.OneOf(Icons))
	fmt.Fprintf(&b, "- `data-transition`: %s.\n", jsonform.OneOf(Transitions))
	b.WriteString("- Styles are inline: lengths in px, hex, `rgb()` or `hsl()` colours; never " +
		"classes, `<style>`, `margin`, `z-index`, `em`, `%` font sizes, `var()` or `url()`. " +
		"An image is a file of the version named by its path, never a URL.\n")
	return b.String()
}
