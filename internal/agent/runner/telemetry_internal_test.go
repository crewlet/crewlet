package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/phase"
	"github.com/crewlet/crewlet/internal/agent/subagent"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
	"github.com/crewlet/crewlet/internal/agent/turn"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/panics"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// The prompt measurement is held here over values (the arithmetic: what each
// term counts, what a tool array comes to) and in fallback_test.go through a
// runner (the figure against what a provider was actually handed).

// THE TEXT TERMS ARE THE TWO MESSAGES A PHASE OPENS WITH, byte for byte, and a
// tool term that cannot be encoded does not zero them: the row goes out either
// way, and a text term zeroed by a schema's failure is uninterpretable.
func TestTheTextTermsAreTheOpeningPair(t *testing.T) {
	t.Parallel()
	m, err := measurePrompt("system text", "user text", nil)
	if err != nil {
		t.Fatalf("measurePrompt: %v", err)
	}
	if m.system != 11 || m.user != 9 || m.tools != 0 {
		t.Errorf("measured %d/%d/%d, want 11/9/0", m.system, m.user, m.tools)
	}
	broken, err := measurePrompt("system text", "user text",
		[]llm.ToolDef{{Name: "broken", Parameters: map[string]any{"type": make(chan int)}}})
	if err == nil {
		t.Fatal("an unencodable schema measured cleanly")
	}
	if broken.system != 11 || broken.user != 9 || broken.toolCount != 1 {
		t.Errorf("the failed tool term took the rest with it: %+v", broken)
	}
}

// THE APPROXIMATION IS OVER EVERY TERM, so the columns beside it reproduce it:
// a total omitting a component (the tool array, usually the largest) could not
// be re-derived from its own row.
func TestTheApproximationCoversEveryTermMeasured(t *testing.T) {
	t.Parallel()
	m := promptMeasure{system: 400, user: 200, tools: 1_200, toolCount: 9}
	if want := (400 + 200 + 1200) / bytesPerToken; m.approximateTokens() != want {
		t.Errorf("approximateTokens() = %d, want %d — every character term over the ratio",
			m.approximateTokens(), want)
	}
	// The count is not a character term: adding it would move the figure when a
	// tool is renamed into nothing.
	counted := promptMeasure{toolCount: 9}
	if counted.approximateTokens() != 0 {
		t.Error("a surface of nine definitions with no bytes approximated above zero")
	}
}

// A TOOL ARRAY IS MEASURED AS THE WIRE CARRIES IT: compact JSON, one
// {name, description, parameters} object per tool, in no vendor's wrapping.
// That keeps the figure comparable across harnesses; each wraps the
// definitions in its own vendor's shape, so the model is sent strictly more.
func TestAToolArrayIsMeasuredAsCompactWireJSON(t *testing.T) {
	t.Parallel()
	defs := []llm.ToolDef{{
		Name:        "post_message",
		Description: "post to a channel",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{"text": map[string]any{"type": "string"}},
		},
	}}
	got, err := toolDefBytes(defs)
	if err != nil {
		t.Fatalf("toolDefBytes: %v", err)
	}
	want := len(`[{"name":"post_message","description":"post to a channel",` +
		`"parameters":{"properties":{"text":{"type":"string"}},"type":"object"}}]`)
	if got != want {
		t.Errorf("measured %d bytes, want %d — the compact array, not an indented one", got, want)
	}

	// No tools is no bytes, and reachable: an empty surface must be told apart
	// from an unmeasured one, which is why the field carries no omitempty.
	if got, err := toolDefBytes(nil); got != 0 || err != nil {
		t.Errorf("toolDefBytes(nil) = %d, %v, want 0 and no error", got, err)
	}
}

// A TOOL WITH NO SCHEMA AND NO DESCRIPTION IS STILL A TOOL, not a panic.
// The surface fills an absent schema in, but a measurement that panics takes
// the turn goroutine with it, so the arithmetic holds for any caller. The
// fields are dropped rather than sent empty, as both vendors do.
func TestAToolWithNothingToDeclareStillMeasures(t *testing.T) {
	t.Parallel()
	got, err := toolDefBytes([]llm.ToolDef{{Name: "ping"}})
	if err != nil {
		t.Fatalf("toolDefBytes: %v", err)
	}
	if want := len(`[{"name":"ping"}]`); got != want {
		t.Errorf("measured %d bytes, want %d", got, want)
	}
}

