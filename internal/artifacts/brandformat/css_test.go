package brandformat_test

import (
	"math"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts/brandformat"
	"github.com/crewlet/crewlet/internal/objstore"
)

const (
	hashA = "a3f1c2d4e5b60718293a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e8f90"
	hashB = "0b1c2d3e4f5061728394a5b6c7d8e9f00b1c2d3e4f5061728394a5b6c7d8e9f0"
	hashC = "ffeeddccbbaa99887766554433221100ffeeddccbbaa99887766554433221100"
)

// served is the files an adoption of the honest kit passes the generator.
func served(t *testing.T) (brandformat.Brand, map[brandformat.Slot]brandformat.ServedFile) {
	t.Helper()
	k, err := brandformat.ReadKit(kit(t))
	if err != nil {
		t.Fatal(err)
	}
	files := map[brandformat.Slot]brandformat.ServedFile{}
	for i, f := range k.Files {
		files[f.Slot] = brandformat.ServedFile{Hash: objstore.Hash([]string{hashA, hashB, hashC}[i%3]),
			Essence: f.Essence, Ratio: f.Ratio}
	}
	return k.Brand, files
}

// THE STYLESHEET DEFINES THE KIT'S INPUTS AND NOTHING ELSE: the colour roles
// the brand sets, both faces' lists, each logo's address and ratio, and an
// @font-face per uploaded file, every address the public brand route's.
//
// Mutation: write --brand-logo-primary without its ratio and the logo line
// set loses its pair.
func TestTheStylesheetDefinesTheKitsInputsAndNothingElse(t *testing.T) {
	t.Parallel()
	b, files := served(t)
	css := string(brandformat.CSS(adoptable(t, b), files))
	for _, want := range []string{
		"  --brand-primary: #2450e6;\n", "  --brand-secondary: #9584ff;\n", "  --brand-text: #121625;\n",
		"  --brand-light-ground: #ffffff;\n", "  --brand-dark-ground: #0d0f16;\n",
		"  --brand-font-heading: " + brandformat.FaceFamilies["geist"] + ";\n",
		`  --brand-font-body: "Acme Sans", ` + brandformat.FaceFamilies["system-sans"] + ";\n",
		"  --brand-logo-primary: url(/_crewlet/brand/" + hashA + ".svg);\n",
		"  --brand-logo-primary-ratio: 4;\n",
		"  --brand-logo-on-dark: url(/_crewlet/brand/" + hashB + ".png);\n",
		"  --brand-logo-on-dark-ratio: 3;\n",
		"  --brand-logo-mark: url(/_crewlet/brand/" + hashC + ".webp);\n",
		"  --brand-logo-mark-ratio: 1;\n",
		"@font-face {\n  font-family: \"Acme Sans\";\n  src: url(/_crewlet/brand/" + hashA +
			".woff2) format(\"woff2\");\n  font-weight: 400;\n  font-style: normal;\n  font-display: swap;\n}\n",
		"  font-weight: 700;\n",
	} {
		if !strings.Contains(css, want) {
			t.Errorf("the stylesheet lacks %q:\n%s", want, css)
		}
	}
	if strings.Contains(css, "--brand-primary-on-dark") {
		t.Error("an absent primary_on_dark wrote a declaration: the kit derives it from the primary")
	}
	checkCSS(t, []byte(css))
	if again := string(brandformat.CSS(adoptable(t, b), files)); again != css {
		t.Error("the stylesheet is not the same bytes twice")
	}
}

