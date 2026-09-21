package statelog

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
)

// EVERY FLEET-WIDE WRITE THAT CHANGES WHAT A NODE RUNS IS AUTHENTICATED, and
// this is where.
//
// # Why the framework signs and not each domain
//
// The embedded broker has no auth of its own. It binds no listener on the
// default topology, but a fleet clusters those members over a port, and a
// record on a domain's log is whatever the next node applies: rows every peer
// derives identically, a seat's environment, a person's grants. An unsigned
// record on such a log is not a message, it is an INSTRUCTION anyone who can
// reach the cluster port may write.
//
// So a record is framed and signed by the framework, on its way to the
// appender, and verified by the framework before any domain decodes it. Each
// domain signing its own would be one implementation per domain of a rule that
// is identical for all of them, in files nothing compares — and the one that
// forgot would be indistinguishable from the ones that did not, because an
// unsigned record looks exactly like a record from a build that predates the
// rule.
//
// # The frame names the key that signed it
//
// A verifier looks the id up and refuses a record signed under a key it does
// not hold, rather than retrying against whichever key is active. That is what
// makes a keyring rotation survivable in both directions during a rollout: a
// node that has added k2 and one that has not both verify a k1 record, and
// both verify a k2 record once k2 is on their ring. Derived from the whole
// keyring instead, every node that had loaded a different key SET would derive
// a different key and refuse every peer's records for the length of the
// rollout.
//
// [runtoken] states the same rule for the credential a sandbox carries in a
// URL path, and encodes it differently for a good reason: a path segment needs
// a digest of the key id, and this frame is bytes, so it carries the id
// itself. Two encodings of one rule, each pointing at the other.
//
// # Two failures, and only one of them is recoverable
//
// A key this node does not hold is a fact about THIS NODE, and it changes when
// an operator adds the key. Such a record is RETAINED and reprocessed, the
// same disposition a record from a newer build gets, because both mean "this
// build cannot read this yet" and both stop being true without the record
// changing.
//
// A MAC that fails under a key this node DOES hold is a fact about the RECORD,
// and no operator action makes it true. It is refused permanently and the
// applier stops: a record whose signature fails under a key the fleet holds
// was written by something that is not the fleet, and continuing past it would
// be applying whatever came after it.

// frameMagic prefixes every signed record, so a record from before this rule
// fails as an unsigned record naming itself rather than as a bad signature.
var frameMagic = [4]byte{'c', 's', 'l', 'g'}

// frameVersion is the frame's own version, independent of any domain's record
// version: the frame is the framework's and every domain shares it.
const frameVersion byte = 1

// macLen is the SHA-256 HMAC's length.
const macLen = sha256.Size

// maxKeyIDLen bounds the id the frame carries. Operator-chosen key ids are
// short names in a config file; the bound exists so a malformed frame cannot
// claim a length the rest of the record does not have.
const maxKeyIDLen = 64

// Verdict is what verification concluded about one record's frame.
//
// A NAMED TYPE WITH THREE VALUES rather than a bool, because the two failures
// have opposite dispositions: one is recoverable and one is permanent, and a
// caller handed `false` for both would have to guess which.
type Verdict string

const (
	// Verified: the frame's MAC matches under the key it names.
	Verified Verdict = "verified"

	// KeyUnknown: the frame names a key this node does not hold. The
	// record is retained and reprocessed when the key arrives.
	KeyUnknown Verdict = "key_unknown"

	// Tampered: the frame names a key this node holds and the MAC does not
	// match under it, or the frame is not a frame at all. Permanent.
	Tampered Verdict = "tampered"
)

// Valid reports whether v is one of the three answers.
func (v Verdict) Valid() bool {
	return v == Verified || v == KeyUnknown || v == Tampered
}

// ErrUnsigned is what a nil signer or verifier refuses with, so a deployment
// that reaches a domain with no keyring is told what to configure rather than
// running one log signed and another not.
var ErrUnsigned = errors.New("statelog: no keyring")

// Key is one keyring entry as the framework reads it.
type Key struct {
	ID       string
	Material string
}

// Keyring is the fleet's key material for record signatures.
type Keyring struct {
	// ActiveID names the key new records are signed under. It must name
	// one of Keys.
	ActiveID string

	// Keys is every key a record may have been signed under. A key stays
	// here for as long as a record signed under it could still be
	// UNAPPLIED on any node — which, because a log is replayed from its
	// floor, means for as long as the log holds records from that period.
	Keys []Key
}

