package mcp

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/redact"
)

// TESTING A SERVER: one connection of its own, walked stage by stage.
//
// A test brings up a connection exactly as the engine brings up the server it
// runs ([connect]): the same transport, for a local server the same launch,
// the same scrubbed environment ([mergedEnv]) and the same startup budget per
// stage. It then lists the server's tools and closes the connection. Nothing
// it opens is handed to a bridge, so a test changes nothing a seat's turn
// sees, and a server that is serving keeps serving beside it.
//
// # A test never calls a tool
//
// It makes three requests of the server: the handshake, the listing and the
// session's close. A tool call is a write somebody else's system carries out,
// and a person asking whether a server works has not asked for one; nothing
// here can make one, since [Test] holds a client whose only methods it uses
// are connect, list and stop.
//
// # Each stage is timed where it happens
//
// A remote server's connection reports each of its stages through the
// standard library's trace hooks ([httptrace.ClientTrace]): the name looked
// up, the socket connected, the TLS handshake. A test dials on a transport of
// its own, a clone of the shared one, so its first request opens a connection
// rather than reusing one a running server holds, and every stage is measured
// rather than skipped; the clone's pool is closed when the test ends. A local
// server's launch is timed from the moment its process starts.
//
// # The class is decided where the facts are
//
// A failure carries the class [connect] and [ClassOf] decide (failure.go), and
// the stage is the one whose facts produced it: a lookup that failed is the
// resolve stage's, a dial the connect stage's, a certificate the TLS stage's,
// an answer the server gave the discover stage's, a process that could not
// start or exited before it answered the launch stage's. A server that answers
// and lists no tool it can be offered under is [FailEmptyTools], a finding
// about a server that works rather than a failure to reach it.
//
// # The words carry no value
//
// Every sentence the report holds names the address with its credentials
// withheld ([redact.URL]), and the values the spec carries (its headers and
// its environment) are withheld wherever a server's error or its process's
// last words repeat them ([withheld]). The engine passes the sentences through
// the redaction pass and bounds them before they leave the node.

// TestStage is one stage of a test, in the order a test walks them.
type TestStage string

// The stages. A remote server walks address, resolve, connect, tls, discover
// and list; a local one launch, discover and list.
const (
	// StageAddress reads the configured address.
	StageAddress TestStage = "address"
	// StageResolve looks up the address's host name.
	StageResolve TestStage = "resolve"
	// StageConnect opens a connection to it.
	StageConnect TestStage = "connect"
	// StageTLS shakes hands over TLS, for an https address.
	StageTLS TestStage = "tls"
	// StageLaunch starts a local server's process.
	StageLaunch TestStage = "launch"
	// StageDiscover is the MCP handshake: the server says what it is.
	StageDiscover TestStage = "discover"
	// StageList lists the server's tools.
	StageList TestStage = "list"
)

// TestStages is every stage, for the gate holding a reader's copy.
var TestStages = []TestStage{StageAddress, StageResolve, StageConnect, StageTLS, StageLaunch,
	StageDiscover, StageList}

// Valid reports whether s is a stage this build walks.
func (s TestStage) Valid() bool { return slices.Contains(TestStages, s) }

// StagesOf is the stages a server of kind walks, in order.
func StagesOf(kind TransportKind) []TestStage {
	if kind == TransportHTTP {
		return []TestStage{StageAddress, StageResolve, StageConnect, StageTLS, StageDiscover, StageList}
	}
	return []TestStage{StageLaunch, StageDiscover, StageList}
}

// StepState is how one stage of a test went.
type StepState string

// The states.
const (
	// StepPassed is a stage that did what it is for.
	StepPassed StepState = "passed"
	// StepFailed is the stage the test stopped at.
	StepFailed StepState = "failed"
	// StepSkipped is a stage this address has no need of: no name to look
	// up for an address written as one, no TLS for plain http.
	StepSkipped StepState = "skipped"
	// StepNotReached is a stage after the one that failed.
	StepNotReached StepState = "not_reached"
)

// StepStates is every state, for the gate holding a reader's copy.
var StepStates = []StepState{StepPassed, StepFailed, StepSkipped, StepNotReached}

