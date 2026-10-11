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
// # Minted under the active key, verified under any
//
// The key is the fleet's Tier A keyring ([secrets.Keyring]), the one secret
// every node already loads, and a keyring ROTATES ROLLING: the new key is
// added on every node, made active, and the old one dropped, one restart at a
// time. So for the length of a rotation two live nodes hold different rings —
// one has the new key and the other has not received it yet, or one has
// dropped the old key and the other still mints under it.
//
// A token therefore NAMES THE KEY THAT SIGNED IT, the move the sealed envelope
// makes for the same reason (internal/secrets): it is minted under the ACTIVE
// key and verified under whichever key it names, if this node holds that key.
// A node that has added a key it has not activated mints under the old one,
// which every node holds, and a node that has activated the new key still
// verifies everything minted under the old one until the old one is dropped.
// The only token a node refuses mid-rotation is one signed under a key it
// does not hold, which is the order a rotation is documented to avoid: add
// the key everywhere before any node activates it.
//
// One key derived from the WHOLE ring is what this was, and it is the shape
// that cannot work: it changes the moment one node adds a key, so every token
// that crossed between a node that had restarted and one that had not was
// forged as far as the other was concerned — in both directions, for every
// endpoint at once, for as long as the rotation took.
//
// # A key per domain, never the keyring key itself
//
// Every signer names its DOMAIN — the endpoint its tokens are for — and signs
// under a key derived from the keyring key and that domain (HKDF-SHA-256), the
// derivation the keyring's fingerprints use. The domain is what stops one
// endpoint's token validating at another: without it a token minted for the
// telemetry receiver would validate at the tool bridge, since both would be
// HMACs over the same key and the subject is just a string. And the keyring
// key itself is never used, because it seals secrets with AES-GCM, and one key
// used by two algorithms is a key whose every use has to be proven safe
// against the other.
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
	"crypto/hkdf"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/secrets"
)

// version prefixes every token, so a future format can be told from this one
// rather than failing as a bad signature.
const version = "v1"

// minTTL floors a mint. A token that expired before it was handed over is a
// run that fails on its first call for a reason nothing in the config
// explains.
const minTTL = time.Second

// keyBytes is the size of a domain's signing key: SHA-256's own, the most an
// HMAC-SHA-256 key buys before it is hashed down to that anyway.
const keyBytes = sha256.Size

// processKeyID names the key a signer with no keyring generates for itself.
//
// Any name would do, because the key behind it exists in this process alone: a
// peer either holds no key by this name, and refuses at the lookup, or holds
// its own, and refuses at the signature. It is named rather than random so a
// token minted by a node with no keyring says so to whoever decodes it.
const processKeyID = "process"

// field encodes one variable field of a token.
//
// URL-SAFE and unpadded, because the token is a path segment: standard
// base64's `+` and `/` would need escaping, and a `=` in a path is legal but
// normalised differently by enough proxies to matter. And EVERY variable field
// goes through it — the key id and the subject as well as the signature —
// because "." delimits the fields and a keyring key id may contain one, while
// a subject is whatever its caller chose: encoded, neither can split the
// token, and neither can carry a "#" or a "/" that a client would cut the path
// at.
var field = base64.RawURLEncoding

// Signer mints and validates tokens.
//
// SAFE FOR CONCURRENT USE, and holds no state to protect: a token is a
// function of the keys, the subject and the clock.
type Signer struct {
	// active is the encoded id of the key a mint signs under.
	active string
	// keys is every key a token may name, by encoded id, each already
	// derived for this signer's domain.
	keys map[string][]byte
	now  func() time.Time
}

