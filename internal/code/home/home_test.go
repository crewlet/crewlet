package home_test

import (
	"context"
	"errors"
	"go/build"
	"log/slog"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/gitproc"
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
// repository's due changes and its rechecks — the changes a moved target asks
// merged again, oldest first, as many as a read asks — answered at the floor a
// read names, and the facts of each head. A revise takes its change's facts
// off the list, a mergeability its merge, and a landing the whole change, as
// the rows' own would; a change owing nothing more leaves the list. And each
// repository's changes whose asks are due a settling, which a settling takes
// off, as its counts would.
type estate struct {
	mu       sync.Mutex
	due      map[string][]code.ChangeDue
	rechecks map[string][]code.ChangeDue
	unsettle map[string][]code.AskDue
	settles  []code.AskSettle
	owners   map[string][]code.PathOwner
	facts    map[string]code.RevisionFactsAnswer
	// factsRead is how many times the facts of each head were read.
	factsRead map[string]int
	at        statelog.Position
	floors    []statelog.Freshness
	asked     []code.ChangesDueQuery
	revises   []code.Revise
	merges    []code.Mergeability
	landings  []code.ChangeLanded
	// landRefusal and landErr are what every landing is answered while
	// either is set — a refusal that decided nothing, or a call that failed —
	// the change left on the list as its rows would leave it.
	landRefusal code.ChangeRefusal
	landErr     error
	changed     chan struct{}
}

func newEstate() *estate {
	return &estate{due: map[string][]code.ChangeDue{}, rechecks: map[string][]code.ChangeDue{},
		unsettle: map[string][]code.AskDue{}, owners: map[string][]code.PathOwner{},
		facts: map[string]code.RevisionFactsAnswer{}, factsRead: map[string]int{},
		changed: make(chan struct{}, 1), at: statelog.Position{Stream: topics.CodeLogStream, Seq: 40}}
}

// hold puts change n of repo on repo's due list, owing its facts alone, its
// head head and its target main at tip, the facts its home reads of it f —
// with the trees a git node reads beside them, where f names none and its
// head is no merged one's.
func (e *estate) hold(repo string, n int64, head, tip string, f code.RevisionFactsAnswer) code.ChangeDue {
	e.mu.Lock()
	defer e.mu.Unlock()
	due := code.ChangeDue{ID: changeID(repo, n), Number: n, Revision: 1, Head: head,
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
func (e *estate) owe(repo string, due code.ChangeDue) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.due[repo] = append(e.due[repo], due)
}

// owesSettling puts dues among repo's changes whose asks are due a settling.
func (e *estate) owesSettling(repo string, dues ...code.AskDue) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.unsettle[repo] = append(e.unsettle[repo], dues...)
}

// settled is every settling of asks the home asked, in the order it asked.
func (e *estate) settled() []code.AskSettle {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.settles)
}

// recheck puts due among repo's rechecks, after every recheck already there:
// the order the rows name them in, the oldest merge first.
func (e *estate) recheck(repo string, due code.ChangeDue) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.rechecks[repo] = append(e.rechecks[repo], due)
}

func (e *estate) Read(_ context.Context, q code.ReadQuery) (code.ReadAnswer, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	switch {
	case q.Kind == code.ReadChecksDue || q.Kind == code.ReadQueueDue:
		// THE CHECK WORK'S AND THE QUEUE'S LISTINGS: these cases hold no
		// change owing either.
		return code.ReadAnswer{Position: e.at}, nil
	case q.Kind != code.ReadChangesDue:
		return code.ReadAnswer{}, errors.New("the home asked a read that is not its listing")
	case q.ChangesDue == nil || q.ChangesDue.RecheckBefore.IsZero():
		return code.ReadAnswer{}, errors.New("the home's listing named no instant its refresh is paced by")
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
		return code.ReadAnswer{DueRepos: slices.Compact(repos), Position: e.at}, nil
	}
	repo := q.ChangesDue.RepoID
	again := e.rechecks[repo]
	again = slices.Clone(again[:min(len(again), q.ChangesDue.Rechecks)])
	return code.ReadAnswer{DueChanges: slices.Clone(e.due[repo]), Rechecks: again, PathOwners: e.owners[repo],
		AskSettles: slices.Clone(e.unsettle[repo]), Position: e.at}, nil
}

