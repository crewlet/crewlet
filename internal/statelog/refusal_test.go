package statelog_test

import (
	"errors"
	"net"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/queue"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
)

// A RECORD TOO LARGE IS REFUSED ONCE, AS TOO LARGE — on a real broker.
//
// The client's refusal is a plain error rather than the broker's, and it was
// read as no answer: the write asked the log what landed, found nothing,
// retook its snapshot, decided the same record and was refused again —
// sixteen times — and then told its caller a colleague kept editing the
// object.
//
// TWO PATHS TO ONE ANSWER. A record past its domain's declared largest is
// refused by the publisher before it is sent, on a log that keeps a gate
// reserve or none; one inside the declaration that the SERVER still cannot
// carry is refused by the client, and that refusal is what has to be read as
// too large. No declaration reaches past the transport's contract any more
// ([statelog.MaxTransportRecordBytes]) and a server announcing less than that
// is refused when the queue opens, so the second path is a server that
// CHANGED under a running node — a reconnect to a cluster member configured
// apart from the one it booted against, or a server restarted with a smaller
// max_payload. The case stages the second: it opens on an operator's server
// that carries the contract and restarts it under the log with less.
func TestAnOversizedRecordIsRefusedOnceAsTooLarge(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		domain  statelog.Domain
		size    int
		payload int32
		sent    int64
	}{
		"past the declaration, on a log that keeps a gate reserve": {
			probeDomain{}, probeMaxRecord + 1, 0, 0},
		"past the declaration, on a log that keeps none": {
			unreservedDomain{}, statelog.MaxTransportRecordBytes + 1, 0, 0},
		"inside the declaration and past the server's max_payload": {
			probeDomain{}, probeMaxRecord - 1024, probeMaxRecord / 2, 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			var h *harness
			if tc.payload == 0 {
				h = newHarnessFor(t, tc.domain)
			} else {
				srv := startOperatorServer(t, queue.MaxPayloadBytes)
				h = newHarnessOn(t, tc.domain, js.Config{URL: srv.URL()})
				srv.restart(tc.payload, h.q.Conn())
			}
			_, err := h.writeSized(probeSubject("a"), "op-large", tc.size)
			if !errors.Is(err, queue.ErrTooLarge) {
				t.Fatalf("an oversized record answered %v, want queue.ErrTooLarge", err)
			}
			if errors.Is(err, statelog.ErrConflict) {
				t.Fatalf("an oversized record was reported as a conflict: %v", err)
			}
			if n := h.appends.appends.Load(); n != tc.sent {
				t.Fatalf("an oversized record was sent %d times, want %d — the "+
					"same record is refused the same way on every round", n, tc.sent)
			}
		})
	}
}

// unreservedDomain is the probe log claiming no identity, which is what keeps
// no gate reserve — the vector changelog's shape, down to declaring the
// largest record the transport carries.
type unreservedDomain struct{ probeDomain }

func (unreservedDomain) ClaimsIdentity() bool { return false }

func (unreservedDomain) Stream() statelog.StreamSpec {
	spec := probeDomain{}.Stream()
	spec.MaxRecordBytes = statelog.MaxTransportRecordBytes
	return spec
}

// operatorServer is a NATS server outside the queue's own configuration — an
// operator's, whose max_payload is its own — which a case can restart under a
// running log announcing another.
type operatorServer struct {
	t     *testing.T
	store string
	ns    *server.Server
}

// startOperatorServer starts one with JetStream, announcing maxPayload, and
// stops it when the test ends.
func startOperatorServer(t *testing.T, maxPayload int32) *operatorServer {
	t.Helper()
	s := &operatorServer{t: t, store: t.TempDir()}
	s.start(-1, maxPayload)
	t.Cleanup(s.stop)
	return s
}

