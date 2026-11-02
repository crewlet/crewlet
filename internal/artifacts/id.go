package artifacts

import (
	"errors"
	"fmt"

	"github.com/google/uuid"
)

// THE ID'S DERIVATION AND ITS TEXT FORM, owned by the domain that mints ids
// (internal/artifactsdomain) and written here, in the leaf, because every
// surface that names an artifact parses and prints one: the HTTP routes, the
// tools, the ticket codec, the presence room and the dashboard's contract.
// Two parsers would be two answers to whether a string names an artifact.

// ErrInvalidID reports a string that is not an artifact id in its one text
// form.
var ErrInvalidID = errors.New("artifacts: not an artifact id")

// IDFor is the id the operation opID creates: a NAME-BASED UUID over the
// operation, so the retry an unknown outcome asks for names the artifact its
// first attempt created rather than one nothing will ever find. The rule is
// internal/doclet's pageIDOf rule, for the same reason.
func IDFor(opID string) ID { return ID(uuid.NewSHA1(IDNamespace, []byte(opID))) }

// IsZero reports the zero id, which names no artifact: every derived id is a
// version 5 UUID and none of them is all zeros.
func (id ID) IsZero() bool { return id == ID{} }

// UUID is the id as the uuid it is, for the builders that take one
// (internal/presence's room, and the subject it is told on).
func (id ID) UUID() uuid.UUID { return uuid.UUID(id) }

// String renders the id in its canonical form: lowercase, hyphenated, 36
// characters. The zero id renders as the zero uuid's text, which no parser
// here accepts back as an artifact.
func (id ID) String() string { return uuid.UUID(id).String() }

// ParseID reads an id in its canonical form and no other.
//
// ONE SPELLING, because an id is compared as text in rows, subjects, URLs and
// scope paths: uuid.Parse also takes braces, a `urn:uuid:` prefix, upper case
// and the 32-character form, and a second spelling accepted here would be a
// second row key for one artifact. The zero uuid is refused for the reason
// [ID.IsZero] gives.
func ParseID(s string) (ID, error) {
	if len(s) != 36 {
		return ID{}, fmt.Errorf("%w: %q is %d characters, and an artifact id is the "+
			"36-character lowercase hyphenated uuid", ErrInvalidID, s, len(s))
	}
	parsed, err := uuid.Parse(s)
	if err != nil {
		return ID{}, fmt.Errorf("%w: %q: %w", ErrInvalidID, s, err)
	}
	if parsed.String() != s {
		return ID{}, fmt.Errorf("%w: %q is not in canonical form (lowercase, "+
			"hyphenated); write it as %q", ErrInvalidID, s, parsed.String())
	}
	id := ID(parsed)
	if id.IsZero() {
		return ID{}, fmt.Errorf("%w: the zero uuid names no artifact", ErrInvalidID)
	}
	return id, nil
}

// MarshalText writes the canonical form, and the zero id as EMPTY TEXT.
//
// EMPTY RATHER THAN THE ZERO UUID'S DIGITS, so a value that names no artifact
// (an upload intent for a create, a frame not yet minted) round-trips through
// JSON and through the estate wire as the absence it is, and an `omitempty`
// string field holding one is left out.
func (id ID) MarshalText() ([]byte, error) {
	if id.IsZero() {
		return []byte{}, nil
	}
	return []byte(id.String()), nil
}

// UnmarshalText reads the canonical form, and empty text as the zero id.
func (id *ID) UnmarshalText(text []byte) error {
	if len(text) == 0 {
		*id = ID{}
		return nil
	}
	parsed, err := ParseID(string(text))
	if err != nil {
		return err
	}
	*id = parsed
	return nil
}