func (e *estate) GitRead(_ context.Context, q code.GitQuery) (code.GitAnswer, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := q.Validate(); err != nil {
		return code.GitAnswer{}, err
	}
	e.factsRead[q.RevisionFacts.Head]++
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
	switch {
	case w.Revise != nil:
		e.revises = append(e.revises, *w.Revise)
		e.done(w.Revise.RepoID, w.Revise.ChangeID, func(d *code.ChangeDue) { d.FactsDue = false })
	case w.Mergeability != nil:
		// A MERGE RECORDS THE READINESS beside it, and a refresh records it
		// alone: either leaves the change owing no refresh.
		e.merges = append(e.merges, *w.Mergeability)
		e.done(w.Mergeability.RepoID, w.Mergeability.ChangeID, func(d *code.ChangeDue) {
			d.MergeDue, d.RefreshDue = false, false
		})
	case w.Asks != nil:
		e.settles = append(e.settles, *w.Asks)
		e.unsettle[w.Asks.RepoID] = slices.DeleteFunc(e.unsettle[w.Asks.RepoID], func(d code.AskDue) bool {
			return d.ChangeID == w.Asks.ChangeID
		})
		select {
		case e.changed <- struct{}{}:
		default:
		}
	}
	return code.ConversationResult{Outcome: statelog.OutcomeApplied, Position: e.at}, nil
}

// Checks is no write these cases' changes owe: they owe no check work.
func (e *estate) Checks(context.Context, code.ChecksRequest) (code.ChecksResult, error) {
	return code.ChecksResult{}, errors.New("a case owing no check work was asked a write of its checks")
}

// Queue is no write these cases' targets owe: they owe no tip work.
func (e *estate) Queue(context.Context, code.QueueProposal) (code.QueueResult, error) {
	return code.QueueResult{}, errors.New("a case owing no tip work was asked a green or a broken record")
}

