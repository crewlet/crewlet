package artifactupload_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/png"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/brandformat"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactupload"
	"github.com/crewlet/crewlet/internal/artifactupload/lint"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// kitJSON is a kit's brand.json: a wordmark for light grounds, a raster for
// dark ones, its text in textColour.
func kitJSON(textColour string) string {
	return `{"v": 1, "name": "Acme", "tagline": "Tools that last",
  "logos": {"primary": "logos/primary.svg", "on_dark": "logos/on-dark.png"},
  "colors": {"primary": "#2450e6", "text": "` + textColour + `", "light_ground": "#ffffff"},
  "type": {"heading": {"kit": "geist"}, "body": {"stack": "system-sans"}},
  "voice": {"summary": "Plain and warm."}}`
}

// authoredLogo is a wordmark as an editor exports it: a comment, an XML
// declaration and namespace declarations, none of which the stored form keeps.
const authoredLogo = `<?xml version="1.0" encoding="UTF-8"?>
<!-- exported by a drawing tool -->
<svg xmlns="http://www.w3.org/2000/svg" xmlns:xlink="http://www.w3.org/1999/xlink" version="1.1" viewBox="0 0 200 50">
  <rect width="200" height="50" fill="#2450e6"/>
</svg>
`

// pngWithText is a 300 by 100 PNG carrying a tEXt chunk, metadata the stored
// form drops.
func pngWithText(t *testing.T) string {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 300, 100))
	img.Set(0, 0, color.NRGBA{R: 0xff, A: 0xff})
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	raw := b.Bytes()
	data := []byte("Author\x00someone@example.com")
	var chunk bytes.Buffer
	_ = binary.Write(&chunk, binary.BigEndian, uint32(len(data)))
	chunk.WriteString("tEXt")
	chunk.Write(data)
	_ = binary.Write(&chunk, binary.BigEndian, crc32.ChecksumIEEE(append([]byte("tEXt"), data...)))
	at := len(raw) - 12 // before IEND
	return string(raw[:at]) + chunk.String() + string(raw[at:])
}

// kitFiles is a kit's version as its editor uploads it.
func kitFiles(t *testing.T, textColour string) map[string]string {
	return map[string]string{
		brandformat.EntryPath: kitJSON(textColour),
		"logos/primary.svg":   authoredLogo,
		"logos/on-dark.png":   pngWithText(t),
	}
}

// createKit makes a brand kit through an upload, answering the outcome.
func createKit(h *harness, files map[string]string) (artifactsdomain.Published, error) {
	h.t.Helper()
	c := h.person("ada", h.opKey("create"))
	up := h.upload(c, artifacts.ID{}, files)
	got, _, err := h.svc.Create(h.t.Context(), c, artifactupload.CreateRequest{
		Kind: artifacts.KindBrand, Title: "Acme brand", Owner: c.Authority.Actor.Name,
		OwnerKind: c.Authority.Actor.Kind, Upload: up, Files: specs(files), Entry: brandformat.EntryPath,
	})
	return got, err
}

// A KIT'S LOGOS ARE STORED IN THE ENGINE'S FORM: the drawing re-serialised
// with its comment and declarations gone, the raster without its metadata,
// and the stored version reads under the brand format.
//
// Mutation: skip sanitizeBrandAssets and the drawing is stored as uploaded.
func TestAKitIsStoredWithItsLogosInTheEnginesForm(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	got, err := createKit(h, kitFiles(t, "#111111"))
	if err != nil {
		t.Fatalf("the kit was refused: %v", err)
	}
	v := h.version(got.Artifact, 1)
	svg := h.file(v, "logos/primary.svg")
	want, err := brandformat.Sanitize("logos/primary.svg", "image/svg+xml", []byte(authoredLogo))
	if err != nil {
		t.Fatal(err)
	}
	if svg != string(want) || strings.Contains(svg, "exported by") {
		t.Errorf("the drawing is stored as %q, want its canonical form %q", svg, want)
	}
	if raster := h.file(v, "logos/on-dark.png"); strings.Contains(raster, "someone@example.com") {
		t.Error("the raster logo is stored with its metadata")
	}
	if h.unnamedBytes() != 0 {
		t.Errorf("%d stored bytes are named by no row: the sanitised logos' upload was kept", h.unnamedBytes())
	}
}

