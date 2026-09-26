package tracker

import (
	"testing"

	"github.com/crewlet/crewlet/internal/objstore"
)

// A FILE RECORD THAT IS NOT THE FILE ITS SUBJECT ARBITRATED IS REFUSED, and so
// is one filing a chunk under a placement group its hash does not place it in:
// the first would leave the arbitrated path held by nothing, the second a chunk
// the collector never finds among its group's references.
func TestTheApplierRefusesAFileThatIsNotItsSubject(t *testing.T) {
	t.Parallel()
	h := objstore.HashOf([]byte("content"))
	good := File{Project: "ENG", Path: "a/b.md",
		Chunks: []FileChunk{{Hash: h, Size: 7, PG: h.PG()}}}
	id := FileSubject("ENG", "a/b.md").ID
	if err := fileMatches(applyContext{}, id, good); err != nil {
		t.Fatalf("the control was refused: %v", err)
	}
	for name, f := range map[string]File{
		"another path":     {Project: "ENG", Path: "a/c.md", Chunks: good.Chunks},
		"another project":  {Project: "OPS", Path: "a/b.md", Chunks: good.Chunks},
		"an unnormalised":  {Project: "ENG", Path: "/a/b.md", Chunks: good.Chunks},
		"a wrong group":    {Project: "ENG", Path: "a/b.md", Chunks: []FileChunk{{Hash: h, Size: 7, PG: (h.PG() + 1) % 256}}},
		"an invalid chunk": {Project: "ENG", Path: "a/b.md", Chunks: []FileChunk{{Hash: "zz", Size: 7}}},
	} {
		if err := fileMatches(applyContext{}, id, f); err == nil {
			t.Errorf("%s was applied", name)
		}
	}
}