func (e *estate) Change(_ context.Context, p code.ChangeProposal) (code.ChangeResult, error) {
	if err := p.Validate(); err != nil {
		return code.ChangeResult{}, err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.landings = append(e.landings, *p.Landed)
	switch {
	case e.landErr != nil:
		return code.ChangeResult{}, e.landErr
	case e.landRefusal != "":
		return code.ChangeResult{Refusal: e.landRefusal, Detail: "the case's committer decided nothing"}, nil
	}
	e.done(p.Landed.RepoID, p.Landed.ChangeID, func(d *code.ChangeDue) { d.FactsDue, d.MergeDue = false, false })
	return code.ChangeResult{Outcome: statelog.OutcomeApplied, ChangeID: p.Landed.ChangeID, Position: e.at}, nil
}

// done takes what a record settled off change on repo's due list — the change
// leaving it where it owes nothing more, and its rechecks for whatever record
// settled it — as its record's rows would, and says so. Under mu.
func (e *estate) done(repo, change string, settle func(*code.ChangeDue)) {
	for i := range e.due[repo] {
		if e.due[repo][i].ID == change {
			settle(&e.due[repo][i])
		}
	}
	e.due[repo] = slices.DeleteFunc(e.due[repo], func(d code.ChangeDue) bool {
		return d.ID == change && !d.FactsDue && !d.MergeDue && !d.RefreshDue
	})
	e.rechecks[repo] = slices.DeleteFunc(e.rechecks[repo], func(d code.ChangeDue) bool { return d.ID == change })
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

// merged is every mergeability asked so far.
func (e *estate) merged() []code.Mergeability {
	e.mu.Lock()
	defer e.mu.Unlock()
	return slices.Clone(e.merges)
}

// waitDone waits until repo's due list and its rechecks are empty, or fails t
// at the deadline.
func (e *estate) waitDone(t *testing.T, repo string, within time.Duration) {
	t.Helper()
	deadline := time.After(within)
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

// waitMerged waits until n mergeabilities have been asked, or fails t at the
// deadline.
func (e *estate) waitMerged(t *testing.T, n int, within time.Duration) []code.Mergeability {
	t.Helper()
	deadline := time.After(within)
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

// asNode is p as node reads it: HomeOf answers the repositories node is home
// to.
type asNode struct {
	*placement
	node string
}

func (a asNode) HomeOf(repos []string) (map[string]bool, error) {
	return a.placement.HomeOfFor(a.node, repos)
}

// nudges is the code log's watches, the repositories' subjects' handler kept
// for the case to call, and the checks subjects' — or watches that do not
// begin, while failing is set.
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
	case topics.CodeLogPrefix + "." + string(code.KindRepo) + ".>":
		n.h = &h
		close(n.began)
	case topics.CodeLogPrefix + "." + string(code.KindChecks) + ".>":
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

// units is a seat git-work share of so many units, as a loop sizes its own
// reads by it.
type units int

func (u units) Units() int { return int(u) }

// loopOn is node's loop over e, p and n, its backstop backstop, started, with
// a recorder of its own; stopped when the case ends. Its cache and pool are
// stand-ins no merge reaches ([unmerged]): every change its cases hold owes
// its facts alone.
func loopOn(t *testing.T, node string, e *estate, p *placement, n *nudges,
	backstop time.Duration) (*home.Loop, *metrics.Recorder) {

	t.Helper()
	return loopOver(t, home.Options{Estate: e, Placement: asNode{placement: p, node: node}, Nudges: n,
		Cache: unmerged{t: t}, Git: unmerged{t: t}, Chains: unqueued{t: t}, Tasks: unqueued{t: t}, NodeID: node,
		Share: units(2)},
		backstop)
}

// loopOver is a loop over o, with a recorder of its own, its backstop
// backstop, started; stopped when the case ends.
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

// unmerged is the cache and the pool of a loop whose cases hold no change that
// owes a merge: whatever reaches either is the case's failure, since nothing
// the case holds asks one.
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

// unqueued is a home's merge queue seams where its cases queue nothing: a
// chain built or a work item closed is a failure of the case.
type unqueued struct{ t testing.TB }

func (u unqueued) BuildChain(context.Context, code.ChainBuild) (code.ChainResult, error) {
	u.t.Error("a loop whose targets queue nothing built a chain")
	return code.ChainResult{}, errors.New("nothing is queued")
}

func (u unqueued) CloseWorkItem(context.Context, home.TaskClose) error {
	u.t.Error("a loop whose targets queue nothing closed a work item")
	return errors.New("nothing is queued")
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

// A LANDING THAT DECIDED NOTHING FAILS THE TURN: a change whose head its
// target holds, owing its facts and its merge, whose landing the committer
// answers busy — or whose Change call fails before any answer — ends its
// turn FAILED, counted so and logged code_home_turn_failed, and that turn does
// no more work on the change: no merge is asked of it (the loop's cache and
// pool are stand-ins any use of which fails the case), and the landing is
// asked once. A repository whose landings keep failing reads as failing,
// never as healthy.
//
// Mutation: drop the landing's error on the facts branch of the change's step
// (answer `true, nil` once a landing was asked), and both turns count worked.
func TestALandingThatDecidedNothingFailsTheTurn(t *testing.T) {
	t.Parallel()
	for name, refuse := range map[string]func(*estate){
		"the committer answers busy": func(e *estate) { e.landRefusal = code.ChangeRefusalBusy },
		"the Change call fails": func(e *estate) {
			e.landErr = errors.New("the router reached no committer")
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			e := newEstate()
			due := e.hold(repoA, 1, oid("a"), oid("1"), code.RevisionFactsAnswer{MergeBase: oid("a"),
				HeadInTarget: true, Touched: []string{}})
			e.mu.Lock()
			e.due[repoA][0].MergeDue = true
			refuse(e)
			e.mu.Unlock()
			logs := &lines{}
			p := &placement{homes: map[string]string{repoA: "node-a"}}
			_, rec := loopOver(t, home.Options{Estate: e, Placement: asNode{placement: p, node: "node-a"},
				Nudges: newNudges(), Cache: unmerged{t: t}, Git: unmerged{t: t}, Chains: unqueued{t: t},
				Tasks: unqueued{t: t}, NodeID: "node-a", Logger: slog.New(slog.NewTextHandler(logs, nil))},
				time.Hour)
			waitTurns(t, rec, 1)
			assertTurnFailed(t, rec, logs)
			if _, landings := e.wrote(); len(landings) != 1 || landings[0].ChangeID != due.ID {
				t.Errorf("the turn asked the landings %+v, want change 1's once", landings)
			}
			if merged := e.merged(); len(merged) != 0 {
				t.Errorf("the turn recorded %+v of a change it found landed, want nothing more of it", merged)
			}
		})
	}
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
// of its stored readiness — a review, an ask, an answer to its findings moved
// what it reads — is answered by a mergeability that MERGES NOTHING: its
// revision, head and target named, marked a refresh, and nothing of a merge,
// which its writer stamps from the change's row — with no copy synced, no
// scratch made and no git run, the loop's cache and pool being stand-ins any
// use of which fails the case. And a change whose facts the same turn
// records, owing no merge, is refreshed in that turn: its facts moved what its
// stored readiness read, and no listing named the refresh before they did.
//
// Mutations: merge a change owing a refresh → the stand-ins fail the case;
// refresh nothing after a revise → web's second change is never refreshed.
func TestAHomeRefreshesReadinessWithoutAMerge(t *testing.T) {
	t.Parallel()
	e := newEstate()
	reviewed := code.ChangeDue{ID: changeID(repoA, 1), Number: 1, Revision: 2, Head: oid("b"),
		SourceRef: "refs/heads/feature-b", TargetRef: "refs/heads/main", Tip: oid("1"),
		TipPosition: statelog.Position{Stream: topics.CodeLogStream, Seq: 30}, RefreshDue: true}
	e.owe(repoA, reviewed)
	measured := e.hold(repoA, 2, oid("c"), oid("1"), code.RevisionFactsAnswer{MergeBase: oid("1"),
		PatchID: oid("e"), Touched: []string{"c.go"}})
	loopOn(t, "node-a", e, &placement{homes: map[string]string{repoA: "node-a"}}, newNudges(), time.Hour)
	e.waitDone(t, repoA, 10*time.Second)
	if revises, _ := e.wrote(); len(revises) != 1 || revises[0].ChangeID != measured.ID {
		t.Fatalf("the home recorded the facts %+v, want the second change's alone", revises)
	}
	refreshed := map[string]code.Mergeability{}
	for _, m := range e.waitMerged(t, 2, 10*time.Second) {
		refreshed[m.ChangeID] = m
	}
	for _, due := range []code.ChangeDue{reviewed, measured} {
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

// THE HOME RECORDS THE CLASSES ITS GIT READ: what a revision's facts read
// answered of its code — the trees of its base and its head, and its touched
// paths' classes — rides the revise the home asks the change writer for, as
// the git node read it: the home lexes nothing and judges nothing of it.
//
// Mutation: leave the classes out of the revise — the facts record none, red.
func TestTheHomeRecordsTheClassesItsGitRead(t *testing.T) {
	t.Parallel()
	e := newEstate()
	classed := code.RevisionClasses{WhitespaceOnly: []int{1}, CommentOnly: []int{0}, Complete: true}
	e.hold(repoA, 1, oid("a"), oid("1"), code.RevisionFactsAnswer{MergeBase: oid("1"), BaseTree: oid("3"),
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

// A HOME SETTLES THE ASKS ITS LISTING NAMES: a repository whose only due work
// is a change's asks for review that no longer match its lifecycle — an ended
// change's still waiting, a reopened change's its end left — is listed, and
// its home asks the change's writer to settle each, naming the change and the
// state its asks move to and no asker (the home's own write), nothing more:
// the writer stamps the asks it moves from its own snapshot.
//
// Mutations: drop the settling from Work → no settling is asked, the
// repository staying due; send the listing's state the other way → the
// settlings name the wrong states.
func TestAHomeSettlesTheAsksItsListingNames(t *testing.T) {
	t.Parallel()
	e := newEstate()
	ended := code.AskDue{ChangeID: changeID(repoA, 1), State: code.ReviewerEnded}
	reopened := code.AskDue{ChangeID: changeID(repoA, 2), State: code.ReviewerRequested}
	e.owesSettling(repoA, ended, reopened)
	loopOn(t, "node-a", e, &placement{homes: map[string]string{repoA: "node-a"}}, newNudges(), time.Hour)
	deadline := time.After(10 * time.Second)
	for len(e.settled()) < 2 {
		select {
		case <-e.changed:
		case <-deadline:
			t.Fatalf("the home asked %d settlings in 10 s, want 2: %+v", len(e.settled()), e.settled())
		}
	}
	want := []code.AskSettle{{RepoID: repoA, ChangeID: ended.ChangeID, State: code.ReviewerEnded},
		{RepoID: repoA, ChangeID: reopened.ChangeID, State: code.ReviewerRequested}}
	got := e.settled()
	for i, w := range want {
		if g := got[i]; g.RepoID != w.RepoID || g.ChangeID != w.ChangeID || g.State != w.State ||
			len(g.Reviewers) != 0 {
			t.Fatalf("the home asked the settlings %+v, want %+v", got, want)
		}
	}
}

// FACTS SHORT FOR TIME ARE RECORDED BY NOBODY. A git node that ran its read's
// budget of git out before a revision's patch id, or before its classes, says
// so apart from a bound's omission: the commits did not decide it, the node's
// moment did, so the home records no such facts — a revise takes a revision's
// facts against a tip once, and an id omitted for a slow node would carry no
// approval across the revision for good — and asks again on its next pass,
// recording them once they are whole. An id omitted past its bound is final,
// recorded as omitted on the first pass.
//
// Mutations: record facts that are not final — changes 1's and 2's are
// recorded on the first pass, red; record the bound's omission as none —
// change 3's id is not omitted, red.
func TestFactsShortForTimeAreRecordedByNobody(t *testing.T) {
	t.Parallel()
	e := newEstate()
	touched := []string{"a.go"}
	e.hold(repoA, 1, oid("a"), oid("1"), code.RevisionFactsAnswer{MergeBase: oid("1"), Touched: touched,
		PatchIDOmission: code.OmittedOutOfTime})
	e.hold(repoA, 2, oid("b"), oid("1"), code.RevisionFactsAnswer{MergeBase: oid("1"), Touched: touched,
		PatchID: oid("e"), ClassesOmission: code.OmittedOutOfTime})
	e.hold(repoA, 3, oid("c"), oid("1"), code.RevisionFactsAnswer{MergeBase: oid("1"), Touched: touched,
		PatchIDOmission: code.OmittedPastBound, Classes: code.RevisionClasses{Complete: true}})
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
		if time.Now().After(deadline) {
			t.Fatal("the home never asked again for the facts its git node left short for time")
		}
		time.Sleep(10 * time.Millisecond)
	}
	revises, _ := e.wrote()
	if len(revises) != 1 || revises[0].ChangeID != changeID(repoA, 3) || !revises[0].PatchIDOmitted {
		t.Fatalf("the home recorded %+v, want change 3's facts alone, its id omitted past its bound", revises)
	}

	// WHOLE AT LAST: the next pass records them, as they are.
	e.mu.Lock()
	for _, head := range []string{oid("a"), oid("b")} {
		f := e.facts[head]
		f.PatchID, f.PatchIDOmission, f.ClassesOmission = oid("e"), "", ""
		f.Classes = code.RevisionClasses{Complete: true}
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
// of them imports another or the home: internal/code/query,
// internal/code/compact, internal/code/host, internal/code/home,
// internal/code/checkrun and internal/code/mirror each reach the others' work
// through the estate's router — the check runner's sweep and the mirrors the
// home's loop through the loop's own Worker interface — so a step is the same
// whichever node answers it, and no one of them can call another's in process
// past the router's walk.
func TestTheHomeImportsNoQueryCompactHostCheckRunnerOrMirrors(t *testing.T) {
	t.Parallel()
	const module = "github.com/crewlet/crewlet/"
	six := []string{"internal/code/query", "internal/code/compact", "internal/code/host", "internal/code/home",
		"internal/code/checkrun", "internal/code/mirror"}
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
