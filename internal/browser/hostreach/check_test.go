package hostreach

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/synctest"
	"time"

	"github.com/crewlet/crewlet/internal/browser"
)

// testKernel is the engine's kernel in a case that runs no real box.
const testKernel Kernel = "0f1e2d3c-4b5a-4968-8776-655443322110"

// fixtures reads the engine's routing tables from testdata, so a case runs on
// any system.
func fixtures(name string) ([]byte, error) {
	switch name {
	case routeFile:
		return os.ReadFile(filepath.Join("testdata", "route"))
	case route6File:
		return os.ReadFile(filepath.Join("testdata", "ipv6_route"))
	}
	return nil, fmt.Errorf("the case reads no %s", name)
}

// hostAddrs is the engine's own addresses in a case that lists none of this
// host's.
func hostAddrs() ([]net.Addr, error) {
	return []net.Addr{&net.IPNet{IP: net.IPv4(127, 0, 0, 1), Mask: net.CIDRMask(8, 32)},
		&net.IPNet{IP: net.IPv4(172, 17, 0, 1), Mask: net.CIDRMask(16, 32)}, &net.IPAddr{IP: net.IPv6loopback}}, nil
}

// realChecker listens through listen, real loopback listeners where it is
// nil, over the fixtures' routing tables and hostAddrs.
func realChecker(listen func(network, address string) (net.Listener, error)) checker {
	if listen == nil {
		listen = net.Listen
	}
	return checker{listen: listen, read: fixtures, addrs: hostAddrs, dial: (&net.Dialer{}).DialContext}
}

// handed is what the engine handed the box's half: the port, and each
// route's token by its route.
type handed struct {
	port   string
	tokens map[browser.EnsureCause]string
}

func handedOf(t *testing.T, env map[string]string) handed {
	t.Helper()
	h := handed{port: env[PortEnv], tokens: map[browser.EnsureCause]string{}}
	for pair := range strings.SplitSeq(env[TokensEnv], ",") {
		route, token, ok := strings.Cut(pair, ":")
		if !ok {
			t.Fatalf("%s holds %q, which is no route:token pair", TokensEnv, pair)
		}
		h.tokens[browser.EnsureCause(route)] = token
	}
	if h.port == "" || len(h.tokens) != len(routes) {
		t.Fatalf("the engine handed the box %v, want its port and a token for each of %q", env, routes)
	}
	return h
}

// send connects to the engine's port on 127.0.0.1, writes say, and answers
// what the engine wrote back before it closed the connection, or before ctx
// ended.
func send(ctx context.Context, t *testing.T, port, say string) string {
	t.Helper()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		t.Fatalf("connecting to the check's port: %v", err)
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	if _, err := io.WriteString(conn, say); err != nil {
		t.Fatalf("writing to the check's port: %v", err)
	}
	answer, _ := io.ReadAll(conn)
	return string(answer)
}

// knock connects to the engine's port on 127.0.0.1 and closes the connection
// unwritten.
func knock(ctx context.Context, t *testing.T, port string) {
	t.Helper()
	conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", port))
	if err != nil {
		t.Fatalf("connecting to the check's port: %v", err)
	}
	_ = conn.Close()
}

// boxSaying is a Run that does what does with what the engine handed it, then
// answers line, as a box's half prints it.
func boxSaying(t *testing.T, line string, does func(ctx context.Context, h handed)) func(context.Context, string,
	map[string]string, time.Duration) (string, int, error) {
	return func(ctx context.Context, command string, env map[string]string, _ time.Duration) (string, int, error) {
		if command != Command() {
			t.Errorf("the check ran %.80q, want its own command", command)
		}
		if does != nil {
			does(ctx, handedOf(t, env))
		}
		return line, 0, nil
	}
}