// Valid reports whether s is a state this build reports.
func (s StepState) Valid() bool { return slices.Contains(StepStates, s) }

// TestStep is one stage of a test: how it went, how long it took and what it
// found, in the engine's words, or the failure's sentence for the stage that
// failed.
type TestStep struct {
	Stage  TestStage
	State  StepState
	Took   time.Duration
	Detail string
}

// TestedTool is one tool a test's listing named, as the engine would offer
// it: under its name with the server's prefix, its hints with the operator's
// overrides applied, and the reason it would not be offered, if one.
//
// WORDS THE SERVER WROTE, its title and description: whoever shows them says
// whose they are.
type TestedTool struct {
	// Name is the name the tool would be offered under; Raw the server's.
	Name string
	Raw  string

	Title       string
	Description string
	Annotations Annotations

	// Refused is why the tool would not be offered under Name, "" when it
	// would.
	Refused NameRefusal
}

// TestReport is what one test of a server found.
type TestReport struct {
	// Steps are the stages of [StagesOf] the server's transport, in order,
	// each with its state.
	Steps []TestStep
	// Failure is why the test stopped, or [FailEmptyTools] for a server that
	// answered and lists no tool it can be offered under; nil for a test that
	// found the server serving tools. Its sentence carries no value
	// ([withheld]).
	Failure *Failure
	// Identity is what the server said of itself in the handshake: zero
	// where it never answered one.
	Identity Identity
	// Tools are the tools it listed, as offered, in its order; the ones the
	// spec excludes are left out.
	Tools []TestedTool
	// Took is the whole test, the close included.
	Took time.Duration

	// challenge is the WWW-Authenticate header of the remote server's last
	// answer that was no success, "" where it gave none: what an address
	// check reads a sign-in's metadata address from ([CheckAddress]).
	challenge string
}

// Serving reports whether the test found the server answering and listing:
// every stage passed or was skipped. A server listing no tool it can be
// offered under is serving, and its report says so ([FailEmptyTools]).
func (r TestReport) Serving() bool {
	return r.Failure == nil || r.Failure.Class == FailEmptyTools
}

// testStopBudget bounds closing a test's connection: a local server's whole
// shutdown ladder (three rungs of [shutdownGrace]) and the reap of its tree
// behind it, with time to spare. REASONED: a server that has not gone by then
// was killed by the ladder's last rung and is reaped by its group.
const testStopBudget = 10 * time.Second

// Test brings up a connection to spec's server of its own, walks its stages,
// lists its tools and closes the connection, never calling one of them: see
// the file's comment. Each stage is bounded by the spec's startup budget, and
// the whole test by ctx.
//
// The report is the whole answer, a failure included: Test has no error.
func Test(ctx context.Context, spec Spec, log *slog.Logger) TestReport {
	if log == nil {
		log = logging.Get("mcp.test")
	}
	began := time.Now()
	w := newWalk(spec)
	w.run(ctx, log)
	w.report.Took = time.Since(began)
	return w.report
}

// Unstarted is the report of a test that stopped at its first stage (the
// address, or a local server's launch) for a reason the caller decided before
// anything was dialled or launched: an address the configuration's rule
// refuses, or a value it is started with that does not resolve, worded by
// sentence.
func Unstarted(spec Spec, sentence string) TestReport {
	w := newWalk(spec)
	w.fail(w.report.Steps[0].Stage, 0, "", errors.New(sentence))
	return w.report
}

// walk is one test in progress: the report it builds, stage by stage.
type walk struct {
	spec   Spec
	report TestReport
	at     int // the index of the next step to settle
}

func newWalk(spec Spec) *walk {
	stages := StagesOf(spec.kind())
	steps := make([]TestStep, len(stages))
	for i, s := range stages {
		steps[i] = TestStep{Stage: s, State: StepNotReached}
	}
	return &walk{spec: spec, report: TestReport{Steps: steps, Tools: []TestedTool{}}}
}