// Signer frames and signs one domain's records.
//
// SAFE FOR CONCURRENT USE and immutable: a frame is a function of the key, the
// domain and the body.
type Signer struct {
	domain   string
	activeID string
	keys     map[string][]byte
}

// Verifier opens a framed record.
//
// SEPARATE FROM [Signer] because the two seams are used by different halves —
// the write authority signs and the apply loop verifies — and a node that
// runs a domain it may not write to still has to verify every record it
// applies.
type Verifier struct {
	domain string
	keys   map[string][]byte
}

// NewSigner builds the write half. It refuses a keyring that names no active
// key, because a signer that silently signed under nothing would produce
// records the fleet refuses one at a time, on every node, for ever.
func NewSigner(domain string, ring Keyring) (*Signer, error) {
	keys, err := deriveKeys(domain, ring)
	if err != nil {
		return nil, err
	}
	if _, held := keys[ring.ActiveID]; !held {
		return nil, fmt.Errorf("%w: %s's records are signed under secrets.active_key_id "+
			"and it names %q, which is not in secrets.keys — set it to one of them, "+
			"or run `crewlet secrets keygen` to create the first",
			ErrUnsigned, domain, ring.ActiveID)
	}
	return &Signer{domain: domain, activeID: ring.ActiveID, keys: keys}, nil
}

// NewVerifier builds the read half. A verifier needs no active key: it opens
// whatever the ring can open and answers [KeyUnknown] for the rest.
func NewVerifier(domain string, ring Keyring) (*Verifier, error) {
	keys, err := deriveKeys(domain, ring)
	if err != nil {
		return nil, err
	}
	if len(keys) == 0 {
		return nil, fmt.Errorf("%w: %s's records are verified against secrets.keys "+
			"and this node has none, so it could not tell a peer's record from one "+
			"written by anything else that can reach the broker — run `crewlet "+
			"secrets keygen`", ErrUnsigned, domain)
	}
	return &Verifier{domain: domain, keys: keys}, nil
}

func deriveKeys(domain string, ring Keyring) (map[string][]byte, error) {
	keys := make(map[string][]byte, len(ring.Keys))
	for _, key := range ring.Keys {
		if key.ID == "" {
			return nil, fmt.Errorf("%w: a secrets.keys entry has no id, and the id is "+
				"what a record names so that a rotation does not refuse the fleet's "+
				"own records", ErrUnsigned)
		}
		if len(key.ID) > maxKeyIDLen {
			return nil, fmt.Errorf("%w: the secrets.keys id %q is longer than %d bytes, "+
				"and it travels in every record this node writes", ErrUnsigned,
				key.ID, maxKeyIDLen)
		}
		keys[key.ID] = deriveRecordKey(domain, key.ID, key.Material)
	}
	return keys, nil
}

// deriveRecordKey binds the domain and the id in beside the material, so that
// one domain's record can never verify on another's log and a key moved to a
// new id is a new key rather than the same one wearing a new name.
//
// The "statelog/record" label is what stops a record MAC and a per-run token
// MAC over the same material from ever being the same bytes.
func deriveRecordKey(domain, id, material string) []byte {
	sum := sha256.Sum256([]byte("statelog/record|" + domain + "|" + id + "|" + material))
	return sum[:]
}

// Seal frames and signs one record body.
func (s *Signer) Seal(body []byte) []byte {
	id := s.activeID
	out := make([]byte, 0, len(frameMagic)+2+len(id)+macLen+len(body))
	out = append(out, frameMagic[:]...)
	out = append(out, frameVersion, byte(len(id)))
	out = append(out, id...)
	mac := recordMAC(s.keys[id], s.domain, id, body)
	out = append(out, mac...)
	return append(out, body...)
}

