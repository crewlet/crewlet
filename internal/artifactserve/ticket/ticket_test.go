package ticket_test

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactserve/ticket"
	"github.com/crewlet/crewlet/internal/runtoken"
)

var (
	k1 = runtoken.KeyMaterial{ID: "k1", Material: "material-one"}
	k2 = runtoken.KeyMaterial{ID: "k2", Material: "material-two"}
)

// clock is a settable clock, read by the codec under test.
type clock struct{ at time.Time }

func (c *clock) now() time.Time { return c.at }

func codec(t *testing.T, c *clock, m runtoken.Material) *ticket.Codec {
	t.Helper()
	out, err := ticket.New(m, c.now)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return out
}

func start() *clock {
	return &clock{at: time.Date(2026, 10, 17, 12, 0, 0, 0, time.UTC)}
}

func claims(c *clock) artifacts.TicketClaims {
	return artifacts.TicketClaims{
		Artifact:     artifacts.IDFor("op-1"),
		Version:      3,
		Principal:    "designer",
		Lineage:      "session:6f2c4d0e-8a40-4c55-9a4f-0d7f1b2a3c4d",
		Capabilities: []artifacts.Capability{artifacts.CapabilityDB, artifacts.CapabilityAssets},
		AccessEpoch:  2,
		Document:     "boards/hero <one>.html",
		Nonce:        "q3yC1bRrX0m9t2vKpZ8wLg",
		Expires:      c.at.Add(artifacts.TicketLifetime),
	}
}

// TestATicketOpensToTheClaimsItWasMintedWith: the round trip, with the
// capabilities sorted and the expiry cut to whole seconds on the way in, so the
// claims a serving node reads are the ones that were signed.
func TestATicketOpensToTheClaimsItWasMintedWith(t *testing.T) {
	t.Parallel()
	c := start()
	codec := codec(t, c, runtoken.OneKey(k1.ID, k1.Material))
	in := claims(c)
	in.Expires = in.Expires.Add(-400 * time.Millisecond)
	tok, err := codec.Mint(in)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	if strings.ContainsAny(tok, "/+= ") {
		t.Fatalf("a ticket rides in a path segment and carries a character that "+
			"needs escaping: %q", tok)
	}
	got, verdict, err := codec.Open(tok)
	if err != nil || verdict != ticket.Valid {
		t.Fatalf("Open = %v, %v; want valid", verdict, err)
	}
	want := in
	want.Expires = time.Unix(in.Expires.Unix(), 0).UTC()
	want.Capabilities = []artifacts.Capability{artifacts.CapabilityAssets, artifacts.CapabilityDB}
	if got.Artifact != want.Artifact || got.Version != want.Version ||
		got.Principal != want.Principal || got.Lineage != want.Lineage ||
		got.AccessEpoch != want.AccessEpoch || got.Nonce != want.Nonce ||
		!got.Expires.Equal(want.Expires) ||
		strings.Join(caps(got.Capabilities), ",") != strings.Join(caps(want.Capabilities), ",") {
		t.Fatalf("Open = %+v, want %+v", got, want)
	}
}

func caps(in []artifacts.Capability) []string {
	out := make([]string, len(in))
	for i, c := range in {
		out[i] = string(c)
	}
	return out
}

