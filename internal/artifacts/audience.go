package artifacts

import (
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/crewlet/crewlet/internal/iam"
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

// Validate refuses an audience that cannot be addressed at all: a kind this
// build does not know (a later build's `link` included), a seat or a unit with
// no id, an id that could not round-trip through the text form or a URL
// segment, or a company audience carrying an id.
//
// IT IS WHAT A STORED SHARE IS HELD TO, and so what a withdrawal accepts. A
// NEW grant is held to [Audience.ValidateGrant] as well: a share written
// before that rule existed (a login named as a seat) is the owner's to
// withdraw or change, never a record the engine can neither honour nor remove.
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
	return notAnAudience(string(a.Kind))
}

// notAnAudience is what a value naming no kind of audience is refused with,
// whether it came as a kind ([Audience.Validate]) or as an audience's text
// ([ParseAudience]): what it is not, and the forms an audience is written in.
// ONE SENTENCE for both, because a person who typed `team:eng` and one whose
// request carried the kind `team` made the same mistake, and were told it in
// two different sentences, each in a program's words ("kind … is not one this
// build shares with (want one of …)" and "… is not `seat:<handle>`, …").
func notAnAudience(what string) error {
	return fmt.Errorf("%w: %q isn't someone you can share with. Use `seat:<handle>`, "+
		"`unit:<key>` or `company`", ErrInvalidAudience, what)
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

// ValidateGrant refuses an audience a NEW share may not name: everything
// [Audience.Validate] refuses, and a seat audience that is not a seat handle.
//
// A SEAT AUDIENCE NAMES A SEAT, in the chart's own grammar, and NEVER A LOGIN:
// the kinds are a closed set of company principals ([AudienceKind]), and the
// authority table matches a seat share against the principal's SEAT alone. A
// person's dotted login and a machine's or a Tier A token's coloned one are
// disjoint from a handle by construction ([iam.ValidSeatHandle]), so a share to
// one was written, counted by the listing and then admitted by nothing:
// `seat:token:maya` reached nobody while saying it reached Maya.
func (a Audience) ValidateGrant() error {
	if err := a.Validate(); err != nil {
		return err
	}
	if !a.Reaches() {
		return fmt.Errorf("%w: %q is not a seat handle: a share names the seat a "+
			"person or an agent holds, and a login (a person's dotted name, a service "+
			"account's or an API token's coloned one) is not a seat, so share with the "+
			"seat it acts as", ErrInvalidAudience, a.ID)
	}
	return nil
}

// Reaches reports whether a well-formed audience can reach anybody: false for
// a seat audience naming a login, which a share written before
// [Audience.ValidateGrant] may hold and which only a withdrawal is for.
func (a Audience) Reaches() bool {
	return a.Kind != AudienceSeat || iam.ValidSeatHandle(a.ID)
}

// ParseAudience reads an audience's text form back.
//
// THE KIND IS CUT AT THE FIRST SEPARATOR, so a unit key that itself contains
// one still reads back whole (a seat handle never does); an audience the text form could
// not have produced is refused rather than guessed at.
func ParseAudience(s string) (Audience, error) {
	if s == string(AudienceCompany) {
		return Audience{Kind: AudienceCompany}, nil
	}
	kind, id, ok := strings.Cut(s, audienceSeparator)
	if !ok {
		return Audience{}, notAnAudience(s)
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
