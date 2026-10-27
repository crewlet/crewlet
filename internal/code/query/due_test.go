package query_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/code/query"
)

// cutPool is a query.Pool whose processes of one git command wait until the
// context they run under ends — a read lane busy past it, or a run that
// outlasts it — and end as gitproc ends a run its caller ended, that
// context's cause wrapped. Every other process runs on the pool beneath.
type cutPool struct {
	query.Pool
	command string
}

func (p cutPool) Run(ctx context.Context, c gitproc.Cmd) (gitproc.Result, error) {
	if c.Args[0] != p.command {
		return p.Pool.Run(ctx, c)
	}
	<-ctx.Done()
	return gitproc.Result{Exit: -1}, fmt.Errorf("gitproc: git %s was ended by its caller: %w", c.Args[0],
		context.Cause(ctx))
}

// A READ WHOSE ANSWER FALLS DUE SAYS SO WHILE ITS ASKER LISTENS. A git node's
// read runs under a context that ends a reply's margin before its asker's
// deadline (code.AnswerBy), and every wait it makes ends there: its copy's
// sync, its unit of the node's share, a check of what it names and its own
// git — each held here past that instant — answer ErrGitUnready naming the
// wait, before the asker's deadline, so the asker hears a busy node rather
// than taking it for a gone one. A run of its own git ended so is NEVER
// answered too large: it had less than its budget, which a node with the time
// would have given it. A caller that goes away is answered its own ending,
// which nobody listens for.
//
// Mutations: answer a sync, a share or a run the answer's due instant ended
// with the context's error — each its row red; answer a run of the read's own
// git the instant ended as too large — its row red.
func TestAReadWhoseAnswerFallsDueSaysSoWhileItsAskerListens(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	x.f.Declare(t, code.Declared{Name: "beta", Settings: codetest.Settings()})
	head := x.commit(map[string]string{"f": "1\n"}, "one")
	x.push("beta", "main")
	warm, cold := x.repo(codetest.RepoName), x.repo("beta")
	node := x.f.Nodes()[0]

	// A SHARE OF ONE UNIT, HELD: a take waits code.GitShareWait, ten times
	// what the asker's attempt leaves the read below.
	held := code.NewGitShare(node.ID(), 2, nil)
	unit, err := held.Take(t.Context(), code.SeatWorkRead)
	if err != nil {
		t.Fatal(err)
	}
	defer unit()

	for name, tc := range map[string]struct {
		o    query.Options
		repo string
		want code.UnreadyReason
	}{
		"its copy's sync": {query.Options{Cache: &holdingCache{Cache: node.Cache(), repo: cold,
			entered: make(chan struct{}), released: make(chan struct{})}}, cold, code.UnreadySyncing},
		"its unit of the share":       {query.Options{Share: held}, warm, code.UnreadyShare},
		"a check of what it names":    {query.Options{Git: cutPool{Pool: node.Pool(), command: "cat-file"}}, warm, code.UnreadyShare},
		"its own git, run or waiting": {query.Options{Git: cutPool{Pool: node.Pool(), command: "ls-tree"}}, warm, code.UnreadyShare},
	} {
		s := x.server(0, tc.o)
		query.SetSyncWait(s, time.Minute)
		// THE ASKER LEAVES THE READ A FIFTH OF A SECOND past the reply's
		// margin: what it is held on outlasts that, every wait but the one
		// a case holds being far shorter.
		deadline := time.Now().Add(code.ReplyMargin + 200*time.Millisecond)
		ctx, cancel := code.AnswerBy(t.Context(), deadline)
		_, err := s.Read(ctx, treeQ(tc.repo, code.TreeQuery{Rev: head}))
		answered := time.Now()
		cancel()
		switch {
		case errors.Is(err, code.ErrSliceTooLarge):
			t.Errorf("%s: a read the answer's due instant ended there = %v, want it unready: it had less than "+
				"its budget, and a node with the time would answer it", name, err)
		case !unready(err, tc.want):
			t.Errorf("%s: a read the answer's due instant ended there = %v, want ErrGitUnready{%s}", name, err,
				tc.want)
		case !answered.Before(deadline):
			t.Errorf("%s: the read answered at %v, past its asker's deadline %v: nobody heard it", name,
				answered, deadline)
		}
	}

	// A CALLER THAT GOES AWAY is answered its own ending, not unready.
	s := x.server(0, query.Options{Cache: &holdingCache{Cache: node.Cache(), repo: cold,
		entered: make(chan struct{}), released: make(chan struct{})}})
	query.SetSyncWait(s, time.Minute)
	ctx, leave := context.WithCancel(t.Context())
	time.AfterFunc(50*time.Millisecond, leave)
	if _, err := s.Read(ctx, treeQ(cold, code.TreeQuery{Rev: head})); !errors.Is(err, context.Canceled) {
		t.Errorf("a read whose caller went away = %v, want the caller's own ending", err)
	}
}

// budgetPool is a query.Pool that records the deadline every process is
// given, by its git command, and runs it on the pool beneath — reporting a
// run of a command in took as having taken that long, and answering one of
// ends as gitproc answers a run killed at its deadline.
type budgetPool struct {
	query.Pool
	took map[string]time.Duration
	ends string

	mu    sync.Mutex
	given map[string][]time.Duration
}