// settle records the next stage's state, which must be stage.
func (w *walk) settle(stage TestStage, state StepState, took time.Duration, detail string) {
	for w.at < len(w.report.Steps) && w.report.Steps[w.at].Stage != stage {
		w.at++
	}
	if w.at == len(w.report.Steps) {
		return
	}
	w.report.Steps[w.at] = TestStep{Stage: stage, State: state, Took: took, Detail: detail}
	w.at++
}

func (w *walk) pass(stage TestStage, took time.Duration, detail string) {
	w.settle(stage, StepPassed, took, detail)
}

func (w *walk) skip(stage TestStage, detail string) { w.settle(stage, StepSkipped, 0, detail) }

// fail stops the test at stage, with class (empty where none fits) and the
// error's sentence, every value the spec carries withheld from it.
func (w *walk) fail(stage TestStage, took time.Duration, class FailureClass, err error) {
	said := w.said(err.Error())
	w.settle(stage, StepFailed, took, said)
	w.report.Failure = &Failure{Class: class, Said: said, At: time.Now().UTC()}
}

// said is a sentence as a report carries it: the address written with its
// credentials withheld, and every value of the spec withheld.
func (w *walk) said(s string) string {
	if w.spec.URL != "" {
		s = strings.ReplaceAll(s, w.spec.URL, redact.URL(w.spec.URL))
	}
	return withheld(s, w.spec)
}

// minWithheldValue is the shortest value [withheld] looks for. REASONED: a
// credential a vendor issues is far longer, and a shorter value (a flag's
// "true", a port) withheld wherever it appears would garble the sentence
// around it while protecting nothing.
const minWithheldValue = 8

// withheld is s with every header and environment value spec carries
// replaced: a server's error, or its process's last words, can repeat the
// credential it was started with, and a test's report is read by people and
// models the credential was never shown to.
func withheld(s string, spec Spec) string {
	values := make([]string, 0, len(spec.Headers)+len(spec.Env))
	for _, v := range spec.Headers {
		values = append(values, v)
	}
	for _, v := range spec.Env {
		values = append(values, v)
	}
	// LONGEST FIRST, so a value containing another is withheld whole.
	slices.SortFunc(values, func(a, b string) int { return len(b) - len(a) })
	for _, v := range values {
		if len(v) >= minWithheldValue {
			s = strings.ReplaceAll(s, v, "[withheld]")
		}
	}
	return s
}

func (w *walk) run(ctx context.Context, log *slog.Logger) {
	if err := w.spec.validate(); err != nil {
		w.fail(w.report.Steps[0].Stage, 0, "", err)
		return
	}
	if w.spec.kind() == TransportHTTP {
		w.runRemote(ctx, log)
		return
	}
	w.runLocal(ctx, log)
}

// runRemote walks a remote server: its address read, then one connection on a
// transport of the test's own whose stages the trace reports.
func (w *walk) runRemote(ctx context.Context, log *slog.Logger) {
	u, err := url.Parse(w.spec.URL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		w.fail(StageAddress, 0, "", fmt.Errorf("the address %s is not an http or https address",
			redact.URL(w.spec.URL)))
		return
	}
	w.pass(StageAddress, 0, redact.URL(w.spec.URL))

	base := httpx.Customized(0).Transport
	defer closeIdle(base)
	clock := &stageClock{}
	began := time.Now()
	c, err := connectWith(ctx, w.spec, log, dialing{base: base, trace: clock.trace()})
	handshook := time.Now()
	seen := clock.read()
	if !w.remoteStages(u, seen, began, err) {
		w.close(ctx, c)
		return
	}
	discoverFrom := began
	if !seen.gotConn.IsZero() {
		discoverFrom = seen.gotConn
	}
	if err != nil {
		w.fail(StageDiscover, handshook.Sub(discoverFrom), ClassOf(err), err)
		w.close(ctx, c)
		return
	}
	w.pass(StageDiscover, handshook.Sub(discoverFrom), identityDetail(c.identity))
	w.report.Identity = c.identity
	w.list(ctx, c)
	w.close(ctx, c)
}

