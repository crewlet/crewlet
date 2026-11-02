// Package externaltest starts an EXTERNAL NATS server for a test — the
// `stream.type: nats` topology, a server outside this engine's own
// configuration whose max_payload and authentication are its operator's — and
// restarts it under a running client with another max_payload, or reloads its
// options live under one.
//
// # Why a package
//
// Three suites need one: the jetstream backend's (a server below the
// transport's contract is refused, a message too large for a server the
// connection reached since names that server's own limit, and no sentence
// naming a server carries its URL's credential), the state log's (a record the
// server cannot carry is refused once, as too large) and the engine's (a boot
// against a server below the contract is refused by name). They held four
// copies of the same thirty lines between them, and the restart is the part
// copies drift on: it has to keep the port and the store,
// so the client reconnects and the streams it provisioned survive, and it has
// to wait until the CLIENT reads the new limit rather than until the server is
// up, since the client refuses against what it last read.
//
// # Why every server authenticates
//
// The engine refuses an external server that authenticates nobody: Tier A
// requires `stream.type: nats` to present a credential, and the dial refuses a
// server whose INFO admits anonymous clients. So a server this package starts
// requires [Token] unless a configure states authentication of its own, and a
// fixture that dials it through the engine presents Token (`stream.token`, or
// the queue's Config.Token). [Anonymous] is the configure of the cases whose
// subject is that refusal. [Server.URL] still carries no credential: a case
// that signs in through the URL puts its own there.
//
// # Why not jetstreamtest
//
// Importing jetstreamtest IS forming a quorum: its exported surface is
// multi-member constructors and nothing else, and internal/solo derives which
// packages need the runner to themselves from that import alone. A single
// server there would make the declaration a lie for every suite that wanted
// only this.
//
// # Why the server states its size
//
// Left unset, an external server's storage cap is three quarters of the free
// space on its store's volume, and an account with no JetStream limit makes a
// node stand its own volume's free space in for its broker's limit. On a test
// runner both are the same machine's disk, and facts about it: a node that
// boots on one fails on a fuller disk. So the server's cap is fixed and its account states a limit
// below it ([brokertest.StoreMaxBytes]), which the node reads back as an
// operator's limit like any other. [StartUnstated] is the operator's broker
// that states none, for a case whose subject is that.
package externaltest

import (
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/queue/jetstream/brokertest"
)

// readyWithin bounds a start and a client's reconnect after a restart. Thirty
// seconds, as every other test broker in this tree waits for its own start:
// both are local work on a loaded CI runner and neither is what a case
// measures, so the bound only has to separate "slow" from "never".
const readyWithin = 30 * time.Second

// Token is the bearer token every server [Start] or [StartUnstated] starts
// requires, unless a configure states authentication of its own.
const Token = "externaltest-fixture-token"

// Anonymous is a configure that leaves the server authenticating nobody — the
// shape the engine's dial refuses, for the cases whose subject is that.
func Anonymous(o *server.Options) { o.Authorization = "" }

// serverMaxStore is the server's own storage cap: three times the account's
// limit.
//
// FAR ENOUGH ABOVE THE ACCOUNT'S that stating the limit is always admitted.
// nats-server weighs an account limit against the cap as the change from
// "unlimited", one byte more than the limit asked for, ON TOP OF what the
// server's streams already reserve. On a fresh server that is nothing, but a
// restarted one recovers its streams under the unlimited default before
// [Server.limitAccount] runs, and they can reserve the account's whole limit:
// so the cap has to hold the limit twice and a byte. Under [Start] nothing
// else binds at this number, since the node holds its reservations to the
// account's limit; under [StartUnstated] it is the one cap that refuses.
const serverMaxStore = 3 * brokertest.StoreMaxBytes

// Server is one external NATS server with JetStream, listening on loopback.
type Server struct {
	t         testing.TB
	store     string
	configure []func(*server.Options)
	ns        *server.Server

	// unstated leaves the account with no JetStream limit of its own
	// ([StartUnstated]).
	unstated bool

	// opts is what the running server was configured from, kept for
	// [Server.Reload]: nats-server reloads a whole option set, so a reload
	// is these with one change rather than a fresh set that would also
	// differ in every field nothing meant to change.
	opts server.Options
}

// Start starts one announcing maxPayload — nats-server's own default, 1 MiB,
// where it is zero — requiring [Token], with each of configure applied to its
// options after that (authentication of its own, say, or [Anonymous]), its
// account limited to [brokertest.StoreMaxBytes] of file storage, and stops it
// when the test ends.
func Start(t testing.TB, maxPayload int32, configure ...func(*server.Options)) *Server {
	t.Helper()
	return startServer(t, false, maxPayload, configure)
}

// StartUnstated is [Start] with an account that states no JetStream limit, the
// operator's broker a node can read no limit from. Its server's cap is still
// fixed, so what it refuses does not depend on the machine either.
func StartUnstated(t testing.TB, maxPayload int32, configure ...func(*server.Options)) *Server {
	t.Helper()
	return startServer(t, true, maxPayload, configure)
}

func startServer(t testing.TB, unstated bool, maxPayload int32, configure []func(*server.Options)) *Server {
	t.Helper()
	s := &Server{t: t, store: t.TempDir(), configure: configure, unstated: unstated}
	s.start(-1, maxPayload)
	t.Cleanup(s.stop)
	return s
}

