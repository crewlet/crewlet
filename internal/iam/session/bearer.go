package session

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// THE BEARER'S WIRE FORMAT, and what each field is doing there.
//
//	v2.<key tag>.<generation>.<lineage>~<rotation>~<person>.<epoch>.<start position>.<absolute expiry>.<idle expiry>.<mac>
//
// NINE FIELDS, dot-separated, with the session's own triple joined by `~` so
// that the three values a node needs before it reads anything travel as one
// token. Every one of them is here because a node has to answer with it and
// has no other way to know it:
//
//   - the KEY TAG, so a verifier knows which keyring entry to look up rather
//     than trying each one — which is what makes adding a key zero-downtime;
//   - the GENERATION, the fleet-wide counter a restore's last step moves, so
//     one write ends every session in the company;
//   - the LINEAGE, which is the session's identity, the subject its records
//     arbitrate on, AND — being a uuid7 — the instant it began, which is what
//     the rotation index is derived from;
//   - the ROTATION index, which is the window this cookie was issued in — an
//     integer, for the reason rotate.go gives at length;
//   - the PERSON, so a node can read their row without first reading the
//     session's;
//   - the EPOCH the session was opened at, which is what lets a node that has
//     NOT YET APPLIED the session's start record still serve reads: the
//     signature and the epoch together are proof the sign-in happened;
//   - the START POSITION, which is what turns "no row" into the two answers it
//     actually is — a session that ended, or one this node has not seen;
//   - the ABSOLUTE expiry, which no re-issue ever moves;
//   - the IDLE expiry, which every re-issue does, with no store write at all.
//
// NOTHING HERE IS SECRET and nothing here grants anything on its own: a bearer
// in a proxy log discloses a lineage, a person id and two deadlines, and is
// worthless without the mac. What it deliberately does NOT carry is anything
// about the person — no login, no address, no grants — because a cookie is the
// value most likely to end up somewhere nobody meant it to.

// Bearer is one parsed cookie.
type Bearer struct {
	// KeyTag names the keyring entry this bearer was signed under.
	KeyTag string

	// Generation is the fleet-wide session generation at mint.
	Generation uint64

	// Lineage is the session's own uuid7. Its embedded instant is the
	// session's start, which [Signer.rotationAt] derives the index from.
	Lineage uuid.UUID

	// Rotation is the window index this cookie was issued in.
	Rotation uint64

	// Person is whose session it is.
	Person string

	// Epoch is the person's revocation epoch at sign-in.
	Epoch uint64

	// StartPosition is the packed iam-log position the session's start
	// record landed at.
	StartPosition uint64

	// AbsoluteExpiresAt is never moved by a re-issue; IdleExpiresAt is
	// moved by every one.
	AbsoluteExpiresAt time.Time
	IdleExpiresAt     time.Time
}

// ErrMalformed reports a cookie that is not a bearer of this format at all.
//
// DISTINCT FROM A REFUSAL, because the two are different events: a malformed
// value is a client sending rubbish, a stale format, or a cookie from another
// application on the same host, and none of those is a session that was
// revoked. Collapsing them would make an operator reading the log unable to
// tell an attack from a leftover cookie.
var ErrMalformed = errors.New("session: not a bearer of this format")

// Mint is what issuing a bearer needs.
//
// THE CALLER STATES THE DEADLINES rather than this package deriving them: the
// absolute lifetime is configured and belongs to whoever read the config, and
// a signer that reached for it would be a second reader of a field that is
// allowed to change under it. The IDLE deadline is the exception and is
// derived here, because [Idle] is a constant precisely so that every node
// agrees on it.
type Mint struct {
	// Lineage is the session's uuid7. It MUST be one: the rotation index
	// is derived from the instant inside it, so a lineage minted any other
	// way is a session whose age nothing can compute.
	Lineage uuid.UUID

	Person        string
	Epoch         uint64
	Generation    uint64
	StartPosition uint64

	// AbsoluteExpiresAt is the deadline no re-issue moves.
	AbsoluteExpiresAt time.Time
}

// Mint issues a bearer for a session that has just started.
func (s *Signer) Mint(m Mint) (string, error) {
	switch {
	case m.Lineage == uuid.Nil:
		return "", errors.New("session: a bearer needs its session's lineage")
	case m.Lineage.Version() != 7:
		return "", fmt.Errorf("session: lineage %s is a version-%d uuid and "+
			"the rotation index is derived from the instant inside a version-7 "+
			"one — a session whose start nothing can read is one whose age "+
			"nothing can compute", m.Lineage, m.Lineage.Version())
	case m.Person == "":
		return "", errors.New("session: a bearer needs the person it is for — " +
			"a node reads their row before it reads the session's")
	case m.AbsoluteExpiresAt.IsZero():
		return "", errors.New("session: a bearer needs an absolute deadline; " +
			"a zero one read as 'never' is a session nobody bounded")
	}
	now := s.now()
	return s.issue(Bearer{
		Generation:        m.Generation,
		Lineage:           m.Lineage,
		Person:            m.Person,
		Epoch:             m.Epoch,
		StartPosition:     m.StartPosition,
		AbsoluteExpiresAt: m.AbsoluteExpiresAt,
	}, s.rotationAt(m.Lineage, now), now)
}

