package artifactupload

import (
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/docindex"
	"github.com/crewlet/crewlet/internal/artifacts/slideformat"
)

// starter is the content a create that asks for its kind's starter publishes:
// the files, inline, and the entry they open on. A kind's starter is its
// format's, so the engine holds it and holds it to that format like any
// version; a kind whose format has none is refused.
//
// THE ENGINE'S RATHER THAN THE CLIENT'S because a create from the dashboard
// carries no upload, and a deployment whose object store presigns transfers
// sends them to an origin the dashboard's own policy does not let it reach.
func starter(kind artifacts.Kind, title string) ([]FileSpec, string, error) {
	var (
		files map[string][]byte
		entry string
	)
	switch spec, _ := kind.Spec(); spec.Format {
	case artifacts.FormatSlides:
		files, entry = slideformat.Starter(title), slideformat.IndexPath
	case artifacts.FormatDoc:
		files, entry = docindex.Starter(title), docindex.StarterEntry
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
