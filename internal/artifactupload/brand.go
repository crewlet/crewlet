package artifactupload

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/brandformat"
	"github.com/crewlet/crewlet/internal/artifacts/manifest"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/artifactupload/lint"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE BRAND THROUGH THE PIPELINE: a kit's logos stored in the form the engine
// writes, a framed version's opt-out read from its entry page, and the
// adoption, which copies every file the adopted brand serves.
//
// A KIT'S LOGOS ARE STORED SANITISED (brandformat.Sanitize): an SVG as svgsafe
// re-serialised it, a PNG or WebP without its metadata, so the bytes a kit
// version names are already the bytes a brand serves, and a logo the dashboard
// draws from a draft kit is the engine's form, never the author's.
//
// AN ADOPTION COPIES, NEVER NAMES: every logo and face of the adopted version
// is read back, sanitised again by this build's rules and stored as a fresh
// object beside the generated stylesheet, all under one reservation, so the
// adopted brand names only objects its own record uploaded (artifactsdomain's
// brand file has the reason).

// brandAssets are the files of a kit that are stored sanitised: its logos. A
// face is stored as it came once its header is inside the bounds, which
// brandformat.Read checks with the rest of the kit.
var brandAssets = []string{"image/svg+xml", "image/png", "image/webp"}

// sanitizeBrandAssets replaces every logo a kit's publish brings new bytes for
// with its sanitised form, refusing the publish, finding by finding, for one
// outside its asset rule.
func (r *run) sanitizeBrandAssets(ctx context.Context) error {
	var refusals []lint.Finding
	for _, p := range r.sortedPaths() {
		f := r.files[p]
		essence := f.typ.Essence()
		if f.carried || !slices.Contains(brandAssets, essence) {
			continue
		}
		data := f.text
		if data == nil {
			if !f.hasObj {
				continue
			}
			// PAST ITS BOUND BY ITS RECORDED SIZE is refused before a byte
			// of it is read.
			if msg := brandformat.TooLarge(essence, f.entry.Object.Size); msg != "" {
				refusals = append(refusals, brandFindings(&brandformat.Refused{Findings: []brandformat.Finding{
					{Path: p, Rule: brandformat.RuleAsset, Message: msg}}})...)
				continue
			}
			// A RASTER UPLOAD IS HELD AS THE OBJECT IT ARRIVED AS: its
			// bytes are read back, verified, to be sanitised.
			read, err := readObject(ctx, r.s.objects, f.entry.Object, true)
			if err != nil {
				return err
			}
			data = read
		}
		out, err := brandformat.Sanitize(p, essence, data)
		var refused *brandformat.Refused
		switch {
		case errors.As(err, &refused):
			refusals = append(refusals, brandFindings(refused)...)
			continue
		case err != nil:
			return err
		}
		f.text = data
		f.produce(out)
	}
	if len(refusals) > 0 {
		// THE INDEX IS JUDGED IN THE SAME ANSWER: a refused logo stops the
		// publish before the format check, which would otherwise wait for
		// the next attempt to report the index's own faults, so a kit with
		// a bad SVG and a bad brand.json took two refusals to learn both.
		index, err := brandformat.CheckIndex(pendingSource{ctx: ctx, run: r})
		if err != nil {
			return err
		}
		refusals = append(refusals, BrandFindings(index)...)
		return &lint.Refusal{Findings: refusals}
	}
	return nil
}

// pendingSource is the version a run is composing, before its manifest: every
// file the publish brings or carries, read from the bytes the run holds or the
// object it names.
type pendingSource struct {
	ctx context.Context
	run *run
}

func (p pendingSource) Paths() []string { return p.run.sortedPaths() }

func (p pendingSource) Essence(path string) (string, bool) {
	f, ok := p.run.files[path]
	if !ok {
		return "", false
	}
	return f.typ.Essence(), true
}

func (p pendingSource) Read(path string) ([]byte, error) {
	f, ok := p.run.files[path]
	switch {
	case !ok:
		return nil, fmt.Errorf("artifactupload: the version holds no %q", path)
	case f.text != nil:
		return f.text, nil
	case f.hasObj:
		return readObject(p.ctx, p.run.s.objects, f.entry.Object, true)
	}
	return nil, fmt.Errorf("artifactupload: %q holds no bytes yet", path)
}