// A KIT OUTSIDE THE FORMAT IS REFUSED, EVERY FINDING NAMING ITS FILE: a
// drawing that runs a script, a file the index does not name and a key the
// index does not carry are each a brand_format finding of the ONE refusal,
// though the drawing is refused before the format check, and nothing is
// stored.
//
// Mutation: answer the drawing's refusal without CheckIndex and the index's
// faults wait for the next attempt.
func TestAKitOutsideTheFormatIsRefusedFileByFile(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	files := kitFiles(t, "#111111")
	files["logos/primary.svg"] = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 200 50"><script>alert(1)</script></svg>`
	files["notes.txt"] = "draft"
	files[brandformat.EntryPath] = strings.Replace(files[brandformat.EntryPath], `"v": 1,`, `"v": 1, "fonts": {},`, 1)
	_, err := createKit(h, files)
	var refusal *lint.Refusal
	if !errors.As(err, &refusal) {
		t.Fatalf("the kit was answered %v, want a policy refusal", err)
	}
	paths := map[string]bool{}
	for _, f := range refusal.Findings {
		if f.Code != lint.CodeBrandFormat {
			t.Errorf("a finding is %s, want %s", f.Code, lint.CodeBrandFormat)
		}
		paths[f.Path] = true
	}
	for _, p := range []string{"logos/primary.svg", "notes.txt", brandformat.EntryPath} {
		if !paths[p] {
			t.Errorf("the refusal names nothing at %s: %v", p, refusal.Findings)
		}
	}
}

// AN ADOPTION COPIES EVERY FILE IT SERVES AND GENERATES ITS STYLESHEET: the
// adopted logos are fresh objects holding the kit's stored bytes (so their
// hashes equal the kit's entries'), the stylesheet is the brand format's for
// those files, and every byte the adoption stored is named.
//
// Mutation: name the kit's objects instead of copying them, and the adopted
// keys are the kit's.
func TestAnAdoptionCopiesEveryFileAndGeneratesTheStylesheet(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	got, err := createKit(h, kitFiles(t, "#111111"))
	if err != nil {
		t.Fatalf("the kit was refused: %v", err)
	}
	if _, err := h.svc.AdoptBrand(t.Context(), h.person("maya", h.opKey("adopt")), got.Artifact, 1); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	a, adopted, err := h.domain.reader.Brand(t.Context(), statelog.Freshness{Level: statelog.ReadSession})
	if err != nil || !adopted {
		t.Fatalf("the brand reads adopted=%v (%v)", adopted, err)
	}
	v := h.version(got.Artifact, 1)
	kitObjects := map[objstore.Key]bool{}
	byPath := map[string]objstore.Object{}
	for _, e := range v.Manifest.Files {
		kitObjects[e.Object.Key] = true
		byPath[e.Path] = e.Object
	}
	if len(a.Files) != 3 || a.Files[0].Slot != string(brandformat.SlotCSS) {
		t.Fatalf("the adoption serves %+v, want the stylesheet and two logos", a.Files)
	}
	for _, f := range a.Files {
		if kitObjects[f.Object.Key] {
			t.Errorf("the adoption names the kit's own object for %s", f.Slot)
		}
	}
	if primary, _ := a.File(brandformat.SlotLogoPrimary); primary.Object.Hash != byPath["logos/primary.svg"].Hash {
		t.Errorf("the adopted primary logo hashes %s, the kit's %s", primary.Object.Hash,
			byPath["logos/primary.svg"].Hash)
	}
	css := h.read(a.Files[0].Object)
	if !strings.Contains(css, "--brand-primary: #2450e6;") || !strings.Contains(css, "--brand-logo-primary: url(/_crewlet/brand/") ||
		!strings.Contains(css, "--brand-logo-primary-ratio: 4;") {
		t.Errorf("the stylesheet is %q", css)
	}
	if h.unnamedBytes() != 0 {
		t.Errorf("%d stored bytes are named by no row after the adoption", h.unnamedBytes())
	}
}