// Open verifies a framed record and returns the body it carries.
//
// THE BODY COMES BACK FOR [Verified] AND [KeyUnknown], and never for
// [Tampered]. A record signed under a key this node does not hold still has to
// be FILED — retained under its own scope, so that later records touching the
// same objects wait behind it rather than applying on rows it never wrote —
// and the scope is inside the body. So the framework decodes that body to file
// it and applies nothing: there is exactly one apply site and it applies on
// [Verified] alone.
//
// [Tampered] bytes are not from this fleet, so no domain decoder is handed
// them at all.
//
// TWO RESULTS AND NO ERROR, because there is no fourth answer and never can
// be: the keys were derived when this verifier was built, and every way a
// record can fail to open — a frame that is not one, a truncated one, an id
// this node does not hold, a MAC that does not match — is one of the three
// verdicts. An error channel here would be nil at every call site, and a
// reader would have to write a branch that cannot be reached to satisfy it.
func (v *Verifier) Open(framed []byte) ([]byte, Verdict) {
	id, mac, body, ok := splitFrame(framed)
	if !ok {
		// NOT A FRAME AT ALL is [Tampered] rather than a third answer.
		// Nothing writes an unframed record any more, so what reaches
		// here is a record from something that is not this fleet — or,
		// on a log written before this rule existed, a deployment whose
		// upgrade path is stated in the release notes rather than
		// guessed at by an applier.
		return nil, Tampered
	}
	key, held := v.keys[id]
	if !held {
		return body, KeyUnknown
	}
	if !hmac.Equal(mac, recordMAC(key, v.domain, id, body)) {
		return nil, Tampered
	}
	return body, Verified
}

// KeyIDs is every id this verifier holds, sorted, for the log line that says
// why a record was retained.
func (v *Verifier) KeyIDs() []string {
	out := make([]string, 0, len(v.keys))
	for id := range v.keys {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// splitFrame parses a frame without trusting any length it declares.
func splitFrame(framed []byte) (id string, mac, body []byte, ok bool) {
	const head = len(frameMagic) + 2
	if len(framed) < head {
		return "", nil, nil, false
	}
	if [4]byte(framed[:4]) != frameMagic || framed[4] != frameVersion {
		return "", nil, nil, false
	}
	idLen := int(framed[5])
	if idLen == 0 || idLen > maxKeyIDLen || len(framed) < head+idLen+macLen {
		return "", nil, nil, false
	}
	return string(framed[head : head+idLen]),
		framed[head+idLen : head+idLen+macLen],
		framed[head+idLen+macLen:], true
}

// recordMAC is the signature itself.
//
// THE ID AND THE DOMAIN ARE INSIDE THE MAC, not merely beside it: a frame
// whose id was rewritten to name a different key, or replayed onto another
// domain's log, must not verify. Binding them into the derived key alone would
// leave the id in the frame unauthenticated, so it is bound in both places —
// the derivation makes the key different, and this makes the claim itself
// part of what is signed.
func recordMAC(key []byte, domain, id string, body []byte) []byte {
	mac := hmac.New(sha256.New, key)
	var n [8]byte
	binary.BigEndian.PutUint64(n[:], uint64(len(domain)))
	mac.Write(n[:])
	mac.Write([]byte(domain))
	binary.BigEndian.PutUint64(n[:], uint64(len(id)))
	mac.Write(n[:])
	mac.Write([]byte(id))
	mac.Write(body)
	return mac.Sum(nil)
}

// signedAppender signs every body on its way to the log.
//
// WRAPPING THE APPENDER rather than signing at each call site is what makes
// the rule impossible to forget: there are two appends in this package today
// (a write authority's and a read index's barrier), and the third one somebody
// adds is signed by construction rather than by review.
type signedAppender struct {
	Appender
	signer *Signer
}

func (a signedAppender) Append(ctx context.Context, subject, msgID string,
	expect *uint64, body []byte) (uint64, bool, error) {

	return a.Appender.Append(ctx, subject, msgID, expect, a.signer.Seal(body))
}

// signing wraps log so every record it carries is signed, refusing a nil
// signer rather than writing an unsigned one.
func signing(log Appender, signer *Signer, domain string) (Appender, error) {
	if signer == nil {
		return nil, fmt.Errorf("%w: %s has no signer, so its records would be "+
			"unsigned and every node that verifies would refuse them — Tier A "+
			"secrets.keys is required wherever a state-log domain runs",
			ErrUnsigned, domain)
	}
	return signedAppender{Appender: log, signer: signer}, nil
}

// OneKey is a keyring with a single key, which is what a deployment that has
// run `crewlet secrets keygen` once holds and what a test wants: the rotation
// arms are exercised by keyrings that actually carry two.
func OneKey(id, material string) Keyring {
	return Keyring{ActiveID: id, Keys: []Key{{ID: id, Material: material}}}
}
