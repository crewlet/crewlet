package artifactupload

import (
	"context"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/canvasformat"
	"github.com/crewlet/crewlet/internal/artifacts/docindex"
	"github.com/crewlet/crewlet/internal/artifacts/slideformat"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
)

// DocStarter is a doc begun with its text already written: the one tab's
// Markdown under the title, which the engine writes as the tab's heading.
type DocStarter struct {
	Body string `json:"body"`
}

// starter is the content a create that asks for its kind's starter publishes:
// the files, inline, and the entry they open on. A kind's starter is its
// format's, held to that format; a kind whose format has none is refused, and
// each kind's own field is refused on any other kind. A doc's may carry its
// writer's text (req.Doc); a deck's is composed from its plan (req.Deck; nil
// is [slideformat.DefaultPlan]), one naming [slideformat.BrandTheme] drawn in
// the brand adopted now ([Service.brandLook]), refused if none; a design's is
// the layout drawn (req.Layout). THE ENGINE'S RATHER THAN THE CLIENT'S: a
// dashboard create carries no upload, and a presigning store's origin is one
// the dashboard's policy blocks. A brand deck answers the adoption it was
// composed from, so the run copies from that same adoption.
func (s *Service) starter(ctx context.Context, req CreateRequest) ([]FileSpec, string,
	*artifactsdomain.Adoption, error) {

	var (
		files   map[string][]byte
		entry   string
		copies  []FileSpec
		adopted *artifactsdomain.Adoption
	)
	kind, title := req.Kind, req.Title
	spec, _ := kind.Spec()
	if req.Doc != nil && spec.Format != artifacts.FormatDoc {
		return nil, "", nil, fmt.Errorf("%w: a %s artifact does not begin from a doc's text", ErrInvalid, kind)
	}
	if req.Deck != nil && spec.Format != artifacts.FormatSlides {
		return nil, "", nil, fmt.Errorf("%w: a %s artifact does not begin from a deck plan", ErrInvalid, kind)
	}
	if (req.Layout != nil) != (spec.Format == artifacts.FormatDesign) {
		return nil, "", nil, fmt.Errorf("%w: a layout is a design's starter, and a design starts from one", ErrInvalid)
	}
	switch spec.Format {
	case artifacts.FormatDesign:
		var err error
		if files, err = canvasformat.Starter(title, *req.Layout); err != nil {
			return nil, "", nil, fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		entry = canvasformat.Path
	case artifacts.FormatSlides:
		var p slideformat.Plan
		if req.Deck != nil {
			p = *req.Deck
		}
		var (
			composed map[string][]byte
			err      error
		)
		if p.Theme == slideformat.BrandTheme {
			var look slideformat.Look
			if look, copies, adopted, err = s.brandLook(ctx); err != nil {
				return nil, "", nil, err
			}
			composed, err = slideformat.ComposeBranded(title, p, look)
		} else {
			composed, err = slideformat.Compose(title, p)
		}
		if err != nil {
			return nil, "", nil, fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		files, entry = composed, slideformat.IndexPath
	case artifacts.FormatDoc:
		files, entry = docindex.Starter(title), docindex.StarterEntry
		if req.Doc != nil {
			files = docindex.Written(title, req.Doc.Body)
			if n := int64(len(files[entry])); n > artifacts.MaxInlineBytes {
				return nil, "", nil, fmt.Errorf("%w: the doc's text is %d bytes and a doc begun with "+
					"its text carries at most %d; start it shorter and add the rest as a new version",
					ErrInvalid, n, artifacts.MaxInlineBytes)
			}
		}
	default:
		return nil, "", nil, fmt.Errorf("%w: a %s artifact has no starter to begin from", ErrInvalid, kind)
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	out := make([]FileSpec, len(paths), len(paths)+len(copies))
	for i, p := range paths {
		out[i] = FileSpec{Path: p, Inline: files[p]}
	}
	return append(out, copies...), entry, adopted, nil
}
