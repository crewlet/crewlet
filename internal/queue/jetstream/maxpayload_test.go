package jetstream

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/crewlet/crewlet/internal/queue"
)

// A SERVER THAT CANNOT CARRY THE CONTRACT IS REFUSED WHEN THE QUEUE OPENS.
//
// The client refuses a message past what its server announced, locally, and
// nothing compared that with what this build sends: a server at nats-server's
// 1 MiB default took every small write and refused the first large one, so a
// node booted, served, and refused its first large event or state-log record
// only when it was written.
// Refused at Open, the misconfiguration is a boot that names the setting, the
// value the server announced and the value this build needs.
//
// The boundary is exact in both directions, because the need is exact: a
// state-log record at its log's declared largest is [queue.MaxPayloadBytes] on
// the wire, so a byte short refuses a record the build will write, and a
// server announcing more than the contract carries it.
//
// Mutation: compare against the default 1 MiB, or against the contract with
// `>` rather than `>=`, and a row goes red — or drop heldToTheContract from
// dial and every refusing row does.
func TestAServerBelowTheContractIsRefusedWhenTheQueueOpens(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		maxPayload int32
		refused    bool
	}{
		{"nats-server's own default", 0, true},
		{"a byte short of the contract", queue.MaxPayloadBytes - 1, true},
		{"exactly the contract", queue.MaxPayloadBytes, false},
		{"more than the contract", 2 * queue.MaxPayloadBytes, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := startOperatorServer(t, tc.maxPayload)
			announced := tc.maxPayload
			if announced == 0 {
				announced = server.MAX_PAYLOAD_SIZE
			}
			q, err := Open(t.Context(), Config{URL: srv.URL()})
			if !tc.refused {
				if err != nil {
					t.Fatalf("a server announcing a max_payload of %d was refused: %v",
						announced, err)
				}
				if err := q.Stop(context.WithoutCancel(t.Context())); err != nil {
					t.Errorf("Stop: %v", err)
				}
				return
			}
			if err == nil {
				_ = q.Stop(context.WithoutCancel(t.Context()))
				t.Fatalf("a server announcing a max_payload of %d opened, and the "+
					"first message past it would be refused only when it is written",
					announced)
			}
			// THE SETTING, WHAT THE SERVER SAID, WHAT THIS BUILD NEEDS, and
			// the spelling an operator types into the server's own file —
			// the whole of what the remedy turns on.
			for _, want := range []string{"max_payload", strconv.Itoa(int(announced)),
				strconv.Itoa(queue.MaxPayloadBytes), "max_payload: 8MB",
				srv.URL(), "stream.url"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal %q does not say %q", err, want)
				}
			}
		})
	}
}

