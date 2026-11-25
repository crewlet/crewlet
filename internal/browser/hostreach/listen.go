package hostreach

import (
	"bufio"
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/crewlet/crewlet/internal/browser"
)

// The most a box's half dials, as reach.js caps them: addresses its hosts
// file, routing tables and the engine name, and proxy addresses, each asked
// six times for the engine's loopback ([proxyAsks]) and at most twice
// elsewhere. A box that names more says too_many and dials none.
const (
	maxAddresses      = 64
	maxProxyAddresses = 8
	// proxyAsks is a GET and a tunnel for each of reach.js's three
	// LOOPBACK_TARGETS.
	proxyAsks = 6
)

// The port's bounds.
const (
	// maxOpen is how many connections the port holds at once: an honest box
	// makes at most 112 ([maxAddresses], and [maxProxyAddresses] asked
	// [proxyAsks] times; its asks elsewhere come after and never reach the
	// engine) and the engine 2 of its own, so more is somebody else.
	maxOpen = 128
	// lineMax is the longest first line read of a connection: a proxy's GET of
	// a token in absolute form runs about 70 bytes.
	lineMax = 128
	// portTries is how many ports are tried for one free on both loopbacks.
	portTries = 16
)

// The answers a token that arrived is given, raw and to a proxy's GET.
const (
	answerRaw  = "ok\n"
	answerHTTP = "HTTP/1.1 200 OK\r\nContent-Length: 3\r\nConnection: close\r\n\r\nok\n"
)

// tokens is what one check minted: a token for each of [routes], and the
// engine's own, which only its drain sends.
type tokens struct {
	route map[browser.EnsureCause]string
	drain string
}

// port is the check's listeners, 127.0.0.1 and ::1 on one port, and what
// reached them.
type port struct {
	listeners []net.Listener
	number    int
	tokens    tokens

	mu      sync.Mutex
	arrived map[browser.EnsureCause]bool
	open    int
	// disturbance is what first reached the port that was not the check's,
	// "" while nothing has; strangers is closed with it.
	disturbance string
	strangers   chan struct{}
	// drained holds, for each listener, a channel closed once the engine's own
	// token was read there.
	drained []chan struct{}
	served  sync.WaitGroup
}

// open binds the check's port: 127.0.0.1 on a port the kernel picks, then ::1
// on the same one, a port taken there tried afresh up to [portTries] times. A
// host with no IPv6 loopback is listened on at 127.0.0.1 alone. Never a
// wildcard address.
func (c checker) open(t tokens) (*port, error) {
	for range portTries {
		v4, err := c.listen("tcp4", "127.0.0.1:0")
		if err != nil {
			return nil, err
		}
		addr, ok := v4.Addr().(*net.TCPAddr)
		if !ok {
			_ = v4.Close()
			return nil, fmt.Errorf("127.0.0.1's listener answers %v, which is no TCP address", v4.Addr())
		}
		listeners := []net.Listener{v4}
		v6, err := c.listen("tcp6", net.JoinHostPort("::1", strconv.Itoa(addr.Port)))
		switch {
		case err == nil:
			listeners = append(listeners, v6)
		case errors.Is(err, syscall.EADDRINUSE):
			_ = v4.Close()
			continue
		case !errors.Is(err, syscall.EADDRNOTAVAIL) && !errors.Is(err, syscall.EAFNOSUPPORT) &&
			!errors.Is(err, syscall.EPROTONOSUPPORT):
			_ = v4.Close()
			return nil, err
		}
		p := &port{listeners: listeners, number: addr.Port, tokens: t, arrived: map[browser.EnsureCause]bool{},
			strangers: make(chan struct{})}
		for range listeners {
			p.drained = append(p.drained, make(chan struct{}))
		}
		return p, nil
	}
	return nil, fmt.Errorf("no port was free on both 127.0.0.1 and ::1 in %d tries", portTries)
}

// serve accepts every connection on each listener until it is closed, each
// read on a goroutine of its own, cut once ctx ends.
func (p *port) serve(ctx context.Context) {
	for i, l := range p.listeners {
		p.served.Go(func() { p.accept(ctx, l, i) })
	}
}