// A SCHEMA THAT CANNOT BE ENCODED NAMES ITS OWN TOOL. Unreachable from a real
// surface (MCP schemas come through json.Unmarshal, builtins are literals),
// but encoding/json names only the Go type, and the tool's name is what a
// person can go and fix.
func TestAnUnencodableSchemaNamesTheToolItBelongsTo(t *testing.T) {
	t.Parallel()
	_, err := toolDefBytes([]llm.ToolDef{
		{Name: "healthy", Parameters: map[string]any{"type": "object"}},
		{Name: "broken", Parameters: map[string]any{"type": make(chan int)}},
	})
	if err == nil {
		t.Fatal("a schema json cannot encode measured cleanly, so nothing would report it")
	}
	if !strings.Contains(err.Error(), `"broken"`) {
		t.Errorf("error does not name the tool to go and fix: %v", err)
	}
	// The error wraps json's, so a caller can still reach the cause.
	var unsupported *json.UnsupportedTypeError
	if !errors.As(err, &unsupported) {
		t.Errorf("the json error was restated rather than wrapped: %v", err)
	}
}

// THE TALLY NAMES THE PHASE THAT FAILED, and still counts it as one that ran:
// a card reads the two together. A later phase's success does not clear it,
// since the failure ended the turn; a turn nothing failed in names nothing.
func TestTheTallyNamesThePhaseThatFailed(t *testing.T) {
	var s Spend
	s.record(phaseRecord{Phase: phase.Execute}, RecordCalls{})
	if s.FailedIn != "" {
		t.Fatalf("a phase that succeeded named %q as failed", s.FailedIn)
	}
	s.record(phaseRecord{Phase: phase.Review, Failed: true}, RecordCalls{})
	if s.FailedIn != "review" {
		t.Errorf("failed in %q, want review", s.FailedIn)
	}
	if !slices.Equal(s.Phases, []string{"execute", "review"}) {
		t.Errorf("phases %v, want the failed phase listed as one that ran", s.Phases)
	}
}

// A BOX THAT COULD NOT BE STARTED IS NAMED BY ITS CLASS, never `error`: the
// kind is the sandbox's class, read before any deadline its chain carries; a
// provider's own kind and an unclassed failure keep theirs.
//
// Mutation: drop the sandbox arm and every class row reads `error`; read the
// deadline first and the timed-out row reads `timeout`.
func TestABoxThatCouldNotBeStartedIsNamedByItsClass(t *testing.T) {
	t.Parallel()
	for _, why := range sandbox.Whys {
		err := fmt.Errorf("launching the executor's run: %w",
			sandbox.Because(why, errors.New("starting the box: the runtime did not answer")))
		if got := ErrorKind(err); got != builtin.SandboxKindPrefix+string(why) {
			t.Errorf("a box %s failure is kind %q; want %q", why, got,
				builtin.SandboxKindPrefix+string(why))
		}
	}
	timedOut := sandbox.Because(sandbox.WhyProviderUnavailable,
		fmt.Errorf("starting the box: %w", context.DeadlineExceeded))
	if got := ErrorKind(timedOut); got != "sandbox_provider_unavailable" {
		t.Errorf("a box start that ran out of time is kind %q", got)
	}
	if got := ErrorKind(errors.New("a broken thing")); got != "error" {
		t.Errorf("an unclassed failure is kind %q, want error", got)
	}
}

// A LAUNCH THIS NODE CANCELLED (a drain, a lost lease) IS `canceled`, never the
// sandbox's: the container backend leaves its caller's own cancellation
// unclassed, so the turn's record never sends a person to fix a setup a drain
// interrupted. Not parallel: the backend finds its runtime on PATH, here a
// stand-in whose every command waits until it is killed.
//
// Mutation: class a cancelled container start in internal/sandbox's Create, or
// read a sandbox class before the cancellation in ErrorKind, and this goes red.
func TestALaunchThisNodeCancelledIsCanceled(t *testing.T) {
	dir := t.TempDir()
	// Under the fork lock, as internal/sandbox's own stand-ins are written: a
	// fork while the file is open for writing makes its exec fail (ETXTBSY).
	syscall.ForkLock.RLock()
	err := os.WriteFile(filepath.Join(dir, "docker"), []byte("#!/bin/sh\nexec sleep 60\n"), 0o700) //nolint:gosec // the case's own program, to be run
	syscall.ForkLock.RUnlock()
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	provider, err := sandbox.NewLocal(sandbox.LocalOptions{Placement: sandbox.Container,
		StateDir: t.TempDir(), Image: "img", Runtime: "docker"})
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	box, err := provider.Create(cancelled, sandbox.Spec{})
	if err == nil {
		_ = box.Close(t.Context())
		t.Fatal("Create handed out a box its caller cancelled")
	}
	if got := ErrorKind(fmt.Errorf("launching the executor's run: %w", err)); got != "canceled" {
		t.Errorf("a launch this node cancelled (%v) is kind %q, want canceled", err, got)
	}
}

