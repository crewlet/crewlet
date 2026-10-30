package jetstream

import (
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nkeys"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/runtoken"
)

// brokerAuth decides every connection to one embedded broker: nats-server's
// CustomClientAuthentication for its clients and its leaf links, and its
// CustomRouterAuthentication for a clustered member's routes. The package
// doc's "Every connection presents a key" is the decision; this is the one
// place it is made.
//
// SAFE FOR CONCURRENT USE: the server calls Check from its own goroutines, one
// per connection, and nothing here changes after the server starts but the
// refusal tally, which holds its own lock.
type brokerAuth struct {
	// ns is the server this decides for, for the accounts an admitted
	// connection is bound to. Set once the server is built and before it
	// starts, and read only from the connections Start lets in — the go
	// statement that starts it orders the two.
	ns *server.Server
	// name is the server's name, which every line this writes carries: a
	// process can run several brokers (a fleet test does), and a refusal
	// that names no member names nothing.
	name string
	// fleet is what this node presents and accepts on its fleet listeners:
	// every key of its ring.
	fleet FleetAuth
	// engine is this process's own clients' identity, bound to the global
	// account and admitted in process only.
	engine processUser
	// system is the membership code's identity, bound to the system account
	// and admitted in process only; zero unless this is a clustered member,
	// which alone declares a system account.
	system processUser
	tally  *refusalTally
	log    *slog.Logger
}

// newBrokerAuth builds the authenticator for one broker start: this process's
// own key, minted fresh — and the system account's, on a clustered member —
// beside the fleet's credentials cfg carries.
//
// A KEY PER START, whose private half never leaves this process: nothing
// outside it ever holds anything that authenticates as the engine or as the
// membership code, so no file, log line or peer can be the way in.
func newBrokerAuth(cfg Config) (*brokerAuth, error) {
	engine, err := newProcessUser("engine")
	if err != nil {
		return nil, err
	}
	var system processUser
	if cfg.ClusterName != "" {
		if system, err = newProcessUser("system account"); err != nil {
			return nil, err
		}
	}
	return &brokerAuth{
		name: cfg.ServerName, fleet: cfg.FleetAuth, engine: engine, system: system,
		tally: newRefusalTally(time.Now),
		log:   logging.Get("queue.jetstream").With("server", cfg.ServerName),
	}, nil
}

// Listener is the listener a refused connection reached: a member's route
// port, its leaf listener, or a client connection — in process, or on a
// clustered member's loopback client listener.
type Listener string

const (
	ListenerRoute  Listener = "route"
	ListenerLeaf   Listener = "leaf"
	ListenerClient Listener = "client"
)

// listenerKinds is every listener, in the order a report lists them.
var listenerKinds = []Listener{ListenerRoute, ListenerLeaf, ListenerClient}

// Valid reports whether a listener off the wire is one this build knows.
func (l Listener) Valid() bool { return slices.Contains(listenerKinds, l) }

// Presented is what a refused connection presented: nothing, a credential no
// key of this node's ring derives, or a key this node holds with a proof its
// material does not produce.
type Presented string

const (
	PresentedNone    Presented = "none"
	PresentedUnknown Presented = "unknown"
	PresentedInvalid Presented = "invalid"
)

// presentedKinds is every answer, in the order a report lists them.
var presentedKinds = []Presented{PresentedNone, PresentedUnknown, PresentedInvalid}

// Valid reports whether an answer off the wire is one this build knows.
func (p Presented) Valid() bool { return slices.Contains(presentedKinds, p) }

// PeerRefusal is one class of refused connection: the listener it reached and
// what it presented. Nine classes, so whatever a scanner sends, what is kept
// about refusals is bounded.
type PeerRefusal struct {
	Listener  Listener
	Presented Presented
}

// verdict is what [brokerAuth.judge] decides about one connection.
type verdict struct {
	// refused is the class of a refusal, and zero for an admission.
	refused PeerRefusal
	// key is what a refusal line names the key by: a configured id where
	// this node holds the key, the presented key where it is well formed,
	// and empty otherwise — never a password, never attacker text of any
	// other shape.
	key string
	// account is the account an admitted connection is bound to; nil binds
	// none, which is a route's (routes carry every account).
	account *server.Account
	// user is the name an admitted connection is registered under, in the
	// server's own reports.
	user string
	// operator is the key id an admitted operator connection presented,
	// and empty for every other admission.
	operator string
}

