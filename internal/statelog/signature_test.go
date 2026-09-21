package statelog_test

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/statelog"
)

// The broker has no auth of its own, so these are the cases that say what a
// record's signature is worth: what it proves, what it refuses, and which of
// the two failures an operator can still recover from.

func ring(active string, keys ...statelog.Key) statelog.Keyring {
	return statelog.Keyring{ActiveID: active, Keys: keys}
}

var (
	key1 = statelog.Key{ID: "k1", Material: "material-one"}
	key2 = statelog.Key{ID: "k2", Material: "material-two"}
)

func mustSign(t *testing.T, domain string, r statelog.Keyring) *statelog.Signer {
	t.Helper()
	s, err := statelog.NewSigner(domain, r)
	if err != nil {
		t.Fatalf("NewSigner: %v", err)
	}
	return s
}

func mustVerify(t *testing.T, domain string, r statelog.Keyring) *statelog.Verifier {
	t.Helper()
	v, err := statelog.NewVerifier(domain, r)
	if err != nil {
		t.Fatalf("NewVerifier: %v", err)
	}
	return v
}

// TestASignedRecordOpensToTheBodyItCarries is the ordinary path, and it also
// pins that the frame is transparent: what comes back is what went in.
func TestASignedRecordOpensToTheBodyItCarries(t *testing.T) {
	t.Parallel()
	body := []byte(`{"kind":"task.create","v":1}`)
	framed := mustSign(t, "tracker", ring("k1", key1)).Seal(body)
	if bytes.Equal(framed, body) {
		t.Fatal("the frame added nothing, so nothing was signed")
	}
	got, verdict, err := mustVerify(t, "tracker", ring("k1", key1)).Open(framed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if verdict != statelog.Verified {
		t.Fatalf("verdict = %q, want %q", verdict, statelog.Verified)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("the body came back as %q, want %q", got, body)
	}
}

// TestATamperedRecordIsRefusedPermanently. This is the disposition that stops
// the applier: no operator action makes it true later.
func TestATamperedRecordIsRefusedPermanently(t *testing.T) {
	t.Parallel()
	signer := mustSign(t, "tracker", ring("k1", key1))
	verifier := mustVerify(t, "tracker", ring("k1", key1))
	framed := signer.Seal([]byte(`{"kind":"task.create"}`))

	for name, mutate := range map[string]func([]byte) []byte{
		"a flipped body byte": func(b []byte) []byte {
			out := bytes.Clone(b)
			out[len(out)-1] ^= 0xFF
			return out
		},
		"a flipped MAC byte": func(b []byte) []byte {
			out := bytes.Clone(b)
			out[len(out)-40] ^= 0xFF
			return out
		},
		"a truncated frame": func(b []byte) []byte { return b[:len(b)-1] },
		"no frame at all":   func([]byte) []byte { return []byte(`{"kind":"task.create"}`) },
		"an empty record":   func([]byte) []byte { return nil },
		"the magic only":    func(b []byte) []byte { return b[:4] },
	} {
		t.Run(name, func(t *testing.T) {
			body, verdict, err := verifier.Open(mutate(framed))
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if verdict != statelog.Tampered {
				t.Errorf("verdict = %q, want %q", verdict, statelog.Tampered)
			}
			if body != nil {
				t.Errorf("a tampered record handed back %d bytes of body — no domain "+
					"decoder may be given bytes this fleet did not write", len(body))
			}
		})
	}
}

// TestAnUnknownKeyIsRecoverableAndKeepsTheBody is the other disposition, and
// the two halves of it are the whole reason the verdict is not a bool: the
// record is retained rather than refused, and the framework needs its body to
// FILE it under its own scope while it waits.
func TestAnUnknownKeyIsRecoverableAndKeepsTheBody(t *testing.T) {
	t.Parallel()
	body := []byte(`{"kind":"task.create"}`)
	framed := mustSign(t, "tracker", ring("k2", key2)).Seal(body)

	// A node that does not hold k2 yet.
	got, verdict, err := mustVerify(t, "tracker", ring("k1", key1)).Open(framed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if verdict != statelog.KeyUnknown {
		t.Fatalf("verdict = %q, want %q", verdict, statelog.KeyUnknown)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("an unverifiable record gave back %q; the framework needs the body "+
			"to retain it under its own scope", got)
	}

	// The operator adds k2. The SAME bytes now verify — that is what
	// makes this disposition recoverable.
	got, verdict, err = mustVerify(t, "tracker", ring("k1", key1, key2)).Open(framed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if verdict != statelog.Verified {
		t.Errorf("verdict = %q after the key arrived, want %q", verdict, statelog.Verified)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("body = %q, want %q", got, body)
	}
}

// TestARotationVerifiesInBothDirections is the property that makes a rollout
// survivable: while the fleet is mixed, a node that has added the new key and
// one that has not must each verify the other's records.
func TestARotationVerifiesInBothDirections(t *testing.T) {
	t.Parallel()
	body := []byte(`{"kind":"task.create"}`)
	oldNode := ring("k1", key1)
	newNode := ring("k1", key1, key2) // added, not yet active
	flipped := ring("k2", key1, key2) // active flipped

	for name, tc := range map[string]struct{ signer, verifier statelog.Keyring }{
		"old writes, new reads":     {oldNode, newNode},
		"new writes, old reads":     {newNode, oldNode},
		"flipped writes, new reads": {flipped, newNode},
		"new writes, flipped reads": {newNode, flipped},
	} {
		t.Run(name, func(t *testing.T) {
			framed := mustSign(t, "tracker", tc.signer).Seal(body)
			_, verdict, err := mustVerify(t, "tracker", tc.verifier).Open(framed)
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			if verdict != statelog.Verified {
				t.Errorf("verdict = %q, want %q — a rotation must not split the fleet",
					verdict, statelog.Verified)
			}
		})
	}

	// AND THE ONE DIRECTION THAT MUST NOT WORK: a node that dropped k1
	// refuses a record signed under it, which is what the drop is for.
	framed := mustSign(t, "tracker", oldNode).Seal(body)
	_, verdict, err := mustVerify(t, "tracker", statelog.OneKey("k2", key2.Material)).Open(framed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if verdict != statelog.KeyUnknown {
		t.Errorf("verdict = %q after the key was dropped, want %q", verdict, statelog.KeyUnknown)
	}
}

// TestOneDomainsRecordNeverVerifiesOnAnothersLog. A record replayed from one
// log onto another must not be applied there, and the domain is bound into
// both the derived key and the signed bytes so that neither half alone is the
// whole claim.
func TestOneDomainsRecordNeverVerifiesOnAnothersLog(t *testing.T) {
	t.Parallel()
	r := ring("k1", key1)
	framed := mustSign(t, "tracker", r).Seal([]byte(`{"kind":"task.create"}`))
	_, verdict, err := mustVerify(t, "pages", r).Open(framed)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if verdict != statelog.Tampered {
		t.Errorf("verdict = %q on another domain's log, want %q", verdict, statelog.Tampered)
	}
}

// TestARewrittenKeyIdDoesNotVerify: the id in the frame is a CLAIM, so it is
// inside the MAC as well as beside it. Rewriting it to name a key the verifier
// holds must not make the record verify under that key.
func TestARewrittenKeyIdDoesNotVerify(t *testing.T) {
	t.Parallel()
	both := ring("k1", key1, key2)
	framed := mustSign(t, "tracker", ring("k1", key1)).Seal([]byte(`{"kind":"x"}`))
	rewritten := bytes.Replace(framed, []byte("k1"), []byte("k2"), 1)
	if bytes.Equal(rewritten, framed) {
		t.Fatal("the fixture did not rewrite the id")
	}
	_, verdict, err := mustVerify(t, "tracker", both).Open(rewritten)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if verdict != statelog.Tampered {
		t.Errorf("verdict = %q for a rewritten key id, want %q", verdict, statelog.Tampered)
	}
}

// TestAKeyringThatCannotSignIsRefusedAtConstruction. A signer that silently
// signed under nothing would produce records the fleet refuses one at a time,
// on every node, for ever — so the refusal is at boot and it names the field.
func TestAKeyringThatCannotSignIsRefusedAtConstruction(t *testing.T) {
	t.Parallel()
	for name, r := range map[string]statelog.Keyring{
		"no keys at all":       {},
		"no active id":         {Keys: []statelog.Key{key1}},
		"active names no key":  ring("k9", key1),
		"a key with no id":     ring("k1", key1, statelog.Key{Material: "x"}),
		"an id that is a book": ring("k1", statelog.Key{ID: strings.Repeat("x", 65), Material: "m"}),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := statelog.NewSigner("tracker", r); err == nil {
				t.Error("a keyring that cannot sign built a signer")
			} else if !errors.Is(err, statelog.ErrUnsigned) {
				t.Errorf("error = %v, want one wrapping ErrUnsigned so a caller can "+
					"tell a configuration fault from a broker one", err)
			}
		})
	}
	if _, err := statelog.NewVerifier("tracker", statelog.Keyring{}); err == nil {
		t.Error("a node with no keys built a verifier, so it could not tell a peer's " +
			"record from one written by anything else that reaches the broker")
	}
}

// TestAVerifierNamesWhatItHolds, because the log line that says a record was
// retained has to say what the node would have needed.
func TestAVerifierNamesWhatItHolds(t *testing.T) {
	t.Parallel()
	got := mustVerify(t, "tracker", ring("k2", key2, key1)).KeyIDs()
	if len(got) != 2 || got[0] != "k1" || got[1] != "k2" {
		t.Errorf("KeyIDs() = %v, want the ids sorted", got)
	}
}

// TestEveryVerdictIsValidAndNothingElseIs, because the disposition is read off
// this value and an unknown one would be neither retained nor refused.
func TestEveryVerdictIsValidAndNothingElseIs(t *testing.T) {
	t.Parallel()
	for _, v := range []statelog.Verdict{statelog.Verified, statelog.KeyUnknown, statelog.Tampered} {
		if !v.Valid() {
			t.Errorf("%q is not Valid", v)
		}
	}
	for _, v := range []statelog.Verdict{"", "ok", "VERIFIED", "unknown"} {
		if v.Valid() {
			t.Errorf("%q reported itself Valid", v)
		}
	}
}
