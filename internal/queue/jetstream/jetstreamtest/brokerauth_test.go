package jetstreamtest

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
	"github.com/crewlet/crewlet/internal/runtoken"
)

// fleetOf is the fleet credentials of a ring of keys, active the one named.
func fleetOf(t testing.TB, active string, keys ...runtoken.KeyMaterial) js.FleetAuth {
	t.Helper()
	auth, err := js.FleetAuthFrom(runtoken.Material{ActiveID: active, Keys: keys})
	if err != nil {
		t.Fatalf("derive fleet credentials: %v", err)
	}
	return auth
}

// operatorKey is a client option presenting the harness ring's operator key,
// loaded from a seed file exactly as the nats tool's `--nkey FILE` loads one.
func operatorKey(t testing.TB) nats.Option {
	t.Helper()
	seed, _, _, err := js.OperatorCredential(harnessRing)
	if err != nil {
		t.Fatalf("the harness ring's operator key: %v", err)
	}
	file := filepath.Join(t.TempDir(), "operator.nk")
	if err := os.WriteFile(file, append(seed, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	opt, err := nats.NkeyOptionFromSeed(file)
	if err != nil {
		t.Fatalf("read the operator key as the nats tool reads it: %v", err)
	}
	return opt
}

// refusalCount is what one member's broker has refused, by class — handed the
// refusals counted before it was installed as well.
type refusalCount struct {
	mu sync.Mutex
	n  map[js.PeerRefusal]uint64
}

func countRefusalsOf(srv *js.Server) *refusalCount {
	r := &refusalCount{n: map[js.PeerRefusal]uint64{}}
	srv.OnPeerRefused(func(p js.PeerRefusal, n uint64) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.n[p] += n
	})
	return r
}

func (r *refusalCount) of(p js.PeerRefusal) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n[p]
}

// eventually waits up to budget for cond, failing the case naming what it
// waited for. The counts and the routes are the wait conditions; no case
// sleeps for a fixed time and then asserts.
func eventually(t testing.TB, budget time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(budget)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("waited %s for %s", budget, what)
		}
		time.Sleep(20 * time.Millisecond) //fixedwait:liveness
	}
}

// strangerMember starts a member of the named cluster routing to route,
// holding auth: what a member of ANOTHER fleet — or an impostor — looks like
// to the member it dials. It never forms a quorum, and nothing waits for one.
func strangerMember(t *testing.T, name, cluster, route string, auth js.FleetAuth) *js.Server {
	t.Helper()
	for attempt := 1; ; attempt++ {
		port := freePorts(t.Context(), t, 1)[0]
		srv, err := js.StartServer(t.Context(), js.Config{ServerName: name,
			ClusterName: cluster, ClusterHost: "127.0.0.1", ClusterPort: port,
			ClusterURLs: []string{route}, StoreDir: t.TempDir(), StoreMaxBytes: brokertest.StoreMaxBytes,
			FleetAuth: auth})
		if err == nil {
			t.Cleanup(srv.Shutdown)
			return srv
		}
		if !errors.Is(err, js.ErrRoutePortTaken) || attempt == 4 {
			t.Fatalf("start %s: %v", name, err)
		}
	}
}

