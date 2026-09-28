package config

import (
	"errors"
	"slices"
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
		// ONE KEY GRAMMAR, the placement vocabulary's: the object map's
		// failure domain names one of these keys, so a key this accepted
		// and the map refused would be a domain nobody could name.
		{"label key with a space", "node:\n  labels:\n    \"my zone\": eu\n", "node.labels.my zone", ErrUnknownValue},
		{"label key too long", "node:\n  labels:\n    " + strings.Repeat("k", 64) + ": eu\n", "node.labels." + strings.Repeat("k", 64), ErrUnknownValue},

		{"no store path", "store:\n  path: \"\"\n", "store.path", ErrMissing},
		{"negative pool", "store:\n  max_open_conns: -1\n", "store.max_open_conns", ErrOutOfRange},

		{"unknown stream type", "stream:\n  type: kafka\n", "stream.type", ErrUnknownValue},
		// A CLUSTER BLOCK WITHOUT A NAME CONFIGURES NOTHING: the embedded
		// server takes its route port, its bind interface, its advertise
		// address and its peers only from a NAMED cluster, so a node
		// written this way starts solo, binds no route listener and forms
		// no cluster — while every other reading of the same file calls it
		// clustered.
		//
		// EVERY FIELD, because the guard first shipped naming two of them:
		// host and advertise, the two an operator reaches for after
		// reading this block's own warnings about unauthenticated cluster
		// access and about NAT, were the two it silently accepted.
		{
			"cluster port with no cluster name",
			"stream:\n  cluster:\n    port: 6222\n",
			"stream.cluster.name", ErrMissing,
		},
		{
			"cluster peers with no cluster name",
			"stream:\n  cluster:\n    peers: [\"nats://a:6222\"]\n",
			"stream.cluster.name", ErrMissing,
		},
		{
			"cluster host with no cluster name",
			"stream:\n  cluster:\n    host: 10.0.0.4\n",
			"stream.cluster.name", ErrMissing,
		},
		{
			"cluster advertise with no cluster name",
			"stream:\n  cluster:\n    advertise: nat.example.com:6222\n",
			"stream.cluster.name", ErrMissing,
		},
		{"external stream with no url", "stream:\n  type: nats\ncoordination:\n  type: embedded-kv\n", "stream.url", ErrMissing},
		{"embedded stream with a url", "stream:\n  url: nats://localhost:4222\n", "stream.url", ErrConflict},
		// `debug` starts the EMBEDDED server verbose. Against an external
		// cluster it reaches nothing, so it is refused for the reason
		// `url` and `store_dir` are refused the other way round: a flag
		// nobody reads is the classic "I configured it and nothing
		// happened".
		{
			"external stream with the embedded broker's debug flag",
			"stream:\n  type: nats\n  url: nats://x:4222\n  debug: true\n" +
				"coordination:\n  type: embedded-kv\n",
			"stream.debug", ErrConflict,
		},

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

		// A CORS ALLOW-LIST IS COMPARED AGAINST THE BROWSER'S `Origin`
		// HEADER EXACTLY, and that header is always `scheme://host[:port]`
		// with no path and no trailing slash. Every shape below is one an
		// operator plausibly writes and no browser can ever equal, so
		// accepting it produces an allow-list that looks configured and a
		// fetch that fails in a console this engine never sees.
		// THE WILDCARD IS REFUSED RATHER THAN HONOURED, and the refusal
		// says what honouring it would expose: it was this field's own
		// previous default, and what it does is let any site a logged-in
		// operator visits read LLM transcripts, diary entries and the
		// whole event stream.
		{"wildcard origin", "api:\n  auth:\n    allowed_origins: ['*']\n", "api.auth.allowed_origins[0]", ErrShape},
		{"origin with no scheme", "api:\n  auth:\n    allowed_origins: [ops.example.com]\n", "api.auth.allowed_origins[0]", ErrShape},
		{"origin with a trailing slash", "api:\n  auth:\n    allowed_origins: ['https://ops.example.com/']\n", "api.auth.allowed_origins[0]", ErrShape},
		{"origin with a path", "api:\n  auth:\n    allowed_origins: ['https://ops.example.com/dashboard']\n", "api.auth.allowed_origins[0]", ErrShape},
		{"empty origin", "api:\n  auth:\n    allowed_origins: ['']\n", "api.auth.allowed_origins[0]", ErrMissing},

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
// AND THE WILDCARD'S REFUSAL NAMES WHAT IT WOULD COST.
//
// Every other bad origin here is a typo. `*` is a DECISION — the one an
// operator makes on purpose, having read that CORS is blocking them — so the
// refusal has to be the place they learn what it opens, or they will reach
// for `api.auth.disabled` instead.
func TestTheWildcardOriginRefusalSaysWhatItWouldExpose(t *testing.T) {
	t.Parallel()
	_, err := ParseBootstrap([]byte("api:\n  auth:\n    allowed_origins: ['*']\n"), EnvOnly())
	if err == nil {
		t.Fatal("a wildcard origin was accepted")
	}
	for _, want := range []string{"any site", "https://ops.example.com"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal does not mention %q: %v", want, err)
		}
	}
}

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
		// AN EMPTY COORDINATION TYPE IS LOCAL, and so a fleet on it is
		// refused: it used to escape this rule — neither "local" nor
		// "embedded-kv" — while the engine ran it as local, every node of
		// the fleet claiming every seat.
		{
			name: "a fleet on an empty coordination type",
			yaml: "coordination:\n  type: \"\"\nstream:\n  cluster:\n    name: acme\n    peers: [nats://a:6222, nats://b:6222]\n",
			path: "coordination.type",
			want: "every node in a fleet would claim every seat",
		},
		// AN EMPTY STREAM TYPE IS EMBEDDED, including to the rule that
		// needs members for copies.
		{
			name: "replicas on an empty stream type with nobody to replicate to",
			yaml: "stream:\n  type: \"\"\n  replicas: 3\n",
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

// A PEER LIST COUNTS THE OTHER MEMBERS, and only them. Operators paste one
// list of every member into every node's file, so a node's own route arrives
// in its own peers — and counted as a member, it passed a two-node fleet as
// three and let three copies onto two members. Every rule that counts members
// is asked here, each with the count it gets wrong when the entry is counted.
func TestAPeerListCountsOnlyTheOtherMembers(t *testing.T) {
	t.Parallel()
	const kv = "coordination:\n  type: embedded-kv\n"
	cluster := func(extra, peers string) string {
		return "stream:\n  store_dir: /var/lib/crewlet/stream\n" + extra +
			"  cluster:\n    name: acme\n    port: 6222\n" +
			"    host: 10.0.0.11\n    advertise: node-a.internal\n    peers: [" + peers + "]\n"
	}
	for _, tc := range []struct {
		name, yaml, path, says string
	}{
		{
			// THE QUORUM RULE, with this node matched by its bound listener.
			name: "a two-node fleet that lists itself",
			yaml: kv + cluster("", "nats://10.0.0.11:6222, nats://node-b.internal:6222"),
			path: "stream.cluster.peers",
			says: "not counting peers[0]",
		},
		{
			// By its advertised address, whose bare host keeps the route
			// port — and with the scheme, the case and the port's leading
			// zero all different from what this node wrote.
			name: "a two-node fleet that lists itself by its advertised name",
			yaml: kv + cluster("", "nats://node-b.internal:6222, NATS-ROUTE://Node-A.Internal:06222"),
			path: "stream.cluster.peers",
			says: "which is this node's own route (cluster.advertise)",
		},
		{
			// An IPv6 listener, written two ways.
			name: "a two-node fleet that lists itself by an IPv6 literal",
			yaml: kv + "stream:\n  cluster:\n    name: acme\n    port: 6222\n    host: \"fd00::11\"\n" +
				"    peers: [\"nats://[fd00:0:0::11]:6222\", nats://node-b.internal:6222]\n",
			path: "stream.cluster.peers",
			says: "(cluster.host and cluster.port)",
		},
		{
			name: "a two-node fleet that lists its other member twice",
			yaml: kv + cluster("", "nats://node-b.internal:6222, nats://NODE-B.internal:6222"),
			path: "stream.cluster.peers",
			says: "which repeats peers[0]",
		},
		{
			// THE REPLICA RULE: three copies on two members.
			name: "three copies on two members, one of them listed twice",
			yaml: kv + cluster("  replicas: 3\n", "nats://node-b.internal:6222, nats://node-b.internal:6222"),
			path: "stream.replicas",
			says: "this cluster names 2",
		},
		{
			name: "three copies on two members, this one listed",
			yaml: "stream:\n  replicas: 3\n  cluster:\n    name: acme\n    port: 6222\n    host: 10.0.0.11\n" +
				"    peers: [nats://10.0.0.11:6222, nats://node-b.internal:6222]\n" + kv,
			path: "stream.replicas",
			says: "this cluster names 2",
		},
		{
			// A LIST OF NOBODY BUT THIS NODE.
			name: "a peer list naming only this node",
			yaml: kv + cluster("", "nats://node-a.internal:6222"),
			path: "stream.cluster.peers",
			says: "names no member but this one",
		},
		{
			// THE SAME-HOST RULE, over the other members: they are all on
			// this host, and this node's own entry, spelled with its
			// private address, used to hide that.
			name: "a declined fsync on a cluster whose other members are all local",
			yaml: kv + "stream:\n  replicas: 3\n  sync: 30s\n  cluster:\n    name: acme\n    port: 6222\n" +
				"    host: 10.0.0.11\n    peers: [nats://10.0.0.11:6222, nats://localhost:6223, nats://127.0.0.1:6224]\n",
			path: "stream.sync",
			says: "every other member",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := rejectsBootstrap(t, tc.yaml, tc.path)
			if !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("the refusal must say %q; got %v", tc.says, err)
			}
		})
	}

	// AND WHAT THE COUNT CANNOT RECOGNISE IT COUNTS. An alias of this host
	// is indistinguishable from another member in this file, so the list
	// below is three members to every rule — and it loads, which is the
	// documented limit rather than a case the discount covers.
	for _, doc := range []string{
		kv + cluster("  replicas: 3\n", "nats://10.0.0.11:6222, nats://node-b.internal:6222, nats://node-c.internal:6222"),
		kv + cluster("", "nats://localhost:6222, nats://node-b.internal:6222"),
		// A listener bound to every interface is no address to match.
		kv + "stream:\n  store_dir: /var/lib/crewlet/stream\n  cluster:\n    name: acme\n" +
			"    port: 6222\n    host: 0.0.0.0\n" +
			"    peers: [nats://0.0.0.0:6222, nats://node-b.internal:6222]\n",
	} {
		if _, err := ParseBootstrap([]byte(doc), EnvOnly()); err != nil {
			t.Errorf("%q should load:\n%v", doc, err)
		}
	}
}

