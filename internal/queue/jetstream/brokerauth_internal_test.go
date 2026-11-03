package jetstream

import (
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nkeys"

	"github.com/crewlet/crewlet/internal/runtoken"
)

// fakeConn is one connection as the server hands it to an authenticator: what
// it presented, the nonce it was sent and where it came from — and the user it
// was bound as, if any.
type fakeConn struct {
	kind       int
	opts       server.ClientOpts
	nonce      []byte
	remote     net.Addr
	registered *server.User
}

func (f *fakeConn) GetOpts() *server.ClientOpts                 { return &f.opts }
func (f *fakeConn) GetTLSConnectionState() *tls.ConnectionState { return nil }
func (f *fakeConn) RegisterUser(u *server.User)                 { f.registered = u }
func (f *fakeConn) RemoteAddress() net.Addr                     { return f.remote }
func (f *fakeConn) GetNonce() []byte                            { return f.nonce }
func (f *fakeConn) Kind() int                                   { return f.kind }
func (f *fakeConn) GetID() uint64                               { return 7 }

// testAddr is a connection's remote address on a network of a test's choosing:
// "pipe" is this process's own in-memory dial.
type testAddr struct{ network, addr string }

func (a testAddr) Network() string { return a.network }
func (a testAddr) String() string  { return a.addr }

var (
	overPipe = testAddr{"pipe", "pipe"}
	overTCP  = testAddr{"tcp", "127.0.0.1:50123"}
)

// authRing is the two-key ring the authenticator cases hold: k1 and k2, k2
// active, so a credential of the key that is NOT presented is admitted too.
var authRing = runtoken.Material{ActiveID: "k2", Keys: []runtoken.KeyMaterial{
	{ID: "k1", Material: "the authenticator cases' first placeholder material"},
	{ID: "k2", Material: "the authenticator cases' second placeholder material"},
}}

// testAuth is the authenticator of a broker started for cfg — STARTED, so the
// accounts it binds a connection to are the ones a running server has:
// nats-server sets its system account only as it starts (server.go Start,
// v2.15.1), and a server only built answers no system account at all. Nothing
// connects to it but what a case hands Check itself: a clustered cfg names a
// peer port nothing listens on, so its route never forms.
//
// A ROUTE PORT LOST BEFORE THE START says nothing about the case, so a taken
// one is asked again on another; anything else is the answer.
func testAuth(t *testing.T, cfg Config) *brokerAuth {
	t.Helper()
	for attempt := 1; ; attempt++ {
		if cfg.ClusterName != "" {
			cfg.ClusterHost, cfg.ClusterPort = "127.0.0.1", freePort(t)
			cfg.ClusterURLs = []string{fmt.Sprintf("nats://127.0.0.1:%d", freePort(t))}
		}
		e, err := startEmbedded(t.Context(), cfg)
		if err == nil {
			t.Cleanup(e.shutdown)
			return e.auth
		}
		if !errors.Is(err, ErrRoutePortTaken) || attempt == 4 {
			t.Fatalf("start the broker: %v", err)
		}
	}
}

// signer is one nkey a case presents: its public key and its seed's signature
// over a nonce.
type signer struct {
	public string
	sign   func([]byte) []byte
}

func signerOfSeed(t *testing.T, seed string) signer {
	t.Helper()
	kp, err := nkeys.FromSeed([]byte(seed))
	if err != nil {
		t.Fatal(err)
	}
	public, err := kp.PublicKey()
	if err != nil {
		t.Fatal(err)
	}
	return signer{public: public, sign: func(n []byte) []byte {
		sig, err := kp.Sign(n)
		if err != nil {
			t.Fatal(err)
		}
		return sig
	}}
}

func signerOfUser(t *testing.T, u processUser) signer {
	t.Helper()
	return signer{public: u.public, sign: func(n []byte) []byte {
		sig, err := u.sign(n)
		if err != nil {
			t.Fatal(err)
		}
		return sig
	}}
}

