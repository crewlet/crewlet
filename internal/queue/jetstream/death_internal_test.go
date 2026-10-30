package jetstream

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A BROKER THAT CANNOT USE ITS STORE REFUSES ITS BOOT AT ONCE, WITH ITS OWN
// REASON — and a clustered member is not told its route port was taken.
//
// # The failure this pins
//
// nats-server reports a JetStream that will not start through its logger's
// Fatalf and returns from Start there, before it opens a listener. The logger
// used to log that at Error and nothing else, so the boot waited out its whole
// readiness budget for a server that was never going to be ready — thirty
// seconds solo, two minutes clustered — and a clustered member, whose route
// listener Start never reached, was then reported as one whose route port had
// been taken: [ErrRoutePortTaken], which the harnesses retry on, for a fault no
// port can fix.
//
// # The input
//
// A store whose `jetstream` directory is a FILE. nats-server refuses it ("not a
// directory") whoever runs the test — a directory made read-only would be
// writable anyway to a suite running as root, and pass for the wrong reason.
// The store directory itself is a directory, so the lock taken in it
// ([StoreDirLock]) lets the start through to nats-server, which is the refusal
// this case is about; a store directory that is itself a file is refused by the
// lock instead, and has a case of its own.
func TestABrokerThatCannotUseItsStoreRefusesItsBootAtOnce(t *testing.T) {
	t.Parallel()
	for _, clustered := range []bool{false, true} {
		t.Run("clustered="+strconv.FormatBool(clustered), func(t *testing.T) {
			t.Parallel()
			store := filepath.Join(t.TempDir(), "store")
			if err := os.MkdirAll(store, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(store, "jetstream"),
				[]byte("not a directory"), 0o600); err != nil {
				t.Fatalf("plant a file where the store's jetstream directory goes: %v", err)
			}
			cfg := Config{StoreDir: store}
			if clustered {
				cfg.ServerName, cfg.ClusterName = "member-1", "unusable-store"
				cfg.ClusterHost = "127.0.0.1"
				cfg.FleetAuth = testFleetAuth()
			}

			begun := time.Now()
			err := startOnAFreeRoutePort(t, cfg)
			took := time.Since(begun)
			if err == nil {
				t.Fatal("a broker whose store is a file started")
			}
			if !errors.Is(err, ErrBrokerDied) {
				t.Fatalf("the boot did not report the broker's own refusal: %v", err)
			}
			// NATS-SERVER'S OWN WORDS, which name what failed better than
			// any summary of them: the step, and why.
			if !strings.Contains(err.Error(), "Can't start JetStream") {
				t.Errorf("the refusal does not carry nats-server's reason: %v", err)
			}
			if errors.Is(err, ErrRoutePortTaken) || errors.Is(err, ErrLeafPortTaken) {
				t.Errorf("a refused store is reported as a taken port, which a "+
					"harness retries and an operator goes hunting for: %v", err)
			}
			// AT ONCE: the store is refused within milliseconds of Start,
			// and a boot that missed the refusal waits out the whole
			// listener budget for a server that never opens one. Under the
			// budget is the line between the two.
			if took >= listenTimeout {
				t.Errorf("the refusal took %s, the whole %s budget — the boot "+
					"waited for a server that had already given up", took, listenTimeout)
			}
		})
	}
}

// startOnAFreeRoutePort starts cfg, on a route port nothing held a moment ago
// when cfg is clustered, and answers what the start answered.
//
// A PORT LOST BEFORE THE START is the one answer that says nothing about the
// case — the start's own probe refuses it as [ErrRoutePortTaken] with no
// [ErrBrokerDied] beside it — so it is tried again on another; anything else
// is the answer.
func startOnAFreeRoutePort(t *testing.T, cfg Config) error {
	t.Helper()
	for attempt := 1; ; attempt++ {
		if cfg.ClusterName != "" {
			cfg.ClusterPort = freePort(t)
		}
		srv, err := StartServer(t.Context(), cfg)
		if err == nil {
			srv.Shutdown()
			return nil
		}
		lost := errors.Is(err, ErrRoutePortTaken) && !errors.Is(err, ErrBrokerDied)
		if !lost || attempt == 4 {
			return err
		}
	}
}

