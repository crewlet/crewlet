package brandformat_test

import (
	"math"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts/brandformat"
)

// THE RATIO IS WCAG 2's, TO THE DIGIT @crewlethq/tokens GIVES: the extremes,
// a grey that sits just under AA (the case a rounded ratio would pass), and
// the order of the two colours never mattering.
//
// Mutation: linearise at 0.03928, or drop the 0.05 flare, and the grey moves.
func TestTheRatioIsWCAGsUnrounded(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		a, b string
		want float64
	}{
		{"#ffffff", "#000000", 21},
		{"#000000", "#ffffff", 21},
		{"#777777", "#ffffff", 4.478089453577214},
		{"#121625", "#f6f7fb", 16.805960612212946},
		{"#735af2", "#735af2", 1},
	} {
		got, err := brandformat.Ratio(c.a, c.b)
		if err != nil {
			t.Fatalf("Ratio(%s, %s): %v", c.a, c.b, err)
		}
		if math.Abs(got-c.want) > 1e-9 {
			t.Errorf("Ratio(%s, %s) = %.15f, want %.15f", c.a, c.b, got, c.want)
		}
	}
	if _, err := brandformat.Ratio("#FFFFFF", "#000000"); err == nil {
		t.Error("an uppercase colour was measured: brand colours are lowercase hex")
	}
}

// CONTRAST IS MEASURED ON WHAT A PAGE PAINTS: an absent text and grounds are
// the kit's own values, the label on the accent is the better of black and
// white, and the pairs come back in the order the adopt dialog lists them,
// only the first refusing.
//
// Mutation: measure an absent text as "" (black) and the first pair reads 19
// rather than the Crewlet text's ratio.
func TestContrastIsMeasuredOnTheEffectiveColours(t *testing.T) {
	t.Parallel()
	b := brandformat.Brand{Colors: brandformat.Colors{Primary: "#ffd400"}}
	got := brandformat.Contrast(b)
	if len(got) != len(brandformat.ContrastPairs) {
		t.Fatalf("%d pairs measured, want %d", len(got), len(brandformat.ContrastPairs))
	}
	for i, r := range got {
		if r.ContrastPair != brandformat.ContrastPairs[i] {
			t.Errorf("pair %d is %+v, want %+v", i, r.ContrastPair, brandformat.ContrastPairs[i])
		}
		if r.Refuses != (i == 0) {
			t.Errorf("pair %d (%s on %s) refuses = %v", i, r.Foreground, r.Ground, r.Refuses)
		}
	}
	text := got[0]
	if text.ForegroundColor != brandformat.Fallbacks.Text || text.GroundColor != brandformat.Fallbacks.LightGround {
		t.Errorf("an absent text was measured as %s on %s, want the kit's %s on %s",
			text.ForegroundColor, text.GroundColor, brandformat.Fallbacks.Text, brandformat.Fallbacks.LightGround)
	}
	if !text.Passes() {
		t.Errorf("the kit's own text on its own ground fails at %.2f", text.Ratio)
	}
	if label := got[1]; label.ForegroundColor != "#000000" || !label.Passes() {
		t.Errorf("the label on a yellow accent is %s at %.2f, want black, passing",
			label.ForegroundColor, label.Ratio)
	}
	if onDark := got[4]; onDark.ForegroundColor != "#ffd400" {
		t.Errorf("an absent primary_on_dark was measured as %s, want the primary", onDark.ForegroundColor)
	}

	pale := brandformat.Brand{Colors: brandformat.Colors{Primary: "#2450e6", Text: "#999999"}}
	if r := brandformat.Contrast(pale)[0]; r.Passes() {
		t.Errorf("a grey text on the light ground passes at %.2f", r.Ratio)
	}
}

// EVERY FACE RESOLVES TO ONE LIST THAT CARRIES NO SYNTAX: the kit faces and
// the system stacks are in the table and hold nothing a stylesheet reads as
// syntax, and an uploaded family is quoted before the system sans list.
//
// Mutation: quote a kit face in the table and the syntax check fails.
func TestEveryFaceResolvesToOneListThatCarriesNoSyntax(t *testing.T) {
	t.Parallel()
	for _, name := range append(append([]string(nil), brandformat.KitFaces...), brandformat.SystemStacks...) {
		list := brandformat.FaceFamilies[name]
		if list == "" {
			t.Errorf("face %s resolves to nothing", name)
			continue
		}
		if strings.ContainsAny(list, `"';{}()<>\`) {
			t.Errorf("face %s's list %q carries a character a stylesheet reads as syntax", name, list)
		}
	}
	if len(brandformat.FaceFamilies) != len(brandformat.KitFaces)+len(brandformat.SystemStacks) {
		t.Errorf("the table resolves %d faces, want exactly the kit faces and the stacks",
			len(brandformat.FaceFamilies))
	}
	got := brandformat.FontStack(brandformat.Face{Family: "Acme Sans 2"})
	if want := `"Acme Sans 2", ` + brandformat.FaceFamilies["system-sans"]; got != want {
		t.Errorf("an uploaded family resolves to %q, want %q", got, want)
	}
	if got := brandformat.FontStack(brandformat.Face{}); got != "" {
		t.Errorf("a face naming no source resolves to %q", got)
	}
}