// THE AUTHENTICATOR ADMITS ITS FOUR KINDS AND NOTHING ELSE, binds each to its
// account, and counts every refusal in its class.
//
// The four: a route presenting a ring key's route credential; a leaf link
// presenting a ring key's leaf nkey and signing the nonce; an operator on the
// client listener presenting a ring key's operator nkey and signing it; this
// process's own clients, in process, as the engine (the global account) or the
// membership code (the system account). Every other kind, transport and
// credential is refused — the operator's key in process, the engine's over a
// socket, a leaf key as an operator, a key the ring does not hold, a held key
// whose proof fails, a username or a token where an nkey is the credential —
// and counted under its listener and what it presented.
//
// Over a started member whose one peer is a port nothing listens on, so its
// accounts are the ones a running broker binds to and nothing connects to it
// but the cases.
//
// Mutations: accept the engine key over TCP → its row, red; accept any nkey
// with a valid signature → the foreign-key rows, red; bind the operator or the
// system key to the wrong account → their rows, red; accept a leaf key as an
// operator → its row, red; verify a leaf against the active key only → the k1
// leaf row, red.
func TestTheAuthenticatorAdmitsOnlyItsFourKinds(t *testing.T) {
	t.Parallel()
	member := Config{ServerName: "a", ClusterName: "fleet", StoreDir: t.TempDir(),
		FleetAuth: fleetAuthOf(t, authRing)}
	auth := testAuth(t, member)
	global, system := auth.ns.GlobalAccount(), auth.ns.SystemAccount()
	if system == nil {
		t.Fatal("a clustered member declares no system account")
	}

	var leaves, operators []signer
	for _, key := range authRing.Keys {
		leafSeed, _, err := derivedNkey(LeafKeyDomain, key)
		if err != nil {
			t.Fatal(err)
		}
		leaves = append(leaves, signerOfSeed(t, leafSeed))
		operatorSeed, _, err := derivedNkey(ClientKeyDomain, key)
		if err != nil {
			t.Fatal(err)
		}
		operators = append(operators, signerOfSeed(t, operatorSeed))
	}
	foreignKP, err := nkeys.CreateUser()
	if err != nil {
		t.Fatal(err)
	}
	foreignSeed, err := foreignKP.Seed()
	if err != nil {
		t.Fatal(err)
	}
	foreign := signerOfSeed(t, string(foreignSeed))
	engine, sys := signerOfUser(t, auth.engine), signerOfUser(t, auth.system)
	routeUser := func(id string) (string, string) {
		return runtoken.KeyTag(RouteKeyDomain, id), auth.fleet.routes[runtoken.KeyTag(RouteKeyDomain, id)].password
	}
	k1User, k1Pass := routeUser("k1")
	k2User, k2Pass := routeUser("k2")

	nonce := []byte("the authenticator cases' nonce")
	nkey := func(s signer) server.ClientOpts {
		return server.ClientOpts{Nkey: s.public,
			Sig: base64.RawURLEncoding.EncodeToString(s.sign(nonce))}
	}
	badSig := func(s signer) server.ClientOpts {
		o := nkey(s)
		o.Sig = base64.RawURLEncoding.EncodeToString(s.sign([]byte("another nonce")))
		return o
	}
	admitted := PeerRefusal{}
	for _, tc := range []struct {
		name    string
		kind    int
		remote  net.Addr
		opts    server.ClientOpts
		refused PeerRefusal
		account *server.Account
	}{
		{"a route with no credential", server.ROUTER, overTCP, server.ClientOpts{}, PeerRefusal{ListenerRoute, PresentedNone}, nil},
		{"a route on the active key", server.ROUTER, overTCP, server.ClientOpts{Username: k2User, Password: k2Pass}, admitted, nil},
		{"a route on the other ring key", server.ROUTER, overTCP, server.ClientOpts{Username: k1User, Password: k1Pass}, admitted, nil},
		{"a route on a key the ring lacks", server.ROUTER, overTCP, server.ClientOpts{Username: runtoken.KeyTag(RouteKeyDomain, "k9"), Password: k1Pass}, PeerRefusal{ListenerRoute, PresentedUnknown}, nil},
		{"a route on a held key with another password", server.ROUTER, overTCP, server.ClientOpts{Username: k1User, Password: k2Pass}, PeerRefusal{ListenerRoute, PresentedInvalid}, nil},
		{"a leaf with no credential", server.LEAF, overTCP, server.ClientOpts{}, PeerRefusal{ListenerLeaf, PresentedNone}, nil},
		{"a leaf on the active key", server.LEAF, overTCP, nkey(leaves[1]), admitted, global},
		{"a leaf on the other ring key", server.LEAF, overTCP, nkey(leaves[0]), admitted, global},
		{"a leaf on a foreign key", server.LEAF, overTCP, nkey(foreign), PeerRefusal{ListenerLeaf, PresentedUnknown}, nil},
		{"a leaf on an operator key", server.LEAF, overTCP, nkey(operators[1]), PeerRefusal{ListenerLeaf, PresentedUnknown}, nil},
		{"a leaf with a bad signature", server.LEAF, overTCP, badSig(leaves[1]), PeerRefusal{ListenerLeaf, PresentedInvalid}, nil},
		{"a leaf with a password", server.LEAF, overTCP, server.ClientOpts{Username: "leaf", Password: "p"}, PeerRefusal{ListenerLeaf, PresentedUnknown}, nil},
		{"the engine in process", server.CLIENT, overPipe, nkey(engine), admitted, global},
		{"the membership code in process", server.CLIENT, overPipe, nkey(sys), admitted, system},
		{"nothing in process", server.CLIENT, overPipe, server.ClientOpts{}, PeerRefusal{ListenerClient, PresentedNone}, nil},
		{"an operator key in process", server.CLIENT, overPipe, nkey(operators[1]), PeerRefusal{ListenerClient, PresentedUnknown}, nil},
		{"a foreign key in process", server.CLIENT, overPipe, nkey(foreign), PeerRefusal{ListenerClient, PresentedUnknown}, nil},
		{"the engine key with a bad signature", server.CLIENT, overPipe, badSig(engine), PeerRefusal{ListenerClient, PresentedInvalid}, nil},
		{"a token in process", server.CLIENT, overPipe, server.ClientOpts{Token: "t"}, PeerRefusal{ListenerClient, PresentedUnknown}, nil},
		{"an operator on the active key", server.CLIENT, overTCP, nkey(operators[1]), admitted, global},
		{"an operator on the other ring key", server.CLIENT, overTCP, nkey(operators[0]), admitted, global},
		{"the engine key over TCP", server.CLIENT, overTCP, nkey(engine), PeerRefusal{ListenerClient, PresentedUnknown}, nil},
		{"the system key over TCP", server.CLIENT, overTCP, nkey(sys), PeerRefusal{ListenerClient, PresentedUnknown}, nil},
		{"a leaf key over TCP", server.CLIENT, overTCP, nkey(leaves[1]), PeerRefusal{ListenerClient, PresentedUnknown}, nil},
		{"a foreign key over TCP", server.CLIENT, overTCP, nkey(foreign), PeerRefusal{ListenerClient, PresentedUnknown}, nil},
		{"an operator key with a bad signature", server.CLIENT, overTCP, badSig(operators[1]), PeerRefusal{ListenerClient, PresentedInvalid}, nil},
		{"nothing over TCP", server.CLIENT, overTCP, server.ClientOpts{}, PeerRefusal{ListenerClient, PresentedNone}, nil},
		{"the route credential as a client", server.CLIENT, overTCP, server.ClientOpts{Username: k2User, Password: k2Pass}, PeerRefusal{ListenerClient, PresentedUnknown}, nil},
		{"a token over TCP", server.CLIENT, overTCP, server.ClientOpts{Token: "t"}, PeerRefusal{ListenerClient, PresentedUnknown}, nil},
		{"a gateway", server.GATEWAY, overTCP, server.ClientOpts{Username: k2User, Password: k2Pass}, PeerRefusal{ListenerClient, PresentedUnknown}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			auth.tally = newRefusalTally(time.Now)
			c := &fakeConn{kind: tc.kind, opts: tc.opts, nonce: nonce, remote: tc.remote}
			got := auth.Check(c)
			counted := map[PeerRefusal]uint64{}
			auth.tally.install(func(r PeerRefusal, n uint64) { counted[r] += n })
			if want := tc.refused == admitted; got != want {
				t.Fatalf("admitted = %v, want %v", got, want)
			}
			if tc.refused == admitted {
				if len(counted) != 0 {
					t.Errorf("an admission was counted as refusals %v", counted)
				}
				switch {
				case tc.account == nil && c.registered != nil:
					t.Errorf("a route was bound to account %v", c.registered.Account)
				case tc.account != nil && (c.registered == nil || c.registered.Account != tc.account):
					t.Errorf("bound as %+v, want account %s", c.registered, tc.account.Name)
				}
				return
			}
			if c.registered != nil {
				t.Errorf("a refused connection was bound as %+v", c.registered)
			}
			if len(counted) != 1 || counted[tc.refused] != 1 {
				t.Errorf("counted %v, want one %v", counted, tc.refused)
			}
		})
	}

	// A MEMBER WITH NO SYSTEM ACCOUNT ADMITS NO SYSTEM KEY: a solo member
	// mints none, and a key from another start is a stranger's.
	solo := testAuth(t, Config{StoreDir: t.TempDir()})
	stray, err := newProcessUser("stray system account")
	if err != nil {
		t.Fatal(err)
	}
	c := &fakeConn{kind: server.CLIENT, opts: nkey(signerOfUser(t, stray)), nonce: nonce, remote: overPipe}
	if solo.Check(c) {
		t.Errorf("a solo member admitted a system key in process, bound as %+v", c.registered)
	}
}

