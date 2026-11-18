package iamdomain

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/iam"
)

// THE BLIND INDEX: how this estate looks somebody up by email without holding
// the address, which a plain column would put in every node's database,
// snapshot, backup and directory record.
//
// A blind is an HMAC-SHA256 of the NORMALISED address under the company's key.
// KEYED, because addresses are enumerable (a domain plus a name list is a few
// million guesses), so a plain digest is a lookup table. ONE KEY FOR THE
// COMPANY, never per node: a directory decide compares by the blind, and nodes
// deriving different blinds would each admit a duplicate identity, so a node
// that cannot read the key refuses rather than guessing. NOT A PASSWORD HASH:
// HMAC is fast because the input is an address, not a secret; a credential
// goes through internal/iam's deliberately slow verifier.

// BlindKeyName is where the company's blind-index key lives in the fleet
// secret store.
//
// ONE NAME, with no version: every blind derives from the key, so rotating it
// means re-publishing the whole directory, a planned operation rather than a
// sweep, and nothing here tries a previous key.
//
// IN THE ENGINE'S OWN NAMESPACE ([secrets.Reserved]), never an operator
// secret: one an operator could list, reveal, PUT over (orphaning every
// address so each could enrol again) or hand to a child process through `${…}`
// in an `mcp_env`. A lost key comes back with the coordination store it lived
// in.
const BlindKeyName = "iam/blind-index-key"

// blindDomain separates this HMAC's inputs from every other use of the same
// key, in internal/runtoken's shape: two uses sharing a domain would let a
// value minted for one be presented to the other.
const blindDomain = "crewlet/iam/blind/v1"

// ErrNoBlindKey reports that this node cannot compute a blind at all.
//
// DISTINCT FROM A LOOKUP THAT FOUND NOBODY (the three-valued rule): "nobody
// holds this address" refuses a sign-in, while "this node could not tell"
// answers 503 and tells an operator the key is missing.
var ErrNoBlindKey = errors.New("no email lookup key")

// Blinder derives the blind index for a value the company matches on.
//
// A VALUE HOLDING THE KEY, read once per epoch from the sealed store, not a
// key passed at every call site: a caller choosing where to get it is how one
// derives a blind under a key nobody else has.
type Blinder struct{ key []byte }

// NewBlinder builds one over the company's key.
//
// It refuses an EMPTY key: an empty HMAC key is valid and yields stable,
// plausible blinds any deployment with the same bug could reproduce, while the
// estate looks as if it works.
func NewBlinder(key []byte) (*Blinder, error) {
	if len(key) == 0 {
		return nil, fmt.Errorf("%w: the company's %s isn't set, so this node "+
			"can't look people up by email", ErrNoBlindKey, BlindKeyName)
	}
	if len(key) < MinBlindKeyBytes {
		return nil, fmt.Errorf("%w: the company's %s is %d bytes, and it "+
			"needs at least %d", ErrNoBlindKey, BlindKeyName,
			len(key), MinBlindKeyBytes)
	}
	return &Blinder{key: append([]byte(nil), key...)}, nil
}

// Blinds is where a writer or a sign-in gets the company's blinder, at the
// moment it needs one.
//
// A SOURCE, NOT A VALUE: whichever node first needs a blind mints the key,
// after every node has built its writer, so a blinder fixed at construction
// would be nil on a fresh deployment and refuse every address. An error is the
// third value: the key could not be established, which is neither "nobody
// holds this address" nor a blind.
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
// THIRTY-TWO, SHA-256's output width and internal/whsec's floor for a webhook
// signing key. HMAC pads a short key, so nothing below refuses one: the
// refusal belongs here, where the value is accepted.
const MinBlindKeyBytes = 32

// Email is the blind for an email address.
//
// IT NORMALISES FIRST, through [iam.NormalizeEmail], the fold the org chart
// matches a vendor payload's address by: two folds would let one address reach
// a seat and a different person.
func (b *Blinder) Email(address string) (string, error) {
	folded := iam.NormalizeEmail(address)
	if folded == "" {
		// NOT FINDABLE, a value the caller typed: a whitespace-only address
		// passes an empty check, and must not answer as a 500.
		return "", fmt.Errorf("%w: the email address is empty",
			ErrNotFindable)
	}
	return b.derive("email", folded)
}

// InvitationID is the invitation an issue names, derived from the operation
// key it is published under, so a retry whose outcome nobody could establish
// names the invitation its first attempt issued and hands back its link.
//
// UNDER THE COMPANY'S KEY: the id opens nothing alone (the link's SECRET does,
// [Blinder.InvitationSecret]), but the secret derives from it, so a plain hash
// of a typed key would let anybody who saw the key compute the link. Its own
// MAC domain ([invitationIDDomain]) keeps any address's blind from being an
// invitation id.
//
// A UUID7 AT THE KEY'S INSTANT, the instant a redemption's operation id
// carries: the key must be a uuid7 ([operationKey]).
func (b *Blinder) InvitationID(key string) (string, error) {
	if b == nil || len(b.key) == 0 {
		return "", ErrNoBlindKey
	}
	id, err := operationKey(key)
	if err != nil {
		return "", err
	}
	return uuid7At(instantOf(id), b.mac(invitationIDDomain, id.String())[:16]).
		String(), nil
}

// invitationIDDomain separates an invitation id's MAC from every blind the same
// key derives, for [blindDomain]'s reason.
const invitationIDDomain = "crewlet/iam/invitation-id/v1"

