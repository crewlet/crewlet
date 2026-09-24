package iamdomain

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/iam"
)

// THE BLIND INDEX: how this estate looks somebody up without holding what it
// looked them up by.
//
// # The problem a plain column has
//
// An email address is the thing a person signs in with, so something has to
// resolve one to a person on every sign-in. The obvious column holds the
// address in the clear, and then the address is in every node's database, in
// every snapshot a node donates, in every backup, and — worst — in the
// SUBJECT the claim on it arbitrates on, which is a broker path carried in
// cleartext in every delivery, every consumer's filter and every operator's
// stream listing.
//
// # A keyed hash, and why keyed rather than plain
//
// A blind is an HMAC-SHA256 of the NORMALISED address under a key the company
// holds. A node with the key computes it from an address; nobody without the
// key can go the other way.
//
// PLAIN SHA-256 WOULD NOT DO, and the reason is the size of the input space
// rather than the strength of the hash: an address is drawn from a set an
// attacker can enumerate — a company's domain plus a name list is a few
// million guesses — so an unkeyed digest of one is a lookup table, not a
// blind. The key is what makes the set unenumerable without it.
//
// # ONE KEY FOR THE COMPANY, and it can never be per node
//
// The blind is the ARBITRATION SUBJECT of the claim on an address. Two nodes
// that computed different blinds for one address would publish to two
// subjects, neither would contend with the other, and BOTH claims would land —
// which is the duplicate identity the whole subject grammar exists to prevent,
// arriving through the one door that grammar cannot watch. So the key is a
// fleet secret, read through the company's sealed store, and a node that
// cannot read it refuses rather than guessing.
//
// # It is NOT a password hash and must never be used as one
//
// HMAC is fast on purpose: this runs on every sign-in and every lookup. What
// makes it safe here is that the input is an address rather than a secret —
// an attacker who recovers one has learned an email address they could have
// guessed. A credential goes through internal/iam's own verifier, which is
// deliberately slow.

// BlindKeyName is where the company's blind-index key lives in the fleet
// secret store.
//
// ONE NAME, and it carries no version: rotating this key is not a rotation in
// the ordinary sense — every blind in the estate is derived from it, so a new
// key means re-deriving every claim subject, which is a re-publication of the
// whole directory rather than a re-encryption of it. That is an operation
// somebody plans, not a sweep that runs, so there is no second name for a
// previous key to live under and no arm here that tries both.
//
// IN THE ENGINE'S OWN NAMESPACE of the company's secret store
// ([secrets.Reserved]). It used to be an environment-variable name, which
// made it an operator secret: listable,
// revealable — and a PUT of a different value orphaned every address in the
// directory and let each be claimed again, while `${…}` in an `mcp_env` handed
// the key that makes every address enumerable to a child process. No operator
// surface addresses it now, and a lost key comes back with the coordination
// store it lived in.
const BlindKeyName = "iam/blind-index-key"

// blindDomain separates this HMAC's inputs from every other use of the same
// key, in the shape internal/runtoken established: a key is only ever safe in
// one domain, and two uses that shared one would let a value minted for the
// first be presented to the second.
const blindDomain = "crewlet/iam/blind/v1"

// ErrNoBlindKey reports that this node cannot compute a blind at all.
//
// DISTINCT FROM A LOOKUP THAT FOUND NOBODY, and the distinction is the whole
// three-valued rule this tree applies everywhere: "nobody holds this address"
// and "this node could not tell" send a request to opposite places — a sign-in
// refused, and a sign-in answered 503 while an operator is told the key is
// missing.
var ErrNoBlindKey = errors.New("iamdomain: no blind-index key")

// Blinder derives the blind index for a value the company matches on.
//
// A VALUE TYPE HOLDING THE KEY, rather than a function taking it at every call
// site: the key is read once per epoch from the sealed store, and a signature
// that took it each time would have every caller deciding where to get it
// from — which is how one of them comes to derive a blind under a key nobody
// else has.
type Blinder struct{ key []byte }

// NewBlinder builds one over the company's key.
//
// It refuses an EMPTY key rather than deriving under one, because an empty
// HMAC key is a valid HMAC key: it would produce stable, plausible blinds that
// any other deployment with the same bug could reproduce, and the estate would
// look exactly as it does when it is working.
func NewBlinder(key []byte) (*Blinder, error) {
	if len(key) == 0 {
		return nil, fmt.Errorf("%w: the company's %s is unset, so this node "+
			"cannot resolve an address to a person and cannot form the subject "+
			"a claim on one arbitrates against", ErrNoBlindKey, BlindKeyName)
	}
	if len(key) < MinBlindKeyBytes {
		return nil, fmt.Errorf("%w: the company's %s is %d bytes and the floor "+
			"is %d — a short key is what makes an address space an attacker can "+
			"enumerate into a lookup table", ErrNoBlindKey, BlindKeyName,
			len(key), MinBlindKeyBytes)
	}
	return &Blinder{key: append([]byte(nil), key...)}, nil
}

