package setuptest_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/setup/setuptest"
	"github.com/crewlet/crewlet/internal/vendortext"
)

// A HINT'S WORD CAP COUNTS WHAT A PERSON READS: a menu path's levels, never
// the chevron between them, and no punctuation run as a word of its own.
func TestTextWordsCountsWhatAPersonReads(t *testing.T) {
	t.Parallel()
	text := vendortext.Of(vendortext.W("From "),
		vendortext.UI("Business Settings", "System users").At("https://business.facebook.com/settings/system-users"),
		vendortext.W(", with "), vendortext.UI("ads_read"), vendortext.W("."))
	if n := setuptest.TextWords(text); n != 7 {
		t.Errorf("TextWords(%q) = %d, want 7", text, n)
	}
}
