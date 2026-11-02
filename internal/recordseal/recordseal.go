// Package recordseal is a record's seal under the fleet's keyring: the frame
// that names the key a record was signed under, the MAC over it, the signer
// that writes one and the verifier that opens one into one of three verdicts
// (ADR-0031).
//
// EVERY FLEET-WIDE WRITE THAT CHANGES WHAT A NODE RUNS IS AUTHENTICATED, and
// this is how. A state-log record is sealed on its way to the appender and
// opened before any domain decodes it ([internal/statelog] wraps its appender
// and its readers around this package), and every other channel whose
// messages are instructions seals under the same frame in key domains of its
// own: a request one node asks another to carry out and its reply
// ([internal/signedask]), a person's answer to a parked run
// ([internal/sandbox]'s answer seal), a note to a coding run on its record.
//
// # Why a leaf
//
// The seal imports nothing of the engine: no state log, no store, no
// configuration, no domain. What drives a box, what answers a scatter and
// what fences a row each needs to sign and verify, and none of them needs an
// ordered log, an applier or a SQL estate to do it. Kept inside the state
// log, the signer made every one of them link the state log's whole graph
// for one HMAC, and a package that must not reach the log — the sandbox,
// which carries a run's code floor as bytes it never reads — could only be
// told not to read it by a list of the names it was still allowed. As a leaf
// the rule is the graph itself: a package that seals imports this, and a
// package that links the state log does so because it reads a log.
//
// The leaf is held, not declared: TestTheRecordSealLinksNothingOfTheEngine
// reads what this package links and refuses every path of the module but its
// own. One engine import here is one more graph in everything that seals, and
// no test downstream would name it — the sandbox's refuses the state log, the
// configuration and the code host, not the next package somebody reaches for.
//
// # Why the framework seals and not each domain
//
// The broker's admission is no authority over a record. An embedded fleet's
// broker admits only a connection presenting a key derived from this same
// keyring (ADR-0087), but it decides per connection, and a route's password
// crosses the fleet network in clear — so anything that captured one is a
// member — while an external cluster admits whom its operator chooses. And a
// record on a domain's log is whatever the next node applies: rows every peer
// derives identically, a seat's environment, a person's grants. An unsigned
// record on such a log is not a message, it is an INSTRUCTION anything the
// broker admitted may write.
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
// [internal/runtoken] states the same rule for the credential a sandbox
// carries in a URL path, and encodes it differently for a good reason: a path
// segment needs a digest of the key id, and this frame is bytes, so it carries
// the id itself. Two encodings of one rule, each pointing at the other.
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
package recordseal

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
)

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
	// Verified is a frame whose MAC matches under the key it names.
	Verified Verdict = "verified"

	// KeyUnknown is a frame naming a key this node does not hold. The
	// record is retained and reprocessed when the key arrives.
	KeyUnknown Verdict = "key_unknown"

	// Tampered is a frame naming a key this node holds whose MAC does not
	// match under it, or bytes that are not a frame at all. Permanent.
	Tampered Verdict = "tampered"
)

// Valid reports whether v is one of the three answers.
func (v Verdict) Valid() bool {
	return v == Verified || v == KeyUnknown || v == Tampered
}

// ErrUnsigned is what a nil signer or verifier refuses with, so a deployment
// that reaches a domain with no keyring is told what to configure rather than
// running one log signed and another not.
var ErrUnsigned = errors.New("recordseal: no keyring")

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

// recordKeyLabel separates a record key from every other key derived from the
// same material: it is what stops a record MAC and a per-run token MAC over
// one keyring from ever being the same bytes.
//
// IT STILL SAYS "statelog", and must: the label is inside every key a record
// was ever signed under, so it is part of every signature on every log a
// fleet holds. It names where the frame was first defined, not where it lives;
// changing it to match the package would re-key the fleet and refuse every
// record already written as [Tampered].
const recordKeyLabel = "statelog/record|"

// deriveRecordKey binds the domain and the id in beside the material, so that
// one domain's record can never verify on another's log and a key moved to a
// new id is a new key rather than the same one wearing a new name.
func deriveRecordKey(domain, id, material string) []byte {
	sum := sha256.Sum256([]byte(recordKeyLabel + domain + "|" + id + "|" + material))
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

// ClaimedKeyID is the key id a frame names, or empty when the bytes are not a
// frame. It authenticates nothing: it is what a refusal REPORTS, and the id is
// whatever the writer put there.
func ClaimedKeyID(framed []byte) string {
	id, _, _, ok := splitFrame(framed)
	if !ok {
		return ""
	}
	return id
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

// OneKey is a keyring with a single key, which is what a deployment that has
// run `crewlet secrets keygen` once holds and what a test wants: the rotation
// arms are exercised by keyrings that actually carry two.
func OneKey(id, material string) Keyring {
	return Keyring{ActiveID: id, Keys: []Key{{ID: id, Material: material}}}
}
