package codetest

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/runtoken"
	"github.com/crewlet/crewlet/internal/statelog"
)

// CODING BOXES ON A FLEET: every git node of one fleet admits a box by the
// fleet's ONE run store (FixtureRuns), as every node of a deployment reads
// the one pending store on the fleet's coordination store, and verifies its
// token under the fleet's keyring — a ring holding a RETIRED key beside the
// active one (boxKeyring), so every box case also runs across a rotation in
// flight, since runtoken verifies a token by any key the ring holds.

// boxKeyring is the fleet's keyring as a run token's signer reads it: the
// active key k2 and the key k1 it replaced, still held.
var boxKeyring = runtoken.Material{ActiveID: "k2", Keys: []runtoken.KeyMaterial{
	{ID: "k1", Material: "the codetest fleet's retired run-token key"},
	{ID: "k2", Material: "the codetest fleet's active run-token key"},
}}

// NewBoxTokens is a box token signer over the fleet's keyring, as each git
// node builds its own over the keyring every node is configured with.
func NewBoxTokens(t testing.TB) *host.BoxTokens {
	t.Helper()
	tokens, err := host.NewBoxTokens(boxKeyring, nil)
	if err != nil {
		t.Fatalf("codetest: box tokens over the fleet's keyring: %v", err)
	}
	return tokens
}

// FixtureRuns is the fleet's coding runs' rows, in memory: the [host.Runs]
// every git node of a fleet admits a box by. A case launches a run on it
// (Launch), ends, relaunches or removes it, stops its seat, and makes the
// store fail — and reads back every floor raise the hosts made (Raises).
type FixtureRuns struct {
	tokens *host.BoxTokens

	mu   sync.Mutex
	runs map[string]*host.BoxRun // by turn id
	// stops is each seat's pauser who asked its running work to stop.
	stops map[string]string
	// pausesUnread answers Stopped as a node that has not read the pauses.
	pausesUnread bool
	// failing is the store unreachable: every call answers it.
	failing error
	raises  []Raise
	onRaise func(Raise) error
	// reads counts Run's calls: a request refused before its run is read
	// costs the store nothing.
	reads int
}

// Raise is one floor raise a host asked of the run store.
type Raise struct {
	Ref host.BoxRunRef
	Pos statelog.Position
}

// NewFixtureRuns is an empty run store whose tokens are signed by tokens.
func NewFixtureRuns(tokens *host.BoxTokens) *FixtureRuns {
	return &FixtureRuns{tokens: tokens, runs: map[string]*host.BoxRun{}, stops: map[string]string{}}
}

var _ host.Runs = (*FixtureRuns)(nil)

// Launch is a run of seat given grants, launched at floor — the position its
// launch read the repositories at (a launch always reads one) — and RUNNING:
// a fresh turn id and launch id, provenance as a launch stamps it.
func (r *FixtureRuns) Launch(seat string, floor statelog.Position, grants ...host.BoxGrant) host.BoxRunRef {
	ref := host.BoxRunRef{TurnID: uuid.Must(uuid.NewV7()).String(), LaunchID: uuid.NewString()}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.runs[ref.TurnID] = &host.BoxRun{Ref: ref, Live: true, Status: "running", Seat: seat,
		Repos: append([]host.BoxGrant(nil), grants...), Floor: floor,
		Provenance: code.RunProvenance{TurnID: ref.TurnID, RunID: ref.LaunchID, WorkBackend: "native",
			WorkKey: "ENG-1", Model: "fixture-model", CodingAgent: "claude-code"}}
	return ref
}

// Token is ref's git credential, as the launch hands it to the box.
func (r *FixtureRuns) Token(ref host.BoxRunRef) string { return r.tokens.Mint(ref) }

// End ends ref's run: its job left its box and its collection is reading
// what it pushed (status resumed).
func (r *FixtureRuns) End(ref host.BoxRunRef) {
	r.edit(ref, func(run *host.BoxRun) { run.Live, run.Status = false, "resumed" })
}

// Relaunch launches ref's turn again — a second run_sandbox call of the same
// turn — under a fresh launch id, which every token of the first job no
// longer matches.
func (r *FixtureRuns) Relaunch(ref host.BoxRunRef) host.BoxRunRef {
	next := host.BoxRunRef{TurnID: ref.TurnID, LaunchID: uuid.NewString()}
	r.edit(ref, func(run *host.BoxRun) { run.Ref, run.Provenance.RunID = next, next.LaunchID })
	return next
}

// Remove deletes ref's row: a run that ended and was collected.
func (r *FixtureRuns) Remove(ref host.BoxRunRef) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.runs, ref.TurnID)
}

// SetProvenance replaces ref's run's provenance — a work item with a key a
// vendor wrote, say — keeping its two ids.
func (r *FixtureRuns) SetProvenance(ref host.BoxRunRef, p code.RunProvenance) {
	r.edit(ref, func(run *host.BoxRun) {
		p.TurnID, p.RunID = run.Provenance.TurnID, run.Provenance.RunID
		run.Provenance = p
	})
}

// Stop pauses seat with stop_running, by who; Resume lifts it.
func (r *FixtureRuns) Stop(seat, by string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.stops[seat] = by
}

// Resume lifts seat's stop.
func (r *FixtureRuns) Resume(seat string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.stops, seat)
}

