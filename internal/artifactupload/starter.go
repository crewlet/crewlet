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
// text its writer wrote (doc), held to the inline bound of a call; a doc's
// text on any other kind is refused.
//
// THE ENGINE'S RATHER THAN THE CLIENT'S because a create from the dashboard
// carries no upload, and a deployment whose object store presigns transfers
// sends them to an origin the dashboard's own policy does not let it reach.
func starter(kind artifacts.Kind, title string, doc *DocStarter) ([]FileSpec, string, error) {
	var (
		files map[string][]byte
		entry string
	)
	spec, _ := kind.Spec()
	if doc != nil && spec.Format != artifacts.FormatDoc {
		return nil, "", fmt.Errorf("%w: a %s artifact does not begin from a doc's text", ErrInvalid, kind)
	}
	switch spec.Format {
	case artifacts.FormatSlides:
		files, entry = slideformat.Starter(title), slideformat.IndexPath
	case artifacts.FormatDoc:
		files, entry = docindex.Starter(title), docindex.StarterEntry
		if doc != nil {
			files = docindex.Written(title, doc.Body)
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