// TestAnExpiredTicketIsToldApartFromAForgedOne: within the grace a ticket that
// ran out is Expired (the host mints another), and past it, or forged, Invalid.
func TestAnExpiredTicketIsToldApartFromAForgedOne(t *testing.T) {
	t.Parallel()
	c := start()
	codec := codec(t, c, runtoken.OneKey(k1.ID, k1.Material))
	tok, err := codec.Mint(claims(c))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}

	c.at = c.at.Add(artifacts.TicketLifetime)
	got, verdict, _ := codec.Open(tok)
	if verdict != ticket.Expired || got.Artifact.IsZero() {
		t.Fatalf("at its expiry: %v with claims %+v; want expired, claims kept", verdict, got)
	}
	c.at = c.at.Add(artifacts.TicketExpiredGrace - time.Second)
	if _, verdict, _ := codec.Open(tok); verdict != ticket.Expired {
		t.Fatalf("inside the grace: %v, want expired", verdict)
	}
	c.at = c.at.Add(2 * time.Second)
	if got, verdict, _ := codec.Open(tok); verdict != ticket.Invalid || !got.Artifact.IsZero() {
		t.Fatalf("past the grace: %v with claims %+v; want invalid and no claims", verdict, got)
	}

	c.at = start().at
	parts := strings.Split(tok, ".")
	last := parts[len(parts)-1]
	flipped := "A"
	if last[0] == 'A' {
		flipped = "B"
	}
	parts[len(parts)-1] = flipped + last[1:]
	if _, verdict, _ := codec.Open(strings.Join(parts, ".")); verdict != ticket.Invalid {
		t.Fatalf("a ticket whose signature was altered opened %v", verdict)
	}
	if _, verdict, _ := codec.Open(strings.Repeat("a", 5000)); verdict != ticket.Invalid {
		t.Fatalf("an oversized token opened %v", verdict)
	}
	if _, verdict, _ := codec.Open(""); verdict != ticket.Invalid {
		t.Fatalf("an empty token opened %v", verdict)
	}
}

// TestATicketSurvivesARotationAndNotADroppedKey: the keyring rule every runtoken
// keeps, held for this domain.
func TestATicketSurvivesARotationAndNotADroppedKey(t *testing.T) {
	t.Parallel()
	c := start()
	before := codec(t, c, runtoken.Material{ActiveID: "k1", Keys: []runtoken.KeyMaterial{k1}})
	tok, err := before.Mint(claims(c))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	flipped := codec(t, c, runtoken.Material{ActiveID: "k2", Keys: []runtoken.KeyMaterial{k1, k2}})
	if _, verdict, _ := flipped.Open(tok); verdict != ticket.Valid {
		t.Fatalf("flipping the active key refused an open frame's ticket: %v", verdict)
	}
	dropped := codec(t, c, runtoken.Material{ActiveID: "k2", Keys: []runtoken.KeyMaterial{k2}})
	if _, verdict, _ := dropped.Open(tok); verdict != ticket.Invalid {
		t.Fatalf("a ticket signed under a dropped key opened %v", verdict)
	}
}

// TestAnotherDomainsTokenIsNoTicket: a telemetry or tool bridge token over the
// same keyring never opens as a ticket.
func TestAnotherDomainsTokenIsNoTicket(t *testing.T) {
	t.Parallel()
	c := start()
	m := runtoken.OneKey(k1.ID, k1.Material)
	tickets := codec(t, c, m)
	good, err := tickets.Mint(claims(c))
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	subject := strings.Split(good, ".")[2]
	other, err := runtoken.New(runtoken.Options{Domain: "crewlet.otel.v1", Material: m, Now: c.now})
	if err != nil {
		t.Fatalf("runtoken.New: %v", err)
	}
	if _, verdict, _ := tickets.Open(other.Mint(subject, time.Hour)); verdict != ticket.Invalid {
		t.Fatalf("another domain's token over the same subject opened %v", verdict)
	}
}

