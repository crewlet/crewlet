package runtoken_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/secrets"
)

// material is a fixed 32-byte key, so two calls with the same fill are two
// nodes holding the same key.
func material(fill byte) []byte { return bytes.Repeat([]byte{fill}, 32) }

// ring builds a keyring whose active key is active, from key id to fill.
func ring(active string, keys map[string]byte) secrets.Keyring {
	out := secrets.Keyring{ActiveID: active, Keys: map[string][]byte{}}
	for id, fill := range keys {
		out.Keys[id] = material(fill)
	}
	return out
}

func signer(t *testing.T, opts runtoken.Options) *runtoken.Signer {
	t.Helper()
	s, err := runtoken.New(opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

// field is how a token spells one of its fields, for the cases that forge one.
var field = base64.RawURLEncoding

// A ROLLING KEYRING ROTATION NEVER SPLITS THE FLEET.
//
// The new key is added on every node, made active, and the old one dropped,
// one restart at a time — so at every step two neighbouring rings are live at
// once, and a token minted on either must verify on the other, in both
// directions: minted before a node moved on and verified after it, and minted
// after and verified by a node that has not moved yet. A key derived from the
// whole ring failed the very first step both ways, since adding a key changed
// it; minting under anything but the active key, or verifying under the active
// key alone, fails a later one.
func TestARollingKeyringRotationNeverSplitsTheFleet(t *testing.T) {
	t.Parallel()
	steps := []struct {
		name string
		keys secrets.Keyring
	}{
		{"before the rotation", ring("2026.q3", map[string]byte{"2026.q3": 1})},
		{"the new key added", ring("2026.q3", map[string]byte{"2026.q3": 1, "2026.q4": 2})},
		{"the new key activated", ring("2026.q4", map[string]byte{"2026.q3": 1, "2026.q4": 2})},
		{"the old key dropped", ring("2026.q4", map[string]byte{"2026.q4": 2})},
	}
	for i := 0; i+1 < len(steps); i++ {
		behind, ahead := steps[i], steps[i+1]
		slow := signer(t, runtoken.Options{Domain: "test", Keys: behind.keys})
		fast := signer(t, runtoken.Options{Domain: "test", Keys: ahead.keys})
		if got := fast.Validate(slow.Mint("run-1", time.Hour)); got != "run-1" {
			t.Errorf("a token minted %s did not verify %s: got %q", behind.name, ahead.name, got)
		}
		if got := slow.Validate(fast.Mint("run-2", time.Hour)); got != "run-2" {
			t.Errorf("a token minted %s did not verify on a node still %s: got %q",
				ahead.name, behind.name, got)
		}
	}
}

// A TOKEN SIGNED UNDER A KEY THIS NODE DOES NOT HOLD IS REFUSED, which is the
// other half of verifying under any key: "any" means any key the node holds.
//
// The two cases a rotation can produce are a node that activated the new key
// before every node held it — the order the rotation is documented to avoid —
// and a token minted before the old key was dropped, which dropping the key
// retires.
func TestATokenUnderAKeyThisNodeDoesNotHoldIsRefused(t *testing.T) {
	t.Parallel()
	old := signer(t, runtoken.Options{Domain: "test", Keys: ring("k1", map[string]byte{"k1": 1})})
	activated := signer(t, runtoken.Options{
		Domain: "test", Keys: ring("k2", map[string]byte{"k1": 1, "k2": 2}),
	})
	dropped := signer(t, runtoken.Options{Domain: "test", Keys: ring("k2", map[string]byte{"k2": 2})})

	if got := old.Validate(activated.Mint("run", time.Hour)); got != "" {
		t.Errorf("a node without the new key verified a token signed under it as %q", got)
	}
	if got := dropped.Validate(old.Mint("run", time.Hour)); got != "" {
		t.Errorf("a node that dropped the old key verified a token signed under it as %q", got)
	}
}

// ONE KEY ID OVER DIFFERENT MATERIAL IS A DIFFERENT KEY. The token names the
// key by its id, and a node whose keyring spells that id over other bytes — a
// fault of its own — must refuse at the signature rather than trust the name.
func TestAKeyIdOverDifferentMaterialIsRefused(t *testing.T) {
	t.Parallel()
	mine := signer(t, runtoken.Options{Domain: "test", Keys: ring("k1", map[string]byte{"k1": 1})})
	theirs := signer(t, runtoken.Options{Domain: "test", Keys: ring("k1", map[string]byte{"k1": 9})})
	if got := theirs.Validate(mine.Mint("run", time.Hour)); got != "" {
		t.Errorf("a token verified under another key of the same id as %q", got)
	}
}

// ONE ENDPOINT'S TOKEN NEVER VALIDATES AT ANOTHER, whatever keyring the two
// share: a telemetry token turning into a tool-call token is exactly the
// escalation a per-run credential exists to bound.
func TestADomainKeysItsTokensApartFromEveryOtherEndpoint(t *testing.T) {
	t.Parallel()
	keys := ring("k1", map[string]byte{"k1": 1})
	telemetry := signer(t, runtoken.Options{Domain: "crewlet.otlp.v1", Keys: keys})
	bridge := signer(t, runtoken.Options{Domain: "crewlet.mcp.v1", Keys: keys})
	if got := bridge.Validate(telemetry.Mint("run", time.Hour)); got != "" {
		t.Errorf("a telemetry token validated at the bridge as %q", got)
	}
	if got := telemetry.Validate(bridge.Mint("run", time.Hour)); got != "" {
		t.Errorf("a bridge token validated at the receiver as %q", got)
	}
}

// THE KEYRING KEY ITSELF NEVER SIGNS. It seals secrets with AES-GCM, and a
// signature under it would be the same key used by a second algorithm.
func TestTheKeyringKeyIsNeverTheSigningKey(t *testing.T) {
	t.Parallel()
	token := signer(t, runtoken.Options{Domain: "test", Keys: ring("k1", map[string]byte{"k1": 1})}).
		Mint("run", time.Hour)
	cut := strings.LastIndex(token, ".")
	raw := hmac.New(sha256.New, material(1))
	raw.Write([]byte(token[:cut]))
	if token[cut+1:] == field.EncodeToString(raw.Sum(nil)) {
		t.Error("the token is signed with the keyring key itself, not a key derived for its domain")
	}
}

// A SIGNER WITH NO KEYRING SIGNS UNDER A KEY NO OTHER PROCESS HOLDS: its own
// tokens verify, and another such signer's do not, which is the documented
// cost of running without a keyring rather than a deterministic key anybody
// could reproduce.
func TestNoKeyringSignsUnderAKeyOfThisProcessAlone(t *testing.T) {
	t.Parallel()
	one := signer(t, runtoken.Options{Domain: "test"})
	two := signer(t, runtoken.Options{Domain: "test"})
	token := one.Mint("run", time.Hour)
	if got := one.Validate(token); got != "run" {
		t.Errorf("a per-process signer refused its own token: got %q", got)
	}
	if got := two.Validate(token); got != "" {
		t.Errorf("another per-process signer verified the token as %q", got)
	}
	keyed := signer(t, runtoken.Options{Domain: "test", Keys: ring("k1", map[string]byte{"k1": 1})})
	if got := keyed.Validate(token); got != "" {
		t.Errorf("a keyed signer verified a per-process token as %q", got)
	}
}

// A KEYRING THAT CANNOT SAY WHICH KEY MINTS IS REFUSED, and so is a signer for
// no endpoint: each would otherwise mint tokens nothing can verify, or tokens
// every endpoint accepts.
func TestASignerThatCouldNotMintSafelyIsRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		opts runtoken.Options
	}{
		{"no domain", runtoken.Options{Keys: ring("k1", map[string]byte{"k1": 1})}},
		{"an active key the ring does not hold", runtoken.Options{
			Domain: "test", Keys: ring("k9", map[string]byte{"k1": 1}),
		}},
		{"keys with no active one", runtoken.Options{
			Domain: "test", Keys: ring("", map[string]byte{"k1": 1}),
		}},
		{"an active key and no keys", runtoken.Options{
			Domain: "test", Keys: secrets.Keyring{ActiveID: "k1"},
		}},
		{"a key of the wrong size", runtoken.Options{
			Domain: "test", Keys: secrets.Keyring{ActiveID: "k1", Keys: map[string][]byte{"k1": []byte("short")}},
		}},
	} {
		if _, err := runtoken.New(tc.opts); err == nil {
			t.Errorf("%s: New accepted it", tc.name)
		}
	}
}