// Check admits one connection or refuses it. A refusal is counted and, once a
// minute a class, logged (see [refusalTally]); an operator admitted on a
// member's client listener is logged every time.
func (a *brokerAuth) Check(c server.ClientAuthentication) bool {
	v := a.judge(c)
	remote := ""
	if addr := c.RemoteAddress(); addr != nil {
		remote = addr.String()
	}
	if v.refused != (PeerRefusal{}) {
		a.refuse(v.refused, v.key, remote)
		return false
	}
	if v.account != nil {
		// THE ONE BINDING CALL ClientAuthentication offers. Its error is
		// not returned, and needs no reading here: only an account
		// connection limit closes the connection, and none is configured;
		// a nil account is never handed in (both are the server's own,
		// built before it starts); and a leaf loop is keyed on what the
		// leaf's CONNECT records after this returns.
		c.RegisterUser(&server.User{Username: v.user, Account: v.account})
	}
	if v.operator != "" {
		a.log.Info("broker_operator_admitted", "key", v.operator, "remote", remote,
			"detail", "an operator's client connected to this member's loopback client "+
				"listener with the operator key derived from secrets.keys — the nats "+
				"tool, on this member's own host")
	}
	return true
}

// judge decides one connection, by the listener it reached.
func (a *brokerAuth) judge(c server.ClientAuthentication) verdict {
	opts := c.GetOpts()
	switch c.Kind() {
	case server.ROUTER:
		return a.judgeRoute(opts)
	case server.LEAF:
		return a.judgeLeaf(opts, c.GetNonce())
	case server.CLIENT:
		if inProcess(c.RemoteAddress()) {
			return a.judgeInProcess(opts, c.GetNonce())
		}
		return a.judgeOperator(opts, c.GetNonce())
	default:
		// NO OTHER KIND IS CONFIGURED — no gateway, websocket or MQTT
		// listener — so one arriving here is refused unrecognised rather
		// than waved through on a kind nobody decided about.
		return verdict{refused: PeerRefusal{ListenerClient, PresentedUnknown}}
	}
}

// judgeRoute admits a route presenting a route credential of this node's
// ring: the username a key's tag, the password the one that key derives.
//
// A PASSWORD, because a NATS route carries a username and password or a
// certificate and nothing else — its CONNECT has no signature. So it crosses
// the fleet network in clear once per connection (the package doc's residual).
// Compared in constant time: a route port is reachable by whatever the network
// lets reach it.
func (a *brokerAuth) judgeRoute(opts *server.ClientOpts) verdict {
	refused := func(p Presented, key string) verdict {
		return verdict{refused: PeerRefusal{ListenerRoute, p}, key: key}
	}
	if opts.Username == "" {
		return refused(PresentedNone, "")
	}
	held, ok := a.fleet.routes[opts.Username]
	if !ok {
		return refused(PresentedUnknown, routeTagShaped(opts.Username))
	}
	if subtle.ConstantTimeCompare([]byte(opts.Password), []byte(held.password)) != 1 {
		return refused(PresentedInvalid, held.id)
	}
	return verdict{}
}

// judgeLeaf admits a leaf link presenting a leaf nkey of this node's ring and
// signing this connection's nonce with it.
func (a *brokerAuth) judgeLeaf(opts *server.ClientOpts, nonce []byte) verdict {
	refused := func(p Presented, key string) verdict {
		return verdict{refused: PeerRefusal{ListenerLeaf, p}, key: key}
	}
	if opts.Nkey == "" {
		return refused(nothingOrUnknown(opts), "")
	}
	held, ok := a.fleet.leaves[opts.Nkey]
	if !ok {
		return refused(PresentedUnknown, publicShaped(opts.Nkey))
	}
	if !signedNonce(opts.Nkey, opts.Sig, nonce) {
		return refused(PresentedInvalid, held.id)
	}
	return verdict{account: a.ns.GlobalAccount(), user: "leaf:" + held.id}
}

// judgeInProcess admits a client of this process: the engine's key, on the
// global account, and — on a member that declared one — the membership code's,
// on the system account. Nothing else is in process, so nothing else is let
// in this way: not an operator's key, and not another start's engine key.
func (a *brokerAuth) judgeInProcess(opts *server.ClientOpts, nonce []byte) verdict {
	refused := func(p Presented, key string) verdict {
		return verdict{refused: PeerRefusal{ListenerClient, p}, key: key}
	}
	var account *server.Account
	var user string
	switch {
	case opts.Nkey == "":
		return refused(nothingOrUnknown(opts), "")
	case opts.Nkey == a.engine.public:
		account, user = a.ns.GlobalAccount(), "engine"
	case a.system.declared() && opts.Nkey == a.system.public:
		account, user = a.ns.SystemAccount(), "system"
	default:
		return refused(PresentedUnknown, publicShaped(opts.Nkey))
	}
	if !signedNonce(opts.Nkey, opts.Sig, nonce) {
		return refused(PresentedInvalid, user)
	}
	return verdict{account: account, user: user}
}

