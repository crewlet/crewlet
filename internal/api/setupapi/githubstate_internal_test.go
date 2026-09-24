package setupapi

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/secrets"
)

// THE STATE IS THE ONLY THING GUARDING AN UNAUTHENTICATED WRITE, and these are
// its rules, held against the mint and the open themselves.
//
// GitHub's redirect carries no engine credential, so what stands between the
// callback and anybody who can reach the engine is the state alone — and the
// callback's writes are the continuation of the gesture that minted it, so the
// state is also the only thing that can say whose gesture that was.

// ring is a keyring over named keys, the first name active.
func ring(t *testing.T, keys map[string][]byte, active string) secrets.Cipher {
	t.Helper()
	cipher, err := secrets.NewCipher(secrets.Keyring{ActiveID: active, Keys: keys})
	if err != nil {
		t.Fatalf("NewCipher: %v", err)
	}
	return cipher
}

// freshKey is one key, as `crewlet secrets keygen` mints it.
func freshKey(t *testing.T) []byte {
	t.Helper()
	key, err := secrets.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return key
}

// flowAt is a flow over cipher whose clock reads *now.
func flowAt(cipher secrets.Cipher, now *time.Time) *AppFlow {
	return newAppFlow(&Service{clock: func() time.Time { return *now }}, cipher, nil)
}

// began is a person who began a creation through their own machine token, so
// every part of the party is distinct.
var began = iam.Actor{Name: "jane.doe", Kind: iam.ActorOperator,
	OperatorID: "pat:0192f00d-0000-7000-8000-00000000000a"}

// A STATE CARRIES ITS SEAT AND WHO BEGAN IT, whole, from the begin route to
// the callback. The callback's writes record that party, and they used to
// record `setup`, which is nobody. Mutation: drop the credential from the mint
// and the party comes back short.
func TestAStateCarriesItsSeatAndWhoBeganIt(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	flow := flowAt(ring(t, map[string][]byte{"k1": freshKey(t)}, "k1"), &now)

	state, err := flow.mintState("sre-lead", began)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	opened, err := flow.openState(state)
	if err != nil {
		t.Fatalf("a state this flow minted was refused: %v", err)
	}
	if opened.Seat != "sre-lead" || opened.party() != began {
		t.Errorf("the state opened as %q begun by %+v, want sre-lead begun by %+v",
			opened.Seat, opened.party(), began)
	}
}

// A STATE IS REFUSED UNLESS THIS FLEET SEALED IT FOR THIS FLOW, WITHIN ITS
// WINDOW — and every refusal is the one sentinel, because telling a caller
// which of its links was wrong tells an attacker the same.
func TestAStateIsRefusedUnlessThisFleetSealedItForThisFlow(t *testing.T) {
	t.Parallel()
	key := freshKey(t)
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	flow := flowAt(ring(t, map[string][]byte{"k1": key}, "k1"), &now)
	good, err := flow.mintState("sre-lead", began)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	raw, err := base64.RawURLEncoding.DecodeString(good)
	if err != nil {
		t.Fatalf("the state is not the URL-safe encoding it claims: %v", err)
	}

	// SEALED BY THIS FLEET'S KEYRING FOR THIS FLOW, the body a state
	// would carry: what the refusals below differ from it by is the point.
	sealAs := func(cipher secrets.Cipher, aad string, body appState) string {
		t.Helper()
		doc, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		sealed, err := cipher.Encrypt(string(doc), aad)
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString([]byte(sealed))
	}
	valid := appState{Seat: "sre-lead", ExpiresAt: now.Add(time.Minute),
		By: began.Name, ByKind: string(began.Kind), OperatorID: began.OperatorID}
	tampered := append([]byte(nil), raw...)
	tampered[len(tampered)-3] ^= 0x01

	for name, state := range map[string]string{
		"empty":           "",
		"not a state":     "v1.sre-lead.9999999999.deadbeef",
		"a bare envelope": string(raw),
		"tampered":        base64.RawURLEncoding.EncodeToString(tampered),
		// ANOTHER KEYRING — another deployment, or a key nobody holds.
		"another keyring": sealAs(ring(t, map[string][]byte{"k1": freshKey(t)}, "k1"),
			stateAAD, valid),
		// ANOTHER PURPOSE under this very keyring: a login flight or a
		// stored secret is sealed by the same keys, and the context is
		// what stops one opening as the other.
		"another purpose": sealAs(ring(t, map[string][]byte{"k1": key}, "k1"),
			"iam_oidc/flight", valid),
		"lapsed": sealAs(ring(t, map[string][]byte{"k1": key}, "k1"), stateAAD,
			appState{Seat: "sre-lead", ExpiresAt: now, By: began.Name,
				ByKind: string(began.Kind)}),
		// A STATE THAT NAMES NOBODY, which this flow never mints: its
		// writes would record nobody.
		"no beginner": sealAs(ring(t, map[string][]byte{"k1": key}, "k1"), stateAAD,
			appState{Seat: "sre-lead", ExpiresAt: now.Add(time.Minute)}),
		"no seat": sealAs(ring(t, map[string][]byte{"k1": key}, "k1"), stateAAD,
			appState{ExpiresAt: now.Add(time.Minute), By: began.Name}),
	} {
		if _, err := flow.openState(state); !errors.Is(err, ErrStateRefused) {
			t.Errorf("%s: opened with %v, want %v", name, err, ErrStateRefused)
		}
	}
	// THE CONTROL: the same seal, the same context, inside the window.
	if _, err := flow.openState(sealAs(ring(t, map[string][]byte{"k1": key}, "k1"),
		stateAAD, valid)); err != nil {
		t.Errorf("a well-formed state was refused (%v), so the cases above "+
			"prove nothing", err)
	}

	// AND IT LAPSES WITH ITS WINDOW, which is the claim bucket's age.
	now = now.Add(manifestTTL)
	if _, err := flow.openState(good); !errors.Is(err, ErrStateRefused) {
		t.Errorf("a state %s old opened (%v), want it lapsed", manifestTTL, err)
	}
}

