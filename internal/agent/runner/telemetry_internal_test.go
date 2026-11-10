package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
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

// The prompt measurement is exercised HERE, over values, as well as through a
// runner in fallback_test.go. Two levels because they hold different things:
// the runner cases hold the figure against what a provider was actually
// handed, and these hold the arithmetic itself — what each term counts and
// what a tool array comes to — without a provider, a surface or a turn to
// stand up first.

// THE TEXT TERMS ARE THE TWO MESSAGES A PHASE OPENS WITH, byte for byte, and a
// tool term that cannot be encoded does not take them down with it: the row
// goes out either way, and a text term zeroed because a schema could not be
// encoded is a number nobody can interpret.
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

// THE APPROXIMATION IS OVER EVERY TERM, so the columns beside it reproduce it.
//
// A total that omitted one of its own components would be a number nobody
// could re-derive from the row it sits on — and the term it omitted for as
// long as this event existed was the tool array, which is usually the largest.
func TestTheApproximationCoversEveryTermMeasured(t *testing.T) {
	t.Parallel()
	m := promptMeasure{system: 400, user: 200, tools: 1_200, toolCount: 9}
	if want := (400 + 200 + 1200) / bytesPerToken; m.approximateTokens() != want {
		t.Errorf("approximateTokens() = %d, want %d — every character term over the ratio",
			m.approximateTokens(), want)
	}
	// The count is not a character term. Adding it would make the figure
	// move when a tool is renamed into nothing.
	counted := promptMeasure{toolCount: 9}
	if counted.approximateTokens() != 0 {
		t.Error("a surface of nine definitions with no bytes approximated above zero")
	}
}

// A TOOL ARRAY IS MEASURED AS THE WIRE CARRIES IT: compact JSON, one
// {name, description, parameters} object per tool.
//
// The compact form is the definitions' own JSON in no vendor's wrapping, which
// is what makes the figure comparable across harnesses: each one reads them over
// MCP, annotations and all, and hands them to its model in its own vendor's
// shape, so the model is sent strictly more — which is why the choice is
// written down rather than left to be inferred from a number.
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

	// No tools is no bytes, and it has to be reachable: a phase whose
	// surface is empty must be distinguishable from one that was never
	// measured, which is why the field carries no omitempty.
	if got, err := toolDefBytes(nil); got != 0 || err != nil {
		t.Errorf("toolDefBytes(nil) = %d, %v, want 0 and no error", got, err)
	}
}

// A TOOL WITH NO SCHEMA AND NO DESCRIPTION IS STILL A TOOL, not a panic.
//
// The surface fills an absent schema in before it ever renders a definition,
// so a nil map cannot arrive from there — but this arithmetic is the kind that
// gets called from somewhere else later, and a measurement that panics takes
// the turn goroutine with it. The fields are dropped rather than sent empty,
// which is what both vendors do.
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

// A SCHEMA THAT CANNOT BE ENCODED NAMES ITS OWN TOOL.
//
// An MCP server's schema reached this map through json.Unmarshal and a
// builtin's is a literal in this tree, so this is unreachable from a real
// surface — and it is exactly the case where the error a person reads decides
// whether they can act. encoding/json reports the offending Go type, of which
// a surface has one per tool; the tool's name is the thing they can go and
// look at.
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
	// And the error is a wrapper over what json said, so a caller can still
	// reach the cause.
	var unsupported *json.UnsupportedTypeError
	if !errors.As(err, &unsupported) {
		t.Errorf("the json error was restated rather than wrapped: %v", err)
	}
}

// THE TALLY NAMES THE PHASE THAT FAILED, and the phase still counts as one
// that ran: its record completed, and a card reads the two together — every
// phase that ran, and which of them broke. A phase that succeeded after an
// earlier one failed does not clear it, since the failure is what ended the
// turn; one nothing failed in names nothing.
func TestTheTallyNamesThePhaseThatFailed(t *testing.T) {
	var s Spend
	s.record(phaseRecord{Phase: phase.Execute})
	if s.FailedIn != "" {
		t.Fatalf("a phase that succeeded named %q as failed", s.FailedIn)
	}
	s.record(phaseRecord{Phase: phase.Review, Failed: true})
	if s.FailedIn != "review" {
		t.Errorf("failed in %q, want review", s.FailedIn)
	}
	if !slices.Equal(s.Phases, []string{"execute", "review"}) {
		t.Errorf("phases %v, want the failed phase listed as one that ran", s.Phases)
	}
}

