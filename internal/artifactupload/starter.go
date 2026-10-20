package artifactupload

import (
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/docindex"
	"github.com/crewlet/crewlet/internal/artifacts/slideformat"
)

// DocStarter is a doc begun with its text already written: the one tab's
// Markdown under the title, which the engine writes as the tab's heading.
type DocStarter struct {
	Body string `json:"body"`
}

// starter is the content a create that asks for its kind's starter publishes:
// the files, inline, and the entry they open on. A kind's starter is its
// format's, so the engine holds it and holds it to that format like any
// version; a kind whose format has none is refused. A doc's may carry the
// text its writer wrote (req.Doc), held to the inline bound of a call. A
// deck's is composed from its plan (req.Deck; nil is
// [slideformat.DefaultPlan]), and a plan the format does not hold is refused
// as invalid, naming what it holds. Each kind's field is refused on any other
// kind.
//
// THE ENGINE'S RATHER THAN THE CLIENT'S because a create from the dashboard
// carries no upload, and a deployment whose object store presigns transfers
// sends them to an origin the dashboard's own policy does not let it reach.
func starter(req CreateRequest) ([]FileSpec, string, error) {
	var (
		files map[string][]byte
		entry string
	)
	kind, title := req.Kind, req.Title
	spec, _ := kind.Spec()
	if req.Doc != nil && spec.Format != artifacts.FormatDoc {
		return nil, "", fmt.Errorf("%w: a %s artifact does not begin from a doc's text", ErrInvalid, kind)
	}
	if req.Deck != nil && spec.Format != artifacts.FormatSlides {
		return nil, "", fmt.Errorf("%w: a %s artifact does not begin from a deck plan", ErrInvalid, kind)
	}
	switch spec.Format {
	case artifacts.FormatSlides:
		var p slideformat.Plan
		if req.Deck != nil {
			p = *req.Deck
		}
		composed, err := slideformat.Compose(title, p)
		if err != nil {
			return nil, "", fmt.Errorf("%w: %w", ErrInvalid, err)
		}
		files, entry = composed, slideformat.IndexPath
	case artifacts.FormatDoc:
		files, entry = docindex.Starter(title), docindex.StarterEntry
		if req.Doc != nil {
			files = docindex.Written(title, req.Doc.Body)
			if n := int64(len(files[entry])); n > artifacts.MaxInlineBytes {
				return nil, "", fmt.Errorf("%w: the doc's text is %d bytes and a doc begun with "+
					"its text carries at most %d; start it shorter and add the rest as a new version",
					ErrInvalid, n, artifacts.MaxInlineBytes)
			}
		}
	default:
		return nil, "", fmt.Errorf("%w: a %s artifact has no starter to begin from", ErrInvalid, kind)
	}
	paths := make([]string, 0, len(files))
	for p := range files {
		paths = append(paths, p)
	}
	slices.Sort(paths)
	out := make([]FileSpec, len(paths))
	for i, p := range paths {
		out[i] = FileSpec{Path: p, Inline: files[p]}
	}
	return out, entry, nil
}
