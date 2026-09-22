// Package runtoken is the signed, self-describing credential a per-run
// endpoint carries in its own URL path.
//
// # Why a token in the path at all
//
// Two edges of this engine are reachable by something running INSIDE a
// sandbox: the OTLP receiver a coding agent exports telemetry to, and the MCP
// bridge it calls the seat's tools through. Neither can be given the API's own
// token — that credential reads the whole company, and handing it to a box
// running generated code is the failure the sandbox exists to prevent. So what
// authenticates a request there is a credential that is worth nothing outside
// one run and expires with it, and it rides in the path because the clients
// are a vendor's OTLP exporter and a vendor's MCP client: neither takes an
// arbitrary header the engine chooses.
//
// # Signed and self-describing, not a key into a map
//
// Minting and verifying happen on DIFFERENT NODES of a fleet: the node running
// the seat mints when it starts a run, and the node the box reaches verifies
// when it calls back, which for telemetry is whichever node the box can see.
// An in-memory store makes those the same process by assumption, and a fleet
// then refuses every request that reached any other node, from every run,
// visible only as retry noise inside a sandbox nobody is watching. Expiry
// rides in the token too, so nothing is reaped and a restart does not
// invalidate a live run's endpoint.
//
// # The token names the key that signed it
//
// Every key in the fleet's keyring derives its own signing key, and the token
// carries a TAG naming which one it was signed under, exactly as the secret
// store's envelope carries the id of the key that sealed it. A verifier looks
// the tag up; a tag it does not hold is refused, never quietly retried against
// the active key.
//
// That is what makes a keyring rotation survivable. Derived from the whole
// keyring instead — which is what this did — every process that had loaded a
// different key SET derived a different key, so the moment an operator added
// k2 the nodes that had restarted refused every token minted by the nodes that
// had not, in both directions, for the length of the rollout. Worse, it
// invalidated tokens MID-RUN: a detached coding run's box would keep exporting
// telemetry and calling tools against a credential the fleet no longer
// recognised, with nothing in the config looking wrong. Tagged, the runbook is
// add a key, restart, flip active, restart, drop the old key once the longest
// token lifetime has passed, and nothing in flight ever dies.
//
// The tag is a digest of (domain, key id) rather than the id itself, so the
// grammar constrains nothing an operator may name and a token in a proxy log
// discloses no key ids.
//
// # One implementation, because it was two
//
// This was written once for the OTLP receiver and would have been written a
// second time for the MCP bridge. The two copies would have had to agree about
// the version prefix, the separator, the base64 alphabet, the constant-time
// compare and the clock — five decisions, none of them locally obvious, in two
// files that nothing compares. The same rule the webhook-secret format follows
// (internal/whsec): a grammar that more than one caller must agree on has one
// definition.
package runtoken

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"strconv"
	"strings"
	"time"
)

// version prefixes every token, so a future format can be told from this one
// rather than failing as a bad signature.
const version = "v2"

// minTTL floors a mint. A token that expired before it was handed over is a
// run that fails on its first call for a reason nothing in the config
// explains.
const minTTL = time.Second

// tagLen is how much of the (domain, key id) digest names the key.
//
// Eight base64 characters is 48 bits over a keyring an operator writes by
// hand — a handful of entries, all of them chosen — so a collision is not a
// risk being managed, it is arithmetic. It is short because it rides in a URL
// path beside the subject and the signature.
const tagLen = 8

// ephemeralTag names the per-process key a deployment with no keyring signs
// under. It is a constant rather than a random value so that one process's
// own tokens validate at itself, which is the whole of what an ephemeral key
// can promise.
const ephemeralTag = "ephemeral"

// KeyMaterial is one keyring entry: an id every node agrees on, and the
// material behind it.
//
// THE MATERIAL IS NOT RESOLVED. Tier A's ${VAR} references reach here
// verbatim, because the keyring is read before the secret store that would
// resolve them is open. Two nodes reading the same document derive the same
// key either way: what matters is that they agree, not that this is plaintext.
type KeyMaterial struct {
	ID       string
	Material string
}

// Material is the fleet's keyring as a token issuer sees it.
type Material struct {
	// ActiveID names the key new tokens are signed under. A Material whose
	// ActiveID names no entry is ephemeral, the same as an empty one —
	// Tier A refuses that combination before a node reaches here, and a
	// signer cannot mint under a key nobody named.
	ActiveID string

	// Keys is every key a token may have been signed under, including the
	// active one. A key stays here for as long as a token signed under it
	// could still be presented.
	Keys []KeyMaterial
}

// Usable reports whether this material can sign for the fleet rather than for
// one process. The caller logs what an unusable one costs, because only the
// caller knows which endpoint is about to be unverifiable.
func (m Material) Usable() bool {
	for _, key := range m.Keys {
		if key.ID == m.ActiveID {
			return true
		}
	}
	return false
}

// Signer mints and validates tokens.
//
// SAFE FOR CONCURRENT USE, and holds no mutable state: a token is a function
// of the keyring, the subject and the clock.
type Signer struct {
	activeTag string
	keys      map[string][]byte
	now       func() time.Time
}

