package config

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/seat/placement"
)

// Every Tier A rejection, with the field path an operator can search for.
func TestBootstrapValidatorRejections(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		yaml string
		path string
		kind error
	}{
		{"node id shape", "node:\n  id: has space\n", "node.id", ErrUnknownValue},
		{"node id too long", "node:\n  id: " + strings.Repeat("a", 65) + "\n", "node.id", ErrUnknownValue},
		{"unknown node role", "node:\n  roles: [ingres]\n", "node.roles", ErrUnknownValue},
		{"empty node roles", "node:\n  roles: []\n", "node.roles", ErrMissing},
		{"blank label key", "node:\n  labels:\n    \"\": eu\n", "node.labels", ErrMissing},

		{"no store path", "store:\n  path: \"\"\n", "store.path", ErrMissing},
		{"negative pool", "store:\n  max_open_conns: -1\n", "store.max_open_conns", ErrOutOfRange},

		{"unknown stream type", "stream:\n  type: kafka\n", "stream.type", ErrUnknownValue},
		{"external stream with no url", "stream:\n  type: nats\ncoordination:\n  type: embedded-kv\n", "stream.url", ErrMissing},
		{"embedded stream with a url", "stream:\n  url: nats://localhost:4222\n", "stream.url", ErrConflict},

		{"unknown coordination type", "coordination:\n  type: zookeeper\n", "coordination.type", ErrUnknownValue},

		// HALF A KEYPAIR dials and is refused by the broker with an
		// error naming neither file — which is the one shape of TLS
		// misconfiguration a config can catch on a laptop.
		{
			"stream client cert with no key",
			"stream:\n  type: nats\n  url: nats://x:4222\n  tls:\n    cert: /etc/c.pem\n" +
				"coordination:\n  type: embedded-kv\n",
			"stream.tls.key", ErrMissing,
		},
		{
			"stream client key with no cert",
			"stream:\n  type: nats\n  url: nats://x:4222\n  tls:\n    key: /etc/k.pem\n" +
				"coordination:\n  type: embedded-kv\n",
			"stream.tls.cert", ErrMissing,
		},
		{"port out of range", "api:\n  port: 70000\n", "api.port", ErrOutOfRange},
		{"token with no id", "api:\n  auth:\n    tokens:\n      - id: \"\"\n        token: abc\n", "api.auth.tokens[0].id", ErrMissing},
		{"token with no value", "api:\n  auth:\n    tokens:\n      - id: founder\n        token: \"\"\n", "api.auth.tokens[0].token", ErrMissing},
		{"duplicate token id", "api:\n  auth:\n    tokens:\n      - {id: founder, token: a}\n      - {id: founder, token: b}\n", "api.auth.tokens[1].id", ErrConflict},

		{"active key names nothing", "secrets:\n  active_key_id: nope\n  keys:\n    - {id: k1, material: bWF0}\n", "secrets.active_key_id", ErrUnknownValue},
		{"keys with no active id", "secrets:\n  keys:\n    - {id: k1, material: bWF0}\n", "secrets.active_key_id", ErrMissing},
		{"key id with a colon", "secrets:\n  active_key_id: \"a:b\"\n  keys:\n    - {id: \"a:b\", material: bWF0}\n", "secrets.keys[0].id", ErrUnknownValue},
		{"key with no material", "secrets:\n  active_key_id: k1\n  keys:\n    - {id: k1, material: \"\"}\n", "secrets.keys[0].material", ErrMissing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := rejectsBootstrap(t, tc.yaml, tc.path)
			if !errors.Is(err, tc.kind) {
				t.Fatalf("want %v, got %v", tc.kind, err)
			}
		})
	}
}

