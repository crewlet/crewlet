package brandformat

import "fmt"

// ---- what an adopted brand holds ------------------------------------------ //
//
// A DRAFT NEEDS ITS NAME, AN ADOPTED BRAND ITS LOGO AND ITS COLOUR. A kit is
// published step by step while it is set up, so [Read] requires only `name`
// and holds every field that is present to its rule; what the company's
// brand must hold is decided where the brand is adopted, as its contrast is:
// [MissingToAdopt] names the fields an adoption refuses a brand for leaving
// out, and the adoption (internal/artifactsdomain's CheckBrandAdoptable)
// refuses it naming each one.
//
// WHAT AN ADOPTED BRAND MAKES IS COMPUTED ONLY FROM A COMPLETE ONE. The
// stylesheet ([CSS]) and the slide theme ([SlideTheme]) take an [Adoptable],
// which only [AsAdoptable] and [DecodeAdopted] make, and only for a brand
// that holds every field of [AdoptionFields]: a draft's head can be read,
// outlined and searched, and can never be drawn as the company's brand by
// mistake, because nothing compiles that would do it.

// AdoptionField is one field an adopted brand holds, and what it is, as a
// refusal names it.
type AdoptionField struct {
	Name string
	What string
}

// AdoptionFields are the fields a brand must hold to be adopted, in the order
// a refusal names them: a logo to draw it by and a colour to paint it with.
// Everything else an adoption serves falls back to the artifact kit's own.
var AdoptionFields = []AdoptionField{
	{Name: "logos.primary", What: "the logo on a light ground, which every branded page draws"},
	{Name: "colors.primary", What: "the brand's accent, #rrggbb"},
}

// MissingToAdopt names every field of [AdoptionFields] b leaves out, in that
// order; none for a brand that may be adopted.
func MissingToAdopt(b Brand) []string {
	var out []string
	for _, f := range AdoptionFields {
		if adoptionValue(b, f.Name) == "" {
			out = append(out, f.Name)
		}
	}
	return out
}

// adoptionValue is the value b holds for one field of [AdoptionFields].
func adoptionValue(b Brand, name string) string {
	switch name {
	case "logos.primary":
		return b.Logos.Primary
	case "colors.primary":
		return b.Colors.Primary
	}
	return ""
}

// AdoptionFindings are the findings an adoption refuses a brand for, one per
// missing field (from [MissingToAdopt]), each naming the field and what it
// is, on [EntryPath]: the same shape as a publish's findings, so a surface
// shows them as it shows those.
func AdoptionFindings(missing []string) []Finding {
	out := make([]Finding, 0, len(missing))
	for _, name := range missing {
		what := ""
		for _, f := range AdoptionFields {
			if f.Name == name {
				what = f.What
			}
		}
		out = append(out, Finding{Path: EntryPath, Rule: RuleField,
			Message: fmt.Sprintf("%s: is required to adopt the brand: %s", name, what)})
	}
	return out
}

// Adoptable is a brand that holds every field of [AdoptionFields]: the one a
// stylesheet and a slide theme are made from. Its zero value holds nothing and
// is never handed out.
type Adoptable struct {
	brand Brand
}

// Brand is the adoptable brand's index.
func (a Adoptable) Brand() Brand { return a.brand }

// AsAdoptable is b as an [Adoptable], and false where b leaves out a field of
// [AdoptionFields].
func AsAdoptable(b Brand) (Adoptable, bool) {
	if len(MissingToAdopt(b)) > 0 {
		return Adoptable{}, false
	}
	return Adoptable{brand: b}, true
}

// DecodeAdopted reads an adopted brand's canonical index, as [Decode] does,
// and holds it to what an adopted brand holds: an adoption is refused for
// leaving a field out, so a snapshot that does is a row no adoption wrote,
// refused naming the field rather than drawn incomplete.
func DecodeAdopted(canonical []byte) (Adoptable, error) {
	b, err := Decode(canonical)
	if err != nil {
		return Adoptable{}, err
	}
	a, ok := AsAdoptable(b)
	if !ok {
		return Adoptable{}, &Refused{Findings: AdoptionFindings(MissingToAdopt(b))}
	}
	return a, nil
}