// AN ABSENT ROLE WRITES NOTHING, AND A MISSING FILE WRITES NOTHING: the kit
// keeps its own value wherever an input is unset.
//
// Mutation: write every colour role whatever its value and the empty text
// writes `--brand-text: ;`.
func TestAnAbsentRoleOrFileWritesNothing(t *testing.T) {
	t.Parallel()
	b := brandformat.Brand{Logos: brandformat.Logos{Primary: "logo.svg"},
		Colors: brandformat.Colors{Primary: "#2450e6"},
		Type:   brandformat.Type{Heading: brandformat.Face{Kit: "geist"}, Body: brandformat.Face{Stack: "system-serif"}}}
	css := string(brandformat.CSS(adoptable(t, b), nil))
	want := ":root {\n  --brand-primary: #2450e6;\n  --brand-font-heading: " + brandformat.FaceFamilies["geist"] +
		";\n  --brand-font-body: " + brandformat.FaceFamilies["system-serif"] + ";\n}\n"
	if css != want {
		t.Errorf("the stylesheet is\n%s\nwant\n%s", css, want)
	}
}

// NO VALUE LEAVES ITS DECLARATION, WHATEVER THE BRAND HOLDS: a brand that
// skipped every check, carrying the injection corpus in every field the
// generator writes and in the files it names, generates a stylesheet that is
// still only the kit's inputs in their strict shapes.
//
// Mutation: drop the generator's colour check and `#fff;--x:1` writes a
// second property.
func TestNoValueLeavesItsDeclaration(t *testing.T) {
	t.Parallel()
	for _, evil := range corpus {
		b := brandformat.Brand{
			Logos: brandformat.Logos{Primary: evil},
			Colors: brandformat.Colors{Primary: evil, Secondary: evil, Text: evil, LightGround: evil,
				DarkGround: evil, PrimaryOnDark: evil},
			Type: brandformat.Type{
				Heading: brandformat.Face{Kit: evil},
				Body: brandformat.Face{Family: evil, Licensed: true,
					Files: []brandformat.FontFile{{Path: "f.woff2", Weight: 400, Style: evil}}},
			},
		}
		files := map[brandformat.Slot]brandformat.ServedFile{
			brandformat.SlotLogoPrimary:                           {Hash: objstore.Hash(evil), Essence: "image/svg+xml", Ratio: 2},
			brandformat.SlotLogoMark:                              {Hash: hashA, Essence: evil, Ratio: 1},
			brandformat.SlotLogoOnDark:                            {Hash: hashA, Essence: "image/png", Ratio: math.Inf(1)},
			brandformat.FontFileSlot(brandformat.SlotFontBody, 0): {Hash: objstore.Hash(evil), Essence: "font/woff2"},
		}
		css := brandformat.CSS(adoptable(t, b), files)
		checkCSS(t, css)
		if len(css) != 0 {
			t.Errorf("a brand holding only %q wrote:\n%s", evil, css)
		}
	}
}

// FuzzCSSWritesOnlyBrandDeclarations: whatever a brand and its files hold, the
// generated stylesheet is a `:root` block of the kit's inputs and @font-face
// rules, every value in its strict shape and every URL the brand route's.
func FuzzCSSWritesOnlyBrandDeclarations(f *testing.F) {
	for _, evil := range corpus {
		f.Add(evil, evil, evil, evil, evil, 2.0, 400)
	}
	f.Add("#2450e6", "#ffffff", "Acme Sans", "geist", hashA, 4.0, 700)
	f.Fuzz(func(t *testing.T, color, ground, family, kitFace, hash string, ratio float64, weight int) {
		b := brandformat.Brand{
			Logos:  brandformat.Logos{Primary: "logo.svg"},
			Colors: brandformat.Colors{Primary: color, LightGround: ground, Text: ground},
			Type: brandformat.Type{
				Heading: brandformat.Face{Kit: kitFace},
				Body: brandformat.Face{Family: family, Licensed: true,
					Files: []brandformat.FontFile{{Path: "f.woff2", Weight: weight, Style: "normal"}}},
			},
		}
		files := map[brandformat.Slot]brandformat.ServedFile{
			brandformat.SlotLogoPrimary:                           {Hash: objstore.Hash(hash), Essence: "image/svg+xml", Ratio: ratio},
			brandformat.SlotLogoMark:                              {Hash: objstore.Hash(hash), Essence: ground, Ratio: ratio},
			brandformat.FontFileSlot(brandformat.SlotFontBody, 0): {Hash: objstore.Hash(hash), Essence: "font/woff2"},
		}
		a, ok := brandformat.AsAdoptable(b)
		if !ok {
			// A BRAND WITH NO COLOUR IS NO ADOPTABLE ONE, and no stylesheet is
			// ever made from it.
			return
		}
		checkCSS(t, brandformat.CSS(a, files))
	})
}

