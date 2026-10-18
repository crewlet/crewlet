package artifactupload

import (
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/artifacts"
)

// THE PER-TURN GUARDRAILS (an agent's turn segment): at most
// [artifacts.TurnPublishes] publishes, [artifacts.TurnPublishesPerArtifact] of
// them on one artifact, and [artifacts.TurnBytesWritten] bytes written, every
// byte the pipeline stores counted (a transfer, an edit's result, a linted page,
// a copied CDN file, inline content). Past a bound the call is refused
// ([*TurnQuota]) before any byte moves.
//
// A turn is named by its call's TurnID, and a call with none (a person, a
// script, an operator's assistant between turns) is held to none of these: its
// bounds are the reservation budget's. The tally is this node's: a turn segment
// runs on the node that holds its seat, and every tool call it makes is a call
// on that node. A publish is counted once per operation key, so a turn retrying
// a call it lost the answer to is not charged twice.

// turnIdle is how long an untouched turn's tally is kept: a day, past any turn
// segment, so a tally is forgotten only for a turn that has ended.
const turnIdle = 24 * time.Hour

// turnGuard is this node's tallies.
type turnGuard struct {
	now func() time.Time

	mu    sync.Mutex
	turns map[string]*turnTally
	swept time.Time
}

// turnTally is one turn's use.
type turnTally struct {
	publishes   int64
	perArtifact map[string]int64
	bytes       int64
	ops         map[string]bool
	touched     time.Time
}

func newTurnGuard(now func() time.Time) *turnGuard {
	return &turnGuard{now: now, turns: map[string]*turnTally{}}
}

// tally is the turn's tally under g.mu, forgetting idle ones at most once a
// minute.
func (g *turnGuard) tally(turn string) *turnTally {
	now := g.now()
	if now.Sub(g.swept) > time.Minute {
		for id, t := range g.turns {
			if now.Sub(t.touched) > turnIdle {
				delete(g.turns, id)
			}
		}
		g.swept = now
	}
	t, ok := g.turns[turn]
	if !ok {
		t = &turnTally{perArtifact: map[string]int64{}, ops: map[string]bool{}}
		g.turns[turn] = t
	}
	t.touched = now
	return t
}

// publish counts one publish of artifact (empty for a create) by the turn,
// refused past a bound. opKey names the call, so a retry of it is not counted
// again; an empty key counts every time.
func (g *turnGuard) publish(turn, artifact, opKey string) error {
	if turn == "" {
		return nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	t := g.tally(turn)
	if opKey != "" && t.ops[opKey] {
		return nil
	}
	if t.publishes >= artifacts.TurnPublishes {
		return &TurnQuota{What: "publishes", Limit: artifacts.TurnPublishes, Used: t.publishes}
	}
	if artifact != "" && t.perArtifact[artifact] >= artifacts.TurnPublishesPerArtifact {
		return &TurnQuota{What: "publishes_on_artifact", Limit: artifacts.TurnPublishesPerArtifact,
			Used: t.perArtifact[artifact]}
	}
	t.publishes++
	if artifact != "" {
		t.perArtifact[artifact]++
	}
	if opKey != "" {
		t.ops[opKey] = true
	}
	return nil
}

// spend charges n bytes about to be written to the turn, refused past the
// bound; the answer gives them back where they were never written.
func (g *turnGuard) spend(turn string, n int64) (giveBack func(), err error) {
	if turn == "" || n <= 0 {
		return func() {}, nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	t := g.tally(turn)
	if t.bytes+n > artifacts.TurnBytesWritten {
		return nil, &TurnQuota{What: "bytes", Limit: artifacts.TurnBytesWritten, Used: t.bytes}
	}
	t.bytes += n
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			defer g.mu.Unlock()
			if t, ok := g.turns[turn]; ok {
				t.bytes -= n
			}
		})
	}, nil
}
