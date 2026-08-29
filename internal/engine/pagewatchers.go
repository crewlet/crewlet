package engine

import (
	"context"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/confluence"
	"github.com/crewlet/crewlet/internal/coord"
)

// The engine's own page-subscription list, over the coordination store.
//
// # Why the engine keeps one at all
//
// A wiki page event names only who edited it, so without a subscription list
// every change that is not an explicit mention falls all the way through to
// the space lead — including the follow-up to a page a seat edited five
// minutes ago, and including the second event on a page a lead deliberately
// delegated by mentioning somebody. The lead-fallback prompt tells a lead to
// delegate by mention precisely so the follow-ups stop coming to them, and
// without this that loop never closes.
//
// # Why not Confluence's own watcher list
//
// See the rationale in internal/confluence's package doc. In short: it is
// mostly people, it costs a call per event, and a per-role token frequently
// cannot read it — so the answer would be "nobody" on the deployments that
// need it most.
//
// # Why the completion Ledger and not a bucket of its own
//
// The question is set membership over a scope, tested against a caller-known
// candidate list — which is exactly [coord.Ledger], down to the failure
// polarity. Adding a second primitive with the same shape would mean two
// retention rules, two backends' worth of conformance cases, and eventually
// a disagreement between them.

// pageWatchers subscribes seats to the pages they touch.
type pageWatchers struct {
	ledger coord.Ledger
	// prefix namespaces the scope, so one company's Confluence page ids
	// cannot collide with a completion ledger keyed on a seat handle.
	prefix string
}

// watchScope is the ledger scope one page's subscribers live under.
func (w pageWatchers) watchScope(pageID string) string {
	return w.prefix + strings.TrimSpace(pageID)
}

// Watching returns the subset of handles subscribed to the page.
func (w pageWatchers) Watching(ctx context.Context, pageID string, handles []string) (map[string]bool, error) {
	if w.ledger == nil || strings.TrimSpace(pageID) == "" || len(handles) == 0 {
		return nil, nil
	}
	return w.ledger.Worked(ctx, w.watchScope(pageID), handles)
}

// Watch subscribes one handle to a page.
//
// The DETAIL is what subscribed the seat, which is the only thing an operator
// reading the coordination store could otherwise not reconstruct: a seat that
// edited a page and a seat somebody delegated to are subscribed identically
// and arrived there very differently.
func (w pageWatchers) Watch(ctx context.Context, pageID, handle string, at time.Time) error {
	if w.ledger == nil || strings.TrimSpace(pageID) == "" || strings.TrimSpace(handle) == "" {
		return nil
	}
	return w.ledger.Record(ctx, w.watchScope(pageID), handle, "confluence_touch", at)
}

// confluenceWatchers is the subscription list for this node's Confluence
// parser, or nil where there is no coordination store.
//
// NIL IS A SUPPORTED ANSWER, not a degraded one to work around: a single node
// with no fleet store routes by mention and space lead alone, which is what
// this build did before subscriptions existed. The parser documents nil as
// exactly that.
func (e *Engine) confluenceWatchers() *pageWatchers {
	if e.backends == nil || e.backends.Fleet == nil {
		return nil
	}
	return &pageWatchers{ledger: e.backends.Fleet, prefix: confluenceWatchPrefix}
}

// confluenceWatchPrefix namespaces page subscriptions inside the ledger.
const confluenceWatchPrefix = "confluence:page:"

// watchersFor converts a possibly-nil *pageWatchers into an interface that is
// nil when it should be.
//
// THE TYPED-NIL TRAP, made impossible at the one call site rather than
// documented at it: assigning a nil *pageWatchers to an interface field
// produces a non-nil interface, so every nil check downstream passes and the
// first call panics. Every consumer of an optional pointer-backed seam in
// this package goes through a converter like this one.
func watchersFor(w *pageWatchers) confluence.Watchers {
	if w == nil {
		return nil
	}
	return *w
}
