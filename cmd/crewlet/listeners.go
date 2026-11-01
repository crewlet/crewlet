package main

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strconv"
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
// api.public is bound by [api.PublicListener]'s rule and the artifact origin by
// [api.ArtifactsListener]'s: each on an ingress node whose file sets a port
// for it. A failed bind of any of them closes what was bound and fails the boot,
// naming the address, before anything slow has run.
//
// THE NODE ID IS THE ONE THE ENGINE WILL RUN UNDER, resolved by the same rule,
// so /health names the node by the name its peers will know it by from its
// first answer.
func bindListeners(ctx context.Context, boot *config.Bootstrap, nodeID string,
	bind bindFunc, log *slog.Logger,
) (*nodeListeners, error) {
	if boot.API.Port == 0 {
		return nil, nil
	}
	surface, err := api.NewBoot(boot, nodeID)
	if err != nil {
		return nil, err
	}
	server, addr, err := listenAPI(ctx, bind, boot.API.Addr(), surface.Handler(), log)
	if err != nil {
		return nil, err
	}
	l := &nodeListeners{api: server, boot: surface}
	publicAddr := ""
	if api.PublicListener(boot, nodeID) {
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

// boxHostURL is where a box on this node's own host — the `direct` cell —
// reaches the two routes a box dials, the tool bridge and the telemetry
// receiver: this node's own listener that serves them, as this host dials it
// ([engine.Options.BoxHostURL]). Empty where the node runs no seat, and so
// launches no box, or binds no listener a box of its own reaches (api.port 0):
// such a node's boxes dial CREWLET_MCP_BRIDGE_URL like any other.
//
// THE LISTENER THEY ARE SERVED ON. An ingress node serves them to anyone:
// on api.public where it binds one — they are public routes — and on api.port
// otherwise, dialled at the address it bound ([loopbackURL]). A node without
// the role serves them on api.port beside its probes, to this host's loopback
// ALONE (internal/api's Probes): a seat pool with no route in from the
// network, which is what such a node is for, still runs every seat a direct
// box serves. So its listener must be one loopback reaches — bound on every
// interface or on loopback — and one bound to a single address that is not
// loopback derives none: a call from this host to that address arrives from
// it, not from loopback, and would be refused as from off the host.
func (l *nodeListeners) boxHostURL(boot *config.Bootstrap, nodeID string) string {
	profile := boot.Profile(nodeID)
	if l == nil || !profile.RunsSeats() {
		return ""
	}
	if !profile.RunsIngress() {
		if !reachedOnLoopback(l.api.socket.Addr()) {
			return ""
		}
		return loopbackURL(l.api.socket.Addr())
	}
	served := l.api
	if l.public != nil {
		served = l.public
	}
	return loopbackURL(served.socket.Addr())
}

// reachedOnLoopback reports whether a process on this host reaches a TCP
// listener over the loopback interface: one bound to every interface, or to a
// loopback address.
func reachedOnLoopback(addr net.Addr) bool {
	tcp, ok := addr.(*net.TCPAddr)
	if !ok {
		return false
	}
	return tcp.IP == nil || tcp.IP.IsUnspecified() || tcp.IP.IsLoopback()
}

// loopbackURL is the URL a process on this host dials a TCP listener at: the
// address it bound where that is one address, and loopback where it took
// every interface. Plain http, because a listener of this process serves no
// TLS of its own — that is terminated in front of it — and a box on the host
// dials it directly. Empty for an address that is not TCP.
func loopbackURL(addr net.Addr) string {
	tcp, ok := addr.(*net.TCPAddr)
	if !ok {
		return ""
	}
	ip := tcp.IP
	if ip == nil || ip.IsUnspecified() {
		// EVERY INTERFACE, which a Go listener on ":port" is dual-stack:
		// IPv4 loopback reaches it whichever family it was written in.
		ip = net.IPv4(127, 0, 0, 1)
	}
	return "http://" + net.JoinHostPort(ip.String(), strconv.Itoa(tcp.Port))
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
