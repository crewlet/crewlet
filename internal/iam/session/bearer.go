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
//	v3.<key tag>.<generation>.<lineage>~<person>.<epoch>.<start position>.<absolute expiry>.<idle expiry>[.<scope>].<mac>
//
// NINE FIELDS, dot-separated, with the session's own pair joined by `~` so
// that the two values a node needs before it reads anything travel as one
// token — and one more, present only where it says something: the scope, on a
// session that may do less than everything. Every one of them is here because
// a node has to answer with it and has no other way to know it:
//
//   - the KEY TAG, so a verifier knows which keyring entry to look up rather
//     than trying each one — which is what makes adding a key zero-downtime;
//   - the GENERATION, the fleet-wide counter a restore's last step moves, so
//     one write ends every session in the company;
//   - the LINEAGE, which is the session's identity and the subject its
//     records arbitrate on;
//   - the PERSON, so a node can read their row without first reading the
//     session's;
//   - the EPOCH the session was opened at, which is what lets a node that has
//     NOT YET APPLIED the session's start record still serve reads: the
//     signature and the epoch together are proof the sign-in happened;
//   - the START POSITION, which is what turns "no row" into the two answers it
//     actually is — a session that ended, or one this node has not seen;
//   - the ABSOLUTE expiry, which no re-issue ever moves;
//   - the IDLE expiry, which every re-issue does, with no store write at all;
//   - the SCOPE, present only on a session that may do less than everything:
//     `enrol`, one its sign-in opened on a password alone where a second
//     factor is required ([Bearer.EnrolmentOnly]).
//
// # A session exchanged from a Tier A token carries nothing of the token
//
// Its person is the token's LOGIN (`token:<id>`), and the request guard
// re-composes it from the entry this node holds under that id on every
// request, so removing or renaming the entry ends it on the next one. What it
// does NOT answer to is the entry's VALUE: a new value put under the same id
// leaves the sessions the old one opened working until their one-hour lifetime
// ends, and an operator who has to cut them off at once rotates by giving the
// token a new id. A MAC over the value once rode here for that, and went: a
// second credential check on every request, a key derivation and an ending of
// its own, to shorten an hour an id change already shortens to nothing.
//
// # There is no rotation index, and v2 had one
//
// v2 carried the session's age in rotation windows beside the lineage, derived
// from the instant inside a uuid7 lineage so that "rotating" a cookie wrote
// nothing. What an index can PROVE is the whole question, and the answer is
// nothing a replay leaves behind. A cookie whose index trails the clock is an
// idle session and a captured one in identical bytes — the only thing that
// could tell them apart is a record of which index was last issued, which is
// exactly what deriving the index removed — so that arm was served and
// re-issued. The one arm that still acted, an index AHEAD of the clock past a
// two-minute overlap, could not be a replay either: the index is inside the
// signed payload, so only a node holding the keyring wrote it, and it wrote it
// from its own clock. That arm fired on exactly one thing — two nodes whose
// clocks disagreed by more than the overlap — and its answer was to bump the
// person's revocation epoch, which signed them out everywhere and ended every
// machine token they held, over an NTP fault on somebody else's host.
//
// With neither arm deciding anything, the index did one thing: it forced a
// re-issue at every window boundary, which [ReissueAfter] already does five
// minutes into any window. So it is gone, and with it `session.rotate_after`
// and the reuse revocation. What bounds a captured cookie is what always did:
// the idle deadline (twelve hours from the last use), the absolute deadline
// (configured), the person's revocation epoch and the fleet's generation (one
// write each, immediate). The version moved because the session pair is a
// field v2 read as a triple — a v2 bearer is malformed here, and a v3 one is
// malformed to a v2 build, rather than either being misread.
//
// # Why the scope is in the bearer, and why it is optional
//
// IT IS THE SIGN-IN'S OWN FACT, decided once when the session opened, and the
// bearer is the one copy of the session every node holds. The session's row
// carries it too, but a node that has not applied the row yet serves reads on
// the signature and the epoch alone ([RowBehind]) — and every sign-in answers
// before any node applies its row. A restriction only the row carried was one
// every node ignored for the apply latency after every password sign-in, the
// one that answered it included: a password alone read the whole company, the
// socket's snapshot among it, on a deployment that requires a second factor.
//
// ABSENT WHEN THE SESSION IS WHOLE, and that is the rolling-upgrade decision
// rather than tidiness. A whole session's bearer is byte for byte the nine
// fields every build reads, so a fleet part-way through an upgrade keeps every
// ordinary session on every node. A scoped one is ten, which a build that
// knows no scope refuses as MALFORMED — the direction that fails safe, because
// such a build would otherwise serve it whole. What that costs is a person
// part-way through enrolling asked to sign in again on that node, for a
// session that could do nothing but enrol. And a scope this build cannot NAME
// is refused the same way, for the same reason: a newer build narrowing a
// session in a way this one would serve whole.
//
// NOTHING HERE IS SECRET and nothing here grants anything on its own: a bearer
// in a proxy log discloses a lineage, a person id, two deadlines and whether
// the session may only enrol, and is worthless without the mac. What it deliberately does NOT carry is anything
// about the person — no login, no address, no grants — because a cookie is the
// value most likely to end up somewhere nobody meant it to.

// Bearer is one parsed cookie.
type Bearer struct {
	// KeyTag names the keyring entry this bearer was signed under.
	KeyTag string

	// Generation is the fleet-wide session generation at mint.
	Generation uint64

	// Lineage is the session's own id.
	Lineage uuid.UUID

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

	// EnrolmentOnly marks a session that may do nothing but enrol a second
	// factor: the bearer's SCOPE, signed with everything else and carried
	// through every re-issue, so every node reads it whether or not it has
	// applied the session's row. See the format's doc above, and
	// [Validation.EnrolmentOnly] for the one reading of it.
	EnrolmentOnly bool
}

