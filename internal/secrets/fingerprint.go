package secrets

// A FINGERPRINT: a value's keyed digest under every key of the keyring, so two
// processes can learn whether they hold the same value without either of them
// publishing it.
//
// # Why keyed, and why under the keyring
//
// The value it is taken of is a credential table — which key is which role, and
// a digest of each key's value — and an unkeyed hash of that is a test anybody
// who can read the place it is published can run guesses against: a token short
// enough to type is short enough to search for. Under a key only the fleet
// holds, a fingerprint answers "same" or "different" to a peer holding the key
// and nothing to anybody else. The keyring is the one secret every node of a
// fleet already shares — the configuration plane seals its payload under it,
// and a node that cannot open that payload cannot apply a revision — so it is
// the key, rather than a second shared secret an operator would have to
// distribute and keep in step.
//
// # Under EVERY key, and not under the active one
//
// A keyring rotation is rolling: the new key is added on every node, made
// active, and the old one dropped, one restart at a time. So two live nodes can
// name different active keys, and one can hold a key the other has not received
// yet or has already dropped. A fingerprint under the active key alone would
// differ between them although the value is the same; one under a single key
// derived from the whole ring — the run-token idiom, which hashes every key
// together — would differ the moment one node added the new key. Taken under
// every key, two nodes compare under the keys they share, and the rotation the
// keyring exists to make zero-downtime never leaves a pair with none in common.
//
// # A CHECK beside each digest
//
// Two digests that differ under one key id say the values differ only if the
// key behind that id is the same on both sides. It is not on a node whose
// keyring was written with different material under the same id — a fault of
// its own, which the configuration plane meets as an envelope it cannot open —
// and reading that as "the values differ" would send an operator to compare two
// tables that are identical. So each entry carries a check derived from the key
// alone, and a pair whose checks differ was taken under different keys and says
// nothing about the value.
//
// # The domain
//
// Every fingerprint names what it is OF, and is taken under a key derived from
// the keyring key and that domain (HKDF-SHA-256) — never the keyring key
// itself, which seals secrets with AES-GCM, and one key used by two algorithms
// is a key whose every use has to be proven safe against the other. The domain
// also keeps a fingerprint of one kind of value from ever matching another's.

import (
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
)

// Fingerprint is a value's digest under every key of a keyring, by key id. An
// empty one is a value taken under no key — a node with no keyring — which
// compares with nothing.
type Fingerprint map[string]KeyedDigest

// KeyedDigest is a value's digest under one keyring key.
type KeyedDigest struct {
	// Check identifies the key material the digest was taken under, and says
	// nothing about the value: two entries whose checks differ were taken
	// under different keys, whatever their ids say.
	Check string
	// Digest is the value's HMAC-SHA-256 under the domain's key, in hex.
	Digest string
}

// checkBytes is how much of the derived check a fingerprint carries.
//
// EIGHT BYTES: the check only has to tell two keys apart, never resist a
// forger — anybody who can write a fingerprint can write any check they like —
// so the odds that matter are two different keys colliding by chance, one in
// 2^64.
const checkBytes = 8

// Fingerprint takes value's digest under every key of the ring, for domain.
//
// A ring with no keys yields an empty fingerprint, which is the honest answer
// for a node with no keyring: it holds nothing a peer could compare against.
func (k Keyring) Fingerprint(domain string, value []byte) (Fingerprint, error) {
	if domain == "" {
		return nil, errors.New("secrets: a fingerprint needs the domain it is taken in")
	}
	out := make(Fingerprint, len(k.Keys))
	for id, material := range k.Keys {
		if len(material) != keyBytes {
			return nil, fmt.Errorf("%w: key %q is %d bytes, want %d", ErrNoKey, id,
				len(material), keyBytes)
		}
		// TWO DERIVATIONS, one per purpose, so the check can never equal a
		// digest whatever value a caller fingerprints.
		key, err := hkdf.Key(sha256.New, material, nil, "crewlet/fingerprint/"+domain, keyBytes)
		if err != nil {
			return nil, fmt.Errorf("secrets: derive the %s key from %q: %w", domain, id, err)
		}
		check, err := hkdf.Key(sha256.New, material, nil, "crewlet/fingerprint-check/"+domain, checkBytes)
		if err != nil {
			return nil, fmt.Errorf("secrets: derive the %s check from %q: %w", domain, id, err)
		}
		mac := hmac.New(sha256.New, key)
		mac.Write(value)
		out[id] = KeyedDigest{Check: hex.EncodeToString(check), Digest: hex.EncodeToString(mac.Sum(nil))}
	}
	return out, nil
}

// Agreement is how two fingerprints' values stand against each other.
type Agreement string

const (
	// AgreementSame is two values a shared key says are identical.
	AgreementSame Agreement = "same"
	// AgreementDifferent is two values a shared key says differ.
	AgreementDifferent Agreement = "different"
	// AgreementIncomparable is two fingerprints with no key in common — one
	// of them taken under no key at all, or the two under keyrings that share
	// no key material — which says nothing either way. Never read as either
	// of the other two: a pair nothing can compare has not been shown to
	// agree.
	AgreementIncomparable Agreement = "incomparable"
)

// Agreements is every agreement, in the order a screen lists them.
var Agreements = []Agreement{AgreementSame, AgreementDifferent, AgreementIncomparable}

// Valid reports whether a is an agreement this build sends.
func (a Agreement) Valid() bool { return slices.Contains(Agreements, a) }

// Against reports how this fingerprint's value stands against other's.
//
// Compared under every key id the two share whose CHECK matches — the same
// key, not only the same name. DIFFERENT WINS over same: honest fingerprints
// under two shared keys cannot disagree, so a pair that does was written by
// something other than this function, and a pair that disagrees under any key
// they share is never reported as agreeing.
func (f Fingerprint) Against(other Fingerprint) Agreement {
	out := AgreementIncomparable
	for id, mine := range f {
		theirs, ok := other[id]
		if !ok || mine.Check == "" || mine.Check != theirs.Check {
			continue
		}
		if mine.Digest == "" || mine.Digest != theirs.Digest {
			return AgreementDifferent
		}
		out = AgreementSame
	}
	return out
}
