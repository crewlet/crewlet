package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/github"
	"github.com/crewlet/crewlet/internal/words"
)

// A pull request read for a chat link's preview, with the company's agents'
// apps (the participant lookup's walk): GitHub has no per-person grant here,
// and the caller (internal/api/queries' unfurls) admits only a repository
// under an owner the company's GitHub names, so a reader learns only a title,
// a state and a size of the company's own code the link already points at.
//
// Cached per pull request, a failure as long as an answer: every reader of a
// room previews the same links against an installation's rate limit.

// pullPreviewTTL is how long one preview is reused: a minute, the scale at
// which a pull request's state (a review, a merge) is worth redrawing.
const pullPreviewTTL = time.Minute

// pullPreviewEntries bounds the cache: 512 pull requests is every link a busy
// company shares in an hour, at well under a kibibyte each.
const pullPreviewEntries = 512

// pullPreviewReads bounds this node's previews in flight: four, so a room of
// links spends a handful of an installation's requests rather than a burst,
// and a slow API holds four reads rather than every chat request. A read
// that cannot start inside its caller's deadline is a link drawn plain.
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
	owners  []string
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
// credential or a removed app must not go on answering. The host is the
// preview's own check (`chat_unfurls`).
func (g *githubPullReader) wire(lookup *github.SeatLookup, owners []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.lookup, g.owners, g.entries = lookup, owners, nil
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
		// REWIRED WHILE THIS READ WAS OUT: the old credential's answer is
		// not kept.
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

// current is the lookup of the GitHub wiring now in force, nil without one,
// and the owners the company's configuration names there.
func (g *githubPullReader) current() (*github.SeatLookup, []string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.lookup, g.owners
}

// ChangeRef is raw's tracker name, where raw is a pull request under the
// company's GitHub owners, or why one on its host is refused: the owners are
// the code the company's credentials are for, as a chat link's preview holds.
func (e *Engine) ChangeRef(raw string) (ref, refused string) {
	lookup, owners := e.githubPulls.current()
	if lookup == nil {
		return "", ""
	}
	pull, ref, ok := github.ChangeRef(lookup.Opts.WebBase, raw)
	switch {
	case !ok:
		return "", ""
	case len(owners) == 0:
		return "", fmt.Sprintf("a pull request under %s, and the company's GitHub names no "+
			"organization or repository of its own.", pull.Owner)
	case !slices.Contains(owners, pull.Owner):
		return "", fmt.Sprintf("a pull request under %s, which the company's GitHub does not "+
			"cover: a task waits only on one under %s.", pull.Owner, words.Or(owners))
	}
	return ref, ""
}

// ownedChange reports whether a stored change is a pull request under the
// company's owners, the only ones the poll and the webhook settle.
func (e *Engine) ownedChange(ref string) bool {
	got, refused := e.ChangeRef(ref)
	return got != "" && refused == ""
}
