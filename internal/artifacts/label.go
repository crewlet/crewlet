package artifacts

import (
	"encoding/base32"
	"errors"
	"fmt"
	"strings"
)

// THE HOST LABEL CODEC: an artifact's id in the DNS label its origin is named
// by (D48). One encoder and one parser, here, so the serving origin, the frame
// builder and the config check that a rendered label fits a DNS label all reach
// one answer about one artifact.

// ErrInvalidLabel reports a host label that does not decode to an artifact.
var ErrInvalidLabel = errors.New("artifacts: not an artifact host label")

// labelEncoding is lowercase Crockford base32 without padding: 128 bits take
// exactly [LabelLen] characters, the last carrying three bits and two zero
// padding bits.
var labelEncoding = base32.NewEncoding(LabelAlphabet).WithPadding(base32.NoPadding)

// Label is the id's host label.
//
// THE ID'S OWN BYTES, never a keyed digest, so a keyring rotation cannot move
// an artifact's origin and wipe every viewer's storage on it.
func (id ID) Label() Label {
	return Label(labelEncoding.EncodeToString(id[:]))
}

// String is the label as text.
func (l Label) String() string { return string(l) }

// ParseLabel reads a host label back to the id it encodes.
//
// CASE-INSENSITIVE, because a DNS label is: a browser or a proxy may hand the
// host back in any case. NOTHING ELSE IS FORGIVEN. A label of another length,
// a character outside the alphabet (including Crockford's I, L, O and U, which
// the alphabet leaves out precisely so nobody types them) or one whose two
// trailing padding bits are not zero is refused rather than mapped, so exactly
// one spelling, up to case, names each artifact.
func ParseLabel(s string) (ID, error) {
	if len(s) != LabelLen {
		return ID{}, fmt.Errorf("%w: %q is %d characters and a label is %d",
			ErrInvalidLabel, s, len(s), LabelLen)
	}
	lower := strings.ToLower(s)
	for i := range len(lower) {
		if !strings.ContainsRune(LabelAlphabet, rune(lower[i])) {
			return ID{}, fmt.Errorf("%w: %q carries %q, which is not in the label "+
				"alphabet %q", ErrInvalidLabel, s, lower[i], LabelAlphabet)
		}
	}
	raw, err := labelEncoding.DecodeString(lower)
	if err != nil {
		return ID{}, fmt.Errorf("%w: %q: %w", ErrInvalidLabel, s, err)
	}
	var id ID
	if len(raw) != len(id) {
		return ID{}, fmt.Errorf("%w: %q decodes to %d bytes, not %d",
			ErrInvalidLabel, s, len(raw), len(id))
	}
	copy(id[:], raw)
	// THE CANONICAL SPELLING: a label whose padding bits are set decodes to
	// the same bytes as the one whose bits are clear, and accepting both
	// would give one artifact two origins.
	if string(id.Label()) != lower {
		return ID{}, fmt.Errorf("%w: %q is not the canonical label for its id "+
			"(its final character carries padding bits)", ErrInvalidLabel, s)
	}
	if id.IsZero() {
		return ID{}, fmt.Errorf("%w: %q encodes the zero id, which names no "+
			"artifact", ErrInvalidLabel, s)
	}
	return id, nil
}
