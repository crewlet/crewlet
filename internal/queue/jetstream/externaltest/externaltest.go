// Package externaltest starts an EXTERNAL NATS server for a test — the
// `stream.type: nats` topology, a server outside this engine's own
// configuration whose max_payload and authentication are its operator's — and
// restarts it under a running client with another max_payload.
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
// # Why not jetstreamtest
//
// Importing jetstreamtest IS forming a quorum: its exported surface is
// multi-member constructors and nothing else, and internal/solo derives which
// packages need the runner to themselves from that import alone. A single
// server there would make the declaration a lie for every suite that wanted
// only this.
package externaltest

import (
	"net"
	"strconv"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// readyWithin bounds a start and a client's reconnect after a restart. Thirty
// seconds, as every other test broker in this tree waits for its own start:
// both are local work on a loaded CI runner and neither is what a case
// measures, so the bound only has to separate "slow" from "never".
const readyWithin = 30 * time.Second

// Server is one external NATS server with JetStream, listening on loopback.
type Server struct {
	t         testing.TB
	store     string
	configure []func(*server.Options)
	ns        *server.Server
}

// Start starts one announcing maxPayload — nats-server's own default, 1 MiB,
// where it is zero — with each of configure applied to its options (its
// authentication, say), and stops it when the test ends.
func Start(t testing.TB, maxPayload int32, configure ...func(*server.Options)) *Server {
	t.Helper()
	s := &Server{t: t, store: t.TempDir(), configure: configure}
	s.start(-1, maxPayload)
	t.Cleanup(s.stop)
	return s
}

func (s *Server) start(port int, maxPayload int32) {
	s.t.Helper()
	opts := &server.Options{
		Host: "127.0.0.1", Port: port, NoLog: true, NoSigs: true,
		JetStream: true, StoreDir: s.store, MaxPayload: maxPayload,
	}
	for _, c := range s.configure {
		c(opts)
	}
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