// brandFindings is a brand refusal's findings in the linter's shape
// ([brandRefusal]), for a run that gathers every file's before it answers.
func brandFindings(refused *brandformat.Refused) []lint.Finding {
	var policy *lint.Refusal
	if errors.As(brandRefusal(refused), &policy) {
		return policy.Findings
	}
	return nil
}

// brandOff reports a framed version that opts out of the adopted brand: an
// html version whose entry page says so on its `<html>` element
// ([lint.BrandOff]). A design's entry is its canvas, not a page, so a design
// pins the brand and each artboard that opts out is served without it.
func (r *run) brandOff(ctx context.Context, m artifacts.Manifest) (bool, error) {
	if r.kind != artifacts.KindHTML || m.Entry == "" {
		return false, nil
	}
	page, err := versionSource{ctx: ctx, run: r, m: m}.Read(m.Entry)
	if err != nil {
		return false, err
	}
	return lint.BrandOff(page), nil
}

// AdoptBrand adopts version of the brand kit kit as the company's brand: the
// kit read back and held to the brand format, its contrast checked, every file
// it serves copied as a fresh object beside the stylesheet generated from it,
// and the adoption decided by the domain. A refusal leaves nothing behind; an
// outcome nobody could establish leaves everything for the retry of the same
// operation, which is answered with what the first attempt landed.
//
// The caller decided the authority (an administrator, a person, a recent
// proof) and admitted the read of the kit's content; the pipeline decides
// what the kit is.
func (s *Service) AdoptBrand(ctx context.Context, c artifactsdomain.Call, kit artifacts.ID,
	version int) (artifactsdomain.Written, error) {

	principal, err := principalOf(c)
	if err != nil {
		return artifactsdomain.Written{}, err
	}
	head, _, err := s.reads.Artifact(ctx, kit, statelog.Freshness{Level: statelog.ReadStale})
	switch {
	case err != nil:
		return artifactsdomain.Written{}, err
	case head.Kind != artifacts.KindBrand:
		return artifactsdomain.Written{}, fmt.Errorf("%w: artifact %s is a %s, and only a brand kit "+
			"is adopted as the company's brand", ErrInvalid, kit, head.Kind)
	case head.Status == artifactsdomain.StatusTrashed:
		return artifactsdomain.Written{}, fmt.Errorf("%w: %s", artifactsdomain.ErrTrashed, kit)
	}
	v, err := s.reads.Version(ctx, kit, version)
	if err != nil {
		return artifactsdomain.Written{}, err
	}
	src := storedSource{ctx: ctx, objects: s.objects, m: v.Manifest}
	k, err := brandformat.ReadKit(src)
	var refused *brandformat.Refused
	if errors.As(err, &refused) {
		return artifactsdomain.Written{}, brandRefusal(refused)
	}
	if err != nil {
		return artifactsdomain.Written{}, err
	}
	adoptable, refusal := artifactsdomain.CheckBrandAdoptable(k.Brand)
	if refusal != nil {
		return artifactsdomain.Written{}, refusal
	}
	canonical, err := brandformat.Canonical(k.Brand)
	if err != nil {
		return artifactsdomain.Written{}, err
	}
	r := &run{s: s, c: c, principal: principal, kind: artifacts.KindBrand,
		files: map[string]*pending{}, removes: map[string]bool{}, vendored: map[string]bool{},
		versions: map[int]artifactsdomain.Version{}}
	if c.OpKey != "" {
		if r.stage, r.stageNew, err = s.stageFor(ctx, c, principal, "brand:"+kit.String(),
			artifacts.ID{}); err != nil {
			return artifactsdomain.Written{}, err
		}
	}
	if done, ok := r.answered(); ok {
		return artifactsdomain.Written{Result: done.Published.Result}, nil
	}
	paths, err := r.copyBrand(k, adoptable)
	if err != nil {
		return artifactsdomain.Written{}, r.fail(ctx, err)
	}
	if err = r.stageProduced(ctx); err != nil {
		return artifactsdomain.Written{}, r.fail(ctx, err)
	}
	in := artifactsdomain.BrandAdoptInput{Kit: kit, Version: version, BrandJSON: canonical}
	named := artifacts.Manifest{}
	for i, p := range paths {
		f := r.files[p]
		slot := brandformat.SlotCSS
		if i > 0 {
			slot = k.Files[i-1].Slot
		}
		in.Files = append(in.Files, artifactsdomain.BrandFile{Slot: string(slot),
			ContentType: f.typ.Served, Object: f.entry.Object})
		in.Uploaded = append(in.Uploaded, f.entry.Object.Key)
		named.Files = append(named.Files, f.entry)
	}
	written, err := s.writes.AdoptBrand(ctx, c, in)
	if err != nil {
		return written, r.fail(ctx, err)
	}
	r.close(ctx, artifactsdomain.Published{Artifact: kit, Number: version, Result: written.Result}, named)
	return written, nil
}

