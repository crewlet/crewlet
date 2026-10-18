package artifacts

import (
	"errors"
	"fmt"
	"strings"
	"unicode"
)

// AN AUDIENCE'S TEXT FORM: `seat:<handle>`, `unit:<key>` or `company`. It is
// what a share is addressed by in a URL (`DELETE /artifacts/{id}/shares/{audience}`),
// in a tool argument and in a log line, so it is written once, here.

// ErrInvalidAudience reports an audience that names no principal set this build
// shares with.
var ErrInvalidAudience = errors.New("artifacts: not an audience")

// MaxAudienceIDBytes bounds a seat handle or a unit key an audience names: far
// past any handle the chart accepts, and short enough that a share row and its
// URL segment stay a bounded size.
const MaxAudienceIDBytes = 128

// audienceSeparator joins an audience's kind to its id.
const audienceSeparator = ":"

// String renders the audience in its one text form.
func (a Audience) String() string {
	if a.Kind == AudienceCompany {
		return string(AudienceCompany)
	}
	return string(a.Kind) + audienceSeparator + a.ID
}

// Validate refuses an audience that cannot name a principal set: a kind this
// build does not know (a later build's `link` included), a seat or a unit with
// no id, an id that could not round-trip through the text form or a URL
// segment, or a company audience carrying an id.
func (a Audience) Validate() error {
	switch a.Kind {
	case AudienceCompany:
		if a.ID != "" {
			return fmt.Errorf("%w: the company audience names everybody and carries "+
				"no id, and this one carries %q", ErrInvalidAudience, a.ID)
		}
		return nil
	case AudienceSeat, AudienceUnit:
		return validAudienceID(a.Kind, a.ID)
	}
	return fmt.Errorf("%w: kind %q is not one this build shares with (want one of "+
		"%v)", ErrInvalidAudience, a.Kind, AudienceKinds)
}

// validAudienceID refuses an id that is empty, too long, or carries a
// character the text form or a URL path segment would read differently.
func validAudienceID(kind AudienceKind, id string) error {
	switch {
	case id == "":
		return fmt.Errorf("%w: a %s audience names its %s", ErrInvalidAudience, kind,
			map[AudienceKind]string{AudienceSeat: "seat handle", AudienceUnit: "unit key"}[kind])
	case len(id) > MaxAudienceIDBytes:
		return fmt.Errorf("%w: the %s id is %d bytes and the maximum is %d",
			ErrInvalidAudience, kind, len(id), MaxAudienceIDBytes)
	}
	for _, r := range id {
		if unicode.IsSpace(r) || unicode.IsControl(r) || r == '/' || r == '?' || r == '#' {
			return fmt.Errorf("%w: the %s id %q carries whitespace, a control "+
				"character or a URL delimiter", ErrInvalidAudience, kind, id)
		}
	}
	return nil
}

// ParseAudience reads an audience's text form back.
//
// THE KIND IS CUT AT THE FIRST SEPARATOR, so a seat handle or unit key that
// itself contains one still reads back whole; an audience the text form could
// not have produced is refused rather than guessed at.
func ParseAudience(s string) (Audience, error) {
	if s == string(AudienceCompany) {
		return Audience{Kind: AudienceCompany}, nil
	}
	kind, id, ok := strings.Cut(s, audienceSeparator)
	if !ok {
		return Audience{}, fmt.Errorf("%w: %q is not `seat:<handle>`, "+
			"`unit:<key>` or `company`", ErrInvalidAudience, s)
	}
	a := Audience{Kind: AudienceKind(kind), ID: id}
	if a.Kind == AudienceCompany {
		return Audience{}, fmt.Errorf("%w: %q: the company audience is written "+
			"`company`, with no id", ErrInvalidAudience, s)
	}
	if err := a.Validate(); err != nil {
		return Audience{}, err
	}
	return a, nil
}
