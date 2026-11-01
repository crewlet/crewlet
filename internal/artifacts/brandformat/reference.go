package brandformat

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/artifacts/svgsafe"
	"github.com/crewlet/crewlet/internal/jsonform"
	"github.com/crewlet/crewlet/internal/words"
)

// ---- the reference -------------------------------------------------------- //
//
// THE FORMAT AN AGENT READS BEFORE IT WRITES A KIT, built from what [Read]
// holds a kit to: the keys and their types are [IndexForm] (the strict
// decode's own tree), every bound is the constant the rule checks against,
// every closed set is the list the rule reads, and the example is a [Brand]
// value a test reads back through [Decode]. Nothing here is written twice, so
// the reference cannot promise a key, a bound or a value the reader refuses.

// fieldRules is the rule each key of the index is held to, keyed by its path in
// [IndexForm] (`[]` is every element of a list). A test holds the keys to the
// form's paths, both ways.
func fieldRules() map[string]string {
	face := func(slot string) map[string]string {
		p := "type." + slot
		return map[string]string{
			p: fmt.Sprintf("the %s face: {} (unset, the kit's own face) or EXACTLY ONE of kit, stack "+
				"and family", slot),
			p + ".kit":      "a face the artifact kit ships: " + jsonform.OneOf(KitFaces),
			p + ".stack":    "a system font stack: " + jsonform.OneOf(SystemStacks),
			p + ".family":   familyRule(),
			p + ".files":    fmt.Sprintf("an uploaded family's WOFF2 files, 1 to %d, each its own weight and style", MaxFontFilesPerFamily),
			p + ".licensed": "true with an uploaded family: you attest the company may embed it in what it publishes",
			p + ".files[].path": fmt.Sprintf("the WOFF2 file's path in the version (font/woff2, at most %s)",
				size(MaxFontFileBytes)),
			p + ".files[].weight": fmt.Sprintf("%d to %d in steps of %d", MinFontWeight, MaxFontWeight, FontWeightStep),
			p + ".files[].style":  jsonform.OneOf(FontStyles),
		}
	}
	logo := func(what string) string {
		return what + ": the path of an SVG, PNG or WebP file of the version"
	}
	colour := func(what string) string {
		return what + ": `#` and six lowercase hex digits, such as #2450e6"
	}
	rules := map[string]string{
		"v":    fmt.Sprintf("required, always %d", Version),
		"name": fmt.Sprintf("required: the company's name as the brand shows it, plain text, 1 to %d bytes; it may be a domain (\"Booking.com\")", MaxNameBytes),
		"tagline": fmt.Sprintf("one line under the name, plain text, at most %d bytes",
			MaxTaglineBytes),
		"logos":                  "the logo files; each optional in a draft",
		"logos.primary":          logo("the logo on a light ground, required to adopt"),
		"logos.on_dark":          logo("the logo on a dark ground; absent, a dark ground shows the primary"),
		"logos.mark":             logo("the square symbol alone, for a chip, a favicon or a slide's corner"),
		"colors":                 "the colour roles; an absent one takes the artifact kit's fallback",
		"colors.primary":         colour("the brand's accent, required to adopt"),
		"colors.secondary":       colour("the supporting accent"),
		"colors.text":            colour(fmt.Sprintf("body text on light_ground (contrast at least %.1f:1 to adopt)", MinTextContrast)),
		"colors.light_ground":    colour("the light theme's page ground"),
		"colors.dark_ground":     colour("the dark theme's page ground"),
		"colors.primary_on_dark": colour("the accent on the dark ground; absent, derived from primary"),
		"type":                   "the two typefaces",
		"voice":                  "how the company writes, which agents follow for tone and wording",
		"voice.summary": fmt.Sprintf("plain text, at most %d bytes, newlines allowed",
			MaxVoiceSummaryBytes),
		"voice.write_like": fmt.Sprintf("example lines to write like: at most %d, each non-empty "+
			"plain text on one line of at most %d bytes", MaxVoiceExamples, MaxVoiceExampleBytes),
		"voice.avoid": fmt.Sprintf("lines never to write: at most %d, each non-empty plain text "+
			"on one line of at most %d bytes", MaxVoiceExamples, MaxVoiceExampleBytes),
	}
	for _, slot := range []string{"heading", "body"} {
		for k, v := range face(slot) {
			rules[k] = v
		}
	}
	return rules
}