// copyBrand lays out the adoption's files as files this run produces: the
// stylesheet first, then a sanitised copy of every file the kit serves, in
// the kit's order. It answers their paths in that order: the slot's name and
// the file's extension, which is all a produced file's path is for.
//
// THE COPY IS THE STORED FORM THE KIT WAS READ WITH (brandformat.KitFile's
// Stored): sanitised by this build's rules in the one read brandformat.ReadKit
// made, never read and sanitised a second time.
func (r *run) copyBrand(k brandformat.Kit, a brandformat.Adoptable) ([]string, error) {
	served := make(map[brandformat.Slot]brandformat.ServedFile, len(k.Files))
	paths := []string{brandStylesheet}
	for _, kf := range k.Files {
		out := kf.Stored
		ext, ok := brandformat.Ext(kf.Essence)
		if !ok {
			return nil, fmt.Errorf("artifactupload: the kit serves %s as %s, which the brand route "+
				"has no extension for", kf.Path, kf.Essence)
		}
		p := string(kf.Slot) + "." + ext
		if err := r.produceFile(p, out); err != nil {
			return nil, err
		}
		paths = append(paths, p)
		served[kf.Slot] = brandformat.ServedFile{Hash: objstore.HashOf(out), Essence: kf.Essence,
			Ratio: kf.Ratio}
	}
	css := brandformat.CSS(a, served)
	if len(css) == 0 || len(css) > artifactsdomain.MaxBrandCSSBytes {
		return nil, fmt.Errorf("artifactupload: the brand stylesheet generated for the kit is %d "+
			"bytes, outside 1 to %d", len(css), artifactsdomain.MaxBrandCSSBytes)
	}
	if err := r.produceFile(brandStylesheet, css); err != nil {
		return nil, err
	}
	return paths, nil
}

// brandStylesheet is the generated stylesheet's place among an adoption's
// produced files.
const brandStylesheet = "brand.css"

// produceFile adds bytes the run stores to its files, at p.
func (r *run) produceFile(p string, data []byte) error {
	typ, err := typeOf(p, "")
	if err != nil {
		return err
	}
	r.files[p] = &pending{path: p, typ: typ, text: data, produced: true}
	return nil
}

// storedSource is a stored version as a format reads it: every file the
// manifest names, read back through the verifying reader.
type storedSource struct {
	ctx     context.Context
	objects *objstore.Store
	m       artifacts.Manifest
}

func (v storedSource) Paths() []string {
	out := make([]string, len(v.m.Files))
	for i, e := range v.m.Files {
		out[i] = e.Path
	}
	return out
}

func (v storedSource) Essence(p string) (string, bool) {
	e, ok := manifest.EntryAt(v.m, p)
	if !ok {
		return "", false
	}
	typ, err := typeOf(e.Path, e.ContentType)
	if err != nil {
		return "", false
	}
	return typ.Essence(), true
}

// Size is the size the version's manifest records for the file at p, which
// brandformat holds to its bound before reading it ([brandformat.Sizer]).
func (v storedSource) Size(p string) (int64, bool) {
	e, ok := manifest.EntryAt(v.m, p)
	if !ok {
		return 0, false
	}
	return e.Object.Size, true
}

func (v storedSource) Read(p string) ([]byte, error) {
	e, ok := manifest.EntryAt(v.m, p)
	if !ok {
		return nil, fmt.Errorf("artifactupload: the version holds no %q", p)
	}
	return readObject(v.ctx, v.objects, e.Object, true)
}