// A PEER IS A ROUTE THE SERVER CAN DIAL. nats-server drops an entry that does
// not parse and retries one with no host or no port for ever, so both leave a
// cluster short of the member the entry was meant to reach with nothing but a
// debug line to say so.
func TestAPeerTheServerCannotDialIsRefused(t *testing.T) {
	t.Parallel()
	for _, peer := range []string{
		"node-b.internal:6222", // no scheme: a scheme "node-b.internal" and no host
		"nats://node-b.internal",
		"nats://:6222",
		"nats://node-b.internal:0",
		"nats://node-b.internal:70000",
		"nats://node-b.internal:route",
	} {
		t.Run(peer, func(t *testing.T) {
			t.Parallel()
			doc := "coordination:\n  type: embedded-kv\nstream:\n  cluster:\n    name: acme\n" +
				"    peers: [\"nats://node-c.internal:6222\", \"" + peer + "\", \"nats://node-d.internal:6222\"]\n"
			err := rejectsBootstrap(t, doc, "stream.cluster.peers[1]")
			if !strings.Contains(err.Error(), "route URL") {
				t.Fatalf("the refusal must say what a peer is; got %v", err)
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
		"coordination:\n  type: embedded-kv\nstream:\n  replicas: 3\n  store_dir: /var/lib/crewlet/stream\n" +
			"  cluster:\n    name: acme\n    peers: [nats://b:6222, nats://c:6222]\n",
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
	for _, role := range []placement.NodeRole{placement.RoleData, placement.RoleIngress, placement.RoleSeats, placement.RoleWorkers} {
		if !roles.Has(role) {
			t.Fatalf("an undeclared node must run %s", role)
		}
	}
}

// EVERY BAD NODE LABEL KEY IS REPORTED, IN THE SAME ORDER ON EVERY RUN — the
// Tier A half of TestEveryBadPlacementKeyIsReportedInOneStableOrder. A problem
// list that reshuffled with map order would make two validations of one file
// print different output, and `crewlet validate` diffed across a change would
// show churn where nothing moved.
func TestEveryBadNodeLabelKeyIsReportedInOneStableOrder(t *testing.T) {
	t.Parallel()
	keys := []string{"a a", "b b", "c c", "d d", "e e", "f f"}
	var doc strings.Builder
	doc.WriteString("node:\n  labels:\n")
	for _, k := range slices.Backward(keys) {
		doc.WriteString("    \"" + k + "\": x\n")
	}
	// The empty key sorts first and is placed on the map itself.
	doc.WriteString("    \"\": x\n")
	want := []string{"node.labels"}
	for _, k := range keys {
		want = append(want, "node.labels."+k)
	}
	// Repeated because map order is random per iteration: an unsorted walk
	// matches the sorted one by chance on some runs, never on twenty.
	for range 20 {
		_, err := ParseBootstrap([]byte(doc.String()), EnvOnly())
		var got []string
		for _, p := range Problems(err) {
			got = append(got, p.Path)
		}
		if !slices.Equal(got, want) {
			t.Fatalf("problems at %q, want %q", got, want)
		}
	}
}

func TestNodeProfileCarriesRolesAndLabels(t *testing.T) {
	t.Parallel()
	cfg, err := ParseBootstrap([]byte("node:\n  roles: [seats]\n  labels:\n    zone: eu\n"+
		"store:\n  path: /tmp/n.db\n  scratch: true\n"+
		"stream:\n  leaf:\n    urls: [nats-leaf://data-a.internal:7422]\n"+
		"coordination:\n  type: embedded-kv\n"), EnvOnly())
	if err != nil {
		t.Fatal(err)
	}
	profile := cfg.Profile("node-7")
	if !profile.RunsSeats() || profile.RunsIngress() || profile.HoldsData() {
		t.Fatalf("profile roles = %v", profile.Roles.Names())
	}
	if profile.Broker != placement.BrokerLeaf {
		t.Fatalf("a node joining through leaf urls advertises broker %q", profile.Broker)
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

// AN EMPTY TYPE IS THE DEFAULT ONE, decided once, before any rule reads it.
//
// `stream.type: ""` is how a file — or a ${VAR} that resolved to nothing —
// says "unset", and the default is embedded. One rule read it as CLUSTERED
// (anything that is not embedded) while every other reader, and the engine,
// took it as embedded — so a single node saying it was refused as a fleet on
// local coordination. Both types now carry their defaults out of the loader,
// so nothing downstream reads an empty one at all.
func TestAnEmptyTypeIsTheDefaultOne(t *testing.T) {
	t.Parallel()
	cfg, err := ParseBootstrap([]byte("stream:\n  type: \"\"\ncoordination:\n  type: \"\"\n"), NewResolver(MapSource{}))
	if err != nil {
		t.Fatalf("a single node with empty types was refused: %v", err)
	}
	if cfg.Stream.Type != StreamEmbedded || cfg.Coordination.Type != CoordinationLocal {
		t.Fatalf("types = (%q, %q), want (%q, %q)", cfg.Stream.Type, cfg.Coordination.Type,
			StreamEmbedded, CoordinationLocal)
	}
	cfg, err = ParseBootstrap([]byte("stream:\n  type: \"${STREAM_TYPE}\"\n"), NewResolver(MapSource{"STREAM_TYPE": ""}))
	if err != nil || cfg.Stream.Type != StreamEmbedded {
		t.Fatalf("a stream type from an empty variable = (%v, %v), want the embedded default", cfg, err)
	}
}
