package jetstream

import (
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
)

// TestTheEmbeddedServerCarriesWhatTheContractPromises pins the two options
// whose absence no behavioural case provokes, where a Config decides them. An
// unset MaxPayload leaves nats-server's 1 MiB default, which every test payload
// fits, and fails every boot at the queue's own check
// (TestAnEmbeddedBrokerWithoutTheCeilingFailsTheBootAndBlamesTheBuild); an
// unset SyncAlways leaves the file store on a 2-minute background flush, which
// only a power cut tells apart.
func TestTheEmbeddedServerCarriesWhatTheContractPromises(t *testing.T) {
	t.Parallel()

	t.Run("the payload ceiling is the contract's, not the broker's default", func(t *testing.T) {
		t.Parallel()
		cfg := Config{StoreDir: t.TempDir()}
		opts, _, err := embeddedOptions(cfg, authFor(t, cfg))
		if err != nil {
			t.Fatalf("embeddedOptions: %v", err)
		}
		if int(opts.MaxPayload) != queue.MaxPayloadBytes {
			t.Errorf("MaxPayload = %d, want queue.MaxPayloadBytes (%d): a producer "+
				"that sized itself against the contract would have its publish "+
				"refused by the broker",
				opts.MaxPayload, queue.MaxPayloadBytes)
		}
	})

	// THE FSYNC IS THE OPERATOR'S SETTING, never inferred from the replica
	// count: a quorum survives one failure class, but a rack losing power takes
	// a majority together, and a first fleet is three nodes on one rack. The
	// rows assert the replica count has NO effect.
	for _, tc := range []struct {
		name     string
		cfg      Config
		want     bool
		wantSync time.Duration
	}{
		{"unset declines the fsync", Config{}, false, 0},
		{"set at R1", Config{SyncAlways: true, Replicas: 1}, true, 0},
		{"set at R3, where the inference used to refuse it",
			Config{SyncAlways: true, Replicas: 3}, true, 0},
		{"unset at R1, where the inference used to force it",
			Config{Replicas: 1}, false, 0},
		{"an interval is passed through when the fsync is declined",
			Config{SyncInterval: 30 * time.Second}, false, 30 * time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := tc.cfg
			cfg.StoreDir = t.TempDir()
			opts, _, err := embeddedOptions(cfg, authFor(t, cfg))
			if err != nil {
				t.Fatalf("embeddedOptions: %v", err)
			}
			if opts.SyncAlways != tc.want {
				t.Errorf("SyncAlways = %v, want %v: the fsync per write is "+
					"stream.sync's answer and nothing else's",
					opts.SyncAlways, tc.want)
			}
			if opts.SyncInterval != tc.wantSync {
				t.Errorf("SyncInterval = %v, want %v: an operator who declines "+
					"the fsync is choosing a window, and leaving it unset "+
					"takes the server's two minutes instead",
					opts.SyncInterval, tc.wantSync)
			}
		})
	}
}

// A MEMBER'S CLIENT LISTENER IS FOR THIS HOST, AND NOBODY IS TOLD OF IT.
//
// A clustered member must open one (nats-server routes only once it has), yet
// every connection of this process is in process: bound wider it is open to
// anything reaching the machine, and advertised its URL is gossiped into every
// client's INFO. So it binds loopback and advertises nothing, and a member that
// routes nowhere (solo, leaf listener only, a leaf) opens none. What a live
// member's client discovers is jetstreamtest's
// TestAMembersOwnClientDiscoversNoPeer.
//
// Mutations: leave Host empty or NoAdvertise off → the clustered row, red; let
// a member with only a leaf listener listen → its row, red.
func TestAMembersClientListenerIsLoopbackAndUnadvertised(t *testing.T) {
	t.Parallel()
	member := Config{ServerName: "a", ClusterName: "c", ClusterPort: 6222,
		ClusterURLs: []string{"nats://127.0.0.1:6223"}, StoreDir: t.TempDir(),
		FleetAuth: testFleetAuth()}
	opts, _, err := embeddedOptions(member, authFor(t, member))
	if err != nil {
		t.Fatalf("a clustered member's options: %v", err)
	}
	if opts.DontListen || opts.Host != "127.0.0.1" {
		t.Errorf("a clustered member's client listener: DontListen=%v on %q, want it "+
			"open — routing needs it — on 127.0.0.1 alone", opts.DontListen, opts.Host)
	}
	if !opts.Cluster.NoAdvertise {
		t.Error("a clustered member advertises its client URL to its peers and theirs " +
			"to its clients, where no connection of the engine dials one")
	}

	for name, cfg := range map[string]Config{
		"a solo member": {StoreDir: t.TempDir()},
		"a member with only a leaf listener": {ServerName: "a", LeafPort: 7422,
			StoreDir: t.TempDir(), FleetAuth: testFleetAuth()},
		"a leaf": {ServerName: "leaf", LeafURLs: []string{"nats-leaf://127.0.0.1:7422"},
			FleetAuth: testFleetAuth()},
		"a cluster that is named and routes nowhere": {ServerName: "a", ClusterName: "c", StoreDir: t.TempDir()},
	} {
		opts, scratch, err := embeddedOptions(cfg, authFor(t, cfg))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		removeScratch(scratch)
		if !opts.DontListen {
			t.Errorf("%s opens a client listener, where nothing could dial it", name)
		}
	}
}

