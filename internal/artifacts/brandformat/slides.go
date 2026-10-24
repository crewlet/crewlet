package brandformat

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/artifacts/slideformat"
)

// ---- the slide theme ------------------------------------------------------ //
//
// A DECK IS COPIED FROM THE BRAND AT ITS CREATION, never restyled after: a
// slide holds its styles inline (internal/artifacts/slideformat), so the
// brand's slide theme is a theme like the starter's own, its every value a
// slide's own content. Its colours are derived from the kit's effective
// colours, and its faces are the kit's lists read through the slide format's
// family grammar, which is narrower than a stylesheet's (a name begins with a
// letter, at most eight names): `-apple-system` is dropped there, never
// quoted into a family that does not exist.

// slideFamily is one name the slide format's font-family grammar takes.
var slideFamily = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9 _-]{0,39}$`)

// maxSlideFamilies is how many names a slide's font-family list holds.
const maxSlideFamilies = 8

// SlideTheme is the theme a deck begun in the company's brand is drawn in:
// covers and section slides on the dark ground with an ink that reads on it,
// every other slide on the light ground in the brand's text, the accent the
// brand's primary, cards and hairlines mixed from the ground and the text, and
// the heading and body faces the brand's.
func SlideTheme(b Brand) slideformat.Theme {
	c := Effective(b)
	featureInk := Fallbacks.DarkText
	if r, _ := Ratio(featureInk, c.DarkGround); r < 4.5 {
		featureInk = OnColor(c.DarkGround)
	}
	return slideformat.Theme{
		ID: slideformat.BrandTheme, Name: b.Name, Tag: "Your company brand",

		Feature: c.DarkGround, FeatureInk: featureInk, FeatureMuted: mix(featureInk, c.DarkGround, 0.3),

		Stage: c.LightGround, Ink: c.Text, Muted: mix(c.Text, c.LightGround, 0.35),
		Accent: c.Primary,
		Card:   mix(c.LightGround, "#ffffff", 0.6), Edge: mix(c.Text, c.LightGround, 0.86),

		Display: slideStack(b.Type.Heading), Body: slideStack(b.Type.Body),

		DisplayWeight: 600, Tracking: -1,
	}
}

// slideStack is a face's list read through the slide format's family grammar.
func slideStack(f Face) string {
	var out []string
	for _, name := range strings.Split(FontStack(f), ",") {
		name = strings.Trim(strings.TrimSpace(name), `"`)
		if slideFamily.MatchString(name) && len(out) < maxSlideFamilies {
			out = append(out, name)
		}
	}
	if len(out) == 0 {
		return "sans-serif"
	}
	return strings.Join(out, ", ")
}

// mix is a linear blend of two `#rrggbb` colours in sRGB, t of the way from a
// to b, as `#rrggbb`. A colour outside the grammar mixes as black, which no
// caller passes: the brand is read first.
func mix(a, b string, t float64) string {
	ca, cb := channels(a), channels(b)
	var out [3]int
	for i := range out {
		out[i] = int(math.Round(float64(ca[i])*(1-t) + float64(cb[i])*t))
	}
	return fmt.Sprintf("#%02x%02x%02x", out[0], out[1], out[2])
}

func channels(hex string) [3]int {
	if !ColorPattern.MatchString(hex) {
		return [3]int{}
	}
	n, _ := strconv.ParseUint(hex[1:], 16, 32)
	return [3]int{int(n >> 16 & 0xff), int(n >> 8 & 0xff), int(n & 0xff)}
}
