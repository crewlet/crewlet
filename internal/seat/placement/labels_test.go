package placement

import (
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/iam"
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

// A LABEL VALUE IS ONE GRAMMAR TOO, matched exactly between a node and a
// selector, and bounded because every node re-sends its labels on each
// heartbeat: what it accepts and refuses is pinned here rather than in each
// place a value is written.
func TestALabelValueIsPrintableBoundedAndHasNoWhitespaceAroundIt(t *testing.T) {
	t.Parallel()
	for _, value := range []string{
		"", "eu", "true", "eu west", "région", `say "hi"`,
		strings.Repeat("v", MaxLabelValueBytes),
	} {
		if err := CheckLabelValue(value); err != nil {
			t.Errorf("CheckLabelValue(%q) = %v, want accepted", value, err)
		}
	}
	for name, value := range map[string]string{
		"one byte past the bound":     strings.Repeat("v", MaxLabelValueBytes+1),
		"a leading space":             " eu",
		"a trailing space":            "eu ",
		"an inner tab":                "eu\twest",
		"an inner no-break space":     "eu\u00a0west",
		"a line separator":            "eu\u2028west",
		"a control character":         "e\x00u",
		"invalid UTF-8":               "e\xffu",
		"a multi-byte value too long": strings.Repeat("é", MaxLabelValueBytes/2+1),
	} {
		if err := CheckLabelValue(value); !errors.Is(err, ErrLabelValue) {
			t.Errorf("%s: CheckLabelValue(%q) = %v, want ErrLabelValue", name, value, err)
		}
	}
}

// A LABEL VALUE HOLDS A SEAT HANDLE. crewlet-operator writes one as a per-seat
// pool's `crewlet.ai/seat` label, so a value bound under a handle's own width
// would refuse a node the operator rendered for a seat with a long handle.
func TestALabelValueHoldsASeatHandle(t *testing.T) {
	t.Parallel()
	if MaxLabelValueBytes < iam.MaxLogin {
		t.Fatalf("a label value holds %d bytes, under a seat handle's %d", MaxLabelValueBytes,
			iam.MaxLogin)
	}
	handle := strings.Repeat("h", iam.MaxLogin)
	if !iam.ValidSeatHandle(handle) {
		t.Fatalf("the control: a handle of %d bytes is not a seat handle", iam.MaxLogin)
	}
	if err := CheckLabelValue(handle); err != nil {
		t.Fatalf("a seat handle at its widest is not a label value: %v", err)
	}
}