// TestAMintRefusesClaimsNoTicketMayCarry: every field is held, and a deadline
// outside the ticket's own lifetime is refused rather than clamped.
func TestAMintRefusesClaimsNoTicketMayCarry(t *testing.T) {
	t.Parallel()
	c := start()
	codec := codec(t, c, runtoken.OneKey(k1.ID, k1.Material))
	cases := map[string]func(*artifacts.TicketClaims){
		"no artifact":       func(cl *artifacts.TicketClaims) { cl.Artifact = artifacts.ID{} },
		"version zero":      func(cl *artifacts.TicketClaims) { cl.Version = 0 },
		"no principal":      func(cl *artifacts.TicketClaims) { cl.Principal = "" },
		"spaced principal":  func(cl *artifacts.TicketClaims) { cl.Principal = "a b" },
		"no lineage":        func(cl *artifacts.TicketClaims) { cl.Lineage = "" },
		"classless lineage": func(cl *artifacts.TicketClaims) { cl.Lineage = "abc" },
		"negative epoch":    func(cl *artifacts.TicketClaims) { cl.AccessEpoch = -1 },
		"no nonce":          func(cl *artifacts.TicketClaims) { cl.Nonce = "" },
		"padded nonce":      func(cl *artifacts.TicketClaims) { cl.Nonce = "abc=" },
		"unknown capability": func(cl *artifacts.TicketClaims) {
			cl.Capabilities = []artifacts.Capability{"camera"}
		},
		"no expiry":       func(cl *artifacts.TicketClaims) { cl.Expires = time.Time{} },
		"expired":         func(cl *artifacts.TicketClaims) { cl.Expires = c.at.Add(-time.Second) },
		"past lifetime":   func(cl *artifacts.TicketClaims) { cl.Expires = c.at.Add(2 * time.Hour) },
		"expiring now":    func(cl *artifacts.TicketClaims) { cl.Expires = c.at },
		"huge principal":  func(cl *artifacts.TicketClaims) { cl.Principal = strings.Repeat("a", 200) },
		"no document":     func(cl *artifacts.TicketClaims) { cl.Document = "" },
		"rooted document": func(cl *artifacts.TicketClaims) { cl.Document = "/index.html" },
		"control in document": func(cl *artifacts.TicketClaims) {
			cl.Document = "boards/a\x00.html"
		},
		"document past the path bound": func(cl *artifacts.TicketClaims) {
			cl.Document = strings.Repeat("a", artifacts.MaxPathBytes+1)
		},
		"unknown purpose": func(cl *artifacts.TicketClaims) { cl.Purpose = "embed" },
		// A PREVIEW IS GRANTED WHAT DRAWING NEEDS, and lives minutes.
		"preview with the datastore": func(cl *artifacts.TicketClaims) {
			cl.Purpose = artifacts.TicketPurposePreview
			cl.Capabilities = []artifacts.Capability{artifacts.CapabilityAssets, artifacts.CapabilityDB}
			cl.Expires = c.at.Add(artifacts.PreviewTicketLifetime)
		},
		"preview with presence": func(cl *artifacts.TicketClaims) {
			cl.Purpose = artifacts.TicketPurposePreview
			cl.Capabilities = []artifacts.Capability{artifacts.CapabilityPresence}
			cl.Expires = c.at.Add(artifacts.PreviewTicketLifetime)
		},
		"preview past its lifetime": func(cl *artifacts.TicketClaims) {
			cl.Purpose = artifacts.TicketPurposePreview
			cl.Capabilities = nil
			cl.Expires = c.at.Add(artifacts.PreviewTicketLifetime + time.Minute)
		},
	}
	for name, mutate := range cases {
		cl := claims(c)
		mutate(&cl)
		if _, err := codec.Mint(cl); !errors.Is(err, ticket.ErrInvalidClaims) {
			t.Errorf("%s: Mint = %v, want ErrInvalidClaims", name, err)
		}
	}
}

// TestAPreviewsTicketOpensAsAPreview: a preview's claims round trip with their
// purpose, so a serving node and the ready route read the ticket as a preview.
func TestAPreviewsTicketOpensAsAPreview(t *testing.T) {
	t.Parallel()
	c := start()
	codec := codec(t, c, runtoken.OneKey(k1.ID, k1.Material))
	in := claims(c)
	in.Purpose = artifacts.TicketPurposePreview
	in.Capabilities = []artifacts.Capability{artifacts.CapabilityAssets}
	in.Expires = c.at.Add(artifacts.PreviewTicketLifetime)
	tok, err := codec.Mint(in)
	if err != nil {
		t.Fatalf("Mint: %v", err)
	}
	out, verdict, err := codec.Open(tok)
	if err != nil || verdict != ticket.Valid {
		t.Fatalf("Open: %v %v", verdict, err)
	}
	if out.Purpose != artifacts.TicketPurposePreview || len(out.Capabilities) != 1 {
		t.Errorf("the preview opened as %+v", out)
	}
}