// judgeOperator admits a client of a clustered member's loopback client
// listener presenting the operator nkey derived from a key of this node's ring
// and signing this connection's nonce with it — the nats tool, for the broker
// gestures no engine verb performs. Bound to the global account, never the
// system one: a membership change goes through the engine.
func (a *brokerAuth) judgeOperator(opts *server.ClientOpts, nonce []byte) verdict {
	refused := func(p Presented, key string) verdict {
		return verdict{refused: PeerRefusal{ListenerClient, p}, key: key}
	}
	if opts.Nkey == "" {
		return refused(nothingOrUnknown(opts), "")
	}
	id, ok := a.fleet.operators[opts.Nkey]
	if !ok {
		return refused(PresentedUnknown, publicShaped(opts.Nkey))
	}
	if !signedNonce(opts.Nkey, opts.Sig, nonce) {
		return refused(PresentedInvalid, id)
	}
	return verdict{account: a.ns.GlobalAccount(), user: "operator:" + id, operator: id}
}

// inProcess reports whether a connection came through the in-memory pipe this
// process's own clients dial ([embeddedServer.connect]) rather than a socket.
func inProcess(addr net.Addr) bool { return addr != nil && addr.Network() == "pipe" }

// nothingOrUnknown is what a connection that presented no nkey presented:
// nothing at all, or a credential of another kind — a username, a password, a
// token, a JWT — which no key of the ring derives on a listener that takes an
// nkey.
func nothingOrUnknown(opts *server.ClientOpts) Presented {
	if opts.Username == "" && opts.Password == "" && opts.Token == "" &&
		opts.JWT == "" && opts.Sig == "" {
		return PresentedNone
	}
	return PresentedUnknown
}

// signedNonce reports whether sig is public's signature over nonce, read as
// the server reads one: unpadded base64url, then standard base64.
func signedNonce(public, sig string, nonce []byte) bool {
	if sig == "" || len(nonce) == 0 {
		return false
	}
	raw, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		if raw, err = base64.StdEncoding.DecodeString(sig); err != nil {
			return false
		}
	}
	key, err := nkeys.FromPublicKey(public)
	if err != nil {
		return false
	}
	return key.Verify(nonce, raw) == nil
}

// routeTagLen is how long a route username is: a key's tag
// ([runtoken.KeyTag]), whose length is runtoken's to decide.
var routeTagLen = len(runtoken.KeyTag(RouteKeyDomain, ""))

// routeTagShaped is a presented route username where it has a key tag's shape
// — its length, of the base64url alphabet — and empty otherwise, so a refusal
// line names a peer's key and never a stranger's text.
func routeTagShaped(username string) string {
	if len(username) != routeTagLen {
		return ""
	}
	for _, r := range username {
		if !(r >= 'A' && r <= 'Z' || r >= 'a' && r <= 'z' || r >= '0' && r <= '9' ||
			r == '-' || r == '_') {
			return ""
		}
	}
	return username
}

// publicShaped is a presented nkey where it is a well-formed user public key,
// and empty otherwise, for routeTagShaped's reason.
func publicShaped(key string) string {
	if nkeys.IsValidPublicUserKey(key) {
		return key
	}
	return ""
}

// refuse counts one refusal and logs it when its class is due a line.
func (a *brokerAuth) refuse(r PeerRefusal, key, remote string) {
	logged, refused := a.tally.count(r)
	if !logged {
		return
	}
	a.log.Warn("broker_peer_refused", "listener", string(r.Listener),
		"presented", string(r.Presented), "key", key, "remote", remote,
		"refused", refused, "detail", refusalDetail(r.Presented, key))
}

// refusalDetail is a refusal's remedy, by what was presented.
func refusalDetail(p Presented, key string) string {
	switch p {
	case PresentedNone:
		return "a connection presented no fleet credential: a process outside the " +
			"fleet, or a node not yet running this build"
	case PresentedUnknown:
		what := "a credential"
		if key != "" {
			what = "key " + key
		}
		return "it presented " + what + ", which no key of this node's secrets.keys " +
			"derives: the two nodes' keyrings differ — give both the same keys, or " +
			"finish the rotation (docs/concepts/secret-store.md#rotating-the-keyring-with-nothing-in-flight-lost)"
	default:
		return "it presented key " + key + " with a credential this node's material " +
			"for that id does not produce: the same key id carries different material " +
			"on the two nodes"
	}
}

// refusalLogEvery is how often one class of refusal is logged again.
//
// A MINUTE, because a refused peer does not go away: a route re-dials every
// one to thirty seconds and a leaf every second (nats-server's
// DEFAULT_ROUTE_CONNECT, _MAX and DEFAULT_LEAF_NODE_RECONNECT), so a line per
// attempt is the same cause repeated until nobody reads it. One a minute a
// class keeps the cause readable — the first at once, then the next carrying
// every refusal since — and the counter has every one.
const refusalLogEvery = time.Minute