func (s *Server) start(port int, maxPayload int32) {
	s.t.Helper()
	opts := &server.Options{
		Host: "127.0.0.1", Port: port, NoLog: true, NoSigs: true,
		JetStream: true, StoreDir: s.store, MaxPayload: maxPayload,
		JetStreamMaxStore: serverMaxStore,
		// BEFORE the configures, so one stating authentication of its own
		// replaces this rather than sitting beside it.
		Authorization: Token,
	}
	for _, c := range s.configure {
		c(opts)
	}
	// A COPY, taken before NewServer: the server writes its derived
	// defaults back into the set it is handed, and a reload built from
	// those would be a set nobody configured.
	s.opts = *opts
	ns, err := server.NewServer(opts)
	if err != nil {
		s.t.Fatalf("configure an external NATS server: %v", err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(readyWithin) {
		ns.Shutdown()
		s.t.Fatalf("the external NATS server was not ready within %s", readyWithin)
	}
	s.ns = ns
	if !s.unstated {
		s.limitAccount()
	}
}

// limitAccount states the account's JetStream file-store limit.
//
// ON THE RUNNING SERVER, because [server.Options] has no field for an
// account's JetStream limits: nats-server takes them from a parsed accounts
// block, from an account JWT, or from the running account, and the first two
// would mean writing the server's whole account configuration, sign-in
// included, in place of the options the suites hand [Start]. It runs before [Start] hands the case a URL, so no
// client of the case's ever reads the server without it; after a restart it
// runs before [Server.Restart] waits for the client, and a client that read
// its budget in between would read the account as unlimited — none of the
// suites restarting a server reads one.
//
// THE FILE STORE ALONE: every other limit is nats-server's "unlimited", as
// an account with JetStream enabled and nothing configured has it.
func (s *Server) limitAccount() {
	s.t.Helper()
	err := s.ns.GlobalAccount().UpdateJetStreamLimits(map[string]server.JetStreamAccountLimits{
		"": {
			MaxMemory: -1, MaxStore: brokertest.StoreMaxBytes,
			MaxStreams: -1, MaxConsumers: -1, MaxAckPending: -1,
			MemoryMaxStreamBytes: -1, StoreMaxStreamBytes: -1,
		},
	})
	if err != nil {
		s.ns.Shutdown()
		s.t.Fatalf("limit the external NATS server's account to %d bytes of file "+
			"storage: %v", brokertest.StoreMaxBytes, err)
	}
}

func (s *Server) stop() {
	s.ns.Shutdown()
	s.ns.WaitForShutdown()
}

// URL is the server's client address, which carries no credential: a case
// that signs in puts its own into the URL it dials.
func (s *Server) URL() string { return s.ns.ClientURL() }

// HostPort is the server's host and port, for a case composing its own URL.
func (s *Server) HostPort() string {
	addr := s.ns.Addr().(*net.TCPAddr)
	return net.JoinHostPort(addr.IP.String(), strconv.Itoa(addr.Port))
}

// Reload lowers or raises the running server's max_payload LIVE — no restart,
// so a connected client is not reconnected and is told only what the server
// chooses to send it, which for a reloaded max_payload is nothing: nats-server
// applies it to the connections it holds without an INFO. That is the
// operator's gesture this package exists to stage beside a restart.
//
// MAX_PAYLOAD AND NOTHING ELSE, because it is the one reload that is safe to
// stage under a client that is using the server. nats-server's authorization
// reload rewrites account state its client read loops read without a lock,
// which the race detector reports inside nats-server itself whenever a
// connected node is mid-request; a case that changes what the server admits
// restarts it instead ([Server.RestartAs]).
//
// A later [Server.Restart] or [Server.RestartAs] starts from the reloaded
// limit.
func (s *Server) Reload(maxPayload int32) {
	s.t.Helper()
	next := s.opts
	// THE PORT THE SERVER BOUND, since a random one (-1) reloaded as
	// written is a change of port, which nats-server refuses to reload.
	next.Port = s.ns.Addr().(*net.TCPAddr).Port
	next.MaxPayload = maxPayload
	s.opts = next
	// ReloadOptions takes ownership of the set it is handed.
	handed := next
	if err := s.ns.ReloadOptions(&handed); err != nil {
		s.t.Fatalf("reload the external NATS server's max_payload: %v", err)
	}
}

// RestartAs stops the server and starts it again on its own port and its own
// store, at the max_payload it announces now, configured by configure in place
// of what [Start] was given, over [Token] as Start's are — and waits only for
// the server.
//
// For a case whose subject is a server that no longer admits a running client:
// its credentials rotated, say. Such a client never reconnects, so nothing
// here waits for it; what the case waits for is what the client does about
// being refused.
func (s *Server) RestartAs(configure ...func(*server.Options)) {
	s.t.Helper()
	port := s.ns.Addr().(*net.TCPAddr).Port
	s.stop()
	s.configure = configure
	s.start(port, s.opts.MaxPayload)
}

// Restart stops the server and starts it again on its own port and its own
// store, announcing maxPayload, and waits until nc has reconnected to it and
// reads the new limit — which is what the client refuses against from then on.
func (s *Server) Restart(maxPayload int32, nc *nats.Conn) {
	s.t.Helper()
	port := s.ns.Addr().(*net.TCPAddr).Port
	s.stop()
	s.start(port, maxPayload)
	want := int64(maxPayload)
	if want == 0 {
		want = server.MAX_PAYLOAD_SIZE
	}
	deadline := time.Now().Add(readyWithin)
	for !nc.IsConnected() || nc.MaxPayload() != want {
		if time.Now().After(deadline) {
			s.t.Fatalf("the client did not reconnect to the restarted server "+
				"within %s (connected=%v, reading a max_payload of %d, want %d)",
				readyWithin, nc.IsConnected(), nc.MaxPayload(), want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