// ONLY A TOKEN THAT ARRIVES FINDS A ROUTE: each route's token, sent raw or
// in a proxy's GET of it, in origin form or absolute, finds that route and
// is answered; two routes that arrive name the first in their order; and a
// token with a byte more, another check's token, or a GET of another path
// finds none and withholds the browser as unchecked, never passes.
//
// Mutations, each red: compare tokens by prefix; map a token to another
// route; drop either GET form; let a connection with no token pass.
func TestOnlyATokenThatArrivesFindsARoute(t *testing.T) {
	t.Parallel()
	check := func(t *testing.T, does func(ctx context.Context, h handed)) Verdict {
		t.Helper()
		v, err := realChecker(nil).check(t.Context(), Request{
			Kernel: func() (Kernel, error) { return testKernel, nil },
			Run:    boxSaying(t, Answer(testKernel), does),
		})
		if err != nil {
			t.Fatalf("Check: %v", err)
		}
		return v
	}
	forms := map[string]func(h handed, token string) (say, answer string){
		"raw": func(_ handed, token string) (string, string) { return token + "\n", answerRaw },
		"a GET in origin form": func(_ handed, token string) (string, string) {
			return "GET /" + token + " HTTP/1.1\r\nHost: 127.0.0.1\r\n\r\n", answerHTTP
		},
		"a GET in absolute form": func(h handed, token string) (string, string) {
			return "GET http://localhost:" + h.port + "/" + token + " HTTP/1.0\r\n\r\n", answerHTTP
		},
	}
	for _, route := range routes {
		for form, spell := range forms {
			t.Run(string(route)+", "+form, func(t *testing.T) {
				t.Parallel()
				v := check(t, func(ctx context.Context, h handed) {
					say, want := spell(h, h.tokens[route])
					if got := send(ctx, t, h.port, say); got != want {
						t.Errorf("the engine answered %q, want %q", got, want)
					}
				})
				if v.Off || v.Reason != browser.ReasonHostNetwork || v.Cause != route || v.Detail != arrivedSaid[route] {
					t.Errorf("the verdict is %+v, want host_network %s", v, route)
				}
			})
		}
	}
	t.Run("two routes, the first in order", func(t *testing.T) {
		t.Parallel()
		v := check(t, func(ctx context.Context, h handed) {
			send(ctx, t, h.port, h.tokens[browser.CauseProxy]+"\n")
			send(ctx, t, h.port, h.tokens[browser.CauseGateway]+"\n")
		})
		if v.Cause != browser.CauseGateway {
			t.Errorf("the verdict is %+v, want host_network gateway, before proxy", v)
		}
	})
	stranger := map[string]func(h handed) string{
		"a token with a byte more": func(h handed) string { return h.tokens[browser.CauseLoopback] + "0\n" },
		"a token cut short":        func(h handed) string { return h.tokens[browser.CauseLoopback][:31] + "\n" },
		"another check's token":    func(handed) string { return strings.Repeat("ab", 16) + "\n" },
		"a GET of another path": func(h handed) string {
			return "GET /x" + h.tokens[browser.CauseLoopback] + " HTTP/1.1\r\n\r\n"
		},
	}
	for name, say := range stranger {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			v := check(t, func(ctx context.Context, h handed) {
				if got := send(ctx, t, h.port, say(h)); got != "" {
					t.Errorf("the engine answered %q to a stranger", got)
				}
			})
			if v.Off || v.Cause != browser.CauseUnchecked || !strings.Contains(v.Detail, "none of the check's tokens") {
				t.Errorf("the verdict is %+v, want unchecked for a connection with none of the check's tokens", v)
			}
		})
	}
}