// accept takes every connection l queues, in the order the kernel queued
// them. Past [maxOpen] at once it closes the new one rather than stop
// accepting, so a full queue never drops one of the box's unseen.
func (p *port) accept(ctx context.Context, l net.Listener, at int) {
	for {
		conn, err := l.Accept()
		if errors.Is(err, net.ErrClosed) {
			return
		}
		if err != nil {
			p.disturb("an accept failed: " + err.Error())
			return
		}
		p.mu.Lock()
		over := p.open >= maxOpen
		if !over {
			p.open++
		}
		p.mu.Unlock()
		if over {
			// MARKED BEFORE IT IS CLOSED, so whoever sees it closed finds the
			// port disturbed by it.
			p.disturb(fmt.Sprintf("more than %d connections at once", maxOpen))
			_ = conn.Close()
			continue
		}
		p.served.Go(func() { p.read(ctx, conn, at) })
	}
}

// read takes a connection's first line: one of the check's tokens, raw or in
// a proxy's GET, which is recorded before it is answered, or the engine's
// own, which drains its listener. Anything else, or no line before ctx ends,
// is somebody other than the box.
func (p *port) read(ctx context.Context, conn net.Conn, at int) {
	defer func() {
		_ = conn.Close()
		p.mu.Lock()
		p.open--
		p.mu.Unlock()
	}()
	stop := context.AfterFunc(ctx, func() { _ = conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	line, err := bufio.NewReaderSize(conn, lineMax+1).ReadSlice('\n')
	switch {
	case errors.Is(err, bufio.ErrBufferFull):
		p.disturb(fmt.Sprintf("a line longer than %d bytes", lineMax))
		return
	case errors.Is(err, os.ErrDeadlineExceeded):
		p.disturb("a connection held open without a line until the check's end")
		return
	case err != nil:
		p.disturb("a connection that closed before it sent a line")
		return
	}
	token, http := tokenOf(strings.TrimRight(string(line), "\r\n"))
	if same(token, p.tokens.drain) {
		p.markDrained(at)
		return
	}
	for _, route := range routes {
		if same(token, p.tokens.route[route]) {
			p.mu.Lock()
			p.arrived[route] = true
			p.mu.Unlock()
			answer := answerRaw
			if http {
				answer = answerHTTP
			}
			_, _ = conn.Write([]byte(answer))
			return
		}
	}
	p.disturb("a line that carried none of the check's tokens")
}

// tokenOf is the token a connection's first line carries: the line itself, or
// the path of a proxy's GET, in origin form or absolute, its slash dropped;
// http says it came in a GET, which is answered in HTTP.
func tokenOf(line string) (token string, http bool) {
	method, rest, ok := strings.Cut(line, " ")
	if !ok || method != "GET" {
		return line, false
	}
	target, version, ok := strings.Cut(rest, " ")
	if !ok || (version != "HTTP/1.1" && version != "HTTP/1.0") {
		return "", true
	}
	if scheme, authority, absolute := strings.Cut(target, "://"); absolute && strings.EqualFold(scheme, "http") {
		_, path, found := strings.Cut(authority, "/")
		if !found {
			return "", true
		}
		target = "/" + path
	}
	path, rooted := strings.CutPrefix(target, "/")
	if !rooted {
		return "", true
	}
	return path, true
}

// same compares a token in time that does not depend on where it differs.
func same(got, want string) bool {
	return want != "" && subtle.ConstantTimeCompare([]byte(got), []byte(want)) == 1
}

// markDrained records that the engine's own token was read on listener at.
func (p *port) markDrained(at int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-p.drained[at]:
	default:
		close(p.drained[at])
	}
}

// disturb records that what reached the port was not the check's, the first
// such kept.
func (p *port) disturb(what string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.disturbance == "" {
		p.disturbance = what
		close(p.strangers)
	}
}

// disturbed reports whether something other than the check reached the port.
func (p *port) disturbed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.disturbance != ""
}

// shut closes the listeners and waits for every connection's read, each cut
// at the check's end at the latest, then answers each route that arrived and
// what first disturbed the port, "" for nothing.
func (p *port) shut() (arrived map[browser.EnsureCause]bool, disturbance string) {
	for _, l := range p.listeners {
		_ = l.Close()
	}
	p.served.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.arrived, p.disturbance
}
