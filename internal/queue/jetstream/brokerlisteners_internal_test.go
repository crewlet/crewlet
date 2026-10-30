package jetstream

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
)

// loneMember starts a clustered member whose one configured peer is a port
// nothing listens on: it opens its route listener and its loopback client
// listener and forms no quorum — the shape a case about one member's listeners
// needs, in a package that forms no cluster. Ready in tens of milliseconds:
// [StartServer] waits for the listeners, never for a peer.
func loneMember(t *testing.T) *Server {
	t.Helper()
	cfg := Config{ServerName: "lone", ClusterName: "lone", ClusterHost: "127.0.0.1",
		ClusterURLs: []string{fmt.Sprintf("nats://127.0.0.1:%d", freePort(t))},
		StoreDir:    t.TempDir(), FleetAuth: testFleetAuth()}
	for attempt := 1; ; attempt++ {
		cfg.ClusterPort = freePort(t)
		srv, err := StartServer(t.Context(), cfg)
		if err == nil {
			t.Cleanup(srv.Shutdown)
			return srv
		}
		// A PORT LOST BEFORE THE START says nothing about the case; anything
		// else is the answer.
		if !errors.Is(err, ErrRoutePortTaken) || attempt == 4 {
			t.Fatalf("start a lone clustered member: %v", err)
		}
	}
}

// presenting is a client option presenting an nkey seed.
func presenting(t *testing.T, seed string) nats.Option {
	t.Helper()
	kp, err := nkeys.FromSeed([]byte(seed))
	if err != nil {
		t.Fatal(err)
	}
	public, err := kp.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	return nats.Nkey(public, kp.Sign)
}

