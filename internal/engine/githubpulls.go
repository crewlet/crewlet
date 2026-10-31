package engine

import (
	"context"
	"errors"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/github"
)

// A pull request read for a chat link's preview.
//
// # The company's credential, and why that is the right one
//
// GitHub has no per-person permission in this engine: a person reads the
// company's code through its links, never through a grant. So a preview is
// read with the credential the company's own agents act with (their apps,
// through the same walk as a thread's participant lookup), and only for a
// repository under an owner the company's GitHub configuration names, which
// the caller checks (internal/api/queries' unfurls). What a reader of the
// room learns is a title, a state and a size of a pull request in the
// company's own code, which the link beside it already points at.
//
// # Cached per pull request, briefly
//
// Every reader of a room previews the same links, and a room open on twenty
// screens would otherwise read one pull request twenty times a minute against
// an installation's rate limit. A preview is kept for [pullPreviewTTL], a
// failure as long, at most [pullPreviewEntries] at once.

// pullPreviewTTL is how long one pull request's preview is reused: a minute,
// the scale at which a pull request's state is worth redrawing (a review, a
// merge) and short enough that a preview is never an afternoon old.
const pullPreviewTTL = time.Minute

// pullPreviewEntries bounds the cache: 512 pull requests is every link a busy
// company shares in an hour, at well under a kibibyte each.
const pullPreviewEntries = 512

// pullPreviewReads bounds how many previews this node reads from GitHub at
// once: four, so a screen opening a room of links, or many screens at once,
// spends a handful of an installation's requests rather than a burst, and a
// slow API holds four reads rather than every chat request on the node. A
// read that cannot start inside its caller's deadline is a link drawn plain.
const pullPreviewReads = 4

// errNoGitHub is a preview asked of a company that does not run GitHub.
var errNoGitHub = errors.New("engine: this company does not run GitHub, so no pull " +
	"request is read for a preview")

// githubPullReader is the pull request reader of the current GitHub wiring.
type githubPullReader struct {
	// reading holds a slot per read in flight ([pullPreviewReads]).
	reading chan struct{}
	once    sync.Once

	mu      sync.Mutex
	lookup  *github.SeatLookup
	entries map[github.PullRef]pullPreview
	now     func() time.Time
}

// pullPreview is one cached read.
type pullPreview struct {
	pr  github.PullRequest
	err error
	at  time.Time
}

// wire replaces the reader's credential with a new wiring's, or empties it
// (nil) when GitHub is retired, dropping every cached read: a rotated
// credential or a removed app must not go on answering. Which host a pull
// request's address is read against is the preview's own check, against the
// company document (`chat_unfurls`), before it asks.
func (g *githubPullReader) wire(lookup *github.SeatLookup) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.lookup, g.entries = lookup, nil
}

// clock is the reader's time, the wall clock unless a suite set one.
func (g *githubPullReader) clock() time.Time {
	if g.now != nil {
		return g.now()
	}
	return time.Now()
}

// read is one pull request, from the cache while it is fresh.
func (g *githubPullReader) read(ctx context.Context, ref github.PullRef) (
	github.PullRequest, error) {

	g.mu.Lock()
	lookup := g.lookup
	if hit, ok := g.entries[ref]; ok && g.clock().Sub(hit.at) < pullPreviewTTL {
		g.mu.Unlock()
		return hit.pr, hit.err
	}
	g.mu.Unlock()
	if lookup == nil {
		return github.PullRequest{}, errNoGitHub
	}
	g.once.Do(func() { g.reading = make(chan struct{}, pullPreviewReads) })
	select {
	case g.reading <- struct{}{}:
	case <-ctx.Done():
		return github.PullRequest{}, ctx.Err()
	}
	pr, err := lookup.PullRequest(ctx, ref)
	<-g.reading
	if err != nil && ctx.Err() != nil {
		// THE CALLER WENT AWAY, which says nothing about the pull request.
		return github.PullRequest{}, err
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.lookup != lookup {
		// REWIRED WHILE THIS READ WAS OUT: its answer is the old
		// credential's, and is not kept.
		return pr, err
	}
	if g.entries == nil {
		g.entries = map[github.PullRef]pullPreview{}
	}
	if len(g.entries) >= pullPreviewEntries {
		g.evictOldest()
	}
	g.entries[ref] = pullPreview{pr: pr, err: err, at: g.clock()}
	return pr, err
}

// evictOldest drops the read made longest ago.
func (g *githubPullReader) evictOldest() {
	var oldest github.PullRef
	var at time.Time
	first := true
	for ref, entry := range g.entries {
		if first || entry.at.Before(at) {
			oldest, at, first = ref, entry.at, false
		}
	}
	delete(g.entries, oldest)
}

// PullRequestPreview reads one pull request for a chat link's preview through
// the agents' apps of the GitHub wiring now in force, cached briefly.
func (e *Engine) PullRequestPreview(ctx context.Context, ref github.PullRef) (
	github.PullRequest, error) {

	return e.githubPulls.read(ctx, ref)
}