// AN UNREADABLE KIT IS REFUSED BEFORE A BYTE IS STORED: the contrast is the
// pipeline's first question after the format, answered with the pair.
func TestAnUnreadableKitIsRefusedBeforeAByteIsStored(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	got, err := createKit(h, kitFiles(t, "#dddddd"))
	if err != nil {
		t.Fatalf("a draft with a pale text was refused at publish: %v", err)
	}
	before := len(h.stored())
	_, err = h.svc.AdoptBrand(t.Context(), h.person("maya", h.opKey("adopt")), got.Artifact, 1)
	var contrast *artifactsdomain.BrandContrast
	if !errors.As(err, &contrast) {
		t.Fatalf("the pale kit was adopted or refused otherwise: %v", err)
	}
	if after := len(h.stored()); after != before {
		t.Errorf("the refused adoption stored %d objects", after-before)
	}
}

// A PAGE THAT OPTS OUT PINS NO BRAND, and every other page pins the one
// adopted when it is published.
func TestAPageThatOptsOutOfTheBrandPinsNone(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	kit, err := createKit(h, kitFiles(t, "#111111"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.svc.AdoptBrand(t.Context(), h.person("maya", h.opKey("adopt")), kit.Artifact, 1); err != nil {
		t.Fatalf("adopt: %v", err)
	}
	branded := h.create(h.person("ada", h.opKey("create")), map[string]string{
		"index.html": `<!doctype html><html><body><p>ours</p></body></html>`})
	own := h.create(h.person("ada", h.opKey("create")), map[string]string{
		"index.html": `<!doctype html><html data-crewlet-brand="off"><body><p>own</p></body></html>`})
	if v := h.version(branded.Artifact, 1); v.Brand == "" {
		t.Error("a page published after an adoption pins no brand")
	}
	if v := h.version(own.Artifact, 1); v.Brand != "" {
		t.Errorf("a page that opts out pins %q", v.Brand)
	}
}

// read is one stored object's bytes, verified.
func (h *harness) read(o objstore.Object) string {
	h.t.Helper()
	rc, err := h.objects.Open(h.t.Context(), o)
	if err != nil {
		h.t.Fatalf("open %s: %v", o.Key, err)
	}
	defer func() { _ = rc.Close() }()
	var b bytes.Buffer
	if _, err := b.ReadFrom(rc); err != nil {
		h.t.Fatalf("read %s: %v", o.Key, err)
	}
	return b.String()
}

// A DRAFT PUBLISHES STEP BY STEP AND IS ADOPTED ONLY WHEN COMPLETE: a kit
// holding its name alone, then its name and its logo, each publishes as a
// version of the kit, and adopting either is refused naming what it leaves
// out before a byte is stored.
//
// Mutation: require logos.primary at publish again and the name-only draft is
// refused at its first save.
func TestADraftPublishesAndIsAdoptedOnlyWhenComplete(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		files   map[string]string
		missing []string
	}{
		"the name alone": {map[string]string{brandformat.EntryPath: `{"v": 1, "name": "Acme"}`},
			[]string{"logos.primary", "colors.primary"}},
		"a name and a logo": {map[string]string{
			brandformat.EntryPath: `{"v": 1, "name": "Acme", "logos": {"primary": "logos/primary.svg"}}`,
			"logos/primary.svg":   authoredLogo}, []string{"colors.primary"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, options{})
			got, err := createKit(h, c.files)
			if err != nil {
				t.Fatalf("the draft was refused at publish: %v", err)
			}
			before := len(h.stored())
			_, err = h.svc.AdoptBrand(t.Context(), h.person("maya", h.opKey("adopt")), got.Artifact, 1)
			var incomplete *artifactsdomain.BrandIncomplete
			if !errors.As(err, &incomplete) || !slices.Equal(incomplete.Missing, c.missing) {
				t.Fatalf("adopting the draft answered %v, want it refused naming %v", err, c.missing)
			}
			if after := len(h.stored()); after != before {
				t.Errorf("the refused adoption stored %d objects", after-before)
			}
		})
	}
}
