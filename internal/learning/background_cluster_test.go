package learning

import (
	"context"
	"errors"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm/chain"
	"github.com/crewlet/crewlet/internal/store"
)

// THE CLUSTERING LOOP, as the background runner drives it.
//
// cluster_test.go covers the pass. These cases are the loop around it: that
// `scheduler_interval_seconds` is what sets its cadence — the knob validated
// with a shipped 3600 and was read by nothing — and that a seat the epoch
// cannot resolve is skipped rather than run with no role.

// A CONFIGURED CADENCE IS THE ONE THE LOOP TICKS AT.
func TestTheClusterLoopTicksAtTheConfiguredInterval(t *testing.T) {
	t.Parallel()
	var ticks sync.WaitGroup
	ticks.Add(3)
	var mu sync.Mutex
	seen := 0

	b := NewBackground(BackgroundOptions{
		Cluster: &Synthesizer{}, // never reached: RoleFor answers nil below
		RoleFor: func(string) *org.Role {
			mu.Lock()
			seen++
			if seen <= 3 {
				ticks.Done()
			}
			mu.Unlock()
			return nil
		},
		Seats:           func() []string { return []string{"dev"} },
		ClusterInterval: 5 * time.Millisecond,
	})
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	b.Start(ctx)

	done := make(chan struct{})
	go func() { ticks.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		mu.Lock()
		defer mu.Unlock()
		t.Fatalf("the loop ticked %d times in 2s at a 5ms interval — the "+
			"configured cadence is not what it runs at", seen)
	}
}

// A SEAT THE EPOCH CANNOT RESOLVE IS SKIPPED, not run with no role: a pass
// with no role resolves no model, and answering with whichever chain replied
// would charge one seat's auxiliary call to another's.
//
// Proven by the MODEL LOOKUP, which is the first thing the pass does that a
// missing role would corrupt — and by the resolvable seat in the same roster
// reaching it, so the case is about the skip rather than about a pass that
// never runs.
func TestAnUnresolvableSeatIsSkippedRatherThanRun(t *testing.T) {
	t.Parallel()
	db := openTestStore(t)
	writeClusterableTurns(t, db, "known")
	writeClusterableTurns(t, db, "ghost")

	models := &recordingModels{}
	syn, err := NewSynthesizer(models, NewSkills(db), SynthesizerOptions{
		Episodes: NewEpisodes(db), MinToolCalls: 3, ClusterMinSize: 3,
	})
	if err != nil {
		t.Fatalf("NewSynthesizer: %v", err)
	}
	b := NewBackground(BackgroundOptions{
		Cluster: syn,
		RoleFor: func(handle string) *org.Role {
			if handle == "ghost" {
				return nil
			}
			return &org.Role{Name: "Dev"}
		},
		Seats: func() []string { return []string{"ghost", "known"} },
	})
	b.clusterPass(t.Context())

	if got := models.roles(); len(got) != 1 || got[0] != "Dev" {
		t.Fatalf("model lookups = %v, want exactly the resolvable seat's role — "+
			"an unresolvable seat reached the model chain", got)
	}
}

// A CLUSTERING PASS WITH NO ROLE RESOLVER IS REFUSED AT CONSTRUCTION. One
// without it would fail per seat, per tick, forever — and the failure would
// read as a broken model chain rather than as missing wiring.
func TestAClusterPassWithoutARoleResolverIsNotArmed(t *testing.T) {
	t.Parallel()
	b := NewBackground(BackgroundOptions{
		Cluster: &Synthesizer{},
		Seats:   func() []string { return []string{"dev"} },
	})
	if b.cluster != nil {
		t.Fatal("a clustering pass was armed with no way to resolve a seat's role")
	}
}

// --- fixtures --------------------------------------------------------- //

// recordingModels answers every lookup and remembers whose role it was for.
type recordingModels struct {
	mu    sync.Mutex
	asked []string
}

func (m *recordingModels) Head(role *org.Role, _ phase.Phase) (chain.Member, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	name := ""
	if role != nil {
		name = role.Name
	}
	m.asked = append(m.asked, name)
	// An error rather than a provider: the pass logs it and moves on, and
	// this case is about WHICH seats got this far, not about drafting.
	return chain.Member{}, errNoModelForTest
}

func (m *recordingModels) roles() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.asked...)
}

var errNoModelForTest = errors.New("learning: no model in this test")

func openTestStore(t *testing.T) *store.DB {
	t.Helper()
	db, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "cluster.db"), store.Options{})
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// writeClusterableTurns gives a seat a cluster big enough to qualify.
func writeClusterableTurns(t *testing.T, db *store.DB, handle string) {
	t.Helper()
	eps := NewEpisodes(db)
	base := time.Now().UTC().Add(-time.Hour)
	for i := range 4 {
		_, err := eps.Append(t.Context(), Episode{
			ID: handle + "-ep-" + strconv.Itoa(i), Handle: handle, Role: "Dev",
			TurnID:    handle + "-turn-" + strconv.Itoa(i),
			StartedAt: base, EndedAt: base.Add(time.Duration(i) * time.Minute),
			TaskSummary: "ship it", PlanSummary: "cut, tag, announce",
			ToolSequence:  []string{"fetch", "build", "tag", "announce"},
			ReviewOutcome: "done", Kind: KindRaw,
		})
		if err != nil {
			t.Fatalf("Append: %v", err)
		}
	}
}

// THE THREE LOOPS HAVE THEIR OWN CADENCES. Sharing one would make tuning any
// of them silently move the others.
func TestEachBackgroundLoopKeepsItsOwnCadence(t *testing.T) {
	t.Parallel()
	b := NewBackground(BackgroundOptions{})
	if b.curatorEvery != CuratorInterval {
		t.Errorf("curator = %v, want %v", b.curatorEvery, CuratorInterval)
	}
	if b.lifecycleEvery != LifecycleInterval {
		t.Errorf("lifecycle = %v, want %v", b.lifecycleEvery, LifecycleInterval)
	}
	if b.clusterEvery != ClusterInterval {
		t.Errorf("cluster = %v, want %v", b.clusterEvery, ClusterInterval)
	}
}