// A ROUTE WITHOUT THE FLEET'S KEY IS REFUSED, and counted for what it
// presented.
//
// Three dial member 0 of a formed cluster: a nats-server configured as any
// member of it would be but presenting nothing; a member of the cluster on
// another keyring; and a member on the fleet's own key id with other material.
// Each is refused — counted as `route/none`, `route/unknown` and
// `route/invalid` — and none appears among member 0's route peers. The counts
// are the wait condition.
//
// Mutation: the router authenticator admits every route → none is counted and
// each joins, red.
func TestARouteWithoutTheFleetsKeyIsRefused(t *testing.T) {
	t.Parallel()
	c := StartCluster(t, 3, js.Config{})
	refused := countRefusalsOf(c.Servers[0])
	cluster, toMember0 := c.Configs[0].ClusterName, routeURL(c.Configs[0].ClusterPort)

	// NATS-SERVER'S OWN, presenting nothing.
	rogue, err := server.NewServer(&server.Options{ServerName: "rogue", Host: "127.0.0.1",
		Port: -1, NoSigs: true, Cluster: server.ClusterOpts{Name: cluster,
			Host: "127.0.0.1", Port: -1}, Routes: server.RoutesFromStr(toMember0)})
	if err != nil {
		t.Fatal(err)
	}
	go rogue.Start()
	t.Cleanup(func() { rogue.Shutdown(); rogue.WaitForShutdown() })
	if !rogue.ReadyForConnections(30 * time.Second) {
		t.Fatal("the rogue server never started")
	}
	strangerMember(t, "foreign", cluster, toMember0, fleetOf(t, "k9",
		runtoken.KeyMaterial{ID: "k9", Material: "another fleet's placeholder material"}))
	strangerMember(t, "impostor", cluster, toMember0, fleetOf(t, "jetstreamtest",
		runtoken.KeyMaterial{ID: "jetstreamtest", Material: "not this fleet's material"}))

	for _, want := range []js.PeerRefusal{
		{Listener: js.ListenerRoute, Presented: js.PresentedNone},
		{Listener: js.ListenerRoute, Presented: js.PresentedUnknown},
		{Listener: js.ListenerRoute, Presented: js.PresentedInvalid},
	} {
		eventually(t, 30*time.Second, fmt.Sprintf("member 0 to count a %s/%s refusal",
			want.Listener, want.Presented), func() bool { return refused.of(want) > 0 })
	}
	for _, stranger := range []string{"rogue", "foreign", "impostor"} {
		if slices.Contains(c.Servers[0].RoutePeers(), stranger) {
			t.Errorf("%s routes to member 0: %v", stranger, c.Servers[0].RoutePeers())
		}
	}
}

// A ROUTE LEARNED BY GOSSIP AUTHENTICATES TOO.
//
// Seeded 0↔1 and 2→0 only, member 1 and member 2 learn of each other through
// member 0 and dial each other themselves — a route whose URL nobody wrote, so
// the one credential it can present is the cluster block's. The full mesh
// forms.
//
// Mutation: present the credential in the explicit route URLs only → the route
// between 1 and 2 never forms, red.
func TestGossipedRoutesAuthenticate(t *testing.T) {
	t.Parallel()
	c := withFreshPorts(t.Context(), t, "gossip cluster", func(ctx context.Context, name string) (*Cluster, error) {
		ports := freePorts(ctx, t, 3)
		seeds := [][]string{{routeURL(ports[1])}, {routeURL(ports[0])}, {routeURL(ports[0])}}
		c := &Cluster{life: t}
		for i := range 3 {
			if err := c.start(ctx, t, memberConfig(js.Config{}, name, i, 3, ports[i], seeds[i]), i); err != nil {
				return c, err
			}
		}
		return c, nil
	})
	one, two := c.Configs[1].ServerName, c.Configs[2].ServerName
	eventually(t, 30*time.Second, "members 1 and 2 to route to each other by gossip", func() bool {
		return slices.Contains(c.Servers[1].RoutePeers(), two) &&
			slices.Contains(c.Servers[2].RoutePeers(), one)
	})
}

