package home_test

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"go/build"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitlet/home"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// The home's loop over stand-ins for the code log's rows (estate), each
// repository's home (placement) and the code log's watch (nudges), so a case
// says which repository a node worked, at which floor, and what it asked of
// the repository's writers.

// The repositories the cases work.
const (
	repoA = "01920000-0000-7000-8000-00000000000a"
	repoB = "01920000-0000-7000-8000-00000000000b"
)

// estate is the code log and the git reads as a home's loop reaches them: each
// repository's due changes, its rechecks (changes a moved target asks merged
// again, oldest first, as many as a read asks), its changes whose asks are due
// a settling, and each head's facts. Each write takes off what its rows would:
// a revise the facts, a mergeability the merge, a landing the whole change, a
// settling the asks; a change owing nothing more leaves the list.
type estate struct {
	mu       sync.Mutex
	due      map[string][]gitlet.ChangeDue
	rechecks map[string][]gitlet.ChangeDue
	unsettle map[string][]gitlet.AskDue
	settles  []gitlet.AskSettle
	owners   map[string][]gitlet.PathOwner
	gates    map[string][]gitlet.RuleGate
	facts    map[string]gitlet.RevisionFactsAnswer
	// factsRead counts the reads of each head's facts.
	factsRead map[string]int
	at        statelog.Position
	// revisedAt is where a revise's record lands (e.at when zero);
	// checkFloors is every floor a repository's check work was read at.
	revisedAt   statelog.Position
	checkFloors []statelog.Freshness
	floors      []statelog.Freshness
	asked       []gitlet.ChangesDueQuery
	revises     []gitlet.Revise
	merges      []gitlet.Mergeability
	landings    []gitlet.ChangeLanded
	// landRefusal or landErr, while set, answers every landing (a refusal
	// that decided nothing, or a failed call), leaving the change listed.
	landRefusal gitlet.ChangeRefusal
	landErr     error
	// recording, if set, runs as each mergeability is recorded: a slow
	// change writer.
	recording func()
	// pushes is the repositories' pushes in log order; introduced serves
	// them to a landing's walk newest first.
	pushes  []gitlet.IntroducedRow
	changed chan struct{}
}

func newEstate() *estate {
	return &estate{due: map[string][]gitlet.ChangeDue{}, rechecks: map[string][]gitlet.ChangeDue{},
		unsettle: map[string][]gitlet.AskDue{}, owners: map[string][]gitlet.PathOwner{},
		gates: map[string][]gitlet.RuleGate{}, facts: map[string]gitlet.RevisionFactsAnswer{}, factsRead: map[string]int{},
		changed: make(chan struct{}, 1), at: statelog.Position{Stream: topics.CodeLogStream, Seq: 40}}
}

// hold puts change n of repo on its due list owing its facts alone, head head
// into main at tip, and makes f the facts read of head, adding the base and
// head trees a Gitlet node reads where f names none and head is not merged.
func (e *estate) hold(repo string, n int64, head, tip string, f gitlet.RevisionFactsAnswer) gitlet.ChangeDue {
	e.mu.Lock()
	defer e.mu.Unlock()
	due := gitlet.ChangeDue{ID: changeID(repo, n), Number: n, Revision: 1, Head: head,
		SourceRef: "refs/heads/feature-" + string(rune('a'+n)), TargetRef: "refs/heads/main", Tip: tip,
		TipPosition: statelog.Position{Stream: topics.CodeLogStream, Seq: 30}, FactsDue: true}
	e.due[repo] = append(e.due[repo], due)
	f.Head, f.Target = head, tip
	if !f.HeadInTarget && f.HeadTree == "" {
		f.BaseTree, f.HeadTree = oid("7"), oid("8")
	}
	e.facts[head] = f
	return due
}

// owe puts due on repo's due list as it is.
func (e *estate) owe(repo string, due gitlet.ChangeDue) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.due[repo] = append(e.due[repo], due)
}

// owesSettling puts dues among repo's changes whose asks are due a settling.
func (e *estate) owesSettling(repo string, dues ...gitlet.AskDue) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.unsettle[repo] = append(e.unsettle[repo], dues...)
}

// settled is every settling of asks the home asked, in the order it asked.
func (e *estate) settled() []gitlet.AskSettle {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.settles)
}

// recheck appends due to repo's rechecks: the rows name them oldest merge
// first.
func (e *estate) recheck(repo string, due gitlet.ChangeDue) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rechecks[repo] = append(e.rechecks[repo], due)
}

// QueueDue answers no due queue work: no queue in these cases owes any.
func (e *estate) QueueDue(context.Context, gitlet.QueueDueQuery) (gitlet.QueueDueAnswer, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return gitlet.QueueDueAnswer{Position: e.at}, nil
}

