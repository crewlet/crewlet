package brandformat_test

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts/brandformat"
)

// draft is a kit's version holding brand.json as raw and the files it names
// from the honest kit.
func draft(t *testing.T, raw string, files ...string) version {
	t.Helper()
	honest := kit(t)
	v := version{brandformat.EntryPath: []byte(raw)}
	for _, p := range files {
		v[p] = honest[p]
	}
	return v
}

// A DRAFT NEEDS ONLY ITS NAME: a kit saved after each step of its setup (the
// name alone, then a logo, then a colour, in any order) publishes, its faces
// unset and every absent role the kit's own, and is a brand no adoption would
// take until it holds a logo and a colour.
//
// Mutation: require logos.primary at publish again and the name-only draft is
// refused.
func TestADraftNeedsOnlyItsName(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		raw     string
		files   []string
		missing []string
	}{
		"the name alone": {`{"v": 1, "name": "Acme"}`, nil, []string{"logos.primary", "colors.primary"}},
		"a name and a logo": {`{"v": 1, "name": "Acme", "logos": {"primary": "logos/primary.svg"}}`,
			[]string{"logos/primary.svg"}, []string{"colors.primary"}},
		"a name and a colour": {`{"v": 1, "name": "Acme", "colors": {"primary": "#2450e6"}}`, nil,
			[]string{"logos.primary"}},
		"a logo and a colour, faces unset": {`{"v": 1, "name": "Acme", "logos": {"primary": "logos/primary.svg"},
			"colors": {"primary": "#2450e6"}, "type": {"heading": {}, "body": {}}, "voice": {}}`,
			[]string{"logos/primary.svg"}, nil},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b, err := brandformat.Read(draft(t, c.raw, c.files...))
			if err != nil {
				t.Fatalf("the draft was refused at publish: %v", err)
			}
			if b.Text() != "Acme" {
				t.Errorf("the draft's search text is %q", b.Text())
			}
			if got := brandformat.MissingToAdopt(b); !slices.Equal(got, c.missing) {
				t.Errorf("the draft leaves out %v for an adoption, want %v", got, c.missing)
			}
			if _, ok := brandformat.AsAdoptable(b); ok != (len(c.missing) == 0) {
				t.Errorf("the draft is adoptable = %v with %v missing", ok, c.missing)
			}
			canonical, err := brandformat.Canonical(b)
			if err != nil {
				t.Fatal(err)
			}
			again, err := brandformat.Decode(canonical)
			if err != nil {
				t.Fatalf("the draft's canonical index does not read back: %v\n%s", err, canonical)
			}
			if string(mustCanonical(t, again)) != string(canonical) {
				t.Errorf("the draft's canonical index is not a fixed point: %s", canonical)
			}
			if strings.Contains(string(canonical), `"primary":""`) {
				t.Errorf("the draft's canonical index writes an absent field as empty: %s", canonical)
			}
		})
	}
}

// A DRAFT'S PRESENT FIELDS KEEP EVERY RULE: a partial kit is held to the
// closed keys, the colour grammar, the asset rules and the face rules exactly
// as a whole one is, so leaving a field out is the only thing a draft is
// allowed.
//
// Mutation: skip the colour check when the logo is absent and the upper-case
// colour is read.
func TestADraftsPresentFieldsKeepEveryRule(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		v    version
		rule brandformat.Rule
		want string
	}{
		"an unknown key": {draft(t, `{"v": 1, "name": "Acme", "slogan": "x"}`), brandformat.RuleIndex,
			"not a key"},
		"a colour outside its grammar": {draft(t, `{"v": 1, "name": "Acme", "colors": {"primary": "#2450E6"}}`),
			brandformat.RuleField, "six lowercase hex"},
		"a logo the version does not hold": {draft(t,
			`{"v": 1, "name": "Acme", "logos": {"primary": "logos/primary.svg"}}`),
			brandformat.RuleReference, "holds no file"},
		"a logo that runs a script": {version{
			brandformat.EntryPath: []byte(`{"v": 1, "name": "Acme", "logos": {"primary": "logo.svg"}}`),
			"logo.svg": []byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 10 10">` +
				`<script>alert(1)</script></svg>`)}, brandformat.RuleAsset, "runs no code"},
		"a licence with no family": {draft(t,
			`{"v": 1, "name": "Acme", "type": {"heading": {"licensed": true}}}`), brandformat.RuleField,
			"names no face"},
		"a link in the tagline": {draft(t, `{"v": 1, "name": "Acme", "tagline": "see www.evil.test"}`),
			brandformat.RuleField, "link"},
		"no name": {draft(t, `{"v": 1, "logos": {"primary": "logos/primary.svg"}}`, "logos/primary.svg"),
			brandformat.RuleField, "name: is required"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if !names(readRefused(t, c.v), c.rule, c.want) {
				t.Errorf("the partial kit's findings do not say %q", c.want)
			}
		})
	}
}

// AN ADOPTED SNAPSHOT IS A COMPLETE BRAND, AND ONLY A COMPLETE BRAND IS DRAWN:
// DecodeAdopted refuses an index that leaves out a field an adoption requires,
// naming each in the shape a publish's findings take, and reads a complete
// one as the adoptable brand a stylesheet and a slide theme are made from.
//
// Mutation: let DecodeAdopted answer a brand with no colour and the stylesheet
// is generated for it.
func TestOnlyACompleteBrandIsReadAsAdopted(t *testing.T) {
	t.Parallel()
	_, err := brandformat.DecodeAdopted([]byte(`{"v":1,"name":"Acme","logos":{},"colors":{},` +
		`"type":{"heading":{},"body":{}},"voice":{}}`))
	var refused *brandformat.Refused
	if !errors.As(err, &refused) || len(refused.Findings) != 2 {
		t.Fatalf("an incomplete snapshot was read as adopted: %v", err)
	}
	for i, field := range []string{"logos.primary", "colors.primary"} {
		f := refused.Findings[i]
		if f.Path != brandformat.EntryPath || f.Rule != brandformat.RuleField ||
			!strings.HasPrefix(f.Message, field+": is required to adopt the brand") {
			t.Errorf("finding %d is %+v, want one naming %s", i, f, field)
		}
	}
	b, err := brandformat.Read(kit(t))
	if err != nil {
		t.Fatal(err)
	}
	a, err := brandformat.DecodeAdopted(mustCanonical(t, b))
	if err != nil || a.Brand().Name != "Acme" || a.Brand().Logos.Primary == "" {
		t.Errorf("a complete brand did not read as adopted: %+v (%v)", a.Brand(), err)
	}
}

// mustCanonical is b's canonical index.
func mustCanonical(t *testing.T, b brandformat.Brand) []byte {
	t.Helper()
	out, err := brandformat.Canonical(b)
	if err != nil {
		t.Fatal(err)
	}
	return out
}