// PausesUnread has Stopped answer as a node that has not yet read the
// pauses (true), or as one that has (false).
func (r *FixtureRuns) PausesUnread(unread bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pausesUnread = unread
}

// Fail makes the store unreachable (err), or reachable again (nil).
func (r *FixtureRuns) Fail(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.failing = err
}

// OnRaise is told every floor raise as it is asked, before it is written —
// a case's window onto WHEN a host raises, against what it writes — and
// fails that raise where it answers an error.
func (r *FixtureRuns) OnRaise(fn func(Raise) error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.onRaise = fn
}

// Raises is every floor raise the fleet's hosts asked, in order.
func (r *FixtureRuns) Raises() []Raise {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Raise(nil), r.raises...)
}

// Floor is ref's run's floor as its row holds it.
func (r *FixtureRuns) Floor(ref host.BoxRunRef) statelog.Position {
	r.mu.Lock()
	defer r.mu.Unlock()
	if run, ok := r.runs[ref.TurnID]; ok {
		return run.Floor
	}
	return statelog.Position{}
}

// edit changes ref's row under the lock, when it is there.
func (r *FixtureRuns) edit(ref host.BoxRunRef, fn func(*host.BoxRun)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if run, ok := r.runs[ref.TurnID]; ok {
		fn(run)
	}
}

// Reads is how many times a host has read a run's row.
func (r *FixtureRuns) Reads() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.reads
}

// Run is the row of turnID's run, as it stands (host.Runs).
func (r *FixtureRuns) Run(_ context.Context, turnID string) (host.BoxRun, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.reads++
	if r.failing != nil {
		return host.BoxRun{}, false, r.failing
	}
	run, ok := r.runs[turnID]
	if !ok {
		return host.BoxRun{}, false, nil
	}
	out := *run
	out.Repos = append([]host.BoxGrant(nil), run.Repos...)
	return out, true, nil
}

// RaiseFloor raises ref's run's floor to pos while ref's launch is the row's
// and the run is live (host.Runs), recording the call.
func (r *FixtureRuns) RaiseFloor(_ context.Context, ref host.BoxRunRef, pos statelog.Position) error {
	r.mu.Lock()
	hook := r.onRaise
	failing := r.failing
	r.mu.Unlock()
	if hook != nil {
		if err := hook(Raise{Ref: ref, Pos: pos}); err != nil {
			return err
		}
	}
	if failing != nil {
		return failing
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.raises = append(r.raises, Raise{Ref: ref, Pos: pos})
	run, ok := r.runs[ref.TurnID]
	if !ok || !run.Live || run.Ref.LaunchID != ref.LaunchID {
		return nil
	}
	if run.Floor.IsZero() {
		run.Floor = pos
		return nil
	}
	if before, err := run.Floor.Before(pos); err == nil && before {
		run.Floor = pos
	}
	return nil
}

// Stopped is who stopped seat (host.Runs).
func (r *FixtureRuns) Stopped(_ context.Context, seat string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.failing != nil:
		return "", r.failing
	case r.pausesUnread:
		return "", fmt.Errorf("codetest: this node has not yet read which seats are paused")
	}
	return r.stops[seat], nil
}

// BoxUser is the Basic user name a box presents (host.BoxUser).
const BoxUser = host.BoxUser

// NewBox is a coding box's git for ref's run on f: a client whose credential
// helper answers a node's challenge as host.BoxUser with the run's token, on
// the git the fleet's persons run (Fleet.ClientGit).
func NewBox(t testing.TB, f Fleet, ref host.BoxRunRef) *Person {
	t.Helper()
	return clientWith(t, f.ClientGit(), BoxUser, f.Runs().Token(ref))
}

// BoxURL is repo's URL under /box/git/ through n.
func BoxURL(n Node, repo string) string { return n.URL() + "/box/git/" + repo }

// BoxURL is RepoName's box URL through the i-th git node.
func (c *Case) BoxURL(i int) string { return BoxURL(c.Fleet.Nodes()[i], RepoName) }

// LaunchBox launches a run of seat given RepoName at access, its floor the
// repository's position now, and answers the run and its box's git.
func (c *Case) LaunchBox(seat string, access code.Access) (host.BoxRunRef, *Person) {
	c.T.Helper()
	rows := c.Rows()
	ref := c.Fleet.Runs().Launch(seat, rows.Position, host.BoxGrant{RepoID: rows.Repo.ID, Name: RepoName,
		Access: access, Branch: code.SeatNamespace(seat) + "work"})
	return ref, NewBox(c.T, c.Fleet, ref)
}

// SplitURL is a URL whose git requests a proxy sends by WHAT they are: a
// push's or a fetch's advertisement (info/refs) to adverts, and the RPC that
// follows it to rpcs — what a balancer may do with the two halves of one
// push. base is the path below each node's URL, the repository's.
func SplitURL(t testing.TB, adverts, rpcs Node, base string) string {
	t.Helper()
	proxy := func(n Node) *httputil.ReverseProxy {
		target, err := url.Parse(n.URL())
		if err != nil {
			t.Fatalf("codetest: %s's URL: %v", n.ID(), err)
		}
		p := httputil.NewSingleHostReverseProxy(target)
		p.Transport = n.Client().Transport
		return p
	}
	a, b := proxy(adverts), proxy(rpcs)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/info/refs") {
			a.ServeHTTP(w, r)
			return
		}
		b.ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + base
}