// THE VERDICT TAKES THE KERNEL, THEN THE ROUTES, THEN THE LINE: what the
// engine decides before anything runs (no shared kernel another_kernel, its
// own kernel, tables, addresses or listener unread unchecked, the box never
// asked); a box on another kernel or with no boot id, even where a route
// arrived; one in its machine's network namespace, loopback, whatever arrived;
// a route, over every unchecked outcome; then a disturbed port, a check out of
// time, unrun, with no line or one off the grammar, and any word but checked
// and no_node, unchecked; no_node is node_missing; checked alone is off.
//
// Mutations, each red: judge arrivals before the kernel or the namespace, or a
// disturbed port before an arrival; drop the namespace's test; read any word
// but checked as off; run the box before the engine's kernel is known.
func TestTheVerdictTakesTheKernelThenTheRoutesThenTheLine(t *testing.T) {
	t.Parallel()
	const other = "9a8b7c6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d"
	line := func(kernel, w string) string {
		return linePrefix + " kernel=" + kernel + " net=" + answerNet + " " + w + "\n"
	}
	inMachine := func(kernel, w string) string {
		return linePrefix + " kernel=" + kernel + " net=" + initNetNS + " " + w + "\n"
	}
	clean := line(string(testKernel), "checked")
	arrived := func(routes ...browser.EnsureCause) map[browser.EnsureCause]bool {
		m := map[browser.EnsureCause]bool{}
		for _, r := range routes {
			m[r] = true
		}
		return m
	}
	noShared := &sharedKernelError{detail: "this host runs darwin, whose container runtimes all run their boxes " +
		"in a virtual machine: " + remedy}
	type want struct {
		reason browser.EnsureReason
		cause  browser.EnsureCause
		says   string
	}
	unchecked := func(says string) want { return want{browser.ReasonHostNetwork, browser.CauseUnchecked, says} }
	for name, c := range map[string]struct {
		o    outcome
		want want // zero: off
	}{
		"clean": {o: outcome{engine: testKernel, stdout: clean}},
		"clean after a login shell's words": {o: outcome{engine: testKernel,
			stdout: "Welcome to the box\n\n" + clean + "\n"}},
		"a box the engine could not ask": {o: outcome{unasked: "the box's handle cannot say which kernel its " +
			"runtime runs on"}, want: unchecked("cannot say which kernel")},
		"an engine on no kernel a box shares": {o: outcome{kernelErr: noShared},
			want: want{browser.ReasonHostNetwork, browser.CauseAnotherKernel, "this host runs darwin"}},
		"an engine whose boot id is unread": {o: outcome{kernelErr: errors.New("boot_id is unreadable")},
			want: unchecked("could not read its own kernel's boot id: boot_id is unreadable")},
		"an engine whose tables are unread": {o: outcome{engine: testKernel, tablesErr: errors.New("gone")},
			want: unchecked("could not read its own routing table")},
		"an engine whose addresses are unlisted": {o: outcome{engine: testKernel, addrsErr: errors.New("gone")},
			want: unchecked("could not list its own addresses")},
		"an engine that could not listen": {o: outcome{engine: testKernel, listenErr: errors.New("no ports")},
			want: unchecked("could not listen on its loopback for the check: no ports")},
		"a box on another kernel, a route arrived too": {o: outcome{engine: testKernel,
			stdout: line(other, "checked"), arrived: arrived(browser.CauseHostAlias)},
			want: want{browser.ReasonHostNetwork, browser.CauseAnotherKernel, "another kernel than this engine's"}},
		"a box on another kernel with no node": {o: outcome{engine: testKernel, stdout: line(other, "no_node")},
			want: want{browser.ReasonHostNetwork, browser.CauseAnotherKernel, "another kernel"}},
		"a box that could not read its boot id": {o: outcome{engine: testKernel,
			stdout: line("unreadable", "checked")},
			want: want{browser.ReasonHostNetwork, browser.CauseAnotherKernel, "could not read its kernel's boot id"}},
		"a box in its machine's network namespace": {o: outcome{engine: testKernel,
			stdout: inMachine(string(testKernel), "checked")},
			want: want{browser.ReasonHostNetwork, browser.CauseLoopback, "network namespace of the machine"}},
		"a box in its machine's network namespace, a gateway arrived too": {o: outcome{engine: testKernel,
			stdout: inMachine(string(testKernel), "no_node"), arrived: arrived(browser.CauseGateway)},
			want: want{browser.ReasonHostNetwork, browser.CauseLoopback, "network namespace of the machine"}},
		"a box on another kernel in that namespace": {o: outcome{engine: testKernel,
			stdout: inMachine(other, "checked")},
			want: want{browser.ReasonHostNetwork, browser.CauseAnotherKernel, "another kernel"}},
		"a box whose namespace is unread": {o: outcome{engine: testKernel,
			stdout: linePrefix + " kernel=" + string(testKernel) + " net=unreadable checked\n"}},
		"a route arrived, the port disturbed and the box silent": {o: outcome{engine: testKernel,
			arrived: arrived(browser.CauseRootlessGateway), disturbance: "a stranger", timedOut: true},
			want: want{browser.ReasonHostNetwork, browser.CauseRootlessGateway, "rootless runtime"}},
		"the port disturbed": {o: outcome{engine: testKernel, stdout: clean, disturbance: "a stranger"},
			want: unchecked("other than the box connected to the check's port on this engine's loopback (a stranger)")},
		"the port disturbed and the box silent": {o: outcome{engine: testKernel, disturbance: "a stranger",
			timedOut: true}, want: unchecked("(a stranger)")},
		"the engine's own connection failed": {o: outcome{engine: testKernel, stdout: clean,
			undrained: errors.New("refused")}, want: unchecked("its own listener to finish the check: refused")},
		"silent past the bound": {o: outcome{engine: testKernel, timedOut: true, exit: 124},
			want: unchecked("answered nothing within 30s")},
		"an exec that could not run": {o: outcome{engine: testKernel, runErr: errors.New("no such container")},
			want: unchecked("could not run: no such container")},
		"a non-zero exit with no line": {o: outcome{engine: testKernel, exit: 137, stdout: "\n"},
			want: unchecked("exited 137 and printed no line")},
		"no line at all": {o: outcome{engine: testKernel}, want: unchecked("printed no line")},
		"a line off the grammar": {o: outcome{engine: testKernel, stdout: line(string(testKernel), "maybe")},
			want: unchecked("a line this engine does not read")},
		"held": {o: outcome{engine: testKernel, stdout: line(string(testKernel), "held")},
			want: unchecked("held open without an answer for 5s")},
		"too many": {o: outcome{engine: testKernel, stdout: line(string(testKernel), "too_many")},
			want: unchecked("more than 64 addresses")},
		"unreadable tables": {o: outcome{engine: testKernel, stdout: line(string(testKernel), "unreadable")},
			want: unchecked("could not read its routing tables, its hosts file or its socket tables")},
		"forwarded": {o: outcome{engine: testKernel, stdout: line(string(testKernel), "forwarded")},
			want: unchecked("held by no process the check can see")},
		"a proxy down": {o: outcome{engine: testKernel, stdout: line(string(testKernel), "proxy_down")},
			want: unchecked("names no address the box could connect to")},
		"a proxy that refused alike": {o: outcome{engine: testKernel, stdout: line(string(testKernel), "proxy_refused")},
			want: unchecked("refused every request the check made of it")},
		"a line with no namespace": {o: outcome{engine: testKernel,
			stdout: linePrefix + " kernel=" + string(testKernel) + " checked\n"},
			want: unchecked("a line this engine does not read")},
		"no node": {o: outcome{engine: testKernel, stdout: line(string(testKernel), "no_node")},
			want: want{browser.ReasonNodeMissing, browser.CauseNotOnPath, "finds no node"}},
	} {
		got := decide(c.o)
		switch {
		case c.want == (want{}) && (!got.Off || got.Reason != "" || got.Cause != ""):
			t.Errorf("%s: the verdict is %+v, want the box off its host's loopback", name, got)
		case c.want == (want{}):
		case got.Off || got.Reason != c.want.reason || got.Cause != c.want.cause ||
			!strings.Contains(got.Detail, c.want.says):
			t.Errorf("%s: the verdict is %+v, want %s %s saying %q", name, got, c.want.reason, c.want.cause,
				c.want.says)
		case !slices.Contains(c.want.reason.Causes(), c.want.cause):
			t.Errorf("%s: %s takes no cause %s", name, c.want.reason, c.want.cause)
		}
	}

	ran := false
	run := func(context.Context, string, map[string]string, time.Duration) (string, int, error) {
		ran = true
		return Answer(testKernel), 0, nil
	}
	for name, c := range map[string]struct {
		kernel func() (Kernel, error)
		listen func(network, address string) (net.Listener, error)
		addrs  func() ([]net.Addr, error)
	}{
		"no kernel a box shares": {kernel: func() (Kernel, error) { return "", noShared }},
		"an unread kernel":       {kernel: func() (Kernel, error) { return "", errors.New("unreadable") }},
		"a refused listener": {kernel: func() (Kernel, error) { return testKernel, nil },
			listen: func(string, string) (net.Listener, error) { return nil, syscall.EACCES }},
		"unlisted addresses": {kernel: func() (Kernel, error) { return testKernel, nil },
			addrs: func() ([]net.Addr, error) { return nil, syscall.EACCES }},
	} {
		ran = false
		ch := realChecker(c.listen)
		if c.addrs != nil {
			ch.addrs = c.addrs
		}
		v, err := ch.check(t.Context(), Request{Kernel: c.kernel, Run: run})
		if err != nil || v.Off || ran {
			t.Errorf("%s: the verdict is %+v (%v), the box asked %t; want withheld with nothing run", name, v, err, ran)
		}
	}
}

