package brandformat_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts/brandformat"
	"github.com/crewlet/crewlet/internal/artifacts/slideformat"
)

// THE BRAND'S SLIDE THEME IS INSIDE THE SLIDE FORMAT: every colour and both
// face lists it derives are set on a slide the slide format reads without a
// finding, for a kit face, a system stack and an uploaded family alike, and a
// name the slide grammar refuses (`-apple-system`) is dropped from the list
// rather than quoted into a family that does not exist.
//
// Mutation: hand the slide theme FontStack's list unfiltered and the slide is
// refused for `-apple-system`.
func TestTheBrandsSlideThemeIsInsideTheSlideFormat(t *testing.T) {
	t.Parallel()
	for name, b := range map[string]brandformat.Brand{
		"honest": func() brandformat.Brand {
			b, err := brandformat.Read(kit(t))
			if err != nil {
				t.Fatal(err)
			}
			return b
		}(),
		"defaults": {Name: "Plain", Logos: brandformat.Logos{Primary: "l.svg"},
			Colors: brandformat.Colors{Primary: "#ffd400"},
			Type: brandformat.Type{Heading: brandformat.Face{Stack: "system-serif"},
				Body: brandformat.Face{Stack: "system-mono"}}},
		"unset faces": {Name: "Unset", Logos: brandformat.Logos{Primary: "l.svg"},
			Colors: brandformat.Colors{Primary: "#2450e6"}},
		"pale dark ground": {Name: "Pale", Logos: brandformat.Logos{Primary: "l.svg"},
			Colors: brandformat.Colors{Primary: "#2450e6", DarkGround: "#dddddd"},
			Type:   brandformat.Type{Heading: brandformat.Face{Kit: "geist-mono"}, Body: brandformat.Face{Kit: "geist"}}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			th := brandformat.SlideTheme(adoptable(t, b))
			if th.ID != slideformat.BrandTheme || th.Name != b.Name || th.Tag == "" {
				t.Errorf("the theme is named %q %q %q", th.ID, th.Name, th.Tag)
			}
			for _, face := range []string{th.Display, th.Body} {
				if strings.Contains(face, "-apple-system") || strings.Contains(face, `"`) {
					t.Errorf("a face list carries a name the slide grammar refuses: %q", face)
				}
			}
			if r, _ := brandformat.Ratio(th.FeatureInk, th.Feature); r < 4.5 {
				t.Errorf("the feature ink %s reads at %.2f on %s", th.FeatureInk, r, th.Feature)
			}
			slide := fmt.Sprintf(`<section id="s" style="background:%s;color:%s;font-family:%s">`+
				`<h1 style="color:%s;font-family:%s;font-weight:%d;letter-spacing:%dpx">T</h1>`+
				`<p style="color:%s">a</p><p style="color:%s">b</p>`+
				`<div style="background:%s;border:2px solid %s"><p style="color:%s">c</p></div>`+
				`<div style="background:%s"><p style="color:%s">d</p></div></section>`,
				th.Stage, th.Ink, th.Body, th.Accent, th.Display, th.DisplayWeight, th.Tracking,
				th.Muted, th.FeatureMuted, th.Card, th.Edge, th.Ink, th.Feature, th.FeatureInk)
			_, findings := slideformat.ParseSlide("slides/s.html", "s", []byte(slide),
				func(string) (string, bool) { return "", false })
			if len(findings) > 0 {
				t.Errorf("the brand's slide theme is outside the slide format: %v\n%s", findings, slide)
			}
		})
	}
}