// The strict shapes a generated stylesheet's values take.
var (
	cssBlock    = regexp.MustCompile(`(?s)\A(:root|@font-face) \{\n(.*?)\n\}\n`)
	cssDecl     = regexp.MustCompile(`\A  ([a-z-]+): (.+);\z`)
	cssColor    = regexp.MustCompile(`\A#[0-9a-f]{6}\z`)
	cssLogo     = regexp.MustCompile(`\Aurl\(/_crewlet/brand/[0-9a-f]{64}\.(svg|png|webp)\)\z`)
	cssRatio    = regexp.MustCompile(`\A[0-9]+(\.[0-9]{1,4})?\z`)
	cssFamily   = regexp.MustCompile(`\A"[A-Za-z](?:[A-Za-z0-9-]| [A-Za-z0-9-]){0,39}"\z`)
	cssFontSrc  = regexp.MustCompile(`\Aurl\(/_crewlet/brand/[0-9a-f]{64}\.woff2\) format\("woff2"\)\z`)
	cssWeight   = regexp.MustCompile(`\A[1-9]00\z`)
	cssUploaded = regexp.MustCompile(`\A"[A-Za-z](?:[A-Za-z0-9-]| [A-Za-z0-9-]){0,39}", `)
)

// checkCSS holds a generated stylesheet to its contract, block by block and
// declaration by declaration, failing on anything else.
func checkCSS(t *testing.T, css []byte) {
	t.Helper()
	stacks := map[string]bool{}
	for _, list := range brandformat.FaceFamilies {
		stacks[list] = true
	}
	rest := string(css)
	for rest != "" {
		m := cssBlock.FindStringSubmatch(rest)
		if m == nil {
			t.Fatalf("the stylesheet holds something other than a :root or @font-face block:\n%s", rest)
		}
		rest = rest[len(m[0]):]
		seen := map[string]bool{}
		for _, line := range strings.Split(m[2], "\n") {
			d := cssDecl.FindStringSubmatch(line)
			if d == nil {
				t.Fatalf("a %s line is not one declaration: %q", m[1], line)
			}
			prop, value := d[1], d[2]
			if seen[prop] {
				t.Errorf("%s declares %s twice", m[1], prop)
			}
			seen[prop] = true
			ok := false
			switch {
			case m[1] == "@font-face":
				switch prop {
				case "font-family":
					ok = cssFamily.MatchString(value)
				case "src":
					ok = cssFontSrc.MatchString(value)
				case "font-weight":
					ok = cssWeight.MatchString(value)
				case "font-style":
					ok = value == "normal" || value == "italic"
				case "font-display":
					ok = value == "swap"
				}
			case slices.Contains([]string{"--brand-primary", "--brand-primary-on-dark", "--brand-secondary",
				"--brand-text", "--brand-light-ground", "--brand-dark-ground"}, prop):
				ok = cssColor.MatchString(value)
			case prop == "--brand-font-heading" || prop == "--brand-font-body":
				ok = stacks[value] || (cssUploaded.MatchString(value) &&
					stacks[cssUploaded.ReplaceAllString(value, "")])
			case prop == "--brand-logo-primary" || prop == "--brand-logo-on-dark" || prop == "--brand-logo-mark":
				ok = cssLogo.MatchString(value)
			case strings.HasSuffix(prop, "-ratio") && strings.HasPrefix(prop, "--brand-logo-"):
				ok = cssRatio.MatchString(value)
			}
			if !ok {
				t.Errorf("%s writes %s: %q, outside the contract", m[1], prop, value)
			}
		}
	}
}
