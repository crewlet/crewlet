package e2e

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet/gitlettest"
	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/mcprelay"
	"github.com/crewlet/crewlet/internal/notehelper/helperbuild"
	"github.com/crewlet/crewlet/internal/solo"
)

// The process-wide log sink for this package's tests.
//
// TestMain is its ONLY legitimate owner besides the harness: `run` installing a
// writer it was handed would point the global at 29 parallel tests' own
// buffers (a data race, and one test's lines in another's output). The
// single-engine cases run in parallel (the fleet cases run alone, before them:
// [noParallel]), so the sink is installed once, here, and the recorder is
// written from many goroutines. It runs through [solo.Run], since
// cluster_test.go stands up a fleet of engines, each embedding its own NATS
// server, in this one process (`go doc ./internal/solo`).
func TestMain(m *testing.M) {
	// THE HOOK FIRST, before the sink: every node here that hosts code execs
	// THIS binary as its proc-receive hook ([gitlettest.HookCommand], handed
	// over as engine.Options.GitHook). The hook's stdout is git's protocol and
	// its stderr reaches the pushing client, so a log line on either corrupts
	// a push.
	if status, ok := gitlettest.RunHookIfAsked(); ok {
		os.Exit(status)
	}
	// This binary is also one of three processes a model call re-executes it
	// as, dispatched before the sink or the framework so the child's stdout is
	// exactly the stream it speaks, and here rather than as a test function,
	// which every ordinary run would report as a skip. A process is one of
	// these or a test run, never two:
	//   - the relay to a seat's tools ([mcprelay.DispatchIfRelay]), on ARGV and
	//     BEFORE the scripted harness's variable, which it inherits;
	//   - the scripted model's harness ([scriptedHarnessEnv]);
	//   - the executor's harness in its box ([harnessEnv]).
	mcprelay.DispatchIfRelay()
	if os.Getenv(heldToolServerEnv) != "" {
		os.Exit(runHeldToolServer())
	}
	if endpoint := os.Getenv(scriptedHarnessEnv); endpoint != "" {
		os.Exit(runScriptedHarness(endpoint))
	}
	if endpoint := os.Getenv(harnessEnv); endpoint != "" {
		os.Exit(runHarness(endpoint))
	}
	binary, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e: this binary cannot name its own path, and every "+
			"company here runs it as its model's harness: %v\n", err)
		os.Exit(1)
	}
	scriptedHarnessBinary = binary
	if scriptedShape, err = claudeCodeRun(binary); err != nil {
		fmt.Fprintf(os.Stderr, "e2e: every company here runs its model's harness as Claude Code "+
			"runs: %v\n", err)
		os.Exit(1)
	}
	logging.Configure(slog.LevelDebug, logging.FormatJSON, logs)
	// Every launch writes the note helper, which this binary finds only where
	// helperbuild names it (go doc ./internal/notehelper/helperbuild).
	os.Exit(helperbuild.WithHelpers(func() int { return solo.Run(m) }))
}

// logs indexes what a test can assert on, and passes on what a PERSON needs.
//
// THREE JOBS. This is the engine's ONLY log destination for the package, at
// debug: keeping every line is a large buffer for nothing, and discarding all
// but the asserted ones loses the engine's account of every failure.
//  1. A case asserting on log lines WATCHES its trace before it starts the
//     work; only a watched trace's lines are kept ([traceRecorder.watch]).
//  2. Every line at WARN or above is written through to stderr, the one record
//     that survives a crash or a suite timeout, which run no cleanup.
//  3. It is KEPT, numbered, so a failing case prints the lines logged while it
//     ran ([traceRecorder.attribute]): `go test` credits a stderr line from an
//     engine goroutine to whichever test printed last.
var logs = &traceRecorder{through: os.Stderr}

// traceKept is how many lines one watched trace keeps, and loudKept how many
// WARN and ERROR lines the ring holds.
//
// traceKept is per watched trace, not shared by the run, so a case reading
// lines back does not depend on how many lines earlier cases wrote. A turn logs
// a few hundred lines at debug, so one that reached it has run away, and its
// first lines are the ones a correlation assertion reads.
//
// loudKept is about three and a half times the 2,345 WARN and ERROR lines one
// whole run of this package logged (measured before the cases ran in
// parallel), so a failing case's window, at most the whole parallel phase, is
// not cut short in practice; when it is, the dump says how many lines fell out.
const (
	traceKept = 4096
	loudKept  = 8192
)

type traceRecorder struct {
	// through receives the lines a person reads. Never the buffers' job:
	// the buffers answer assertions and failures, this answers a crash.
	through io.Writer

	mu sync.Mutex
	// traces holds the lines of every watched trace, by trace id. A trace
	// nobody watches is never kept, which is what makes a case's assertion
	// independent of how much every other case logged before it.
	traces map[string][]string
	// loud is the ring of WARN and ERROR lines; loudSeen counts every one
	// ever written, so line n of the run sits at loud[n % loudKept] while it
	// is still held.
	loud     []string
	loudSeen int
	// attributed is every case already printing its window on failure, so a
	// case that boots several nodes prints it once.
	attributed map[*testing.T]bool
}

// loud reports whether a line is one a reader of a failing run needs.
//
// Matched on the rendered JSON rather than a slog level hook, because this is
// an io.Writer: by the time a line arrives its level is a field like any
// other. The two names are what logging.FormatJSON emits for slog's WARN and
// ERROR.
func loud(line string) bool {
	return strings.Contains(line, `"level":"WARN"`) ||
		strings.Contains(line, `"level":"ERROR"`)
}

// traceKey is how a rendered line carries a trace id: the attribute
// logging.FormatJSON writes for the trace a line's context is in.
const traceKey = `"trace_id":"`

