package executor_test

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/panics"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// plantedPanic is what a scrub planted to panic panics with.
const plantedPanic = "a planted defect in the scrub"

// panickingBox is b with every file it hands out panicking at its first read:
// the take reads the session's archive only in its scrub, so the scrub is what
// panics.
type panickingBox struct{ sandbox.Sandbox }

func (b panickingBox) OpenFile(ctx context.Context, path string) (io.ReadCloser, error) {
	f, err := b.Sandbox.OpenFile(ctx, path)
	if err != nil {
		return nil, err
	}
	return panickingFile{f}, nil
}

// panickingFile panics at every read.
type panickingFile struct{ io.ReadCloser }

func (panickingFile) Read([]byte) (int, error) { panic(plantedPanic) }

// A SCRUB THAT PANICS REFUSES ITS TAKE, AND THE ENGINE GOES ON. The scrub reads
// a box's archive on a goroutine of the take's own, which nothing above it can
// recover, so a defect a box's text reaches in a redaction or a rewrite ended
// the engine's process, every seat on the node with it. A panic in the scrub is
// contained there: the take's reader ends on an error carrying the panic's
// message, the take keeps nothing, and the panic is told to whoever observes
// contained panics, under the take's own scope, its stack in the log.
//
// Before the fix this case ends the test binary.
func TestAScrubThatPanicsRefusesItsTakeRatherThanEndTheEngine(t *testing.T) {
	t.Parallel()
	var (
		mu    sync.Mutex
		heard []*panics.Panic
	)
	stop := panics.Observe(func(p *panics.Panic) {
		mu.Lock()
		defer mu.Unlock()
		heard = append(heard, p)
	})
	defer stop()
	runner, b := sessionBox(t, "2.1.310")
	_, res := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "x"})
	if res.Captured == nil {
		t.Fatal("the run's session was not captured")
	}
	taken, err := runner.TakeSession(t.Context(), panickingBox{b}, sandbox.SessionTake{MaxBytes: takeBound})
	if err != nil || taken.Body == nil {
		t.Fatalf("the take: %+v, %v", taken, err)
	}
	_, err = io.ReadAll(taken.Body)
	_ = taken.Body.Close()
	if err == nil || !strings.Contains(err.Error(), plantedPanic) {
		t.Errorf("the take's reader ended on %v; want the scrub's panic, said", err)
	}
	if err = taken.Keep(t.Context()); err == nil || !strings.Contains(err.Error(), plantedPanic) {
		t.Errorf("the take's keep answered %v; want it refused with the panic's message", err)
	}
	mu.Lock()
	defer mu.Unlock()
	told := false
	for _, p := range heard {
		if p.Scope == panics.ScopeSessionTake && p.Value == plantedPanic {
			told = true
		}
	}
	if !told {
		t.Errorf("the panic was not told under the take's scope; heard %d", len(heard))
	}
}