func (e *estate) Read(_ context.Context, q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case q.Kind == gitlet.ReadChecksDue:
		// Check work: no change here owes any; only the floor is kept.
		if q.ChecksDue != nil && q.ChecksDue.RepoID != "" {
			e.checkFloors = append(e.checkFloors, q.Fresh)
		}
		return gitlet.ReadAnswer{Position: e.at}, nil
	case q.Kind == gitlet.ReadIntroduced:
		return e.introduced(q)
	case q.Kind != gitlet.ReadChangesDue:
		return gitlet.ReadAnswer{}, errors.New("the home asked a read that is not its listing")
	case q.ChangesDue == nil || q.ChangesDue.RecheckBefore.IsZero():
		return gitlet.ReadAnswer{}, errors.New("the home's listing named no instant its refresh is paced by")
	}
	e.floors = append(e.floors, q.Fresh)
	e.asked = append(e.asked, *q.ChangesDue)
	if q.ChangesDue.RepoID == "" {
		var repos []string
		for repo, changes := range e.due {
			if len(changes) > 0 || len(e.rechecks[repo]) > 0 {
				repos = append(repos, repo)
			}
		}
		for repo, changes := range e.rechecks {
			if len(changes) > 0 && len(e.due[repo]) == 0 {
				repos = append(repos, repo)
			}
		}
		for repo, dues := range e.unsettle {
			if len(dues) > 0 {
				repos = append(repos, repo)
			}
		}
		slices.Sort(repos)
		return gitlet.ReadAnswer{DueRepos: slices.Compact(repos), Position: e.at}, nil
	}
	repo := q.ChangesDue.RepoID
	again := e.rechecks[repo]
	again = slices.Clone(again[:min(len(again), q.ChangesDue.Rechecks)])
	return gitlet.ReadAnswer{DueChanges: slices.Clone(e.due[repo]), Rechecks: again, PathOwners: e.owners[repo],
		Gates: e.gates[repo], AskSettles: slices.Clone(e.unsettle[repo]), Position: e.at}, nil
}

// introduced answers a landing walk's read as the reader's descending
// `introduced` read does: q's repository's pushes below its bound, newest
// first by position then push id, at most its limit a page, keyed after the
// last served. Under mu.
func (e *estate) introduced(q gitlet.ReadQuery) (gitlet.ReadAnswer, error) {
	in := q.Introduced
	if in == nil || in.RepoID == "" || !in.Descending || in.Before.IsZero() || q.Limit < 1 {
		return gitlet.ReadAnswer{}, fmt.Errorf("a landing's walk read no repository's pushes below a position, "+
			"newest first and a page at a time: %+v", q)
	}
	var below []gitlet.IntroducedRow
	for _, row := range e.pushes {
		if row.Position.Packed() < in.Before.Packed() {
			below = append(below, row)
		}
	}
	slices.SortStableFunc(below, func(a, b gitlet.IntroducedRow) int {
		if c := cmp.Compare(b.Position.Packed(), a.Position.Packed()); c != 0 {
			return c
		}
		return strings.Compare(b.PushID, a.PushID)
	})
	from := 0
	if q.Cursor != "" {
		from = slices.IndexFunc(below, func(r gitlet.IntroducedRow) bool { return r.PushID == q.Cursor }) + 1
	}
	page := below[from:min(len(below), from+q.Limit)]
	out := gitlet.ReadAnswer{Introduced: slices.Clone(page), Position: e.at}
	if from+len(page) < len(below) {
		out.Next = page[len(page)-1].PushID
	}
	return out, nil
}

func (e *estate) GitRead(_ context.Context, q gitlet.GitQuery) (gitlet.GitAnswer, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := q.Validate(); err != nil {
		return gitlet.GitAnswer{}, err
	}
	e.factsRead[q.RevisionFacts.Head]++
	f, ok := e.facts[q.RevisionFacts.Head]
	if !ok || f.Target != q.RevisionFacts.Target {
		return gitlet.GitAnswer{}, gitlet.ErrObjectNotFound
	}
	return gitlet.GitAnswer{Kind: gitlet.GitRevisionFacts, RepoID: q.RepoID, Position: e.at, RevisionFacts: &f}, nil
}

func (e *estate) Converse(_ context.Context, w gitlet.ConversationWrite) (gitlet.ConversationResult, error) {
	if err := w.Validate(); err != nil {
		return gitlet.ConversationResult{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case w.Revise != nil:
		e.revises = append(e.revises, *w.Revise)
		e.done(w.Revise.RepoID, w.Revise.ChangeID, func(d *gitlet.ChangeDue) { d.FactsDue = false })
		if !e.revisedAt.IsZero() {
			return gitlet.ConversationResult{Outcome: statelog.OutcomeApplied, Position: e.revisedAt}, nil
		}
	case w.Mergeability != nil:
		// A merge records the readiness beside it, a refresh alone: either
		// leaves the change owing no refresh.
		e.merges = append(e.merges, *w.Mergeability)
		if e.recording != nil {
			e.recording()
		}
		e.done(w.Mergeability.RepoID, w.Mergeability.ChangeID, func(d *gitlet.ChangeDue) {
			d.MergeDue, d.RefreshDue = false, false
		})
	case w.Asks != nil:
		e.settles = append(e.settles, *w.Asks)
		e.unsettle[w.Asks.RepoID] = slices.DeleteFunc(e.unsettle[w.Asks.RepoID], func(d gitlet.AskDue) bool {
			return d.ChangeID == w.Asks.ChangeID
		})
		select {
		case e.changed <- struct{}{}:
		default:
		}
	}
	return gitlet.ConversationResult{Outcome: statelog.OutcomeApplied, Position: e.at}, nil
}

// Checks is no write these cases' changes owe: they owe no check work.
func (e *estate) Checks(context.Context, gitlet.ChecksRequest) (gitlet.ChecksResult, error) {
	return gitlet.ChecksResult{}, errors.New("a case owing no check work was asked a write of its checks")
}

// Queue is no write these cases' targets owe: they owe no tip work.
func (e *estate) Queue(context.Context, gitlet.QueueProposal) (gitlet.QueueResult, error) {
	return gitlet.QueueResult{}, errors.New("a case owing no tip work was asked a green or a broken record")
}

func (e *estate) Change(_ context.Context, p gitlet.ChangeProposal) (gitlet.ChangeResult, error) {
	if err := p.Validate(); err != nil {
		return gitlet.ChangeResult{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.landings = append(e.landings, *p.Landed)
	switch {
	case e.landErr != nil:
		return gitlet.ChangeResult{}, e.landErr
	case e.landRefusal != "":
		return gitlet.ChangeResult{Refusal: e.landRefusal, Detail: "the case's committer decided nothing"}, nil
	}
	e.done(p.Landed.RepoID, p.Landed.ChangeID, func(d *gitlet.ChangeDue) { d.FactsDue, d.MergeDue = false, false })
	return gitlet.ChangeResult{Outcome: statelog.OutcomeApplied, ChangeID: p.Landed.ChangeID, Position: e.at}, nil
}

// done applies settle to change on repo's due list, drops it from the list
// once it owes nothing and from repo's rechecks whatever settled it, as the
// rows would, and signals changed. Under mu.
func (e *estate) done(repo, change string, settle func(*gitlet.ChangeDue)) {
	for i := range e.due[repo] {
		if e.due[repo][i].ID == change {
			settle(&e.due[repo][i])
		}
	}
	e.due[repo] = slices.DeleteFunc(e.due[repo], func(d gitlet.ChangeDue) bool {
		return d.ID == change && !d.FactsDue && !d.MergeDue && !d.RefreshDue
	})
	e.rechecks[repo] = slices.DeleteFunc(e.rechecks[repo], func(d gitlet.ChangeDue) bool { return d.ID == change })
	select {
	case e.changed <- struct{}{}:
	default:
	}
}

// waitReads waits until the loop has read its listing at least n times (its
// first pass counts), or fails t.
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
		if time.Now().After(deadline) { //fixedwait:measure a poll's own deadline, whose firing only fails the case
			t.Fatalf("the loop read its listing %d times in 10s, want %d", read, n)
		}
		time.Sleep(5 * time.Millisecond) //fixedwait:liveness
	}
}

// wrote is every revise and landing asked so far.
func (e *estate) wrote() ([]gitlet.Revise, []gitlet.ChangeLanded) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.revises), slices.Clone(e.landings)
}