// A BOX THAT COULD NOT BE STARTED IS NAMED BY ITS CLASS. Every executor
// runs in a coding sandbox, as its harness's own run, and a box its
// provider would not start failed the phase and the turn as `error`, while
// the backend that met it had said what clears it. The kind is the sandbox's
// class, read before a deadline its chain carries; a provider's own kind and
// an unclassed failure keep theirs.
//
// Mutation: drop the sandbox arm and every class row reads `error`; read the
// deadline first and the timed-out row reads `timeout`.
func TestABoxThatCouldNotBeStartedIsNamedByItsClass(t *testing.T) {
	t.Parallel()
	for _, why := range sandbox.Whys {
		err := fmt.Errorf("launching the executor's run: %w",
			sandbox.Because(why, errors.New("e2b: create: 503 Service Unavailable")))
		if got := ErrorKind(err); got != builtin.SandboxKindPrefix+string(why) {
			t.Errorf("a box %s failure is kind %q; want %q", why, got,
				builtin.SandboxKindPrefix+string(why))
		}
	}
	timedOut := sandbox.Because(sandbox.WhyProviderUnavailable,
		fmt.Errorf("e2b: create: %w", context.DeadlineExceeded))
	if got := ErrorKind(timedOut); got != "sandbox_provider_unavailable" {
		t.Errorf("a provider request that ran out of time is kind %q", got)
	}
	if got := ErrorKind(errors.New("a broken thing")); got != "error" {
		t.Errorf("an unclassed failure is kind %q, want error", got)
	}

	// A LAUNCH THIS NODE CANCELLED (a drain, a lost lease) is `canceled`,
	// never the provider: the backend leaves the caller's own cancellation
	// unclassed, and was marking it provider_unavailable.
	provider, err := sandbox.NewE2B(sandbox.E2BOptions{APIKey: "k", Domain: "e2b.example.com",
		HTTP: &http.Client{Transport: refusedTransport{context.Canceled}}})
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = provider.Create(cancelled, sandbox.Spec{})
	if got := ErrorKind(fmt.Errorf("launching the executor's run: %w", err)); got != "canceled" {
		t.Errorf("a launch this node cancelled is kind %q, want canceled", got)
	}
}

// AN OUTAGE IS NOT THE ENGINE'S OWN DEFECT, ON THE RECORD. A failed phase or
// turn records the condition that failed it under the condition's own kind,
// the classification every tool and API answer reads: a coordination store
// that did not answer, no data node serving, no data node of a build that
// reads the call whole, a busy store, a queue down, and a disk that is full,
// read-only or failing or a process out of descriptors, read by the error
// number its system call returned. Each was `error`, the word an issue report
// reads as a defect of the engine. A driver message that merely reads like one
// stays a fault.
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

// refusedTransport fails every request with err, before any host answers.
type refusedTransport struct{ err error }

func (r refusedTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, r.err }

// A TURN'S CALLS TO EACH MCP TOOL ARE COUNTED ONCE, IN ONE OUTCOME, every
// phase's and every delegated worker's and an executor run's as its bridge
// recorded them: the tally the record ending the turn states
// ([types.TurnTally.Tools]) and the usage domain counts a seat's calls from.
// Each call is filed under the server whose tool it NAMED, so a call the
// engine refused before any tool ran counts against that server; a builtin's
// call and a name nothing registered are no server's.
//
// And the phase record's row carries the same two facts, `source` and
// `refusal`, for a reader of the turn's own calls.
//
// Mutation: file a call by its Origin rather than its Source, and the refused
// call vanishes; count an unavailable call as failed, or a refusal as failed,
// and its outcome moves; drop the worker's executions from recordWorker, and
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
		// REFUSED BEFORE ANY TOOL RAN: no origin, untimed, and still
		// github's tool.
		{Name: "github_close_issue", Failed: true, Refusal: mcp.RefusalForbidden,
			Source: mcp.Origin("github")},
		// NO SERVER'S: a builtin, and a name nothing registered.
		{Name: "remember", Origin: mcp.OriginBuiltin, Source: mcp.OriginBuiltin},
		{Name: "ghost", Failed: true, Refusal: mcp.RefusalNotFound},
	}
	var s Spend
	var mu sync.Mutex
	s.record(phaseRecord{Phase: phase.Execute, Result: toolloop.Result{Executions: execute}})
	s.recordWorker(&mu, subagent.Result{Executions: []toolloop.Execution{
		answered("github_search", "github", false, "", 50),
	}})
	bridged := agentRunResult("done", []BridgedCall{{
		Call: ledger.Call{Name: "github_search"}, StartedAt: at.Add(time.Hour),
		Duration: 70 * time.Millisecond, Origin: mcp.Origin("github"), Server: "github",
		Source: mcp.Origin("github"),
	}}, nil, RunRecord{}, at, at.Add(2*time.Hour))
	s.record(phaseRecord{Phase: phase.Execute, Result: bridged.Result})

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

// A CONTAINED PANIC IS ITS OWN KIND, through any wrapping and whatever its value:
// it is reported as engine.panicked, so every consumer that counts unclassified
// failures (the failure loop, a lost message) must never count it again.
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
