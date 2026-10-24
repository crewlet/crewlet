package artifactupload

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/artifacts/brandformat"
	"github.com/crewlet/crewlet/internal/artifacts/mediatype"
	"github.com/crewlet/crewlet/internal/artifacts/slideformat"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A FILE COPIED FROM THE ADOPTED BRAND ([FileSpec.Brand]).
//
// The company's adopted brand keeps its logos and faces as rows of its own
// snapshot, each naming an object (ADR-0048). A version that shows the logo or
// sets a slide in the brand's typeface needs those bytes as a file of ITS OWN:
// a deck's image and face are files of the version, and an agent's page may
// carry the logo as an image. So the pipeline COPIES them: the snapshot's
// object is read back through the store's verifying reader and stored again as
// an object this run produced, under the run's own reservation, exactly as a
// copied CDN file is. The version never names the adoption's object, because
// one object is never shared between two rows (ADR-0026), and a later
// adoption (or a clear) changes nothing in a version already published.
//
// WHICH ADOPTION: the one current when the run reads it, at the linearizable
// level a seat's own reads take, so a copy made right after an administrator
// adopted is a copy of what they adopted. A slot the adoption does not hold,
// the generated stylesheet (which the artifact origin links, and no version
// carries), and a path whose type is not the file's are refused by name.
//
// A FACE AS A WHOLE (`font.heading`, `font.body`) COPIES ITS REGULAR FILE
// ([FaceFile]): a version names a face by one file, as a deck's index does, so
// the slot a person reads in the brand is the file a slide is set in. Each file
// of an uploaded family is also its own slot ([brandformat.FontFileSlot]) for
// the rare page that wants another weight.

// brandRead is the level the adoption is read at for a copy: a seat reads
// linearizable, and a copy is made of the brand the seat just read.
var brandRead = statelog.Freshness{Level: statelog.ReadLinearizable}

// BrandDir is where a version keeps the brand files the engine copies into it
// by default: a starter's logo and faces, and the paths get_company_brand
// suggests for an agent's own copies.
const BrandDir = "brand/"

// BrandPath is the path a brand file of slot and content type lands at by
// default: `brand/logo-primary.svg`, `brand/fonts/heading-0.woff2`. One rule,
// so the paths a starter writes and the ones an agent is told to copy to are
// the same paths.
func BrandPath(slot brandformat.Slot, contentType string) (string, error) {
	ext, ok := brandExt(contentType)
	if !ok {
		return "", fmt.Errorf("artifactupload: the brand's %s is %s, which the brand never "+
			"serves", slot, contentType)
	}
	name := strings.NewReplacer(".", "-", "_", "-").Replace(string(slot))
	if face, i, ok := fontFile(slot); ok {
		return BrandDir + "fonts/" + strings.TrimPrefix(string(face), "font.") + "-" +
			strconv.Itoa(i) + ext, nil
	}
	return BrandDir + name + ext, nil
}

// fontFile splits a face file's slot into its face and index
// ([brandformat.FontFileSlot]), false for any other slot.
func fontFile(slot brandformat.Slot) (brandformat.Slot, int, bool) {
	for _, face := range []brandformat.Slot{brandformat.SlotFontHeading, brandformat.SlotFontBody} {
		rest, ok := strings.CutPrefix(string(slot), string(face)+".")
		if !ok {
			continue
		}
		i, err := strconv.Atoi(rest)
		if err != nil || i < 0 || brandformat.FontFileSlot(face, i) != slot {
			return "", 0, false
		}
		return face, i, true
	}
	return "", 0, false
}

// brandExt is the extension, with its dot, a brand file of contentType is
// named by: the brand format's own ([brandformat.Ext]), so a copy is named as
// the public brand route serves it.
func brandExt(contentType string) (string, bool) {
	essence, _, _ := strings.Cut(strings.ToLower(strings.TrimSpace(contentType)), ";")
	ext, ok := brandformat.Ext(strings.TrimSpace(essence))
	if !ok {
		return "", false
	}
	return "." + ext, true
}

// adoption is the adopted brand this run copies from, read once per run.
func (r *run) adoption(ctx context.Context) (artifactsdomain.Adoption, error) {
	if r.brand != nil {
		return *r.brand, nil
	}
	a, err := r.s.adoption(ctx)
	if err != nil {
		return artifactsdomain.Adoption{}, err
	}
	r.brand = &a
	return a, nil
}

// adoption is the adopted brand now, or the refusal of a copy when the company
// has adopted none.
func (s *Service) adoption(ctx context.Context) (artifactsdomain.Adoption, error) {
	a, ok, err := s.reads.Brand(ctx, brandRead)
	switch {
	case err != nil:
		return artifactsdomain.Adoption{}, fmt.Errorf("artifactupload: read the adopted brand: %w", err)
	case !ok:
		return artifactsdomain.Adoption{}, fmt.Errorf("%w: the company has adopted no brand, so "+
			"there is nothing to copy from it; an administrator adopts one on Company, Brand", ErrInvalid)
	}
	return a, nil
}

