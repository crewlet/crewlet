package ticket

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/runtoken"
)

// Domain separates view tickets from every other runtoken the fleet mints: a
// ticket is an HMAC over the same keyring as the telemetry and tool bridge
// tokens, and the domain is what keeps one from validating as another.
const Domain = "crewlet.artifact.view.v1"

// Verdict is what opening a ticket concluded.
type Verdict string

const (
	// Valid is a ticket this fleet minted whose claims have not expired.
	Valid Verdict = "valid"

	// Expired is a ticket this fleet minted whose claims ran out less than
	// [artifacts.TicketExpiredGrace] ago: the host mints another.
	Expired Verdict = "expired"

	// Invalid is everything else: forged, truncated, signed under a key
	// this node does not hold, malformed claims, or expired past the
	// grace. Nothing about which is ever told to the holder.
	Invalid Verdict = "invalid"
)

// Verdicts are every verdict, in declaration order.
var Verdicts = []Verdict{Valid, Expired, Invalid}

// Valid reports whether v is a verdict this package answers.
func (v Verdict) Valid() bool { return slices.Contains(Verdicts, v) }

// ErrInvalidClaims reports claims a ticket may not carry, naming the field.
var ErrInvalidClaims = errors.New("ticket: invalid claims")

// The bounds a claim's free-text fields are held to. A ticket rides in a URL
// path, and a path past a few kilobytes is one some proxy truncates; these keep
// the whole token well inside that, and refuse a value no mint of this build
// produces.
const (
	// maxPrincipal is a record owner's width: a seat handle or a machine
	// login, both held to iam.MaxLogin, with room to spare.
	maxPrincipal = 128

	// maxLineage is a credential's name: a class and a uuid, or a Tier A
	// token's login.
	maxLineage = 160

	// maxNonce bounds the bridge nonce; a mint writes 22 characters.
	maxNonce = 64

	// maxToken bounds what Open will even try to read, before any
	// signature is checked: an attacker's megabyte is refused by its
	// length rather than base64-decoded.
	maxToken = 4096
)

// Codec mints and opens tickets.
//
// SAFE FOR CONCURRENT USE and holding no mutable state: a ticket is a function
// of the keyring, the claims and the clock.
type Codec struct {
	signer *runtoken.Signer
	now    func() time.Time
}

// New builds a codec over the fleet's keyring, or refuses one that cannot sign
// for the fleet (runtoken.ErrNoKeyring). now is the clock; nil takes UTC wall
// time.
func New(m runtoken.Material, now func() time.Time) (*Codec, error) {
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	signer, err := runtoken.New(runtoken.Options{Domain: Domain, Material: m, Now: now})
	if err != nil {
		return nil, fmt.Errorf("ticket: %w", err)
	}
	return &Codec{signer: signer, now: now}, nil
}

// Mint signs claims into a ticket.
//
// THE CLAIMS ARE NORMALISED BEFORE THEY ARE SIGNED: the expiry is cut to whole
// seconds in UTC (the precision the runtoken carries its own expiry in) and the
// capabilities are sorted and de-duplicated, so what [Codec.Open] answers is
// byte for byte what was signed, and two mints of one grant are one value.
//
// A MINT NEVER OUTLIVES [artifacts.TicketLifetime], whatever the caller asks:
// an expiry already past, or one more than a lifetime away, is refused rather
// than clamped, because either is a caller that computed the wrong deadline and
// a silent clamp would hide it.
func (c *Codec) Mint(claims artifacts.TicketClaims) (string, error) {
	claims = normalise(claims)
	if err := check(claims); err != nil {
		return "", err
	}
	now := c.now()
	switch {
	case !claims.Expires.After(now):
		return "", fmt.Errorf("%w: expires %s, which is not after now (%s)",
			ErrInvalidClaims, claims.Expires.Format(time.RFC3339), now.Format(time.RFC3339))
	case claims.Expires.Sub(now) > artifacts.TicketLifetime+time.Second:
		return "", fmt.Errorf("%w: expires %s, more than the ticket lifetime of %s "+
			"from now", ErrInvalidClaims, claims.Expires.Format(time.RFC3339),
			artifacts.TicketLifetime)
	}
	subject, err := encode(claims)
	if err != nil {
		return "", err
	}
	// THE GRACE RIDES ON THE TOKEN, never on the claims: the token outlives
	// the claims by the grace, so a ticket that simply ran out still
	// verifies and is answered Expired rather than Invalid.
	return c.signer.Mint(subject, claims.Expires.Sub(now)+artifacts.TicketExpiredGrace), nil
}