func (r *traceRecorder) Write(p []byte) (int, error) {
	line := string(p)
	traced := strings.Contains(line, traceKey)
	isLoud := loud(line)
	if traced || isLoud {
		r.mu.Lock()
		if traced {
			r.keepTraced(line)
		}
		if isLoud {
			if len(r.loud) < loudKept {
				r.loud = append(r.loud, line)
			} else {
				r.loud[r.loudSeen%loudKept] = line
			}
			r.loudSeen++
		}
		r.mu.Unlock()
	}
	if r.through != nil && isLoud {
		// Best effort: a test's diagnostics must never fail the test.
		_, _ = r.through.Write(p)
	}
	return len(p), nil
}

// keepTraced files a line under EVERY watched trace it carries, with r.mu held.
//
// NOT ONLY THE FIRST IT NAMES. The logger appends the context's trace after
// the record's own attributes (internal/logging), so a line that also logs a
// trace id of its own — the OTLP receiver's, the scheduler's — carries two, and
// filing it under the first lost it to the trace whose context wrote it. The
// closing quote is part of the match, so a watched id is never matched inside a
// longer one.
func (r *traceRecorder) keepTraced(line string) {
	for id, kept := range r.traces {
		if len(kept) < traceKept && strings.Contains(line, traceKey+id+`"`) {
			r.traces[id] = append(kept, line)
		}
	}
}

// watch starts keeping the lines that carry traceID, for as long as t runs.
//
// CALLED BEFORE THE WORK THAT LOGS UNDER IT, since a line written before the
// trace was watched is not kept.
func (r *traceRecorder) watch(t *testing.T, traceID string) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.traces == nil {
		r.traces = map[string][]string{}
	}
	if _, already := r.traces[traceID]; !already {
		r.traces[traceID] = nil
	}
	t.Cleanup(func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.traces, traceID)
	})
}

// linesFor returns the recorded lines carrying one watched trace id. Matching
// on the caller's own id is what makes this safe while other tests log in
// parallel.
func (r *traceRecorder) linesFor(traceID string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.traces[traceID]...)
}

// attribute makes every WARN and ERROR line logged from now until t ends part
// of t's own output, if t fails. Every node constructor here calls it, so a
// case that stands a node up gets it without asking.
//
// Every line of the window, other cases' engines' included: a line carries no
// mark of which engine wrote it, and the window is the one thing certain to
// hold all of this case's own. CALLED BEFORE THE CASE'S ENGINES ARE BUILT, so
// its cleanup runs after their stops (cleanups run last-registered first) and
// the window covers what an engine says on its way down.
func (r *traceRecorder) attribute(t *testing.T) {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.attributed[t] {
		return
	}
	if r.attributed == nil {
		r.attributed = map[*testing.T]bool{}
	}
	r.attributed[t] = true
	from := r.loudSeen
	t.Cleanup(func() {
		r.mu.Lock()
		delete(r.attributed, t)
		r.mu.Unlock()
		if !t.Failed() {
			return
		}
		lines, lost := r.loudSince(from)
		if len(lines) == 0 && lost == 0 {
			return
		}
		t.Logf("%d WARN/ERROR line(s) were logged while this case ran, by every "+
			"engine in the process — the lines name none:", len(lines)+lost)
		if lost > 0 {
			t.Logf("(the first %d fell out of the %d-line ring before this case "+
				"ended)", lost, loudKept)
		}
		for _, line := range lines {
			t.Log(strings.TrimRight(line, "\n"))
		}
	})
}

// loudSince is every loud line from number from on that the ring still holds,
// and how many of them it no longer does.
func (r *traceRecorder) loudSince(from int) (lines []string, lost int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if oldest := r.loudSeen - len(r.loud); from < oldest {
		lost, from = oldest-from, oldest
	}
	for n := from; n < r.loudSeen; n++ {
		lines = append(lines, r.loud[n%loudKept])
	}
	return lines, lost
}

var _ io.Writer = (*traceRecorder)(nil)

// A LINE IS KEPT UNDER EVERY WATCHED TRACE IT CARRIES, and under no trace it
// does not.
//
// The one case reading lines back asserts that a turn's lines carry its
// trigger's trace, so a line the recorder files under the wrong trace, or under
// none, reads as an engine that ran without correlation. A line that logs a
// trace id of its own carries the context's after it, which is the shape filing
// by the first id lost; and a watched id inside a longer one is a different
// trace, which a match without its closing quote would take.
func TestALineIsKeptUnderEveryWatchedTraceItCarries(t *testing.T) {
	t.Parallel()
	const (
		ctxTrace = "4bf92f3577b34da6a3ce929d0e0e4736"
		ownTrace = "0af7651916cd43dd8448eb211c80319c"
		stranger = "ffffffffffffffffffffffffffffffff"
	)
	r := &traceRecorder{}
	r.watch(t, ctxTrace)
	r.watch(t, ownTrace)

	both := `{"level":"INFO","msg":"spans_received","trace_id":"` + ownTrace +
		`","trace_id":"` + ctxTrace + `"}` + "\n"
	longer := `{"level":"INFO","msg":"elsewhere","trace_id":"` + ctxTrace + `ff"}` + "\n"
	unwatched := `{"level":"INFO","msg":"elsewhere","trace_id":"` + stranger + `"}` + "\n"
	for _, line := range []string{both, longer, unwatched} {
		if _, err := r.Write([]byte(line)); err != nil {
			t.Fatalf("write: %v", err)
		}
	}

	for _, id := range []string{ctxTrace, ownTrace} {
		if got := r.linesFor(id); len(got) != 1 || got[0] != both {
			t.Errorf("trace %s kept %q, want only the line carrying it", id, got)
		}
	}
}