// keepChallenge keeps the challenge of the remote server's last answer that
// was no success.
func (w *walk) keepChallenge(c *client) {
	if c == nil || c.ident == nil {
		return
	}
	if answered := c.ident.failed.Load(); answered != nil {
		w.report.challenge = answered.challenge
	}
}

// remoteStages settles resolve, connect and tls from what the trace saw, and
// reports whether the test goes on to the handshake: false where one of them
// failed.
func (w *walk) remoteStages(u *url.URL, seen stageTimes, began time.Time, err error) bool {
	host := u.Hostname()
	switch {
	case seen.dnsErr != nil:
		w.fail(StageResolve, seen.dnsDone.Sub(seen.dnsStart), FailDNS, seen.dnsErr)
		return false
	case !seen.dnsStart.IsZero():
		w.pass(StageResolve, seen.dnsDone.Sub(seen.dnsStart), resolveDetail(host, seen.dnsAddrs))
	case isAddress(host):
		w.skip(StageResolve, host+" is an address, with no name to look up")
	case seen.connStart.IsZero() && err != nil:
		// NOTHING WAS LOOKED UP OR DIALLED: the request failed before it
		// reached the network (a proxy this process could not use, a
		// transport that refused it). Its class is the error's own.
		w.fail(StageResolve, time.Since(began), ClassOf(err), err)
		return false
	default:
		// A NAME A PROXY RESOLVES: the trace sees the proxy's lookup, or
		// none where the proxy is written as an address.
		w.skip(StageResolve, "the proxy this process sends through looks the name up")
	}
	switch {
	case seen.connErr != nil:
		class := ClassOf(seen.connErr)
		if class == "" {
			class = FailUnreachable
		}
		w.fail(StageConnect, seen.connDone.Sub(seen.connStart), class, seen.connErr)
		return false
	case !seen.connStart.IsZero():
		w.pass(StageConnect, seen.connDone.Sub(seen.connStart), "connected to "+seen.connAddr)
	case err != nil:
		class := ClassOf(err)
		if class == "" {
			class = FailUnreachable
		}
		w.fail(StageConnect, time.Since(began), class, err)
		return false
	default:
		w.skip(StageConnect, "no connection of the test's own was seen opening")
	}
	switch {
	case !strings.EqualFold(u.Scheme, "https"):
		w.skip(StageTLS, "plain http, with no TLS")
	case seen.tlsErr != nil:
		w.fail(StageTLS, seen.tlsDone.Sub(seen.tlsStart), FailTLS, seen.tlsErr)
		return false
	case !seen.tlsStart.IsZero():
		w.pass(StageTLS, seen.tlsDone.Sub(seen.tlsStart), tlsDetail(seen.tlsState))
	case err != nil && ClassOf(err) == FailTLS:
		w.fail(StageTLS, time.Since(began), FailTLS, err)
		return false
	default:
		w.skip(StageTLS, "no TLS handshake of the test's own was seen")
	}
	return true
}

// runLocal walks a local server: its process launched, then the handshake.
func (w *walk) runLocal(ctx context.Context, log *slog.Logger) {
	began := time.Now()
	c, err := connectWith(ctx, w.spec, log, dialing{})
	handshook := time.Now()
	if c == nil || c.child == nil {
		// THE PIPE FOR ITS LAST WORDS COULD NOT BE MADE: nothing started.
		w.fail(StageLaunch, handshook.Sub(began), ClassOf(err), err)
		return
	}
	launched := began
	if at := c.child.launched.Load(); at != 0 {
		launched = time.Unix(0, at)
	}
	switch {
	case c.child.startErr.Load() != nil:
		w.fail(StageLaunch, handshook.Sub(began), ClassOf(err), err)
		w.close(ctx, c)
		return
	case err != nil && ClassOf(err) == FailExited:
		// EXITED BEFORE IT ANSWERED: its own last words say why, which the
		// error does not carry.
		w.fail(StageLaunch, handshook.Sub(began), FailExited, withLastWords(err, c.stderrTail()))
		w.close(ctx, c)
		return
	}
	w.pass(StageLaunch, launched.Sub(began), "the process started")
	if err != nil {
		w.fail(StageDiscover, handshook.Sub(launched), ClassOf(err), err)
		w.close(ctx, c)
		return
	}
	w.pass(StageDiscover, handshook.Sub(launched), identityDetail(c.identity))
	w.report.Identity = c.identity
	w.list(ctx, c)
	w.close(ctx, c)
}