// gatedListener is a real listener whose Accept yields nothing until its
// address is asked after the box's command returned, as the engine's drain
// asks it to connect: so a connection the box made is queued, unaccepted,
// when the command ends.
type gatedListener struct {
	net.Listener
	returned *atomic.Bool
	gate     chan struct{}
	opened   sync.Once
	closed   chan struct{}
	shut     sync.Once
}

func (l *gatedListener) Addr() net.Addr {
	if l.returned.Load() {
		l.opened.Do(func() { close(l.gate) })
	}
	return l.Listener.Addr()
}

func (l *gatedListener) Accept() (net.Conn, error) {
	select {
	case <-l.gate:
	case <-l.closed:
		return nil, net.ErrClosed
	}
	return l.Listener.Accept()
}

func (l *gatedListener) Close() error {
	l.shut.Do(func() { close(l.closed) })
	return l.Listener.Close()
}

// A CONNECTION QUEUED BEFORE THE CHECK ENDED IS READ BEFORE THE VERDICT: a
// box's token that reached the engine's port while the engine accepted
// nothing is read once the command ends, before the verdict, because the
// engine connects to its own listener last and waits for its own token.
//
// Mutation: close the listeners without the drain, and the token, never
// accepted, is lost: the box passes.
func TestAConnectionQueuedBeforeTheCheckEndedIsReadBeforeTheVerdict(t *testing.T) {
	t.Parallel()
	var returned atomic.Bool
	listen := func(network, address string) (net.Listener, error) {
		l, err := net.Listen(network, address)
		if err != nil {
			return nil, err
		}
		return &gatedListener{Listener: l, returned: &returned, gate: make(chan struct{}),
			closed: make(chan struct{})}, nil
	}
	v, err := realChecker(listen).check(t.Context(), Request{
		Kernel: func() (Kernel, error) { return testKernel, nil },
		Run: func(ctx context.Context, _ string, env map[string]string, _ time.Duration) (string, int, error) {
			defer returned.Store(true)
			h := handedOf(t, env)
			conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", h.port))
			if err != nil {
				t.Fatalf("connecting to the check's port: %v", err)
			}
			if _, err := io.WriteString(conn, h.tokens[browser.CauseLoopback]+"\n"); err != nil {
				t.Fatalf("writing the token: %v", err)
			}
			_ = conn.Close()
			return Answer(testKernel), 0, nil
		},
	})
	if err != nil || v.Cause != browser.CauseLoopback {
		t.Errorf("the verdict is %+v (%v), want host_network loopback from the queued token", v, err)
	}
}