// ANY SUBJECT ROUND-TRIPS, AND EVERY TOKEN IS ONE PATH SEGMENT. The subject is
// whatever the endpoint's caller chose and the key id whatever the operator
// named the key — Tier A allows a "." in one — and a "." among them would split
// the token's fields, while a "#", "/" or "?" would cut the path a client
// sends.
func TestAnySubjectRoundTripsAsOnePathSegment(t *testing.T) {
	t.Parallel()
	s := signer(t, runtoken.Options{Domain: "test", Keys: ring("key.2026", map[string]byte{"key.2026": 1})})
	for _, subject := range []string{
		"trace-abc",
		"9f2c~0190f6a4-7d1e-7c3a-9b1e-2f0c4d5e6a7b",
		"a.b.c",
		"fragment#and/path?query=1&x",
		"spaces and ünïcödé",
	} {
		token := s.Mint(subject, time.Hour)
		if got := s.Validate(token); got != subject {
			t.Errorf("subject %q validated as %q", subject, got)
		}
		if url.PathEscape(token) != token {
			t.Errorf("subject %q minted %q, which a path segment escapes", subject, token)
		}
	}
}

// A FORGED, TAMPERED, MALFORMED OR EXPIRED TOKEN IS REFUSED — and the caller
// cannot tell which, because telling it tells an attacker the same.
func TestATokenIsRefusedEveryWayItCanBeWrong(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	clock := at
	keys := ring("k1", map[string]byte{"k1": 1, "k2": 2})
	s := signer(t, runtoken.Options{Domain: "test", Keys: keys, Now: func() time.Time { return clock }})
	good := s.Mint("sre-lead", 10*time.Minute)
	if s.Validate(good) != "sre-lead" {
		t.Fatal("a fresh token did not validate")
	}
	parts := strings.Split(good, ".")
	with := func(i int, value string) string {
		forged := append([]string(nil), parts...)
		forged[i] = value
		return strings.Join(forged, ".")
	}

	for _, tc := range []struct{ name, token string }{
		{"empty", ""},
		{"not a token at all", "hello"},
		{"too few fields", strings.Join(parts[:4], ".")},
		{"too many fields", good + ".extra"},
		{"an unknown version", with(0, "v2")},
		// Re-pointed at another key this node holds: the signature was
		// made under the key the token named, so it names that key or
		// none.
		{"another key's id", with(1, field.EncodeToString([]byte("k2")))},
		{"a key id this node does not hold", with(1, field.EncodeToString([]byte("k9")))},
		{"another subject", with(2, field.EncodeToString([]byte("other-seat")))},
		{"a later expiry", with(3, strconv.FormatInt(at.Add(time.Hour).Unix(), 10))},
		{"a truncated signature", good[:len(good)-4]},
		{"a forged signature", with(4, field.EncodeToString(bytes.Repeat([]byte{7}, 32)))},
	} {
		if got := s.Validate(tc.token); got != "" {
			t.Errorf("%s validated as %q", tc.name, got)
		}
	}

	// EXPIRY RIDES IN THE TOKEN, so nothing is reaped and a restart does
	// not invalidate a live run's endpoint.
	clock = at.Add(10 * time.Minute)
	if got := s.Validate(good); got != "" {
		t.Errorf("a token at its expiry validated as %q", got)
	}
}

// A MINT IS NEVER SHORTER THAN A SECOND. A token that expired before it was
// handed over is a run that fails on its first call.
func TestAMintOutlivesItsOwnHandover(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)
	s := signer(t, runtoken.Options{Domain: "test", Now: func() time.Time { return at }})
	if got := s.Validate(s.Mint("run", 0)); got != "run" {
		t.Errorf("a token minted with no TTL validated as %q at the instant it was minted", got)
	}
}
