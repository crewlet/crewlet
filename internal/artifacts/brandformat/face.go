package brandformat

import "regexp"

// ---- faces ---------------------------------------------------------------- //
//
// A FACE RESOLVES TO ONE font-family LIST, the same everywhere it is drawn: the
// generated brand stylesheet's `--brand-font-heading` and `--brand-font-body`,
// the Brand page's preview (the dashboard contract's BRAND_FACE_FAMILIES, gated
// to [FaceFamilies]), and an agent's answer from `get_company_brand`.
//
// EVERY NAME IS A BARE IDENTIFIER SEQUENCE, never quoted, so the same list is a
// valid CSS value and a plain TypeScript string, and no value here carries a
// character a stylesheet would read as syntax. An uploaded family is the one
// name quoted, by [FontStack], because its grammar admits a word that begins
// with a digit. A slide's `font-family` grammar is narrower (names that begin
// with a letter, at most eight), so [SlideTheme] draws its faces from the same
// lists through that grammar.

// FaceFamilies are the font-family lists the kit faces and the system stacks
// resolve to. The kit faces are the artifact kit's own (@crewlethq/tokens
// `font.family.sans` and `font.family.mono`, which the kit serves as Geist and
// Geist Mono from the page's own origin); the stacks are curated system faces
// that need no file.
var FaceFamilies = map[string]string{
	"geist": "Geist, -apple-system, BlinkMacSystemFont, Segoe UI, Noto Sans, Helvetica, Arial, " +
		"sans-serif, Apple Color Emoji, Segoe UI Emoji",
	"geist-mono": "Geist Mono, ui-monospace, SFMono-Regular, SF Mono, Menlo, Consolas, " +
		"Liberation Mono, monospace",
	"system-sans": "system-ui, -apple-system, BlinkMacSystemFont, Segoe UI, Roboto, Noto Sans, " +
		"Helvetica Neue, Arial, sans-serif",
	"system-serif": "ui-serif, Iowan Old Style, Palatino Linotype, Palatino, Georgia, Cambria, " +
		"Times New Roman, serif",
	"system-mono": "ui-monospace, SFMono-Regular, SF Mono, Menlo, Consolas, Liberation Mono, monospace",
}

// uploadedFallback is what follows an uploaded family in its list: the system
// sans stack, drawn while the face loads and wherever it cannot.
const uploadedFallback = "system-sans"

// FamilyPattern is an uploaded family's name: an ASCII letter, then letters,
// digits, single spaces and hyphens. It is the slide format's family grammar
// narrowed (no `_`, no run of spaces), so every brand face can also be a deck's
// face; its length is held apart, to [MaxFamilyNameBytes] ([ValidFamily]), so
// the dashboard's copy of the pattern (BRAND_FAMILY_NAME_PATTERN) needs no
// bounded repetition to agree with it.
var FamilyPattern = regexp.MustCompile(`^[A-Za-z](?:[A-Za-z0-9-]| [A-Za-z0-9-])*$`)

// ValidFamily reports an uploaded family's name is inside its grammar and its
// bound.
func ValidFamily(name string) bool {
	return len(name) <= MaxFamilyNameBytes && FamilyPattern.MatchString(name)
}

// The weights a face's file may draw: the CSS whole hundreds.
const (
	MinFontWeight  = 100
	MaxFontWeight  = 900
	FontWeightStep = 100
)

// ValidWeight reports w is a weight a face's file may draw.
func ValidWeight(w int) bool {
	return w >= MinFontWeight && w <= MaxFontWeight && w%FontWeightStep == 0
}

// FontStyles are the styles a face's file may draw.
var FontStyles = []string{"normal", "italic"}

// FontStack is the font-family list a face resolves to: a kit face's or a
// system stack's list, or an uploaded family's name, quoted, before the system
// sans stack. A face naming none of the three resolves to "" ([Read] refuses
// such a face).
func FontStack(f Face) string {
	switch {
	case f.Kit != "":
		return FaceFamilies[f.Kit]
	case f.Stack != "":
		return FaceFamilies[f.Stack]
	case f.Family != "":
		return `"` + f.Family + `", ` + FaceFamilies[uploadedFallback]
	}
	return ""
}