// failingListener is a real listener whose first Accept fails.
type failingListener struct {
	net.Listener
	failed atomic.Bool
}

func (l *failingListener) Accept() (net.Conn, error) {
	if !l.failed.Swap(true) {
		return nil, errors.New("the double's accept failed")
	}
	return l.Listener.Accept()
}

// ANYTHING ELSE ON THE CHECK'S PORT WITHHOLDS AS UNCHECKED, where the box
// itself answers checked: a line with none of the check's tokens; a
// connection that closes before its line; more than 128 at once, the one
// past them closed and the port still accepting; and an accept that fails.
// Each detail says which.
//
// Mutations, each red: let a connection with no token pass; stop accepting
// at the cap, or close past it unmarked; let a failed accept pass.
func TestAnythingElseOnTheCheckPortWithholdsAsUnchecked(t *testing.T) {
	t.Parallel()
	for name, c := range map[string]struct {
		listen func(network, address string) (net.Listener, error)
		does   func(t *testing.T, ctx context.Context, h handed)
		says   string
	}{
		"a garbage line": {says: "a line that carried none of the check's tokens",
			does: func(t *testing.T, ctx context.Context, h handed) { send(ctx, t, h.port, "hello\n") }},
		"a connection closed before its line": {says: "a connection that closed before it sent a line",
			does: func(t *testing.T, ctx context.Context, h handed) { knock(ctx, t, h.port) }},
		"a line past its bound": {says: "a line longer than 128 bytes",
			does: func(t *testing.T, ctx context.Context, h handed) {
				send(ctx, t, h.port, strings.Repeat("a", lineMax+1)+"\n")
			}},
		"more than 128 at once": {says: "more than 128 connections at once",
			does: func(t *testing.T, ctx context.Context, h handed) {
				held := make([]net.Conn, 0, maxOpen)
				defer func() {
					for _, conn := range held {
						_ = conn.Close()
					}
				}()
				for range maxOpen {
					conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort("127.0.0.1", h.port))
					if err != nil {
						t.Fatalf("connecting to the check's port: %v", err)
					}
					held = append(held, conn)
				}
				// The one past them is closed by the engine, its read the
				// signal, while the rest stay open.
				if got := send(ctx, t, h.port, ""); got != "" {
					t.Errorf("the connection past the cap was answered %q", got)
				}
			}},
		"an accept that fails": {says: "an accept failed: the double's accept failed",
			listen: func(network, address string) (net.Listener, error) {
				l, err := net.Listen(network, address)
				if err != nil {
					return nil, err
				}
				return &failingListener{Listener: l}, nil
			}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			v, err := realChecker(c.listen).check(t.Context(), Request{
				Kernel: func() (Kernel, error) { return testKernel, nil },
				Run: boxSaying(t, Answer(testKernel), func(ctx context.Context, h handed) {
					if c.does != nil {
						c.does(t, ctx, h)
					}
				}),
			})
			if err != nil || v.Off || v.Cause != browser.CauseUnchecked || !strings.Contains(v.Detail, c.says) {
				t.Errorf("the verdict is %+v (%v), want unchecked saying %q", v, err, c.says)
			}
		})
	}
}

