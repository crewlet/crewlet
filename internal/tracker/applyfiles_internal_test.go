package tracker

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
)

// A FILE RECORD THAT IS NOT THE FILE ITS SUBJECT ARBITRATED IS REFUSED, and so
// is one naming an object beside a digest no reader could check it against:
// the first would leave the arbitrated path held by nothing, the second a row
// the backup refuses to read, stopping every backup for good.
//
// AND A RECORD NAMING NO OBJECT APPLIES: a removal names none, and nor does a
// put an earlier build wrote naming chunks — which every node must apply
// alike, as a live file whose content this build cannot read, or the log
// stops at it.
func TestTheApplierRefusesAFileThatIsNotItsSubject(t *testing.T) {
	t.Parallel()
	h := objstore.HashOf([]byte("content"))
	key := objstore.KeyAt(time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC))
	good := File{Project: "ENG", Path: "a/b.md", Hash: h, Size: 7, Object: key}
	id := FileSubject("ENG", "a/b.md").ID
	removed := time.Date(2026, 10, 6, 1, 0, 0, 0, time.UTC)
	for name, f := range map[string]File{
		"a put":              good,
		"a removal":          {Project: "ENG", Path: "a/b.md", RemovedAt: &removed},
		"a chunk-era put":    {Project: "ENG", Path: "a/b.md", Hash: h, Size: 7},
		"an empty file":      {Project: "ENG", Path: "a/b.md", Hash: objstore.HashOf(nil), Object: key},
		"a chunk-era, empty": {Project: "ENG", Path: "a/b.md"},
	} {
		if err := fileMatches(applyContext{}, id, f); err != nil {
			t.Errorf("%s was refused: %v", name, err)
		}
	}
	for name, f := range map[string]File{
		"another path":    {Project: "ENG", Path: "a/c.md", Hash: h, Object: key},
		"another project": {Project: "OPS", Path: "a/b.md", Hash: h, Object: key},
		"an unnormalised": {Project: "ENG", Path: "/a/b.md", Hash: h, Object: key},
		"an object with no digest": {Project: "ENG", Path: "a/b.md", Size: 7,
			Object: key},
		"an object with a bad digest": {Project: "ENG", Path: "a/b.md", Hash: "zz",
			Size: 7, Object: key},
		"an object of negative size": {Project: "ENG", Path: "a/b.md", Hash: h,
			Size: -1, Object: key},
	} {
		if err := fileMatches(applyContext{}, id, f); err == nil {
			t.Errorf("%s was applied", name)
		}
	}
}
