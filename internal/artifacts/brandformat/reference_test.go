package brandformat_test

import (
	"encoding/json"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts/brandformat"
)

// THE REFERENCE COVERS THE WHOLE INDEX, and nothing else: every key the
// strict decode admits has its rule, and no rule names a key it refuses. An
// agent that writes only what the reference lists writes a kit the reader
// takes.
//
// Mutation: add a key to Brand without its rule, or drop a rule, and this
// fails naming it.
func TestTheReferenceCoversEveryKeyOfTheIndex(t *testing.T) {
	t.Parallel()
	missing, unknown := brandformat.IndexForm.Unruled(brandformat.FieldRules())
	if len(missing) > 0 || len(unknown) > 0 {
		t.Fatalf("the reference has no rule for %v, and a rule for keys the index lacks %v",
			missing, unknown)
	}
	ref := brandformat.Reference()
	for _, p := range brandformat.IndexForm.Paths() {
		if !strings.Contains(ref, "`"+p.Path+"`") {
			t.Errorf("the reference does not list %s", p.Path)
		}
	}
	for _, want := range []string{"EXACTLY ONE of kit, stack and family", "geist", "system-serif",
		"No `<style>` element", "GoPhrase.me", "logos.primary", "160 bytes"} {
		if !strings.Contains(ref, want) {
			t.Errorf("the reference does not say %q", want)
		}
	}
}

var fencedJSON = regexp.MustCompile("(?s)```json\n(.*?)\n```")

// A KIT WRITTEN FROM THE REFERENCE IS READ FIRST TIME: its example, and the
// uploaded family it shows, are read clean by the same reader a publish uses,
// so following the reference never earns a refusal.
//
// Mutation: tighten a rule the example breaks (a shorter summary bound) and
// this fails before an agent ever meets it.
func TestAKitWrittenFromTheReferenceIsReadFirstTime(t *testing.T) {
	t.Parallel()
	m := fencedJSON.FindStringSubmatch(brandformat.Reference())
	if m == nil {
		t.Fatal("the reference carries no JSON example")
	}
	b, err := brandformat.Decode([]byte(m[1]))
	if err != nil {
		t.Fatalf("the reference's example is refused: %v", err)
	}
	v := version{brandformat.EntryPath: []byte(m[1]),
		b.Logos.Primary: []byte(svgLogo), b.Logos.Mark: []byte(svgMark)}
	if _, err := brandformat.ReadKit(v); err != nil {
		t.Fatalf("a kit of the reference's example and its logos is refused: %v", err)
	}
	family := regexp.MustCompile("`(\\{\"family\".*?\\})`").FindStringSubmatch(brandformat.Reference())
	if family == nil {
		t.Fatal("the reference shows no uploaded family")
	}
	var face brandformat.Face
	if err := json.Unmarshal([]byte(family[1]), &face); err != nil {
		t.Fatalf("the reference's family is not a face: %v", err)
	}
	b.Type.Body = face
	raw, err := json.Marshal(b)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := brandformat.Decode(raw); err != nil {
		t.Fatalf("a kit with the reference's uploaded family is refused: %v", err)
	}
}

// svgMark is a square mark.
const svgMark = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 50 50"><circle cx="25" cy="25" r="20" fill="#2450e6"/></svg>`

