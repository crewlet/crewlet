package home_test

import (
	"context"
	"errors"
	"go/build"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/home"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE HOME'S LOOP OVER STAND-INS: the rows a code log would hold, as the due
// listing answers them (estate), who is home to each repository (placement)
// and the code log's watch (nudges) — so each case says exactly which
// repository a node worked, at which floor, and what it asked of the
// repository's writers.

// The repositories the cases work, and the change each holds.
const (
	repoA = "01920000-0000-7000-8000-00000000000a"
	repoB = "01920000-0000-7000-8000-00000000000b"
)

// estate is the code log and the git reads as a home's loop reaches them: each
// repository's due changes, answered at the floor a read names, and the facts
// of each head — a revise or a landing asked of it takes its change off the
// list, as the rows' own would.
type estate struct {
	mu       sync.Mutex
	due      map[string][]code.ChangeDue
	owners   map[string][]code.PathOwner
	facts    map[string]code.RevisionFactsAnswer
	at       statelog.Position
	floors   []statelog.Freshness
	revises  []code.Revise
	landings []code.ChangeLanded
	changed  chan struct{}
}

func newEstate() *estate {
	return &estate{due: map[string][]code.ChangeDue{}, owners: map[string][]code.PathOwner{},
		facts: map[string]code.RevisionFactsAnswer{}, changed: make(chan struct{}, 1),
		at: statelog.Position{Stream: topics.CodeLogStream, Seq: 40}}
}

// hold puts change n of repo on repo's due list, its head head and its target
// main at tip, the facts its home reads of it f.
func (e *estate) hold(repo string, n int64, head, tip string, f code.RevisionFactsAnswer) code.ChangeDue {
	e.mu.Lock()
	defer e.mu.Unlock()
	due := code.ChangeDue{ID: changeID(repo, n), Number: n, Revision: 1, Head: head,
		SourceRef: "refs/heads/feature-" + string(rune('a'+n)), TargetRef: "refs/heads/main", Tip: tip,
		TipPosition: statelog.Position{Stream: topics.CodeLogStream, Seq: 30}}
	e.due[repo] = append(e.due[repo], due)
	f.Head, f.Target = head, tip
	e.facts[head] = f
	return due
}

func (e *estate) Read(_ context.Context, q code.ReadQuery) (code.ReadAnswer, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if q.Kind != code.ReadChangesDue {
		return code.ReadAnswer{}, errors.New("the home asked a read that is not its listing")
	}
	e.floors = append(e.floors, q.Fresh)
	if q.ChangesDue == nil {
		var repos []string
		for repo, changes := range e.due {
			if len(changes) > 0 {
				repos = append(repos, repo)
			}
		}
		slices.Sort(repos)
		return code.ReadAnswer{DueRepos: repos, Position: e.at}, nil
	}
	repo := q.ChangesDue.RepoID
	return code.ReadAnswer{DueChanges: slices.Clone(e.due[repo]), PathOwners: e.owners[repo], Position: e.at}, nil
}

func (e *estate) GitRead(_ context.Context, q code.GitQuery) (code.GitAnswer, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := q.Validate(); err != nil {
		return code.GitAnswer{}, err
	}
	f, ok := e.facts[q.RevisionFacts.Head]
	if !ok || f.Target != q.RevisionFacts.Target {
		return code.GitAnswer{}, code.ErrObjectNotFound
	}
	return code.GitAnswer{Kind: code.GitRevisionFacts, RepoID: q.RepoID, Position: e.at, RevisionFacts: &f}, nil
}

func (e *estate) Converse(_ context.Context, w code.ConversationWrite) (code.ConversationResult, error) {
	if err := w.Validate(); err != nil {
		return code.ConversationResult{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.revises = append(e.revises, *w.Revise)
	e.done(w.Revise.RepoID, w.Revise.ChangeID)
	return code.ConversationResult{Outcome: statelog.OutcomeApplied, Position: e.at}, nil
}

func (e *estate) Change(_ context.Context, p code.ChangeProposal) (code.ChangeResult, error) {
	if err := p.Validate(); err != nil {
		return code.ChangeResult{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.landings = append(e.landings, *p.Landed)
	e.done(p.Landed.RepoID, p.Landed.ChangeID)
	return code.ChangeResult{Outcome: statelog.OutcomeApplied, ChangeID: p.Landed.ChangeID, Position: e.at}, nil
}

// done takes change off repo's due list, as its record's rows would, and says
// so. Under mu.
func (e *estate) done(repo, change string) {
	e.due[repo] = slices.DeleteFunc(e.due[repo], func(d code.ChangeDue) bool { return d.ID == change })
	select {
	case e.changed <- struct{}{}:
	default:
	}
}

// waitReads waits until the loop has read its listing at least n times —
// its first pass's among them — or fails t.
func (e *estate) waitReads(t *testing.T, n int) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		e.mu.Lock()
		read := len(e.floors)
		e.mu.Unlock()
		if read >= n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the loop read its listing %d times in 10s, want %d", read, n)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// wrote is every revise and landing asked so far.
func (e *estate) wrote() ([]code.Revise, []code.ChangeLanded) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.revises), slices.Clone(e.landings)
}

// waitDone waits until repo's due list is empty, or fails t at the deadline.
func (e *estate) waitDone(t *testing.T, repo string, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
	for {
		e.mu.Lock()
		left := len(e.due[repo])
		e.mu.Unlock()
		if left == 0 {
			return
		}
		select {
		case <-e.changed:
		case <-deadline:
			t.Fatalf("repository %s still holds %d due changes after %v", repo, left, within)
		}
	}
}

// placement is who is home to each repository, or an error for all of them.
type placement struct {
	mu    sync.Mutex
	homes map[string]string
	err   error
}

func (p *placement) Home(repo string) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.homes[repo], p.err
}

func (p *placement) HomeOfFor(node string, repos []string) (map[string]bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.err != nil {
		return nil, p.err
	}
	out := map[string]bool{}
	for _, r := range repos {
		if p.homes[r] == node {
			out[r] = true
		}
	}
	return out, nil
}

// asNode is p as node reads it: HomeOf answers the repositories node is home
// to.
type asNode struct {
	*placement
	node string
}

func (a asNode) HomeOf(repos []string) (map[string]bool, error) {
	return a.placement.HomeOfFor(a.node, repos)
}

// nudges is the code log's watch, its handler kept for the case to call — or
// a watch that does not begin, while failing is set.
type nudges struct {
	mu      sync.Mutex
	h       *home.Nudged
	failing bool
	began   chan struct{}
}

func newNudges() *nudges { return &nudges{began: make(chan struct{})} }

func (n *nudges) Watch(_ context.Context, filter string, h home.Nudged) (home.Watching, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	if n.failing {
		return nil, errors.New("the consumer could not be made")
	}
	if filter != topics.CodeLogPrefix+"."+string(code.KindRepo)+".>" {
		return nil, errors.New("the home watched " + filter + ", not every repository's subject")
	}
	n.h = &h
	close(n.began)
	return stopped{}, nil
}

// subject tells the loop a record landed on repo's subject at seq.
func (n *nudges) subject(t *testing.T, repo string, seq uint64) {
	t.Helper()
	select {
	case <-n.began:
	case <-time.After(10 * time.Second):
		t.Fatal("the loop's watch never began")
	}
	n.mu.Lock()
	h := n.h
	n.mu.Unlock()
	h.Subject(topics.CodeLogSubject(string(code.KindRepo), repo), statelog.Position{Stream: topics.CodeLogStream,
		Seq: seq})
}

// gap tells the loop it may have missed records up to seq.
func (n *nudges) gap(t *testing.T, seq uint64) {
	t.Helper()
	select {
	case <-n.began:
	case <-time.After(10 * time.Second):
		t.Fatal("the loop's watch never began")
	}
	n.mu.Lock()
	h := n.h
	n.mu.Unlock()
	h.Gap(statelog.Position{Stream: topics.CodeLogStream, Seq: seq})
}

type stopped struct{}

func (stopped) Stop() {}

// loopOn is node's loop over e, p and n, its backstop backstop, started, with
// a recorder of its own; stopped when the case ends.
func loopOn(t *testing.T, node string, e *estate, p *placement, n *nudges,
	backstop time.Duration) (*home.Loop, *metrics.Recorder) {

	t.Helper()
	rec, err := metrics.New()
	if err != nil {
		t.Fatal(err)
	}
	l, err := home.New(home.Options{Estate: e, Placement: asNode{placement: p, node: node}, Nudges: n,
		NodeID: node, Metrics: rec})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	home.SetBackstop(l, backstop)
	l.Start()
	t.Cleanup(func() { l.Stop(context.WithoutCancel(t.Context())) })
	return l, rec
}

// changeID is change n of repo's id: a bare UUIDv7 naming both.
func changeID(repo string, n int64) string {
	return repo[:len(repo)-2] + repo[len(repo)-1:] + string(rune('0'+n))
}

// oid is a whole object id made of c.
func oid(c string) string { return strings.Repeat(c, 40)[:40] }

// counted is what rec counts of name under attrs.
func counted(rec *metrics.Recorder, name string, attrs metrics.Attrs) uint64 {
	var n uint64
	for _, s := range rec.Read() {
		if s.Name != name {
			continue
		}
		match := true
		for k, v := range attrs {
			if s.Attrs[k] != v {
				match = false
			}
		}
		if match {
			n += uint64(s.Total)
		}
	}
	return n
}

// ONLY THE HOME WORKS A REPOSITORY: two git nodes' loops over one code log,
// each home to one of two repositories with a change due, each work their own
// and nothing of the other's — a pass lists both, and schedules only what its
// placement says it is home to; a nudge for the other node's repository
// schedules nothing — and each record names the node that asked it.
//
// Mutation: schedule every listed repository whatever its home, and node-b
// records repoA's facts too.
func TestOnlyTheHomeWorksARepository(t *testing.T) {
	t.Parallel()
	e := newEstate()
	e.hold(repoA, 1, oid("a"), oid("1"), code.RevisionFactsAnswer{MergeBase: oid("1"), PatchID: oid("e"),
		Touched: []string{"a.go"}})
	e.hold(repoB, 1, oid("b"), oid("2"), code.RevisionFactsAnswer{MergeBase: oid("2"), PatchID: oid("f"),
		Touched: []string{"b.go"}})
	p := &placement{homes: map[string]string{repoA: "node-a", repoB: "node-b"}}
	na, nb := newNudges(), newNudges()
	_, recA := loopOn(t, "node-a", e, p, na, time.Hour)
	_, recB := loopOn(t, "node-b", e, p, nb, time.Hour)
	e.waitDone(t, repoA, 10*time.Second)
	e.waitDone(t, repoB, 10*time.Second)
	revises, _ := e.wrote()
	if len(revises) != 2 {
		t.Fatalf("the two homes recorded %d revisions' facts, want one each: %+v", len(revises), revises)
	}
	for _, r := range revises {
		want := map[string]string{repoA: "node-a", repoB: "node-b"}[r.RepoID]
		if r.Node != want {
			t.Errorf("repository %s's facts were recorded by %s, want its home %s", r.RepoID, r.Node, want)
		}
	}
	na.subject(t, repoB, 41)
	nb.subject(t, repoA, 41)
	if got := counted(recA, metrics.CodeHomeNudges, metrics.Attrs{"outcome": "elsewhere"}) +
		counted(recB, metrics.CodeHomeNudges, metrics.Attrs{"outcome": "elsewhere"}); got != 2 {
		t.Errorf("the nudges for each other's repository counted %d elsewhere, want 2", got)
	}
	if got := counted(recA, metrics.CodeHomePasses, metrics.Attrs{"worker": home.WorkerChanges,
		"outcome": "worked"}); got != 1 {
		t.Errorf("node-a counts %d turns worked, want its one repository's", got)
	}
}

// A NUDGE ONLY SHORTENS THE WAIT: a change due on a repository whose home's
// backstop is an hour away is worked the moment a record on the repository is
// told — its listing read at that record's position, so the rows it reads
// hold the record — and a pass of the backstop finds the same work from the
// rows alone, with no nudge at all: the nudge decides nothing the rows do not.
//
// Mutation: read the nudged repository's due changes stale rather than at the
// record's position, and its listing names no floor.
func TestANudgeOnlyShortensTheWait(t *testing.T) {
	t.Parallel()
	e := newEstate()
	p := &placement{homes: map[string]string{repoA: "node-a"}}
	n := newNudges()
	l, _ := loopOn(t, "node-a", e, p, n, time.Hour)
	// THE FIRST PASS, run at Start, has found nothing due.
	e.waitReads(t, 1)
	e.hold(repoA, 1, oid("a"), oid("1"), code.RevisionFactsAnswer{MergeBase: oid("1"), PatchID: oid("e"),
		Touched: []string{"a.go"}})
	n.subject(t, repoA, 57)
	e.waitDone(t, repoA, 10*time.Second)
	e.mu.Lock()
	floors := slices.Clone(e.floors)
	e.mu.Unlock()
	nudged := statelog.Position{Stream: topics.CodeLogStream, Seq: 57}
	if !slices.ContainsFunc(floors, func(f statelog.Freshness) bool {
		return f.Level == statelog.ReadSession && slices.Contains(f.Floors, nudged)
	}) {
		t.Fatalf("the nudged repository was read at %+v, none at the record's position %s", floors, nudged)
	}

	e.hold(repoA, 2, oid("c"), oid("1"), code.RevisionFactsAnswer{MergeBase: oid("1"), PatchID: oid("f"),
		Touched: []string{"c.go"}})
	if _, err := l.Poll(t.Context(), statelog.Position{}); err != nil {
		t.Fatalf("Poll: %v", err)
	}
	e.waitDone(t, repoA, 10*time.Second)
	if revises, _ := e.wrote(); len(revises) != 2 {
		t.Fatalf("the home recorded %d revisions' facts, want the nudged change's and the one the pass found",
			len(revises))
	}
}

// A LOST NUDGE IS CAUGHT BY THE BACKSTOP: with no watch running at all — its
// consumer cannot be made — a change due on a repository is found and worked
// by the backstop's next pass; and a watch's GAP — it may have missed records
// up to a position — asks a pass at once, its listing read at that position.
//
// Mutation: run no pass but the first, and the change waits for a nudge that
// never comes.
func TestALostNudgeIsCaughtByTheBackstop(t *testing.T) {
	t.Parallel()
	e := newEstate()
	p := &placement{homes: map[string]string{repoA: "node-a", repoB: "node-a"}}
	deaf := newNudges()
	deaf.failing = true
	_, rec := loopOn(t, "node-a", e, p, deaf, 20*time.Millisecond)
	// THE FIRST PASS, at the start, has listed nothing due: the change made
	// due after it is found by a later pass or by nobody.
	e.waitReads(t, 1)
	e.hold(repoA, 1, oid("a"), oid("1"), code.RevisionFactsAnswer{MergeBase: oid("1"), PatchID: oid("e"),
		Touched: []string{"a.go"}})
	e.waitDone(t, repoA, 10*time.Second)

	e2 := newEstate()
	n := newNudges()
	loopOn(t, "node-a", e2, p, n, time.Hour)
	e2.waitReads(t, 1)
	e2.hold(repoB, 1, oid("b"), oid("2"), code.RevisionFactsAnswer{MergeBase: oid("2"), PatchID: oid("f"),
		Touched: []string{"b.go"}})
	n.gap(t, 77)
	e2.waitDone(t, repoB, 10*time.Second)
	e2.mu.Lock()
	floors := slices.Clone(e2.floors)
	e2.mu.Unlock()
	through := statelog.Position{Stream: topics.CodeLogStream, Seq: 77}
	if !slices.ContainsFunc(floors, func(f statelog.Freshness) bool { return slices.Contains(f.Floors, through) }) {
		t.Fatalf("the pass a gap asked read at %+v, none at the gap's position %s", floors, through)
	}
	if got := counted(rec, metrics.CodeHomePasses, metrics.Attrs{"worker": home.WorkerChanges,
		"outcome": "worked"}); got == 0 {
		t.Errorf("the deaf home counts no turn worked")
	}
}

// A CHANGE WHOSE HEAD IS IN ITS TARGET IS LANDED: its facts read, finding the
// head inside the target's tip — the target moved past it — the home asks the
// committer for the landing, at the change's revision and head and the tip it
// read, and records no facts of it; a change whose head is not in its target
// has its facts recorded, its touched paths matched against the repository's
// path owners on the home, beside the digest of the owners matched against.
//
// Mutation: record the facts of a head its target holds rather than land it,
// and the landing is never asked.
func TestAChangeWhoseHeadIsInItsTargetIsLanded(t *testing.T) {
	t.Parallel()
	e := newEstate()
	owners := []code.PathOwner{{Owner: code.Owner{Kind: code.OwnerUnit, Ref: "Platform"}, Paths: []string{"upload/**"}},
		{Owner: code.Owner{Kind: code.OwnerSeat, Ref: "cto"}, Paths: []string{"docs/*.md"}}}
	e.owners[repoA] = owners
	landed := e.hold(repoA, 1, oid("a"), oid("1"), code.RevisionFactsAnswer{MergeBase: oid("a"),
		HeadInTarget: true, Touched: []string{}})
	measured := e.hold(repoA, 2, oid("b"), oid("1"), code.RevisionFactsAnswer{MergeBase: oid("9"),
		PatchID: oid("e"), Touched: []string{"upload/retry.go", "README.md"}})
	p := &placement{homes: map[string]string{repoA: "node-a"}}
	loopOn(t, "node-a", e, p, newNudges(), time.Hour)
	e.waitDone(t, repoA, 10*time.Second)
	revises, landings := e.wrote()
	want := code.ChangeLanded{RepoID: repoA, ChangeID: landed.ID, Revision: landed.Revision, HeadOID: landed.Head,
		TargetRef: landed.TargetRef, TargetOID: landed.Tip, Node: "node-a"}
	if len(landings) != 1 || landings[0] != want {
		t.Fatalf("the home asked the landings %+v, want %+v", landings, want)
	}
	if len(revises) != 1 || revises[0].ChangeID != measured.ID {
		t.Fatalf("the home recorded the facts of %+v, want change 2's alone", revises)
	}
	r := revises[0]
	matched, digest := code.MatchOwners(owners, []string{"upload/retry.go", "README.md"}, false)
	if !slices.Equal(r.RequestedOwners, matched) || r.OwnersDigest != digest || len(matched) != 1 ||
		r.TargetPosition != measured.TipPosition || r.BaseOID != oid("9") || r.PatchID != oid("e") {
		t.Fatalf("change 2's facts are %+v, want them matched to %v under digest %s at the tip's position %s",
			r, matched, digest, measured.TipPosition)
	}
}

// THE HOME IMPORTS NO QUERY, COMPACT OR HOST, and none of them imports another
// or the home: internal/code/query, internal/code/compact, internal/code/host
// and internal/code/home each reach the others' work through the estate's
// router, so a step is the same whichever node answers it, and no one of them
// can call another's in process past the router's walk.
func TestTheHomeImportsNoQueryCompactOrHost(t *testing.T) {
	t.Parallel()
	const module = "github.com/crewlet/crewlet/"
	four := []string{"internal/code/query", "internal/code/compact", "internal/code/host", "internal/code/home"}
	root := sourcetree.Root(t)
	for _, pkg := range four {
		p, err := build.Default.ImportDir(filepath.Join(root, filepath.FromSlash(pkg)), 0)
		if err != nil {
			t.Fatalf("read %s: %v", pkg, err)
		}
		for _, other := range four {
			if other != pkg && slices.Contains(p.Imports, module+other) {
				t.Errorf("%s imports %s: the four reach each other's work through the estate's router", pkg, other)
			}
		}
	}
}