// list lists the tools of a connected server and settles the list stage.
func (w *walk) list(ctx context.Context, c *client) {
	began := time.Now()
	defs, err := c.listTools(ctx)
	took := time.Since(began)
	if err != nil {
		// CLASSED AS A START IS: by the server's last answer that was no
		// success, or its process's exit of its own, before the error's
		// own types.
		w.fail(StageList, took, c.startClass(err), err)
		return
	}
	offered, refused, left := 0, 0, 0
	for _, def := range defs {
		if w.spec.Levels.Blocks(def.Name) {
			left++
			continue
		}
		t := testedTool(def, w.spec)
		// THE SERVER'S WORDS CARRY NO VALUE EITHER: a server can repeat the
		// credential it was started with in a tool's text.
		t.Title, t.Description = withheld(t.Title, w.spec), withheld(t.Description, w.spec)
		if t.Refused != "" {
			refused++
		} else {
			offered++
		}
		w.report.Tools = append(w.report.Tools, t)
	}
	w.pass(StageList, took, listDetail(len(defs), offered, refused, left))
	if offered == 0 {
		w.report.Failure = &Failure{Class: FailEmptyTools, Said: emptySaid(len(defs), refused, left),
			At: time.Now().UTC()}
	}
}

// testedTool is def as the engine would offer it under spec: the rules of
// [server.toolsFor] and [newTool], read rather than filed.
func testedTool(def toolDef, spec Spec) TestedTool {
	name := spec.ToolPrefix + def.Name
	ann := def.Annotations
	if override, ok := spec.AnnotationOverrides[def.Name]; ok {
		ann = ann.Merge(override)
	} else if override, ok := spec.AnnotationOverrides[name]; ok {
		ann = ann.Merge(override)
	}
	return TestedTool{Name: name, Raw: def.Name, Title: def.Title, Description: def.Description,
		Annotations: ann, Refused: nameRefusal(name)}
}

// close ends a test's connection, within [testStopBudget] of a context the
// test's own cancellation does not cut short: a test whose caller went away
// still ends the process it started.
func (w *walk) close(ctx context.Context, c *client) {
	if c == nil {
		return
	}
	// BEFORE THE CLOSE, whose own request (a session's end) is answered
	// too, and would replace the answer the test is about.
	w.keepChallenge(c)
	stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), testStopBudget)
	defer cancel()
	_, _ = c.close(stopCtx)
}

// closeIdle releases a test transport's pool.
func closeIdle(rt http.RoundTripper) {
	if t, ok := rt.(interface{ CloseIdleConnections() }); ok {
		t.CloseIdleConnections()
	}
}

// withLastWords is err with the last line its process wrote, which is where a
// server that exits says why.
func withLastWords(err error, tail []string) error {
	for i := len(tail) - 1; i >= 0; i-- {
		if line := strings.TrimSpace(tail[i]); line != "" {
			return fmt.Errorf("%w; its last words: %s", err, line)
		}
	}
	return err
}

// isAddress reports whether host is written as an IP address.
func isAddress(host string) bool {
	_, err := netip.ParseAddr(host)
	return err == nil
}

func resolveDetail(host string, addrs []string) string {
	switch len(addrs) {
	case 0:
		return host + " resolved"
	case 1:
		return host + " resolved to " + addrs[0]
	}
	return host + " resolved to " + addrs[0] + " and " + strconv.Itoa(len(addrs)-1) + " more"
}

func tlsDetail(state tls.ConnectionState) string {
	detail := tls.VersionName(state.Version)
	if len(state.PeerCertificates) > 0 {
		detail += ", a certificate valid until " + state.PeerCertificates[0].NotAfter.UTC().Format(time.DateOnly)
	}
	return detail
}

