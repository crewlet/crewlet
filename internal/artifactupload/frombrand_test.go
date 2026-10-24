package artifactupload_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/brandformat"
	"github.com/crewlet/crewlet/internal/artifacts/manifest"
	"github.com/crewlet/crewlet/internal/artifacts/slideformat"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactupload"
	"github.com/crewlet/crewlet/internal/objstore"
)

// The adopted brand's files as its snapshot keeps them.
const (
	brandLogo     = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 120 40"><rect width="120" height="40"/></svg>`
	brandDarkLogo = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 120 40"><rect width="120" height="40" fill="#fff"/></svg>`
)

// brandFont is a WOFF2 file's bytes as far as the pipeline's content check
// reads them: its signature, then a body that says which file it is.
func brandFont(which string) []byte { return append([]byte("wOF2"), which...) }

// adopt stores the brand's files in the harness's object store and answers
// them as the adopted brand: Acme, a logo for each ground, and an uploaded
// heading family of a bold and a regular file.
func (h *harness) adopt(t *testing.T) brandformat.Brand {
	t.Helper()
	b := brandformat.Brand{V: brandformat.Version, Name: "Acme",
		Logos:  brandformat.Logos{Primary: "logo.svg", OnDark: "logo-dark.svg"},
		Colors: brandformat.Colors{Primary: "#0f7b5f"},
		Type: brandformat.Type{
			Heading: brandformat.Face{Family: "Acme Sans", Licensed: true, Files: []brandformat.FontFile{
				{Path: "fonts/bold.woff2", Weight: 700, Style: "normal"},
				{Path: "fonts/regular.woff2", Weight: 400, Style: "normal"}}},
			Body: brandformat.Face{Kit: "geist"}},
	}
	canonical, err := brandformat.Canonical(b)
	if err != nil {
		t.Fatal(err)
	}
	put := func(slot brandformat.Slot, ct string, data []byte) artifactsdomain.BrandFile {
		obj, err := h.objects.Put(t.Context(), bytes.NewReader(data), int64(len(data)),
			objstore.PutMeta{ContentType: ct})
		if err != nil {
			t.Fatal(err)
		}
		return artifactsdomain.BrandFile{Slot: string(slot), ContentType: ct, Object: obj}
	}
	a := artifactsdomain.Adoption{Kit: artifacts.IDFor("kit"), KitVersion: 3, BrandJSON: canonical,
		Files: []artifactsdomain.BrandFile{
			put(brandformat.SlotCSS, "text/css", []byte(":root{--brand-primary:#0f7b5f}")),
			put(brandformat.SlotLogoPrimary, "image/svg+xml", []byte(brandLogo)),
			put(brandformat.SlotLogoOnDark, "image/svg+xml", []byte(brandDarkLogo)),
			put(brandformat.FontFileSlot(brandformat.SlotFontHeading, 0), "font/woff2", brandFont("bold")),
			put(brandformat.FontFileSlot(brandformat.SlotFontHeading, 1), "font/woff2", brandFont("regular")),
		}}
	h.mu.Lock()
	h.adopted = &a
	h.mu.Unlock()
	return b
}

// adoptedObject is the snapshot's object in slot.
func (h *harness) adoptedObject(slot brandformat.Slot) objstore.Object {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, f := range h.adopted.Files {
		if f.Slot == string(slot) {
			return f.Object
		}
	}
	return objstore.Object{}
}

// createWithBrand creates a page carrying the brand copies named.
func (h *harness) createWithBrand(copies map[string]brandformat.Slot) (artifactsdomain.Published, error) {
	h.t.Helper()
	files := []artifactupload.FileSpec{{Path: "index.html", Inline: []byte(page)}}
	for p, slot := range copies {
		files = append(files, artifactupload.FileSpec{Path: p, Brand: slot})
	}
	c := h.agent("writer", "turn-1", h.opKey("create"))
	got, _, err := h.svc.Create(h.t.Context(), c, artifactupload.CreateRequest{
		Kind: artifacts.KindHTML, Title: "Launch", Owner: "writer",
		OwnerKind: c.Authority.Actor.Kind, Files: files, Entry: "index.html"})
	return got, err
}

// A BRAND FILE IS COPIED, NEVER SHARED: the version holds the logo's and the
// face's exact bytes as objects of its own, never the snapshot's object, so a
// later adoption or the kit's purge changes nothing in it; a face named as a
// whole copies the file the brand's slide theme sets it in.
func TestABrandFileIsCopiedIntoTheVersionAsItsOwnObject(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	b := h.adopt(t)
	got, err := h.createWithBrand(map[string]brandformat.Slot{
		"brand/logo.svg":      brandformat.SlotLogoPrimary,
		"brand/heading.woff2": brandformat.SlotFontHeading,
	})
	if err != nil {
		t.Fatal(err)
	}
	v := h.version(got.Artifact, 1)
	logo, ok := manifest.EntryAt(v.Manifest, "brand/logo.svg")
	if !ok || h.file(v, "brand/logo.svg") != brandLogo {
		t.Fatalf("the logo was stored as %q", h.file(v, "brand/logo.svg"))
	}
	if snap := h.adoptedObject(brandformat.SlotLogoPrimary); logo.Object.Key == snap.Key || logo.Object.Hash != snap.Hash {
		t.Errorf("the version's logo is %+v and the snapshot's %+v: want the same bytes in "+
			"an object of its own", logo.Object, snap)
	}
	want, _ := artifactupload.FaceFile(b, brandformat.SlotFontHeading)
	face, _ := manifest.EntryAt(v.Manifest, "brand/heading.woff2")
	if face.Object.Hash != h.adoptedObject(want).Hash {
		t.Errorf("the heading face copied %s's bytes? got %s", want, face.Object.Hash)
	}
}

