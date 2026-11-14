package engine

import (
	"fmt"
	"go/ast"
	"go/constant"
	"go/parser"
	"go/token"
	"go/types"
	"net/http"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/httprelay"
	"github.com/crewlet/crewlet/internal/presence"
	queuemem "github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/recordseal"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/signedask"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// EVERY CHANNEL A NODE CARRIES OUT A REQUEST ON SIGNS IN DOMAINS OF ITS OWN —
// the estate service, the fleet broker's membership, a note to a running turn,
// a read of a box asked of its host, a box's tool call relayed to the process
// holding its session, the Dashboard Assistant's replays and stops, a code
// search, and a test of an MCP server asked of the nodes it runs on — and
// none of them in a state log's. So does a person's answer to a parked run,
// sealed onto the run's row and handed to the resumed run as their words, and
// a note to a turn whose executor is out, sealed onto the run's row with the
// seat anchor a person's authority to send one is decided on. And so do the
// sealed channels that are no instruction: presence, which a node displays as
// a person in a room; the live relay, whose answers a node pushes to its
// sockets as a peer's appliers' work; and the assistant's frames, which a node
// streams to a person as their own turn. A domain shared with any of the
// others would let a captured frame of one verify as the other. The domain is
// in the derived key and in the MAC, so two channels sharing one would each
// verify the other's frames: a captured estate reply sent to a member as a
// broker request, or a state-log record replayed as a note. The names live in
// the packages that build them and nothing else compares them; this does,
// here, where all of them are built, and
// TestEverySignedChannelTheTreeBuildsIsClaimed holds the tree to this list.
func TestEveryInstructionChannelSignsInDomainsOfItsOwn(t *testing.T) {
	t.Parallel()
	if owner := claimedDomains(t); len(owner) != len(registeredDomains())+27 {
		t.Fatalf("%d domains claimed: %v", len(owner), owner)
	}
}

// claimedDomains builds every channel a node signs on, as the node builds it,
// and answers which channel signs in each key domain, failing t where two
// channels sign in one.
func claimedDomains(t *testing.T) map[string]string {
	t.Helper()
	ring := recordseal.OneKey("k1", "the channel gate's fleet key material")
	envelopes, err := estate.NewEnvelopes(ring)
	if err != nil {
		t.Fatal(err)
	}
	broker, err := newBrokerWire(ring)
	if err != nil {
		t.Fatal(err)
	}
	desk, err := newSteerDesk("node-channels", ring)
	if err != nil {
		t.Fatal(err)
	}
	boxReads, err := newBoxReads(ring, "node-a", nil, func() *sandbox.Coordinator { return nil })
	if err != nil {
		t.Fatal(err)
	}
	assistant, err := newAssistantWires(ring)
	if err != nil {
		t.Fatal(err)
	}
	relay, err := httprelay.New(httprelay.Options{
		Fabric: queuemem.NewBroker().Client(), Name: mcpbridge.RelayName, Node: "node-a",
		Owner: "node-a:1", Keyring: ring, Handler: http.NotFoundHandler(),
		Admit: func(*http.Request) httprelay.Verdict { return httprelay.Refuse },
	})
	if err != nil {
		t.Fatal(err)
	}
	searchWire, err := newCodeSearchWire(ring)
	if err != nil {
		t.Fatal(err)
	}
	checks, err := newMCPChecks(ring)
	if err != nil {
		t.Fatal(err)
	}
	presenceWire, err := newPresenceWire(ring)
	if err != nil {
		t.Fatal(err)
	}
	liveWire, err := newRelayWire(ring)
	if err != nil {
		t.Fatal(err)
	}
	answers, err := sandbox.NewAnswerSeal(ring)
	if err != nil {
		t.Fatal(err)
	}
	notes, err := newSteerSeals(ring)
	if err != nil {
		t.Fatal(err)
	}
	owner := map[string]string{}
	claim := func(domain, by string) {
		if prior, taken := owner[domain]; taken {
			t.Errorf("%s signs in %q, which %s signs in too", by, domain, prior)
		}
		owner[domain] = by
	}
	for _, d := range registeredDomains() {
		claim(d.Name(), "the "+d.Name()+" state log")
	}
	for name, domains := range map[string][2]string{
		"the estate service":           pair(envelopes.Domains()),
		"the fleet broker":             pair(broker.Domains()),
		"a note to a running turn":     pair(desk.wire.Domains()),
		"a box read asked of its host": pair(boxReads.wire.Domains()),
		"the assistant's replay":       pair(assistant.replays.Domains()),
		"the assistant's stop":         pair(assistant.stops.Domains()),
		"the live relay":               pair(liveWire.Domains()),
		"a code search":                pair(searchWire.Domains()),
		"an MCP server's test":         pair(checks.wire.Domains()),
	} {
		claim(domains[0], name+"'s requests")
		claim(domains[1], name+"'s replies")
	}
	for _, domain := range relay.Domains() {
		claim(domain, "the tool bridge's relay")
	}
	claim(assistant.frames.Domain(), "an assistant turn's frame")
	claim(presenceWire.Domain(), "a presence frame")
	claim(answers.Domain(), "an answer to a parked run")
	note, seat := notes.Domains()
	claim(note, "a note on a run record")
	claim(seat, "a run record's seat anchor")
	return owner
}

func pair(request, reply string) [2]string { return [2]string{request, reply} }

// EVERY CHANNEL THE TREE SIGNS ON IS ONE THE GATE ABOVE HOLDS APART. That gate
// compares the channels it builds, and a channel built without a line there
// was compared with nothing: the MCP server's test channel signed in domains
// no gate held apart from any other's. So this reads the module's source for
// every call that names a key domain — signedask's New and NewOneWay,
// recordseal's NewSigner and NewVerifier — and refuses a domain the gate does
// not claim, one a state log signs in, and one that two functions build,
// since the gate claims each domain for one channel and one channel has one
// constructor.
//
// WITH TYPES, because a name is a constant spelled anywhere: `steer.Channel`
// and `liveRelayChannel` read like any identifier. A name a function is
// handed as a parameter, or as a field of one, is read at each call of that
// function — the run record's seals take theirs through newSealPair, and the
// tool bridge's relay through httprelay.New's options — so a channel built
// through a constructor is judged by the names its callers give.
//
// A NAME THE WALK CANNOT READ FAILS IT, unless [computedChannelNames] says
// why the function that spells it signs only in domains the gate claims; an
// excuse that excuses nothing fails too. So does a signing function used as a
// value, or a function a name enters, since either could be called with any
// name.
//
// AND EVERY DOMAIN THE GATE CLAIMS IS ONE THE WALK FOUND BUILT, but the state
// logs', which the excused functions build: a walk that stopped reading the
// tree would otherwise pass, and so would a claim whose channel moved to a
// name the walk cannot read.
//
// Mutation: take the MCP server's test out of claimedDomains and this fails
// naming internal/engine/mcpcheck.go; name newSealPair's verifier after its
// own parameter plus a suffix and it fails naming the domain no gate claims.
func TestEverySignedChannelTheTreeBuildsIsClaimed(t *testing.T) {
	t.Parallel()
	claimed := claimedDomains(t)
	stateLogs := map[string]bool{}
	for _, d := range registeredDomains() {
		stateLogs[d.Name()] = true
	}
	walk := walkSignedChannels(t)
	t.Logf("read %d packages and %d calls that name a key domain", walk.read, walk.calls)
	builtBy := map[string][]string{}
	for _, built := range walk.built {
		for _, domain := range built.kind.domains(t, built.name) {
			if !slices.Contains(builtBy[domain], built.in) {
				builtBy[domain] = append(builtBy[domain], built.in)
			}
			switch {
			case stateLogs[domain]:
				t.Errorf("%s signs in %q, a state log's domain, so the log's records would verify as "+
					"its frames: name it apart", built, domain)
			case claimed[domain] == "":
				t.Errorf("%s signs in %q, which no channel claimedDomains builds claims: build the "+
					"channel there, where its domains are held apart from every other's", built, domain)
			}
		}
	}
	for domain, in := range builtBy {
		if len(in) > 1 {
			t.Errorf("%q is signed in by the channels %s build, and the gate claims it for one: "+
				"two channels in one domain verify each other's frames", domain, strings.Join(in, " and "))
		}
	}
	for domain, by := range claimed {
		if !stateLogs[domain] && len(builtBy[domain]) == 0 {
			t.Errorf("%s signs in %q, and no call the walk read names it: the walk has stopped "+
				"reading the tree, or cannot read the name of the call that does", by, domain)
		}
	}
	excused := map[string]bool{}
	for _, unread := range walk.unread {
		if _, ok := computedChannelNames[unread.in]; ok {
			excused[unread.in] = true
			continue
		}
		t.Errorf("%s: the walk cannot read the key domain this names, so no gate holds it apart: "+
			"name it with a constant, or say in computedChannelNames why %s signs only in domains "+
			"claimedDomains claims", unread, unread.in)
	}
	for in, why := range computedChannelNames {
		if !excused[in] {
			t.Errorf("computedChannelNames excuses %s because %s, and it hands no signing call a "+
				"name the walk cannot read: take it out", in, why)
		}
	}
	for _, use := range walk.asValue {
		t.Error(use)
	}
}

// computedChannelNames are the functions that hand a signing call a name no
// constant gives, each with why every domain it signs in is one the gate
// claims anyway. By the function's full name, its package's path less the
// module's.
var computedChannelNames = map[string]string{
	"(*internal/engine.stateLog).signerFor": "it signs a state log's records in the log's own " +
		"domain, which claimedDomains claims for each of registeredDomains()",
	"(*internal/engine.stateLog).verifierFor": "it opens a state log's records in the log's own " +
		"domain, which claimedDomains claims for each of registeredDomains()",
	"(*internal/engine.Engine).startStateLog": "it builds a signer in the first registered log's " +
		"domain only to refuse, at boot, a node with no record keyring, and signs nothing with it",
	"internal/backup.verifiers": "a backup's replay opens a state log's records in the domain each " +
		"required reference table names, which TestEveryRequiredTableNamesItsDomainsLog holds to " +
		"a registered log's",
}

// signingCall is how a call that names a key domain derives its domains from
// the name it is handed.
type signingCall int

const (
	// twoWay is signedask.New: a request's domain and a reply's.
	twoWay signingCall = iota
	// oneWay is signedask.NewOneWay: a frame's.
	oneWay
	// recordSeal is recordseal's NewSigner and NewVerifier: the name itself.
	recordSeal
)

// signingCalls are the functions that name a key domain, by their full names
// less the module's path.
var signingCalls = map[string]signingCall{
	"internal/signedask.New":          twoWay,
	"internal/signedask.NewOneWay":    oneWay,
	"internal/recordseal.NewSigner":   recordSeal,
	"internal/recordseal.NewVerifier": recordSeal,
}

// domains is the key domains a call of this kind signs in under name, derived
// by the call itself rather than restated, so a change to how signedask
// suffixes a name changes what the gate compares.
func (k signingCall) domains(t *testing.T, name string) []string {
	t.Helper()
	ring := recordseal.OneKey("k1", "the channel walk's fleet key material")
	switch k {
	case twoWay:
		channel, err := signedask.New[mcpCheckAsk, mcpCheckReply](name, ring)
		if err != nil {
			t.Fatalf("a two-way channel named %q: %v", name, err)
		}
		request, reply := channel.Domains()
		return []string{request, reply}
	case oneWay:
		wire, err := signedask.NewOneWay[presence.Frame](name, ring)
		if err != nil {
			t.Fatalf("a one-way channel named %q: %v", name, err)
		}
		return []string{wire.Domain()}
	default:
		// A record seal derives its key and its MAC under the domain it is
		// handed, and keeps no other name.
		if _, err := recordseal.NewSigner(name, ring); err != nil {
			t.Fatalf("a record seal named %q: %v", name, err)
		}
		return []string{name}
	}
}

// maxNameHops is how many calls a channel's name is followed through, from
// the one that names its domain out to a constant. The tree's deepest is one
// (newSealPair, httprelay.New); a name passed further is one a reader could
// not follow either, and the walk refuses it rather than chase a recursion.
const maxNameHops = 4

// channelName is the name a signing call is handed, as the walk reads it.
type channelName struct {
	op channelNameOp
	// value is a constant's.
	value string
	// parts are a concatenation's operands.
	parts [2]*channelName
	// param is the position, among the parameters of the function the name
	// is spelled in, of the parameter the name is or is a field of; field is
	// that field, or "".
	param int
	field string
	// unread says what the walk could not read.
	unread *unreadName
}

type channelNameOp int

const (
	constantName channelNameOp = iota
	joinedName
	parameterName
	unreadOp
)

// unreadName is a name, or a part of one, the walk cannot read: where, in
// which function, and what; and where the call is that it names a channel
// for, which is elsewhere when the name came through a constructor.
type unreadName struct {
	at, in, what string
	built        string
}

func (u *unreadName) String() string {
	if u.built == "" || u.built == u.at {
		return u.at + ": " + u.what
	}
	return u.at + ": " + u.what + ", naming the channel at " + u.built
}

// unreadPart is the first part of n the walk cannot read, or nil.
func (n *channelName) unreadPart() *unreadName {
	switch n.op {
	case unreadOp:
		return n.unread
	case joinedName:
		if u := n.parts[0].unreadPart(); u != nil {
			return u
		}
		return n.parts[1].unreadPart()
	}
	return nil
}

// open is whether n is, or is built from, a parameter of its function.
func (n *channelName) open() bool {
	switch n.op {
	case parameterName:
		return true
	case joinedName:
		return n.parts[0].open() || n.parts[1].open()
	}
	return false
}

// spelled is the name a name read whole spells.
func (n *channelName) spelled() string {
	if n.op == joinedName {
		return n.parts[0].spelled() + n.parts[1].spelled()
	}
	return n.value
}

// enteredName is a channel whose name enters a function through a parameter:
// its kind, its name in terms of the function's parameters, where the call
// that names its domain is, the calls the name came through, and how many
// calls of the function gave it one.
type enteredName struct {
	kind  signingCall
	name  *channelName
	built string
	via   []string
	in    string
	given int
}

// builtChannel is a call the walk read the name of: its kind, its name, the
// function that names it (the one spelling the constant it comes to), where
// the call is, and the calls its name came through.
type builtChannel struct {
	kind  signingCall
	name  string
	in    string
	built string
	via   []string
}

func (b builtChannel) String() string {
	if len(b.via) == 0 {
		return b.built
	}
	return b.built + " (named at " + strings.Join(b.via, ", then ") + ")"
}

// channelWalk is what the walk of the tree's signing calls found.
type channelWalk struct {
	fset *token.FileSet
	root string
	// entered is every function a channel's name enters through a
	// parameter of, by full name; reaches is every package holding an
	// exported one, whose importers may call it.
	entered map[string][]*enteredName
	reaches map[string]bool
	built   []builtChannel
	unread  []*unreadName
	asValue []string
	// read and calls are what the walk judged.
	read, calls int
}

// walkSignedChannels reads every call in the module's non-test source that
// names a key domain, and what each is named.
//
// IT READS ONLY WHERE SUCH A CALL OR ITS NAME CAN BE: a package that imports
// signedask or recordseal, which a call of one needs, and a package that
// imports one holding an exported function a name enters through a parameter
// of. An unexported one is called only in its own package, and a name that
// enters an exported method is refused, since any package holding the type may
// call it. sourcetree hands each package after every package of the module it
// imports, so the functions a package's imports hold are known when it is
// selected. Neither signedask nor recordseal is read: a call inside them is
// the derivation every call outside is judged by.
func walkSignedChannels(t *testing.T) *channelWalk {
	t.Helper()
	w := &channelWalk{fset: token.NewFileSet(), root: sourcetree.Root(t),
		entered: map[string][]*enteredName{}, reaches: map[string]bool{}}
	derivation := map[string]bool{
		modulePath + "/internal/signedask":  true,
		modulePath + "/internal/recordseal": true,
	}
	err := sourcetree.TypeCheck{
		Fset: w.fset,
		Select: func(p *sourcetree.Listed) bool {
			// A test-support package (a name ending in "test", the
			// certification suites) builds channels for a test, never
			// for a node.
			if strings.HasSuffix(path.Base(p.ImportPath), "test") || derivation[p.ImportPath] {
				return false
			}
			imports, ok := importsOf(p)
			if !ok {
				return true // the check reports what it cannot parse
			}
			for imported := range imports {
				if derivation[imported] || w.reaches[imported] {
					return true
				}
			}
			return false
		},
		Info: func() *types.Info {
			return &types.Info{Types: map[ast.Expr]types.TypeAndValue{},
				Defs: map[*ast.Ident]types.Object{}, Uses: map[*ast.Ident]types.Object{},
				Selections: map[*ast.SelectorExpr]*types.Selection{}}
		},
	}.Run(w.root, func(u *sourcetree.Unit) error {
		w.unit(u.Files, u.Info)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, entries := range w.entered {
		for _, e := range entries {
			if e.given == 0 {
				w.unread = append(w.unread, &unreadName{at: e.built, in: e.in,
					what: "a name " + e.in + " is handed, and nothing in the tree calls it"})
			}
		}
	}
	return w
}

// importsOf is every package p's non-test files import, or false where one
// cannot be parsed.
func importsOf(p *sourcetree.Listed) (map[string]bool, bool) {
	imports := map[string]bool{}
	for _, name := range p.GoFiles {
		f, err := parser.ParseFile(token.NewFileSet(), filepath.Join(p.Dir, name), nil, parser.ImportsOnly)
		if err != nil {
			return nil, false
		}
		for _, spec := range f.Imports {
			if imported, unquoteErr := strconv.Unquote(spec.Path.Value); unquoteErr == nil {
				imports[imported] = true
			}
		}
	}
	return imports, true
}

// placedCall is a call and the function declaration it is in, nil at
// package level.
type placedCall struct {
	call *ast.CallExpr
	in   *ast.FuncDecl
}

// unit reads one package's signing calls, then the calls of every function
// a name enters — its own and its imports' — then every use of either that is
// not a call.
func (w *channelWalk) unit(files []*ast.File, info *types.Info) {
	w.read++
	var calls []placedCall
	callees := map[*ast.Ident]bool{}
	for _, file := range files {
		for _, decl := range file.Decls {
			fn, _ := decl.(*ast.FuncDecl)
			ast.Inspect(decl, func(n ast.Node) bool {
				if call, ok := n.(*ast.CallExpr); ok {
					calls = append(calls, placedCall{call: call, in: fn})
					if id := calleeIdent(call); id != nil {
						callees[id] = true
					}
				}
				return true
			})
		}
	}
	for _, c := range calls {
		kind, signs := signingCalls[fullName(calleeOf(c.call, info))]
		if !signs {
			continue
		}
		w.calls++
		w.settle(kind, w.readName(c.call.Args[0], c.in, info), w.at(c.call.Pos()), nil, c.in, info)
	}
	// A function a name enters may be called anywhere in its package, above
	// or below the call that names the domain, and through another such
	// function, so the calls are read until none gives a name it has not.
	given := map[*ast.CallExpr]map[*enteredName]bool{}
	for more := true; more; {
		more = false
		for _, c := range calls {
			callee := calleeOf(c.call, info)
			for _, e := range w.entered[fullName(callee)] {
				if given[c.call][e] {
					continue
				}
				if given[c.call] == nil {
					given[c.call] = map[*enteredName]bool{}
				}
				given[c.call][e] = true
				more = true
				e.given++
				name := w.give(e.name, c.call, callee, c.in, info)
				w.settle(e.kind, name, e.built, append(slices.Clip(e.via), w.at(c.call.Pos())), c.in, info)
			}
		}
	}
	for id, obj := range info.Uses {
		f, ok := obj.(*types.Func)
		if !ok || callees[id] {
			continue
		}
		name := fullName(f)
		if _, signs := signingCalls[name]; signs || len(w.entered[name]) > 0 {
			w.asValue = append(w.asValue, fmt.Sprintf("%s uses %s as a value, which may be called "+
				"with any name, so no gate holds its domains apart: call it", w.at(id.Pos()), name))
		}
	}
}

// settle records what a signing call was named, as read in the function in:
// built, when the walk read it whole; entered, when it is one of in's
// parameters or built from one, for in's callers to give; or unread.
func (w *channelWalk) settle(kind signingCall, name *channelName, built string, via []string,
	in *ast.FuncDecl, info *types.Info) {
	if unread := name.unreadPart(); unread != nil {
		named := *unread
		named.built = built
		w.unread = append(w.unread, &named)
		return
	}
	fn := declared(in, info)
	if !name.open() {
		w.built = append(w.built, builtChannel{kind: kind, name: name.spelled(), in: fullName(fn),
			built: built, via: via})
		return
	}
	switch {
	case fn.Signature().Recv() != nil && fn.Exported():
		w.unread = append(w.unread, &unreadName{at: built, in: fullName(fn),
			what: "a name " + fullName(fn) + " is handed, an exported method any package holding " +
				"its type may call"})
	case len(via) >= maxNameHops:
		w.unread = append(w.unread, &unreadName{at: built, in: fullName(fn),
			what: fmt.Sprintf("a name passed through more than %d calls", maxNameHops)})
	default:
		w.entered[fullName(fn)] = append(w.entered[fullName(fn)],
			&enteredName{kind: kind, name: name, built: built, via: via, in: fullName(fn)})
		if fn.Exported() {
			w.reaches[fn.Pkg().Path()] = true
		}
	}
}

// readName reads e, spelled in the function in, as a channel's name.
func (w *channelWalk) readName(e ast.Expr, in *ast.FuncDecl, info *types.Info) *channelName {
	if value := info.Types[e].Value; value != nil && value.Kind() == constant.String {
		return &channelName{op: constantName, value: constant.StringVal(value)}
	}
	switch e := e.(type) {
	case *ast.ParenExpr:
		return w.readName(e.X, in, info)
	case *ast.BinaryExpr:
		if e.Op == token.ADD {
			return &channelName{op: joinedName,
				parts: [2]*channelName{w.readName(e.X, in, info), w.readName(e.Y, in, info)}}
		}
	case *ast.Ident:
		if i, ok := parameter(e, in, info); ok {
			return &channelName{op: parameterName, param: i}
		}
	case *ast.SelectorExpr:
		if base, ok := ast.Unparen(e.X).(*ast.Ident); ok {
			if s := info.Selections[e]; s != nil && s.Kind() == types.FieldVal && len(s.Index()) == 1 {
				if i, ok := parameter(base, in, info); ok {
					return &channelName{op: parameterName, param: i, field: e.Sel.Name}
				}
			}
		}
	}
	return w.unreadAt(e, in, info, "")
}

// give reads n, a name in terms of callee's parameters, as the call gives it,
// spelled in the function in.
func (w *channelWalk) give(n *channelName, call *ast.CallExpr, callee *types.Func, in *ast.FuncDecl,
	info *types.Info) *channelName {
	switch n.op {
	case joinedName:
		return &channelName{op: joinedName, parts: [2]*channelName{
			w.give(n.parts[0], call, callee, in, info), w.give(n.parts[1], call, callee, in, info)}}
	case constantName, unreadOp:
		return n
	}
	arg := n.param
	// A method expression, (*T).m(x, …), takes its receiver first.
	if sel, ok := callExprFun(call).(*ast.SelectorExpr); ok {
		if s := info.Selections[sel]; s != nil && s.Kind() == types.MethodExpr {
			arg++
		}
	}
	signature := callee.Signature()
	if arg >= len(call.Args) || call.Ellipsis.IsValid() ||
		(signature.Variadic() && n.param == signature.Params().Len()-1) {
		return w.unreadAt(call, in, info, " gives its name among a variadic parameter's arguments")
	}
	given := call.Args[arg]
	if n.field == "" {
		return w.readName(given, in, info)
	}
	literal := compositeLiteral(given)
	if literal == nil {
		return w.unreadAt(given, in, info, " is not a composite literal, whose "+n.field+" the walk reads")
	}
	for _, element := range literal.Elts {
		if kv, ok := element.(*ast.KeyValueExpr); ok {
			if key, ok := kv.Key.(*ast.Ident); ok && key.Name == n.field {
				return w.readName(kv.Value, in, info)
			}
		}
	}
	return w.unreadAt(given, in, info, " leaves "+n.field+" at its zero")
}

// unreadAt is e, spelled in the function in, as a name the walk cannot read.
func (w *channelWalk) unreadAt(e ast.Expr, in *ast.FuncDecl, info *types.Info, why string) *channelName {
	return &channelName{op: unreadOp, unread: &unreadName{at: w.at(e.Pos()),
		in: fullName(declared(in, info)), what: types.ExprString(e) + why}}
}

// at is pos as a path under the module's root and a line.
func (w *channelWalk) at(pos token.Pos) string {
	position := w.fset.Position(pos)
	if rel, err := filepath.Rel(w.root, position.Filename); err == nil {
		position.Filename = filepath.ToSlash(rel)
	}
	return fmt.Sprintf("%s:%d", position.Filename, position.Line)
}

// parameter is the position among in's parameters of the one id names, and
// whether it names one whose value the function never changes: a parameter
// assigned to, its address taken, or a pointer, may not hold at the signing
// call what the caller gave.
func parameter(id *ast.Ident, in *ast.FuncDecl, info *types.Info) (int, bool) {
	v, ok := info.Uses[id].(*types.Var)
	if !ok || in == nil {
		return 0, false
	}
	i := 0
	for _, field := range in.Type.Params.List {
		if len(field.Names) == 0 {
			i++
			continue
		}
		for _, name := range field.Names {
			if info.Defs[name] == v {
				_, pointer := v.Type().Underlying().(*types.Pointer)
				return i, !pointer && !changed(in.Body, v, info)
			}
			i++
		}
	}
	return 0, false
}

// changed is whether body assigns to v, to a field or an element of it,
// takes its address, or calls a method of it with a pointer receiver, which
// takes the address for the call.
func changed(body *ast.BlockStmt, v *types.Var, info *types.Info) bool {
	if body == nil {
		return false
	}
	names := func(e ast.Expr) bool {
		for e != nil {
			switch x := ast.Unparen(e).(type) {
			case *ast.Ident:
				return info.Uses[x] == v || info.Defs[x] == v
			case *ast.SelectorExpr:
				e = x.X
			case *ast.IndexExpr:
				e = x.X
			default:
				return false
			}
		}
		return false
	}
	found := false
	ast.Inspect(body, func(n ast.Node) bool {
		switch n := n.(type) {
		case *ast.AssignStmt:
			found = found || slices.ContainsFunc(n.Lhs, names)
		case *ast.IncDecStmt:
			found = found || names(n.X)
		case *ast.UnaryExpr:
			found = found || (n.Op == token.AND && names(n.X))
		case *ast.RangeStmt:
			found = found || (n.Tok == token.ASSIGN && (names(n.Key) || names(n.Value)))
		case *ast.SelectorExpr:
			if s := info.Selections[n]; s != nil && s.Kind() == types.MethodVal && names(n.X) {
				if recv := s.Obj().(*types.Func).Signature().Recv(); recv != nil {
					_, pointer := recv.Type().(*types.Pointer)
					found = found || pointer
				}
			}
		}
		return !found
	})
	return found
}

// compositeLiteral is the composite literal e is, or the address of, or nil.
func compositeLiteral(e ast.Expr) *ast.CompositeLit {
	e = ast.Unparen(e)
	if u, ok := e.(*ast.UnaryExpr); ok && u.Op == token.AND {
		e = ast.Unparen(u.X)
	}
	literal, _ := e.(*ast.CompositeLit)
	return literal
}

// callExprFun is the function a call calls, less parentheses and type
// arguments.
func callExprFun(call *ast.CallExpr) ast.Expr {
	fun := ast.Unparen(call.Fun)
	switch f := fun.(type) {
	case *ast.IndexExpr:
		fun = ast.Unparen(f.X)
	case *ast.IndexListExpr:
		fun = ast.Unparen(f.X)
	}
	return fun
}

// calleeIdent is the identifier naming the function a call calls, or nil.
func calleeIdent(call *ast.CallExpr) *ast.Ident {
	switch f := callExprFun(call).(type) {
	case *ast.Ident:
		return f
	case *ast.SelectorExpr:
		return f.Sel
	}
	return nil
}

// calleeOf is the function a call calls, when it names one.
func calleeOf(call *ast.CallExpr, info *types.Info) *types.Func {
	id := calleeIdent(call)
	if id == nil {
		return nil
	}
	f, _ := info.Uses[id].(*types.Func)
	return f
}

// declared is the function in declares, or nil at package level.
func declared(in *ast.FuncDecl, info *types.Info) *types.Func {
	if in == nil {
		return nil
	}
	f, _ := info.Defs[in.Name].(*types.Func)
	return f
}

// fullName is f's full name less the module's path — "internal/signedask.New",
// "(*internal/engine.stateLog).signerFor" — or "" for none.
func fullName(f *types.Func) string {
	if f == nil {
		return ""
	}
	return strings.ReplaceAll(f.Origin().FullName(), modulePath+"/", "")
}
