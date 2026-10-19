package artifactupload

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/canvasformat"
	"github.com/crewlet/crewlet/internal/artifacts/manifest"
	"github.com/crewlet/crewlet/internal/artifacts/slideformat"
	"github.com/crewlet/crewlet/internal/artifactupload/lint"
)

// checkFormat holds the composed version to its kind's format: for a deck,
// the index, every slide it orders and every file they name, read whole and
// refused, finding by finding, when any of it is outside the slide format.
//
// THE WHOLE VERSION, NOT THE FILES THIS PUBLISH BRINGS: a patch that removes an
// image breaks a carried slide that shows it, and an index that drops a slide
// from its order leaves that slide's file unplaced, so what is judged is what
// the version will be. A carried slide is read back through the verifying
// reader; a deck is small, and every publish of one reads it once.
func (r *run) checkFormat(ctx context.Context, m artifacts.Manifest) error {
	spec, _ := r.kind.Spec()
	switch spec.Format {
	case artifacts.FormatNone:
		return nil
	case artifacts.FormatSlides:
		deck, err := slideformat.Read(versionSource{ctx: ctx, run: r, m: m})
		var refused *slideformat.Refused
		if errors.As(err, &refused) {
			return slideRefusal(refused)
		}
		if err != nil {
			return err
		}
		r.deck = &deck
		return nil
	case artifacts.FormatDesign:
		return r.checkCanvas(versionSource{ctx: ctx, run: r, m: m})
	}
	return fmt.Errorf("artifactupload: a %s artifact's format %q is not one this pipeline "+
		"validates", r.kind, spec.Format)
}

// slideRefusal is a deck's refusal in the linter's shape, so a publish
// answers every kind's policy findings the same way.
func slideRefusal(refused *slideformat.Refused) error {
	out := &lint.Refusal{Findings: make([]lint.Finding, len(refused.Findings))}
	for i, f := range refused.Findings {
		out.Findings[i] = lint.Finding{Path: f.Path, Line: f.Line, Code: lint.CodeSlideFormat,
			Message: string(f.Rule) + ": " + f.Message}
	}
	return out
}

// versionSource is a composed version as the slide format reads it: the bytes
// this run holds for a file it brings, else the object the manifest names.
type versionSource struct {
	ctx context.Context
	run *run
	m   artifacts.Manifest
}

func (v versionSource) Paths() []string {
	out := make([]string, len(v.m.Files))
	for i, e := range v.m.Files {
		out[i] = e.Path
	}
	return out
}

func (v versionSource) Essence(p string) (string, bool) {
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

func (v versionSource) Read(p string) ([]byte, error) {
	e, ok := manifest.EntryAt(v.m, p)
	if !ok {
		return nil, fmt.Errorf("artifactupload: the version holds no %q", p)
	}
	if f := v.run.files[p]; f != nil && f.text != nil && !f.carried {
		return f.text, nil
	}
	return readObject(v.ctx, v.run.s.objects, e.Object, true)
}

// checkCanvas holds a design's canvas to its format and to the version's pages,
// answering every problem as a policy finding on canvas.json, and keeps the
// canvas for the search text. The entry is canvas.json already: the kind's
// entry rule fixes it.
func (r *run) checkCanvas(v versionSource) error {
	canvas, err := canvasformat.Read(v, v.m)
	var refused *canvasformat.Error
	if errors.As(err, &refused) {
		return designRefusal(refused.Problems...)
	}
	if err != nil {
		return err
	}
	r.canvas = &canvas
	return nil
}

// designRefusal is a canvas's problems in the linter's shape, each on
// canvas.json at its place in it.
func designRefusal(problems ...canvasformat.Problem) error {
	out := &lint.Refusal{Findings: make([]lint.Finding, len(problems))}
	for i, p := range problems {
		out.Findings[i] = lint.Finding{Path: canvasformat.Path, Code: lint.CodeDesignFormat,
			Message: p.String()}
	}
	return out
}

// canvasText is what a design says: the canvas's own words, then each
// artboard's readable text in the canvas's order, until the bound.
func (r *run) canvasText(ctx context.Context, m artifacts.Manifest) (string, error) {
	limit := int(artifacts.MaxSearchTextBytes)
	v := versionSource{ctx: ctx, run: r, m: m}
	parts := []string{r.canvas.SearchText()}
	used := len(parts[0])
	for _, p := range r.canvas.Order {
		if used >= limit {
			break
		}
		text, err := v.Read(p)
		if err != nil {
			return "", err
		}
		page := lint.Text(lint.Source(text), limit-used)
		parts = append(parts, page)
		used += len(page) + 1
	}
	return truncateText(strings.Join(parts, "\n"), limit), nil
}