// THE LIMIT READ IS THE ONE THE CONNECTION IS HELD TO, an account's included.
//
// nats-server holds a client to the lowest of its own max_payload and the
// payload limits of the account it signed in to and of its user — an
// account's `limits { max_payload }` in the server's file here, the `payload`
// limit of an account's or a user's JWT in operator mode — and says so only in
// the INFO it sends once the connection has authenticated, after the PONG that
// completes the connect. Read straight off the connect, the check saw the
// server's own 8 MiB and passed a node whose every message past the account's
// limit would be refused, which is the failure it exists to move to the boot.
//
// The server here carries the contract and the account it signs the node in
// to states a limit of its own: a quarter of it, refused naming the account's
// number; exactly the contract, which opens — the control that the account is
// otherwise one the queue works in.
//
// Mutation: read nc.MaxPayload() without the round trip before it in
// announcedMaxPayload and the refusing row goes red (the connection has almost
// never read the second INFO by then; -count=20 shows it).
func TestALimitTheSigningInAccountStatesIsTheOneRead(t *testing.T) {
	t.Parallel()
	const password = "not-in-any-refusal"
	for _, tc := range []struct {
		name    string
		account int
		refused bool
	}{
		{"an account allowing a quarter of the contract", queue.MaxPayloadBytes / 4, true},
		{"an account allowing exactly the contract", queue.MaxPayloadBytes, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			conf := filepath.Join(dir, "nats.conf")
			// The server's own limit in the spelling the refusal tells an
			// operator to type, so this also certifies that the remedy it
			// names is one nats-server reads as the contract.
			body := fmt.Sprintf(`host: 127.0.0.1
port: -1
max_payload: %s
jetstream { store_dir: %q }
accounts {
  ENGINE {
    jetstream: enabled
    users: [ { user: crewlet, password: %q } ]
    limits { max_payload: %s }
  }
}
`, natsSize(queue.MaxPayloadBytes), filepath.Join(dir, "store"), password,
				natsSize(tc.account))
			if err := os.WriteFile(conf, []byte(body), 0o600); err != nil {
				t.Fatalf("write the server's configuration: %v", err)
			}
			opts, err := server.ProcessConfigFile(conf)
			if err != nil {
				t.Fatalf("read the server's configuration: %v", err)
			}
			if opts.MaxPayload != queue.MaxPayloadBytes {
				t.Fatalf("nats-server reads max_payload: %s as %d bytes, not the "+
					"contract's %d — the remedy every refusal names is wrong",
					natsSize(queue.MaxPayloadBytes), opts.MaxPayload,
					queue.MaxPayloadBytes)
			}
			opts.NoLog, opts.NoSigs = true, true
			ns, err := server.NewServer(opts)
			if err != nil {
				t.Fatalf("configure the server: %v", err)
			}
			go ns.Start()
			t.Cleanup(func() {
				ns.Shutdown()
				ns.WaitForShutdown()
			})
			if !ns.ReadyForConnections(30 * time.Second) {
				t.Fatal("the server never became ready")
			}
			url := strings.Replace(ns.ClientURL(), "nats://",
				"nats://crewlet:"+password+"@", 1)

			q, err := Open(t.Context(), Config{URL: url})
			if !tc.refused {
				if err != nil {
					t.Fatalf("an account allowing the contract was refused: %v", err)
				}
				if err := q.Stop(context.WithoutCancel(t.Context())); err != nil {
					t.Errorf("Stop: %v", err)
				}
				return
			}
			if err == nil {
				_ = q.Stop(context.WithoutCancel(t.Context()))
				t.Fatalf("a queue opened on a connection its account holds to %d "+
					"bytes, because the server's own %d was all it read",
					tc.account, queue.MaxPayloadBytes)
			}
			for _, want := range []string{"max_payload", strconv.Itoa(tc.account),
				strconv.Itoa(queue.MaxPayloadBytes), "account"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal %q does not say %q", err, want)
				}
			}
			if strings.Contains(err.Error(), password) {
				t.Errorf("the refusal %q carries the password stream.url holds", err)
			}
		})
	}
}

