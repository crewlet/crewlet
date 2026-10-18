package main

import (
	"context"
	"log/slog"
	"net/http"
	"sync/atomic"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/config"
)

// nodeListeners is the sockets `crewlet run` binds before the boot: api.port,
// and api.public where this node serves one, each answering internal/api's
// boot surface until [serveAPI] hands it the node's own.
type nodeListeners struct {
	api    *apiListener
	public *apiListener
	boot   *api.Boot
}

// bindListeners binds the node's listeners on the addresses boot names and
// serves them the boot surface, or binds nothing when api.port is 0, which is
// the hard off switch for both.
//
// bridge says whether this node will have an agent-mode tool bridge, which is
// what decides whether a node without the ingress role binds api.public at all
// ([api.PublicListener]), and whether its boot surface tells a call to the
// bridge to come back ([api.NewBoot]). A failed bind of either closes what was bound and
// fails the boot, naming the address, before anything slow has run.
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
	// A SIGNAL BEFORE THE HANDOVER is a node stopping before it served, and
	// the boot surface answers a drain's answers from then on, as far as a
	// starting node can ([api.Boot]). The context is the signal context,
	// cancelled by the first SIGINT or SIGTERM.
	context.AfterFunc(ctx, surface.Stopping)
	log.InfoContext(ctx, "api_bound", "addr", addr, "node", nodeID,
		// Empty unless this node binds api.public.
		"public_addr", publicAddr,
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

// close closes both listeners, the public one first for [httpSurface.stop]'s
// reason. It is the close of a boot that failed before the handover; after it
// the sockets are the surface's, and [shutdown] closes them.
func (l *nodeListeners) close(ctx context.Context, log *slog.Logger) {
	if l == nil {
		return
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
