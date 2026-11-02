package artifactupload_test

import (
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts/brandformat"
	"github.com/crewlet/crewlet/internal/artifactupload/lint"
)

// THE GP-38 REPLAY (ADR-0059). On the GoPhrase engine a designer seat (turn
// 01a17c82-716c-7cd8-bdc4-99cfda5c47a9) spent rounds 11 to 24 of one turn on
// create_artifact refusals, learning one fault of its brand kit per attempt.
// These are the faults it met, in one version, published through the real
// pipeline: every one comes back in ONE refusal, and the version with them
// fixed lands on the next attempt.

// gp38Faulty is the GP-38 kit as the model's attempts wrote it, every fault
// at once: no `v`, keys the format does not carry at three depths (fonts,
// colors.background, type.mono, type.heading.weights, voice.x, logos.zzz), an
// object where a colour and a logo path go, a string where a face goes, a face
// naming two sources, a voice line past its bound, a drawing with a
// stylesheet, and a notes file nothing names. Its summary names the brand's
// own domain, which is not a fault.
func gp38Faulty() map[string]string {
	return map[string]string{
		brandformat.EntryPath: `{
  "name": "GoPhrase",
  "fonts": {"body": "Inter"},
  "logos": {"primary": {"path": "logos/primary.svg"}, "mark": "logos/mark.svg", "zzz": "x"},
  "colors": {"primary": {"hex": "#2450e6"}, "background": "#ffffff", "text": "#14161a"},
  "type": {"heading": {"kit": "geist", "family": "GoPhrase Sans", "weights": [400]},
    "body": "Inter", "mono": {"stack": "system-mono"}},
  "voice": {"summary": "Friendly and exact, as GoPhrase.me speaks to learners.",
    "write_like": ["Find the phrase.", "Hear it said.", "` + strings.Repeat("a", 223) + `"],
    "x": "probe"}
}`,
		"logos/primary.svg": authoredLogo,
		"logos/mark.svg": `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 50 50">` +
			`<style>circle{fill:#2450e6}</style><circle cx="25" cy="25" r="20"/></svg>`,
		"NOTES.md": "draft notes",
	}
}

// gp38Fixed is the same kit with every finding of the refusal acted on.
func gp38Fixed() map[string]string {
	return map[string]string{
		brandformat.EntryPath: `{
  "v": 1,
  "name": "GoPhrase",
  "logos": {"primary": "logos/primary.svg", "mark": "logos/mark.svg"},
  "colors": {"primary": "#2450e6", "light_ground": "#ffffff", "text": "#14161a"},
  "type": {"heading": {"kit": "geist"}, "body": {"stack": "system-sans"}},
  "voice": {"summary": "Friendly and exact, as GoPhrase.me speaks to learners.",
    "write_like": ["Find the phrase.", "Hear it said."]}
}`,
		"logos/primary.svg": authoredLogo,
		"logos/mark.svg": `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 50 50">` +
			`<circle cx="25" cy="25" r="20" fill="#2450e6"/></svg>`,
	}
}

// Mutation: stop the decode at its first fault, skip the field rules while a
// structural fault stands, or answer the drawing's refusal without the index's
// findings, and a finding below goes missing from the first refusal.
func TestTheGP38KitGetsEveryFaultInOneRefusalAndLandsNext(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	_, err := createKit(h, gp38Faulty())
	var refusal *lint.Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("the faulty kit was answered %v, want one policy refusal", err)
	}
	var lines []string
	for _, f := range refusal.Findings {
		lines = append(lines, f.String())
	}
	all := strings.Join(lines, "\n")
	for _, want := range []string{
		"v: is 0, and this build reads brand format 1",
		"fonts is not a key the brand format carries: the top level of brand.json takes v, name, tagline, logos, colors, type, voice",
		"logos.primary is an object, and the format reads a string there",
		"logos.zzz is not a key the brand format carries: logos takes primary, on_dark, mark",
		"colors.primary is an object, and the format reads a string there",
		"colors.background is not a key the brand format carries",
		"type.heading.weights is not a key the brand format carries: type.heading takes kit, stack, family, files, licensed",
		"type.body is a string, and the format reads an object {kit, stack, family, files, licensed} there",
		"type.mono is not a key the brand format carries: type takes heading, body",
		"voice.x is not a key the brand format carries: voice takes summary, write_like, avoid",
		"type.heading: names kit and family",
		"voice.write_like[2]: is 223 bytes and may be at most 160",
		"logos/mark.svg",
		"NOTES.md: reference: brand.json names no logo or face at this path",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("the one refusal does not say %q; it said:\n%s", want, all)
		}
	}
	if strings.Contains(all, "GoPhrase.me") {
		t.Errorf("the brand's own domain was refused:\n%s", all)
	}
	if _, err := createKit(h, gp38Fixed()); err != nil {
		t.Fatalf("the kit with every finding fixed was refused: %v", err)
	}
}

var referenceExample = regexp.MustCompile("(?s)```json\n(.*?)\n```")

// A MODEL THAT READS get_artifact_format FIRST LANDS AT ONCE: the brand
// reference's example, with the logo files it names, is published through the
// real pipeline on the first attempt.
//
// Mutation: tighten a rule without updating the reference, and the first
// attempt is refused.
func TestAKitWrittenFromTheFormatReferenceLandsFirstTime(t *testing.T) {
	t.Parallel()
	m := referenceExample.FindStringSubmatch(brandformat.Reference())
	if m == nil {
		t.Fatal("the brand reference carries no example")
	}
	b, err := brandformat.Decode([]byte(m[1]))
	if err != nil {
		t.Fatalf("the reference's example does not decode: %v", err)
	}
	files := map[string]string{brandformat.EntryPath: m[1]}
	for _, p := range []string{b.Logos.Primary, b.Logos.OnDark, b.Logos.Mark} {
		if p != "" {
			files[p] = authoredLogo
		}
	}
	if _, err := createKit(newHarness(t, options{}), files); err != nil {
		t.Fatalf("a kit written from the reference was refused: %v", err)
	}
}