// A COPY THE BRAND CANNOT ANSWER IS REFUSED BY NAME, and nothing is published:
// a path whose type is not the file's, the stylesheet (linked, never carried),
// a slot the brand holds no file in, a face every page already has, and a
// company with no brand adopted.
func TestABrandCopyTheBrandCannotAnswerIsRefusedByName(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	if _, err := h.createWithBrand(map[string]brandformat.Slot{"brand/logo.svg": brandformat.SlotLogoPrimary}); !errors.Is(err, artifactupload.ErrInvalid) ||
		!strings.Contains(err.Error(), "adopted no brand") {
		t.Fatalf("a copy with no brand adopted answered %v", err)
	}
	h.adopt(t)
	for name, c := range map[string]struct {
		path string
		slot brandformat.Slot
		says string
	}{
		"the wrong extension": {"brand/logo.png", brandformat.SlotLogoPrimary, ".svg"},
		"the stylesheet":      {"brand/brand.css", brandformat.SlotCSS, "stylesheet"},
		"a slot it lacks":     {"brand/mark.svg", brandformat.SlotLogoMark, "logo.primary"},
		"a kit face":          {"brand/body.woff2", brandformat.SlotFontBody, "every page already has"},
	} {
		_, err := h.createWithBrand(map[string]brandformat.Slot{c.path: c.slot})
		if !errors.Is(err, artifactupload.ErrInvalid) || !strings.Contains(err.Error(), c.says) {
			t.Errorf("%s answered %v, want a refusal saying %q", name, err, c.says)
		}
	}
}

// A DECK BEGUN IN THE BRAND IS THE BRAND'S, ONCE: drawn in the brand's slide
// theme, its cover showing the logo meant for its dark ground, its heading
// typeface copied in and declared, every file held to the slide format, and
// the copies made from the adoption it was composed from. With no brand
// adopted it is refused, naming the engine's themes as the way on.
func TestADeckBegunInTheBrandCopiesItsLogoAndFaces(t *testing.T) {
	t.Parallel()
	h := newHarness(t, options{})
	c := h.agent("writer", "turn-1", h.opKey("create"))
	req := artifactupload.CreateRequest{Kind: artifacts.KindSlides, Title: "Board review",
		Owner: "writer", OwnerKind: c.Authority.Actor.Kind, Starter: true,
		Deck: &slideformat.Plan{Theme: slideformat.BrandTheme}}
	if _, _, err := h.svc.Create(t.Context(), c, req); !errors.Is(err, artifactupload.ErrInvalid) ||
		!strings.Contains(err.Error(), "engine's themes") {
		t.Fatalf("a brand deck with no brand answered %v", err)
	}
	b := h.adopt(t)
	got, _, err := h.svc.Create(t.Context(), h.agent("writer", "turn-1", h.opKey("create")), req)
	if err != nil {
		t.Fatal(err)
	}
	v := h.version(got.Artifact, 1)
	idx := h.file(v, slideformat.IndexPath)
	if !strings.Contains(idx, `"theme": "brand"`) || !strings.Contains(idx, `"family": "Acme Sans"`) {
		t.Errorf("the deck's index:\n%s", idx)
	}
	// THE BRAND THEME'S COVER IS ITS DARK GROUND, so the logo for dark
	// grounds is the one on it.
	if h.file(v, "brand/logo-on-dark.svg") != brandDarkLogo {
		t.Errorf("the cover's logo was copied as %q", h.file(v, "brand/logo-on-dark.svg"))
	}
	if !strings.Contains(h.file(v, slideformat.SlidePath("cover")), `src="brand/logo-on-dark.svg"`) {
		t.Errorf("the cover does not show the copied logo:\n%s", h.file(v, slideformat.SlidePath("cover")))
	}
	file, _ := artifactupload.FaceFile(b, brandformat.SlotFontHeading)
	p, err := artifactupload.BrandPath(file, "font/woff2")
	if err != nil {
		t.Fatal(err)
	}
	if e, ok := manifest.EntryAt(v.Manifest, p); !ok || e.Object.Hash != h.adoptedObject(file).Hash ||
		!strings.Contains(idx, `"src": "`+p+`"`) {
		t.Errorf("the heading face is %+v (held %v) and the index declares:\n%s", e, ok, idx)
	}
	if theme := brandformat.SlideTheme(b); !strings.Contains(h.file(v, slideformat.SlidePath("cover")), theme.Feature) {
		t.Errorf("the cover is not drawn on the brand theme's ground %s", theme.Feature)
	}
}
