package tracker

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/objstore"
)

// A FILE RECORD THAT IS NOT THE FILE ITS SUBJECT ARBITRATED IS REFUSED, and so
// is one naming a chunk that is not a content address: the first would leave
// the arbitrated path held by nothing, the second a row the collector and the
// backup refuse to read, stopping both for good.
func TestTheApplierRefusesAFileThatIsNotItsSubject(t *testing.T) {
	t.Parallel()
	h := objstore.HashOf([]byte("content"))
	good := File{Project: "ENG", Path: "a/b.md", Chunks: []FileChunk{{Hash: h, Size: 7}}}
	id := FileSubject("ENG", "a/b.md").ID
	if err := fileMatches(applyContext{}, id, good); err != nil {
		t.Fatalf("the control was refused: %v", err)
	}
	for name, f := range map[string]File{
		"another path":    {Project: "ENG", Path: "a/c.md", Chunks: good.Chunks},
		"another project": {Project: "OPS", Path: "a/b.md", Chunks: good.Chunks},
		"an unnormalised": {Project: "ENG", Path: "/a/b.md", Chunks: good.Chunks},
		"an invalid chunk": {Project: "ENG", Path: "a/b.md",
			Chunks: []FileChunk{{Hash: h, Size: 7}, {Hash: "zz", Size: 7}}},
		// UPPERCASE IS NOT AN ADDRESS: a name is read back through
		// objstore.ParseHash, which takes lowercase hex only.
		"an uppercase chunk": {Project: "ENG", Path: "a/b.md",
			Chunks: []FileChunk{{Hash: objstore.Hash(strings.ToUpper(string(h))), Size: 7}}},
	} {
		if err := fileMatches(applyContext{}, id, f); err == nil {
			t.Errorf("%s was applied", name)
		}
	}
}