// TestTheClientIsToldTheCeiling is the other half: the client learns
// MaxPayload from the server's INFO and refuses an oversized publish itself,
// making an over-limit delivery a reportable error, not a dropped connection.
func TestTheClientIsToldTheCeiling(t *testing.T) {
	t.Parallel()
	e, err := startEmbedded(t.Context(), Config{StoreDir: brokertest.TempDir(t), StoreMaxBytes: brokertest.StoreMaxBytes})
	if err != nil {
		t.Fatalf("startEmbedded: %v", err)
	}
	t.Cleanup(e.shutdown)

	nc, err := e.connect(watched{})
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(nc.Close)

	if got := nc.MaxPayload(); got != int64(queue.MaxPayloadBytes) {
		t.Errorf("the client sees a ceiling of %d, want %d", got, queue.MaxPayloadBytes)
	}
}

// EVERY SERVER AUTHENTICATES, AND ONLY A CLUSTERED MEMBER ROUTES — WITH ITS
// OWN KEY, ON EVERY ROUTE IT DIALS.
//
// Every server hands every client and leaf link to the one authenticator, with
// no user list beside it, and sends each a nonce (a custom authenticator builds
// no nkey table, so none would go out). Only a clustered member authenticates
// routes, declares the system account under its default name (the metadata
// group's state is stored there), and presents its route credential both ways:
// the cluster block's username, for gossiped routes, and each explicit route's
// URL, all its CONNECT carries. Each start mints its own key. Mutations, each
// red: no router authenticator; the system key in the server's own nkey list;
// no credential on the explicit routes.
func TestEveryServerAuthenticatesAndOnlyAClusteredMemberRoutes(t *testing.T) {
	t.Parallel()
	member := Config{ServerName: "a", ClusterName: "c", ClusterPort: 6222,
		ClusterURLs: []string{"nats://127.0.0.1:6223", "nats://127.0.0.1:6224"},
		StoreDir:    t.TempDir(), FleetAuth: testFleetAuth()}
	for name, cfg := range map[string]Config{
		"a solo member": {StoreDir: t.TempDir()},
		"a member serving leaves": {ServerName: "a", LeafPort: 7422, StoreDir: t.TempDir(),
			FleetAuth: testFleetAuth()},
		"a leaf": {ServerName: "leaf", LeafURLs: []string{"nats-leaf://127.0.0.1:7422"},
			FleetAuth: testFleetAuth()},
		"a clustered member": member,
	} {
		auth := authFor(t, cfg)
		opts, scratch, err := embeddedOptions(cfg, auth)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		removeScratch(scratch)
		if opts.CustomClientAuthentication != auth || !opts.AlwaysEnableNonce {
			t.Errorf("%s: client authenticator %v, nonce %v — every server decides "+
				"every client and leaf link, and sends each a nonce", name,
				opts.CustomClientAuthentication, opts.AlwaysEnableNonce)
		}
		if len(opts.Nkeys) != 0 || len(opts.Users) != 0 || opts.NoAuthUser != "" {
			t.Errorf("%s keeps a user list of its own (nkeys %v, users %v, no-auth %q) "+
				"beside the authenticator", name, opts.Nkeys, opts.Users, opts.NoAuthUser)
		}
		clustered := name == "a clustered member"
		if routes := opts.CustomRouterAuthentication != nil; routes != clustered {
			t.Errorf("%s: router authenticator installed = %v", name, routes)
		}
		if declared := opts.SystemAccount == "$SYS"; declared != clustered {
			t.Errorf("%s: system account %q, accounts %v", name, opts.SystemAccount,
				opts.Accounts)
		}
		if !clustered {
			continue
		}
		username, password := testFleetAuth().routeUser()
		if opts.Cluster.Username != username || opts.Cluster.Password != password {
			t.Errorf("the cluster block presents %q, want the active key's route "+
				"credential, which every gossiped route takes", opts.Cluster.Username)
		}
		if len(opts.Routes) != len(member.ClusterURLs) {
			t.Fatalf("%d routes from %d peers", len(opts.Routes), len(member.ClusterURLs))
		}
		for _, r := range opts.Routes {
			if r.User == nil {
				t.Errorf("route %s carries no credential in its URL, which is all "+
					"its CONNECT presents", r.Host)
				continue
			}
			if got, _ := r.User.Password(); r.User.Username() != username || got != password {
				t.Errorf("route %s carries user %q, not the active key's route "+
					"credential", r.Host, r.User.Username())
			}
		}
	}
	one, err := newBrokerAuth(member)
	if err != nil {
		t.Fatal(err)
	}
	other, err := newBrokerAuth(member)
	if err != nil {
		t.Fatal(err)
	}
	if one.engine.public == other.engine.public || one.system.public == other.system.public {
		t.Error("two starts minted one key: each start mints its own")
	}
	if solo := authFor(t, Config{}); solo.system.declared() {
		t.Error("a solo member minted a system account key it never declares")
	}
}