// The two-slot rules. Each of these fails LATER as something that looks
// like a different problem entirely, which is why they are decided here
// where both halves of the deployment are named in one file.
func TestBootstrapTopologyRules(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		yaml string
		path string
		want string
	}{
		{
			name: "a fleet on local coordination",
			yaml: "stream:\n  cluster:\n    name: acme\n    peers: [nats://a:6222, nats://b:6222, nats://c:6222]\n",
			path: "coordination.type",
			want: "every node in a fleet would claim every seat",
		},
		{
			name: "a two-node fleet",
			yaml: "coordination:\n  type: embedded-kv\nstream:\n  cluster:\n    name: acme\n    peers: [nats://b:6222]\n",
			path: "stream.cluster.peers",
			want: "no coordination quorum",
		},
		{
			name: "replicas with nobody to replicate to",
			yaml: "stream:\n  replicas: 3\n",
			path: "stream.replicas",
			want: "needs peers",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := rejectsBootstrap(t, tc.yaml, tc.path)
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the error must say why; got %v", err)
			}
		})
	}
}

// One node and three nodes are both supported; only two is refused.
func TestSupportedTopologiesLoad(t *testing.T) {
	t.Parallel()
	for _, doc := range []string{
		"",
		"coordination:\n  type: local\n",
		// Three members, which is the fleet shape: a clustered embedded
		// stream carrying its own coordination, and an external one.
		"coordination:\n  type: embedded-kv\nstream:\n  replicas: 3\n  cluster:\n    name: acme\n    peers: [nats://b:6222, nats://c:6222]\n",
		"stream:\n  type: nats\n  url: nats://localhost:4222\ncoordination:\n  type: embedded-kv\n",
		// An external cluster asking for replicas. Its membership is not
		// in this file and cannot be — the url names an address, not a
		// member list — so the peers rule must not reach it. It did, and
		// the cost was silent: every external-NATS fleet was capped at
		// one copy of its streams AND of its lease and fleet KV buckets,
		// so the seat mailboxes and every lease lived on whichever single
		// server held them and died with it.
		"stream:\n  type: nats\n  url: nats://localhost:4222\n  replicas: 3\ncoordination:\n  type: embedded-kv\n",
	} {
		if _, err := ParseBootstrap([]byte(doc), EnvOnly()); err != nil {
			t.Fatalf("%q should load:\n%v", doc, err)
		}
	}
}