// THE EMBEDDED BROKER'S REFUSAL BLAMES THE BUILD, because no setting reaches it
// — on every connection this package opens to it.
//
// The same check runs on the connections to the broker this build configures
// itself, and there it cannot fire unless [embeddedOptions] stopped setting
// the ceiling — the edit this case stages, a broker built from those options
// with MaxPayload taken back out. That is the one change whose cost is
// otherwise invisible: every node boots against nats-server's 1 MiB default
// and fails only its first large write. It fails the boot instead, and a
// sentence sending an operator to stream.url or a server's configuration file
// would send them after a knob that does not exist on this topology.
//
// Three openers, because there are three consumers of a connection and one
// dial beneath them: the queue's own, the coordination store's, and the second
// connection a subsystem owns — a snapshot donor's, streaming mebibyte chunks.
// Checked in one of them only, the others would be the member nobody asked.
//
// Mutation: drop heldToTheContract from embeddedServer.connect and every row
// goes red; hand it `false` for an embedded broker, or swap its two sentences,
// and every row does.
func TestAnEmbeddedBrokerWithoutTheCeilingFailsTheBootAndBlamesTheBuild(t *testing.T) {
	t.Parallel()
	opts, scratch, err := embeddedOptions(Config{})
	if err != nil {
		t.Fatalf("broker options: %v", err)
	}
	opts.MaxPayload = 0
	ns, err := server.NewServer(opts)
	if err != nil {
		removeScratch(scratch)
		t.Fatalf("configure the broker: %v", err)
	}
	go ns.Start()
	t.Cleanup(func() { shutdownAndClean(ns, scratch) })
	if !ns.ReadyForConnections(acceptTimeout) {
		t.Fatalf("the broker was not ready within %v", acceptTimeout)
	}
	broken := &embeddedServer{ns: ns, inProcess: true}

	for _, opener := range []struct {
		name string
		open func() (func(), error)
	}{
		{"the queue's own connection", func() (func(), error) {
			q, err := newQueueOn(t.Context(), Config{}, broken, false)
			if err != nil {
				return nil, err
			}
			return func() { _ = q.Stop(context.WithoutCancel(t.Context())) }, nil
		}},
		{"the coordination store's", func() (func(), error) {
			nc, err := (&Server{embedded: broken}).Conn()
			if err != nil {
				return nil, err
			}
			return nc.Close, nil
		}},
		{"a second connection a subsystem owns", func() (func(), error) {
			nc, err := (&Queue{embedded: broken}).DialOwned()
			if err != nil {
				return nil, err
			}
			return nc.Close, nil
		}},
	} {
		t.Run(opener.name, func(t *testing.T) {
			closeIt, err := opener.open()
			if err == nil {
				closeIt()
				t.Fatalf("%s opened on an embedded broker announcing %d bytes, "+
					"below the contract's %d", opener.name, server.MAX_PAYLOAD_SIZE,
					queue.MaxPayloadBytes)
			}
			for _, want := range []string{"embedded", "max_payload",
				strconv.Itoa(server.MAX_PAYLOAD_SIZE), "this build is broken"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("the refusal %q does not say %q", err, want)
				}
			}
			for _, never := range []string{"stream.url", "max_payload: 8MB"} {
				if strings.Contains(err.Error(), never) {
					t.Errorf("the refusal %q names %q, a setting the embedded "+
						"broker does not read", err, never)
				}
			}
		})
	}
}

// A SECOND CONNECTION TO AN OPERATOR'S SERVER IS HELD TO THE CONTRACT TOO.
//
// [Queue.DialOwned] dials the operator's cluster afresh, and lands on
// whichever member answers — which need not be the one the queue's own
// connection was checked against at boot. What rides it is a snapshot donor
// streaming mebibyte chunks to a joining node, so a member below the contract
// failed the chunks of a transfer a node had advertised, with nothing said
// about why. The case opens a queue on a server that carries the contract,
// restarts it allowing a quarter, and asks for a second connection.
//
// Mutation: drop heldToTheContract from dial and this goes red.
func TestASecondConnectionToAnOperatorsServerIsHeldToTheContract(t *testing.T) {
	t.Parallel()
	const limit = queue.MaxPayloadBytes / 4
	srv := startOperatorServer(t, queue.MaxPayloadBytes)
	q := newQueueWith(t, Config{URL: srv.URL()})
	srv.restart(limit, q.Conn())

	nc, err := q.DialOwned()
	if err == nil {
		nc.Close()
		t.Fatalf("a second connection opened on a server announcing %d bytes, "+
			"below the contract's %d", limit, queue.MaxPayloadBytes)
	}
	for _, want := range []string{"max_payload", strconv.Itoa(limit),
		strconv.Itoa(queue.MaxPayloadBytes), "max_payload: 8MB", srv.URL()} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal %q does not say %q", err, want)
		}
	}
}