// AN OUTAGE IS RECORDED UNDER ITS OWN KIND, never `error`, which an issue
// report reads as an engine defect: a coordination store that did not answer,
// no data node serving, none of a build that reads the call whole, a busy
// store, a queue down, and a disk full, read-only or failing or descriptors
// exhausted, read by its system call's errno. A driver message that merely
// reads like one stays a fault.
//
// Mutation: drop the condition arm from ErrorKind and every row reads `error`;
// drop an errno from the host conditions and its row does; read an estate
// nobody served before a build that cannot read the call, and the refusal,
// which wraps one, reads `no_data_node`.
func TestAnOutageIsRecordedUnderItsOwnKind(t *testing.T) {
	t.Parallel()
	transport := errors.New("nats: dial tcp 10.0.0.7:4222: connection refused")
	disk := func(errno syscall.Errno) error {
		return fmt.Errorf("write the session: %w",
			&os.PathError{Op: "write", Path: "/var/lib/crewlet/x", Err: errno})
	}
	for _, tc := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("read pins: %w: %w", coord.ErrUnavailable, transport),
			builtin.KindCoordinationUnavailable},
		{fmt.Errorf("estate: %w", &estate.ErrUnserved{Detail: "every node"}),
			builtin.KindNoDataNode},
		{fmt.Errorf("record the end: %w", &estate.ErrUnknownKeys{Op: "executor.end",
			Keys: []string{"args.Ending.went_on"}, Nodes: []string{"data-a"},
			Unserved: &estate.ErrUnserved{Detail: "data-a refused it"}}), builtin.KindFleetBuildsDiffer},
		{fmt.Errorf("worklet: %w", store.ErrNoEstate), builtin.KindEstateClosed},
		{fmt.Errorf("worklet: %w: %w", store.ErrBusy, errors.New("database is locked")),
			builtin.KindStoreBusy},
		{fmt.Errorf("publish: %w", queue.ErrNotLive), builtin.KindQueueNotLive},
		{fmt.Errorf("ask: %w: %w", queue.ErrUnavailable, transport),
			builtin.KindQueueUnavailable},
		{fmt.Errorf("chatlet: %w", statelog.ErrUnavailable), builtin.KindStateLogUnavailable},
		{fmt.Errorf("objstore: %w", objstore.ErrFull), builtin.KindFileStoreFull},
		{fmt.Errorf("worklet: %w: no room", store.ErrFull), builtin.KindDiskFull},
		{disk(syscall.ENOSPC), builtin.KindDiskFull},
		{disk(syscall.EDQUOT), builtin.KindDiskFull},
		{disk(syscall.EROFS), builtin.KindDiskReadOnly},
		{disk(syscall.EIO), builtin.KindDiskFailed},
		{disk(syscall.EMFILE), builtin.KindOpenFilesExhausted},
		{disk(syscall.ENFILE), builtin.KindOpenFilesExhausted},
		{errors.New("disk I/O error: no space left on device"), types.ErrorKindUnclassified},
	} {
		if got := ErrorKind(tc.err); got != tc.want {
			t.Errorf("%v is kind %q, want %q", tc.err, got, tc.want)
		}
	}
}