// The node id is stable across restarts because things register under it.
func TestResolveNodeIDPrecedence(t *testing.T) {
	t.Run("defaults to the single-process id", func(t *testing.T) {
		r := NewResolver(MapSource{})
		got, err := ResolveNodeID(&Bootstrap{}, r)
		if err != nil || got != DefaultNodeID {
			t.Fatalf("got %q, %v", got, err)
		}
		got, err = ResolveNodeID(nil, r)
		if err != nil || got != DefaultNodeID {
			t.Fatalf("nil bootstrap: got %q, %v", got, err)
		}
	})

	t.Run("config wins over the environment", func(t *testing.T) {
		r := NewResolver(MapSource{NodeIDEnvVar: "from-env"})
		b := &Bootstrap{Node: Node{ID: "from-config"}}
		if got, _ := ResolveNodeID(b, r); got != "from-config" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("the environment fills in", func(t *testing.T) {
		// How an orchestrator injects a pod name without templating the
		// config file.
		r := NewResolver(MapSource{NodeIDEnvVar: "crewlet-2"})
		if got, _ := ResolveNodeID(&Bootstrap{}, r); got != "crewlet-2" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("a reference resolves", func(t *testing.T) {
		r := NewResolver(MapSource{"MY_POD": "crewlet-7"})
		b := &Bootstrap{Node: Node{ID: "${MY_POD}"}}
		if got, _ := ResolveNodeID(b, r); got != "crewlet-7" {
			t.Fatalf("got %q", got)
		}
	})

	t.Run("a resolved value is still checked", func(t *testing.T) {
		// An orchestrator injecting a name with a '/' must fail loudly at
		// boot rather than surface later as a malformed consumer name.
		r := NewResolver(MapSource{"MY_POD": "bad/name"})
		b := &Bootstrap{Node: Node{ID: "${MY_POD}"}}
		if _, err := ResolveNodeID(b, r); err == nil {
			t.Fatal("a malformed resolved id was accepted")
		}
	})
}

// A holder identity names one INCARNATION, so a replacement process is
// never mistaken for its predecessor — which is what stops two engines
// holding one seat at the same fencing epoch.
func TestIncarnationIsUniquePerCall(t *testing.T) {
	t.Parallel()
	a := NewIncarnation("node-0")
	b := NewIncarnation("node-0")
	if a == b {
		t.Fatal("two incarnations of one node must differ")
	}
	for _, got := range []string{a, b} {
		if !strings.HasPrefix(got, "node-0:") {
			t.Fatalf("an incarnation must carry its node id: %q", got)
		}
	}
}

// Omitting node.roles means every role, which is the single-process
// deployment. Reading it as "no roles" would be a node that does nothing.
func TestUndeclaredRolesMeanEveryRole(t *testing.T) {
	t.Parallel()
	cfg, err := ParseBootstrap(nil, EnvOnly())
	if err != nil {
		t.Fatal(err)
	}
	roles, err := cfg.Node.RoleSet()
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []placement.NodeRole{placement.RoleIngress, placement.RoleSeats, placement.RoleWorkers} {
		if !roles.Has(role) {
			t.Fatalf("an undeclared node must run %s", role)
		}
	}
}

func TestNodeProfileCarriesRolesAndLabels(t *testing.T) {
	t.Parallel()
	cfg, err := ParseBootstrap([]byte("node:\n  roles: [seats]\n  labels:\n    zone: eu\n"), EnvOnly())
	if err != nil {
		t.Fatal(err)
	}
	profile := cfg.Node.Profile("node-7")
	if !profile.RunsSeats() || profile.RunsIngress() {
		t.Fatalf("profile roles = %v", profile.Roles.Names())
	}
	if profile.Labels["zone"] != "eu" {
		t.Fatalf("labels = %v", profile.Labels)
	}
}

// THE SECONDS ACCESSORS ARE THE ONE CONVERSION.
//
// Each of these had zero callers while five sites hand-rolled the same
// `float64(time.Second)` arithmetic — one slip from being off by 10^9, with
// the compiler accepting both spellings. CLAUDE.md's rule is that a field
// named ...Seconds is converted once, at the edge.
func TestTheDurationAccessorsConvertTheirOwnUnits(t *testing.T) {
	t.Parallel()
	store := &Store{BusyTimeoutSeconds: 2.5}
	if got := store.BusyTimeout(); got != 2500*time.Millisecond {
		t.Errorf("BusyTimeout = %v, want 2.5s", got)
	}
	coord := &Coordination{LeaseTTLSeconds: 45}
	if got := coord.LeaseTTL(); got != 45*time.Second {
		t.Errorf("LeaseTTL = %v, want 45s", got)
	}
	// HOURS, not seconds — the one field here measured differently, and
	// the reason a shared helper would be wrong.
	stream := &Stream{EventRetentionHours: 30 * 24}
	if got := stream.EventRetention(); got != 30*24*time.Hour {
		t.Errorf("EventRetention = %v, want 720h", got)
	}
}

// Zero passes straight through, because zero is what every caller reads as
// "take the default" — and the accessor deliberately does not apply it: the
// defaults live with the subsystems that own them, which config must not
// import.
func TestAZeroDurationAccessorStaysZero(t *testing.T) {
	t.Parallel()
	if got := (&Store{}).BusyTimeout(); got != 0 {
		t.Errorf("BusyTimeout = %v, want 0", got)
	}
	if got := (&Coordination{}).LeaseTTL(); got != 0 {
		t.Errorf("LeaseTTL = %v, want 0", got)
	}
	if got := (&Stream{}).EventRetention(); got != 0 {
		t.Errorf("EventRetention = %v, want 0", got)
	}
}
