package estate

import (
	"crypto/hmac"
	"crypto/sha256"
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/runtoken"
)

// A MACHINE TOKEN'S ROW, AS IT CROSSES TO A NODE WITHOUT THE DIRECTORY (a
// Gitlet node checking a `cwl_pat_…` push password), floored on the identity
// log like [opIdentityBinding] (package doc, "What does not cross the wire,
// and who supplies it"): whole but for its verifier, replaced by a MAC over it
// and a per-question nonce under a key derived from the keyring
// ([verifierDomain]). The asker takes the verifier its presented token implies
// only where the MACs agree ([TokenFacts.rowFor]), so [credential.CheckToken]
// stays the ONE table. Not the alternatives: the secret would cross the broker
// in the clear (envelopes are signed, not sealed); the verifier is what a guess
// is tested against; and a verdict formed there would be the second opinion
// [IdentityReader] refuses, since the chart, the admin posture and the stall a
// vouch is judged against are the asking node's.

// verifierDomain is the key domain a token verifier's MAC is derived in
// ([runtoken.DeriveKey]): its own, so no per-run token, cookie, view ticket or
// envelope this fleet's keyring signs ever shares a key with it, and a MAC
// minted for one could never be presented as another.
const verifierDomain = "crewlet.identity.verifier.v1"

// nonceBytes is the nonce a machine-token question carries: sixteen bytes of
// crypto/rand, so no two questions a fleet asks in its life share one — at a
// billion questions a second, a collision is expected after some six hundred
// years.
const nonceBytes = 16

// TokenFacts is a machine token's row as a data node answers a node without
// the directory: the row with its verifier withheld, and the MAC that stands
// in for it — see the file comment.
type TokenFacts struct {
	// Row is everything [credential.CheckToken] reads, as the answering
	// node holds it — its Verifier EMPTY, always ([factsOf]).
	Row credential.TokenRow

	// VerifierMAC is HMAC-SHA256 over the asker's nonce and the stored
	// verifier, under the key the asker named ([verifierMAC]).
	VerifierMAC []byte
}

// machineTokenArgs is a machine-token question: the credential's id, the nonce
// this question alone carries, and the keyring entry the answer is MAC'd
// under.
//
// NEVER THE SECRET, which stays with the presented token on the asking node.
type machineTokenArgs struct {
	ID    string
	Nonce [nonceBytes]byte
	KeyID string
}

// verifierKeys are the keys a verifier's MAC is derived under, one per keyring
// entry, and the entry this node asks under: its active one.
//
// THE ASKER NAMES THE KEY, as an envelope's frame does, so a rotation survives
// both ways: a node that does not hold it runs nothing and the router asks one
// that does ([opIdentityMachineToken]). A key derived from the whole keyring
// would differ between nodes holding different key SETS, refusing every token
// presented through a peer for the length of the rollout.
type verifierKeys struct {
	active string
	keys   map[string][]byte
}

// newVerifierKeys derives every key of ring in [verifierDomain]. The ring is
// one [NewEnvelopes] has already held to a usable shape — an id on every
// entry and an active one among them.
func newVerifierKeys(ring recordseal.Keyring) verifierKeys {
	keys := make(map[string][]byte, len(ring.Keys))
	for _, key := range ring.Keys {
		keys[key.ID] = runtoken.DeriveKey(verifierDomain, key.ID, key.Material)
	}
	return verifierKeys{active: ring.ActiveID, keys: keys}
}

// key is the key the entry id derives, or false for an entry this node does
// not hold.
func (v verifierKeys) key(id string) ([]byte, bool) {
	k, held := v.keys[id]
	return k, held
}

// asking is the entry this node asks under — its active one — and its key, or
// false where it holds none.
func (v verifierKeys) asking() (string, []byte, bool) {
	k, held := v.keys[v.active]
	return v.active, k, held
}

// held is every entry this node holds, sorted, for a refusal that names them.
func (v verifierKeys) held() []string {
	return slices.Sorted(maps.Keys(v.keys))
}

// verifierMAC is HMAC-SHA256 under key over the nonce and then the verifier.
// The nonce is fixed-length, so no other (nonce, verifier) pair writes the same
// bytes.
func verifierMAC(key []byte, nonce [nonceBytes]byte, verifier string) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(nonce[:])
	mac.Write([]byte(verifier))
	return mac.Sum(nil)
}

// factsOf narrows a row for the wire: its verifier MAC'd over the asker's
// nonce under key, and withheld.
//
// MAC'D FOR EVERY ROW, one of another method or none at all included, so the
// answer's shape says nothing the asker's check would not.
func factsOf(row credential.TokenRow, key []byte, nonce [nonceBytes]byte) TokenFacts {
	mac := verifierMAC(key, nonce, row.Verifier)
	row.Verifier = ""
	return TokenFacts{Row: row, VerifierMAC: mac}
}

// rowFor is the row presented is checked against on the asking node: the
// facts' row, with the verifier presented implies set only where the answering
// node's MAC over the stored verifier is the MAC over that one, under key and
// this question's nonce.
//
// A VERIFIER THE WIRE CARRIED IS NEVER BELIEVED, so a build that forgot to
// narrow its answer leaks a verifier but never decides a token here. IN
// CONSTANT TIME ([hmac.Equal]) and computed for every answer, so a refusal's
// timing says nothing about what the id names ([credential.CheckToken]'s rule).
func (f TokenFacts) rowFor(presented credential.Token, key []byte,
	nonce [nonceBytes]byte) credential.TokenRow {

	row := f.Row
	row.Verifier = ""
	implied := credential.TokenVerifier(presented.ID, presented.Secret)
	if hmac.Equal(f.VerifierMAC, verifierMAC(key, nonce, implied)) {
		row.Verifier = implied
	}
	return row
}
