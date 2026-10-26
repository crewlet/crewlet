package query_test

import (
	"context"
	"errors"
	"fmt"
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
// than taking it for a gone one. A caller that goes away is answered its own
// ending, which nobody listens for.
//
// Mutations: answer a sync, a share or a run the answer's due instant ended
// with the context's error — each its row red.
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