// THE ENGINE'S OWN CONNECTION THAT FAILS WITHHOLDS AS UNCHECKED: a drain that
// cannot connect to the engine's own listener, where the box answered
// checked, never reads as drained, and the box is withheld saying so.
//
// Mutations, each red: read a failed dial as drained; discard the drain's
// error in run.
func TestTheEnginesOwnConnectionThatFailsWithholdsAsUnchecked(t *testing.T) {
	t.Parallel()
	ch := realChecker(nil)
	ch.dial = func(context.Context, string, string) (net.Conn, error) { return nil, syscall.ECONNREFUSED }
	v, err := ch.check(t.Context(), Request{Kernel: func() (Kernel, error) { return testKernel, nil },
		Run: boxSaying(t, Answer(testKernel), nil)})
	if err != nil || v.Off || v.Cause != browser.CauseUnchecked ||
		!strings.Contains(v.Detail, "could not connect to its own listener") {
		t.Errorf("the verdict is %+v (%v), want unchecked, the engine's own listener unreached", v, err)
	}
}

// THE ENGINE HANDS THE BOX ITS BOUNDS, WHAT LIES PAST A ROOTLESS GATEWAY AND
// ITS OWN ADDRESSES: the box's half is handed the two dial bounds and its own
// deadline in milliseconds, slirp4netns's and RootlessKit's gateway then
// every default gateway of the engine's own tables (a link-local one too),
// and every address of the engine's interfaces, exactly.
//
// Mutations, each red: hand the box no engine gateways, or no 10.0.2.2 and
// fd00::2; drop the link-local one; hand no addresses of this host.
func TestTheEngineHandsTheBoxItsBoundsAndWhatLiesPastARootlessGateway(t *testing.T) {
	t.Parallel()
	var got map[string]string
	_, err := realChecker(nil).check(t.Context(), Request{Kernel: func() (Kernel, error) { return testKernel, nil },
		Run: func(_ context.Context, _ string, env map[string]string, _ time.Duration) (string, int, error) {
			got = env
			return Answer(testKernel), 0, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		RouteEnv: "1500", AnswerEnv: "5000", DeadlineEnv: "30000",
		BeyondEnv: "10.0.2.2,fd00::2,172.17.0.1,192.168.1.1,fe80::1,2001:db8::1",
		HostEnv:   "127.0.0.1,172.17.0.1,::1",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("the engine handed the box %s=%q, want %q", k, got[k], v)
		}
	}
}

// stillListener is a listener double whose Accept blocks until it is closed,
// at a fixed loopback address: no real socket, so a synctest bubble's clock
// moves while the check waits.
type stillListener struct {
	addr   *net.TCPAddr
	closed chan struct{}
	once   sync.Once
}

func (l *stillListener) Accept() (net.Conn, error) {
	<-l.closed
	return nil, net.ErrClosed
}

func (l *stillListener) Close() error {
	l.once.Do(func() { close(l.closed) })
	return nil
}

func (l *stillListener) Addr() net.Addr { return l.addr }

// A CHECK THAT ANSWERS NOTHING IS CUT AT ITS BOUND: a box whose half never
// returns is cut at exactly CheckTimeout, the context and the exec's own cap
// alike, and withheld as unchecked, saying the bound.
//
// Mutations, each red: run the box unbounded (the bubble deadlocks); bound
// it otherwise.
func TestACheckThatAnswersNothingIsCutAtItsBound(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		listen := func(network, _ string) (net.Listener, error) {
			if network == "tcp6" {
				return nil, syscall.EADDRNOTAVAIL
			}
			return &stillListener{addr: &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 40999},
				closed: make(chan struct{})}, nil
		}
		var took, capped time.Duration
		v, err := realChecker(listen).check(t.Context(), Request{
			Kernel: func() (Kernel, error) { return testKernel, nil },
			Run: func(ctx context.Context, _ string, _ map[string]string, timeout time.Duration) (string, int, error) {
				began := time.Now()
				<-ctx.Done()
				took, capped = time.Since(began), timeout
				return "", 124, nil
			},
		})
		if err != nil || v.Off || v.Cause != browser.CauseUnchecked || !strings.Contains(v.Detail, "within 30s") {
			t.Errorf("the verdict is %+v (%v), want unchecked, answered nothing within 30s", v, err)
		}
		if took != CheckTimeout || capped != CheckTimeout {
			t.Errorf("the box was cut after %v with a cap of %v, want exactly %v for both", took, capped, CheckTimeout)
		}
	})
}