// Open verifies a ticket and answers its claims and verdict. The claims are
// returned for an Expired ticket too, so a log line can name the artifact; they
// are the zero value for an Invalid one, and err says why for the log alone.
func (c *Codec) Open(token string) (artifacts.TicketClaims, Verdict, error) {
	if token == "" || len(token) > maxToken {
		return artifacts.TicketClaims{}, Invalid,
			fmt.Errorf("ticket: a token of %d bytes is not one this build mints", len(token))
	}
	subject := c.signer.Validate(token)
	if subject == "" {
		return artifacts.TicketClaims{}, Invalid,
			errors.New("ticket: the token is forged, malformed, signed under a key this " +
				"node does not hold, or expired past its grace")
	}
	claims, err := decode(subject)
	if err != nil {
		return artifacts.TicketClaims{}, Invalid, err
	}
	if !c.now().Before(claims.Expires) {
		return claims, Expired, nil
	}
	return claims, Valid, nil
}

// normalise is claims in the one form they are signed in.
func normalise(claims artifacts.TicketClaims) artifacts.TicketClaims {
	if !claims.Expires.IsZero() {
		claims.Expires = time.Unix(claims.Expires.Unix(), 0).UTC()
	}
	if len(claims.Capabilities) > 0 {
		caps := slices.Clone(claims.Capabilities)
		slices.Sort(caps)
		claims.Capabilities = slices.Compact(caps)
	} else {
		claims.Capabilities = nil
	}
	return claims
}

// check refuses claims no mint of this build writes, naming the field.
//
// THE LINEAGE IS CARRIED, NOT INTERPRETED: which credential classes exist and
// what each resolves to is the credential resolver's, on the serving side, and
// a second grammar here would be a class one side accepts and the other cannot
// resolve. What is held here is that it is a name at all.
func check(claims artifacts.TicketClaims) error {
	switch {
	case claims.Artifact.IsZero():
		return fmt.Errorf("%w: names no artifact", ErrInvalidClaims)
	case claims.Version < 1:
		return fmt.Errorf("%w: version %d; a version is numbered from 1",
			ErrInvalidClaims, claims.Version)
	case !token(claims.Principal, maxPrincipal):
		return fmt.Errorf("%w: principal %q is not a record owner", ErrInvalidClaims,
			claims.Principal)
	case !token(claims.Lineage, maxLineage) || !strings.Contains(claims.Lineage, ":"):
		return fmt.Errorf("%w: lineage %q names no credential", ErrInvalidClaims,
			claims.Lineage)
	case claims.AccessEpoch < 0:
		return fmt.Errorf("%w: access epoch %d is negative", ErrInvalidClaims,
			claims.AccessEpoch)
	case !nonce(claims.Nonce):
		return fmt.Errorf("%w: the nonce is not base64url of at most %d characters",
			ErrInvalidClaims, maxNonce)
	case claims.Expires.IsZero():
		return fmt.Errorf("%w: carries no expiry", ErrInvalidClaims)
	}
	for _, c := range claims.Capabilities {
		if !c.Valid() {
			return fmt.Errorf("%w: capability %q is not one this build serves",
				ErrInvalidClaims, c)
		}
	}
	return nil
}

// token reports a non-empty value of at most max bytes with no space or
// control character in it.
func token(s string, max int) bool {
	if s == "" || len(s) > max {
		return false
	}
	return !strings.ContainsFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsControl(r)
	})
}

// nonce reports a bridge nonce in the unpadded base64url alphabet.
func nonce(s string) bool {
	if s == "" || len(s) > maxNonce {
		return false
	}
	_, err := base64.RawURLEncoding.DecodeString(s)
	return err == nil
}

// encode is the subject a runtoken carries for claims: their canonical JSON,
// unpadded base64url. Canonical because the struct's field order is fixed and
// normalise sorted the one list in it.
func encode(claims artifacts.TicketClaims) (string, error) {
	raw, err := json.Marshal(claims)
	if err != nil {
		return "", fmt.Errorf("ticket: encode the claims: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

// decode reads a subject back to claims, refusing anything a mint would not
// have signed.
//
// A FIELD THIS BUILD DOES NOT KNOW IS IGNORED rather than refused, the
// additive-evolution rule every signed envelope in this engine follows: a node
// mid-rollout opens a ticket a newer peer minted and serves what this build
// can decide. The claims it does know are held to the same check a mint is.
func decode(subject string) (artifacts.TicketClaims, error) {
	raw, err := base64.RawURLEncoding.DecodeString(subject)
	if err != nil {
		return artifacts.TicketClaims{}, fmt.Errorf("ticket: the subject is not "+
			"base64url: %w", err)
	}
	var claims artifacts.TicketClaims
	dec := json.NewDecoder(bytes.NewReader(raw))
	if err := dec.Decode(&claims); err != nil {
		return artifacts.TicketClaims{}, fmt.Errorf("ticket: the claims do not "+
			"decode: %w", err)
	}
	if dec.More() {
		return artifacts.TicketClaims{}, errors.New("ticket: the subject carries " +
			"more than one claims document")
	}
	normalised := normalise(claims)
	if !slices.Equal(normalised.Capabilities, claims.Capabilities) ||
		!normalised.Expires.Equal(claims.Expires) {
		return artifacts.TicketClaims{}, errors.New("ticket: the claims are not in " +
			"the form a mint signs")
	}
	if err := check(normalised); err != nil {
		return artifacts.TicketClaims{}, err
	}
	return normalised, nil
}
