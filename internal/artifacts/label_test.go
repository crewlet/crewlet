package artifacts

import (
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// A LABEL IS THE ID'S OWN BYTES, round-trips exactly, and fits a DNS label.
func TestALabelIsTheIDsOwnBytesAndRoundTrips(t *testing.T) {
	t.Parallel()
	for _, op := range []string{"a", "b", "op-label", strings.Repeat("x", 300)} {
		id := IDFor(op)
		label := id.Label()
		if len(label) != LabelLen {
			t.Fatalf("label %q is %d characters, want %d", label, len(label), LabelLen)
		}
		for _, r := range label {
			if !strings.ContainsRune(LabelAlphabet, r) {
				t.Fatalf("label %q carries %q outside the alphabet", label, r)
			}
		}
		got, err := ParseLabel(string(label))
		if err != nil || got != id {
			t.Fatalf("ParseLabel(%q) = %s, %v; want %s", label, got, err, id)
		}
	}
	// THE ENCODING IS PINNED: Crockford's alphabet over the big-endian bits.
	// The all-ones id is the vector that exercises the final character's
	// padding bits.
	ones := ID(uuid.Must(uuid.Parse("ffffffff-ffff-ffff-ffff-ffffffffffff")))
	if got, want := ones.Label(), Label("zzzzzzzzzzzzzzzzzzzzzzzzzw"); got != want {
		t.Fatalf("all-ones label = %q, want %q", got, want)
	}
}

// A HOST COMES BACK IN ANY CASE, and is read in any case.
func TestALabelIsReadCaseInsensitively(t *testing.T) {
	t.Parallel()
	id := IDFor("op-case")
	got, err := ParseLabel(strings.ToUpper(string(id.Label())))
	if err != nil || got != id {
		t.Fatalf("an upper-case label did not read back: %s, %v", got, err)
	}
}

// NOTHING ELSE IS FORGIVEN: another length, a character outside the alphabet,
// set padding bits and the zero id are each refused, so one spelling (up to
// case) names one artifact.
func TestALabelRefusesEverySpellingButItsOwn(t *testing.T) {
	t.Parallel()
	good := string(IDFor("op-refuse").Label())
	padded := good[:LabelLen-1] + "z" // the final character's padding bits set
	if padded == good {
		padded = good[:LabelLen-1] + "y"
	}
	zero := strings.Repeat("0", LabelLen)
	for _, s := range []string{
		"", good[:LabelLen-1], good + "0",
		good[:5] + "i" + good[6:], // I is not in Crockford's alphabet
		good[:5] + "u" + good[6:], // nor U
		good[:5] + "-" + good[6:],
		padded, zero,
	} {
		if _, err := ParseLabel(s); !errors.Is(err, ErrInvalidLabel) {
			t.Errorf("ParseLabel(%q) = %v, want ErrInvalidLabel", s, err)
		}
	}
}
