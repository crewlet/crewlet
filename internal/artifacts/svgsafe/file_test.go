package svgsafe_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts/svgsafe"
)

const maxBytes = 512 << 10

// logo is a small, honest logo as an editor exports it: an XML declaration, a
// comment, both namespaces, xlink:href, a gradient, text and a reused symbol.
const logo = `<?xml version="1.0" encoding="UTF-8"?>
<!-- Generator: an editor -->
<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" version="1.1" viewBox="0 0 240 60" width="240" height="60">
  <title>Acme &amp; Co</title>
  <defs>
    <linearGradient id="g"><stop offset="0" stop-color="#2450e6"/><stop offset="1" stop-color="#9584ff"/></linearGradient>
    <symbol id="dot"><circle cx="5" cy="5" r="5"/></symbol>
  </defs>
  <rect width="60" height="60" rx="12" fill="url(#g)"/>
  <use xlink:href="#dot" x="70" y="25"/>
  <text x="90" y="40" font-family="Inter, sans-serif" font-size="28">Acme <tspan font-weight="700">Co</tspan></text>
</svg>
`

func parse(t *testing.T, src string) (svgsafe.Canonical, *svgsafe.Refused) {
	t.Helper()
	c, err := svgsafe.ParseFile([]byte(src), maxBytes)
	var refused *svgsafe.Refused
	if errors.As(err, &refused) {
		return c, refused
	}
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	return c, nil
}

func wrap(inner string) string {
	return `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10">` + inner + `</svg>`
}

// AN HONEST LOGO IS STORED AS THE ENGINE WRITES IT: the comment, the
// declaration, the XLink namespace and `version` gone, xlink:href written as
// href, attributes sorted, text escaped, its viewBox measured; and the stored
// form read again is itself, byte for byte.
//
// Mutation: write attributes in map order and the second pass differs.
func TestAnHonestLogoIsStoredAsTheEngineWritesIt(t *testing.T) {
	t.Parallel()
	c, refused := parse(t, logo)
	if refused != nil {
		t.Fatalf("an honest logo was refused: %v", refused)
	}
	got := string(c.Bytes)
	for _, want := range []string{
		`<svg xmlns="http://www.w3.org/2000/svg" height="60" viewBox="0 0 240 60" width="240">`,
		`<title>Acme &amp; Co</title>`,
		`<use href="#dot" x="70" y="25"/>`,
		`<tspan font-weight="700">Co</tspan>`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the canonical form lacks %s:\n%s", want, got)
		}
	}
	for _, gone := range []string{"<?xml", "<!--", "xmlns:xlink", "xlink:", "version="} {
		if strings.Contains(got, gone) {
			t.Errorf("the canonical form keeps %s:\n%s", gone, got)
		}
	}
	if c.Width != 240 || c.Height != 60 {
		t.Errorf("the viewBox measures %vx%v, want 240x60", c.Width, c.Height)
	}
	again, refused := parse(t, got)
	if refused != nil {
		t.Fatalf("the canonical form was refused: %v", refused)
	}
	if string(again.Bytes) != got {
		t.Errorf("the canonical form is not a fixed point:\n%s\n---\n%s", got, again.Bytes)
	}
}