// THE ENGINE LISTENS ON ITS LOOPBACK ALONE, ON ONE PORT: 127.0.0.1 on a port
// the kernel picks afresh for each check, then ::1 on the same one, never a
// wildcard; a port taken on ::1 is given up for another; and a host with no
// IPv6 loopback is listened on at 127.0.0.1 alone.
//
// Mutations, each red: bind a wildcard; bind each family a port of its own;
// fix the port.
func TestTheEngineListensOnItsLoopbackAloneOnOnePort(t *testing.T) {
	t.Parallel()
	// listened is one listen the check asked for, and where it listened.
	type listened struct {
		network, asked string
		at             *net.TCPAddr
	}
	recording := func(refuse func(network string, call int) error) (func(string, string) (net.Listener, error),
		func() []listened) {
		var mu sync.Mutex
		var calls []listened
		return func(network, address string) (net.Listener, error) {
				mu.Lock()
				defer mu.Unlock()
				calls = append(calls, listened{network: network, asked: address})
				if refuse != nil {
					if err := refuse(network, len(calls)); err != nil {
						return nil, err
					}
				}
				l, err := net.Listen(network, address)
				if err == nil {
					calls[len(calls)-1].at, _ = l.Addr().(*net.TCPAddr)
				}
				return l, err
			}, func() []listened {
				mu.Lock()
				defer mu.Unlock()
				return slices.Clone(calls)
			}
	}
	clean := Request{Kernel: func() (Kernel, error) { return testKernel, nil },
		Run: func(context.Context, string, map[string]string, time.Duration) (string, int, error) {
			return Answer(testKernel), 0, nil
		}}
	for i := range 2 {
		listen, calls := recording(nil)
		if v, err := realChecker(listen).check(t.Context(), clean); err != nil || !v.Off {
			t.Fatalf("a clean check answered %+v, %v", v, err)
		}
		got := calls()
		if len(got) == 0 || got[0].network != "tcp4" || got[0].asked != "127.0.0.1:0" || got[0].at == nil ||
			!got[0].at.IP.IsLoopback() {
			t.Fatalf("check %d listened %+v first, want 127.0.0.1 on a port the kernel picks", i, got)
		}
		if len(got) > 2 {
			t.Errorf("check %d listened %d times on a host whose ports were free", i, len(got))
		}
		if len(got) == 2 {
			v6 := got[1]
			if v6.network != "tcp6" || v6.asked != net.JoinHostPort("::1", fmt.Sprint(got[0].at.Port)) ||
				(v6.at != nil && !v6.at.IP.IsLoopback()) {
				t.Errorf("check %d listened %+v second, want ::1 on 127.0.0.1's port %d", i, v6, got[0].at.Port)
			}
		}
	}

	taken, takenCalls := recording(func(network string, call int) error {
		if network == "tcp6" && call == 2 {
			return &net.OpError{Op: "listen", Net: network, Err: os.NewSyscallError("bind", syscall.EADDRINUSE)}
		}
		return nil
	})
	if v, err := realChecker(taken).check(t.Context(), clean); err != nil || !v.Off {
		t.Fatalf("a check whose first port was taken on ::1 answered %+v, %v", v, err)
	}
	if got := takenCalls(); len(got) < 3 || got[2].network != "tcp4" || got[2].asked != "127.0.0.1:0" {
		t.Errorf("a port taken on ::1 led to %+v, want 127.0.0.1 asked afresh", got)
	}

	noV6, noV6Calls := recording(func(network string, _ int) error {
		if network == "tcp6" {
			return &net.OpError{Op: "listen", Net: network, Err: os.NewSyscallError("bind", syscall.EADDRNOTAVAIL)}
		}
		return nil
	})
	v, err := realChecker(noV6).check(t.Context(), Request{Kernel: clean.Kernel,
		Run: boxSaying(t, Answer(testKernel), func(ctx context.Context, h handed) {
			send(ctx, t, h.port, h.tokens[browser.CauseLoopback]+"\n")
		})})
	if err != nil || v.Cause != browser.CauseLoopback || len(noV6Calls()) != 2 {
		t.Errorf("a host with no IPv6 loopback answered %+v, %v after %+v, want 127.0.0.1 alone, reached", v, err,
			noV6Calls())
	}
}