// Blinds is where a writer or a sign-in gets the company's blinder, at the
// moment it needs one.
//
// # A source rather than a value, because the key is minted on first use
//
// The key does not exist until some node mints it, and the node that mints it
// is whichever first needs a blind — which is after every node has booted and
// built its writer. A blinder fixed at construction was therefore nil on every
// node of every fresh deployment, and every enrolment with an address, every
// invitation and every sign-in by address was refused for a key nothing ever
// wrote. An error here is the third value, as everywhere in this estate: the
// key could not be established, which is neither "nobody holds this address"
// nor a blind.
type Blinds interface {
	Blinder(ctx context.Context) (*Blinder, error)
}

// Blinder makes a blinder its own source: a key already in hand needs no
// resolution. A nil one answers [ErrNoBlindKey] rather than a nil blinder, so a
// typed nil in the interface is a refusal and never a panic.
func (b *Blinder) Blinder(context.Context) (*Blinder, error) {
	if b == nil {
		return nil, fmt.Errorf("%w: the company's %s is unset", ErrNoBlindKey,
			BlindKeyName)
	}
	return b, nil
}

// MinBlindKeyBytes is the floor under the blind-index key.
//
// THIRTY-TWO, which is SHA-256's own block-derived output width and the same
// floor internal/whsec puts under a webhook signing key. HMAC accepts a key of
// any length and pads a short one, so nothing below this layer refuses one —
// which is exactly why the refusal is here, at the edge where the value is
// accepted, rather than trusted where it is used.
const MinBlindKeyBytes = 32

// Email is the blind for an email address.
//
// IT NORMALISES FIRST, through [iam.NormalizeEmail], which is the same fold
// the org chart matches a vendor payload's address by. Two folds would mean
// one address reaching a seat and a different person.
func (b *Blinder) Email(address string) (string, error) {
	folded := iam.NormalizeEmail(address)
	if folded == "" {
		return "", fmt.Errorf("iamdomain: an empty address has no blind — a " +
			"claim on one would arbitrate on a subject every addressless person " +
			"in the company shares")
	}
	return b.derive("email", folded)
}

// Subject is the blind for an identity provider's own subject claim.
//
// NOT NORMALISED, and the asymmetry with an address is the provider's: an
// `iss`/`sub` pair is an opaque identifier the provider assigned, case is
// significant in it, and folding one would merge two accounts the provider
// considers distinct. It is blinded for the address's reason — it identifies
// a person at a third party — and for no other.
func (b *Blinder) Subject(issuer, subject string) (string, error) {
	if issuer == "" || subject == "" {
		return "", fmt.Errorf("iamdomain: an identity-provider binding needs "+
			"both an issuer and a subject, and has issuer %q subject %q — a "+
			"binding missing either would match every account at that provider "+
			"or every provider's account for that subject", issuer, subject)
	}
	return b.derive("oidc", issuer+"\x00"+subject)
}

// InvitationID is the invitation an issue names, derived from the operation
// key it is published under — so a retry of an issue whose outcome nobody could
// establish names the invitation its first attempt issued, and hands back the
// link to it rather than a second invitation the address then refuses.
//
// # Under the company's key, because the id is the verifier
//
// Holding an invitation's link is holding its id, so the id is a bearer
// credential for whatever the invitation confers, and its entropy must not be
// the caller's to choose: a key somebody typed, or minted from a weak source,
// derived under a plain hash would be a link anybody could compute. Under the
// company's key it is unguessable without that key whatever the operation key
// was — and in a MAC domain of its own ([invitationIDDomain]), so no blind of
// any address or subject is ever an invitation id.
//
// A UUID7 AT THE KEY'S INSTANT, which is what [InvitedPersonID] derives the
// person the invitation creates at: the key must be a uuid7 ([operationKey]).
func (b *Blinder) InvitationID(key string) (string, error) {
	if b == nil || len(b.key) == 0 {
		return "", ErrNoBlindKey
	}
	id, err := operationKey(key)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, b.key)
	_, _ = mac.Write([]byte(invitationIDDomain))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(id.String()))
	return uuid7At(instantOf(id), mac.Sum(nil)[:16]).String(), nil
}

// invitationIDDomain separates an invitation id's MAC from every blind the same
// key derives, for [blindDomain]'s reason.
const invitationIDDomain = "crewlet/iam/invitation-id/v1"

// derive is the one HMAC, and the one encoding.
//
// THE CLASS IS INSIDE THE MAC rather than beside it, so an address's blind and
// an identity provider's can never collide even if the two values were equal —
// which they can be, because a provider is free to use an address as its
// subject claim. A separator between the class and the value is what stops
// `("emai", "lx@y")` and `("email", "x@y")` producing one blind.
//
// HEX rather than base64, because the result is a SUBJECT TOKEN on a broker
// path: base64's `+` and `/` are not path-safe, base64url's `-` and `_` are
// but buy nothing here, and hex is the encoding an operator reading a stream
// listing can compare by eye without wondering about padding.
func (b *Blinder) derive(class, value string) (string, error) {
	if b == nil || len(b.key) == 0 {
		return "", ErrNoBlindKey
	}
	mac := hmac.New(sha256.New, b.key)
	_, _ = mac.Write([]byte(blindDomain))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(class))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(value))
	return hex.EncodeToString(mac.Sum(nil)), nil
}