// issue signs one bearer at a rotation index, stamping the idle deadline.
//
// IT ALWAYS SIGNS UNDER THE ACTIVE KEY, including when it is re-issuing a
// cookie that arrived under an older one. That is what makes a keyring
// rotation drain: every live session moves to the new key the first time it is
// used, so by the time the old key is dropped the sessions still on it are the
// ones that have been idle longer than the re-issue window.
func (s *Signer) issue(b Bearer, rotation uint64, now time.Time) (string, error) {
	key, held := s.keys[s.activeTag]
	if !held {
		return "", fmt.Errorf("%w: the active key is not in this signer's "+
			"keyring", ErrNoKeyring)
	}
	b.KeyTag = s.activeTag
	b.Rotation = rotation
	b.IdleExpiresAt = now.Add(Idle)
	payload := b.payload()
	return payload + "." + sign(key, payload), nil
}

// payload is everything the signature covers, which is the whole bearer bar
// the signature itself.
func (b Bearer) payload() string {
	return strings.Join([]string{
		Version,
		b.KeyTag,
		strconv.FormatUint(b.Generation, 10),
		b.Lineage.String() + "~" + strconv.FormatUint(b.Rotation, 10) +
			"~" + b.Person,
		strconv.FormatUint(b.Epoch, 10),
		strconv.FormatUint(b.StartPosition, 10),
		strconv.FormatInt(b.AbsoluteExpiresAt.Unix(), 10),
		strconv.FormatInt(b.IdleExpiresAt.Unix(), 10),
	}, ".")
}

// fields is how many dot-separated parts a bearer has, signature included.
const fields = 9

// parse reads a cookie's structure and verifies its signature.
//
// THE SIGNATURE IS CHECKED BEFORE ANY FIELD IS BELIEVED, which is the ordinary
// rule and worth stating because the fields here are tempting to act on early:
// the person id and the key tag both look like things to look up. Nothing is
// looked up until the mac has verified, so an unauthenticated caller cannot
// make this node read a row of their choosing.
//
// AN UNKNOWN KEY TAG IS REFUSED, never retried against the active key. A
// bearer signed under a key an operator has dropped is exactly as invalid as a
// forged one — that is what dropping a key MEANS — and a fallback would make
// the drop do nothing at all.
func (s *Signer) parse(cookie string) (Bearer, error) {
	parts := strings.Split(cookie, ".")
	if len(parts) != fields || parts[0] != Version {
		return Bearer{}, fmt.Errorf("%w: %d fields at version %q",
			ErrMalformed, len(parts), firstField(parts))
	}
	key, held := s.keys[parts[1]]
	if !held {
		return Bearer{}, fmt.Errorf("%w: signed under a key this node does "+
			"not hold", ErrMalformed)
	}
	payload := strings.Join(parts[:fields-1], ".")
	// CONSTANT TIME. A byte-by-byte compare leaks the signature one byte
	// at a time to anyone who can time the endpoint, and a sign-in
	// endpoint is deliberately reachable with no other credential.
	if !hmac.Equal([]byte(parts[fields-1]), []byte(sign(key, payload))) {
		return Bearer{}, fmt.Errorf("%w: the signature does not verify",
			ErrMalformed)
	}

	var b Bearer
	b.KeyTag = parts[1]
	triple := strings.Split(parts[3], "~")
	if len(triple) != 3 {
		return Bearer{}, fmt.Errorf("%w: the session triple has %d parts",
			ErrMalformed, len(triple))
	}
	lineage, err := uuid.Parse(triple[0])
	if err != nil {
		return Bearer{}, fmt.Errorf("%w: the lineage is not a uuid", ErrMalformed)
	}
	if lineage.Version() != 7 {
		return Bearer{}, fmt.Errorf("%w: the lineage is a version-%d uuid, so "+
			"the session's start is unreadable", ErrMalformed, lineage.Version())
	}
	b.Lineage = lineage
	b.Person = triple[2]
	if b.Person == "" {
		return Bearer{}, fmt.Errorf("%w: the bearer names no person", ErrMalformed)
	}
	for _, field := range []struct {
		raw  string
		into *uint64
	}{
		{parts[2], &b.Generation},
		{triple[1], &b.Rotation},
		{parts[4], &b.Epoch},
		{parts[5], &b.StartPosition},
	} {
		value, err := strconv.ParseUint(field.raw, 10, 64)
		if err != nil {
			return Bearer{}, fmt.Errorf("%w: %q is not a number", ErrMalformed,
				field.raw)
		}
		*field.into = value
	}
	for _, field := range []struct {
		raw  string
		into *time.Time
	}{
		{parts[6], &b.AbsoluteExpiresAt},
		{parts[7], &b.IdleExpiresAt},
	} {
		seconds, err := strconv.ParseInt(field.raw, 10, 64)
		if err != nil {
			return Bearer{}, fmt.Errorf("%w: %q is not an instant",
				ErrMalformed, field.raw)
		}
		*field.into = time.Unix(seconds, 0).UTC()
	}
	return b, nil
}

// firstField is the version a malformed value claimed, for the message, with
// no assumption that there is one.
func firstField(parts []string) string {
	if len(parts) == 0 {
		return ""
	}
	return parts[0]
}

// sign is the mac over a payload.
func sign(key []byte, payload string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	// URL-SAFE AND UNPADDED, because this is a cookie VALUE: `=` and `,`
	// and `;` are the characters a cookie may not carry unquoted, and the
	// standard alphabet's `/` and `+` are legal but re-encoded by enough
	// proxies to matter.
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
