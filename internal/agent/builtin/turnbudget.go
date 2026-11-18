package builtin

import (
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
)

// turnBudget is this node's tally of one per-turn-segment bound: how much of it
// each turn segment has spent, charged once per operation key, so a turn
// retrying a call it lost the answer to is not charged twice.
//
// Per node is enough: a turn segment (named by its run id) runs, and makes
// every tool call, on the node holding its seat. A call outside a turn (a
// person's assistant) is held to none of it. An untouched turn's tally is
// forgotten after [turnBudgetIdle], swept at most once a minute.
type turnBudget struct {
	limit int
	now   func() time.Time

	mu    sync.Mutex
	turns map[string]*turnSpend
	swept time.Time
}

// turnSpend is one turn's use of a bound.
type turnSpend struct {
	used    int
	keys    map[string]int
	touched time.Time
}

// turnBudgetIdle is how long an untouched turn's tally is kept: a day, past
// any turn segment, the publish guardrail's own term.
const turnBudgetIdle = 24 * time.Hour

// newTurnBudget is an empty tally of limit per segment on now; nil is the wall
// clock.
func newTurnBudget(limit int, now func() time.Time) *turnBudget {
	if now == nil {
		now = time.Now
	}
	return &turnBudget{limit: limit, now: now, turns: map[string]*turnSpend{}}
}

// spend charges n to turn under key: past the bound it charges nothing and
// returns false with what the turn had used. An empty turn, or a key already
// charged (a retry), is charged nothing. refund gives back this call's
// charge, for a write that wrote nothing; it is idempotent.
func (g *turnBudget) spend(turn, key string, n int) (refund func(), used int, ok bool) {
	none := func() {}
	if turn == "" {
		return none, 0, true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	if now.Sub(g.swept) > time.Minute {
		for id, t := range g.turns {
			if now.Sub(t.touched) > turnBudgetIdle {
				delete(g.turns, id)
			}
		}
		g.swept = now
	}
	t, held := g.turns[turn]
	if !held {
		t = &turnSpend{keys: map[string]int{}}
		g.turns[turn] = t
	}
	t.touched = now
	if _, charged := t.keys[key]; key != "" && charged {
		return none, t.used, true
	}
	if t.used+n > g.limit {
		return none, t.used, false
	}
	t.used += n
	if key != "" {
		t.keys[key] = n
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			if spent, held := g.turns[turn]; held {
				spent.used -= n
				if key != "" {
					delete(spent.keys, key)
				}
			}
		})
	}, t.used - n, true
}

// FolderTurns is this node's tally of the folder records each agent turn
// segment writes: at most [artifacts.TurnFolderCreates] per segment, so a loop
// that makes a folder per artifact is stopped before it buries the company's
// tree. A create that found every folder already there wrote nothing and is
// given back.
type FolderTurns struct{ budget *turnBudget }

// NewFolderTurns is an empty tally on now; nil is the wall clock.
func NewFolderTurns(now func() time.Time) *FolderTurns {
	return &FolderTurns{budget: newTurnBudget(artifacts.TurnFolderCreates, now)}
}

// spend charges one folder record to turn under the operation key.
func (g *FolderTurns) spend(turn, key string) (refund func(), used int, ok bool) {
	return g.budget.spend(turn, key, 1)
}

// RenderTurns is this node's tally of the renders each agent turn segment
// makes: at most [artifacts.TurnRenders], so a turn looping on its own page
// holds the node's browser for a bounded time.
type RenderTurns struct{ budget *turnBudget }

// NewRenderTurns is an empty tally on now; nil is the wall clock.
func NewRenderTurns(now func() time.Time) *RenderTurns {
	return &RenderTurns{budget: newTurnBudget(artifacts.TurnRenders, now)}
}

// spend charges one render to turn: every call is one, a retry included,
// since each drives the browser again.
func (g *RenderTurns) spend(turn string) (refund func(), used int, ok bool) {
	return g.budget.spend(turn, "", 1)
}
