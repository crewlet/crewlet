package config

import (
	"errors"
	"strings"
	"testing"
)

// publicAddress is the vendor address a public listener on an ingress node
// names.
const publicAddress = "https://hooks.example.com"

// servedPublic is [Served] over doc, with the public listener's own address
// written under its `public:` block when it has one: what an ingress node with
// a public listener must state beside its ports, for a case about something
// else.
func servedPublic(doc string) string {
	return strings.Replace(Served(doc), "  public:\n",
		"  public:\n    external_url: "+publicAddress+"\n", 1)
}

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
			b, err := ParseRunnableBootstrap([]byte(servedPublic(tc.doc)), EnvOnly())
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
	b, err := ParseRunnableBootstrap([]byte(Served("api:\n  port: 8080\n")), EnvOnly())
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
			// Each case carries what serving the API asks for, so the one
			// refusal it names is the only one it earns.
			err := rejectsBootstrap(t, servedPublic(tc.doc), tc.path)
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
	err := rejectsBootstrap(t, Served("api:\n  port: 7422\nstream:\n  store_dir: /var/js\n"+
		"  leaf:\n    port: 7422\ncoordination:\n  type: embedded-kv\n"), "stream.leaf.port")
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("want a conflict, got %v", err)
	}
}

// AN EXTERNAL CLUSTER'S PORTS ARE NOT THIS HOST'S: a url naming the same
// number as a listener here is a socket on another machine.
func TestAnExternalBrokersPortsAreNotThisHosts(t *testing.T) {
	t.Parallel()
	doc := "node:\n  roles: [seats]\nstore:\n  scratch: true\n" +
		"stream:\n  type: nats\n  url: nats://nats.example.com:8443\n  credentials: /etc/crewlet/engine.creds\n" +
		"coordination:\n  type: embedded-kv\napi:\n  port: 8080\n  public:\n    port: 8443\n"
	if _, err := ParseRunnableBootstrap([]byte(doc), EnvOnly()); err != nil {
		t.Fatalf("refused: %v", err)
	}
}

// TWO SOCKETS, TWO ADDRESSES, NEITHER STANDING IN FOR THE OTHER. A person
// reaches the deployment at api.external_url and a vendor or a sandbox box at
// the public listener's own address once there is one, and with a public
// listener those are two sockets, each answering the other's routes 404: every
// webhook, manifest and sandbox endpoint is built on the second, and every
// cookie, origin and link a person follows on the first. A public listener
// with no address of its own composes no vendor address rather than borrowing
// the person's, which is the wrong one exactly where the two differ.
//
// Mutation: fall back to [API.ExternalBase] when the public address is unset
// and the third case is the person's address.
func TestTheWebhookBaseIsThePublicListenersOwnAddress(t *testing.T) {
	t.Parallel()
	both := API{ExternalURL: "https://crewlet.example.com/",
		Public: APIPublic{Port: 8443, ExternalURL: publicAddress + "/"}}
	if got := both.WebhookBase(); got != publicAddress {
		t.Errorf("WebhookBase() = %q, want the public listener's address", got)
	}
	if got := both.ExternalBase(); got != "https://crewlet.example.com" {
		t.Errorf("ExternalBase() = %q, want the person's address", got)
	}
	one := API{ExternalURL: "https://crewlet.example.com"}
	if got := one.WebhookBase(); got != "https://crewlet.example.com" {
		t.Errorf("WebhookBase() = %q with no public listener, want api.external_url: "+
			"one socket serves both", got)
	}
	unnamed := API{ExternalURL: "https://crewlet.example.com",
		Public: APIPublic{Port: 8443}}
	if got := unnamed.WebhookBase(); got != "" {
		t.Errorf("WebhookBase() = %q for a public listener naming no address, want "+
			"\"\": api.external_url answers 404 for every route that listener serves", got)
	}
	// AND THE KEY NAMED FOR IT IS THE ONE IT WAS READ FROM, so an operator
	// told to set the address edits the setting in force — the public
	// listener's even where it names no address yet.
	for _, tc := range []struct {
		name string
		api  API
		want string
	}{
		{"two sockets", both, "api.public.external_url"},
		{"one socket", one, "api.external_url"},
		{"a public listener naming no address", unnamed, "api.public.external_url"},
	} {
		if got := tc.api.WebhookBaseKey(); got != tc.want {
			t.Errorf("%s: WebhookBaseKey() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// A PUBLIC LISTENER ON AN INGRESS NODE NAMES ITS OWN ADDRESS: that node builds
// every address a vendor or a box is handed, and the routes they reach are
// served only on that socket. A node without ingress serves only its seats'
// tool bridge there and builds no vendor address, so the one file every role
// reads is not refused on it.
func TestAPublicListenerOnAnIngressNodeNamesItsOwnAddress(t *testing.T) {
	t.Parallel()
	err := rejectsBootstrap(t, Served("api:\n  port: 8080\n  public:\n    port: 8443\n"),
		"api.public.external_url")
	if !errors.Is(err, ErrMissing) {
		t.Fatalf("want the address missing, got %v", err)
	}
	if _, err := ParseRunnableBootstrap([]byte("node:\n  roles: [data, seats]\n"+
		"api:\n  port: 8080\n  public:\n    port: 8443\n"), EnvOnly()); err != nil {
		t.Fatalf("a node without ingress was asked for a vendor address it never builds: %v", err)
	}
}

// THE PUBLIC ADDRESS IS HELD TO THE SHAPE api.external_url IS, at its own
// field: a value with no scheme registers a hook the vendor reports as healthy
// and delivers nowhere. And one written with no listener to name is refused,
// as a host with no port is: it describes a socket this node opens none of.
func TestAPublicAddressThatCannotBeOneIsRefused(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		public, path string
		kind         error
	}{
		"no scheme": {"    port: 8443\n    external_url: hooks.example.com\n",
			"api.public.external_url", ErrShape},
		"a query": {"    port: 8443\n    external_url: \"https://hooks.example.com?a=b\"\n",
			"api.public.external_url", ErrShape},
		"no listener": {"    external_url: https://hooks.example.com\n",
			"api.public.port", ErrMissing},
		"a port out of range": {"    port: 8443\n    external_url: \"https://hooks.example.com:99999\"\n",
			"api.public.external_url", ErrShape},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := rejectsBootstrap(t, Served("api:\n  port: 8080\n  public:\n"+tc.public), tc.path)
			if !errors.Is(err, tc.kind) {
				t.Fatalf("want %v, got %v", tc.kind, err)
			}
		})
	}
	// AND NOT TO A BROWSER'S: no origin is compared against this address, so
	// a host a browser would rewrite before it sent it is refused only on the
	// person's address and on the allowances.
	doc := Served("api:\n  port: 8080\n  public:\n    port: 8443\n    external_url: https://hooks.bücher.example\n")
	if _, err := ParseRunnableBootstrap([]byte(doc), EnvOnly()); err != nil {
		t.Errorf("a public address outside ASCII was refused, though no browser compares it: %v", err)
	}
}