// InvitationSecret is the secret an invitation's LINK carries beside its id:
// the half that is the credential. The id is stored in the clear (primary key,
// record, snapshots, backups, the cluster port, proxy logs), so the link is
// `<id>.<secret>` and the estate keeps only [InvitationVerifier] of the secret.
//
// DERIVED, NOT MINTED: a MAC of the id under the company's key in its own
// domain ([invitationSecretDomain]), so an issuer's retry with the same
// operation key hands back the same link. Only a key holder can compute it,
// and that holder can already read every sealed address.
//
// Thirty-two bytes, URL-safe base64 without padding: it rides a URL fragment
// and a form field unescaped.
func (b *Blinder) InvitationSecret(id string) (string, error) {
	if b == nil || len(b.key) == 0 {
		return "", ErrNoBlindKey
	}
	if id == "" {
		return "", errors.New("iamdomain: an invitation's secret is derived " +
			"from its id, and this one names none")
	}
	return base64.RawURLEncoding.EncodeToString(
		b.mac(invitationSecretDomain, id)), nil
}

// invitationSecretDomain separates an invitation's link secret from its id and
// from every blind the same key derives.
const invitationSecretDomain = "crewlet/iam/invitation-secret/v1"

// PasswordLinkID is the credential id of the FIRST PASSWORD LINK an
// administrator's create issues a person ([Writer.Create]), derived from the
// person, who derives from the create's operation key ([CreatedPersonID]): a
// retry with that key, answered from the ledger without running the decide,
// hands back the link the first attempt issued. A redemption's person is
// minted per attempt and never gets one.
//
// UNDER THE COMPANY'S KEY, in its own MAC domain ([passwordLinkIDDomain]): the
// id is in the clear and the secret derives from it, so an id computable from
// a person's id would be one step from a computable link. A UUID7 AT THE
// PERSON'S INSTANT, as every credential id here, so a person id that is not
// one is [ErrInvalid].
func (b *Blinder) PasswordLinkID(personID string) (string, error) {
	if b == nil || len(b.key) == 0 {
		return "", ErrNoBlindKey
	}
	id, err := uuid.Parse(personID)
	if err != nil || id.Version() != 7 || id.Variant() != uuid.RFC4122 {
		return "", fmt.Errorf("%w: a first password link is derived at its "+
			"person's instant, so their id must be a uuid7 (%q)", ErrInvalid,
			personID)
	}
	return uuid7At(instantOf(id), b.mac(passwordLinkIDDomain, id.String())[:16]).
		String(), nil
}

// passwordLinkIDDomain separates a first password link's id from every other
// value the company's key derives.
const passwordLinkIDDomain = "crewlet/iam/password-link-id/v1"

// PasswordLinkSecret is the secret a first password link carries beside its
// id (the estate keeps only its [credential.ResetVerifier]), derived from the
// link's id under the company's key for [Blinder.InvitationSecret]'s reason:
// the create's retry hands back the same link. Its encoding is an
// invitation's, which carries no `.`, the separator `<id>.<secret>` splits on.
//
// WHAT THE DERIVATION EXPOSES: a key holder can compute an open link's secret
// from its row's id and set the person's password. That holder already holds
// the fleet keyring, opens every sealed address and signs any record this log
// accepts, so it is no new reach; an invitation's secret rests on the same.
func (b *Blinder) PasswordLinkSecret(linkID string) (string, error) {
	if b == nil || len(b.key) == 0 {
		return "", ErrNoBlindKey
	}
	if linkID == "" {
		return "", errors.New("iamdomain: a password link's secret is derived " +
			"from its id, and this one names none")
	}
	return base64.RawURLEncoding.EncodeToString(
		b.mac(passwordLinkSecretDomain, linkID)), nil
}

// passwordLinkSecretDomain separates a first password link's secret from its
// id, from an invitation's and from every blind the same key derives.
const passwordLinkSecretDomain = "crewlet/iam/password-link-secret/v1"

// mac is one keyed digest of value in its own domain (the domain, a separator,
// the value): every derivation here but an address's blind.
func (b *Blinder) mac(domain, value string) []byte {
	mac := hmac.New(sha256.New, b.key)
	_, _ = mac.Write([]byte(domain))
	_, _ = mac.Write([]byte{0})
	_, _ = mac.Write([]byte(value))
	return mac.Sum(nil)
}

// InvitationVerifier is what the estate keeps of an invitation's link secret:
// its SHA-256, hex.
//
// A HASH, NOT argon2 (internal/iam/credential's rule: spend cost where an
// attacker has a shortcut): a secret derived from thirty-two bytes of MAC has
// no dictionary to grind.
func InvitationVerifier(secret string) string {
	sum := sha256.Sum256([]byte(secret))
	return hex.EncodeToString(sum[:])
}

// derive is the one HMAC, and the one encoding.
//
// THE CLASS IS INSIDE THE MAC, so blinds of different classes never collide on
// equal values. Only addresses are derived, but every blind an estate holds
// includes the class, so dropping it would re-key every address. The separator
// stops `("emai", "lx@y")` and `("email", "x@y")` producing one blind.
//
// HEX, not base64: the result is read as well as compared (an invitation's
// trail object, a value an operator compares by eye), and hex carries no
// padding and nothing a path, a LIKE or a subject reads as structure.
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
