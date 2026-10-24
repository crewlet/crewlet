package brandformat

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/artifacts"
)

// EntryPath is a kit's index, its entry.
const EntryPath = "brand.json"

// Version is the one `brand.json` shape this build reads.
const Version = 1

// The bounds `brand.json` and its assets are held to. Each is the product's
// figure (FINAL B9, B11) and is refused at, never truncated to.
const (
	// MaxIndexBytes bounds `brand.json` itself, and so the canonical bytes an
	// adoption snapshots into its row.
	MaxIndexBytes = 32 << 10

	MaxNameBytes    = 80  // the company's name as a brand shows it
	MaxTaglineBytes = 140 // one line under the name

	// The voice: a summary a model reads in one breath, and a few short
	// lines of each list, so the fenced answer stays small.
	MaxVoiceSummaryBytes = 300
	MaxVoiceExamples     = 5
	MaxVoiceExampleBytes = 160

	// The logos.
	MaxSVGBytes    = 512 << 10
	MaxImageBytes  = 2 << 20
	MaxImageSide   = 4096
	MaxImagePixels = 16_777_216

	// The faces. A family's name is at most 40 bytes, the slide format's
	// family bound, so every brand face can also be a deck's. A family is at
	// most four files (regular, italic, and two weights), each at most 2 MiB,
	// enough for a CJK subset; the WOFF2 header is checked for a table count
	// of 1 to MaxFontTables and a decoded size of at most MaxFontSFNTBytes.
	MaxFamilyNameBytes    = 40
	MaxFontFilesPerFamily = 4
	MaxFontFileBytes      = 2 << 20
	MaxFontTables         = 64
	MaxFontSFNTBytes      = 8 << 20
)

// MinTextContrast is the contrast ratio `text` on `light_ground` must reach
// for an adoption (WCAG 2.2 AA for body text). Below it the adoption is refused
// (`brand_contrast`); a publish of a draft is not.
const MinTextContrast = 4.5

// ColorPattern is every colour value `brand.json` admits: `#` and six lowercase
// hex digits.
var ColorPattern = regexp.MustCompile(`^#[0-9a-f]{6}$`)

// KitFaces are the faces the artifact kit ships, which a kit may name with no
// file of its own.
var KitFaces = []string{"geist", "geist-mono"}

// SystemStacks are the curated system font stacks a kit may name.
var SystemStacks = []string{"system-sans", "system-serif", "system-mono"}

// Brand is `brand.json`: the closed index of a kit. A key not here is refused.
type Brand struct {
	// V is [Version].
	V int `json:"v"`

	// Name is the company's name as the brand shows it, 1 to [MaxNameBytes]
	// bytes of plain text.
	Name string `json:"name"`

	// Tagline is one optional line under the name, at most
	// [MaxTaglineBytes] bytes of plain text.
	Tagline string `json:"tagline,omitempty"`

	Logos  Logos  `json:"logos"`
	Colors Colors `json:"colors"`
	Type   Type   `json:"type"`
	Voice  Voice  `json:"voice"`
}

// Text is what a kit says, for the gallery's search: its name, its tagline
// and its voice, one line each.
func (b Brand) Text() string {
	parts := []string{b.Name, b.Tagline, b.Voice.Summary}
	parts = append(parts, b.Voice.WriteLike...)
	parts = append(parts, b.Voice.Avoid...)
	return strings.Join(slices.DeleteFunc(parts, func(s string) bool { return s == "" }), "\n")
}

// Logos are the kit's logo files, each the path of a file of the version.
type Logos struct {
	// Primary is the logo on a light ground. Required.
	Primary string `json:"primary"`

	// OnDark is the logo on a dark ground; absent, a dark ground shows the
	// primary.
	OnDark string `json:"on_dark,omitempty"`

	// Mark is the square symbol alone, for a chip, a favicon or a slide's
	// corner; absent, those places show the primary.
	Mark string `json:"mark,omitempty"`
}

// Colors are the kit's colour roles, each [ColorPattern]. An absent optional
// role takes the artifact kit's Crewlet fallback.
type Colors struct {
	// Primary is the brand's accent. Required.
	Primary string `json:"primary"`

	// Secondary is the supporting accent.
	Secondary string `json:"secondary,omitempty"`

	// Text is body text on the light ground.
	Text string `json:"text,omitempty"`

	// LightGround and DarkGround are the page grounds of the light and dark
	// themes.
	LightGround string `json:"light_ground,omitempty"`
	DarkGround  string `json:"dark_ground,omitempty"`

	// PrimaryOnDark is the accent on the dark ground, where the primary is
	// too dark to read; absent, the dark theme derives one from the primary.
	PrimaryOnDark string `json:"primary_on_dark,omitempty"`
}

// Type is the kit's two typefaces.
type Type struct {
	Heading Face `json:"heading"`
	Body    Face `json:"body"`
}