// listings is every change-work listing asked so far, in order.
func (e *estate) listings() []gitlet.ChangesDueQuery {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.asked)
}

// noChart is a home holding no chart: its listings name none, so no change is
// due a refresh for one.
func noChart() int64 { return 0 }

// merged is every mergeability asked so far.
func (e *estate) merged() []gitlet.Mergeability {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.merges)
}

// waitDone waits until repo's due list and rechecks are empty, or fails t.
func (e *estate) waitDone(t *testing.T, repo string, within time.Duration) {
	t.Helper()
	deadline := time.After(within) //fixedwait:liveness
	for {
		e.mu.Lock()
		left := len(e.due[repo]) + len(e.rechecks[repo])
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

// waitMerged waits until n mergeabilities have been asked, or fails t.
func (e *estate) waitMerged(t *testing.T, n int, within time.Duration) []gitlet.Mergeability {
	t.Helper()
	deadline := time.After(within) //fixedwait:liveness
	for {
		if got := e.merged(); len(got) >= n {
			return got
		}
		select {
		case <-e.changed:
		case <-deadline:
			t.Fatalf("the home asked %d mergeabilities in %v, want %d: %+v", len(e.merged()), within, n,
				e.merged())
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

// asNode is placement as node reads it: HomeOf answers node's repositories.
type asNode struct {
	*placement
	node string
}

func (a asNode) HomeOf(repos []string) (map[string]bool, error) {
	return a.placement.HomeOfFor(a.node, repos)
}

// nudges is the code log's watches: it keeps the repositories' and the
// checks' handlers for the case to call, and begins none while failing.
type nudges struct {
	mu      sync.Mutex
	h       *home.Nudged
	checks  *home.Nudged
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
	switch filter {
	case topics.CodeLogPrefix + "." + string(gitlet.KindRepo) + ".>":
		n.h = &h
		close(n.began)
	case topics.CodeLogPrefix + "." + string(gitlet.KindChecks) + ".>":
		n.checks = &h
	default:
		return nil, errors.New("the home watched " + filter + ", neither every repository's subject nor every " +
			"repository's checks")
	}
	return stopped{}, nil
}

// subject tells the loop a record landed on repo's subject at seq.
func (n *nudges) subject(t *testing.T, repo string, seq uint64) {
	t.Helper()
	select {
	case <-n.began:
	case <-time.After(10 * time.Second): //fixedwait:liveness
		t.Fatal("the loop's watch never began")
	}
	n.mu.Lock()
	h := n.h
	n.mu.Unlock()
	h.Subject(topics.CodeLogSubject(string(gitlet.KindRepo), repo), statelog.Position{Stream: topics.CodeLogStream,
		Seq: seq})
}

// gap tells the loop it may have missed records up to seq.
func (n *nudges) gap(t *testing.T, seq uint64) {
	t.Helper()
	select {
	case <-n.began:
	case <-time.After(10 * time.Second): //fixedwait:liveness
		t.Fatal("the loop's watch never began")
	}
	n.mu.Lock()
	h := n.h
	n.mu.Unlock()
	h.Gap(statelog.Position{Stream: topics.CodeLogStream, Seq: seq})
}

type stopped struct{}

func (stopped) Stop() {}

// units is a seat's git-work share, by which a loop sizes its reads.
type units int

func (u units) Units() int { return int(u) }

// loopOn starts node's loop over e, p and n with backstop and its own
// recorder, stopped when the case ends. Its cache and pool are [unmerged]: its
// cases' changes owe their facts alone.
func loopOn(t *testing.T, node string, e *estate, p *placement, n *nudges,
	backstop time.Duration) (*home.Loop, *metrics.Recorder) {

	t.Helper()
	return loopOver(t, home.Options{Chart: noChart, Estate: e, Placement: asNode{placement: p, node: node}, Nudges: n,
		Cache: unmerged{t: t}, Git: unmerged{t: t}, Chains: unqueued{t: t}, Tasks: unqueued{t: t}, NodeID: node,
		Share: units(2)},
		backstop)
}

// loopOver starts a loop over o with backstop and its own recorder, stopped
// when the case ends.
func loopOver(t *testing.T, o home.Options, backstop time.Duration) (*home.Loop, *metrics.Recorder) {
	t.Helper()
	rec, err := metrics.New()
	if err != nil {
		t.Fatal(err)
	}
	o.Metrics = rec
	l, err := home.New(o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	home.SetBackstop(l, backstop)
	l.Start()
	t.Cleanup(func() { l.Stop(context.WithoutCancel(t.Context())) })
	return l, rec
}

// unmerged is the cache and pool of a loop whose cases owe no merge: any use
// of either fails the case.
type unmerged struct{ t testing.TB }

func (u unmerged) Holds(string) bool {
	u.t.Error("a loop whose changes owe no merge asked whether its cache holds a copy")
	return false
}

func (u unmerged) Sync(context.Context, string, statelog.Freshness) (cache.Synced, error) {
	u.t.Error("a loop whose changes owe no merge synced its cache")
	return cache.Synced{}, errors.New("no merge is owed")
}

func (u unmerged) NewScratch(context.Context, cache.Synced, string) (*cache.Scratch, error) {
	u.t.Error("a loop whose changes owe no merge made a scratch")
	return nil, errors.New("no merge is owed")
}

func (u unmerged) Run(context.Context, gitproc.Cmd) (gitproc.Result, error) {
	u.t.Error("a loop whose changes owe no merge ran git")
	return gitproc.Result{}, errors.New("no merge is owed")
}

// changeID is change n of repo's id: a bare UUIDv7 naming both.
func changeID(repo string, n int64) string {
	return repo[:len(repo)-2] + repo[len(repo)-1:] + string(rune('0'+n))
}

// unqueued is the merge queue's seams for cases that queue nothing: building
// a chain or closing a work item fails the case.
type unqueued struct{ t testing.TB }

func (u unqueued) BuildChain(context.Context, gitlet.ChainBuild) (gitlet.ChainResult, error) {
	u.t.Error("a loop whose targets queue nothing built a chain")
	return gitlet.ChainResult{}, errors.New("nothing is queued")
}

func (u unqueued) CloseWorkItem(context.Context, home.TaskClose) error {
	u.t.Error("a loop whose targets queue nothing closed a work item")
	return errors.New("nothing is queued")
}

// oid is a whole object id made of c.
func oid(c string) string { return strings.Repeat(c, 40)[:40] }

// observed is the count and sum of rec's observations of histogram name
// under attrs.
func observed(rec *metrics.Recorder, name string, attrs metrics.Attrs) (uint64, float64) {
	var (
		n   uint64
		sum float64
	)
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
			n, sum = n+s.Count, sum+s.Sum
		}
	}
	return n, sum
}

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
			n += s.Total
		}
	}
	return n
}

// ONLY THE HOME WORKS A REPOSITORY: two nodes' loops over one code log, each
// home to one of two repositories with a change due, work only their own: a
// pass lists both but schedules only what its placement gives it, a nudge for
// the other's repository schedules nothing, and each record names its node.
//
// Mutation: schedule every listed repository whatever its home → node-b
// records repoA's facts too.
func TestOnlyTheHomeWorksARepository(t *testing.T) {
	t.Parallel()
	e := newEstate()
	e.hold(repoA, 1, oid("a"), oid("1"), gitlet.RevisionFactsAnswer{MergeBase: oid("1"), PatchID: oid("e"),
		Touched: []string{"a.go"}})
	e.hold(repoB, 1, oid("b"), oid("2"), gitlet.RevisionFactsAnswer{MergeBase: oid("2"), PatchID: oid("f"),
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
	// A turn is counted as it ends, after its list empties: the revise empties
	// it, then the refresh the facts ask (changes.change) is written. So read
	// the count once the turn ends, never once the list is empty.
	waitTurns(t, recA, 1)
	if got := counted(recA, metrics.CodeHomePasses, metrics.Attrs{"worker": home.WorkerChanges,
		"outcome": "worked"}); got != 1 {
		t.Errorf("node-a counts %d turns worked, want its one repository's", got)
	}
}

// A NUDGE ONLY SHORTENS THE WAIT: with the backstop an hour away, a nudge for a
// record on a repository gets its due change worked at once, the listing read
// at the record's position so the rows hold it; and a backstop pass finds the
// same work from the rows with no nudge: the nudge decides nothing they don't.
//
// Mutation: read the nudged repository stale, not at the record's position →
// its listing names no floor.
func TestANudgeOnlyShortensTheWait(t *testing.T) {
	t.Parallel()
	e := newEstate()
	p := &placement{homes: map[string]string{repoA: "node-a"}}
	n := newNudges()
	l, _ := loopOn(t, "node-a", e, p, n, time.Hour)
	// The first pass, run at Start, found nothing due.
	e.waitReads(t, 1)
	e.hold(repoA, 1, oid("a"), oid("1"), gitlet.RevisionFactsAnswer{MergeBase: oid("1"), PatchID: oid("e"),
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

	e.hold(repoA, 2, oid("c"), oid("1"), gitlet.RevisionFactsAnswer{MergeBase: oid("1"), PatchID: oid("f"),
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

// A REVISION'S FACTS SCHEDULE ITS CHECKS' DISPATCH: a head's facts land on the
// change's subject, which no watch sees, and its checks dispatch only once the
// facts are recorded, so the home schedules the repository's check work at the
// facts' position as they are recorded, with the backstop an hour away and no
// later nudge.
//
// Mutation: drop the change work's word that it recorded facts → the check
// work is read at the nudge's position alone and the head waits for the
// backstop.
func TestARevisionsFactsScheduleItsChecksDispatch(t *testing.T) {
	t.Parallel()
	e := newEstate()
	revised := statelog.Position{Stream: topics.CodeLogStream, Seq: 90}
	e.revisedAt = revised
	p := &placement{homes: map[string]string{repoA: "node-a"}}
	n := newNudges()
	loopOn(t, "node-a", e, p, n, time.Hour)
	e.waitReads(t, 1)
	e.hold(repoA, 1, oid("a"), oid("1"), gitlet.RevisionFactsAnswer{MergeBase: oid("1"), PatchID: oid("e"),
		Touched: []string{"a.go"}})
	n.subject(t, repoA, 57)
	e.waitDone(t, repoA, 10*time.Second)
	deadline := time.Now().Add(10 * time.Second)
	for {
		e.mu.Lock()
		floors := slices.Clone(e.checkFloors)
		e.mu.Unlock()
		if slices.ContainsFunc(floors, func(f statelog.Freshness) bool {
			return f.Level == statelog.ReadSession && slices.Contains(f.Floors, revised)
		}) {
			return
		}
		if time.Now().After(deadline) { //fixedwait:measure a poll's own deadline, whose firing only fails the case
			t.Fatalf("the repository's check work was read at %+v, never at the facts' position %s", floors,
				revised)
		}
		time.Sleep(5 * time.Millisecond) //fixedwait:liveness
	}
}

// A LOST NUDGE IS CAUGHT BY THE BACKSTOP: with no watch running (its consumer
// cannot be made), a due change is worked by the backstop's next pass; and a
// watch's gap (records possibly missed up to a position) asks a pass at once,
// its listing read at that position.
//
// Mutation: run no pass but the first → the change waits for a nudge that
// never comes.
func TestALostNudgeIsCaughtByTheBackstop(t *testing.T) {
	t.Parallel()
	e := newEstate()
	p := &placement{homes: map[string]string{repoA: "node-a", repoB: "node-a"}}
	deaf := newNudges()
	deaf.failing = true
	_, rec := loopOn(t, "node-a", e, p, deaf, 20*time.Millisecond)
	// The first pass found nothing due: only a later pass can find the
	// change.
	e.waitReads(t, 1)
	e.hold(repoA, 1, oid("a"), oid("1"), gitlet.RevisionFactsAnswer{MergeBase: oid("1"), PatchID: oid("e"),
		Touched: []string{"a.go"}})
	e.waitDone(t, repoA, 10*time.Second)

	e2 := newEstate()
	n := newNudges()
	loopOn(t, "node-a", e2, p, n, time.Hour)
	e2.waitReads(t, 1)
	e2.hold(repoB, 1, oid("b"), oid("2"), gitlet.RevisionFactsAnswer{MergeBase: oid("2"), PatchID: oid("f"),
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
	// Counted as the turn ends (TestOnlyTheHomeWorksARepository), and worked
	// however long it ran past its 20 ms bound on a loaded runner
	// (TestATurnWhoseWorkFinishesPastItsBoundDidIt).
	waitTurns(t, rec, 1)
	if got := counted(rec, metrics.CodeHomePasses, metrics.Attrs{"worker": home.WorkerChanges,
		"outcome": "worked"}); got == 0 {
		t.Errorf("the deaf home counts no turn worked")
	}
}

// A WATCH THAT BEGINS AS THE LOOP STOPS IS STOPPED: a watch that returns after
// Stop has taken the watches but before the loop's life ends (the window
// home.BeginStop holds open) is stopped at once, before the next pass reads
// its listing, never stored where nothing would stop it; no further watch
// begins.
//
// Mutation: decide by the life's end rather than by stopped → the watch is
// stored, still running when the pass reads.
func TestAWatchThatBeginsAsTheLoopStopsIsStopped(t *testing.T) {
	t.Parallel()
	e := newEstate()
	n := &slowWatch{entered: make(chan struct{}), release: make(chan struct{})}
	l, _ := loopOver(t, home.Options{Chart: noChart, Estate: e, Placement: asNode{placement: &placement{}, node: "node-a"},
		Nudges: n, Cache: unmerged{t: t}, Git: unmerged{t: t}, Chains: unqueued{t: t}, Tasks: unqueued{t: t},
		Share: units(2), NodeID: "node-a"}, time.Hour)
	select {
	case <-n.entered:
	case <-time.After(10 * time.Second): //fixedwait:liveness
		t.Fatal("the loop never began its watch")
	}
	home.BeginStop(l)
	close(n.release)
	e.waitReads(t, 1)
	n.mu.Lock()
	defer n.mu.Unlock()
	if len(n.begun) != 1 || !n.begun[0].stopped {
		t.Fatalf("the loop began %d watches, the first stopped %t once its pass read; want the one watch "+
			"that began as the loop stopped, stopped", len(n.begun), len(n.begun) > 0 && n.begun[0].stopped)
	}
}

// slowWatch is a code log whose first watch signals entered and returns only
// once released; each watch records whether it was stopped.
type slowWatch struct {
	mu      sync.Mutex
	begun   []*stopWatch
	entered chan struct{}
	release chan struct{}
}

func (n *slowWatch) Watch(context.Context, string, home.Nudged) (home.Watching, error) {
	n.mu.Lock()
	first := len(n.begun) == 0
	w := &stopWatch{n: n}
	n.begun = append(n.begun, w)
	n.mu.Unlock()
	if first {
		close(n.entered)
		<-n.release
	}
	return w, nil
}

// stopWatch is one watch of a slowWatch.
type stopWatch struct {
	n       *slowWatch
	stopped bool
}

func (w *stopWatch) Stop() {
	w.n.mu.Lock()
	defer w.n.mu.Unlock()
	w.stopped = true
}

// assertTurnFailed fails t unless rec counts the change work's one turn
// failed and none worked, and logs hold code_home_turn_failed.
func assertTurnFailed(t *testing.T, rec *metrics.Recorder, logs *lines) {
	t.Helper()
	failed := counted(rec, metrics.CodeHomePasses, metrics.Attrs{"worker": home.WorkerChanges, "outcome": "failed"})
	worked := counted(rec, metrics.CodeHomePasses, metrics.Attrs{"worker": home.WorkerChanges, "outcome": "worked"})
	if failed != 1 || worked != 0 {
		t.Errorf("the turn whose landing decided nothing counted failed=%d worked=%d, want failed=1", failed, worked)
	}
	if !strings.Contains(logs.String(), "code_home_turn_failed") {
		t.Errorf("the turn whose landing decided nothing logged no code_home_turn_failed: %s", logs.String())
	}
}

// lines is a log a case reads while the loop's goroutines write it.
type lines struct {
	mu  sync.Mutex
	buf strings.Builder
}

func (l *lines) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.Write(p)
}

func (l *lines) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buf.String()
}

// A HOME REFRESHES A READINESS WITHOUT A MERGE: a change owing only a refresh
// of its stored readiness (a review, an ask or a findings answer moved what it
// reads) gets a mergeability that merges nothing: revision, head and target
// named, marked a refresh, no merge fields (its writer stamps them from the
// change's row), and no sync, scratch or git run. A change whose facts the
// same turn records, owing no merge, is refreshed in that turn too: no listing
// named the refresh before its facts moved its readiness.
//
// Mutations: merge a change owing a refresh → the stand-ins fail the case;
// refresh nothing after a revise → the second change is never refreshed.
func TestAHomeRefreshesReadinessWithoutAMerge(t *testing.T) {
	t.Parallel()
	e := newEstate()
	reviewed := gitlet.ChangeDue{ID: changeID(repoA, 1), Number: 1, Revision: 2, Head: oid("b"),
		SourceRef: "refs/heads/feature-b", TargetRef: "refs/heads/main", Tip: oid("1"),
		TipPosition: statelog.Position{Stream: topics.CodeLogStream, Seq: 30}, RefreshDue: true}
	e.owe(repoA, reviewed)
	measured := e.hold(repoA, 2, oid("c"), oid("1"), gitlet.RevisionFactsAnswer{MergeBase: oid("1"),
		PatchID: oid("e"), Touched: []string{"c.go"}})
	loopOn(t, "node-a", e, &placement{homes: map[string]string{repoA: "node-a"}}, newNudges(), time.Hour)
	e.waitDone(t, repoA, 10*time.Second)
	if revises, _ := e.wrote(); len(revises) != 1 || revises[0].ChangeID != measured.ID {
		t.Fatalf("the home recorded the facts %+v, want the second change's alone", revises)
	}
	refreshed := map[string]gitlet.Mergeability{}
	for _, m := range e.waitMerged(t, 2, 10*time.Second) {
		refreshed[m.ChangeID] = m
	}
	for _, due := range []gitlet.ChangeDue{reviewed, measured} {
		m, ok := refreshed[due.ID]
		switch {
		case !ok:
			t.Errorf("change %d was never refreshed: %+v", due.Number, refreshed)
		case !m.Refresh || m.Revision != due.Revision || m.HeadOID != due.Head || m.TargetRef != due.TargetRef ||
			m.Node != "node-a":
			t.Errorf("change %d was asked %+v, want a refresh of revision %d at %s into %s by node-a", due.Number,
				m, due.Revision, due.Head, due.TargetRef)
		case m.TargetOID != "" || !m.TargetPosition.IsZero() || m.State != "" || len(m.Conflicts) != 0:
			t.Errorf("change %d's refresh names a merge of its own: %+v", due.Number, m)
		}
	}
	if len(e.merged()) != 2 {
		t.Errorf("the home asked %d mergeabilities, want the two refreshes alone: %+v", len(e.merged()),
			e.merged())
	}
}

// A HOME NAMES THE CHART IT HOLDS, its node's chart activation, in every
// change-work listing, the repositories' and a repository's alike, so a change
// whose readiness an earlier chart judged is listed due a refresh
// (gitlet.ChangesDueQuery.ChartEpoch).
//
// Mutation: leave the chart off the repository's listing → it names none.
func TestAHomeNamesTheChartItHoldsInEveryListing(t *testing.T) {
	t.Parallel()
	const chart = int64(1_762_000_060_000)
	e := newEstate()
	e.owe(repoA, gitlet.ChangeDue{ID: changeID(repoA, 1), Number: 1, Revision: 1, Head: oid("b"),
		SourceRef: "refs/heads/feature-b", TargetRef: "refs/heads/main", Tip: oid("1"),
		TipPosition: statelog.Position{Stream: topics.CodeLogStream, Seq: 30}, RefreshDue: true})
	loopOver(t, home.Options{Chart: func() int64 { return chart }, Estate: e,
		Placement: asNode{placement: &placement{homes: map[string]string{repoA: "node-a"}}, node: "node-a"},
		Nudges:    newNudges(), Cache: unmerged{t: t}, Git: unmerged{t: t}, Chains: unqueued{t: t},
		Tasks: unqueued{t: t}, NodeID: "node-a", Share: units(2)}, time.Hour)
	e.waitDone(t, repoA, 10*time.Second)
	listed := map[string]bool{}
	for _, q := range e.listings() {
		listed[q.RepoID] = true
		if q.ChartEpoch != chart {
			t.Errorf("the listing %+v names chart %d, want the node's, %d", q, q.ChartEpoch, chart)
		}
	}
	if !listed[""] || !listed[repoA] {
		t.Fatalf("the home asked the listings %+v, want the repositories' and repository A's", e.listings())
	}
}

// THE HOME RECORDS THE CLASSES ITS GIT READ: the base and head trees and the
// touched paths' classes a revision's facts read answered ride the revise as
// the Gitlet node read them; the home lexes and judges none of it.
//
// Mutation: leave the classes out of the revise → the facts record none.
func TestTheHomeRecordsTheClassesItsGitRead(t *testing.T) {
	t.Parallel()
	e := newEstate()
	classed := gitlet.RevisionClasses{WhitespaceOnly: []int{1}, CommentOnly: []int{0}, Complete: true}
	e.hold(repoA, 1, oid("a"), oid("1"), gitlet.RevisionFactsAnswer{MergeBase: oid("1"), BaseTree: oid("3"),
		HeadTree: oid("4"), PatchID: oid("e"), Touched: []string{"a.go", "b.go"}, Classes: classed})
	p := &placement{homes: map[string]string{repoA: "node-a"}}
	loopOn(t, "node-a", e, p, newNudges(), time.Hour)
	e.waitDone(t, repoA, 10*time.Second)
	revises, _ := e.wrote()
	if len(revises) != 1 {
		t.Fatalf("the home recorded %d revisions' facts, want its one change's: %+v", len(revises), revises)
	}
	if r := revises[0]; r.BaseTree != oid("3") || r.HeadTree != oid("4") || !reflect.DeepEqual(r.Classes, classed) {
		t.Errorf("the facts recorded trees %s and %s and classes %+v, want the read's %s, %s and %+v", r.BaseTree,
			r.HeadTree, r.Classes, oid("3"), oid("4"), classed)
	}
}

// THE HOME MATCHES THE HUMAN PATHS ITS TARGET IS HELD TO: beside the path
// owners, it matches the touched paths, classed as its git read classed them,
// against the human paths the listing's gates hold the target to; the revise
// carries which hold the revision and which their unless waived, by place,
// with the digest of the gates' human paths matched, so no judgment matches
// again.
//
// Mutation: leave the match out of the revise → no human path holds the
// revision.
func TestTheHomeMatchesTheHumanPathsItsTargetIsHeldTo(t *testing.T) {
	t.Parallel()
	e := newEstate()
	e.hold(repoA, 1, oid("a"), oid("1"), gitlet.RevisionFactsAnswer{MergeBase: oid("1"), BaseTree: oid("3"),
		HeadTree: oid("4"), PatchID: oid("e"), Touched: []string{"deploy/app.go", "docs/guide.go"},
		Classes: gitlet.RevisionClasses{CommentOnly: []int{1}, Complete: true}})
	e.gates[repoA] = []gitlet.RuleGate{{Pattern: "refs/heads/main", Gate: gitlet.MergeGate{
		MergeBy: gitlet.MergeByWriters, Approvals: gitlet.ApprovalsNone, Landing: gitlet.Landing{Depth: 4, Batch: 4},
		DeleteSource: gitlet.DeleteSeats, HumanPaths: []gitlet.HumanPath{{Glob: "deploy/**"},
			{Glob: "docs/**", Unless: []gitlet.ChangeClass{gitlet.ChangeClassCommentOnly}}}}}}
	p := &placement{homes: map[string]string{repoA: "node-a"}}
	loopOn(t, "node-a", e, p, newNudges(), time.Hour)
	e.waitDone(t, repoA, 10*time.Second)
	revises, _ := e.wrote()
	if len(revises) != 1 {
		t.Fatalf("the home recorded %d revisions' facts, want its one change's: %+v", len(revises), revises)
	}
	want := gitlet.HumanPathMatch{Held: []int{0}, Waived: []int{1}}
	if r := revises[0]; !reflect.DeepEqual(r.HumanPaths, want) ||
		r.HumanPathsDigest != gitlet.HumanPathsDigest(e.gates[repoA]) {
		t.Errorf("the facts recorded the human paths %+v under %s, want %+v under the gates' %s", r.HumanPaths,
			r.HumanPathsDigest, want, gitlet.HumanPathsDigest(e.gates[repoA]))
	}
}

// A HOME SETTLES THE ASKS ITS LISTING NAMES: a repository whose only due work
// is review asks out of step with a change's lifecycle (an ended change's
// still waiting, a reopened change's left ended) is listed, and its home asks
// the change writer to settle each, naming only the change and the state its
// asks move to, no asker (the home's own write): the writer stamps the asks
// it moves from its own snapshot.
//
// Mutations: drop the settling from Work → none is asked and the repository
// stays due; flip the listing's state → the settlings name the wrong states.
func TestAHomeSettlesTheAsksItsListingNames(t *testing.T) {
	t.Parallel()
	e := newEstate()
	ended := gitlet.AskDue{ChangeID: changeID(repoA, 1), State: gitlet.ReviewerEnded}
	reopened := gitlet.AskDue{ChangeID: changeID(repoA, 2), State: gitlet.ReviewerRequested}
	e.owesSettling(repoA, ended, reopened)
	loopOn(t, "node-a", e, &placement{homes: map[string]string{repoA: "node-a"}}, newNudges(), time.Hour)
	deadline := time.After(10 * time.Second) //fixedwait:liveness
	for len(e.settled()) < 2 {
		select {
		case <-e.changed:
		case <-deadline:
			t.Fatalf("the home asked %d settlings in 10 s, want 2: %+v", len(e.settled()), e.settled())
		}
	}
	want := []gitlet.AskSettle{{RepoID: repoA, ChangeID: ended.ChangeID, State: gitlet.ReviewerEnded},
		{RepoID: repoA, ChangeID: reopened.ChangeID, State: gitlet.ReviewerRequested}}
	got := e.settled()
	for i, w := range want {
		if g := got[i]; g.RepoID != w.RepoID || g.ChangeID != w.ChangeID || g.State != w.State ||
			len(g.Reviewers) != 0 {
			t.Fatalf("the home asked the settlings %+v, want %+v", got, want)
		}
	}
}

// FACTS SHORT FOR TIME ARE RECORDED BY NOBODY: a Gitlet node that ran out of
// its read's git budget before a revision's patch id or classes says so apart
// from a bound's omission. The node's moment decided that, not the commits,
// and a revise takes a revision's facts against a tip once (an id omitted for
// a slow node would carry no approval across the revision for good), so the
// home records nothing and asks again next pass. An id omitted past its bound
// is final, recorded as omitted on the first pass.
//
// Mutations: record non-final facts → changes 1 and 2 are recorded on the
// first pass; record the bound's omission as none → change 3's id is not
// omitted.
func TestFactsShortForTimeAreRecordedByNobody(t *testing.T) {
	t.Parallel()
	e := newEstate()
	touched := []string{"a.go"}
	e.hold(repoA, 1, oid("a"), oid("1"), gitlet.RevisionFactsAnswer{MergeBase: oid("1"), Touched: touched,
		PatchIDOmission: gitlet.OmittedOutOfTime})
	e.hold(repoA, 2, oid("b"), oid("1"), gitlet.RevisionFactsAnswer{MergeBase: oid("1"), Touched: touched,
		PatchID: oid("e"), ClassesOmission: gitlet.OmittedOutOfTime})
	e.hold(repoA, 3, oid("c"), oid("1"), gitlet.RevisionFactsAnswer{MergeBase: oid("1"), Touched: touched,
		PatchIDOmission: gitlet.OmittedPastBound, Classes: gitlet.RevisionClasses{Complete: true}})
	p := &placement{homes: map[string]string{repoA: "node-a"}}
	loopOn(t, "node-a", e, p, newNudges(), 20*time.Millisecond)

	deadline := time.Now().Add(10 * time.Second)
	for {
		e.mu.Lock()
		again := e.factsRead[oid("a")] >= 2 && e.factsRead[oid("b")] >= 2
		e.mu.Unlock()
		if again {
			break
		}
		if time.Now().After(deadline) { //fixedwait:measure a poll's own deadline, whose firing only fails the case
			t.Fatal("the home never asked again for the facts its Gitlet node left short for time")
		}
		time.Sleep(10 * time.Millisecond) //fixedwait:liveness
	}
	revises, _ := e.wrote()
	if len(revises) != 1 || revises[0].ChangeID != changeID(repoA, 3) || !revises[0].PatchIDOmitted {
		t.Fatalf("the home recorded %+v, want change 3's facts alone, its id omitted past its bound", revises)
	}

	// Whole at last: the next pass records them as they are.
	e.mu.Lock()
	for _, head := range []string{oid("a"), oid("b")} {
		f := e.facts[head]
		f.PatchID, f.PatchIDOmission, f.ClassesOmission = oid("e"), "", ""
		f.Classes = gitlet.RevisionClasses{Complete: true}
		e.facts[head] = f
	}
	e.mu.Unlock()
	e.waitDone(t, repoA, 10*time.Second)
	revises, _ = e.wrote()
	if len(revises) != 3 {
		t.Fatalf("the home recorded %d revisions' facts, want the three changes': %+v", len(revises), revises)
	}
	for _, r := range revises[1:] {
		if r.PatchIDOmitted || r.PatchID != oid("e") || !r.Classes.Complete {
			t.Errorf("change %s's facts were recorded as %+v, want its id and its classes whole", r.ChangeID, r)
		}
	}
}

// THE HOME IMPORTS NO QUERY, COMPACT, HOST, CHECK RUNNER OR MIRRORS, and none
// of the six imports another: each reaches the others' work through the
// estate's router (the check runner's sweep and the mirrors reach the home's
// loop through its Worker interface), so a step is the same whichever node
// answers it and none calls another's in process past the router.
func TestTheHomeImportsNoQueryCompactHostCheckRunnerOrMirrors(t *testing.T) {
	t.Parallel()
	const module = "github.com/crewlet/crewlet/"
	six := []string{"internal/gitlet/query", "internal/gitlet/compact", "internal/gitlet/host", "internal/gitlet/home",
		"internal/gitlet/checkrun", "internal/gitlet/mirror"}
	root := sourcetree.Root(t)
	for _, pkg := range six {
		p, err := build.Default.ImportDir(filepath.Join(root, filepath.FromSlash(pkg)), 0)
		if err != nil {
			t.Fatalf("read %s: %v", pkg, err)
		}
		for _, other := range six {
			if other != pkg && slices.Contains(p.Imports, module+other) {
				t.Errorf("%s imports %s: the six reach each other's work through the estate's router", pkg, other)
			}
		}
	}
}