// A FLEET LISTENER WITH NO KEYS IS REFUSED BEFORE ANYTHING IS BUILT: every
// route and leaf link authenticates with a key derived from secrets.keys, so a
// member with a route or leaf listener, a named peer or a leaf link and no
// keyring would form no fleet. A solo member and a cluster only named dial
// nothing and need no key.
//
// Mutations: allow a zero FleetAuth → every refused row, red; refuse a named
// cluster that routes nowhere → its row, red.
func TestAFleetListenerWithNoKeysIsRefused(t *testing.T) {
	t.Parallel()
	for name, cfg := range map[string]Config{
		"a member with a route port": {ServerName: "a", ClusterName: "c", ClusterPort: 6222,
			StoreDir: t.TempDir()},
		"a member naming peers": {ServerName: "a", ClusterName: "c",
			ClusterURLs: []string{"nats://127.0.0.1:6223"}, StoreDir: t.TempDir()},
		"a member with a leaf port": {ServerName: "a", LeafPort: 7422, StoreDir: t.TempDir()},
		"a member on any leaf port": {ServerName: "a", LeafPort: AnyPort, StoreDir: t.TempDir()},
		"a leaf":                    {ServerName: "leaf", LeafURLs: []string{"nats-leaf://127.0.0.1:7422"}},
	} {
		_, scratch, err := embeddedOptions(cfg, authFor(t, cfg))
		removeScratch(scratch)
		if !errors.Is(err, ErrNoFleetKeys) {
			t.Errorf("%s with no keys answered %v, want ErrNoFleetKeys", name, err)
		}
	}
	for name, cfg := range map[string]Config{
		"a solo member":                {StoreDir: t.TempDir()},
		"a cluster that is only named": {ServerName: "a", ClusterName: "c", StoreDir: t.TempDir()},
	} {
		_, scratch, err := embeddedOptions(cfg, authFor(t, cfg))
		removeScratch(scratch)
		if err != nil {
			t.Errorf("%s with no keys was refused: %v", name, err)
		}
	}
}

// A BROKER WITH NO METADATA GROUP SAYS SO — a solo member and a leaf alike —
// rather than answering a group of one or removing nothing successfully.
func TestABrokerWithNoMetadataGroupSaysSo(t *testing.T) {
	t.Parallel()
	srv, err := StartServer(t.Context(), Config{StoreDir: brokertest.TempDir(t), StoreMaxBytes: brokertest.StoreMaxBytes})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Shutdown)
	if _, err := srv.MetaGroup(); !errors.Is(err, ErrNoMetaGroup) {
		t.Errorf("a solo member's metadata group read answered %v", err)
	}
	if err := srv.RemovePeer(t.Context(), "node-b"); !errors.Is(err, ErrNoMetaGroup) {
		t.Errorf("a solo member's removal answered %v", err)
	}
}
