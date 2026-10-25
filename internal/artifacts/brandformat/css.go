package brandformat

import (
	"bytes"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/objstore"
)

// ---- the brand stylesheet ------------------------------------------------- //
//
// THE ONE GENERATOR of the stylesheet an adopted brand serves, run once at the
// adoption and stored as an object: never at serve time, because a generator a
// later build changes would change the bytes behind a hash a published version
// pins.
//
// IT DEFINES INPUTS AND NOTHING ELSE: `--brand-*` custom properties on `:root`,
// the slots the artifact kit's brand layer reads (`@crewlethq/artifact-kit`
// 0.47.0 onward, its README's "A company's brand"), and an `@font-face` rule
// per file of an uploaded family. No selector but `:root`, no property a page
// paints with, so the sheet restyles only what the kit chooses to read from
// it, and a page on an earlier kit is untouched. The kit derives every mode's
// accent family, the label on the accent and the faces from these inputs, and
// keeps its own value wherever an input is absent, which is why an absent role
// writes no declaration at all.
//
// NO VALUE IS WRITTEN THAT DID NOT PASS ITS GRAMMAR HERE, AGAIN. A brand is
// read before an adoption generates its sheet, and the generator still checks
// every value it writes (a colour against [ColorPattern], a hash as 64 hex
// digits, an extension in [ServedExts], a family against its grammar, a ratio
// as a finite positive number, a weight and a style against their sets) and
// omits anything else, so no caller that skipped the read can make a value
// leave its declaration. Every URL it writes is `/_crewlet/brand/<sha256>.<ext>`,
// built from a hash and an extension alone.

// ServedFile is one file an adoption serves, as the stylesheet names it: its
// hash (the public route's path), its essence (its extension) and, for a logo,
// its width over its height.
type ServedFile struct {
	Hash    objstore.Hash
	Essence string
	Ratio   float64
}

// BrandRoute is the path every file the adopted brand serves sits under on
// an artifact's origin: `/_crewlet/brand/<sha256>.<ext>`.
const BrandRoute = "/_crewlet/brand/"

// CSS is the stylesheet an adoptable brand generates, its files named by slot
// (the logos by [SlotLogoPrimary], [SlotLogoOnDark] and [SlotLogoMark], each
// file of a face by [FontFileSlot]). An adoption passes every file the kit
// serves; a slot missing from files writes no declaration for that file. An
// unset face writes no family, and the kit's own face is drawn.
func CSS(a Adoptable, files map[Slot]ServedFile) []byte {
	b := a.brand
	var root []string
	decl := func(name, value string) { root = append(root, "  "+name+": "+value+";") }
	for _, c := range []struct{ name, value string }{
		{"--brand-primary", b.Colors.Primary},
		{"--brand-primary-on-dark", b.Colors.PrimaryOnDark},
		{"--brand-secondary", b.Colors.Secondary},
		{"--brand-text", b.Colors.Text},
		{"--brand-light-ground", b.Colors.LightGround},
		{"--brand-dark-ground", b.Colors.DarkGround},
	} {
		if ColorPattern.MatchString(c.value) {
			decl(c.name, c.value)
		}
	}
	for _, f := range []struct {
		name string
		face Face
	}{{"--brand-font-heading", b.Type.Heading}, {"--brand-font-body", b.Type.Body}} {
		if stack := cssStack(f.face); stack != "" {
			decl(f.name, stack)
		}
	}
	for _, l := range []struct {
		slot Slot
		name string
	}{{SlotLogoPrimary, "primary"}, {SlotLogoOnDark, "on-dark"}, {SlotLogoMark, "mark"}} {
		file, ok := files[l.slot]
		if !ok || !slices.Contains(logoEssences, file.Essence) {
			continue
		}
		url, ok := cssURL(file)
		ratio := cssRatio(file.Ratio)
		if !ok || ratio == "" {
			continue
		}
		decl("--brand-logo-"+l.name, url)
		decl("--brand-logo-"+l.name+"-ratio", ratio)
	}
	var out bytes.Buffer
	if len(root) > 0 {
		out.WriteString(":root {\n")
		out.WriteString(strings.Join(root, "\n"))
		out.WriteString("\n}\n")
	}
	for _, f := range []struct {
		slot Slot
		face Face
	}{{SlotFontHeading, b.Type.Heading}, {SlotFontBody, b.Type.Body}} {
		if f.face.Family == "" || !ValidFamily(f.face.Family) {
			continue
		}
		for i, file := range f.face.Files {
			served, ok := files[FontFileSlot(f.slot, i)]
			if !ok || served.Essence != fontEssence {
				continue
			}
			url, ok := cssURL(served)
			if !ok || !ValidWeight(file.Weight) || !slices.Contains(FontStyles, file.Style) {
				continue
			}
			out.WriteString("@font-face {\n")
			out.WriteString(`  font-family: "` + f.face.Family + "\";\n")
			out.WriteString("  src: " + url + " format(\"woff2\");\n")
			out.WriteString("  font-weight: " + strconv.Itoa(file.Weight) + ";\n")
			out.WriteString("  font-style: " + file.Style + ";\n")
			out.WriteString("  font-display: swap;\n")
			out.WriteString("}\n")
		}
	}
	return out.Bytes()
}

// cssStack is a face's font-family list, or "" for a face outside the format.
func cssStack(f Face) string {
	switch {
	case f.Kit != "" && f.Stack == "" && f.Family == "" && slices.Contains(KitFaces, f.Kit):
	case f.Stack != "" && f.Kit == "" && f.Family == "" && slices.Contains(SystemStacks, f.Stack):
	case f.Family != "" && f.Kit == "" && f.Stack == "" && ValidFamily(f.Family):
	default:
		return ""
	}
	return FontStack(f)
}

// cssURL is a served file's address on the artifact origin, false when its
// hash or its essence is not one the brand route serves.
func cssURL(f ServedFile) (string, bool) {
	ext, ok := Ext(f.Essence)
	if !ok || !f.Hash.Valid() {
		return "", false
	}
	return "url(" + BrandRoute + string(f.Hash) + "." + ext + ")", true
}

// cssRatio is a logo's width over its height as a plain decimal of at most
// four places, never an exponent, or "" for a ratio that is not a finite
// positive number.
func cssRatio(r float64) string {
	if math.IsNaN(r) || math.IsInf(r, 0) || r <= 0 || r > MaxImageSide {
		return ""
	}
	s := strconv.FormatFloat(r, 'f', 4, 64)
	s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	if s == "" || s == "0" {
		return ""
	}
	return s
}