// A STATE TRAVELS IN A URL AND SAYS NOTHING TO THE PARTIES IT PASSES.
//
// It goes to GitHub and comes back as a query parameter, through GitHub's own
// logs, a browser history and every ingress access log. Unescaped, the
// keyring's standard-base64 envelope turns every `+` into a space on the way
// back; signed rather than sealed, it would hand each of those parties the
// name of the person who began the creation and the credential they used.
func TestAStateTravelsInAURLAndDisclosesNothing(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	flow := flowAt(ring(t, map[string][]byte{"k1": freshKey(t)}, "k1"), &now)
	for range 32 {
		state, err := flow.mintState("sre-lead", began)
		if err != nil {
			t.Fatalf("mint: %v", err)
		}
		if escaped := url.QueryEscape(state); escaped != state {
			t.Fatalf("the state needs escaping in a URL: %q", state)
		}
		raw, err := base64.RawURLEncoding.DecodeString(state)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		for _, secret := range []string{began.Name, began.OperatorID, "sre-lead"} {
			if strings.Contains(state, secret) || strings.Contains(string(raw), secret) {
				t.Fatalf("the state carries %q readable by whoever sees the link", secret)
			}
		}
	}
}

// TWO NODES OF ONE FLEET AGREE, ACROSS A ROTATION. The begin and the callback
// land wherever a load balancer puts them, so a state one node sealed has to
// open on another — including one that has already made the next key active,
// which is the middle of every keyring rotation.
func TestTwoNodesOfOneFleetAgreeOnAStateAcrossARotation(t *testing.T) {
	t.Parallel()
	k1, k2 := freshKey(t), freshKey(t)
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	before := flowAt(ring(t, map[string][]byte{"k1": k1}, "k1"), &now)
	after := flowAt(ring(t, map[string][]byte{"k1": k1, "k2": k2}, "k2"), &now)

	state, err := before.mintState("sre-lead", began)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if opened, err := after.openState(state); err != nil || opened.Seat != "sre-lead" {
		t.Errorf("a node that rotated could not open its peer's state: %+v (%v)",
			opened, err)
	}
	// AND BACK, while the old node still holds only the old key: a state
	// sealed under a key it does not have is refused rather than guessed.
	state, err = after.mintState("sre-lead", began)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if _, err := before.openState(state); !errors.Is(err, ErrStateRefused) {
		t.Errorf("a node without the active key opened a state sealed under it: %v", err)
	}
}
