package placement

import (
	"errors"
	"strings"
	"testing"
)

// A LABEL KEY IS ONE GRAMMAR, and it is the one every place a key is written
// checks — so what it accepts and refuses is pinned here rather than in each
// of them.
func TestALabelKeyIsPrintableBoundedAndHasNoWhitespace(t *testing.T) {
	t.Parallel()
	for _, key := range []string{
		"zone", "topology.kubernetes.io/zone", "rack-7", "région",
		strings.Repeat("k", MaxLabelKeyBytes),
	} {
		if err := CheckLabelKey(key); err != nil {
			t.Errorf("CheckLabelKey(%q) = %v, want accepted", key, err)
		}
	}
	for name, key := range map[string]string{
		"empty":                   "",
		"one byte past the bound": strings.Repeat("k", MaxLabelKeyBytes+1),
		"an inner space":          "my zone",
		"a trailing space":        "zone ",
		"a no-break space":        "zone ",
		"a tab":                   "zone\t",
		"a control character":     "zo\x00ne",
		"invalid UTF-8":           "zo\xffne",
	} {
		if err := CheckLabelKey(key); !errors.Is(err, ErrLabelKey) {
			t.Errorf("%s: CheckLabelKey(%q) = %v, want ErrLabelKey", name, key, err)
		}
	}
}