// scopeEnrolment is the scope a bearer of a session that may only enrol a
// second factor carries — see [Bearer.EnrolmentOnly].
const scopeEnrolment = "enrol"

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
	// Lineage is the session's id: the subject its records arbitrate on.
	Lineage uuid.UUID

	Person        string
	Epoch         uint64
	Generation    uint64
	StartPosition uint64

	// AbsoluteExpiresAt is the deadline no re-issue moves.
	AbsoluteExpiresAt time.Time

	// EnrolmentOnly scopes the bearer to enrolling a second factor, for a
	// session its sign-in opened on a password alone where one is
	// required. THE SAME DECISION the session's start record carries, made
	// once by the sign-in — see [Bearer.EnrolmentOnly] for why the bearer
	// carries it too.
	EnrolmentOnly bool
}

// Mint issues a bearer for a session that has just started.
func (s *Signer) Mint(m Mint) (string, error) {
	switch {
	case m.Lineage == uuid.Nil:
		return "", errors.New("session: a bearer needs its session's lineage")
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
		EnrolmentOnly:     m.EnrolmentOnly,
	}, now)
}

// issue signs one bearer, stamping the idle deadline.
//
// IT ALWAYS SIGNS UNDER THE ACTIVE KEY, including when it is re-issuing a
// cookie that arrived under an older one. That is what makes a keyring
// rotation drain: every live session moves to the new key the first time it is
// used, so by the time the old key is dropped the sessions still on it are the
// ones that have been idle longer than the re-issue window.
func (s *Signer) issue(b Bearer, now time.Time) (string, error) {
	key, held := s.keys[s.activeTag]
	if !held {
		return "", fmt.Errorf("%w: the active key is not in this signer's "+
			"keyring", ErrNoKeyring)
	}
	b.KeyTag = s.activeTag
	b.IdleExpiresAt = now.Add(Idle)
	payload := b.payload()
	return payload + "." + sign(key, payload), nil
}

// payload is everything the signature covers, which is the whole bearer bar
// the signature itself — the scope included, so a bearer cannot be widened by
// cutting it off.
func (b Bearer) payload() string {
	parts := []string{
		Version,
		b.KeyTag,
		strconv.FormatUint(b.Generation, 10),
		b.Lineage.String() + "~" + b.Person,
		strconv.FormatUint(b.Epoch, 10),
		strconv.FormatUint(b.StartPosition, 10),
		strconv.FormatInt(b.AbsoluteExpiresAt.Unix(), 10),
		strconv.FormatInt(b.IdleExpiresAt.Unix(), 10),
	}
	if b.EnrolmentOnly {
		parts = append(parts, scopeEnrolment)
	}
	return strings.Join(parts, ".")
}

// fields is how many dot-separated parts a WHOLE session's bearer has,
// signature included; each attribute adds one — see the format's doc.
const fields = 9

// attributes is how many a bearer may carry: a scope.
const attributes = 1

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
	if len(parts) < fields || len(parts) > fields+attributes || parts[0] != Version {
		return Bearer{}, fmt.Errorf("%w: %d fields at version %q",
			ErrMalformed, len(parts), firstField(parts))
	}
	key, held := s.keys[parts[1]]
	if !held {
		return Bearer{}, fmt.Errorf("%w: signed under a key this node does "+
			"not hold", ErrMalformed)
	}
	mac := parts[len(parts)-1]
	payload := strings.Join(parts[:len(parts)-1], ".")
	// CONSTANT TIME. A byte-by-byte compare leaks the signature one byte
	// at a time to anyone who can time the endpoint, and a sign-in
	// endpoint is deliberately reachable with no other credential.
	if !hmac.Equal([]byte(mac), []byte(sign(key, payload))) {
		return Bearer{}, fmt.Errorf("%w: the signature does not verify",
			ErrMalformed)
	}

	var b Bearer
	b.KeyTag = parts[1]
	if err := b.readAttributes(parts[fields-1 : len(parts)-1]); err != nil {
		return Bearer{}, err
	}
	pair := strings.Split(parts[3], "~")
	if len(pair) != 2 {
		return Bearer{}, fmt.Errorf("%w: the session pair has %d parts",
			ErrMalformed, len(pair))
	}
	lineage, err := uuid.Parse(pair[0])
	if err != nil {
		return Bearer{}, fmt.Errorf("%w: the lineage is not a uuid", ErrMalformed)
	}
	b.Lineage = lineage
	b.Person = pair[1]
	if b.Person == "" {
		return Bearer{}, fmt.Errorf("%w: the bearer names no person", ErrMalformed)
	}
	for _, field := range []struct {
		raw  string
		into *uint64
	}{
		{parts[2], &b.Generation},
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

// readAttributes reads what a bearer carries past its fixed fields: a scope,
// at most once — what [Bearer.payload] writes, so a bearer this engine minted
// always reads.
//
// AN ATTRIBUTE THIS BUILD CANNOT NAME IS REFUSED, never served whole: it is a
// newer build saying this session may do less, and reading it as nothing would
// be reading it as everything.
func (b *Bearer) readAttributes(attrs []string) error {
	for i, attr := range attrs {
		switch {
		case attr == scopeEnrolment && i == 0:
			b.EnrolmentOnly = true
		default:
			return fmt.Errorf("%w: attribute %q is not one this build knows "+
				"in that place", ErrMalformed, attr)
		}
	}
	return nil
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
