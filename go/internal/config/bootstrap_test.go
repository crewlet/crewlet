package config

import (
	"errors"
	"strings"
	"testing"

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
		{"unknown driver", "store:\n  driver: postgres\n", "store.driver", ErrUnknownValue},
		{"negative pool", "store:\n  max_open_conns: -1\n", "store.max_open_conns", ErrOutOfRange},

		{"unknown stream type", "stream:\n  type: kafka\n", "stream.type", ErrUnknownValue},
		{"external stream with no url", "stream:\n  type: nats\ncoordination:\n  type: embedded-kv\n", "stream.url", ErrMissing},
		{"embedded stream with a url", "stream:\n  url: nats://localhost:4222\n", "stream.url", ErrConflict},
		{"pulsar tenant on a nats stream", "stream:\n  type: nats\n  url: nats://x:4222\n  tenant: acme\ncoordination:\n  type: embedded-kv\n", "stream.tenant", ErrConflict},

		{"unknown coordination type", "coordination:\n  type: zookeeper\n", "coordination.type", ErrUnknownValue},

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
			name: "pulsar filling the coordination slot",
			yaml: "stream:\n  type: pulsar\n  url: pulsar://localhost:6650\n",
			path: "coordination.type",
			want: "compare-and-set",
		},
		{
			name: "replicas with nobody to replicate to",
			yaml: "stream:\n  replicas: 3\n",
			path: "stream.replicas",
			want: "needs peers",
		},
		{
			// The gap that made every Pulsar topology unrunnable: the
			// slot was chosen and there was nowhere for it to live.
			name: "pulsar with nowhere to keep its leases",
			yaml: "coordination:\n  type: embedded-kv\nstream:\n  type: pulsar\n  url: pulsar://localhost:6650\n  tenant: acme\n  namespace: default\n",
			path: "coordination.nats",
			want: "leases need a NATS estate",
		},
		{
			// Read by nobody. On a NATS stream the coordination store
			// rides the stream's own connection, deliberately.
			name: "a coordination estate on a stream that already carries one",
			yaml: "coordination:\n  type: embedded-kv\n  nats:\n    url: nats://elsewhere:4222\n",
			path: "coordination.nats",
			want: "already carries coordination",
		},
		{
			name: "a coordination estate with local coordination",
			yaml: "coordination:\n  type: local\n  nats:\n    url: nats://elsewhere:4222\n",
			path: "coordination.nats",
			want: "keeps leases on a NATS estate",
		},
		{
			// Two different statements about where the leases live.
			name: "a coordination estate that is both dialled and embedded",
			yaml: "coordination:\n  type: embedded-kv\n  nats:\n    url: nats://elsewhere:4222\n    store_dir: /tmp/coord\nstream:\n  type: pulsar\n  url: pulsar://localhost:6650\n  tenant: acme\n  namespace: default\n",
			path: "coordination.nats.store_dir",
			want: "stores nothing locally",
		},
		{
			// The quorum check used to read stream.cluster.peers, which
			// on a Pulsar topology describes a cluster that does not
			// hold the leases — so a two-member lease cluster counted as
			// one node and passed.
			name: "a two-member lease cluster on a pulsar stream",
			yaml: "coordination:\n  type: embedded-kv\n  nats:\n    cluster:\n      name: coord\n      peers: [nats://b:6222]\nstream:\n  type: pulsar\n  url: pulsar://localhost:6650\n  tenant: acme\n  namespace: default\n",
			path: "coordination.nats.cluster.peers",
			want: "no coordination quorum",
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
		// A Pulsar topology, which could not run at all until the
		// coordination estate existed: the slot was validated, documented
		// and refused at open. Both shapes of estate load.
		"coordination:\n  type: embedded-kv\n  nats:\n    store_dir: /var/lib/crewlet/coord\nstream:\n  type: pulsar\n  url: pulsar://localhost:6650\n  tenant: acme\n  namespace: default\n",
		"coordination:\n  type: embedded-kv\n  nats:\n    url: nats://coord:4222\nstream:\n  type: pulsar\n  url: pulsar://localhost:6650\n  tenant: acme\n  namespace: default\n",
		// Three lease members, which is the fleet shape.
		"coordination:\n  type: embedded-kv\n  nats:\n    store_dir: /var/lib/crewlet/coord\n    replicas: 3\n    cluster:\n      name: coord\n      peers: [nats://b:6222, nats://c:6222]\nstream:\n  type: pulsar\n  url: pulsar://localhost:6650\n  tenant: acme\n  namespace: default\n",
		"coordination:\n  type: embedded-kv\nstream:\n  replicas: 3\n  cluster:\n    name: acme\n    peers: [nats://b:6222, nats://c:6222]\n",
		"stream:\n  type: nats\n  url: nats://localhost:4222\ncoordination:\n  type: embedded-kv\n",
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