// NOTHING THAT RUNS, LOADS OR EXPANDS IS STORED: each of these is refused,
// with a line where the file has one, rather than stripped into a drawing its
// author did not make.
//
// Mutation: skip ElementHint's refusal for foreignObject (treat it as an
// allowed element) and its case passes.
func TestNothingThatRunsLoadsOrExpandsIsStored(t *testing.T) {
	t.Parallel()
	laughs := `<?xml version="1.0"?>
<!DOCTYPE lolz [<!ENTITY lol "lol"><!ENTITY lol2 "&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;">]>
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><text>&lol2;</text></svg>`
	xxe := `<?xml version="1.0"?>
<!DOCTYPE svg [<!ENTITY xxe SYSTEM "file:///etc/passwd">]>
<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><text>&xxe;</text></svg>`
	for name, c := range map[string]struct{ src, want string }{
		"script":         {wrap(`<script>alert(1)</script>`), "runs no code"},
		"foreignObject":  {wrap(`<foreignObject><div/></foreignObject>`), "holds no HTML"},
		"style element":  {wrap(`<style>rect{fill:red}</style>`), "carries no stylesheet"},
		"style attr":     {wrap(`<rect style="fill:red"/>`), "presentation attributes"},
		"event handler":  {wrap(`<rect onclick="x()"/>`), "event handler"},
		"billion laughs": {laughs, "DOCTYPE"},
		"xxe":            {xxe, "DOCTYPE"},
		"external href":  {wrap(`<use href="https://x.test/s.svg#a"/>`), "its own shapes"},
		"javascript":     {wrap(`<use href="javascript:alert(1)"/>`), "javascript:"},
		"data value":     {wrap(`<rect fill="url(data:image/png;base64,AAAA)"/>`), "data:"},
		"external url":   {wrap(`<rect fill="url(https://x.test/a#b)"/>`), "url(#id)"},
		"animate":        {wrap(`<rect><animate attributeName="x" to="1"/></rect>`), "does not animate"},
		"set":            {wrap(`<rect><set attributeName="href" to="javascript:x"/></rect>`), "does not animate"},
		"a":              {wrap(`<a href="#x"><rect id="x"/></a>`), "links to nothing"},
		"image":          {wrap(`<image href="logo.png"/>`), "shows no other file"},
		"stylesheet pi": {`<?xml version="1.0"?><?xml-stylesheet href="evil.css"?>` + wrap(""),
			"processing instruction"},
		"late xml decl": {wrap("") + `<?xml version="1.0"?>`, "processing instruction"},
		"foreign namespace": {`<svg xmlns="http://www.w3.org/2000/svg" xmlns:i="http://ns.example/i" ` +
			`viewBox="0 0 10 10"><i:pgf/></svg>`, "namespace"},
		"foreign attr": {`<svg xmlns="http://www.w3.org/2000/svg" xmlns:i="http://ns.example/i" ` +
			`viewBox="0 0 10 10"><rect i:x="1"/></svg>`, "namespace"},
		"no namespace":  {`<svg viewBox="0 0 10 10"/>`, "declares no namespace"},
		"xml base":      {`<svg xmlns="http://www.w3.org/2000/svg" xml:base="https://x.test/" viewBox="0 0 10 10"/>`, "xml:base"},
		"html root":     {`<html xmlns="http://www.w3.org/2000/svg"/>`, "root"},
		"wrong case":    {`<svg xmlns="http://www.w3.org/2000/svg" viewbox="0 0 10 10"/>`, "write viewBox"},
		"latin1":        {`<?xml version="1.0" encoding="ISO-8859-1"?>` + wrap(""), "well-formed"},
		"no viewBox":    {`<svg xmlns="http://www.w3.org/2000/svg" width="10" height="10"/>`, "no viewBox"},
		"zero viewBox":  {`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 0 10"/>`, "above zero"},
		"wide viewBox":  {`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 900 100"/>`, "between 1:8 and 8:1"},
		"tall viewBox":  {`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 81"/>`, "between 1:8 and 8:1"},
		"dangling ref":  {wrap(`<rect fill="url(#g)"/>`), "#g"},
		"loose text":    {wrap(`hello`), "sits in <text>"},
		"title element": {wrap(`<title><rect/></title>`), "text alone"},
		"two roots":     {wrap("") + wrap(""), "one <svg>"},
		"xml space":     {`<svg xmlns="http://www.w3.org/2000/svg" xml:space="preserve" viewBox="0 0 10 10"/>`, "xml:space"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, refused := parse(t, c.src)
			if refused == nil {
				t.Fatalf("the drawing was stored")
			}
			if !strings.Contains(refused.Error(), c.want) {
				t.Errorf("the refusal says %q, want it to name %q", refused.Error(), c.want)
			}
		})
	}
}

