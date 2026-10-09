package config

import (
	"errors"
	"testing"
)

// THE PUBLIC LISTENER LOADS in the shapes the deployment guide writes it: the
// admin API on a private address and the public routes on every interface,
// both on one wildcard bind with two ports, and two specific addresses sharing
// a port — which are two sockets, one per interface.
func TestThePublicListenerLoads(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		doc  string
		addr string
	}{
		"private admin, public routes everywhere": {
			"api:\n  host: 10.0.0.5\n  port: 8080\n  public:\n    port: 8443\n", ":8443"},
		"two ports on every interface": {
			"api:\n  port: 8080\n  public:\n    host: 0.0.0.0\n    port: 8443\n", "0.0.0.0:8443"},
		"one port on two interfaces": {
			"api:\n  host: 10.0.0.5\n  port: 8080\n  public:\n    host: 203.0.113.7\n    port: 8080\n",
			"203.0.113.7:8080"},
		// ONE FILE FOR EVERY ROLE: a node without ingress serves its bridge
		// on the public listener, so the block is not refused there.
		"a node without ingress": {
			"node:\n  roles: [data, seats]\napi:\n  port: 8080\n  public:\n    port: 8443\n", ":8443"},
		// AND ITS api.port MAY BE 0: the public listener's api.port rule is
		// about the probes an ingress node serves, and this node serves none.
		"a node without ingress and no HTTP surface": {
			"node:\n  roles: [data, seats, workers]\napi:\n  port: 0\n  public:\n    port: 8443\n", ":8443"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			b, err := ParseBootstrap([]byte(tc.doc), EnvOnly())
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !b.API.Public.Enabled() {
				t.Fatal("api.public is set and reads as not enabled")
			}
			if got := b.API.Public.Addr(); got != tc.addr {
				t.Fatalf("public listener binds %q, want %q", got, tc.addr)
			}
		})
	}
}

// UNSET IS NO PUBLIC LISTENER, so every route stays on api.port — the default
// every existing deployment runs.
func TestNoPublicBlockKeepsEveryRouteOnTheAPIPort(t *testing.T) {
	t.Parallel()
	b, err := ParseBootstrap([]byte("api:\n  port: 8080\n"), EnvOnly())
	if err != nil {
		t.Fatal(err)
	}
	if b.API.Public.Enabled() {
		t.Fatal("no api.public block, and the public listener reads as enabled")
	}
}

// EVERY PUBLIC LISTENER THAT CANNOT WORK IS REFUSED at validate, naming the
// field: each would otherwise fail at bind, or — the port shared with the API
// on one address — silently be the admin socket the block exists to separate.
func TestAPublicListenerThatCannotWorkIsRefused(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name, doc, path string
		kind            error
	}{
		{"the API's own port",
			"api:\n  port: 8080\n  public:\n    port: 8080\n", "api.public.port", ErrConflict},
		{"the API's port, the API on every interface",
			"api:\n  host: 0.0.0.0\n  port: 8080\n  public:\n    host: 203.0.113.7\n    port: 8080\n",
			"api.public.port", ErrConflict},
		{"the API's port, one address spelled two ways",
			"api:\n  host: \"::1\"\n  port: 8080\n  public:\n    host: \"[0:0:0:0:0:0:0:1]\"\n    port: 8080\n",
			"api.public.port", ErrConflict},
		{"the cluster route port",
			"api:\n  port: 8080\n  public:\n    port: 6222\nstream:\n  store_dir: /var/js\n" +
				"  cluster:\n    name: c\n    port: 6222\n    peers: [\"nats://b:6222\", \"nats://c:6222\"]\n" +
				"coordination:\n  type: embedded-kv\n",
			"stream.cluster.port", ErrConflict},
		{"the leaf listener",
			"api:\n  port: 8080\n  public:\n    port: 7422\nstream:\n  store_dir: /var/js\n" +
				"  leaf:\n    port: 7422\ncoordination:\n  type: embedded-kv\n",
			"stream.leaf.port", ErrConflict},
		{"a host and no port",
			"api:\n  port: 8080\n  public:\n    host: 203.0.113.7\n", "api.public.port", ErrMissing},
		{"out of range",
			"api:\n  port: 8080\n  public:\n    port: 70000\n", "api.public.port", ErrOutOfRange},
		// api.port 0 is no HTTP surface at all: the public block moves
		// routes off it, and an ingress node with only the public routes
		// answers no probe. Unset roles are every role, ingress included.
		{"no API port",
			"api:\n  port: 0\n  public:\n    port: 8443\n", "api.public.port", ErrConflict},
		{"no API port on a node naming ingress",
			"node:\n  roles: [data, ingress]\napi:\n  port: 0\n  public:\n    port: 8443\n",
			"api.public.port", ErrConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := rejectsBootstrap(t, tc.doc, tc.path)
			if !errors.Is(err, tc.kind) {
				t.Fatalf("want %v, got %v", tc.kind, err)
			}
		})
	}
}

// THE API PORT AND THE BROKER'S OWN LISTENERS ARE HELD APART TOO: the rule is
// every socket this file opens, so the collision the public block made easy to
// write is not the only one caught.
func TestTheAPIPortCannotBeTheBrokersPort(t *testing.T) {
	t.Parallel()
	err := rejectsBootstrap(t, "api:\n  port: 7422\nstream:\n  store_dir: /var/js\n"+
		"  leaf:\n    port: 7422\ncoordination:\n  type: embedded-kv\n", "stream.leaf.port")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("want a conflict, got %v", err)
	}
}

// AN EXTERNAL CLUSTER'S PORTS ARE NOT THIS HOST'S: a url naming the same
// number as a listener here is a socket on another machine.
func TestAnExternalBrokersPortsAreNotThisHosts(t *testing.T) {
	t.Parallel()
	doc := "node:\n  roles: [seats]\nstore:\n  scratch: true\n" +
		"stream:\n  type: nats\n  url: nats://nats.example.com:8443\n" +
		"coordination:\n  type: embedded-kv\napi:\n  port: 8080\n  public:\n    port: 8443\n"
	if _, err := ParseBootstrap([]byte(doc), EnvOnly()); err != nil {
		t.Fatalf("refused: %v", err)
	}
}