// freePort is a loopback port nothing held a moment ago.
func freePort(t *testing.T) int {
	t.Helper()
	var lc net.ListenConfig
	l, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	defer l.Close()
	//nolint:errcheck // Listen on a TCP address always yields *TCPAddr.
	return l.Addr().(*net.TCPAddr).Port
}

// heldPortBindError is a port somebody holds, and the error a listener on it
// gets — the very value nats-server hands its Fatalf.
func heldPortBindError(t *testing.T) (int, error) {
	t.Helper()
	var lc net.ListenConfig
	held, err := lc.Listen(t.Context(), "tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("hold a port: %v", err)
	}
	t.Cleanup(func() { held.Close() })
	_, err = lc.Listen(t.Context(), "tcp", held.Addr().String())
	if !errors.Is(err, syscall.EADDRINUSE) {
		t.Fatalf("binding a held port answered %v, not EADDRINUSE", err)
	}
	//nolint:errcheck // Listen on a TCP address always yields *TCPAddr.
	return held.Addr().(*net.TCPAddr).Port, err
}

// declared is the error a [Death] carries after one Fatalf of format and v.
func declared(format string, v ...any) error {
	d := newDeath()
	d.declare(format, v...)
	return d.Err()
}

// A DEATH NAMES A TAKEN PORT ONLY FROM THE BIND THAT FAILED, AND ONLY THE
// LISTENER WHOSE PORT IT WAS.
//
// The taken port keeps its sentinel because it is the one fatal a different
// number fixes: the harnesses retry on it and on no other death. It is read
// from the error VALUE nats-server hands its Fatalf — the bind's own
// *net.OpError, which names the address and wraps EADDRINUSE — so a refused
// store can never be mistaken for one, and a route port is never blamed for
// the leaf listener's.
func TestADeathNamesATakenPortOnlyFromTheBindThatFailed(t *testing.T) {
	t.Parallel()
	port, bindErr := heldPortBindError(t)
	const routeFormat = "Error listening on router port: %d - %v"

	for name, tc := range map[string]struct {
		cause     error
		listeners listenerSpecs
		want      error // the port sentinel, or nil for none
	}{
		"the route port, taken": {
			cause:     declared(routeFormat, port, bindErr),
			listeners: listenerSpecs{route: listenerSpec{port: port, host: "127.0.0.1"}},
			want:      ErrRoutePortTaken,
		},
		"the leaf port, taken": {
			cause:     declared("Error listening on leafnode port: %d - %v", port, bindErr),
			listeners: listenerSpecs{leaf: listenerSpec{port: port, host: "127.0.0.1"}},
			want:      ErrLeafPortTaken,
		},
		// Tier A lets two listeners share a number only on addresses that
		// cannot overlap, so the address the bind was for decides.
		"one number on two addresses, the route's bound": {
			cause: declared(routeFormat, port, bindErr),
			listeners: listenerSpecs{
				route: listenerSpec{port: port, host: "127.0.0.1"},
				leaf:  listenerSpec{port: port, host: "192.0.2.1"},
			},
			want: ErrRoutePortTaken,
		},
		"one number on two addresses, the leaf's bound": {
			cause: declared(routeFormat, port, bindErr),
			listeners: listenerSpecs{
				route: listenerSpec{port: port, host: "192.0.2.1"},
				leaf:  listenerSpec{port: port, host: "127.0.0.1"},
			},
			want: ErrLeafPortTaken,
		},
		"a port this member was not asked to open": {
			cause:     declared(routeFormat, port, bindErr),
			listeners: listenerSpecs{route: listenerSpec{port: port + 1, host: "127.0.0.1"}},
		},
		"a store JetStream refused": {
			cause: declared("Can't start JetStream: %v",
				errors.New("storage directory is not writable")),
			listeners: listenerSpecs{route: listenerSpec{port: port, host: "127.0.0.1"}},
		},
		// Not a port somebody HOLDS — a privileged one this user may not
		// bind — so no different number on the same host fixes it.
		"the route port, refused": {
			cause: declared(routeFormat, port, &net.OpError{Op: "listen", Net: "tcp",
				Addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: port},
				Err:  os.NewSyscallError("bind", syscall.EACCES)}),
			listeners: listenerSpecs{route: listenerSpec{port: port, host: "127.0.0.1"}},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			err := deathError(tc.cause, tc.listeners)
			if !errors.Is(err, ErrBrokerDied) {
				t.Errorf("a death is not reported as one: %v", err)
			}
			if !errors.Is(err, tc.cause) {
				t.Errorf("nats-server's own error was dropped: %v", err)
			}
			for _, sentinel := range []error{ErrRoutePortTaken, ErrLeafPortTaken} {
				want := tc.want != nil && errors.Is(tc.want, sentinel)
				if got := errors.Is(err, sentinel); got != want {
					t.Errorf("reported as %v = %t, want %t: %v", sentinel, got, want, err)
				}
			}
			if tc.want != nil && !strings.Contains(err.Error(), strconv.Itoa(port)) {
				t.Errorf("a taken port is not named: %v", err)
			}
		})
	}
}