// A VALUE SAYS ONLY WHAT A DRAWING NEEDS. A presentation attribute is parsed
// as CSS, where an escape spells `url(` without writing it and a function
// reaches outside the drawing, so each of these is refused although none
// writes `url(` to an outside address in so many words.
//
// Mutation: drop the backslash refusal and `u\72l(` is stored; let any value
// with a parenthesis through and `image-set()` is.
func TestAValueSaysOnlyWhatADrawingNeeds(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct{ src, want string }{
		"escaped url in fill":   {wrap(`<rect fill="u\72l(https://x.test/p.svg#a)"/>`), "backslash"},
		"escaped url in filter": {wrap(`<rect filter="u\72l(https://x.test/f.svg#a)"/>`), "backslash"},
		"escaped in a keyword":  {wrap(`<rect display="no\6e e"/>`), "backslash"},
		"image-set mask":        {wrap(`<rect mask="image-set('https://x.test/a.png' 1x)"/>`), "url(#id)"},
		"var fill":              {wrap(`<rect fill="var(--x)"/>`), "a colour is"},
		"env stroke-width":      {wrap(`<rect stroke-width="env(safe-area-inset-top)"/>`), "a value is"},
		"blur filter":           {wrap(`<rect filter="blur(2px)"/>`), "own filter"},
		"inset clip-path":       {wrap(`<rect clip-path="inset(0)"/>`), "url(#id)"},
		"attr in font-family":   {wrap(`<text font-family="attr(x)">a</text>`), "font family"},
		"function in transform": {wrap(`<rect transform="translate(var(--x))"/>`), "a transform is"},
		"semicolon":             {wrap(`<rect fill="red;x"/>`), "a colour is"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			_, refused := parse(t, c.src)
			if refused == nil {
				t.Fatalf("the drawing was stored")
			}
			if !strings.Contains(refused.Error(), c.want) {
				t.Errorf("the refusal says %q, want it to name %q", refused.Error(), c.want)
			}
		})
	}

	// WHAT AN EDITOR EXPORTS STILL READS: colour functions, transforms, a
	// font list with quotes, path data, dashes, a filter by reference, and
	// words for a person that are not CSS.
	honest := wrap(`<defs><filter id="f"><feGaussianBlur stdDeviation="1.5 2"/></filter></defs>` +
		`<g transform="matrix(1 0 0 1 -10.5 2e1) rotate(-45, 5 5)" filter="url(#f)">` +
		`<path d="M0 0L10-5.5h3v2z" fill="rgba(36, 80, 230, 0.5)" stroke="hsl(240deg 50% 40% / 80%)" ` +
		`stroke-dasharray="4, 2" stroke-width="1.5px" aria-label="Acme (logo) \ mark"/>` +
		`<text font-family="'Helvetica Neue', Inter, sans-serif" font-size="12" fill="currentColor">` +
		`a</text></g>`)
	if _, refused := parse(t, honest); refused != nil {
		t.Errorf("an honest drawing was refused: %v", refused)
	}
}

// A REFUSAL NAMES THE LINE: an author fixes the line the finding names.
//
// Mutation: report every finding at line 0 and the line is lost.
func TestARefusalNamesTheLine(t *testing.T) {
	t.Parallel()
	_, refused := parse(t, "<svg xmlns=\"http://www.w3.org/2000/svg\" viewBox=\"0 0 10 10\">\n<g>\n"+
		"<rect onclick=\"x()\"/>\n</g>\n</svg>")
	if refused == nil || len(refused.Findings) != 1 || refused.Findings[0].Line != 3 {
		t.Fatalf("findings %+v, want one on line 3", refused)
	}
}

// A USE NEVER DRAWS ITSELF, AND NEVER MULTIPLIES PAST THE BOUND: a use inside
// the element it draws is a cycle, and ten levels of groups each using the one
// below ten times are a hundred elements that draw ten billion.
//
// Mutation: drop the visiting check and the cycle recurses without end (the
// test times out); drop the saturation and the count overflows to a pass.
func TestAUseNeverDrawsItselfAndNeverMultipliesPastTheBound(t *testing.T) {
	t.Parallel()
	_, refused := parse(t, wrap(`<g id="a"><rect/><use href="#a"/></g>`))
	if refused == nil || !strings.Contains(refused.Error(), "draws this use again") {
		t.Errorf("a use inside the group it draws was stored: %v", refused)
	}
	_, refused = parse(t, wrap(`<g id="a"><use href="#b"/></g><g id="b"><use href="#a"/></g>`))
	if refused == nil || !strings.Contains(refused.Error(), "draws this use again") {
		t.Errorf("two groups using each other were stored: %v", refused)
	}

	var b strings.Builder
	b.WriteString(`<defs><g id="l0"><rect width="1" height="1"/></g>`)
	for level := 1; level <= 10; level++ {
		fmt.Fprintf(&b, `<g id="l%d">`, level)
		for range 10 {
			fmt.Fprintf(&b, `<use href="#l%d"/>`, level-1)
		}
		b.WriteString(`</g>`)
	}
	b.WriteString(`</defs><use href="#l10"/>`)
	_, refused = parse(t, wrap(b.String()))
	if refused == nil || !strings.Contains(refused.Error(), "once its <use>s are expanded") {
		t.Errorf("a use bomb was stored: %v", refused)
	}

	// A MODEST REUSE IS A LOGO: forty dots drawn from one symbol.
	b.Reset()
	b.WriteString(`<defs><symbol id="d"><circle r="1"/></symbol></defs>`)
	for i := range 40 {
		fmt.Fprintf(&b, `<use href="#d" x="%d"/>`, i)
	}
	if _, refused := parse(t, wrap(b.String())); refused != nil {
		t.Errorf("a drawing reusing one symbol forty times was refused: %v", refused)
	}
}

