package runtoken_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/runtoken"
)

// The package shipped no test file at all, which is how the whole-keyring
// derivation survived: every property below is one a caller assumed and
// nothing checked.

func ring(active string, keys ...runtoken.KeyMaterial) runtoken.Material {
	return runtoken.Material{ActiveID: active, Keys: keys}
}

var (
	k1 = runtoken.KeyMaterial{ID: "k1", Material: "material-one"}
	k2 = runtoken.KeyMaterial{ID: "k2", Material: "material-two"}
	k3 = runtoken.KeyMaterial{ID: "k3", Material: "material-three"}
)

func signer(t *testing.T, domain string, m runtoken.Material) *runtoken.Signer {
	t.Helper()
	return runtoken.New(runtoken.Options{Domain: domain, Material: m})
}

// TestARotationDoesNotInvalidateAnOutstandingToken is the whole point.
//
// A detached coding run holds its endpoint's token for as long as the run
// lasts, which is minutes to hours and outlives a rollout. The token names the
// key that signed it, so adding a key, flipping the active key and restarting
// every node leaves it valid throughout.
func TestARotationDoesNotInvalidateAnOutstandingToken(t *testing.T) {
	t.Parallel()
	const domain = "crewlet.test.v1"
	before := signer(t, domain, ring("k1", k1))
	token := before.Mint("run-abc", time.Hour)

	// Step one: k2 joins the ring, k1 still active. A node that has
	// restarted and one that has not both validate.
	added := signer(t, domain, ring("k1", k1, k2))
	if got := added.Validate(token); got != "run-abc" {
		t.Fatalf("adding a key invalidated a live token: %q", got)
	}

	// Step two: the active key flips to k2, k1 still on the ring.
	flipped := signer(t, domain, ring("k2", k1, k2))
	if got := flipped.Validate(token); got != "run-abc" {
		t.Fatalf("flipping the active key invalidated a live token: %q", got)
	}
	// And the other direction, which is what splits a fleet mid-rollout:
	// a token minted by the flipped node validates at one that has not
	// flipped yet, because k2 is on both rings.
	fresh := flipped.Mint("run-def", time.Hour)
	if got := added.Validate(fresh); got != "run-def" {
		t.Fatalf("a node that had not flipped refused a peer's token: %q", got)
	}

	// Step three: k1 is dropped once nothing signed under it can still be
	// presented. NOW the old token is refused, which is what the drop is
	// for — an unknown tag is never retried against the active key.
	dropped := signer(t, domain, ring("k2", k2))
	if got := dropped.Validate(token); got != "" {
		t.Errorf("a token signed under a dropped key still validated: %q", got)
	}
	if got := dropped.Validate(fresh); got != "run-def" {
		t.Errorf("dropping k1 invalidated a token signed under k2: %q", got)
	}
}

// TestTheKeyringsOrderDoesNotChangeAToken: two processes read the same
// document and nothing promises the same order.
func TestTheKeyringsOrderDoesNotChangeAToken(t *testing.T) {
	t.Parallel()
	const domain = "crewlet.test.v1"
	one := signer(t, domain, ring("k1", k1, k2, k3))
	two := signer(t, domain, ring("k1", k3, k2, k1))
	if got := two.Validate(one.Mint("subject", time.Hour)); got != "subject" {
		t.Errorf("the token depends on the keyring's order: %q", got)
	}
}

// TestADomainSeparatesTheIssuers. Without it a token minted for the telemetry
// receiver would validate at the tool bridge: both are HMACs over the same
// fleet keyring, and the subject is just a string.
func TestADomainSeparatesTheIssuers(t *testing.T) {
	t.Parallel()
	material := ring("k1", k1)
	otlp := signer(t, "crewlet.otlp.v1", material)
	bridge := signer(t, "crewlet.mcp.v1", material)
	token := otlp.Mint("trace-1", time.Hour)
	if got := bridge.Validate(token); got != "" {
		t.Errorf("one issuer's token validated at another: %q", got)
	}
	if got := otlp.Validate(token); got != "trace-1" {
		t.Errorf("an issuer refused its own token: %q", got)
	}
	// The tag differs too, so the two issuers' tokens are not even
	// confusable before the signature is compared.
	if tagOf(otlp.Mint("x", time.Hour)) == tagOf(bridge.Mint("x", time.Hour)) {
		t.Error("two domains produced the same key tag")
	}
}