func (s *operatorServer) start(port int, maxPayload int32) {
	s.t.Helper()
	ns, err := server.NewServer(&server.Options{
		Host: "127.0.0.1", Port: port, NoLog: true, NoSigs: true,
		JetStream: true, StoreDir: s.store, MaxPayload: maxPayload,
	})
	if err != nil {
		s.t.Fatalf("configure an operator's server: %v", err)
	}
	go ns.Start()
	if !ns.ReadyForConnections(30 * time.Second) {
		ns.Shutdown()
		s.t.Fatal("the operator's server never became ready")
	}
	s.ns = ns
}

func (s *operatorServer) stop() {
	s.ns.Shutdown()
	s.ns.WaitForShutdown()
}

// URL is the server's client address.
func (s *operatorServer) URL() string { return s.ns.ClientURL() }

// restart stops the server and starts it again on its own port and its own
// store, announcing maxPayload, and waits until nc has reconnected to it and
// reads the new limit — which is what the client refuses against from then on.
func (s *operatorServer) restart(maxPayload int32, nc *nats.Conn) {
	s.t.Helper()
	port := s.ns.Addr().(*net.TCPAddr).Port
	s.stop()
	s.start(port, maxPayload)
	deadline := time.Now().Add(30 * time.Second)
	for !nc.IsConnected() || nc.MaxPayload() != int64(maxPayload) {
		if time.Now().After(deadline) {
			s.t.Fatalf("the client did not reconnect to the restarted server "+
				"within 30s (connected=%v, reading a max_payload of %d, want %d)",
				nc.IsConnected(), nc.MaxPayload(), maxPayload)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// NO DECLARATION ADMITS A RECORD THE TRANSPORT CANNOT CARRY.
//
// The transport's limit is on the signed MESSAGE, and a declaration is on the
// payload a decide forms, so a log declared at the transport's own number
// passed the publisher's refusal with a record the broker then refused past
// its max_payload — naming a server setting on the broker the engine
// configures itself, where the declaration was supposed to be the one refusal
// a record meets before it is sent. The spec is refused instead, and the
// largest the transport carries is what a log that wants it all declares.
//
// Mutation: hold the declaration to [queue.MaxPayloadBytes] again and the
// transport's own number validates.
func TestADeclarationPastWhatTheTransportCarriesIsRefused(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		declared int64
		valid    bool
	}{
		{statelog.MaxTransportRecordBytes, true},
		{statelog.MaxTransportRecordBytes + 1, false},
		{queue.MaxPayloadBytes, false},
		{0, false},
	} {
		spec := probeDomain{}.Stream()
		spec.MaxRecordBytes = tc.declared
		err := spec.Validate()
		if (err == nil) != tc.valid {
			t.Errorf("a declared largest record of %d validates %v, want %v",
				tc.declared, err, tc.valid)
		}
		if tc.valid && spec.MaxAppendBytes() > queue.MaxPayloadBytes {
			t.Errorf("a valid declaration of %d appends %d bytes, past the "+
				"transport's %d", tc.declared, spec.MaxAppendBytes(),
				queue.MaxPayloadBytes)
		}
	}
}

// A BROKER REFUSAL THAT IS NOT A FULL LOG IS NOT REPORTED AS ONE.
//
// Every API error the framework had no case for came back `log_full`, with a
// remedy to raise the stream's byte ceiling. A stream that is not there has
// room to spare; the refusal now carries the broker's own error.
func TestABrokerRefusalThatIsNotAFullLogIsNotReportedAsOne(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.appends.fail(&jetstream.APIError{ErrorCode: jetstream.JSErrCodeStreamNotFound,
		Description: "stream not found"}, true)
	_, err := h.write(probeSubject("a"), "op-refused", "hello")
	var refusal *statelog.Unavailable
	if errors.As(err, &refusal) && refusal.Reason == statelog.ReasonLogFull {
		t.Fatalf("a missing stream was reported as a full log: %v", err)
	}
	var apiErr *jetstream.APIError
	if !errors.As(err, &apiErr) || apiErr.ErrorCode != jetstream.JSErrCodeStreamNotFound {
		t.Fatalf("the refusal %v does not carry the broker's own error", err)
	}
	if n := h.appends.appends.Load(); n != 1 {
		t.Fatalf("a refused record was sent %d times, want once", n)
	}
}