// Face is one typeface, from EXACTLY ONE source: a kit face, a system stack,
// or an uploaded family.
type Face struct {
	// Kit is one of [KitFaces].
	Kit string `json:"kit,omitempty"`

	// Stack is one of [SystemStacks].
	Stack string `json:"stack,omitempty"`

	// Family is an uploaded family's name, 1 to [MaxFamilyNameBytes] bytes:
	// an ASCII letter, then letters, digits, single spaces and hyphens. Its
	// Files are its WOFF2 files.
	Family string     `json:"family,omitempty"`
	Files  []FontFile `json:"files,omitempty"`

	// Licensed is the publisher's attestation that the company may embed
	// the uploaded family in what it publishes. Required true with Files.
	Licensed bool `json:"licensed,omitempty"`
}

// FontFile is one WOFF2 file of an uploaded family.
type FontFile struct {
	// Path is the file of the version.
	Path string `json:"path"`

	// Weight is the CSS weight it draws, 100 to 900 in steps of 100.
	Weight int `json:"weight"`

	// Style is `normal` or `italic`.
	Style string `json:"style"`
}

// Voice is how the company writes, which agents read when they write an
// artifact (through `get_company_brand`, fenced) and nowhere else.
type Voice struct {
	Summary   string   `json:"summary,omitempty"`
	WriteLike []string `json:"write_like,omitempty"`
	Avoid     []string `json:"avoid,omitempty"`
}

// Slot is one file the adopted brand serves, and what the `from_brand` file
// source names.
type Slot string

// The slots.
const (
	// SlotCSS is the stylesheet generated at adoption: `--brand-*` custom
	// properties and the `@font-face` rules naming the faces' files.
	SlotCSS Slot = "css"

	SlotLogoPrimary Slot = "logo.primary"
	SlotLogoOnDark  Slot = "logo.on_dark"
	SlotLogoMark    Slot = "logo.mark"

	// SlotFontHeading and SlotFontBody name a face in `from_brand`, which
	// copies the face's regular file (artifactupload.FaceFile), as a deck
	// names a face by one file; each file of an uploaded family is also its
	// own slot under [FontFileSlot], which the brand files table keys it by.
	SlotFontHeading Slot = "font.heading"
	SlotFontBody    Slot = "font.body"
)

// SourceSlots are the slots the `from_brand` file source accepts, in the order
// `get_company_brand` lists them.
var SourceSlots = []Slot{SlotLogoPrimary, SlotLogoOnDark, SlotLogoMark, SlotFontHeading, SlotFontBody}

// FontFileSlot is the slot one file of a face is kept under in the adopted
// brand's file table: the face's slot and the file's index in its Files.
func FontFileSlot(face Slot, index int) Slot {
	return Slot(fmt.Sprintf("%s.%d", face, index))
}

// Source is what the validator reads a version through.
type Source = artifacts.FileSource

// Rule is which rule a [Finding] names.
type Rule string

// The rules a kit can break.
const (
	RuleIndex     Rule = "index"     // brand.json is not the closed index: unparseable, an unknown key, a wrong version
	RuleField     Rule = "field"     // a field outside its grammar: a colour, a name, a weight, a face naming two sources
	RuleBound     Rule = "bound"     // a count or a size past its bound
	RuleReference Rule = "reference" // a path the version does not hold
	RuleAsset     Rule = "asset"     // a logo or a face outside its asset rules
	RuleLicence   Rule = "licence"   // an uploaded family with no licence attestation
)

// Finding is one way a kit is outside the format: the file, the line where
// there is one, the rule and what to change.
type Finding struct {
	Path    string `json:"path"`
	Line    int    `json:"line,omitempty"`
	Rule    Rule   `json:"rule"`
	Message string `json:"message"`
}

func (f Finding) String() string {
	if f.Line > 0 {
		return fmt.Sprintf("%s:%d: %s: %s", f.Path, f.Line, f.Rule, f.Message)
	}
	return fmt.Sprintf("%s: %s: %s", f.Path, f.Rule, f.Message)
}

// Refused is a kit outside the format: every finding, in file order, so an
// author fixes them in one pass.
type Refused struct {
	Findings []Finding
}

// ErrRefused is every refusal this package answers, wrapped by [*Refused], so
// a reader of a stored kit tells a kit outside the format from a file it could
// not read.
var ErrRefused = errors.New("brandformat: refused")

// Unwrap makes every refusal an [ErrRefused].
func (r *Refused) Unwrap() error { return ErrRefused }

func (r *Refused) Error() string {
	lines := make([]string, len(r.Findings))
	for i, f := range r.Findings {
		lines[i] = f.String()
	}
	return "brandformat: the kit is outside the brand format:\n" + strings.Join(lines, "\n")
}
