package brandformat_test

import (
	"bytes"
	"math"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts/brandformat"
)

// AN HONEST KIT IS READ WITH EVERY FILE IT SERVES, in the order the adopted
// brand serves them: the logos first, then each face's files in their order,
// each logo with its width over its height.
//
// Mutation: list the faces before the logos in files and the order fails.
func TestAnHonestKitIsReadWithEveryFileItServes(t *testing.T) {
	t.Parallel()
	k, err := brandformat.ReadKit(kit(t))
	if err != nil {
		t.Fatalf("an honest kit was refused: %v", err)
	}
	if k.Brand.Name != "Acme" || k.Brand.Type.Body.Family != "Acme Sans" {
		t.Errorf("the brand read is %+v", k.Brand)
	}
	want := []brandformat.KitFile{
		{Slot: brandformat.SlotLogoPrimary, Path: "logos/primary.svg", Essence: "image/svg+xml", Ratio: 4},
		{Slot: brandformat.SlotLogoOnDark, Path: "logos/on-dark.png", Essence: "image/png", Ratio: 3},
		{Slot: brandformat.SlotLogoMark, Path: "logos/mark.webp", Essence: "image/webp", Ratio: 1},
		{Slot: brandformat.FontFileSlot(brandformat.SlotFontBody, 0), Path: "fonts/body-0.woff2", Essence: "font/woff2"},
		{Slot: brandformat.FontFileSlot(brandformat.SlotFontBody, 1), Path: "fonts/body-1.woff2", Essence: "font/woff2"},
	}
	// EACH FILE COMES WITH ITS STORED FORM, Sanitize's answer for it, so an
	// adoption copies it without a second read and a second check.
	src := kit(t)
	for i := range k.Files {
		data, err := src.Read(k.Files[i].Path)
		if err != nil {
			t.Fatal(err)
		}
		stored, err := brandformat.Sanitize(k.Files[i].Path, k.Files[i].Essence, data)
		if err != nil || !bytes.Equal(k.Files[i].Stored, stored) {
			t.Errorf("%s's stored form is not Sanitize's (%v)", k.Files[i].Path, err)
		}
		k.Files[i].Stored = nil
	}
	if !reflect.DeepEqual(k.Files, want) {
		t.Errorf("the kit serves\n%+v\nwant\n%+v", k.Files, want)
	}
	if b, err := brandformat.Read(kit(t)); err != nil || !reflect.DeepEqual(b, k.Brand) {
		t.Errorf("Read answers %+v (%v), want ReadKit's brand", b, err)
	}
}

// THE INDEX IS DECODED STRICTLY: a key twice (encoding/json would keep the
// second, where another reader keeps the first), a key the format does not
// carry, a value of the wrong type, anything after the object and anything
// past the bound are each refused, naming the line.
//
// Mutation: drop the repeated-key check in the walk and the nested duplicate
// is read as its last value.
func TestTheIndexIsDecodedStrictly(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		index string
		want  string
		line  int
	}{
		"duplicate":        {strings.Replace(index, `"name": "Acme",`, `"name": "Acme", "name": "Evil",`, 1), "name is written twice", 3},
		"nested duplicate": {strings.Replace(index, `"primary": "#2450e6",`, `"primary": "#2450e6", "primary": "#000000",`, 1), "colors.primary is written twice", 6},
		"unknown":          {strings.Replace(index, `"v": 1,`, `"v": 1, "theme": "x",`, 1), "theme is not a key", 2},
		"unknown nested":   {strings.Replace(index, `"mark": "logos/mark.webp"`, `"mark": "logos/mark.webp", "favicon": "f.png"`, 1), "logos.favicon is not a key", 5},
		"case folded":      {strings.Replace(index, `"name":`, `"Name":`, 1), "Name is not a key", 3},
		"wrong type":       {strings.Replace(index, `"name": "Acme"`, `"name": 7`, 1), "name is a number, and the format reads a string there", 3},
		"object for value": {strings.Replace(index, `"name": "Acme"`, `"name": {"x": 1}`, 1), "name is an object", 3},
		"trailing":         {index + `{}`, "more after its value", 0},
		"not json":         {`{"v": 1,`, "not JSON", 0},
		"version":          {strings.Replace(index, `"v": 1`, `"v": 2`, 1), "brand format 1", 2},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			v := kit(t)
			v[brandformat.EntryPath] = []byte(c.index)
			findings := readRefused(t, v)
			found := false
			for _, f := range findings {
				if strings.Contains(f.Message, c.want) && (c.line == 0 || f.Line == c.line) {
					found = true
				}
			}
			if !found {
				t.Errorf("findings %v, want one saying %q at line %d", findings, c.want, c.line)
			}
		})
	}
	big := kit(t)
	big[brandformat.EntryPath] = []byte(strings.Replace(index, `"Acme"`,
		`"`+strings.Repeat("a", brandformat.MaxIndexBytes)+`"`, 1))
	if !names(readRefused(t, big), brandformat.RuleBound, "may be at most") {
		t.Error("an index past its bound was read")
	}
	missing := kit(t)
	delete(missing, brandformat.EntryPath)
	if !names(readRefused(t, missing), brandformat.RuleIndex, "holds no brand.json") {
		t.Error("a kit with no index was read")
	}
}