// Options configure [New].
type Options struct {
	// Domain names the endpoint these tokens are for, and keys them apart
	// from every other endpoint's. Required.
	Domain string

	// Keys is the keyring tokens are signed under: the ACTIVE key mints and
	// every key verifies. It MUST be the fleet's own — the Tier A keyring,
	// as config.Secrets.Keyring decodes it — because one node mints and
	// another may verify.
	//
	// THE ZERO KEYRING takes a random per-process key, which is correct for
	// a single process and cannot work across two: the honest answer for a
	// deployment with no keyring, whose caller logs what it costs, because
	// only the caller knows which endpoint is about to be unverifiable. A
	// keyring with keys and no usable active one is refused.
	Keys secrets.Keyring

	// Now is the clock. Nil takes wall-clock time.
	//
	// WALL CLOCK, NOT MONOTONIC, because the expiry travels between
	// processes and a monotonic reading's epoch is per-boot — meaningless
	// anywhere but where it was taken.
	Now func() time.Time
}

// New builds a signer.
func New(opts Options) (*Signer, error) {
	if opts.Domain == "" {
		return nil, errors.New("runtoken: a signer needs the domain its tokens are for")
	}
	ring := opts.Keys
	if ring.ActiveID == "" && len(ring.Keys) == 0 {
		ring = processKeyring()
	}
	if err := ring.Validate(); err != nil {
		return nil, fmt.Errorf("runtoken: the %s signing keyring: %w", opts.Domain, err)
	}
	keys := make(map[string][]byte, len(ring.Keys))
	for id, material := range ring.Keys {
		key, err := hkdf.Key(sha256.New, material, nil, "crewlet/runtoken/"+opts.Domain, keyBytes)
		if err != nil {
			return nil, fmt.Errorf("runtoken: derive the %s key from %q: %w", opts.Domain, id, err)
		}
		keys[field.EncodeToString([]byte(id))] = key
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Signer{active: field.EncodeToString([]byte(ring.ActiveID)), keys: keys, now: now}, nil
}

// processKeyring is the one-key ring a signer with no keyring signs under.
func processKeyring() secrets.Keyring {
	key := make([]byte, keyBytes)
	// A read from crypto/rand cannot fail on any platform this runs on, and
	// a key that silently stayed zero would make every token forgeable — so
	// the error is not ignored, it is impossible.
	if _, err := rand.Read(key); err != nil {
		panic("runtoken: no randomness for the signing key: " + err.Error())
	}
	return secrets.Keyring{ActiveID: processKeyID, Keys: map[string][]byte{processKeyID: key}}
}

// Mint returns a token scoped to one subject, valid for ttl, signed under the
// active key.
//
// The SUBJECT is whatever the endpoint needs to know about the caller and
// nothing more: a trace id for the telemetry receiver, a session for the tool
// bridge. It is not secret — it travels in a URL, and the encoding that keeps
// it a single path segment hides nothing — so it must never be a value that
// grants anything on its own. An empty subject validates as nothing.
func (s *Signer) Mint(subject string, ttl time.Duration) string {
	if ttl < minTTL {
		ttl = minTTL
	}
	payload := version + "." + s.active + "." + field.EncodeToString([]byte(subject)) + "." +
		strconv.FormatInt(s.now().Add(ttl).Unix(), 10)
	return payload + "." + sign(s.keys[s.active], payload)
}

// Validate returns the token's subject, or empty for one that is forged,
// malformed, expired or signed under a key this node does not hold.
//
// A MANY-WAY ANSWER COLLAPSED TO TWO ON PURPOSE. The caller's only move for
// any of them is to refuse the request, and telling a caller which of its
// tokens was wrong tells an attacker the same.
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
	if !hmac.Equal([]byte(parts[4]), []byte(sign(key, payload))) {
		return ""
	}
	expiry, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil || !s.now().Before(time.Unix(expiry, 0)) {
		return ""
	}
	subject, err := field.DecodeString(parts[2])
	if err != nil {
		return ""
	}
	return string(subject)
}

// sign is the payload's HMAC-SHA-256 under key, as a token field.
func sign(key []byte, payload string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(payload))
	return field.EncodeToString(mac.Sum(nil))
}
