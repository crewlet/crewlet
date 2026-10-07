package learning

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/auxspend"
	"github.com/crewlet/crewlet/internal/compact"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
)

// pastTurnModel is an auxiliary model that answers every rewrite with a short
// text, records what each rewrite was FOR, and holds each call open until more
// of them are in flight than [compact.Parallel] allows — or a short wait has
// passed — so a render that opened them all at once is seen to have.
type pastTurnModel struct {
	mu       sync.Mutex
	purposes []types.AuxPurpose
	inFlight int
	peak     int
	crowd    chan struct{}
}

func newPastTurnModel() *pastTurnModel {
	return &pastTurnModel{crowd: make(chan struct{})}
}

func (m *pastTurnModel) Model() string { return "aux-small" }

func (m *pastTurnModel) Complete(ctx context.Context, _ llm.Request) (*llm.Completion, error) {
	m.mu.Lock()
	m.inFlight++
	m.peak = max(m.peak, m.inFlight)
	if m.inFlight == compact.Parallel+1 {
		close(m.crowd)
	}
	m.mu.Unlock()
	select {
	case <-m.crowd:
	case <-time.After(50 * time.Millisecond):
	case <-ctx.Done():
	}
	m.mu.Lock()
	m.inFlight--
	m.mu.Unlock()
	return &llm.Completion{Model: "aux-small", Content: "condensed"}, nil
}

func (m *pastTurnModel) Auxiliary(_ *org.Role, use auxspend.Use) (chain.Member, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.purposes = append(m.purposes, use.Purpose)
	return chain.Member{Key: "aux", Provider: m}, nil
}

// A PAST TURN'S ASK AND ITS ACCOUNT ARE EACH CONDENSED AS WHAT THEY ARE, AND
// NO MORE OF THEM AT ONCE THAN THE COMPACTOR ALLOWS ONE CALLER. The ask is the
// task a turn was given and the account what it concluded, and the compactor
// keeps different things of each; and the turn-start block and the
// query_episodes tool render through this one function, so its bound on the
// rewrites in flight is the bound on how hard both press a seat's provider.
func TestPastTurnsCondensesEachAskAndAccountAsWhatItIs(t *testing.T) {
	t.Parallel()
	model := newPastTurnModel()
	fit := compact.New(model, compact.NewCache()).For(nil, auxspend.Use{Stage: types.AuxStageTurn})
	long := strings.Repeat("the staging deploy keeps failing on the cache key ", 40)
	episodes := make([]Episode, 6)
	for i := range episodes {
		episodes[i] = Episode{Kind: KindRaw, TaskSummary: "Message from Ana: Slack message",
			Ask: strconv.Itoa(i) + " ask " + long, PlanSummary: strconv.Itoa(i) + " did " + long}
	}
	// A compacted row has neither, and is rendered as its pattern.
	episodes = append(episodes, Episode{Kind: KindCompacted, CommonTaskPattern: "Triaging a deploy",
		PlanSummary: long})

	turns := PastTurns(context.Background(), episodes, fit, EpisodeAccountBytes)

	if len(turns) != len(episodes) {
		t.Fatalf("%d past turns for %d episodes", len(turns), len(episodes))
	}
	for i, turn := range turns[:6] {
		if !strings.HasPrefix(turn.Ask, "condensed") || !strings.HasPrefix(turn.Account, "condensed") {
			t.Fatalf("turn %d = %+v, want its ask and its account each condensed", i, turn)
		}
	}
	if compacted := turns[6]; compacted != (PastTurn{}) {
		t.Fatalf("a compacted row rendered as a turn: %+v", compacted)
	}

	model.mu.Lock()
	defer model.mu.Unlock()
	counts := map[types.AuxPurpose]int{}
	for _, purpose := range model.purposes {
		counts[purpose]++
	}
	asks, accounts := types.AuxCondense(string(compact.KindTask)), types.AuxCondense(string(compact.KindOutcome))
	if counts[asks] != 6 || counts[accounts] != 6 || len(model.purposes) != 12 {
		t.Fatalf("rewrites by purpose = %v, want six asks condensed as a task and six accounts as an outcome",
			counts)
	}
	if model.peak > compact.Parallel {
		t.Fatalf("%d rewrites were in flight at once, want at most %d", model.peak, compact.Parallel)
	}
}

