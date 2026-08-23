package config

import (
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestReferencedNamesFindsThemAnywhere(t *testing.T) {
	t.Parallel()
	payload := map[string]any{
		"a": "${ONE}",
		"b": []any{"${TWO}", map[string]any{"c": "prefix-${THREE}-suffix"}},
		"d": 5,
		"e": nil,
		// A duplicate must collapse, or a payload that mentions one
		// credential twice would fingerprint differently from one that
		// mentions it once.
		"f": "${ONE}",
	}
	if got := ReferencedNames(payload); !slices.Equal(got, []string{"ONE", "THREE", "TWO"}) {
		t.Fatalf("got %v", got)
	}
}

// It has to work on the three shapes a payload actually arrives in.
func TestReferencedNamesWalksTypedAndRawPayloads(t *testing.T) {
	t.Parallel()
	cfg := mustCompany(t, `
name: Acme
integrations:
  gitlab:
    enabled: true
    url: https://gitlab.com
    signing_secret: "${GL_SECRET}"
roles:
  - name: CEO
    mcp_env:
      gitlab:
        GITLAB_TOKEN: "${GL_TOKEN}"
`)
	if got := ReferencedNames(cfg); !slices.Equal(got, []string{"GL_SECRET", "GL_TOKEN"}) {
		t.Fatalf("typed payload: %v", got)
	}

	var doc yaml.Node
	if err := yaml.Unmarshal([]byte("a: \"${RAW_ONE}\"\n"), &doc); err != nil {
		t.Fatal(err)
	}
	if got := ReferencedNames(&doc); !slices.Equal(got, []string{"RAW_ONE"}) {
		t.Fatalf("raw node: %v", got)
	}
}

// The whole point. A rotation re-activates the UNCHANGED revision, so the
// payload is byte-identical and the payload alone can never detect one.
func TestFingerprintMovesOnRotation(t *testing.T) {
	t.Parallel()
	f := NewFingerprinter()
	payload := map[string]any{"token": "${ROTATING_SECRET}"}

	before := f.Of(payload, NewResolver(MapSource{"ROTATING_SECRET": "old"}))
	after := f.Of(payload, NewResolver(MapSource{"ROTATING_SECRET": "new"}))

	if before == after {
		t.Fatal("a rotated credential must change the fingerprint")
	}
}

// Equal payload AND equal fingerprint is a true no-op, and must stay one:
// rebuilding every subsystem on every re-activation would make the
// documented rotation gesture a fleet-wide restart.
func TestFingerprintIsStableWhenNothingRotated(t *testing.T) {
	t.Parallel()
	f := NewFingerprinter()
	r := NewResolver(MapSource{"STEADY": "same"})
	payload := map[string]any{"token": "${STEADY}"}
	if f.Of(payload, r) != f.Of(payload, r) {
		t.Fatal("an unchanged resolution must fingerprint the same")
	}
}

// Deliberate: the config layer resolves an unset reference to the empty
// string and every builder consumes it that way, so reporting a difference
// here would disagree with what the engine actually sees.
func TestUnsetAndEmptyFingerprintAlike(t *testing.T) {
	t.Parallel()
	f := NewFingerprinter()
	payload := map[string]any{"token": "${MAYBE_SET}"}
	unset := f.Of(payload, NewResolver(MapSource{}))
	empty := f.Of(payload, NewResolver(MapSource{"MAYBE_SET": ""}))
	if unset != empty {
		t.Fatal("unset and empty must fingerprint alike")
	}
}

// Length-prefixed encoding: ("ab","c") must not digest like ("a","bc"), or
// a rotation that moved a character between two credentials would read as a
// no-op.
func TestValuesCannotCollideAcrossNameBoundaries(t *testing.T) {
	t.Parallel()
	f := NewFingerprinter()
	payload := map[string]any{"x": "${AA}", "y": "${BB}"}
	one := f.Of(payload, NewResolver(MapSource{"AA": "ab", "BB": "c"}))
	two := f.Of(payload, NewResolver(MapSource{"AA": "a", "BB": "bc"}))
	if one == two {
		t.Fatal("values collided across the name boundary")
	}
}

// A fingerprint that reached a log or a row must not be
// offline-brute-forceable, so the digest is keyed per process AND the type
// cannot be printed or serialised into anything but a placeholder.
func TestFingerprintCannotLeakTheSecret(t *testing.T) {
	t.Parallel()
	f := NewFingerprinter()
	r := NewResolver(MapSource{"SEKRIT": "hunter2"})
	fp := f.Of(map[string]any{"t": "${SEKRIT}"}, r)

	rendered := fmt.Sprintf("%v %s", fp, fp)
	if rendered != "fingerprint(redacted) fingerprint(redacted)" {
		t.Fatalf("a fingerprint printed as %q", rendered)
	}
	encoded, err := json.Marshal(map[string]any{"fp": fp})
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != `{"fp":"redacted"}` {
		t.Fatalf("a fingerprint serialised as %s", encoded)
	}
}

// Two processes hold two keys, so a fingerprint means nothing outside the
// process that made it. Making that structural is cheaper than documenting
// it and hoping.
func TestTwoFingerprintersAreIncomparable(t *testing.T) {
	t.Parallel()
	r := NewResolver(MapSource{"SAME": "value"})
	payload := map[string]any{"t": "${SAME}"}
	if NewFingerprinter().Of(payload, r) == NewFingerprinter().Of(payload, r) {
		t.Fatal("two fingerprinters produced comparable digests")
	}
}

// A payload with no references still fingerprints, and stably: a config
// with no secrets at all must read as a clean no-op rather than as a
// rotation every time.
func TestPayloadWithNoReferencesIsStable(t *testing.T) {
	t.Parallel()
	f := NewFingerprinter()
	r := EnvOnly()
	if f.Of(map[string]any{"a": "literal"}, r) != f.Of(map[string]any{"a": "literal"}, r) {
		t.Fatal("a reference-free payload must fingerprint stably")
	}
}