// A REFUSAL IS LOGGED ONCE A MINUTE PER CLASS, AND COUNTED EVERY TIME.
//
// A refused peer re-dials every second or so, so a line per attempt is the same
// cause until nobody reads it. A class's first refusal logs at once; the rest
// inside the minute log nothing; the first past a minute since that class's
// last line logs carrying every refusal since, itself included; another class
// logs at once. And the line names a key only in a shape a key has: a route
// username of a key tag's shape, never 64 bytes a stranger chose.
//
// Mutations: drop the limiter → the quiet minute logs, red; key the limiter on
// the presented key → the second class's row, red (one class, two keys);
// measure the minute from the first refusal rather than the last line → the
// third line, red.
func TestARefusalIsLoggedOnceAMinutePerClass(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	at := time.Date(2026, 10, 30, 9, 0, 0, 0, time.UTC)
	now := func() time.Time { mu.Lock(); defer mu.Unlock(); return at }
	advance := func(d time.Duration) { mu.Lock(); at = at.Add(d); mu.Unlock() }
	var logged bytes.Buffer
	auth := &brokerAuth{fleet: fleetAuthOf(t, authRing), tally: newRefusalTally(now),
		log: slog.New(slog.NewJSONHandler(&logged, nil))}
	lines := func() []map[string]any {
		var out []map[string]any
		for _, line := range strings.Split(strings.TrimSpace(logged.String()), "\n") {
			if line == "" {
				continue
			}
			var m map[string]any
			if err := json.Unmarshal([]byte(line), &m); err != nil {
				t.Fatalf("a log line is not JSON: %q", line)
			}
			out = append(out, m)
		}
		return out
	}
	route := func(username string) *fakeConn {
		return &fakeConn{kind: server.ROUTER, remote: overTCP,
			opts: server.ClientOpts{Username: username, Password: "x"}}
	}
	stranger := strings.Repeat("A", 64)
	tagged := runtoken.KeyTag(RouteKeyDomain, "k9")

	auth.Check(route(stranger))
	if got := lines(); len(got) != 1 || got[0]["refused"] != float64(1) || got[0]["key"] != "" {
		t.Fatalf("the first refusal logged %v, want one line with refused 1 naming no key", got)
	}
	for range 99 {
		advance(100 * time.Millisecond)
		auth.Check(route(tagged))
	}
	if got := lines(); len(got) != 1 {
		t.Fatalf("refusals inside the minute logged %d lines, want the first alone", len(got))
	}
	advance(time.Minute)
	auth.Check(route(tagged))
	got := lines()
	if len(got) != 2 || got[1]["refused"] != float64(100) || got[1]["key"] != tagged {
		t.Fatalf("the refusal past the minute logged %v, want refused 100 naming %s", got, tagged)
	}
	// AND THE NEXT MINUTE IS THE LAST LINE'S, not the first refusal's.
	advance(30 * time.Second)
	auth.Check(route(tagged))
	if got := lines(); len(got) != 2 {
		t.Fatalf("a refusal half a minute after the last line logged: %v", got[len(got)-1])
	}
	// ANOTHER CLASS LOGS AT ONCE.
	auth.Check(&fakeConn{kind: server.LEAF, remote: overTCP})
	if got := lines(); len(got) != 3 || got[2]["listener"] != "leaf" || got[2]["presented"] != "none" {
		t.Fatalf("another class's first refusal logged %v", got)
	}
	for _, line := range lines() {
		if key, _ := line["key"].(string); strings.Contains(key, stranger) {
			t.Errorf("a line names a stranger's text as a key: %v", line)
		}
	}
}