// EVERY FAULT IN ONE REFUSAL: the GoPhrase turn (GP-38) spent fourteen rounds
// learning one structural fault per publish. A kit with an unknown key, an
// object where a colour goes, a string where a face goes, a face naming two
// sources, a voice line past its bound and a file it never names is answered
// with all six at once, each naming the shape the format reads, while the
// brand's own domain in its voice is not one of them.
//
// Mutation: return from the decode at its first fault, or skip the field
// rules when the decode found one, and fewer than six come back.
func TestSixFaultsComeBackInOneRefusal(t *testing.T) {
	t.Parallel()
	v := kit(t)
	v[brandformat.EntryPath] = []byte(`{
  "v": 1,
  "name": "GoPhrase",
  "fonts": {"body": "Inter"},
  "logos": {"primary": "logos/primary.svg", "mark": "logos/mark.webp", "on_dark": "logos/on-dark.png"},
  "colors": {"primary": {"hex": "#2450e6"}, "secondary": "#9584ff"},
  "type": {"heading": {"kit": "geist", "stack": "system-sans"}, "body": "Inter"},
  "voice": {
    "summary": "Friendly and exact, as GoPhrase.me speaks to learners.",
    "write_like": ["Find the phrase.", "` + strings.Repeat("a", brandformat.MaxVoiceExampleBytes+1) + `"]
  }
}`)
	v["NOTES.md"] = []byte("draft notes")
	findings := readRefused(t, v)
	want := []struct {
		rule brandformat.Rule
		says string
	}{
		{brandformat.RuleIndex, "fonts is not a key the brand format carries: the top level of brand.json takes v, name, tagline, logos, colors, type, voice"},
		{brandformat.RuleIndex, "colors.primary is an object, and the format reads a string there"},
		{brandformat.RuleIndex, "type.body is a string, and the format reads an object {kit, stack, family, files, licensed} there"},
		{brandformat.RuleField, "type.heading: names kit and stack"},
		{brandformat.RuleBound, "voice.write_like[1]: is 161 bytes and may be at most 160"},
		{brandformat.RuleReference, "NOTES.md: reference: brand.json names no logo or face at this path"},
	}
	for _, w := range want {
		if !names(findings, w.rule, w.says) {
			t.Errorf("no %s finding says %q", w.rule, w.says)
		}
	}
	if len(findings) != len(want) {
		t.Errorf("%d findings, want exactly %d:\n%v", len(findings), len(want), findings)
	}
}

// THE BRAND'S OWN NAME AS A DOMAIN IS NOT AN ADDRESS: "GoPhrase.me" in the
// voice of the brand GoPhrase was refused as a web address, while any other
// host, and a subdomain of its own, still is.
//
// Mutation: drop ownHost and the first case is refused.
func TestTheBrandsOwnDomainIsItsName(t *testing.T) {
	t.Parallel()
	for text, ok := range map[string]bool{
		"Plain words, as GoPhrase.me says them.":   true,
		"Plain words, as gophrase.app says them.":  true,
		"Plain words, as Go-Phrase.io says them.":  true,
		"Plain words, as promo.gophrase.me says.":  false,
		"Plain words, as phrasebook.me says them.": false,
		"Plain words, see gophrase.me/promo.":      false,
	} {
		v := with(t, `"name": "Acme"`, `"name": "GoPhrase"`)
		v[brandformat.EntryPath] = []byte(strings.Replace(string(v[brandformat.EntryPath]),
			`"Plain, warm and exact.\nNever breathless."`, `"`+text+`"`, 1))
		_, err := brandformat.ReadKit(v)
		if (err == nil) != ok {
			t.Errorf("%q: read error %v, want admitted %v", text, err, ok)
		}
	}
}

// THE BRAND INDEX ADMITS EXACTLY THE KEYS IT ADMITTED BEFORE ADR-0059, so no
// kit, and no adopted snapshot, that read before reads differently now. Before
// it, brand.json was walked against a hand-written key tree (the paths below)
// that already matched keys exactly and refused a key written twice, then
// decoded with encoding/json; the derived form must be that tree, key for key.
// Its rules only loosened (the brand's own domain), so every index accepted
// then is accepted now. An adoption decodes its snapshot when it is decided
// (artifactsdomain.checkAdoption), never when the log is applied, and the
// snapshot is the canonical bytes, exact keys by construction.
//
// Mutation: add a key to Brand, or rename one, and the tree differs.
func TestTheBrandIndexAdmitsTheKeysItAlwaysDid(t *testing.T) {
	t.Parallel()
	before := []string{"v", "name", "tagline",
		"logos", "logos.primary", "logos.on_dark", "logos.mark",
		"colors", "colors.primary", "colors.secondary", "colors.text", "colors.light_ground",
		"colors.dark_ground", "colors.primary_on_dark",
		"type", "voice", "voice.summary", "voice.write_like", "voice.avoid"}
	for _, face := range []string{"type.heading", "type.body"} {
		before = append(before, face, face+".kit", face+".stack", face+".family", face+".licensed",
			face+".files", face+".files[].path", face+".files[].weight", face+".files[].style")
	}
	var now []string
	for _, p := range brandformat.IndexForm.Paths() {
		now = append(now, p.Path)
	}
	slices.Sort(before)
	slices.Sort(now)
	if !slices.Equal(before, now) {
		t.Errorf("the brand index admits\n%v\nand admitted\n%v", now, before)
	}
}
