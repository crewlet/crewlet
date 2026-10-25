package brandformat

import (
	"fmt"
	"math"
	"strconv"
)

// ---- colour and contrast -------------------------------------------------- //
//
// CONTRAST IS JUDGED ON WHAT A PAGE PAINTS, so on the EFFECTIVE colours: every
// role the kit leaves out takes the value the artifact kit paints in its place
// ([Fallbacks]), and a pair whose two sides a reader never sees together is not
// measured. Dark mode keeps the theme's own text (the kit restyles only the
// light mode's), so the dark pair measures that text, not a brand role; and the
// label on the accent is whichever of black and white contrasts more, as the
// kit's `--color-text-on-accent` picks it.
//
// THE MATHS IS WCAG 2's, exactly as @crewlethq/tokens' `contrast()` does it
// (the 0.04045 linearisation threshold, the 0.05 flare, the ratio compared
// unrounded), so the Brand page's badges, which call that function on the
// same fallbacks (the dashboard contract's BRAND_COLOR_FALLBACKS and
// BRAND_CONTRAST_PAIRS, gated to [Fallbacks] and [ContrastPairs]), and the
// adoption's refusal agree to the last digit.

// The pseudo-roles a [ContrastPair] may name beside the colour roles.
const (
	// RoleOnPrimary is the label on the accent: whichever of white and black
	// contrasts more with the primary.
	RoleOnPrimary = "on_primary"

	// RoleDarkText is the body text of the dark mode, which keeps the kit's
	// own ([Fallbacks].DarkText).
	RoleDarkText = "dark_text"
)

// Fallback is the value the artifact kit paints for a role a kit leaves out:
// the default Crewlet theme's own token, so a brand with no `text` is measured
// against the text a page actually shows.
type Fallback struct {
	// Text is the light mode's body text (`color.text.primary`).
	Text string `json:"text"`

	// LightGround and DarkGround are the page grounds of the two modes
	// (`color.surface.background`).
	LightGround string `json:"light_ground"`
	DarkGround  string `json:"dark_ground"`

	// DarkText is the dark mode's body text, which a brand never restyles.
	DarkText string `json:"dark_text"`
}

// Fallbacks are the Crewlet theme's values (@crewlethq/tokens, theme
// `crewlet`), which the kit paints wherever a brand sets no input. An absent
// `primary_on_dark` takes the primary, as the kit's brand layer does.
var Fallbacks = Fallback{
	Text:        "#121625",
	LightGround: "#f6f7fb",
	DarkGround:  "#0c0e16",
	DarkText:    "#e7eaf3",
}

// ContrastPair is one pair of colours an adoption measures: a foreground role,
// a ground role, and the least ratio it should reach.
type ContrastPair struct {
	Foreground string  `json:"foreground"`
	Ground     string  `json:"ground"`
	Min        float64 `json:"min"`

	// Refuses marks the one pair below which an adoption is refused rather
	// than warned about.
	Refuses bool `json:"refuses"`
}

// ContrastPairs are the pairs an adoption reports, in the order the adopt
// dialog lists them. Only the first, `text` on `light_ground`, refuses an
// adoption ([MinTextContrast]); the rest warn. The 4.5 floors are WCAG AA for
// body text, the 3 floors AA for a graphical mark (the accent as a rule, an
// icon or a large heading on a ground).
var ContrastPairs = []ContrastPair{
	{Foreground: "text", Ground: "light_ground", Min: MinTextContrast, Refuses: true},
	{Foreground: RoleOnPrimary, Ground: "primary", Min: 4.5},
	{Foreground: RoleDarkText, Ground: "dark_ground", Min: 4.5},
	{Foreground: "primary", Ground: "light_ground", Min: 3},
	{Foreground: "primary_on_dark", Ground: "dark_ground", Min: 3},
}