// EVERY FIELD IS HELD TO ITS RULE, and a finding names the field, the rule
// and the fix.
//
// Mutation: drop the licence check and the unattested family is read.
func TestEveryFieldIsHeldToItsRule(t *testing.T) {
	t.Parallel()
	body := `"body": {"family": "Acme Sans", "licensed": true,
      "files": [{"path": "fonts/body-0.woff2", "weight": 400, "style": "normal"},
                {"path": "fonts/body-1.woff2", "weight": 700, "style": "normal"}]}`
	for name, c := range map[string]struct {
		old, new string
		rule     brandformat.Rule
		want     string
	}{
		"no name":         {`"name": "Acme"`, `"name": ""`, brandformat.RuleField, "name: is required"},
		"long name":       {`"Acme"`, `"` + strings.Repeat("a", 81) + `"`, brandformat.RuleBound, "at most 80"},
		"url in name":     {`"Acme"`, `"Acme https://evil.test"`, brandformat.RuleField, "link"},
		"www in tagline":  {`"Tools for people who make things"`, `"see www.evil.test"`, brandformat.RuleField, "link"},
		"host and path":   {`"Tools for people who make things"`, `"acme.io/promo"`, brandformat.RuleField, "link"},
		"scheme-relative": {`"Acme"`, `"Acme //evil.example/x"`, brandformat.RuleField, "link"},
		"mail address":    {`"Acme"`, `"ops@evil.example"`, brandformat.RuleField, "link"},
		"markdown link":   {`"Tools for people who make things"`, `"[tools](evil.example)"`, brandformat.RuleField, "link"},
		"bare host":       {`"Tools for people who make things"`, `"Tools at evil.example"`, brandformat.RuleField, "web address"},
		"host in a line":  {`"We fixed it on Tuesday."`, `"Read more at acme-deals.io"`, brandformat.RuleField, "web address"},
		"own subdomain":   {`"We fixed it on Tuesday."`, `"Read more at promo.acme.io"`, brandformat.RuleField, "web address"},
		"markup":          {`"Tools for people who make things"`, `"<b>Tools</b>"`, brandformat.RuleField, "< or >"},
		"control":         {`"Acme"`, `"Ac\u0007me"`, brandformat.RuleField, "control character"},
		"bidi":            {`"Acme"`, `"Ac\u202eme"`, brandformat.RuleField, "U+202E"},
		"not nfc":         {`"Acme"`, `"Acmé"`, brandformat.RuleField, "normal form C"},
		"padded":          {`"Acme"`, `" Acme"`, brandformat.RuleField, "white space"},
		"missing logo":    {`"logos/primary.svg"`, `"logos/gone.svg"`, brandformat.RuleReference, "holds no file at logos/gone.svg"},
		"font as logo":    {`"logos/primary.svg"`, `"fonts/body-0.woff2"`, brandformat.RuleAsset, "font/woff2"},
		"upper colour":    {`"#2450e6"`, `"#2450E6"`, brandformat.RuleField, "six lowercase hex"},
		"short colour":    {`"#9584ff"`, `"#fff"`, brandformat.RuleField, "six lowercase hex"},
		"two sources":     {`{"kit": "geist"}`, `{"kit": "geist", "stack": "system-sans"}`, brandformat.RuleField, "names kit and stack"},
		"unknown kit":     {`{"kit": "geist"}`, `{"kit": "inter"}`, brandformat.RuleField, "not a kit face"},
		"unknown stack":   {`{"kit": "geist"}`, `{"stack": "comic"}`, brandformat.RuleField, "not a system stack"},
		"kit with files":  {`{"kit": "geist"}`, `{"kit": "geist", "licensed": true}`, brandformat.RuleField, "carries no files"},
		"licence alone":   {`{"kit": "geist"}`, `{"licensed": true}`, brandformat.RuleField, "names no face"},
		"bad family":      {`"Acme Sans"`, `"2 Acme"`, brandformat.RuleField, "ASCII letter"},
		"long family":     {`"Acme Sans"`, `"` + strings.Repeat("a", 41) + `"`, brandformat.RuleField, "at most 40"},
		"double space":    {`"Acme Sans"`, `"Acme  Sans"`, brandformat.RuleField, "single spaces"},
		"unlicensed":      {`"licensed": true`, `"licensed": false`, brandformat.RuleLicence, "licensed to true"},
		"no files": {body, `"body": {"family": "Acme Sans", "licensed": true}`,
			brandformat.RuleField, "names its WOFF2 files"},
		"odd weight":      {`"weight": 700`, `"weight": 750`, brandformat.RuleField, "whole hundred"},
		"oblique":         {`"style": "normal"}]`, `"style": "oblique"}]`, brandformat.RuleField, "normal or italic"},
		"repeated weight": {`"weight": 700`, `"weight": 400`, brandformat.RuleField, "repeats weight 400"},
		"long summary": {`"Plain, warm and exact.\nNever breathless."`, `"` + strings.Repeat("a", 301) + `"`,
			brandformat.RuleBound, "at most 300"},
		"newline in a line": {`"We fixed it on Tuesday."`, `"We fixed it.\nOn Tuesday."`, brandformat.RuleField,
			"control character"},
		"empty line": {`"We fixed it on Tuesday."`, `""`, brandformat.RuleField, "is empty"},
		"six lines": {`"write_like": ["We fixed it on Tuesday."]`, `"write_like": ["a", "b", "c", "d", "e", "f"]`,
			brandformat.RuleBound, "at most 5"},
		"data url": {`"Revolutionary"`, `"data:text/html,x"`, brandformat.RuleField, "link"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			findings := readRefused(t, with(t, c.old, c.new))
			if !names(findings, c.rule, c.want) {
				t.Errorf("findings %v, want a %s finding saying %q", findings, c.rule, c.want)
			}
		})
	}
	// PROSE IS NOT A LINK: "data: we keep it" is a sentence.
	if _, err := brandformat.ReadKit(with(t, `"Revolutionary"`, `"Vague data: say the number"`)); err != nil {
		t.Errorf("a sentence holding \"data:\" was refused: %v", err)
	}
	// A COMPANY'S NAME MAY BE A DOMAIN: the name is what the brand shows as
	// its name, and "Booking.com" is one. A path or a scheme is still refused
	// there (above).
	if _, err := brandformat.ReadKit(with(t, `"Acme"`, `"Booking.com"`)); err != nil {
		t.Errorf("a company named as its domain was refused: %v", err)
	}
}

