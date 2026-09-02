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
// Minting and verifying happen in DIFFERENT PROCESSES whenever the API runs on
// its own host: the engine mints when it starts a run, the API verifies when
// the box calls back. An in-memory store makes those the same process by
// assumption, and the documented split deployment then refuses every request
// from every run — visible only as retry noise inside a sandbox nobody is
// watching. Expiry rides in the token too, so nothing is reaped and a restart
// does not invalidate a live run's endpoint.
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
	"slices"
	"strconv"
	"strings"
	"time"
)

// version prefixes every token, so a future format can be told from this one
// rather than failing as a bad signature.
const version = "v1"

// minTTL floors a mint. A token that expired before it was handed over is a
// run that fails on its first call for a reason nothing in the config
// explains.
const minTTL = time.Second

// Signer mints and validates tokens.
//
// SAFE FOR CONCURRENT USE, and holds no state to protect: a token is a
// function of the key, the subject and the clock.
type Signer struct {
	key []byte
	now func() time.Time
}

// Options configure [New].
type Options struct {
	// Key signs every token and MUST be the same in every process that
	// mints or verifies one. Empty takes a random per-process key, which
	// is correct for a single process and cannot work across two.
	Key []byte

	// Now is the clock. Nil takes wall-clock time.
	//
	// WALL CLOCK, NOT MONOTONIC, because the expiry travels between
	// processes and a monotonic reading's epoch is per-boot — meaningless
	// anywhere but where it was taken.
	Now func() time.Time
}

// New builds a signer.
func New(opts Options) *Signer {
	key := opts.Key
	if len(key) == 0 {
		key = make([]byte, 32)
		// A read from crypto/rand cannot fail on any platform this runs
		// on, and a key that silently stayed zero would make every token
		// forgeable — so the error is not ignored, it is impossible.
		if _, err := rand.Read(key); err != nil {
			panic("runtoken: no randomness for the signing key: " + err.Error())
		}
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &Signer{key: key, now: now}
}

// Mint returns a token scoped to one subject, valid for ttl.
//
// The SUBJECT is whatever the endpoint needs to know about the caller and
// nothing more: a trace id for the telemetry receiver, a run id for the tool
// bridge. It is not secret — it travels in a URL — so it must never be a
// value that grants anything on its own.
func (s *Signer) Mint(subject string, ttl time.Duration) string {
	if ttl < minTTL {
		ttl = minTTL
	}
	payload := version + "." + subject + "." +
		strconv.FormatInt(s.now().Add(ttl).Unix(), 10)
	return payload + "." + s.sign(payload)
}

// Validate returns the token's subject, or empty for one that is forged,
// malformed or expired.
//
// A THREE-WAY ANSWER COLLAPSED TO TWO ON PURPOSE. The caller's only move for
// any of them is to refuse the request, and telling a caller which of its
// tokens was wrong tells an attacker the same.
func (s *Signer) Validate(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 4 || parts[0] != version {
		return ""
	}
	payload := strings.Join(parts[:3], ".")
	// CONSTANT TIME. A byte-by-byte compare leaks the signature one byte
	// at a time to anyone who can time the endpoint, and these endpoints
	// are deliberately reachable without other credentials.
	if !hmac.Equal([]byte(parts[3]), []byte(s.sign(payload))) {
		return ""
	}
	expiry, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || !s.now().Before(time.Unix(expiry, 0)) {
		return ""
	}
	return parts[1]
}

func (s *Signer) sign(payload string) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte(payload))
	// URL-SAFE and unpadded, because the token is a path segment: standard
	// base64's `+` and `/` would need escaping, and a `=` in a path is
	// legal but normalised differently by enough proxies to matter.
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// KeyFrom derives a signing key from the fleet's own key material.
//
// The point is that TWO PROCESSES DERIVE THE SAME KEY. In a split deployment
// the engine mints and the API verifies, so a per-process random key means
// every token is forged as far as the verifier is concerned — visible only as
// a run whose telemetry and tool calls all fail, with nothing in the config
// looking wrong.
//
// The DOMAIN separates one endpoint's tokens from another's. Without it a
// token minted for the telemetry receiver would validate at the tool bridge:
// both are HMACs over the same key, and the subject is just a string. It is
// the same reason a signing key is never reused across protocols.
//
// Empty material yields nil, which [New] turns into a per-process key — the
// honest answer for a deployment with no keyring, and correct for a single
// process. The caller logs what that costs, because only the caller knows
// which endpoint is about to be unverifiable.
func KeyFrom(domain string, material []string) []byte {
	if len(material) == 0 {
		return nil
	}
	// SORTED, so two processes reading the same keyring in a different
	// order derive the same key — a map iteration or a re-ordered config
	// would otherwise silently split a fleet in two.
	ordered := slices.Sorted(slices.Values(material))
	sum := sha256.Sum256([]byte(domain + "|" + strings.Join(ordered, "")))
	return sum[:]
}