// A MEMBER'S CLIENT LISTENER IS ON LOOPBACK AND ADMITS ONLY THE OPERATOR KEY.
//
// It is open only because routing needs it, and nothing of this process dials
// it. Over TCP it refuses a client that presents nothing, a stranger's nkey,
// this process's own engine key, a leaf key and the route credential; it
// admits the operator key — loaded from a file exactly as the nats tool's
// `--nkey` loads one — on the global account, where `$SYS` has no responders.
// And the engine's own connection, in process, still works.
//
// Mutations: leave the listener on every interface → its address row, red;
// admit the engine key over TCP → its row, red; bind the operator to the
// system account → the `$SYS` row, red.
func TestAMembersClientListenerAdmitsOnlyTheOperatorKey(t *testing.T) {
	t.Parallel()
	srv := loneMember(t)
	listener := srv.ClientListener()
	if !strings.HasPrefix(listener, "127.0.0.1:") {
		t.Fatalf("the member's client listener is %q, want loopback alone", listener)
	}
	url := "nats://" + listener
	auth := srv.embedded.auth
	username, password := testFleetAuth().routeUser()
	stranger, err := nkeys.CreateUser()
	if err != nil {
		t.Fatal(err)
	}
	strangerSeed, err := stranger.Seed()
	if err != nil {
		t.Fatal(err)
	}
	for name, opts := range map[string][]nats.Option{
		"nothing":               nil,
		"a stranger's nkey":     {presenting(t, string(strangerSeed))},
		"this process's engine": {auth.engine.option()},
		"a leaf key":            {presenting(t, testFleetAuth().leafSeed())},
		"the route credential":  {nats.UserInfo(username, password)},
	} {
		nc, err := nats.Connect(url, append(opts, nats.Timeout(5*time.Second))...)
		if err == nil {
			nc.Close()
			t.Errorf("%s was admitted on the client listener", name)
			continue
		}
		if !strings.Contains(strings.ToLower(err.Error()), "authorization") {
			t.Errorf("%s was turned away for another reason: %v", name, err)
		}
	}

	seed, _, _, err := OperatorCredential(testRing)
	if err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(t.TempDir(), "operator.nk")
	if err := os.WriteFile(file, append(seed, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	fromFile, err := nats.NkeyOptionFromSeed(file)
	if err != nil {
		t.Fatalf("read the operator key as the nats tool reads it: %v", err)
	}
	operator, err := nats.Connect(url, fromFile, nats.Timeout(5*time.Second))
	if err != nil {
		t.Fatalf("the operator key was refused on the client listener: %v", err)
	}
	defer operator.Close()
	sub, err := operator.SubscribeSync("operator.probe")
	if err != nil {
		t.Fatal(err)
	}
	if err := operator.Publish("operator.probe", []byte("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := sub.NextMsg(5 * time.Second); err != nil {
		t.Fatalf("the operator's connection carries nothing on the global account: %v", err)
	}
	if _, err := operator.Request("$SYS.REQ.SERVER.PING", nil, 2*time.Second); !errors.Is(err, nats.ErrNoResponders) {
		t.Errorf("the operator asking the system account answered %v, want no responders", err)
	}

	own, err := srv.Conn()
	if err != nil {
		t.Fatalf("the engine's own connection, in process: %v", err)
	}
	own.Close()
}

// THE SYSTEM KEY REACHES THE SYSTEM ACCOUNT, AND THE ENGINE'S DOES NOT.
//
// The membership code's key is the one thing admitted to `$SYS`, where a voter
// is removed; the engine's own connection is bound to the global account, where
// the system account's subjects have nobody listening.
//
// Mutation: bind the system key to the global account → the system ping goes
// unanswered, red; bind the engine's to the system account → it is answered,
// red.
func TestTheSystemKeyReachesOnlyTheSystemAccount(t *testing.T) {
	t.Parallel()
	srv := loneMember(t)
	system, err := srv.embedded.auth.system.connect(srv.embedded.ns)
	if err != nil {
		t.Fatalf("the membership code's connection: %v", err)
	}
	defer system.Close()
	if _, err := system.Request("$SYS.REQ.SERVER.PING", nil, 5*time.Second); err != nil {
		t.Errorf("the system key asking the system account answered %v", err)
	}
	engine, err := srv.Conn()
	if err != nil {
		t.Fatal(err)
	}
	defer engine.Close()
	if _, err := engine.Request("$SYS.REQ.SERVER.PING", nil, 2*time.Second); !errors.Is(err, nats.ErrNoResponders) {
		t.Errorf("the engine's connection asking the system account answered %v, want no "+
			"responders", err)
	}
}

// THE BROKER'S OWN AUTHENTICATION LINE IS DEBUG.
//
// nats-server reports each refused attempt itself, carrying the username the
// peer sent, at the rate the peer dials. The authenticator already counts every
// refusal and logs a class once a minute, so the server's line goes to DEBUG —
// recognised by the sentinel as an argument or as the format's tail, the two
// shapes a client's Errorf hands the logger (server/client.go authViolation,
// v2.15.1). The dialer's own route error and every other error keep their
// level.
//
// Mutations: drop the routing → the sentinel rows, red; route every Errorf to
// DEBUG → the route-error row, red; match the bare sentinel only when it is the
// whole format → the labelled bare row, red.
func TestTheBrokersOwnAuthenticationLineIsDebug(t *testing.T) {
	t.Parallel()
	if server.ErrAuthentication.Error() != "authentication error" {
		t.Fatalf("nats-server's sentinel reads %q: the bare row below spells what "+
			"the server writes, and must follow it", server.ErrAuthentication)
	}
	for _, tc := range []struct {
		name  string
		emit  func(natsLogger)
		level string
	}{
		{"the sentinel and the peer's username", func(l natsLogger) {
			l.Errorf("cid:7 - %v - %s", server.ErrAuthentication, `User "x"`)
		}, "DEBUG"},
		// THE SENTINEL'S OWN TEXT, as a constant format: the server hands
		// the logger its label and the error's text as one format string,
		// and the case below holds the literal to the sentinel.
		{"the bare sentinel after the label", func(l natsLogger) {
			l.Errorf("127.0.0.1:5000 - cid:7 - authentication error")
		}, "DEBUG"},
		{"the dialer's own route error", func(l natsLogger) {
			l.Errorf("Route Error %s", "'Authorization Violation'")
		}, "ERROR"},
		{"an unrelated error", func(l natsLogger) {
			l.Errorf("stream %q: %v", "INBOX", errors.New("no space"))
		}, "ERROR"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var buf bytes.Buffer
			tc.emit(capture(&buf, slog.LevelDebug))
			if line := decode(t, &buf); line["level"] != tc.level {
				t.Errorf("level = %v, want %v", line["level"], tc.level)
			}
		})
	}
}