// ContrastResult is one measured pair.
type ContrastResult struct {
	ContrastPair

	// ForegroundColor and GroundColor are the effective `#rrggbb` values
	// measured.
	ForegroundColor string `json:"foreground_color"`
	GroundColor     string `json:"ground_color"`

	// Ratio is the WCAG 2 contrast ratio, unrounded.
	Ratio float64 `json:"ratio"`
}

// Passes reports the pair reaches its floor, judged on the unrounded ratio.
func (r ContrastResult) Passes() bool { return r.Ratio >= r.Min }

// Effective is a kit's colours with every optional role filled as the kit
// paints it: [Fallbacks] for the text and the grounds, the primary for an
// absent `primary_on_dark`. An absent `secondary` stays empty: the kit mixes
// its place from the primary rather than painting one colour.
func Effective(b Brand) Colors {
	c := b.Colors
	if c.Text == "" {
		c.Text = Fallbacks.Text
	}
	if c.LightGround == "" {
		c.LightGround = Fallbacks.LightGround
	}
	if c.DarkGround == "" {
		c.DarkGround = Fallbacks.DarkGround
	}
	if c.PrimaryOnDark == "" {
		c.PrimaryOnDark = c.Primary
	}
	return c
}

// Contrast measures every [ContrastPairs] pair on the kit's effective colours,
// in that order. A kit whose colours are outside [ColorPattern] is a kit
// [Read] refuses, and a pair naming such a colour is measured as black, which
// no caller reaches: the adoption reads the kit first.
func Contrast(b Brand) []ContrastResult {
	c := Effective(b)
	role := func(name string) string {
		switch name {
		case "primary":
			return c.Primary
		case "secondary":
			return c.Secondary
		case "text":
			return c.Text
		case "light_ground":
			return c.LightGround
		case "dark_ground":
			return c.DarkGround
		case "primary_on_dark":
			return c.PrimaryOnDark
		case RoleDarkText:
			return Fallbacks.DarkText
		case RoleOnPrimary:
			return OnColor(c.Primary)
		}
		return ""
	}
	out := make([]ContrastResult, len(ContrastPairs))
	for i, p := range ContrastPairs {
		fg, ground := role(p.Foreground), role(p.Ground)
		ratio, _ := Ratio(fg, ground)
		out[i] = ContrastResult{ContrastPair: p, ForegroundColor: fg, GroundColor: ground, Ratio: ratio}
	}
	return out
}

// OnColor is whichever of white and black contrasts more with ground, white on
// a tie: the label a page sets on a fill of that colour.
func OnColor(ground string) string {
	white, _ := Ratio("#ffffff", ground)
	black, _ := Ratio("#000000", ground)
	if black > white {
		return "#000000"
	}
	return "#ffffff"
}

// Ratio is the WCAG 2 contrast ratio of two `#rrggbb` colours, from 1 to 21,
// unrounded.
func Ratio(a, b string) (float64, error) {
	la, err := Luminance(a)
	if err != nil {
		return 0, err
	}
	lb, err := Luminance(b)
	if err != nil {
		return 0, err
	}
	hi, lo := math.Max(la, lb), math.Min(la, lb)
	return (hi + 0.05) / (lo + 0.05), nil
}

// Luminance is a `#rrggbb` colour's WCAG 2 relative luminance, from 0 (black)
// to 1 (white).
func Luminance(hex string) (float64, error) {
	if !ColorPattern.MatchString(hex) {
		return 0, fmt.Errorf("brandformat: %q is not a colour: write # and six lowercase hex digits", hex)
	}
	n, err := strconv.ParseUint(hex[1:], 16, 32)
	if err != nil {
		return 0, fmt.Errorf("brandformat: %q is not a colour: %w", hex, err)
	}
	channel := func(v uint64) float64 {
		c := float64(v) / 255
		if c <= 0.04045 {
			return c / 12.92
		}
		return math.Pow((c+0.055)/1.055, 2.4)
	}
	r, g, bl := channel(n>>16&0xff), channel(n>>8&0xff), channel(n&0xff)
	return 0.2126*r + 0.7152*g + 0.0722*bl, nil
}
