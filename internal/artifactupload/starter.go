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
// format's, so the engine holds it and holds it to that format like any
// version; a kind whose format has none is refused. Each kind's own field
// is refused on any other kind. A doc's may carry the text its writer wrote
// (req.Doc), held to the inline bound of a call. A deck's is composed from
// its plan (req.Deck; nil is [slideformat.DefaultPlan]), and a plan the
// format does not hold is refused as invalid, naming what it holds. A deck
// whose plan names [slideformat.BrandTheme] is drawn in the brand adopted now
// ([Service.brandLook]), its logo and faces copied into the version beside
// its slides; with no brand adopted it is refused, naming the themes there
// are. A design's is the layout the person drew (req.Layout, its boards as
// placeholder pages), which a design always names.
//
// THE ENGINE'S RATHER THAN THE CLIENT'S because a create from the dashboard
// carries no upload, and a deployment whose object store presigns transfers
// sends them to an origin the dashboard's own policy does not let it reach.
//
// A deck begun in the brand also answers the adoption it was composed from, so
// the run copies its logo and faces from that same adoption.
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