// A FINDING NAMES THE LINE THE FIELD IS ON, so an author fixes the line the
// refusal names.
//
// Mutation: report field findings at line 0 and the line is lost.
func TestAFindingNamesTheLineTheFieldIsOn(t *testing.T) {
	t.Parallel()
	findings := readRefused(t, with(t, `"#9584ff"`, `"#FFF"`))
	for _, f := range findings {
		if strings.Contains(f.Message, "colors.secondary") {
			if f.Line != 6 {
				t.Errorf("the secondary colour's finding names line %d, want 6", f.Line)
			}
			return
		}
	}
	t.Errorf("no finding names colors.secondary: %v", findings)
}

// A BAD ASSET IS REFUSED BY ITS PATH, every finding of the kit in one pass:
// the author fixes them all at once.
//
// Mutation: stop at the first finding and only one comes back.
func TestEveryFindingComesBackInOnePass(t *testing.T) {
	t.Parallel()
	v := with(t, `"#9584ff"`, `"#nothex"`)
	v["logos/primary.svg"] = []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10"><script/></svg>`)
	v["fonts/body-1.woff2"] = []byte("not a font")
	findings := readRefused(t, v)
	for _, want := range []string{"colors.secondary", "logos/primary.svg", "fonts/body-1.woff2"} {
		found := false
		for _, f := range findings {
			if strings.Contains(f.String(), want) {
				found = true
			}
		}
		if !found {
			t.Errorf("no finding names %s: %v", want, findings)
		}
	}
}