// Options configure [New].
type Options struct {
	// Domain separates one endpoint's tokens from another's. Without it a
	// token minted for the telemetry receiver would validate at the tool
	// bridge: both are HMACs over the same fleet keyring, and the subject
	// is just a string. It is the same reason a signing key is never
	// reused across protocols.
	Domain string

	// Material is the fleet's keyring. One that cannot sign for the fleet
	// takes a per-process key, which is correct for a single process and
	// cannot work across two.
	Material Material

	// Now is the clock. Nil takes wall-clock time.
	//
	// WALL CLOCK, NOT MONOTONIC, because the expiry travels between
	// processes and a monotonic reading's epoch is per-boot — meaningless
	// anywhere but where it was taken.
	Now func() time.Time
}

// New builds a signer.
func New(opts Options) *Signer {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	s := &Signer{keys: map[string][]byte{}, now: now}
	for _, key := range opts.Material.Keys {
		tag := KeyTag(opts.Domain, key.ID)
		s.keys[tag] = DeriveKey(opts.Domain, key.ID, key.Material)
		if key.ID == opts.Material.ActiveID {
			s.activeTag = tag
		}
	}
	if s.activeTag == "" {
		// EPHEMERAL, and only itself can verify it. A key that silently
		// stayed zero would make every token forgeable, and a read from
		// crypto/rand cannot fail on any platform this runs on — so the
		// error is not ignored, it is impossible.
		key := make([]byte, 32)
		if _, err := rand.Read(key); err != nil {
			panic("runtoken: no randomness for the signing key: " + err.Error())
		}
		s.activeTag = ephemeralTag
		s.keys[ephemeralTag] = key
	}
	return s
}

// Mint returns a token scoped to one subject, valid for ttl, signed under the
// keyring's active key and naming it.
//
// The SUBJECT is whatever the endpoint needs to know about the caller and
// nothing more: a trace id for the telemetry receiver, a run id for the tool
// bridge. It is not secret — it travels in a URL — so it must never be a
// value that grants anything on its own.
func (s *Signer) Mint(subject string, ttl time.Duration) string {
	if ttl < minTTL {
		ttl = minTTL
	}
	payload := version + "." + s.activeTag + "." + subject + "." +
		strconv.FormatInt(s.now().Add(ttl).Unix(), 10)
	return payload + "." + s.sign(s.keys[s.activeTag], payload)
}

// Validate returns the token's subject, or empty for one that is forged,
// malformed, expired, or signed under a key this node does not hold.
//
// A FOUR-WAY ANSWER COLLAPSED TO ONE ON PURPOSE. The caller's only move for
// any of them is to refuse the request, and telling a caller which of its
// tokens was wrong tells an attacker the same. In particular an unknown tag
// is refused rather than retried against the active key: a token signed under
// a key that has been dropped is exactly as invalid as a forged one, and a
// fallback would make the drop do nothing.
func (s *Signer) Validate(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 5 || parts[0] != version {
		return ""
	}
	key, held := s.keys[parts[1]]
	if !held {
		return ""
	}
	payload := strings.Join(parts[:4], ".")
	// CONSTANT TIME. A byte-by-byte compare leaks the signature one byte
	// at a time to anyone who can time the endpoint, and these endpoints
	// are deliberately reachable without other credentials.
	if !hmac.Equal([]byte(parts[4]), []byte(s.sign(key, payload))) {
		return ""
	}
	expiry, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil || !s.now().Before(time.Unix(expiry, 0)) {
		return ""
	}
	return parts[2]
}

func (s *Signer) sign(key []byte, payload string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	// URL-SAFE and unpadded, because the token is a path segment: standard
	// base64's `+` and `/` would need escaping, and a `=` in a path is
	// legal but normalised differently by enough proxies to matter.
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// KeyTag names one key inside one domain, in a form a URL path or a cookie
// value can carry.
//
// EXPORTED FOR THE SECOND CONSUMER. internal/iam/session mints a bearer of its
// own shape under its own domain, and it must tag the key the same way — a
// second derivation of "which key signed this" would be this package's own
// one-implementation rule broken by the package that states it.
func KeyTag(domain, id string) string {
	sum := sha256.Sum256([]byte("tag|" + domain + "|" + id))
	return base64.RawURLEncoding.EncodeToString(sum[:])[:tagLen]
}

// DeriveKey is one key's signing key for one domain.
//
// The id is bound in beside the material so that two keyring entries that
// somehow carried the same material still sign differently, and so that a key
// moved to another id is a different key rather than the same one wearing a
// new name.
func DeriveKey(domain, id, material string) []byte {
	sum := sha256.Sum256([]byte("key|" + domain + "|" + id + "|" + material))
	return sum[:]
}

// OneKey is a keyring with a single key, which is what a single-node
// deployment has and what a test wants: every token is signed under it and
// tagged with it, so the rotation arms are exercised by keyrings that actually
// carry two.
func OneKey(id, material string) Material {
	return Material{ActiveID: id, Keys: []KeyMaterial{{ID: id, Material: material}}}
}
