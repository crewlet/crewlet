package executor

import (
	"bytes"
	"io"
	"math/rand/v2"
	"runtime"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/panics"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A PANIC WHILE A `.json` FILE IS READ IS THE TAKE'S, AND LEAVES NOTHING OF THE
// SCRUB RUNNING: both of the first reading's ways through a `.json` file run on
// the scrub's own goroutine, where the take contains a panic (scrubContained),
// so a panic halfway through one (the box's archive failing under the
// rewrite's read) is the take's refusal, with no rewrite or redaction of the
// file left behind. Read off every goroutine's stack once the take has
// answered; not in parallel, since another case's scrub would be read as this.
//
// Mutation: read the file's two readings on goroutines of their own, fed
// through pipes, and the panic leaves both blocked on their pipes for good,
// a rewrite and a redaction of the file in their stacks.
func TestAPanicWhileAJSONFileIsReadLeavesNothingOfTheScrubRunning(t *testing.T) {
	r := rand.New(rand.NewPCG(1170, 9))
	body := `{"subject":"` + base64Run(r, 1<<20) + `"}`
	archive := tarOf(t, map[string]string{"tasks/s/1.json": body})
	const planted = "a planted defect under a .json file's reading"
	// Half the archive in, its bytes incompressible: inside the file's string,
	// well past its header.
	first := &panicAfter{r: bytes.NewReader(archive), left: len(archive) / 2, value: planted}
	err := scrubContained(t.Context(), func() error {
		return scrubArchive(archiveTar, first, reopenOf(archive), io.Discard, newScrubber(nil), testBounds)
	})
	if err == nil || !strings.Contains(err.Error(), planted) {
		t.Fatalf("the take answered %v; want the panic, said", err)
	}
	stacks := make([]byte, 16<<20)
	stacks = stacks[:runtime.Stack(stacks, true)]
	for _, frame := range []string{"executor.rewriteDocument(", "executor.rewriteText(", "executor.measureFile",
		"executor.(*textScrub)", "executor.(*jsonRewrite)"} {
		if bytes.Contains(stacks, []byte(frame)) {
			t.Errorf("a goroutine is left in %s after the take answered:\n%s", frame, stacks)
			return
		}
	}
}

// panicAfter hands r over, and panics with value once left bytes are read.
type panicAfter struct {
	r     io.Reader
	left  int
	value string
}

func (p *panicAfter) Read(b []byte) (int, error) {
	if p.left <= 0 {
		panic(p.value)
	}
	n, err := p.r.Read(b[:min(len(b), p.left)])
	p.left -= n
	return n, err
}

// A METER THAT PANICS MEASURES NOTHING, AND THE TAKE GOES ON: the meter reads
// what the take writes on a goroutine of its own, which nothing above it can
// recover, so a defect a box's text reaches in the transcript's walk or the
// export's scan would end the engine. Its panic is contained there: told under
// the take's scope, the meter measuring nothing, as one that cannot read its
// archive does, and every byte the take writes after it still taken, so the
// take never waits on a meter that stopped reading.
//
// Mutation: drop the meter's recover, and the panic ends the test binary.
func TestAMeterThatPanicsMeasuresNothingAndTheTakeGoesOn(t *testing.T) {
	t.Parallel()
	const planted = "a planted defect in the meter"
	var (
		mu   sync.Mutex
		told bool
	)
	stop := panics.Observe(func(p *panics.Panic) {
		mu.Lock()
		defer mu.Unlock()
		if p.Scope == panics.ScopeSessionTake && p.Value == planted {
			told = true
		}
	})
	defer stop()
	m := startMeter(t.Context(), func(r io.Reader) (int64, *sandbox.ResumedCount, error) {
		var head [16]byte
		_, _ = io.ReadFull(r, head[:])
		panic(planted)
	})
	// A MEBIBYTE, far past what the meter read before it panicked: each write
	// is taken whole.
	chunk := make([]byte, 16<<10)
	for range 64 {
		if n, err := m.Write(chunk); n != len(chunk) || err != nil {
			t.Fatalf("the take's write was answered %d, %v", n, err)
		}
	}
	m.finish(nil)
	if got := m.context(); got != 0 {
		t.Errorf("a meter that panicked measured %d tokens, want none", got)
	}
	if got := m.count(); got != nil {
		t.Errorf("a meter that panicked counted %+v, want nothing", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if !told {
		t.Error("the meter's panic was not told under the take's scope")
	}
}
