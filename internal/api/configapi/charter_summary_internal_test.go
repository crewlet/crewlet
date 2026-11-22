package configapi

import (
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
)

// THE SUMMARY A CHARTER EDIT WRITES FITS THE ACTIVATION POINTER: it names
// which parts changed, never their words, so at its longest (every part) it is
// a few dozen bytes whatever the charter holds.
//
// Mutation: put the new mission in the summary, and a long one goes past the
// bound.
func TestTheSummaryACharterEditWritesFitsThePointer(t *testing.T) {
	t.Parallel()
	long := string(make([]byte, 1024))
	was := config.Charter{}
	is := config.Charter{Mission: long, Vision: long + long, Policies: []string{long}}
	if err := coord.HoldSummary(charterSummary(was, is)); err != nil {
		t.Errorf("a charter edit of every part: %v", err)
	}
}