// A REFUSAL BEFORE THE COUNTER IS INSTALLED IS HANDED OVER ON INSTALL, AND
// NONE IS COUNTED TWICE OR LOST.
//
// The engine installs its counter once the broker has started, and a peer can
// be refused while it is still starting. So what was counted before reaches the
// sink on install — one call a class, with its count — and what comes after
// arrives one at a time. Raced from eight goroutines across the install, under
// -race, the sum is exactly what was sent.
//
// Mutations: install drops the backlog → the backlog row, red; hand the backlog
// over without the lock → a racing refusal is counted twice or not at all.
func TestOnPeerRefusedHandsOverTheBacklog(t *testing.T) {
	t.Parallel()
	tally := newRefusalTally(time.Now)
	routeNone := PeerRefusal{ListenerRoute, PresentedNone}
	leafUnknown := PeerRefusal{ListenerLeaf, PresentedUnknown}
	for range 3 {
		tally.count(routeNone)
	}
	tally.count(leafUnknown)

	var mu sync.Mutex
	got := map[PeerRefusal]uint64{}
	calls := map[PeerRefusal]int{}
	srv := &Server{embedded: &embeddedServer{auth: &brokerAuth{tally: tally}}}
	srv.OnPeerRefused(func(r PeerRefusal, n uint64) {
		mu.Lock()
		defer mu.Unlock()
		got[r] += n
		calls[r]++
	})
	if got[routeNone] != 3 || got[leafUnknown] != 1 || calls[routeNone] != 1 || calls[leafUnknown] != 1 {
		t.Fatalf("the backlog arrived as %v in %v calls, want 3 and 1, one call a class", got, calls)
	}
	tally.count(routeNone)
	if got[routeNone] != 4 || calls[routeNone] != 2 {
		t.Fatalf("a refusal after the install arrived as %v in %v calls", got, calls)
	}

	// RACED ACROSS THE INSTALL.
	raced := newRefusalTally(time.Now)
	var wg sync.WaitGroup
	const each = 200
	for range 8 {
		wg.Go(func() {
			for range each {
				raced.count(routeNone)
			}
		})
	}
	var sum uint64
	var sumMu sync.Mutex
	raced.install(func(_ PeerRefusal, n uint64) { sumMu.Lock(); sum += n; sumMu.Unlock() })
	wg.Wait()
	if sum != 8*each {
		t.Fatalf("raced across the install, %d refusals were handed over, want %d", sum, 8*each)
	}
}
