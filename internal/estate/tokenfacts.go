package estate

import (
	"crypto/hmac"
	"crypto/sha256"
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A MACHINE TOKEN'S ROW, AS IT CROSSES TO A NODE WITHOUT THE DIRECTORY.
//
// # Why it crosses at all
//
// A node without the `data` role holds no identity rows (ADR-0025), and a node
// that serves requests anyway — a git node answering persons' clones and
// pushes, whose password is their machine token (`cwl_pat_…`) — still has to
// check the token. So the row a token is checked against (the credential, its
// owner as the directory holds them now, and how far the answering node can
// vouch for both) is asked of a data node, floored on the identity log like a
// login's binding ([opIdentityBinding]).
//
// # Why the verifier does not, and what crosses instead
//
// The asker decides on everything else in the row — the grants the mint
// carried and the owner's current ones are what the token acts with, the
// expiry, the revocation and the epochs are what end it, the applied position
// is what makes an absent row three-valued — so the row crosses whole but for
// its VERIFIER, the SHA-256 a presented secret is checked against. A verifier
// on the broker is a value the asker has no use for and an observer would, for
// the reason a binding withholds a password's ([Binding]); and the secret it is
// checked against never leaves the asking node either, since that is the one
// value that authenticates anything.
//
// What crosses in the verifier's place is a MAC over it and a nonce the asker
// drew for this question alone, under a key both nodes derive from the fleet
// keyring in a domain of its own ([verifierDomain]). The asker holds the
// presented token, so it computes the verifier that token implies, MACs it
// over the same nonce, and takes it as the row's verifier only where the two
// MACs agree ([TokenFacts.rowFor]). So [credential.CheckToken] stays the ONE
// table, deciding on the asker exactly what it decides on a data node: the
// verifier it reads there is the stored one wherever the stored one is the
// presented token's, and empty — which no secret verifies against — wherever
// it is not. A MAC is worthless to whoever lacks the keyring, so nothing on the
// broker can test a guess against it; and an answer MAC'd for one question
// verifies under no other question's nonce.
//
// # The alternatives, and why not
//
// Sending the secret to the data node to check there would put the one value
// that authenticates anything on the broker in the clear — envelopes are
// signed, not sealed. Sending the verifier would put the value a guess is
// tested against there instead. And a verdict formed on the data node — "valid"
// — would be the second opinion [IdentityReader] refuses everywhere: the chart
// a bound owner's seat is resolved against, the admin posture the grants are
// clamped by and the stall a vouch is judged against are the asking node's.

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
// THE ASKER NAMES THE KEY, as an envelope's frame names the key that signed it,
// so a keyring rotation is survivable in both directions: a node asks under
// its active key, and a node that does not hold it runs nothing and the router
// asks one that does ([opIdentityMachineToken]). Derived from the whole
// keyring instead, every node that had loaded a different key SET would derive
// a different key, and refuse every token presented through a peer for the
// length of the rollout.
type verifierKeys struct {
	active string
	keys   map[string][]byte
}

// newVerifierKeys derives every key of ring in [verifierDomain]. The ring is
// one [NewEnvelopes] has already held to a usable shape — an id on every
// entry and an active one among them.
func newVerifierKeys(ring statelog.Keyring) verifierKeys {
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
// the nonce this question drew.
//
// A VERIFIER THE WIRE CARRIED IS NEVER BELIEVED, whatever a peer sent: the one
// this sets is the presented token's own or none, so a build that forgot to
// narrow its answer could put a verifier on the broker but never decide a
// token here.
//
// IN CONSTANT TIME ([hmac.Equal]), and computed for every answer — a row of
// another method, or none at all — so how long a refusal takes says nothing
// about what the id names, [credential.CheckToken]'s own rule.
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
