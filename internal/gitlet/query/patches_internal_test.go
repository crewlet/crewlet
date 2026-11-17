package query

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet"
)

// BYTES PAST THE LAST FILE'S PATCH ARE AN ERROR, NOT A PANIC: diff-pairs writing anything
// after the NUL that ends the last file it was fed, in the same write or a later one, stops
// the cutter with an error naming it, and the page cut before stays as it was.
//
// Mutation: drop the guard in Write → the cutter indexes past its files and panics, red.
func TestBytesPastTheLastPatchStopTheCutter(t *testing.T) {
	t.Parallel()
	for _, writes := range [][]string{
		{"diff --git a/a b/a\n\x00extra\n"},
		{"diff --git a/a b/a\n\x00", "extra"},
	} {
		asked := &patchesPage{left: 1 << 16}
		c := &patchesCut{files: []gitlet.FilePatch{{File: 0, Path: "a", Status: "M"}}, room: 1 << 16,
			started: true, asked: asked, cur: asked, headK: -1, stage: func(*patchesPage) {}}
		var err error
		for _, w := range writes {
			if _, err = c.Write([]byte(w)); err != nil {
				break
			}
		}
		if !errors.Is(err, errStop) || c.err == nil || !strings.Contains(c.err.Error(), "past the 1 files") {
			t.Errorf("writes %q: the cutter answered %v with error %v, want it stopped naming the bytes past the "+
				"files", writes, err, c.err)
		}
		if len(asked.patches) != 1 || string(asked.text) != "diff --git a/a b/a\n" {
			t.Errorf("writes %q: the page = %+v %q, want a's patch alone", writes, asked.patches, asked.text)
		}
	}
}