// THE CANONICAL INDEX ROUND-TRIPS: an adoption snapshots Canonical, every
// reader decodes it to the same brand, and the same brand is always the same
// bytes.
//
// Mutation: drop SetEscapeHTML(false) and the ampersand is escaped.
func TestTheCanonicalIndexRoundTrips(t *testing.T) {
	t.Parallel()
	b, err := brandformat.Read(with(t, `"Acme"`, `"Acme & Co"`))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := brandformat.Canonical(b)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"name":"Acme & Co"`) {
		t.Errorf("the canonical index escapes what JSON does not require: %s", raw)
	}
	back, err := brandformat.Decode(raw)
	if err != nil {
		t.Fatalf("the canonical index does not decode: %v", err)
	}
	if !reflect.DeepEqual(back, b) {
		t.Errorf("the canonical index decodes to %+v, want %+v", back, b)
	}
	again, _ := brandformat.Canonical(back)
	if string(again) != string(raw) {
		t.Errorf("the canonical index is not a fixed point:\n%s\n%s", raw, again)
	}
}

// THE INJECTION CORPUS NEVER DECODES: every value security's corpus names, in
// every field that reaches the stylesheet, is refused by Decode, so an
// adoption, which decodes before it generates, never hands one to CSS.
//
// Mutation: relax ColorPattern to allow any non-empty value and the colour
// cases decode.
func TestTheInjectionCorpusNeverDecodes(t *testing.T) {
	t.Parallel()
	b, err := brandformat.Read(kit(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, evil := range corpus {
		for name, set := range map[string]func(*brandformat.Brand){
			"primary":   func(b *brandformat.Brand) { b.Colors.Primary = evil },
			"text":      func(b *brandformat.Brand) { b.Colors.Text = evil },
			"dark":      func(b *brandformat.Brand) { b.Colors.DarkGround = evil },
			"family":    func(b *brandformat.Brand) { b.Type.Body.Family = evil },
			"kit":       func(b *brandformat.Brand) { b.Type.Heading = brandformat.Face{Kit: evil} },
			"stack":     func(b *brandformat.Brand) { b.Type.Heading = brandformat.Face{Stack: evil} },
			"font path": func(b *brandformat.Brand) { b.Type.Body.Files[0].Style = evil },
		} {
			bad := b
			bad.Type.Body.Files = append([]brandformat.FontFile(nil), b.Type.Body.Files...)
			set(&bad)
			raw, err := brandformat.Canonical(bad)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := brandformat.Decode(raw); err == nil {
				t.Errorf("%s %q decoded", name, evil)
			}
		}
	}
}

// corpus is the CSS injection corpus (security r1, B4.6).
var corpus = []string{
	`red;}body{background:url(//x)}`, `#fff;--x:1`, `</style><script>`, `\72 ed`, `/* */`, "\n", "\x00",
	"\ufeff", "\u202e", `#ggg`, `#aabbccdd`, `A";}*{x:url(//y)}/*`, `#2450e6;`, `#2450e6}`, `url(//x)`,
}

// THE RATIO IS THE LOGO'S SHAPE: an SVG's from its viewBox, a raster's from
// its pixels, never rounded away.
func TestTheRatioIsTheLogosShape(t *testing.T) {
	t.Parallel()
	v := kit(t)
	v["logos/primary.svg"] = []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 30 7"/>`)
	k, err := brandformat.ReadKit(v)
	if err != nil {
		t.Fatal(err)
	}
	if got := k.Files[0].Ratio; math.Abs(got-30.0/7) > 1e-12 {
		t.Errorf("a 30 by 7 viewBox reads as ratio %v", got)
	}
}

// A KIT IS CLOSED: a file brand.json names no slot for is refused, named by
// its path, so a version cannot carry a stowaway no brand surface shows and
// no adoption copies.
//
// Mutation: drop the closed-kit walk in files and the stray file reads.
func TestAKitHoldsOnlyTheFilesItsIndexNames(t *testing.T) {
	t.Parallel()
	src := kit(t)
	src["notes.txt"] = []byte("draft notes")
	if !names(readRefused(t, src), brandformat.RuleReference, "notes.txt") {
		t.Error("a file brand.json names no slot for was read")
	}
}

// A KIT'S SEARCH TEXT IS ITS WORDS: the name, the tagline and the voice, so
// the gallery finds a kit by what it says and never by a path or a colour.
func TestAKitIsSearchedByItsWords(t *testing.T) {
	t.Parallel()
	b, err := brandformat.Read(kit(t))
	if err != nil {
		t.Fatal(err)
	}
	text := b.Text()
	for _, want := range []string{b.Name} {
		if !strings.Contains(text, want) {
			t.Errorf("the search text %q leaves out %q", text, want)
		}
	}
	if strings.Contains(text, b.Colors.Primary) || strings.Contains(text, b.Logos.Primary) {
		t.Errorf("the search text %q carries a colour or a path", text)
	}
}

// sizedVersion is a version that knows its files' sizes, as a stored one's
// manifest does, and records every file it is asked to read.
type sizedVersion struct {
	version
	sizes map[string]int64
	read  []string
}

func (v *sizedVersion) Size(p string) (int64, bool) {
	n, ok := v.sizes[p]
	return n, ok
}

func (v *sizedVersion) Read(p string) ([]byte, error) {
	v.read = append(v.read, p)
	return v.version.Read(p)
}

// A FILE PAST ITS BOUND BY ITS RECORDED SIZE IS NEVER READ: a reader that
// knows a file's size refuses it from that, in the words its own check uses,
// before a byte of it is in memory.
//
// Mutation: drop the size check and the oversized logo is read before it is
// refused.
func TestAFilePastItsBoundIsRefusedBeforeItIsRead(t *testing.T) {
	t.Parallel()
	src := &sizedVersion{version: kit(t), sizes: map[string]int64{
		"logos/on-dark.png": brandformat.MaxImageBytes + 1,
	}}
	_, err := brandformat.ReadKit(src)
	findings := refusal(t, err)
	if !names(findings, brandformat.RuleAsset, "export it smaller") {
		t.Errorf("findings %v, want the PNG's bound", findings)
	}
	if slices.Contains(src.read, "logos/on-dark.png") {
		t.Error("the oversized logo was read before it was refused")
	}
}