// A RECONNECT TO A SERVER BELOW THE CONTRACT IS SAID WHEN IT HAPPENS.
//
// The check at the dial reads the member the connection landed on, and a
// client dialling a cluster reconnects to whichever member answers — so a
// member configured apart from the others, or a server whose limit was
// lowered under a running node, is met after boot. Nothing can be refused by
// then; what the line buys is that the first word about it is not a large
// write's `record_too_large`.
//
// Two halves, because the handler is called by the client on a goroutine this
// package does not own: the handler's own answer, asserted directly with a
// logger the case holds, and that it is the handler every connection this
// package opens is given — the queue's own, the coordination store's and a
// subsystem's second, and an operator's dial as much as an embedded one.
//
// Mutation: drop the ReconnectHandler option from either dial and a row of
// the second half goes red; log nothing, or log on a server that carries the
// contract, and the first does.
func TestAReconnectToAServerBelowTheContractIsNamed(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name       string
		maxPayload int32
		logged     bool
	}{
		{"a server below the contract", queue.MaxPayloadBytes / 4, true},
		{"a server that carries it", queue.MaxPayloadBytes, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			srv := startOperatorServer(t, tc.maxPayload)
			nc, err := nats.Connect(srv.URL())
			if err != nil {
				t.Fatalf("connect: %v", err)
			}
			t.Cleanup(nc.Close)
			var logged bytes.Buffer
			reconnectWatch{log: slog.New(slog.NewTextHandler(&logged, nil))}.reconnected(nc)
			line := logged.String()
			if got := strings.Contains(line, "jetstream_max_payload_below_contract"); got != tc.logged {
				t.Fatalf("a reconnect to a server announcing %d logged %q, want a "+
					"line %v", tc.maxPayload, line, tc.logged)
			}
			if tc.logged && !strings.Contains(line, "max_payload="+strconv.Itoa(int(tc.maxPayload))) {
				t.Errorf("the line %q does not carry the max_payload the server "+
					"announced", line)
			}
		})
	}

	t.Run("it is the handler every connection is given", func(t *testing.T) {
		t.Parallel()
		server, err := StartServer(t.Context(), Config{StoreDir: t.TempDir()})
		if err != nil {
			t.Fatalf("StartServer: %v", err)
		}
		t.Cleanup(server.Shutdown)
		q, err := server.Client(t.Context())
		if err != nil {
			t.Fatalf("Client: %v", err)
		}
		t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
		coord, err := server.Conn()
		if err != nil {
			t.Fatalf("Conn: %v", err)
		}
		t.Cleanup(coord.Close)
		owned, err := q.DialOwned()
		if err != nil {
			t.Fatalf("DialOwned: %v", err)
		}
		t.Cleanup(owned.Close)
		dialled, err := dialOptions(Config{})
		if err != nil {
			t.Fatalf("dialOptions: %v", err)
		}
		external := nats.GetDefaultOptions()
		for _, opt := range dialled {
			if err := opt(&external); err != nil {
				t.Fatalf("applying an option: %v", err)
			}
		}

		want := reflect.ValueOf(reconnectWatch{}.reconnected).Pointer()
		for name, installed := range map[string]nats.ConnHandler{
			"the queue's own":             q.Conn().Opts.ReconnectedCB,
			"the coordination store's":    coord.Opts.ReconnectedCB,
			"a subsystem's second":        owned.Opts.ReconnectedCB,
			"an operator's server's dial": external.ReconnectedCB,
		} {
			if installed == nil {
				t.Errorf("%s connection has no reconnect handler, so a reconnect "+
					"to a member below the contract says nothing", name)
				continue
			}
			if reflect.ValueOf(installed).Pointer() != want {
				t.Errorf("%s connection reconnects through a handler that is not "+
					"reconnectWatch.reconnected", name)
			}
		}
	})
}

