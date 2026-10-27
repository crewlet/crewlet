package artifactupload_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/artifactupload"
)

// A PUBLISH HOLDS ONLY THE INDEX IT WRITES TO THE EXACT KEYS (ADR-0059): an
// index the publish brings new bytes for is checked strictly, and one it
// carries from the version before is read as it was stored, so changing one
// slide of a deck accepted under the older rule is not refused for keys it
// never touched. The format packages' stored-read tests hold the carried read
// to what encoding/json accepted.
//
// Mutation: make brings answer true for a carried file and a patch over a
// legacy deck is refused for its index.
func TestOnlyAnIndexThePublishWritesIsHeldToTheExactKeys(t *testing.T) {
	t.Parallel()
	files := map[string]bool{"deck.json": false, "slides/intro.html": true}
	if artifactupload.Brings(files, "deck.json") {
		t.Error("a carried index is checked as though this publish wrote it")
	}
	if !artifactupload.Brings(files, "slides/intro.html") {
		t.Error("a file this publish writes is not one it brings")
	}
	files["deck.json"] = true
	if !artifactupload.Brings(files, "deck.json") {
		t.Error("an index this publish writes is read as stored")
	}
	if artifactupload.Brings(files, "absent.json") {
		t.Error("a file the version does not hold is one the publish brings")
	}
}
