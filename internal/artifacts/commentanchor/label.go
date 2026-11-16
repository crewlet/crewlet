package commentanchor

import (
	"fmt"
	"path"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// MaxLabelQuoteRunes is how much of an element's text a label quotes.
const MaxLabelQuoteRunes = 60

// Label is how a thread's place reads to a person: slideOrder is the asked
// version's slide ids, which number a slide.
func Label(a Anchor, slideOrder []string) string {
	switch a.Kind {
	case KindHTML:
		if a.Name != "" {
			return "Section: " + phrase(a.Name)
		}
		return "A point on the page"
	case KindSlides:
		slide := "A removed slide"
		if i := slices.Index(slideOrder, a.Slide); i >= 0 {
			slide = fmt.Sprintf("Slide %d", i+1)
		}
		if a.Text == "" {
			return slide
		}
		return slide + ": “" + quote(a.Text) + "”"
	case KindDesign:
		base := path.Base(a.Board)
		for _, ext := range []string{".html", ".htm"} {
			if len(base) > len(ext) && strings.EqualFold(base[len(base)-len(ext):], ext) {
				base = base[:len(base)-len(ext)]
				break
			}
		}
		return fmt.Sprintf("%s board, at %d, %d", phrase(base), a.X, a.Y)
	}
	return "The whole version"
}

// phrase is a name as words: '-', '_' and '.' as spaces, the first letter capital.
func phrase(name string) string {
	spaced := strings.Map(func(r rune) rune {
		if r == '-' || r == '_' || r == '.' {
			return ' '
		}
		return r
	}, name)
	out := strings.Join(strings.Fields(spaced), " ")
	first, size := utf8.DecodeRuneInString(out)
	if size == 0 {
		return out
	}
	return string(unicode.ToUpper(first)) + out[size:]
}

// quote is an element's text cut for a label, an ellipsis marking the cut.
func quote(text string) string {
	if utf8.RuneCountInString(text) <= MaxLabelQuoteRunes {
		return text
	}
	return strings.TrimSpace(clipRunes(text, MaxLabelQuoteRunes)) + "…"
}
