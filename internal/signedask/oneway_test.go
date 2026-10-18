package signedask

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// beacon is a one-way frame as a test spells it: who it names, and when it
// stops being believed.
type beacon struct {
	Who   string    `json:"who"`
	Until time.Time `json:"until"`
}

func (b beacon) Expires() time.Time { return b.Until }

func oneWay(t *testing.T, name string, ring statelog.Keyring) *OneWay[beacon] {
	t.Helper()
	w, err := NewOneWay[beacon](name, ring)
	if err != nil {
		t.Fatal(err)
	}
	return w
}

var here = beacon{Who: "seat:dana", Until: instant.Add(time.Minute)}

// A FRAME THE FLEET SEALED IS BELIEVED UNTIL IT EXPIRES, by every node that
// holds the key, as many times as it arrives: a broadcast is meant for every
// listener, and a repeat says only what the original said.
func TestAOneWayFrameIsBelievedUntilItExpires(t *testing.T) {
	t.Parallel()
	sender, receiver := oneWay(t, "test.presence", fleetRing), oneWay(t, "test.presence", fleetRing)
	sealed, err := sender.Seal(here)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		got, d, err := receiver.Open(sealed, instant)
		if d != Admitted || err != nil || got != here {
			t.Fatalf("a frame the fleet sealed opened as (%+v, %s, %v), want it admitted", got, d, err)
		}
	}
	got, d, err := receiver.Open(sealed, here.Until)
	if d != Stale || err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("a frame at its expiry opened as (%+v, %s, %v), want it stale", got, d, err)
	}
}

// A FRAME THE FLEET DID NOT SEAL IS BELIEVED IN NOTHING: not a frame, a MAC
// that fails, a frame sealed in another domain or by another fleet, and a body
// edited after sealing are each refused, and none hands back who it names.
func TestAOneWayFrameTheFleetDidNotSealIsBelievedInNothing(t *testing.T) {
	t.Parallel()
	w := oneWay(t, "test.presence", fleetRing)
	sealed, err := w.Seal(here)
	if err != nil {
		t.Fatal(err)
	}
	stranger := oneWay(t, "test.presence", statelog.OneKey("k1", "somebody else's key material"))
	theirs, err := stranger.Seal(here)
	if err != nil {
		t.Fatal(err)
	}
	elsewhere := oneWay(t, "test.other", fleetRing)
	otherDomain, err := elsewhere.Seal(here)
	if err != nil {
		t.Fatal(err)
	}
	edited := bytes.Replace(bytes.Clone(sealed), []byte("seat:dana"), []byte("seat:erin"), 1)
	for name, frame := range map[string][]byte{
		"not a frame":            []byte(`{"who":"seat:dana"}`),
		"another fleet's key":    theirs,
		"another channel's seal": otherDomain,
		"an edited body":         edited,
	} {
		got, d, err := w.Open(frame, instant)
		if d != Tampered || err == nil || got != (beacon{}) {
			t.Errorf("%s opened as (%+v, %s, %v), want it tampered and believed in nothing",
				name, got, d, err)
		}
	}
}

// A FRAME UNDER A KEY THIS NODE DOES NOT HOLD is a fact about the node, and
// says so, still believing nothing it names.
func TestAOneWayFrameUnderAKeyThisNodeDoesNotHoldNamesTheKey(t *testing.T) {
	t.Parallel()
	rotated := statelog.Keyring{ActiveID: "k2", Keys: []statelog.Key{
		{ID: "k1", Material: "the signed-ask suite's fleet key material"},
		{ID: "k2", Material: "the next key"},
	}}
	sealed, err := oneWay(t, "test.presence", rotated).Seal(here)
	if err != nil {
		t.Fatal(err)
	}
	got, d, err := oneWay(t, "test.presence", fleetRing).Open(sealed, instant)
	if d != KeyUnknown || err == nil || !strings.Contains(err.Error(), "secrets.keys") ||
		got != (beacon{}) {
		t.Fatalf("a frame under a key this node lacks opened as (%+v, %s, %v)", got, d, err)
	}
}

// A FRAME THAT NEVER EXPIRES IS NOT SEALED: every node would refuse it, and
// the sender is the one that should learn.
func TestAOneWayFrameIsNotSealedWithoutAnExpiry(t *testing.T) {
	t.Parallel()
	if _, err := oneWay(t, "test.presence", fleetRing).Seal(beacon{Who: "seat:dana"}); err == nil {
		t.Fatal("a frame with no expiry was sealed")
	}
	if _, err := NewOneWay[beacon]("", fleetRing); err == nil {
		t.Fatal("a one-way channel with no name was built")
	}
	if w := oneWay(t, "test.presence", fleetRing); w.Domain() != "test.presence" {
		t.Fatalf("the channel seals under %q, want its own name", w.Domain())
	}
}
