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

// THE BEARER'S WIRE FORMAT.
//
//	v4.<key tag>.<generation>.<lineage>~<person>.<epoch>.<start position>.<signed in>.<absolute expiry>.<issued>.<idle expiry>[.<scope>].<mac>
//
// Eleven fields, the session's pair joined by `~`, plus a twelfth scope only on
// a restricted session, each one a node must answer with: the KEY TAG; the
// GENERATION (one write ends every session); the LINEAGE (the session's
// identity) and PERSON; the EPOCH, which with the signature proves a sign-in on
// a node that has NOT YET APPLIED its start record; the START POSITION (ended
// versus not yet seen); the SIGN-IN and ABSOLUTE instants, which no re-issue
// moves; the ISSUE and IDLE instants, which every re-issue moves with no store
// write, so a node can enforce SHORTER limits ([Bearer.EndsUnder], ADR-0152).

// # Why the scope is in the bearer, and why it is optional
//
// The bearer is the one copy of the sign-in's scope every node holds: a node
// that has not applied the row serves reads on signature and epoch alone
// ([RowBehind]), so a restriction only the row carried was ignored for the
// apply latency after each password sign-in. ABSENT WHEN WHOLE, for rolling
// upgrades: a whole session is byte for byte the eleven fields every v4 build
// reads, while a build knowing no scope refuses a twelve-field one as MALFORMED
// (failing safe), as it does a scope it cannot NAME. NOTHING HERE IS SECRET:
// it carries nothing about the person (no login, address or grants), because a
// cookie is the value most likely to leak.

// Bearer is one parsed cookie.
//
// A session exchanged from a Tier A token has person `token:<id>`, re-composed
// each request from this node's entry under that id: removing or renaming the
// entry ends it, but a new value under the same id leaves old sessions working
// until their hour ends; cut them off at once by giving the token a new id.
//
// There is no rotation index (v2 had one): a trailing index is an idle session
// and a replay in identical bytes, and one ahead of the clock meant only skewed
// node clocks. A captured cookie is bounded by the idle and absolute
// deadlines, the person's revocation epoch and the fleet generation. A v2 or v3
// bearer is malformed here, never misread.
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

	// SignedInAt is when the sign-in this session continues proved who it
	// was; a replacement (a step-up, a password change) carries it.
	SignedInAt time.Time

	// AbsoluteExpiresAt is never moved by a re-issue; IssuedAt and
	// IdleExpiresAt are moved by every one.
	AbsoluteExpiresAt time.Time
	IssuedAt          time.Time
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
// THE CALLER STATES THE SIGN-IN AND ITS DEADLINE, which a replacement carries
// from the session it replaces; the idle deadline is stamped here, from the
// signer's [Limits].
type Mint struct {
	// Lineage is the session's id: the subject its records arbitrate on.
	Lineage uuid.UUID

	Person        string
	Epoch         uint64
	Generation    uint64
	StartPosition uint64

	// SignedInAt is when the sign-in this session continues happened.
	SignedInAt time.Time

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
	case m.SignedInAt.IsZero():
		return "", errors.New("session: a bearer needs its sign-in instant, " +
			"which a lowered lifetime is measured from")
	}
	now := s.now()
	return s.issue(Bearer{
		Generation:        m.Generation,
		Lineage:           m.Lineage,
		Person:            m.Person,
		Epoch:             m.Epoch,
		StartPosition:     m.StartPosition,
		SignedInAt:        m.SignedInAt,
		AbsoluteExpiresAt: m.AbsoluteExpiresAt,
		EnrolmentOnly:     m.EnrolmentOnly,
	}, now, s.limits().Idle)
}

// issue signs one bearer issued at now, its idle deadline idle later.
//
// IT ALWAYS SIGNS UNDER THE ACTIVE KEY, including when it is re-issuing a
// cookie that arrived under an older one. That is what makes a keyring
// rotation drain: every live session moves to the new key the first time it is
// used, so by the time the old key is dropped the sessions still on it are the
// ones that have been idle longer than the re-issue window.
func (s *Signer) issue(b Bearer, now time.Time, idle time.Duration) (string, error) {
	key, held := s.keys[s.activeTag]
	if !held {
		return "", fmt.Errorf("%w: the active key is not in this signer's "+
			"keyring", ErrNoKeyring)
	}
	b.KeyTag = s.activeTag
	b.IssuedAt, b.IdleExpiresAt = now, now.Add(idle)
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
		strconv.FormatInt(b.SignedInAt.Unix(), 10),
		strconv.FormatInt(b.AbsoluteExpiresAt.Unix(), 10),
		strconv.FormatInt(b.IssuedAt.Unix(), 10),
		strconv.FormatInt(b.IdleExpiresAt.Unix(), 10),
	}
	if b.EnrolmentOnly {
		parts = append(parts, scopeEnrolment)
	}
	return strings.Join(parts, ".")
}

// fields is how many dot-separated parts a WHOLE session's bearer has,
// signature included; each attribute adds one — see the format's doc.
const fields = 11

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
		{parts[6], &b.SignedInAt},
		{parts[7], &b.AbsoluteExpiresAt},
		{parts[8], &b.IssuedAt},
		{parts[9], &b.IdleExpiresAt},
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
