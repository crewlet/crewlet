package builtin

import (
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/mcp"
)

// EVERY CHANGE TOOL NAMES THE ACCESS IT NEEDS: the one table of what each
// change tool's call needs on the change's repository, of its caller and of
// the colleague a turn reads for, holds a row for every change tool and for
// nothing else — so a change tool added without a row, or a row left behind
// by one removed, fails here rather than serving whoever messaged a seat at
// an access nobody named. And a READ's row is read: the node that answers a
// read judges the colleague it is made for at read and nothing else
// (code.JudgeRead, through the ceiling every question of it carries), so a
// read naming more would be a need no read holds anybody to.
//
// Mutations: a change tool's row dropped; a row naming a tool that is not
// one; a read's row raised to review → red.
func TestEveryChangeToolNamesTheAccessItNeeds(t *testing.T) {
	t.Parallel()
	if len(ChangeToolNames()) == 0 {
		t.Fatal("there are no change tools, so this walk certifies nothing")
	}
	for _, name := range ChangeToolNames() {
		need, ok := changeToolNeeds[name]
		switch {
		case !ok:
			t.Errorf("%s names no access in changeToolNeeds", name)
			continue
		case !need.Valid() || need == code.AccessNone:
			t.Errorf("%s needs %s, which grants nothing a call could be held to", name, need)
		}
		annotations, known := codeAnnotations(name)
		if !known {
			t.Errorf("%s is not classified as a code tool", name)
			continue
		}
		if annotations.ReadOnly == mcp.Yes && need != code.AccessRead {
			t.Errorf("%s is a read and needs %s: a read is judged for whoever it is made for at read, so a "+
				"read's row is read", name, need)
		}
	}
	for name := range changeToolNeeds {
		if !slices.Contains(ChangeToolNames(), name) {
			t.Errorf("changeToolNeeds names %s, which is no change tool", name)
		}
	}
}