// TestATicketAtEveryBoundOpens: the longest claims a mint accepts (every
// free-text claim at its bound, every capability, and a document path of
// characters JSON escapes) still make a ticket Open reads, so no mint writes a
// URL the origin refuses by its length.
func TestATicketAtEveryBoundOpens(t *testing.T) {
	t.Parallel()
	c := start()
	codec := codec(t, c, runtoken.OneKey(k1.ID, k1.Material))
	for _, doc := range []string{strings.Repeat(`"`, artifacts.MaxPathBytes),
		strings.Repeat("\u2028", artifacts.MaxPathBytes/3)} {
		cl := claims(c)
		cl.Principal = strings.Repeat("p", 128)
		cl.Lineage = "session:" + strings.Repeat("l", 152)
		cl.Nonce = strings.Repeat("n", 64)
		cl.Capabilities = artifacts.Capabilities
		cl.Document = doc
		tok, err := codec.Mint(cl)
		if err != nil {
			t.Fatalf("Mint: %v", err)
		}
		if _, verdict, err := codec.Open(tok); verdict != ticket.Valid {
			t.Fatalf("a %d-byte ticket at every bound opened %v (%v)", len(tok), verdict, err)
		}
	}
}

// TestASignedSubjectNoMintWritesIsRefused: the decode holds the claims to the
// form a mint signs, so even a holder of the keyring cannot hand the origin a
// subject this build would not have written; a field it does not know is
// ignored, as every signed envelope here ignores one.
func TestASignedSubjectNoMintWritesIsRefused(t *testing.T) {
	t.Parallel()
	c := start()
	m := runtoken.OneKey(k1.ID, k1.Material)
	tickets := codec(t, c, m)
	signer, err := runtoken.New(runtoken.Options{Domain: ticket.Domain, Material: m, Now: c.now})
	if err != nil {
		t.Fatalf("runtoken.New: %v", err)
	}
	id := artifacts.IDFor("op-1").String()
	sign := func(json string) string {
		return signer.Mint(base64.RawURLEncoding.EncodeToString([]byte(json)), time.Hour)
	}
	refused := map[string]string{
		"unsorted capabilities": `{"a":"` + id + `","v":1,"p":"x","k":"session:a","c":["db","assets"],"x":0,"d":"index.html","n":"abc","e":"2026-10-17T12:30:00Z"}`,
		"fractional expiry":     `{"a":"` + id + `","v":1,"p":"x","k":"session:a","x":0,"d":"index.html","n":"abc","e":"2026-10-17T12:30:00.5Z"}`,
		"no artifact":           `{"v":1,"p":"x","k":"session:a","x":0,"d":"index.html","n":"abc","e":"2026-10-17T12:30:00Z"}`,
		"two documents":         `{"a":"` + id + `","v":1,"p":"x","k":"session:a","x":0,"d":"index.html","n":"abc","e":"2026-10-17T12:30:00Z"}{}`,
		"not json":              `not json`,
		"no document":           `{"a":"` + id + `","v":1,"p":"x","k":"session:a","x":0,"n":"abc","e":"2026-10-17T12:30:00Z"}`,
	}
	for name, doc := range refused {
		if _, verdict, _ := tickets.Open(sign(doc)); verdict != ticket.Invalid {
			t.Errorf("%s: opened %v, want invalid", name, verdict)
		}
	}
	additive := `{"a":"` + id + `","v":1,"p":"x","k":"session:a","x":0,"d":"index.html","n":"abc","e":"2026-10-17T12:30:00Z","z":"from a newer build"}`
	if _, verdict, err := tickets.Open(sign(additive)); verdict != ticket.Valid {
		t.Errorf("a field a newer build added refused the ticket: %v, %v", verdict, err)
	}
}

// TestACodecNeedsAKeyringThatCanSign: no per-process key, ever.
func TestACodecNeedsAKeyringThatCanSign(t *testing.T) {
	t.Parallel()
	if _, err := ticket.New(runtoken.Material{}, nil); !errors.Is(err, runtoken.ErrNoKeyring) {
		t.Fatalf("New with no keyring = %v, want ErrNoKeyring", err)
	}
}