// THE FLEET STAYS WHOLE THROUGH A KEYRING ROTATION, with no step of its own.
//
// Three members on {k1}; then the keyring runbook's steps, member by member: add
// k2 with k1 still active, flip the active key to k2, drop k1. After EVERY
// restart the mesh is whole and a replicated append through every member
// lands — because within a step every node already holds every key any node
// presents. "Lands" as the engine's writer waits for it to ([landAppend]): the
// member just stopped may have led the stream, so the group may be electing.
//
// NOT PARALLEL: a member comes back on the route port it had, and a cluster
// another case reserved ports for in the same moment could take it while it
// is down (TestARestartedMemberRejoinsAsItself's reason).
//
// Mutations: accept only the active key → the first member to flip is refused
// by the others, red; have a rotated member's routes refuse the stream's
// subjects → it routes to both others and no append through it is ever
// answered, red once the wait's bound runs out.
func TestAFleetStaysWholeThroughAKeyRotation(t *testing.T) {
	k1 := runtoken.KeyMaterial{ID: "k1", Material: "the rotation case's first placeholder material"}
	k2 := runtoken.KeyMaterial{ID: "k2", Material: "the rotation case's second placeholder material"}
	c := StartCluster(t, 3, js.Config{FleetAuth: fleetOf(t, "k1", k1)})
	clients := []*js.Queue{c.Client(t, 0), c.Client(t, 1), c.Client(t, 2)}
	spec := js.DomainStream{Name: "CREWLET_ROTATION_LOG", Subjects: []string{"crewlet.rotation.>"},
		MaxBytes: 1 << 20, Duplicates: time.Minute}
	// AS A NODE'S BOOT OPENS ONE: provisioned, then opened, which is
	// [js.Queue.DomainLog]'s contract. A restarted member's open meets the
	// election its stop may have left, and the provision's lookup is what waits
	// one out: the open alone gives up in five seconds ([jsprovision.ReadBack]).
	logs := make([]*js.DomainLog, len(clients))
	open := func(when string, i int) {
		t.Helper()
		if err := clients[i].EnsureDomainStream(t.Context(), spec); err != nil {
			t.Fatalf("%s: provision the log through member %d: %v", when, i, err)
		}
		log, err := clients[i].DomainLog(t.Context(), spec.Name)
		if err != nil {
			t.Fatalf("%s: open the log through member %d: %v", when, i, err)
		}
		logs[i] = log
	}
	for i := range clients {
		open("before the rotation", i)
	}
	whole := func(when string) {
		t.Helper()
		eventually(t, 30*time.Second, when+": every member routed to both others", func() bool {
			for i, srv := range c.Servers {
				var others []string
				for j, cfg := range c.Configs {
					if j != i {
						others = append(others, cfg.ServerName)
					}
				}
				if !routesTo(srv.RoutePeers(), others) {
					return false
				}
			}
			return true
		})
		for i, log := range logs {
			if err := landAppend(t.Context(), c, log, spec.Name, "crewlet.rotation.a",
				fmt.Sprintf("%s/%d", when, i), []byte(when)); err != nil {
				t.Fatalf("%s: a replicated append through member %d: %v", when, i, err)
			}
		}
	}
	whole("before the rotation")
	for _, step := range []struct {
		name string
		auth js.FleetAuth
	}{
		{"k2 added, k1 active", fleetOf(t, "k1", k1, k2)},
		{"k2 active", fleetOf(t, "k2", k1, k2)},
		{"k1 dropped", fleetOf(t, "k2", k2)},
	} {
		for i := range c.Servers {
			_ = clients[i].Stop(context.WithoutCancel(t.Context()))
			c.StopMember(i)
			clients[i] = c.RestartMemberWith(t, i, func(cfg *js.Config) { cfg.FleetAuth = step.auth })
			when := fmt.Sprintf("%s, member %d restarted", step.name, i)
			open(when, i)
			whole(when)
		}
	}
}