func identityDetail(id Identity) string {
	name := id.Name
	if name == "" {
		name = "the server, which gave no name"
	}
	if id.Version != "" {
		name += " " + id.Version
	}
	if id.Protocol != "" {
		name += ", protocol " + id.Protocol
	}
	return name
}

func listDetail(listed, offered, refused, excluded int) string {
	detail := toolCount(listed)
	var notes []string
	if refused > 0 {
		notes = append(notes, strconv.Itoa(refused)+" not offered under their names")
	}
	if excluded > 0 {
		notes = append(notes, strconv.Itoa(excluded)+" blocked by the server's tool levels")
	}
	if len(notes) > 0 && offered > 0 {
		detail += ", " + strings.Join(notes, ", ")
	} else if len(notes) > 0 {
		detail += ": " + strings.Join(notes, ", ")
	}
	return detail
}

func emptySaid(listed, refused, excluded int) string {
	switch {
	case listed == 0:
		return "the server answered and lists no tool"
	case refused > 0 && excluded > 0:
		return fmt.Sprintf("the server lists %s: %d cannot be offered under their names and the "+
			"server's tool levels block the rest", toolCount(listed), refused)
	case refused > 0:
		return fmt.Sprintf("the server lists %s, and none can be offered under its name", toolCount(listed))
	}
	return fmt.Sprintf("the server lists %s, and the server's tool levels block every one", toolCount(listed))
}

// toolCount is n tools, in words.
func toolCount(n int) string {
	if n == 1 {
		return "1 tool"
	}
	return strconv.Itoa(n) + " tools"
}

// stageClock is what the trace saw of a test's connection: the FIRST of each
// event, which is the connection the handshake opened.
type stageClock struct {
	mu   sync.Mutex
	seen stageTimes
}

// stageTimes are the instants and outcomes of one connection's stages.
type stageTimes struct {
	dnsStart, dnsDone time.Time
	dnsAddrs          []string
	dnsErr            error

	connStart, connDone time.Time
	connAddr            string
	connErr             error

	tlsStart, tlsDone time.Time
	tlsState          tls.ConnectionState
	tlsErr            error

	gotConn time.Time
}

func (c *stageClock) read() stageTimes {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.seen
}

// trace is the hooks the test's requests report to. A connection that failed
// to connect to one of a name's addresses and then reached another is a
// connection that connected: a failed attempt is kept only while no attempt
// has succeeded.
func (c *stageClock) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		DNSStart: func(httptrace.DNSStartInfo) {
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.seen.dnsStart.IsZero() {
				c.seen.dnsStart = time.Now()
			}
		},
		DNSDone: func(info httptrace.DNSDoneInfo) {
			c.mu.Lock()
			defer c.mu.Unlock()
			if !c.seen.dnsDone.IsZero() {
				return
			}
			c.seen.dnsDone, c.seen.dnsErr = time.Now(), info.Err
			for _, a := range info.Addrs {
				c.seen.dnsAddrs = append(c.seen.dnsAddrs, a.String())
			}
		},
		ConnectStart: func(string, string) {
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.seen.connStart.IsZero() {
				c.seen.connStart = time.Now()
			}
		},
		ConnectDone: func(_, addr string, err error) {
			c.mu.Lock()
			defer c.mu.Unlock()
			if !c.seen.connDone.IsZero() && c.seen.connErr == nil {
				return
			}
			c.seen.connDone, c.seen.connAddr, c.seen.connErr = time.Now(), addr, err
		},
		TLSHandshakeStart: func() {
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.seen.tlsStart.IsZero() {
				c.seen.tlsStart = time.Now()
			}
		},
		TLSHandshakeDone: func(state tls.ConnectionState, err error) {
			c.mu.Lock()
			defer c.mu.Unlock()
			if !c.seen.tlsDone.IsZero() {
				return
			}
			c.seen.tlsDone, c.seen.tlsState, c.seen.tlsErr = time.Now(), state, err
		},
		GotConn: func(httptrace.GotConnInfo) {
			c.mu.Lock()
			defer c.mu.Unlock()
			if c.seen.gotConn.IsZero() {
				c.seen.gotConn = time.Now()
			}
		},
	}
}