// A SERVER THAT IS SIMPLY NOT READY IS NEVER TOLD A PORT WAS TAKEN.
//
// Every listener nats-server cannot bind is a Fatalf, so a taken port ends the
// wait as a death. What reaches the budget is a server still starting, and
// calling that a taken route port — which is what a listener not bound YET
// used to be read as — sends an operator to a port nobody holds.
func TestAServerNotYetReadyIsNotToldAPortWasTaken(t *testing.T) {
	t.Parallel()
	for _, clustered := range []bool{false, true} {
		err := notReadyError(90*time.Second, clustered)
		if errors.Is(err, ErrRoutePortTaken) || errors.Is(err, ErrLeafPortTaken) {
			t.Errorf("clustered=%t: a server still starting is blamed on a port: %v",
				clustered, err)
		}
		if !strings.Contains(err.Error(), "1m30s") {
			t.Errorf("clustered=%t: the failure does not name the budget it ran out: %v",
				clustered, err)
		}
	}
}

// A FATALF AFTER THE BOOT IS DECLARED WHERE THE ENGINE WATCHES FOR IT.
//
// No Fatalf of nats-server v2.15.1 is reachable after the boot on the options
// this package sets, so the server is asked to make one — through its own
// Fatalf, which is the path every call site takes to the logger this package
// installed. The engine ends the node on [Server.Death]; that is only worth
// anything if the declaration reaches it, with the reason and the error value
// nats-server reported.
func TestAFatalfAfterTheBootIsDeclaredOnTheServersDeath(t *testing.T) {
	t.Parallel()
	srv, err := StartServer(t.Context(), Config{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer srv.Shutdown()
	death := srv.Death()
	select {
	case <-death.Done():
		t.Fatalf("a healthy broker reads as dead: %v", death.Err())
	default:
	}
	if death.Err() != nil {
		t.Fatalf("a broker that declared nothing has a cause: %v", death.Err())
	}

	port, bindErr := heldPortBindError(t)
	srv.embedded.ns.Fatalf("Unable to listen for websocket connections: %v", bindErr)

	select {
	case <-death.Done():
	case <-time.After(5 * time.Second):
		t.Fatal("a Fatalf after the boot never reached the server's death")
	}
	cause := death.Err()
	if !errors.Is(cause, ErrBrokerDied) || !errors.Is(cause, syscall.EADDRINUSE) {
		t.Errorf("the declared cause lost the sentinel or nats-server's error: %v", cause)
	}
	if !strings.Contains(cause.Error(), "Unable to listen for websocket connections") ||
		!strings.Contains(cause.Error(), strconv.Itoa(port)) {
		t.Errorf("the declared cause is not nats-server's own sentence: %v", cause)
	}

	// THE FIRST DECLARATION IS THE CAUSE: what follows is the server
	// failing further from the state the first one left it in.
	srv.embedded.ns.Fatalf("a second failure")
	if !errors.Is(death.Err(), cause) {
		t.Errorf("a later Fatalf replaced the first cause: %v", death.Err())
	}
}

// A SHUT-DOWN SERVER, OR NONE, NEVER READS AS DEAD.
//
// The engine watches a nil Death on an external stream and after its own
// shutdown, so nil has to mean "never" rather than panic or close.
func TestANilDeathNeverCloses(t *testing.T) {
	t.Parallel()
	var d *Death
	select {
	case <-d.Done():
		t.Fatal("a nil death closed")
	default:
	}
	if d.Err() != nil {
		t.Fatalf("a nil death has a cause: %v", d.Err())
	}

	srv, err := StartServer(t.Context(), Config{})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	srv.Shutdown()
	if srv.Death() != nil {
		t.Error("a server that was shut down still hands out its death")
	}
}
