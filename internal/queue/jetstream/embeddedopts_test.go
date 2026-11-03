package jetstream

import (
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/queue"
)

// TestTheEmbeddedServerCarriesWhatTheContractPromises pins the two options
// whose absence is invisible until the day they are needed.
//
// Neither can be caught by an ordinary behavioural case. An unset MaxPayload
// leaves the broker at nats-server's 1 MiB default, which every test payload
// fits inside — its cost used to be one oversized delivery in production,
// refused forever, and is now every boot refused by the queue's own check
// (TestAnEmbeddedBrokerWithoutTheCeilingFailsTheBootAndBlamesTheBuild), which
// this pins at its source. An unset SyncAlways leaves the file store on a
// 2-minute background flush, and nothing short of cutting power to the host
// tells the difference.
//
// So they are asserted where they are decided, on the options a Config
// produces, rather than through behaviour that cannot be provoked.
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

	// THE FSYNC IS THE OPERATOR'S SETTING, not an inference from the
	// replica count. The inference this replaces — a replicated member has
	// a quorum, so it needs no fsync — is true of one failure class and was
	// applied to five: a rack losing power takes a majority together, and a
	// three-node fleet on one rack is what a first production deployment
	// looks like.
	//
	// The replica count is asserted to have NO effect here, which is the
	// half a "does it pass through" test would miss.
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
// A clustered member must open a client listener — nats-server starts routing
// only once one is — and nothing of this process dials it: every connection
// is in process. Bound on every interface, as it was, it was open to anything
// that could reach the machine, and its URL, on the host's external address,
// was gossiped through every member to every client in their INFO. So it binds
// loopback and advertises nothing, and a member that routes nowhere — solo, a
// member with only a leaf listener, a leaf — opens none at all.
//
// Asserted on the options, where it is decided: what a client of a live
// member discovers is jetstreamtest's TestAMembersOwnClientDiscoversNoPeer.
//
// Mutations: leave Host empty → the clustered row, red; leave NoAdvertise
// off → the clustered row, red; let a member with only a leaf listener listen
// → its row, red.
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

// TestTheClientIsToldTheCeiling is the other half: setting MaxPayload on the
// server is only useful because the client learns it from the server's INFO
// and refuses an oversized publish itself. That local refusal is what makes an
// over-limit delivery a reportable error rather than a dropped connection.
func TestTheClientIsToldTheCeiling(t *testing.T) {
	t.Parallel()
	e, err := startEmbedded(t.Context(), Config{StoreDir: t.TempDir()})
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
// Every server this package starts — solo, clustered, a member serving leaves,
// a leaf — hands every client and leaf link to the one authenticator, and sends
// every connection a nonce to sign (with a custom authenticator the server
// builds no nkey table and would send none). Only a clustered member has
// routes for it to decide, declares the system account its membership code
// reaches — under its default name, which the metadata group's own state is
// stored under — and presents its route credential both ways a route can
// carry one: as the cluster block's username, which gossiped routes take, and
// in every explicit route's URL, which is all a route's CONNECT carries. No
// server keeps a user list of its own: the authenticator decides, so a list
// beside it would be a second one nothing reads. And each start mints its own
// key for this process.
//
// Mutations: omit the router authenticator → the member row, red; leave the
// system key in the server's own nkey list → the user-list row, red; give the
// explicit routes no credential → the route-URL row, red.
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

// A FLEET LISTENER WITH NO KEYS IS REFUSED BEFORE ANYTHING IS BUILT.
//
// Every route and leaf link is authenticated with a key derived from
// secrets.keys, so a member that opens a route or a leaf listener, names a
// peer, or dials a leaf link with no keyring would refuse every peer or be
// refused by every one: a fleet that never forms. A solo member and a cluster
// that is only named open and dial nothing, and need no key.
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
	srv, err := StartServer(t.Context(), Config{StoreDir: t.TempDir()})
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
