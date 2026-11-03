package engine_test

import (
	"context"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	codehome "github.com/crewlet/crewlet/internal/code/home"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// behindQueue is a git node's home change work's code seam and chain builder
// with one merge queue in it — repository rateRepo's main, at depth
// rateDepth and batch rateBatch, one change waiting — asked MergeRateScan
// enqueues within the window: the queue worker builds its train through the
// chain builder, the train's record is answered applied and the queue's next
// reads show the train standing, so the worker times a real round of its own
// while its queue is asked past what any round of seconds can land. Every
// other read and write passes to the node's own seam.
type behindQueue struct {
	codehome.Estate

	mu sync.Mutex
	// train is the train the worker recorded, "" before; recorded counts
	// its candidate records.
	train    string
	recorded int
}

// The queue behindQueue shows.
const (
	rateRepoName = "merge-rate"
	rateDepth    = 2
	rateBatch    = 3
	rateTip      = "1111111111111111111111111111111111111111"
	rateHead     = "2222222222222222222222222222222222222222"
	rateCommit   = "3333333333333333333333333333333333333333"
)

var rateRepo = uuid.NewSHA1(uuid.NameSpaceOID, []byte(rateRepoName)).String()

func (q *behindQueue) Read(ctx context.Context, rq code.ReadQuery) (code.ReadAnswer, error) {
	if rq.Kind != code.ReadQueueDue {
		return q.Estate.Read(ctx, rq)
	}
	at := statelog.Position{Stream: topics.CodeLogStream, Seq: 1}
	if rq.QueueDue == nil || rq.QueueDue.RepoID == "" {
		return code.ReadAnswer{DueRepos: []string{rateRepo}, Position: at}, nil
	}
	if rq.QueueDue.RepoID != rateRepo {
		return code.ReadAnswer{Position: at}, nil
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	entry := code.QueueWorkEntry{EntryID: "entry-1", State: code.QueueWaiting, HeadOID: rateHead,
		Method: code.MergeMethodMerge, EnqueuedBy: code.Principal{Kind: code.PrincipalAgent, Seat: "swe"}}
	due := code.QueueDue{TargetRef: "refs/heads/main", Repo: rateRepoName, Tip: rateTip,
		Depth: rateDepth, Batch: rateBatch, Required: []string{"unit"},
		Enqueued: code.MergeRateScan}
	if q.train != "" {
		entry.State, entry.Train = code.QueueTesting, q.train
		entry.CandidateID = code.QueueCandidateID(q.train, 1)
		due.Train = []code.QueueWorkCandidate{{CandidateID: entry.CandidateID, Train: q.train, Ordinal: 1,
			BaseOID: rateTip, TipOID: rateCommit,
			Commits: []code.QueueWorkCommit{{EntryID: entry.EntryID, Commit: rateCommit}}}}
	}
	due.Entries = []code.QueueWorkEntry{entry}
	return code.ReadAnswer{DueQueue: &due, Position: at}, nil
}

func (q *behindQueue) Queue(ctx context.Context, p code.QueueProposal) (code.QueueResult, error) {
	if p.Candidate == nil || p.Candidate.RepoID != rateRepo {
		return q.Estate.Queue(ctx, p)
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.train = p.Candidate.Train
	q.recorded++
	return code.QueueResult{Outcome: statelog.OutcomeApplied}, nil
}

// BuildChain builds the train's one commit, as the code host's chain builder
// answers a merge it built: the queue's candidate is recorded from it.
func (q *behindQueue) BuildChain(_ context.Context, b code.ChainBuild) (code.ChainResult, error) {
	out := code.ChainResult{Tip: rateCommit, Settle: func(context.Context, bool) error { return nil }}
	for _, e := range b.Entries {
		out.Commits = append(out.Commits, code.ChainCommit{EntryID: e.EntryID, Commit: rateCommit})
	}
	return out, nil
}

// trainRecorded is the train the worker recorded and how many candidate
// records it made.
func (q *behindQueue) trainRecorded() (string, int) {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.train, q.recorded
}

// THE MERGE-RATE ALARM BORROWS THE QUEUE'S CAPACITY (M5-D28, ADR-0015), through
// the wiring a running node has: a STARTED git node, alone the home of every
// repository, whose queue worker builds a train onto a merge queue at depth
// rateDepth and batch rateBatch and times it standing — so its reading prices
// the queue at exactly config.LandingCapacity of those figures at the round it
// timed, over code.MergeRateWindow, and names the queue's MergeRateScan
// enqueues against it — which the retention loop's reading carries into the
// alarm table, so code_merge_rate_over_capacity stands on that node naming the
// branch. The threshold is the queue's own: no figure the alarm holds.
//
// Mutations: leave the home's half out of the reading (codeReading), and the
// alarm never fires; price the round at any figure but the one the worker
// timed, or the capacity at any depth or batch but the queue's, and the
// reading's capacity is not LandingCapacity's.
func TestTheMergeRateAlarmBorrowsTheQueuesCapacity(t *testing.T) {
	t.Parallel()
	b := gitNodeBootstrap(t, nil)
	engine.RequireNodeGit(t, b, parsedCompany(t, gitCompany))
	queue := &behindQueue{}
	e, err := engine.New(t.Context(), engine.WithHomeSeams(engine.Options{
		Bootstrap: b, Company: parsedCompany(t, gitCompany),
		ActivatedAt: time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC),
		GitHook:     codetest.HookCommand(t),
	}, func(inner codehome.Estate, _ codehome.Chains) (codehome.Estate, codehome.Chains) {
		queue.Estate = inner
		return queue, queue
	}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.WithoutCancel(t.Context())) })
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	// THE HOME'S READING, read until it names the queue: the worker's first
	// turn — once this node is listed as the repository's home — builds the
	// train, and a later one, a codehome.Backstop after at the latest, finds
	// it standing and prices the queue at its age. The deadline is three
	// passes — the loop's first, at Start, may find this node not yet
	// listed — and room for a loaded machine.
	deadline := time.Now().Add(3*codehome.Backstop + time.Minute)
	var reading statelog.Reading
	for {
		reading = engine.CodeReadingForTest(t.Context(), e)
		if reading.CodeMergeRateOver > 0 {
			break
		}
		if time.Now().After(deadline) {
			train, n := queue.trainRecorded()
			t.Fatalf("the home's reading names no queue past its capacity; the worker recorded %d train(s), "+
				"the last %q", n, train)
		}
		time.Sleep(50 * time.Millisecond)
	}
	if train, n := queue.trainRecorded(); train == "" || n != 1 {
		t.Errorf("the worker recorded %d train(s) before pricing the queue, want the one it then timed", n)
	}
	const named = "main of " + rateRepoName
	switch {
	case reading.CodeMergeRateOver != 1 || reading.CodeMergeRateOverTarget != named:
		t.Fatalf("the reading counts %d queue(s) over, the furthest %q; want the one queue, %q",
			reading.CodeMergeRateOver, reading.CodeMergeRateOverTarget, named)
	case reading.CodeMergeRateOverAsked != code.MergeRateScan:
		t.Errorf("the reading counts %d enqueue(s), want the queue's %d", reading.CodeMergeRateOverAsked,
			code.MergeRateScan)
	case reading.CodeMergeRateWindow != code.MergeRateWindow:
		t.Errorf("the reading's window is %s, want code.MergeRateWindow", reading.CodeMergeRateWindow)
	case reading.CodeMergeRateOverRound <= 0:
		t.Errorf("the reading prices the queue at a round of %s, want the age of the train it timed",
			reading.CodeMergeRateOverRound)
	}
	if want := config.LandingCapacity(rateDepth, rateBatch, reading.CodeMergeRateOverRound,
		code.MergeRateWindow); reading.CodeMergeRateOverCapacity != want {
		t.Errorf("the reading's capacity is %d at a round of %s, want LandingCapacity's %d at the queue's "+
			"depth %d and batch %d", reading.CodeMergeRateOverCapacity, reading.CodeMergeRateOverRound, want,
			rateDepth, rateBatch)
	}

	// AND THE ALARM, at the retention loop's next evaluation of its reading.
	// The rest is room for a loaded machine.
	deadline = time.Now().Add(statelog.AlarmInterval + time.Minute)
	for {
		kinds, _ := e.Alarms()
		if slices.Contains(kinds, statelog.KindCodeMergeRateOver) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the node's alarms are %v with a queue asked past its capacity, want %s", kinds,
				statelog.KindCodeMergeRateOver)
		}
		time.Sleep(50 * time.Millisecond)
	}
	report, ok := e.RetentionReport(t.Context())
	if !ok {
		t.Fatal("a node evaluating alarms has no retention report")
	}
	var detail string
	for _, a := range report.Alarms {
		if a.Kind == statelog.KindCodeMergeRateOver {
			detail = a.Detail
		}
	}
	if !strings.Contains(detail, named) {
		t.Errorf("the report's %s says %q, want the queue named", statelog.KindCodeMergeRateOver, detail)
	}
}