// A MESSAGE TOO LARGE FOR THE SERVER NAMES THE SERVER'S OWN LIMIT, on every
// verb that sends one.
//
// The embedded broker is configured at exactly [queue.MaxPayloadBytes], and a
// server announcing less is refused when the queue opens — so on every
// connection this backend OPENS the contract's number and the connection's
// agree. They part only when the server CHANGES under a running node: a
// reconnect to a cluster member configured apart from the one the node booted
// against, or a server restarted with a smaller max_payload. The client
// refuses against what the server it reached said, so a refusal naming the
// contract there names a limit the message is inside of, and sends whoever
// reads it looking for the wrong knob — and a scatter request between the two
// failed with the client's bare error rather than the contract's
// [queue.ErrTooLarge], which is the one failure a producer must be able to
// tell from a broker that is merely down.
//
// So the queue opens against an external server that carries the contract,
// the server is restarted under it allowing a quarter of that, and each
// message is larger than the new limit and smaller than the contract.
func TestAMessageTooLargeNamesTheServersOwnLimit(t *testing.T) {
	t.Parallel()
	const limit = queue.MaxPayloadBytes / 4
	srv := startOperatorServer(t, queue.MaxPayloadBytes)
	q := newQueueWith(t, Config{URL: srv.URL()})
	// Provisioned against the server the queue opened on, as a node's log
	// is at boot; the restarted server recovers it from its store.
	if err := q.EnsureDomainStream(t.Context(), probeDomain()); err != nil {
		t.Fatalf("EnsureDomainStream: %v", err)
	}
	srv.restart(limit, q.Conn())
	size := limit + 1024
	names := strconv.Itoa(limit) + "-byte limit"

	t.Run("publish", func(t *testing.T) {
		t.Parallel()
		e := ev(1)
		e.Extra = map[string]json.RawMessage{
			"blob": json.RawMessage(`"` + strings.Repeat("x", size) + `"`),
		}
		err := q.Publish(t.Context(), "crewlet.agent.big.inbox", e)
		if !errors.Is(err, queue.ErrTooLarge) {
			t.Fatalf("an event over the server's limit was refused with %v, "+
				"want %v", err, queue.ErrTooLarge)
		}
		if !strings.Contains(err.Error(), names) {
			t.Errorf("the refusal %q does not name the server's %d-byte "+
				"max_payload, the limit it came from", err, limit)
		}
	})

	t.Run("ask", func(t *testing.T) {
		t.Parallel()
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		_, err := q.Ask(ctx, "crewlet.test.big.ask", make([]byte, size), 1)
		if !errors.Is(err, queue.ErrTooLarge) {
			t.Fatalf("a request over the server's limit and inside the "+
				"contract's was refused with %v, want %v", err, queue.ErrTooLarge)
		}
		if !strings.Contains(err.Error(), names) {
			t.Errorf("the refusal %q does not name the server's %d-byte "+
				"max_payload", err, limit)
		}
	})

	t.Run("append", func(t *testing.T) {
		t.Parallel()
		log, err := q.DomainLog(t.Context(), probeDomain().Name)
		if err != nil {
			t.Fatalf("DomainLog: %v", err)
		}
		_, _, err = log.Append(t.Context(), "crewlet.probe.log.object.big", "",
			nil, make([]byte, size))
		// THE CLIENT'S OWN SENTINEL, still: the state log classifies an
		// append by it, and a wrap that dropped it would turn a permanent
		// refusal into the unknown answer every round retries.
		if !errors.Is(err, nats.ErrMaxPayload) {
			t.Fatalf("a record over the server's limit was refused with %v, "+
				"want one wrapping %v", err, nats.ErrMaxPayload)
		}
		for _, want := range []string{strconv.Itoa(size) + " bytes", names} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal %q does not say %q — the record's size and "+
					"the server's limit are the whole of what its remedy turns on",
					err, want)
			}
		}
	})
}

// operatorServer is a NATS server outside this package's own configuration —
// an operator's, whose max_payload is its own — which a case can restart under
// a running client announcing another.
type operatorServer struct {
	t     *testing.T
	store string
	ns    *server.Server
}

// startOperatorServer starts one with JetStream, announcing maxPayload — or
// nats-server's own default where it is zero — and stops it when the test
// ends.
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
