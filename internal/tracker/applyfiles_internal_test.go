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
// AND A LIVE FILE NAMES ITS OBJECT: only a removal names none. Every put
// this build writes names one, so a live row naming nothing would be a file
// listed with content nobody can read.
func TestTheApplierRefusesAFileThatIsNotItsSubject(t *testing.T) {
	t.Parallel()
	h := objstore.HashOf([]byte("content"))
	key := objstore.KeyAt(time.Date(2026, 10, 6, 0, 0, 0, 0, time.UTC))
	good := File{Project: "ENG", Path: "a/b.md", Hash: h, Size: 7, Object: key}
	id := FileSubject("ENG", "a/b.md").ID
	removed := time.Date(2026, 10, 6, 1, 0, 0, 0, time.UTC)
	for name, f := range map[string]File{
		"a put":         good,
		"a removal":     {Project: "ENG", Path: "a/b.md", RemovedAt: &removed},
		"an empty file": {Project: "ENG", Path: "a/b.md", Hash: objstore.HashOf(nil), Object: key},
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
		"a live file naming no object":   {Project: "ENG", Path: "a/b.md", Hash: h, Size: 7},
		"a live, empty file naming none": {Project: "ENG", Path: "a/b.md"},
	} {
		if err := fileMatches(applyContext{}, id, f); err == nil {
			t.Errorf("%s was applied", name)
		}
	}
}