// brandFile fills f with a copy of the adopted brand's file in slot: its bytes
// read back verified, its type the one the snapshot recorded, which the path's
// extension must serve.
func (r *run) brandFile(ctx context.Context, f *pending, slot brandformat.Slot) error {
	a, err := r.adoption(ctx)
	if err != nil {
		return err
	}
	if slot == brandformat.SlotCSS {
		return fmt.Errorf("%w: %q copies the brand's stylesheet, which the artifact origin links "+
			"to every page and no version carries; copy a logo or a face instead", ErrInvalid, f.path)
	}
	if slot == brandformat.SlotFontHeading || slot == brandformat.SlotFontBody {
		b, err := brandformat.Decode(a.BrandJSON) //nolint:govet // shadow: scoped to this block; see .golangci.yml
		if err != nil {
			return fmt.Errorf("artifactupload: the adopted brand's snapshot does not read: %w", err)
		}
		file, ok := FaceFile(b, slot)
		if !ok {
			return fmt.Errorf("%w: %q copies the brand's %s, which is a face every page already "+
				"has (%s) and no file of the brand; leave the copy out", ErrInvalid, f.path, slot,
				brandformat.FontStack(faceOf(b, slot)))
		}
		slot = file
	}
	i := slices.IndexFunc(a.Files, func(b artifactsdomain.BrandFile) bool { return b.Slot == string(slot) })
	if i < 0 {
		return fmt.Errorf("%w: %q copies the brand's %s, and the adopted brand holds none; it holds %s",
			ErrInvalid, f.path, slot, strings.Join(copyableSlots(a), ", "))
	}
	file := a.Files[i]
	typ, err := mediatype.Resolve(f.path, file.ContentType)
	if err != nil {
		want := "a path with its own extension"
		if ext, ok := brandExt(file.ContentType); ok {
			want = "a path ending " + ext
		}
		return fmt.Errorf("%w: %q copies the brand's %s, which is %s: name it with %s",
			ErrInvalid, f.path, slot, file.ContentType, want)
	}
	body, err := readObject(ctx, r.s.objects, file.Object, true)
	switch {
	case errors.Is(err, objstore.ErrNotFound), errors.Is(err, objstore.ErrCorrupt):
		return fmt.Errorf("artifactupload: the adopted brand's %s is not the object its snapshot "+
			"records: %w", slot, err)
	case err != nil:
		return fmt.Errorf("artifactupload: read the adopted brand's %s: %w", slot, err)
	}
	if err := checkContent(f.path, typ, headOf(body, typ.Text), int64(len(body))); err != nil {
		return err
	}
	f.typ, f.text, f.produced = typ, body, true
	return nil
}

// copyableSlots are the slots of the adoption a version may copy, in the
// snapshot's order: every file but the stylesheet.
func copyableSlots(a artifactsdomain.Adoption) []string {
	out := make([]string, 0, len(a.Files))
	for _, f := range a.Files {
		if f.Slot != string(brandformat.SlotCSS) {
			out = append(out, f.Slot)
		}
	}
	if len(out) == 0 {
		return []string{"no file"}
	}
	return out
}

// ---- the brand starter --------------------------------------------------- //

// brandLook is the look a deck begun in [slideformat.BrandTheme] is drawn in,
// from the brand adopted now, and the copies the version takes with it: the
// logo the cover shows and one file of each uploaded typeface.
//
// THE COVER'S LOGO IS THE ONE MEANT FOR ITS GROUND ([CoverLogo]).
//
// ONE FILE PER FAMILY, because a deck declares a face as a family and one font
// file, with no weight or style beside it: the regular file nearest the weight
// the theme sets that face in. A heading and a body set in the same uploaded
// family are declared once.
func (s *Service) brandLook(ctx context.Context) (slideformat.Look, []FileSpec, *artifactsdomain.Adoption, error) {
	a, err := s.adoption(ctx)
	if err != nil {
		return slideformat.Look{}, nil, nil, fmt.Errorf("%w (a deck in %q begins from it); pick one of "+
			"the engine's themes instead", err, slideformat.BrandTheme)
	}
	b, err := brandformat.Decode(a.BrandJSON)
	if err != nil {
		return slideformat.Look{}, nil, nil, fmt.Errorf("artifactupload: the adopted brand's "+
			"snapshot does not read: %w", err)
	}
	theme := brandformat.SlideTheme(b)
	look := slideformat.Look{Theme: theme, LogoAlt: b.Name}
	held := map[string]artifactsdomain.BrandFile{}
	for _, f := range a.Files {
		held[f.Slot] = f
	}
	var specs []FileSpec
	copyTo := func(slot brandformat.Slot) (string, error) {
		f, ok := held[string(slot)]
		if !ok {
			return "", fmt.Errorf("artifactupload: the adopted brand's snapshot holds no %s, "+
				"which its brand.json names", slot)
		}
		dest, pathErr := BrandPath(slot, f.ContentType)
		if pathErr != nil {
			return "", pathErr
		}
		specs = append(specs, FileSpec{Path: dest, Brand: slot})
		return dest, nil
	}

	logo, err := CoverLogo(theme, a.Files)
	if err != nil {
		return slideformat.Look{}, nil, nil, err
	}
	if look.Logo, err = copyTo(brandformat.Slot(logo.Slot)); err != nil {
		return slideformat.Look{}, nil, nil, err
	}

	for _, face := range []struct {
		key  string
		slot brandformat.Slot
	}{{"heading", brandformat.SlotFontHeading}, {"body", brandformat.SlotFontBody}} {
		family := faceOf(b, face.slot).Family
		file, ok := FaceFile(b, face.slot)
		if !ok || declares(look.Faces, family) {
			continue
		}
		src, err := copyTo(file)
		if err != nil {
			return slideformat.Look{}, nil, nil, err
		}
		if look.Faces == nil {
			look.Faces = map[string]slideformat.IndexFace{}
		}
		look.Faces[face.key] = slideformat.IndexFace{Family: family, Src: src}
	}
	return look, specs, &a, nil
}