// A TURN'S CALLS TO EACH MCP TOOL ARE COUNTED ONCE, IN ONE OUTCOME, across
// every phase, delegated worker and bridged executor call: the tally the
// turn's ending record states ([types.TurnTally.Tools]) and the usage domain
// reads. A call is filed under the server whose tool it NAMED, so one refused
// before any tool ran counts against that server; a builtin's call and an
// unregistered name are no server's. The phase record's row carries the same
// `source` and `refusal`.
//
// Mutation: file a call by its Origin rather than its Source, and the refused
// call vanishes; count an unavailable call or a refusal as failed, and its
// outcome moves; drop the worker's executions from recordWorker, and
// github_search counts one call.
func TestATurnsMCPCallsAreCountedOncePerOutcome(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	answered := func(name, server string, failed bool, refusal mcp.Refusal, ms int) toolloop.Execution {
		return toolloop.Execution{Name: name, Failed: failed, Refusal: refusal,
			Origin: mcp.Origin(server), Server: server, Source: mcp.Origin(server),
			StartedAt: at.Add(time.Duration(ms) * time.Second), Duration: time.Duration(ms) * time.Millisecond}
	}
	execute := []toolloop.Execution{
		answered("github_get_issue", "github", false, "", 100),
		answered("github_get_issue", "github", true, "", 200),
		answered("github_get_issue", "github", true, mcp.RefusalUnavailable, 5),
		// REFUSED BEFORE ANY TOOL RAN: no origin, untimed, still github's tool.
		{Name: "github_close_issue", Failed: true, Refusal: mcp.RefusalForbidden,
			Source: mcp.Origin("github")},
		// NO SERVER'S: a builtin, and a name nothing registered.
		{Name: "remember", Origin: mcp.OriginBuiltin, Source: mcp.OriginBuiltin},
		{Name: "ghost", Failed: true, Refusal: mcp.RefusalNotFound},
	}
	var s Spend
	var mu sync.Mutex
	first := phaseRecord{Phase: phase.Execute, Result: toolloop.Result{Executions: execute}}
	s.record(first, first.kept().calls)
	s.recordWorker(&mu, subagent.Result{Executions: []toolloop.Execution{
		answered("github_search", "github", false, "", 50),
	}})
	bridged := agentRunResult("done", []BridgedCall{{
		Call: ledger.Call{Name: "github_search"}, StartedAt: at.Add(time.Hour),
		Duration: 70 * time.Millisecond, Origin: mcp.Origin("github"), Server: "github",
		Source: mcp.Origin("github"),
	}}, nil, RunRecord{}, at, at.Add(2*time.Hour))
	second := phaseRecord{Phase: phase.Execute, Result: bridged.Result}
	s.record(second, second.kept().calls)

	want := []types.ToolTally{
		{Server: "github", Tool: "github_close_issue", Calls: 1, Refused: 1},
		{Server: "github", Tool: "github_get_issue", Calls: 3, Failed: 1, Unavailable: 1,
			TotalMS: 305, LastAt: at.Add(200 * time.Second)},
		{Server: "github", Tool: "github_search", Calls: 2, TotalMS: 120, LastAt: at.Add(time.Hour)},
	}
	if !reflect.DeepEqual(s.Tools, want) {
		t.Errorf("the tally is\n%+v\nwant\n%+v", s.Tools, want)
	}

	rows := toolExecutions(execute)
	for i, wantRow := range []map[string]any{
		{"source": "mcp:github"}, {"source": "mcp:github"},
		{"source": "mcp:github", "refusal": "unavailable"},
		{"source": "mcp:github", "refusal": "forbidden"},
		{"source": "builtin"}, {"refusal": "not_found"},
	} {
		for _, key := range []string{"source", "refusal"} {
			if got, want := rows[i][key], wantRow[key]; got != want {
				t.Errorf("%s's row %d: %s = %v, want %v", execute[i].Name, i, key, got, want)
			}
		}
	}
}

// A CONTAINED PANIC IS ITS OWN KIND, through any wrapping and whatever its
// value: reported as engine.panicked, so no consumer counting unclassified
// failures (the failure loop, a lost message) counts it again.
//
// Mutation: drop the panic arm from ErrorKind and every row reads `error`.
func TestAContainedPanicIsItsOwnKind(t *testing.T) {
	t.Parallel()
	contained := func(value any) (p *panics.Panic) {
		defer func() { p = panics.Recovered(panics.ScopeQueue, recover()) }()
		panic(value)
	}
	for name, err := range map[string]error{
		"a turn's panic":                    fmt.Errorf("turn: execute round 1: %w", turn.Recovered(panics.ScopeTurn, "nil map")),
		"a turn's panic of a cancellation":  turn.Recovered(panics.ScopeTurn, context.Canceled),
		"a resume abandoned on a panic":     fmt.Errorf("resume abandoned: %w", turn.Recovered(panics.ScopeTurn, "x")),
		"a queue handler's panic":           fmt.Errorf("handler panicked: %w", contained("index out of range").Err()),
		"a panic whose value is a provider": turn.Recovered(panics.ScopeToolCall, &llm.Error{Kind: llm.KindAuth}),
	} {
		if got := ErrorKind(err); got != types.ErrorKindPanicked {
			t.Errorf("%s: %v is kind %q, want %q", name, err, got, types.ErrorKindPanicked)
		}
	}
	if got := ErrorKind(errors.New("a broken thing")); got != types.ErrorKindUnclassified {
		t.Errorf("an unclassified failure is kind %q", got)
	}
}

// A CALL'S IMAGES ARE RECORDED BY TYPE AND SIZE, NEVER AS PICTURES: a row keeps
// `images` only when its call's answer carried one.
func TestARowRecordsACallsImagesByTypeAndSize(t *testing.T) {
	t.Parallel()
	rows := toolExecutions([]toolloop.Execution{
		{Name: "read_comment", Output: "ana's crop of the chart",
			Images: []mcp.ImageRecord{{MIMEType: mcp.ImagePNG, Bytes: 1234}}},
		{Name: "read_page", Output: "a page"},
	})
	raw, err := json.Marshal(rows)
	if err != nil {
		t.Fatal(err)
	}
	var got []map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatal(err)
	}
	images, _ := json.Marshal(got[0]["images"])
	if string(images) != `[{"bytes":1234,"mime_type":"image/png"}]` || got[0]["result"] != "ana's crop of the chart" {
		t.Errorf("the image's row records %s beside %q, want its type and size beside its text",
			images, got[0]["result"])
	}
	if _, ok := got[1]["images"]; ok {
		t.Errorf("a row of an answer with no image records %v", got[1]["images"])
	}
}