func (p *budgetPool) Run(ctx context.Context, c gitproc.Cmd) (gitproc.Result, error) {
	command := c.Args[0]
	p.mu.Lock()
	if p.given == nil {
		p.given = map[string][]time.Duration{}
	}
	p.given[command] = append(p.given[command], c.Deadline)
	p.mu.Unlock()
	if command == p.ends {
		return gitproc.Result{Exit: -1, Took: c.Deadline}, fmt.Errorf("gitproc: git %s ran past its %v deadline "+
			"and was killed: %w", command, c.Deadline, gitproc.ErrDeadline)
	}
	res, err := p.Pool.Run(ctx, c)
	if took, ok := p.took[command]; ok {
		res.Took = took
	}
	return res, err
}

// deadlines is every deadline command's processes were given, in order.
func (p *budgetPool) deadlines(command string) []time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.given[command])
}

// A READ'S OWN GIT HAS ONE BUDGET ITS PROCESSES SHARE. A comparison runs three
// processes — the merge base, the count of each side, the stat — and they
// share the run time one listing is given (gitproc.ReadDeadline): each is
// handed what the ones before it left, so a merge base that ran six seconds
// leaves the count four. The checks that find what the read names come before
// it, each to its own bound and charged nothing. A process handed the last of
// it and ended there is the read too large to answer, naming the narrowing —
// another node would take as long. And a run is handed what is left of the
// budget whatever its asker's attempt leaves: the instant its answer falls due
// ends it through its context, unready, and never through a deadline of its
// own cut to that instant, which would answer it too large.
//
// Every read here hides the copy's memo (memoless): a page the memo holds is
// answered with no git at all, so the comparison asked a second time would be
// the first one's page, whatever its count's process would have met — and
// what this case holds is what each process of a read's own git is given.
//
// Mutations: give every process the whole budget — the count's deadline ten
// seconds, red; charge the checks — the merge base's under ten, red; cut a
// run's deadline to its asker's attempt — the listing's under ten, red.
func TestAReadsOwnGitHasOneBudgetItsProcessesShare(t *testing.T) {
	t.Parallel()
	x := newFixture(t, codetest.SoloOptions{}, codetest.Settings())
	first := x.commit(map[string]string{"f": "1\n"}, "one")
	second := x.commit(map[string]string{"f": "2\n"}, "two")
	repo := x.repo(codetest.RepoName)
	node := x.f.Nodes()[0]
	unkept := memoless{node.Cache()}

	shared := &budgetPool{Pool: node.Pool(),
		took: map[string]time.Duration{"cat-file": 3 * time.Second, "merge-base": 6 * time.Second}}
	s := x.server(0, query.Options{Git: shared, Cache: unkept})
	if _, err := s.Read(t.Context(), compareQ(repo, first, second)); err != nil {
		t.Fatalf("a comparison = %v", err)
	}
	if got := shared.deadlines("cat-file"); len(got) != 1 || got[0] != gitproc.ReadDeadline {
		t.Errorf("the check of what the comparison names was given %v, want its own whole bound %v", got,
			gitproc.ReadDeadline)
	}
	if got := shared.deadlines("merge-base"); len(got) != 1 || got[0] != gitproc.ReadDeadline {
		t.Errorf("the merge base, the read's own git's first process, was given %v, want the whole budget %v: "+
			"the check before it is charged nothing", got, gitproc.ReadDeadline)
	}
	if got := shared.deadlines("rev-list"); len(got) != 1 || got[0] != gitproc.ReadDeadline-6*time.Second {
		t.Errorf("the count of each side, after a merge base that ran six seconds, was given %v, want the %v "+
			"it left", got, gitproc.ReadDeadline-6*time.Second)
	}
	if got := shared.deadlines("diff-tree"); len(got) != 1 || got[0] <= 0 ||
		got[0] >= gitproc.ReadDeadline-6*time.Second {
		t.Errorf("the stat, after both walks, was given %v, want what the two left of the budget", got)
	}

	spent := &budgetPool{Pool: node.Pool(), ends: "rev-list"}
	_, err := x.server(0, query.Options{Git: spent, Cache: unkept}).Read(t.Context(), compareQ(repo, first, second))
	if !errors.Is(err, code.ErrSliceTooLarge) || !errorHas(err, "compare commits closer together") {
		t.Errorf("a comparison whose count ran out the budget = %v, want it too large, naming the narrowing", err)
	}

	// FIVE SECONDS FROM ITS DUE INSTANT: half the budget, so a deadline cut
	// to that instant shows, and far more than a warm listing takes, so the
	// instant never ends this one.
	listing := &budgetPool{Pool: node.Pool()}
	ctx, cancel := code.AnswerBy(t.Context(), time.Now().Add(code.ReplyMargin+5*time.Second))
	defer cancel()
	s = x.server(0, query.Options{Git: listing, Cache: unkept})
	if _, err := s.Read(ctx, treeQ(repo, code.TreeQuery{Rev: second})); err != nil {
		t.Fatalf("a listing five seconds from its answer's due instant = %v", err)
	}
	if got := listing.deadlines("ls-tree"); len(got) != 1 || got[0] != gitproc.ReadDeadline {
		t.Errorf("a listing five seconds from its answer's due instant was given %v, want the whole budget %v: "+
			"the due instant ends a run through its context, unready, never through a deadline that would "+
			"call it too large", got, gitproc.ReadDeadline)
	}
}

// errorHas reports whether err's sentence holds part.
func errorHas(err error, part string) bool {
	return err != nil && strings.Contains(err.Error(), part)
}
