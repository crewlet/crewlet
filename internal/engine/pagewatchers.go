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
// A wiki page event names only who edited it, so without this list every
// change that is not an explicit mention falls through to the space lead,
// including follow-ups on a page a seat just edited or one a lead delegated by
// mention, which the lead-fallback prompt asks for precisely to stop them.
// Confluence's own watcher list will not do (internal/confluence's package
// doc): mostly people, a call per event, and often unreadable by a per-role
// token. It is a [coord.Ledger] rather than a bucket of its own: set membership
// over a scope tested against known candidates is exactly the Ledger, failure
// polarity included, and a second primitive of that shape means two retention
// rules and two backends' conformance cases.

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

// Watching returns the subset of seats subscribed to the page, each named by
// its handle — see [confluence.Watchers].
func (w pageWatchers) Watching(ctx context.Context, pageID string, seats []string) (map[string]bool, error) {
	if w.ledger == nil || strings.TrimSpace(pageID) == "" || len(seats) == 0 {
		return nil, nil
	}
	return w.ledger.Worked(ctx, w.watchScope(pageID), seats)
}

// Watch subscribes one seat, named by its handle, to a page.
//
// The detail records what subscribed the seat, which an operator reading the
// store could not otherwise tell: an editor and a delegate are subscribed
// identically.
func (w pageWatchers) Watch(ctx context.Context, pageID, seat string, at time.Time) error {
	if w.ledger == nil || strings.TrimSpace(pageID) == "" || strings.TrimSpace(seat) == "" {
		return nil
	}
	return w.ledger.Record(ctx, w.watchScope(pageID), seat, "confluence_touch", at)
}

// confluenceWatchers is the subscription list for this node's Confluence
// parser, or nil where there is no coordination store.
// Nil is supported, not degraded: a node with no fleet store routes by mention
// and space lead alone, as the parser documents.
func (e *Engine) confluenceWatchers() *pageWatchers {
	if e.backends == nil || e.backends.Fleet == nil {
		return nil
	}
	return &pageWatchers{ledger: e.backends.Fleet, prefix: confluenceWatchPrefix}
}

// confluenceWatchPrefix namespaces page subscriptions inside the ledger.
const confluenceWatchPrefix = "confluence:page:"

// watchersFor converts a possibly-nil *pageWatchers into an interface that is
// nil when it should be: a nil pointer in an interface field is a non-nil
// interface, so every nil check downstream passes and the first call panics.
func watchersFor(w *pageWatchers) confluence.Watchers {
	if w == nil {
		return nil
	}
	return *w
}
