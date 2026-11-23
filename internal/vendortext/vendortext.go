// Package vendortext is the one form of words a person follows at a vendor:
// an integration's setup steps, a field's help and an identity sheet's steps.
//
// A [Text] is a list of [Part]s, each plain words or a name on the vendor's
// own screen (a button, a field, a role, a page; several are a menu path),
// either linked to a vendor page. The structure travels on the wire as it is,
// so the dashboard draws each vendor UI name apart from the words around it
// and links the page on the first words that name it, with no markup in any
// sentence and no integration inventing its own. A page some step names is
// linked there, never repeated as a separate button.
package vendortext

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

// Text is words a person follows at a vendor, in order.
type Text []Part

// Part is one run of a [Text]: plain words, or a vendor UI name, either one
// linked where URL is set.
type Part struct {
	// Text is plain words, or a link's words; empty where UI is set.
	Text string `json:"text,omitempty"`

	// UI is a name on the vendor's screen as it prints it; several are a
	// menu path, outermost first.
	UI []string `json:"ui,omitempty"`

	// URL is the vendor page the part links to. A setup form fills its
	// `{field}` placeholders from the form and links nothing until each has
	// a value.
	URL string `json:"url,omitempty"`
}

// Of is a Text from its parts.
func Of(parts ...Part) Text { return Text(parts) }

// Plain is a Text of plain words only; nil for no words, so an omitempty
// field leaves it out.
func Plain(words string) Text {
	if words == "" {
		return nil
	}
	return Text{{Text: words}}
}

// W is plain words.
func W(words string) Part { return Part{Text: words} }

// UI is a vendor UI name, or a menu path outermost first.
func UI(path ...string) Part { return Part{UI: path} }

// At is p linked to the vendor page url.
func (p Part) At(url string) Part {
	p.URL = url
	return p
}

// String is t as plain words, a menu path joined with " > ", for a log line,
// a test or a sentence that cannot carry structure.
func (t Text) String() string {
	var b strings.Builder
	for _, p := range t {
		if len(p.UI) > 0 {
			b.WriteString(strings.Join(p.UI, " > "))
			continue
		}
		b.WriteString(p.Text)
	}
	return b.String()
}

// URLs are the vendor pages t links, in order, each once.
func (t Text) URLs() []string {
	var out []string
	for _, p := range t {
		if p.URL != "" && !slices.Contains(out, p.URL) {
			out = append(out, p.URL)
		}
	}
	return out
}

// Names are the vendor UI names t marks, each a path joined with " > ".
func (t Text) Names() []string {
	var out []string
	for _, p := range t {
		if len(p.UI) > 0 {
			out = append(out, strings.Join(p.UI, " > "))
		}
	}
	return out
}

// markup is what a sentence wrote by hand before Text carried it: a link
// mark, a markdown link, a bare address or bold. A value in a code span (an
// address to type) is a value, not markup; a menu path in one is markup.
var (
	markup   = regexp.MustCompile(`\{link\}|\]\(|https?://|\*\*`)
	codeSpan = regexp.MustCompile("`[^`]*`")
	codePath = regexp.MustCompile("`[^`]* > [^`]*`")
)

// Check reports what in t is not of the one form: a part both words and a
// name, an empty name, a link that is not https, or markup in the words.
func (t Text) Check() error {
	var errs []error
	for i, p := range t {
		switch {
		case len(p.UI) > 0 && p.Text != "":
			errs = append(errs, fmt.Errorf("part %d is both words and a vendor name", i))
		case len(p.UI) == 0 && p.Text == "":
			errs = append(errs, fmt.Errorf("part %d holds nothing", i))
		case slices.Contains(p.UI, ""):
			errs = append(errs, fmt.Errorf("part %d names an empty menu level", i))
		}
		if p.URL != "" && !strings.HasPrefix(p.URL, "https://") {
			errs = append(errs, fmt.Errorf("part %d links %q, which is not an https page", i, p.URL))
		}
		if m := codePath.FindString(p.Text); m != "" {
			errs = append(errs, fmt.Errorf("part %d writes the menu path %s as code: make it a vendortext.UI", i, m))
		}
		if m := markup.FindString(codeSpan.ReplaceAllString(p.Text, "")); m != "" {
			errs = append(errs, fmt.Errorf("part %d writes %q by hand: make it a vendortext part", i, m))
		}
	}
	return errors.Join(errs...)
}

// And is names, each a vendor UI name, said as a list in a sentence:
// "a", "a and b", "a, b and c" (as words.And says plain words).
func And(names ...string) []Part {
	var out []Part
	for i, n := range names {
		switch {
		case i == 0:
		case i == len(names)-1:
			out = append(out, W(" and "))
		default:
			out = append(out, W(", "))
		}
		out = append(out, UI(n))
	}
	return out
}