// THE BOUNDS HOLD: a file past its byte size, past the element count and past
// the nesting depth is refused, each naming the bound.
//
// Mutation: drop the element count and the eleven-thousand-rect file is stored.
func TestTheBoundsHold(t *testing.T) {
	t.Parallel()
	if _, err := svgsafe.ParseFile([]byte(wrap("")), 10); err == nil {
		t.Error("a file past its byte bound was stored")
	}
	if _, refused := parse(t, wrap(strings.Repeat(`<rect/>`, svgsafe.MaxElements+1))); refused == nil ||
		!strings.Contains(refused.Error(), "more than") {
		t.Errorf("a file past the element bound was stored: %v", refused)
	}
	deep := strings.Repeat(`<g>`, svgsafe.MaxDepth) + strings.Repeat(`</g>`, svgsafe.MaxDepth)
	if _, refused := parse(t, wrap(deep)); refused == nil || !strings.Contains(refused.Error(), "deep") {
		t.Errorf("a file nesting past the depth bound was stored: %v", refused)
	}
}

// ESCAPING HOLDS EVERYTHING AS TEXT: a value or a text carrying markup
// characters comes back out as references, so the stored file reads as the
// same tree in any XML reader.
//
// Mutation: write attribute values unescaped and the quote closes the value.
func TestEscapingHoldsEverythingAsText(t *testing.T) {
	t.Parallel()
	c, refused := parse(t, wrap(`<text aria-label="a &quot;b&quot; &amp; c">x &gt; y</text>`))
	if refused != nil {
		t.Fatal(refused)
	}
	got := string(c.Bytes)
	if !strings.Contains(got, `aria-label="a &quot;b&quot; &amp; c"`) || !strings.Contains(got, `x &gt; y`) {
		t.Errorf("the canonical form did not escape: %s", got)
	}
	if again, refused := parse(t, got); refused != nil || string(again.Bytes) != got {
		t.Errorf("the escaped form is not a fixed point: %v", refused)
	}
}

// FuzzAStoredDrawingIsAFixedPoint: whatever a file holds, a drawing the front
// end accepts is stored as bytes it accepts again unchanged, holding none of
// the constructs it refuses.
func FuzzAStoredDrawingIsAFixedPoint(f *testing.F) {
	f.Add(logo)
	f.Add(wrap(`<text>a &lt; b</text>`))
	f.Add(wrap(`<g id="a"><use href="#b"/></g><g id="b"/>`))
	f.Add(`<?xml version="1.0"?><!DOCTYPE x><svg xmlns="http://www.w3.org/2000/svg"/>`)
	f.Fuzz(func(t *testing.T, src string) {
		c, err := svgsafe.ParseFile([]byte(src), maxBytes)
		if err != nil {
			return
		}
		again, err := svgsafe.ParseFile(c.Bytes, maxBytes)
		if err != nil {
			t.Fatalf("the stored form is refused: %v\n%s", err, c.Bytes)
		}
		if string(again.Bytes) != string(c.Bytes) {
			t.Fatalf("the stored form is not a fixed point:\n%s\n---\n%s", c.Bytes, again.Bytes)
		}
		low := strings.ToLower(string(c.Bytes))
		for _, never := range []string{"<script", "<foreignobject", "<style", "<!", "<?"} {
			if strings.Contains(low, never) {
				t.Fatalf("the stored form holds %q:\n%s", never, c.Bytes)
			}
		}
	})
}
