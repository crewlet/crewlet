package collect_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/collect"
)

// A NAME A VENDOR'S CONVENTIONS WRITE IS A LABEL THE ATLAS TAKES: brackets
// become parentheses, the other markup characters go, and a long name is cut
// to a label's length.
func TestAVendorsNameIsMadeALabel(t *testing.T) {
	t.Parallel()
	for raw, want := range map[string]string{
		"[prod] API 5xx rate > 5%": "(prod) API 5xx rate 5%",
		"  plain   name ":          "plain name",
		"`disk` <full>":            "disk full",
	} {
		if got := collect.PlainLabel(raw); got != want {
			t.Errorf("%q is %q, want %q", raw, got, want)
		}
	}
	long := collect.PlainLabel(strings.Repeat("é", 300))
	if n := len([]rune(long)); n != atlas.MaxLabelRunes {
		t.Errorf("a long name is cut to %d characters, want %d", n, atlas.MaxLabelRunes)
	}
	if _, err := atlas.FactName.Value(long); err != nil {
		t.Errorf("the cut name is not a label: %v", err)
	}
}