// CoverLogo is the logo a deck begun in the brand's slide theme shows on its
// cover, of the adoption's files: THE ONE MEANT FOR ITS GROUND, the logo for
// dark grounds when the cover's feature ground is dark (white clears more on
// it than black, the rule the kit's label on the accent follows), and the
// primary logo otherwise or where the brand has no dark one. One rule for the
// deck a starter writes and the sample its theme is picked from.
func CoverLogo(theme slideformat.Theme, files []artifactsdomain.BrandFile) (artifactsdomain.BrandFile, error) {
	held := map[string]artifactsdomain.BrandFile{}
	for _, f := range files {
		held[f.Slot] = f
	}
	onWhite, err := brandformat.Ratio(theme.Feature, "#ffffff")
	if err != nil {
		return artifactsdomain.BrandFile{}, fmt.Errorf("artifactupload: the brand's slide theme "+
			"grounds its cover in %q: %w", theme.Feature, err)
	}
	onBlack, err := brandformat.Ratio(theme.Feature, "#000000")
	if err != nil {
		return artifactsdomain.BrandFile{}, fmt.Errorf("artifactupload: the brand's slide theme "+
			"grounds its cover in %q: %w", theme.Feature, err)
	}
	if dark, ok := held[string(brandformat.SlotLogoOnDark)]; ok && onWhite > onBlack {
		return dark, nil
	}
	primary, ok := held[string(brandformat.SlotLogoPrimary)]
	if !ok {
		return artifactsdomain.BrandFile{}, fmt.Errorf("artifactupload: the adopted brand's "+
			"snapshot holds no %s, which its brand.json names", brandformat.SlotLogoPrimary)
	}
	return primary, nil
}

// FaceFile is the slot of the file a version sets the brand's face in (the
// heading's or the body's): the regular file of an uploaded family nearest the
// weight the brand's slide theme sets that face in. False for a face with no
// file, a kit face or a system stack, which every page already has. One rule
// for a deck begun in the brand, a `from_brand` copy of a face, and the face
// get_company_brand tells an agent to declare.
func FaceFile(b brandformat.Brand, face brandformat.Slot) (brandformat.Slot, bool) {
	f := faceOf(b, face)
	if len(f.Files) == 0 {
		return "", false
	}
	weight := regularWeight
	if face == brandformat.SlotFontHeading {
		weight = brandformat.SlideTheme(b).DisplayWeight
	}
	return brandformat.FontFileSlot(face, nearestRegular(f.Files, weight)), true
}

// faceOf is the brand's face in slot: the heading's for
// [brandformat.SlotFontHeading], the body's otherwise.
func faceOf(b brandformat.Brand, slot brandformat.Slot) brandformat.Face {
	if slot == brandformat.SlotFontHeading {
		return b.Type.Heading
	}
	return b.Type.Body
}

// regularWeight is body text's weight, CSS's `normal`.
const regularWeight = 400

// nearestRegular is the index of the file of files a deck sets text of weight
// in: an upright file before an italic one, then the weight nearest, the
// lighter of two equally near, then the first.
func nearestRegular(files []brandformat.FontFile, weight int) int {
	best := 0
	score := func(f brandformat.FontFile) (int, int, int) {
		italic := 0
		if f.Style == "italic" {
			italic = 1
		}
		d := f.Weight - weight
		if d < 0 {
			d = -d
		}
		return italic, d, f.Weight
	}
	for i := 1; i < len(files); i++ {
		bi, bd, bw := score(files[best])
		ci, cd, cw := score(files[i])
		if ci < bi || ci == bi && (cd < bd || cd == bd && cw < bw) {
			best = i
		}
	}
	return best
}

// declares reports whether faces already declares family.
func declares(faces map[string]slideformat.IndexFace, family string) bool {
	for _, f := range faces {
		if f.Family == family {
			return true
		}
	}
	return false
}
