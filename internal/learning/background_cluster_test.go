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
// with a shipped 3600 and was read by nothing — and that each seat the roster
// lists is clustered over the episodes filed under the handle it was CREATED
// under, on its own role.

// A CONFIGURED CADENCE IS THE ONE THE LOOP TICKS AT.
func TestTheClusterLoopTicksAtTheConfiguredInterval(t *testing.T) {
	t.Parallel()
	var ticks sync.WaitGroup
	ticks.Add(3)
	var mu sync.Mutex
	seen := 0

	b := NewBackground(BackgroundOptions{
		Passes: BackgroundPasses{
			Cluster:         &Synthesizer{},
			ClusterInterval: 5 * time.Millisecond,
		},
		// Every tick reads the roster once; an empty one runs nothing, so
		// the count below is the loop's cadence and nothing else.
		Seats: func() []*org.Role {
			mu.Lock()
			seen++
			if seen <= 3 {
				ticks.Done()
			}
			mu.Unlock()
			return nil
		},
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

// A RENAMED SEAT IS CLUSTERED OVER THE WORK IT DID BEFORE THE RENAME, on its
// own role.
//
// The roster lists roles, and the pass reads a seat's episodes by the handle
// it was CREATED under. It used to be handed the seat's current handle, so a
// seat renamed after a fortnight of the same procedure clustered nothing: its
// episodes are filed under the handle it had when it did the work.
//
// Proven by the MODEL LOOKUP, which the pass reaches only once it has found a
// qualifying cluster — and by the seat beside it with no episodes at all,
// which must not reach it, so the case is about whose episodes were read
// rather than about a pass that asks the model for everyone.
func TestTheClusterLoopReadsARenamedSeatsEpisodesByItsOrigin(t *testing.T) {
	t.Parallel()
	db := openTestStore(t)
	writeClusterableTurns(t, db, "dev")

	models := &recordingModels{}
	syn, err := NewSynthesizer(models, NewSkills(db), SynthesizerOptions{
		Episodes: NewEpisodes(db), MinToolCalls: 3, ClusterMinSize: 3,
	})
	if err != nil {
		t.Fatalf("NewSynthesizer: %v", err)
	}
	renamed := &org.Role{Name: "Dev", DeclaredHandle: "dev-two",
		OriginHandle: "dev", FormerHandles: []string{"dev"}}
	idle := &org.Role{Name: "Ops", DeclaredHandle: "ops"}
	b := NewBackground(BackgroundOptions{
		Passes: BackgroundPasses{Cluster: syn},
		Seats:  func() []*org.Role { return []*org.Role{idle, renamed} },
	})
	b.clusterPass(t.Context(), syn)

	if got := models.roles(); len(got) != 1 || got[0] != "Dev" {
		t.Fatalf("model lookups = %v, want exactly the renamed seat's role — its "+
			"episodes are filed under the handle it was created under, and a "+
			"pass that read them by its new handle found no cluster to draft", got)
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
	if b.passes.CuratorInterval != CuratorInterval {
		t.Errorf("curator = %v, want %v", b.passes.CuratorInterval, CuratorInterval)
	}
	if b.passes.LifecycleInterval != LifecycleInterval {
		t.Errorf("lifecycle = %v, want %v", b.passes.LifecycleInterval, LifecycleInterval)
	}
	if b.passes.ClusterInterval != ClusterInterval {
		t.Errorf("cluster = %v, want %v", b.passes.ClusterInterval, ClusterInterval)
	}
	if b.passes.PromotionInterval != PromotionInterval {
		t.Errorf("promotion = %v, want %v", b.passes.PromotionInterval, PromotionInterval)
	}
}