// A MEMBER ON ANOTHER KEYRING IS REFUSED, AND COUNTED BY KEY.
//
// A member restarted onto a keyring sharing no key with its peers — a rotation
// that dropped the old key before every node held the new one — presents a key
// no peer holds: every route it dials is refused, and each peer counts every
// one of them as `route/unknown`.
//
// DECIDED ON EVERY DIAL, NEVER ON A ROUTE LIST (#803). A member lists a route
// it dialled before its peer has judged it: the peer sends its INFO when it
// accepts the connection, before it reads the CONNECT it judges, and the
// dialler registers the route on that INFO (nats-server v2.15.2, route.go's
// createRoute and processRouteInfo). So the member names a peer for as long as
// each refusal takes to arrive, and a peer names the member while the member
// judges the peer's own dial. Nor does the route report say which end admitted
// a route: an accepted route is marked solicited once its own end dials the
// same peer (upgradeRouteToSolicited), and nats-server calls a route it dialled
// authenticated only at the first PONG to its PING (addRoute). The refusal is
// what decides it, since a refused connection is closed and never routes.
//
// So the case runs on forwarders it counts. The peers' own dials to the member
// stop at their forwarders, so every route between them is one the member
// dialled. Once each peer has refused it, the member's forwarders take no new
// dial and the ones in flight end: a route a peer admitted is held open and
// never would. Every one ends, so every one was refused, and each peer counted
// one refusal for each dial that reached it.
//
// NOT PARALLEL, for TestAFleetStaysWholeThroughAKeyRotation's reason.
//
// Mutations: count nothing, or count an unknown key's route as invalid → no
// route/unknown refusal arrives, red; admit an unknown key's route → none is
// counted, red; admit a route whose refusal was counted → it is held open and
// the member's forwarder never empties, red; hand the sink each refusal
// twice, or only those the log limiter writes → refusals and dials differ,
// red.
func TestAMemberOnAnotherKeyringIsRefusedByKey(t *testing.T) {
	c := StartPartitionableCluster(t, 3, js.Config{})
	peers := []*refusalCount{countRefusalsOf(c.Servers[0]), countRefusalsOf(c.Servers[1])}
	c.StopMember(2)
	for i := range peers {
		// THE PEERS' DIALS TO IT END AT THEIR FORWARDERS, and its own to
		// them carry nothing from before it stopped: the routes it held
		// end with it, and its forwarders open again counting from zero.
		c.drain(t, i, 2)
		c.drain(t, 2, i)
		c.reopen(t, 2, i)
	}
	c.Configs[2].FleetAuth = fleetOf(t, "k9",
		runtoken.KeyMaterial{ID: "k9", Material: "a keyring no peer holds"})
	// THE SERVER ALONE: a client of it would wait for a quorum it cannot reach.
	c.restartServer(t, 2)
	unknown := js.PeerRefusal{Listener: js.ListenerRoute, Presented: js.PresentedUnknown}
	for i, count := range peers {
		eventually(t, 30*time.Second, fmt.Sprintf("member %d to count the stranger's dials", i),
			func() bool { return count.of(unknown) > 0 })
	}
	for i, count := range peers {
		dials := c.drain(t, 2, i)
		if refused := count.of(unknown); refused != uint64(dials) {
			t.Errorf("member %d counted %d %s/%s refusals for the %d routes the member on "+
				"another keyring dialled to it", i, refused, unknown.Listener, unknown.Presented, dials)
		}
	}
}

// AN OPERATOR RESIZES A STREAM THROUGH A MEMBER'S CLIENT LISTENER.
//
// The gesture the docs prescribe before a raised stream.replicas lets a node
// start: an R1 stream created through member 0, made R3 by the nats tool on
// member 1's loopback client listener with the operator key — read back as
// three replicas. Through the plain JetStream API, which every member serves
// beside the fleet's domain, so the tool needs no domain of its own.
//
// Mutations: refuse the operator key → the connection is refused, red; leave
// the client listener unbound → there is nothing to dial, red.
func TestAnOperatorResizesAStreamThroughTheClientListener(t *testing.T) {
	t.Parallel()
	c := StartCluster(t, 3, js.Config{})
	own, err := c.Servers[0].Conn()
	if err != nil {
		t.Fatal(err)
	}
	defer own.Close()
	member0, err := jetstream.New(own)
	if err != nil {
		t.Fatal(err)
	}
	const name = "CREWLET_OPERATOR_PROBE"
	if _, err := member0.CreateStream(t.Context(), jetstream.StreamConfig{Name: name,
		Subjects: []string{"crewlet.operatorprobe.>"}, Replicas: 1, MaxBytes: 1 << 20}); err != nil {
		t.Fatalf("create an R1 stream through member 0: %v", err)
	}

	listener := c.Servers[1].ClientListener()
	if listener == "" {
		t.Fatal("member 1 opens no client listener")
	}
	nc, err := nats.Connect("nats://"+listener, operatorKey(t), nats.Timeout(5*time.Second))
	if err != nil {
		t.Fatalf("the operator key on member 1's client listener: %v", err)
	}
	defer nc.Close()
	tool, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	stream, err := tool.Stream(ctx, name)
	if err != nil {
		t.Fatalf("the operator reads the stream: %v", err)
	}
	cfg := stream.CachedInfo().Config
	cfg.Replicas = 3
	if _, err := tool.UpdateStream(ctx, cfg); err != nil {
		t.Fatalf("the operator resizes the stream to R3: %v", err)
	}
	info, err := member0.Stream(ctx, name)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.CachedInfo().Config.Replicas; got != 3 {
		t.Errorf("the stream reads %d replicas after the resize, want 3", got)
	}
}