// A TEXT THAT FITS COSTS NOTHING, and a turn asked nothing says nothing: a
// turn recorded before its ask was stored has an empty ask, and the reader is
// shown no "asked" line rather than an invented one.
func TestPastTurnsCarriesAShortAskAndAccountWhole(t *testing.T) {
	t.Parallel()
	model := newPastTurnModel()
	fit := compact.New(model, compact.NewCache()).For(nil, auxspend.Use{Stage: types.AuxStageTurn})
	turns := PastTurns(context.Background(), []Episode{
		{Kind: KindRaw, Ask: "Why is\nstaging red?", PlanSummary: "Rolled back to v41."},
		{Kind: KindRaw, PlanSummary: "answered"},
	}, fit, EpisodeAccountBytes)

	want := []PastTurn{{Ask: "Why is staging red?", Account: "Rolled back to v41."}, {Account: "answered"}}
	for i := range want {
		if turns[i] != want[i] {
			t.Fatalf("turn %d = %+v, want %+v", i, turns[i], want[i])
		}
	}
	if len(model.purposes) != 0 {
		t.Fatalf("%d rewrites of texts that fit", len(model.purposes))
	}
}

// deadlineModel answers every rewrite after a pause, recording the deadline
// each call was handed.
type deadlineModel struct {
	mu        sync.Mutex
	pause     time.Duration
	deadlines []time.Time
}

func (m *deadlineModel) Model() string { return "aux-small" }

func (m *deadlineModel) Complete(ctx context.Context, _ llm.Request) (*llm.Completion, error) {
	deadline, _ := ctx.Deadline()
	m.mu.Lock()
	m.deadlines = append(m.deadlines, deadline)
	m.mu.Unlock()
	select {
	case <-time.After(m.pause):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	return &llm.Completion{Model: "aux-small", Content: "condensed"}, nil
}

func (m *deadlineModel) Auxiliary(*org.Role, auxspend.Use) (chain.Member, error) {
	return chain.Member{Key: "aux", Provider: m}, nil
}

// A RENDER HOLDS EVERY REWRITE TO ONE DEADLINE, never one each: rewrites run
// compact.Parallel at a time, so a deadline per rewrite was a deadline per
// WAVE, and the three turns the turn-start block recalls — six texts — waited
// two waves of thirty seconds where the block had been one auxiliary call.
// Every rewrite of a render, the ones queued behind the bound included, is
// handed the same instant, at most EpisodeRewriteTimeout from the start.
func TestARenderHoldsEveryRewriteToOneDeadline(t *testing.T) {
	t.Parallel()
	model := &deadlineModel{pause: 40 * time.Millisecond}
	fit := compact.New(model, compact.NewCache()).For(nil, auxspend.Use{Stage: types.AuxStageTurn})
	long := strings.Repeat("the staging deploy keeps failing on the cache key ", 40)
	episodes := make([]Episode, 6)
	for i := range episodes {
		episodes[i] = Episode{Kind: KindRaw, TaskSummary: "Message from Ana: Slack message",
			Ask: strconv.Itoa(i) + " ask " + long, PlanSummary: strconv.Itoa(i) + " did " + long}
	}
	start := time.Now()
	PastTurns(context.Background(), episodes, fit, EpisodeAccountBytes)

	model.mu.Lock()
	defer model.mu.Unlock()
	if len(model.deadlines) != 2*len(episodes) {
		t.Fatalf("%d rewrites for %d asks and accounts", len(model.deadlines), 2*len(episodes))
	}
	first := model.deadlines[0]
	for i, deadline := range model.deadlines {
		if !deadline.Equal(first) {
			t.Fatalf("rewrite %d was handed %v, rewrite 0 %v: a deadline per rewrite, "+
				"so a wave queued behind compact.Parallel waits a deadline of its own",
				i, deadline.Sub(start), first.Sub(start))
		}
	}
	if first.IsZero() || first.Sub(start) > EpisodeRewriteTimeout+time.Millisecond {
		t.Fatalf("the render's deadline is %v after its start, want at most %v",
			first.Sub(start), EpisodeRewriteTimeout)
	}
}
