package jetstream

import (
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"github.com/nats-io/nkeys"

	"github.com/crewlet/crewlet/internal/runtoken"
)

// testRing is the keyring every fleet this package's own tests stand up holds:
// a literal placeholder, never a key anybody deploys.
var testRing = runtoken.OneKey("jetstream-test", "the jetstream package's test fleet key material")

// testFleetAuth is the fleet credentials of [testRing], for a test that starts
// a member with a route or leaf listener, or a leaf, through [StartServer] or
// [Config] directly — the fixture jetstreamtest's harness gives its own
// clusters, which this package cannot import.
var testFleetAuth = sync.OnceValue(func() FleetAuth {
	auth, err := FleetAuthFrom(testRing)
	if err != nil {
		panic(err)
	}
	return auth
})

// authFor is a fresh authenticator for cfg, for a case that asks only which
// options cfg produces: nothing it is handed ever connects.
func authFor(t *testing.T, cfg Config) *brokerAuth {
	t.Helper()
	auth, err := newBrokerAuth(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return auth
}

// fleetAuthOf is a FleetAuth over keys, failing the test on a refusal.
func fleetAuthOf(t *testing.T, m runtoken.Material) FleetAuth {
	t.Helper()
	auth, err := FleetAuthFrom(m)
	if err != nil {
		t.Fatalf("derive fleet credentials: %v", err)
	}
	return auth
}

// EACH LISTENER DERIVES ITS OWN CREDENTIAL, PER KEY, FROM THE KEYRING'S RULE.
//
// The three listeners take unrelated secrets from one key — so a route
// password captured on the wire opens no leaf or client listener — and none is
// a run token's key for the same id. Each key derives its own, so adding a key
// to a ring moves nothing the keys already in it present: a node that has
// added k2 and a node that has not still agree on k1. The route username is
// the key's tag, which names no configured id, and the active key is the one
// presented.
//
// COMPARED AS DERIVED BYTES — the route password decoded, the leaf and
// operator seeds' raw 32 bytes — never a base64 password against a seed's
// encoding, which differ whatever the derivation does.
//
// Mutations: one domain for two listeners → the pairwise row, red; derive every
// key from the whole ring's material → the second-key row, red; accept a
// Material whose active id names no key → the refusal row, red.
func TestEachListenerDerivesItsOwnCredentialPerKey(t *testing.T) {
	t.Parallel()
	k1 := runtoken.KeyMaterial{ID: "k1", Material: "the first key's placeholder material"}
	k2 := runtoken.KeyMaterial{ID: "k2", Material: "the second key's placeholder material"}
	one := fleetAuthOf(t, runtoken.Material{ActiveID: "k1", Keys: []runtoken.KeyMaterial{k1}})
	two := fleetAuthOf(t, runtoken.Material{ActiveID: "k2", Keys: []runtoken.KeyMaterial{k1, k2}})

	username, password := one.routeUser()
	if want := runtoken.KeyTag(RouteKeyDomain, "k1"); username != want {
		t.Errorf("the route username is %q, want the key's tag %q", username, want)
	}
	if strings.Contains(username, "k1") {
		t.Errorf("the route username %q names the configured id", username)
	}
	route, err := base64.RawURLEncoding.DecodeString(password)
	if err != nil || len(route) != 32 {
		t.Fatalf("the route password is not base64url of 32 bytes: %d bytes, %v", len(route), err)
	}
	leaf := rawSeed(t, one.leafSeed())
	operatorSeed, operatorPublic, operatorID, err := OperatorCredential(
		runtoken.Material{ActiveID: "k1", Keys: []runtoken.KeyMaterial{k1}})
	if err != nil {
		t.Fatalf("OperatorCredential: %v", err)
	}
	operator := rawSeed(t, string(operatorSeed))
	token := runtoken.DeriveKey("crewlet.otlp.v1", "k1", k1.Material)
	for name, pair := range map[string][2][]byte{
		"route and leaf":     {route, leaf},
		"route and operator": {route, operator},
		"leaf and operator":  {leaf, operator},
		"route and a token":  {route, token},
		"leaf and a token":   {leaf, token},
		"operator and token": {operator, token},
	} {
		if bytes.Equal(pair[0], pair[1]) {
			t.Errorf("%s derive the same 32 bytes from one key", name)
		}
	}

	// A SECOND KEY MOVES NOTHING THE FIRST PRESENTS, and the active one is
	// what a node presents.
	if two.routes[username].password != password {
		t.Error("adding k2 changed k1's route password, so a node that added a " +
			"key would refuse every peer that had not")
	}
	if _, ok := two.leaves[publicOf(t, one.leafSeed())]; !ok {
		t.Error("adding k2 changed k1's leaf key")
	}
	if two.operators[operatorPublic] != "k1" {
		t.Error("adding k2 changed k1's operator key")
	}
	if got, _ := two.routeUser(); got != runtoken.KeyTag(RouteKeyDomain, "k2") {
		t.Errorf("a ring whose active key is k2 presents route username %q", got)
	}
	if publicOf(t, two.leafSeed()) == publicOf(t, one.leafSeed()) {
		t.Error("a ring whose active key is k2 presents k1's leaf key")
	}
	if operatorID != "k1" || one.operators[operatorPublic] != "k1" {
		t.Errorf("the operator credential derives from %q and is accepted as %q, want k1",
			operatorID, one.operators[operatorPublic])
	}

	// A RING THAT CANNOT PRESENT ANYTHING IS REFUSED, and an empty one is the
	// zero value.
	if _, err := FleetAuthFrom(runtoken.Material{ActiveID: "k9",
		Keys: []runtoken.KeyMaterial{k1}}); !errors.Is(err, runtoken.ErrNoKeyring) {
		t.Errorf("a ring whose active id names no key answered %v, want ErrNoKeyring", err)
	}
	if zero := fleetAuthOf(t, runtoken.Material{}); !zero.IsZero() {
		t.Error("an empty ring derived credentials")
	}
	if _, _, _, err := OperatorCredential(runtoken.Material{}); !errors.Is(err, ErrNoFleetKeys) {
		t.Errorf("an empty ring's operator credential answered %v, want ErrNoFleetKeys", err)
	}
}

// rawSeed is an encoded nkey seed's 32 raw bytes.
func rawSeed(t *testing.T, seed string) []byte {
	t.Helper()
	_, raw, err := nkeys.DecodeSeed([]byte(seed))
	if err != nil {
		t.Fatalf("decode a derived seed: %v", err)
	}
	return raw
}

// publicOf is an encoded nkey seed's public key.
func publicOf(t *testing.T, seed string) string {
	t.Helper()
	kp, err := nkeys.FromSeed([]byte(seed))
	if err != nil {
		t.Fatalf("read a derived seed: %v", err)
	}
	public, err := kp.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	return public
}

// FLEET CREDENTIALS PRINT AS THE KEY IDS THEY DERIVE FROM, AND NOTHING ELSE.
//
// A Config carries them, and a Config is the kind of value a log line or an
// error prints whole: as JSON through slog, as %v, %+v and %#v through fmt.
//
// Mutations: render the struct in LogValue, or drop String or GoString → the
// secret appears, red.
func TestFleetAuthPrintsNoSecret(t *testing.T) {
	t.Parallel()
	m := runtoken.Material{ActiveID: "k2", Keys: []runtoken.KeyMaterial{
		{ID: "k1", Material: "the first key's placeholder material"},
		{ID: "k2", Material: "the second key's placeholder material"},
	}}
	auth := fleetAuthOf(t, m)
	cfg := Config{ServerName: "a", FleetAuth: auth}
	var logged bytes.Buffer
	slog.New(slog.NewJSONHandler(&logged, nil)).Info("x", "auth", auth, "cfg", cfg)
	printed := logged.String() + fmt.Sprint(cfg) + fmt.Sprintf("%+v %#v %v", cfg, cfg, auth)

	var secrets []string
	for _, rk := range auth.routes {
		secrets = append(secrets, rk.password)
	}
	for _, lk := range auth.leaves {
		secrets = append(secrets, lk.seed)
	}
	seed, _, _, err := OperatorCredential(m)
	if err != nil {
		t.Fatal(err)
	}
	secrets = append(secrets, string(seed))
	for _, secret := range secrets {
		if strings.Contains(printed, secret) {
			t.Fatalf("a printed or logged Config carries a derived secret:\n%s", printed)
		}
	}
	for _, id := range []string{"k1", "k2"} {
		if !strings.Contains(logged.String(), id) || !strings.Contains(fmt.Sprint(auth), id) {
			t.Errorf("the printed credentials do not name key %s:\n%s", id, printed)
		}
	}
}