// Reference is the brand format as an agent reads it before writing a kit:
// every key of `brand.json` with its type and rule, the rules across keys, the
// file rules, and an example that reads clean.
func Reference() string {
	var b strings.Builder
	fmt.Fprintf(&b, "# A brand kit: %s, brand format %d\n\n", EntryPath, Version)
	b.WriteString("A kit is `" + EntryPath + "` at the version's root plus the logo and font " +
		"files it names, and nothing else: a file " + EntryPath + " does not name (a README, " +
		"notes) is refused. Every key below is the whole format: a key not listed is refused, " +
		"and so is a value of another type. `[]` is each element of a list.\n\n")
	b.WriteString("## Every key\n\n")
	b.WriteString(IndexForm.Outline(fieldRules()))
	b.WriteString("\n## Rules across keys\n\n")
	fmt.Fprintf(&b, "- A draft needs only `v` and `name`; every other key is optional, and "+
		"every key present is held to its rule. To adopt, a kit also needs %s.\n",
		adoptionList())
	b.WriteString("- A face is `{}` (unset) or names exactly one source: `{\"kit\": \"geist\"}`, " +
		"`{\"stack\": \"system-serif\"}`, or an uploaded family with its files and the licence " +
		"attestation, such as " + exampleFamily() + ". A kit face or a stack carries no files " +
		"and no `licensed`.\n")
	b.WriteString("- Text (name, tagline, voice) is plain text: Unicode normal form C, no " +
		"leading or trailing white space, no `<` or `>`, no control or invisible character, " +
		"and no link or address (no scheme, `www.`, mail address, Markdown link or " +
		"`host/path`). A bare domain is refused too, except the brand's own name: for the " +
		"brand `GoPhrase`, `GoPhrase.me` may be written, and `promo.gophrase.me` may not.\n")
	b.WriteString("\n## The files it names\n\n")
	fmt.Fprintf(&b, "- A logo is SVG (at most %s), PNG or WebP (at most %s, %d px a side, "+
		"%d pixels in all), with a width over height from 1:%d to %d:1. It is stored "+
		"without its metadata.\n", size(MaxSVGBytes), size(MaxImageBytes), MaxImageSide,
		MaxImagePixels, int(1/svgsafe.MinAspect), int(svgsafe.MaxAspect))
	fmt.Fprintf(&b, "- An SVG is a drawing alone: the root `<svg>` with a `viewBox`, and only "+
		"the elements %s. No `<style>` element, no `style` attribute (use presentation "+
		"attributes such as `fill`), no script or event attribute, no `<a>`, `<image>` or "+
		"`<foreignObject>`, no animation, no DOCTYPE or entity, and every reference "+
		"(`href`, `url(#id)`) to an id the same drawing declares.\n",
		strings.Join(svgsafe.Elements(), ", "))
	fmt.Fprintf(&b, "- A face's file is WOFF2, at most %s, with 1 to %d tables.\n",
		size(MaxFontFileBytes), MaxFontTables)
	b.WriteString("\n## An example that reads clean\n\n```json\n")
	b.WriteString(exampleIndex())
	b.WriteString("\n```\n")
	return b.String()
}

// exampleBrand is the reference's example: a draft every rule admits.
func exampleBrand() Brand {
	return Brand{
		V: Version, Name: "Acme", Tagline: "Tools for people who make things",
		Logos:  Logos{Primary: "logos/primary.svg", Mark: "logos/mark.svg"},
		Colors: Colors{Primary: "#2450e6", Text: "#14161a", LightGround: "#ffffff", DarkGround: "#0d0f14"},
		Type:   Type{Heading: Face{Kit: "geist"}, Body: Face{Stack: "system-sans"}},
		Voice: Voice{
			Summary:   "Plain, warm and exact. Write as Acme.io would: short sentences, no hype.",
			WriteLike: []string{"We fixed it on Tuesday."},
			Avoid:     []string{"Revolutionary"},
		},
	}
}

// exampleFace is the reference's uploaded family.
func exampleFace() Face {
	return Face{Family: "Acme Sans", Licensed: true, Files: []FontFile{
		{Path: "fonts/acme-sans-400.woff2", Weight: 400, Style: "normal"},
	}}
}

func exampleIndex() string {
	out, err := json.MarshalIndent(exampleBrand(), "", "  ")
	if err != nil {
		panic(fmt.Sprintf("brandformat: the reference's example does not encode: %v", err))
	}
	return string(out)
}

func exampleFamily() string {
	out, err := json.Marshal(exampleFace())
	if err != nil {
		panic(fmt.Sprintf("brandformat: the reference's family does not encode: %v", err))
	}
	return "`" + string(out) + "`"
}

func familyRule() string {
	return fmt.Sprintf("an uploaded family's name: an ASCII letter, then letters, digits, single "+
		"spaces and hyphens, at most %d bytes; it carries files and licensed: true", MaxFamilyNameBytes)
}

// adoptionList is every field an adoption needs, each written as the
// reference writes a key, with what it is ([words.Said]).
func adoptionList() string {
	parts := make([]words.Said, len(AdoptionFields))
	for i, f := range AdoptionFields {
		parts[i] = words.Said("`" + f.Name + "` (" + f.What + ")")
	}
	return words.And(parts)
}

// size is a byte bound as a reader says it.
func size(n int) string {
	switch {
	case n%(1<<20) == 0:
		return fmt.Sprintf("%d MiB", n>>20)
	case n%(1<<10) == 0:
		return fmt.Sprintf("%d KiB", n>>10)
	}
	return fmt.Sprintf("%d bytes", n)
}