// TestAnUnknownKeyTagIsRefused: a token naming a key this node does not hold
// is exactly as invalid as a forged one.
func TestAnUnknownKeyTagIsRefused(t *testing.T) {
	t.Parallel()
	const domain = "crewlet.test.v1"
	token := signer(t, domain, ring("k1", k1)).Mint("subject", time.Hour)
	other := signer(t, domain, ring("k2", k2))
	if got := other.Validate(token); got != "" {
		t.Errorf("a token naming an unheld key validated: %q", got)
	}
	// Even when the SAME material sits under a different id: an id is part
	// of the derivation, so a key moved to a new name is a new key.
	renamed := signer(t, domain, ring("kX", runtoken.KeyMaterial{ID: "kX", Material: k1.Material}))
	if got := renamed.Validate(token); got != "" {
		t.Errorf("a key renamed in the config still validated an old token: %q", got)
	}
}

// TestAForgedMalformedOrExpiredTokenIsRefused, and the caller cannot tell
// which, because telling it tells an attacker the same.
func TestAForgedMalformedOrExpiredTokenIsRefused(t *testing.T) {
	t.Parallel()
	const domain = "crewlet.test.v1"
	now := time.Unix(1_700_000_000, 0)
	s := runtoken.New(runtoken.Options{
		Domain: domain, Material: ring("k1", k1), Now: func() time.Time { return now },
	})
	valid := s.Mint("subject", time.Hour)
	if got := s.Validate(valid); got != "subject" {
		t.Fatalf("a freshly minted token did not validate: %q", got)
	}
	for name, token := range map[string]string{
		"empty":            "",
		"one part":         "v2",
		"too few parts":    "v2.tag.subject.9999999999",
		"too many parts":   valid + ".extra",
		"wrong version":    "v1" + strings.TrimPrefix(valid, "v2"),
		"tampered subject": strings.Replace(valid, "subject", "subjecu", 1),
	} {
		if got := s.Validate(token); got != "" {
			t.Errorf("%s validated as %q", name, got)
		}
	}
	// EXPIRY, which travels in the token: the same signer with a clock
	// past the deadline refuses what it minted.
	now = now.Add(2 * time.Hour)
	if got := s.Validate(valid); got != "" {
		t.Errorf("an expired token validated as %q", got)
	}
}

// TestAMintIsFlooredAwayFromZero: a token that expired before it was handed
// over is a run that fails on its first call for a reason nothing in the
// config explains.
func TestAMintIsFlooredAwayFromZero(t *testing.T) {
	t.Parallel()
	s := signer(t, "crewlet.test.v1", ring("k1", k1))
	if got := s.Validate(s.Mint("subject", 0)); got != "subject" {
		t.Errorf("a zero ttl minted a token that was already invalid: %q", got)
	}
	if got := s.Validate(s.Mint("subject", -time.Hour)); got != "subject" {
		t.Errorf("a negative ttl minted a token that was already invalid: %q", got)
	}
}

// TestAKeyringThatNamesNoActiveKeyIsPerProcess. It is a real deployment — a
// node with no secrets.keys — so it works, alone, and the caller is the one
// that says what it costs.
func TestAKeyringThatNamesNoActiveKeyIsPerProcess(t *testing.T) {
	t.Parallel()
	const domain = "crewlet.test.v1"
	for name, material := range map[string]runtoken.Material{
		"empty":            {},
		"no active id":     ring("", k1),
		"active id unheld": ring("k9", k1),
	} {
		t.Run(name, func(t *testing.T) {
			if material.Usable() {
				t.Fatal("a material that cannot sign for the fleet reported that it could")
			}
			one := signer(t, domain, material)
			token := one.Mint("subject", time.Hour)
			if got := one.Validate(token); got != "subject" {
				t.Errorf("a per-process signer refused its own token: %q", got)
			}
			// AND ONLY ITSELF, which is what the caller's warning is about.
			if got := signer(t, domain, material).Validate(token); got != "" {
				t.Errorf("a second process validated a per-process token: %q", got)
			}
		})
	}
	if !ring("k1", k1).Usable() {
		t.Error("a keyring whose active id names one of its keys reported that it could not sign")
	}
}

// TestASubjectSurvivesTheRoundTrip over the shapes the two issuers actually
// mint: a trace id and a run id.
func TestASubjectSurvivesTheRoundTrip(t *testing.T) {
	t.Parallel()
	s := signer(t, "crewlet.test.v1", ring("k1", k1))
	for _, subject := range []string{
		"4bf92f3577b34da6a3ce929d0e0e4736",
		"019948a2-7c1e-7000-8000-0123456789ab",
		"",
	} {
		if got := s.Validate(s.Mint(subject, time.Hour)); got != subject {
			t.Errorf("subject %q round-tripped as %q", subject, got)
		}
	}
}

func tagOf(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return ""
	}
	return parts[1]
}
