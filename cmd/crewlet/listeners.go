package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/config"
)

// nodeListeners is the sockets `crewlet run` binds before the boot: api.port,
// api.public where this node serves one and api.artifacts where it serves the
// artifact origin, each answering internal/api's boot surface until
// [serveAPI] hands it the node's own.
type nodeListeners struct {
	api       *apiListener
	public    *apiListener
	artifacts *apiListener
	boot      *api.Boot
}

// bindListeners binds the node's listeners on the addresses boot names and
// serves them the boot surface, or binds nothing when api.port is 0, which is
// the hard off switch for every one of them.
//
// bridge says whether this node will have an agent-mode tool bridge, which is
// what decides whether a node without the ingress role binds api.public at all
// ([api.PublicListener]), and whether its boot surface tells a call to the
// bridge to come back ([api.NewBoot]). The artifact origin is bound by
// [api.ArtifactsListener]'s rule: on an ingress node whose file sets a port for
// it. A failed bind of any of them closes what was bound and fails the boot,
// naming the address, before anything slow has run.
//
// THE NODE ID IS THE ONE THE ENGINE WILL RUN UNDER, resolved by the same rule,
// so /health names the node by the name its peers will know it by from its
// first answer.
func bindListeners(ctx context.Context, boot *config.Bootstrap, nodeID string, bridge bool,
	bind bindFunc, log *slog.Logger,
) (*nodeListeners, error) {
	if boot.API.Port == 0 {
		return nil, nil
	}
	surface, err := api.NewBoot(boot, nodeID, bridge)
	if err != nil {
		return nil, err
	}
	server, addr, err := listenAPI(ctx, bind, boot.API.Addr(), surface.Handler(), log)
	if err != nil {
		return nil, err
	}
	l := &nodeListeners{api: server, boot: surface}
	publicAddr := ""
	if api.PublicListener(boot, nodeID, bridge) {
		l.public, publicAddr, err = listenAPI(ctx, bind, boot.API.Public.Addr(), surface.Public(), log)
		if err != nil {
			l.close(context.WithoutCancel(ctx), log)
			return nil, err
		}
	}
	artifactsAddr := ""
	if api.ArtifactsListener(boot, nodeID) {
		l.artifacts, artifactsAddr, err = listenAPI(ctx, bind, boot.API.Artifacts.Addr(),
			surface.Artifacts(), log)
		if err != nil {
			l.close(context.WithoutCancel(ctx), log)
			return nil, err
		}
	}
	// A SIGNAL BEFORE THE HANDOVER is a node stopping before it served, and
	// the boot surface answers a drain's answers from then on, as far as a
	// starting node can ([api.Boot]). The context is the signal context,
	// cancelled by the first SIGINT or SIGTERM.
	context.AfterFunc(ctx, surface.Stopping)
	log.InfoContext(ctx, "api_bound", "addr", addr, "node", nodeID,
		// Empty unless this node binds api.public.
		"public_addr", publicAddr,
		// Empty unless this node binds api.artifacts.
		"artifacts_addr", artifactsAddr,
		"detail", "the listener answers from now on: /health 503 while this node "+
			"recovers what it holds and 200 once it has, /ready 503, and every "+
			"route the node will serve 503 starting until it serves "+
			"(api_listening, or api_probes_listening on a node without ingress)")
	return l, nil
}

// recovered tells the boot surface the engine has recovered what this node
// holds: [engine.Options.Recovered]. A node with no listeners has nobody to
// tell.
func (l *nodeListeners) recovered() {
	if l != nil {
		l.boot.Recovered()
	}
}

// close closes every listener bound, in [httpSurface.stop]'s order and for its
// reason: api.port, which carries the probes, last. It is the close of a boot
// that failed before the handover; after it the sockets are the surface's, and
// [shutdown] closes them.
func (l *nodeListeners) close(ctx context.Context, log *slog.Logger) {
	if l == nil {
		return
	}
	if l.artifacts != nil {
		shutdownListener(ctx, l.artifacts, "artifacts", log)
	}
	if l.public != nil {
		shutdownListener(ctx, l.public, "public", log)
	}
	shutdownListener(ctx, l.api, "api", log)
}

// handlerSlot is the handler one listener's server answers with: the boot
// surface from the bind, the node's own from the handover.
//
// AN ATOMIC POINTER, because the server reads it on every request from the
// goroutine serving that request while the handover writes it from the boot's:
// a plain field would be a data race on exactly the request that lands as the
// node starts serving. See [apiListener] for why the server itself cannot be
// the thing replaced.
type handlerSlot struct {
	current atomic.Pointer[slotted]
}

// slotted boxes a handler, since an atomic pointer holds a pointer to a type
// and an interface value is not one.
type slotted struct{ http.Handler }

func newHandlerSlot(h http.Handler) *handlerSlot {
	s := &handlerSlot{}
	s.store(h)
	return s
}

func (s *handlerSlot) store(h http.Handler) { s.current.Store(&slotted{Handler: h}) }

func (s *handlerSlot) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.current.Load().ServeHTTP(w, r)
}

// unusedConns is every connection of one listener that has not sent a
// request, closed as the listener's shutdown begins (#315).
//
// net/http's Shutdown closes an IDLE connection at once, but counts one that
// has sent nothing yet (StateNew) as active until it is five seconds old (Go
// issue 22682) — and [apiShutdownGrace] is five seconds, so a stop with one
// open spent its whole grace waiting for it and then logged
// api_shutdown_failed, the warning meant for a request that was stuck. Clients
// leave such connections routinely: one whose request finds no idle connection
// dials another, takes the connection the request before it frees if that
// comes first, and keeps the one it dialled idle, never used (a goroutine dump
// of a stop held so showed exactly that); a browser opens them ahead of its
// requests.
//
// So the stop closes them as it closes the idle ones: a connection that has
// sent nothing carries no request to finish. What it shares with an idle
// connection is the one instant a client may be writing its first request
// down it as the stop begins, which then meets a closed connection — as it
// would at the end of Go's five seconds, or on a node that has stopped.
type unusedConns struct {
	mu    sync.Mutex
	conns map[net.Conn]struct{}
	// closing is set once the shutdown began: a connection accepted as it
	// began, whose state arrives after, is closed as it arrives.
	closing bool
}

func newUnusedConns() *unusedConns { return &unusedConns{conns: map[net.Conn]struct{}{}} }

// track is the server's ConnState hook: a connection is unused until its
// state leaves StateNew.
func (u *unusedConns) track(conn net.Conn, state http.ConnState) {
	u.mu.Lock()
	defer u.mu.Unlock()
	switch {
	case state != http.StateNew:
		delete(u.conns, conn)
	case u.closing:
		_ = conn.Close()
	default:
		u.conns[conn] = struct{}{}
	}
}

// close closes every unused connection, and every one tracked after it.
func (u *unusedConns) close() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.closing = true
	for conn := range u.conns {
		_ = conn.Close()
	}
	clear(u.conns)
}

// count is how many unused connections are open.
func (u *unusedConns) count() int {
	u.mu.Lock()
	defer u.mu.Unlock()
	return len(u.conns)
}