// refusalTally counts one broker's refusals, hands them to whoever counts them
// ([Server.OnPeerRefused]), and decides which are logged.
//
// # The arithmetic, exactly
//
// A class's first refusal logs at once. A later one logs only when
// refusalLogEvery has passed since that class's LAST LINE, and carries every
// refusal of the class since it, itself included; refusals inside the minute
// are counted and not logged. A class that goes quiet logs nothing more.
type refusalTally struct {
	mu  sync.Mutex
	now func() time.Time
	// backlog is every refusal counted before a sink was installed, by
	// class: a refusal can land while the broker is still starting.
	backlog map[PeerRefusal]uint64
	sink    func(PeerRefusal, uint64)
	// lines is, by class, when its last line was written and how many
	// refusals it has had since.
	lines map[PeerRefusal]refusalLine
}

type refusalLine struct {
	at    time.Time
	since uint64
}

func newRefusalTally(now func() time.Time) *refusalTally {
	return &refusalTally{now: now, backlog: map[PeerRefusal]uint64{},
		lines: map[PeerRefusal]refusalLine{}}
}

// count records one refusal of class r, and says whether it is logged and,
// when it is, how many refusals of its class the line carries.
//
// THE SINK IS CALLED OUTSIDE THE LOCK, and decided inside it: a refusal that
// finds no sink is in the backlog the install hands over, and one that finds
// the sink is not — so a refusal racing the install is delivered once.
func (t *refusalTally) count(r PeerRefusal) (logged bool, refused uint64) {
	t.mu.Lock()
	sink := t.sink
	if sink == nil {
		t.backlog[r]++
	}
	line := t.lines[r]
	line.since++
	if now := t.now(); line.at.IsZero() || now.Sub(line.at) >= refusalLogEvery {
		logged, refused = true, line.since
		line.at, line.since = now, 0
	}
	t.lines[r] = line
	t.mu.Unlock()
	if sink != nil {
		sink(r, 1)
	}
	return logged, refused
}

// install hands sink the backlog, one call a class with its count, and every
// later refusal as it happens. The backlog and the switch are taken under one
// lock, so nothing counted is handed twice or not at all.
func (t *refusalTally) install(sink func(PeerRefusal, uint64)) {
	t.mu.Lock()
	backlog := t.backlog
	t.backlog = map[PeerRefusal]uint64{}
	t.sink = sink
	t.mu.Unlock()
	for _, l := range listenerKinds {
		for _, p := range presentedKinds {
			if n := backlog[PeerRefusal{l, p}]; n > 0 {
				sink(PeerRefusal{l, p}, n)
			}
		}
	}
}

// OnPeerRefused hands sink every refusal this broker has counted so far — one
// call per class, with its count — and then each later one as it happens, with
// a count of 1. The sink must be safe for concurrent use: refusals arrive on
// the server's own goroutines. Called once; a later call replaces the sink,
// and the backlog it hands is empty by then.
//
// A BACKLOG AT ALL, because the caller can only install once [StartServer] has
// returned, and a refusal can land while it is still waiting for the server's
// listeners: a peer dialling a member that has just bound its route port.
func (s *Server) OnPeerRefused(sink func(PeerRefusal, uint64)) {
	if s.embedded == nil || s.embedded.auth == nil {
		return
	}
	s.embedded.auth.tally.install(sink)
}

// logKeys writes this broker's broker_keys line: the key it presents, every key
// it accepts and the listeners it opened — once a start, so a rotation's
// progress reads node by node and an operator finds the client listener.
func (a *brokerAuth) logKeys(listeners string) {
	presents, accepts := "none", "none"
	if !a.fleet.IsZero() {
		presents, accepts = a.fleet.activeID, strings.Join(a.fleet.acceptedIDs(), ",")
	}
	a.log.Info("broker_keys", "presents", presents, "accepts", accepts,
		"listeners", listeners,
		"detail", "every route, leaf link and client connection to this broker "+
			"presents a key derived from secrets.keys; this node presents the "+
			"first and accepts every one of the second")
}

// openListeners names the listeners a started broker opened, with the address
// each is on, for its broker_keys line: "none" for a broker that opened none.
func openListeners(ns *server.Server, routing, servesLeaves bool) string {
	var out []string
	if routing {
		if addr := ns.ClusterAddr(); addr != nil {
			out = append(out, "route "+addr.String())
		}
	}
	if servesLeaves {
		if varz, err := ns.Varz(nil); err == nil && varz.LeafNode.Port > 0 {
			out = append(out, fmt.Sprintf("leaf %s",
				net.JoinHostPort(varz.LeafNode.Host, fmt.Sprint(varz.LeafNode.Port))))
		}
	}
	if addr := ns.Addr(); addr != nil {
		out = append(out, "client "+addr.String()+" (this host only)")
	}
	if len(out) == 0 {
		return "none"
	}
	return strings.Join(out, ", ")
}
