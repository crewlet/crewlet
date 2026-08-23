package chain

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/logging"
	"github.com/crewlet/crewlet/internal/providers/credential"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

func TestMain(m *testing.M) {
	logging.Configure(slog.LevelError, logging.FormatText, io.Discard)
	// The package logger bound its handler at package-var init, which
	// runs before this. Rebind it or every case prints its own log.
	log = logging.Get("llm.chain")
	os.Exit(m.Run())
}

// fake is a member that answers or fails on command.
type fake struct {
	model string
	err   error
	text  string
	calls atomic.Int32
}

func (f *fake) Model() string { return f.model }

func (f *fake) Complete(context.Context, llm.Request) (*llm.Completion, error) {
	f.calls.Add(1)
	if f.err != nil {
		return nil, f.err
	}
	return &llm.Completion{Content: f.text, InputTokens: 1, OutputTokens: 1}, nil
}

func answering(model, text string) *fake { return &fake{model: model, text: text} }

func failing(model string, kind llm.ErrorKind) *fake {
	return &fake{model: model, err: &llm.Error{
		Kind: kind, Provider: "p", Model: model, Err: fmt.Errorf("%s from %s", kind, model),
	}}
}

func build(t *testing.T, opts Options, members ...Member) *Chain {
	t.Helper()
	c, err := New(members, opts)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c
}

func member(key string, p llm.Provider) Member { return Member{Key: key, Provider: p} }

// --- construction ------------------------------------------------------

func TestNewRefusesAnUnusableChain(t *testing.T) {
	t.Parallel()
	// A seat wired to no model fails on its first turn with an error about
	// a nil provider, naming neither the seat nor the config.
	if _, err := New(nil, Options{}); err == nil {
		t.Fatal("New accepted an empty chain")
	}
	if _, err := New([]Member{{Key: "primary"}}, Options{}); err == nil {
		t.Fatal("New accepted a member with no provider")
	}
	_, err := New([]Member{member("a", answering("m", "x")), {Key: "b"}}, Options{})
	if err == nil || !strings.Contains(err.Error(), `"b"`) {
		t.Fatalf("err = %v, want it to name the offending member", err)
	}
}

// The zero value is not a usable chain, and it has to say so: `Chain{}`
// compiles anywhere, and an exhausted-chain error over no members names
// neither a provider nor a cause.
func TestTheZeroChainRefusesRatherThanReportingAnEmptyExhaustion(t *testing.T) {
	t.Parallel()
	var zero Chain
	if got := zero.Model(); got != "" {
		t.Fatalf("Model() = %q on a zero chain", got)
	}
	out, err := zero.Complete(context.Background(), llm.Request{})
	if out != nil {
		t.Fatalf("Complete returned %+v from a zero chain", out)
	}
	if got := llm.KindOf(err); got != llm.KindFatal {
		t.Fatalf("classified %s, want fatal", got)
	}
	if !strings.Contains(err.Error(), "chain.New") {
		t.Fatalf("err = %v, want it to name the way out", err)
	}
	var chainErr *Error
	if errors.As(err, &chainErr) {
		t.Fatal("an unbuilt chain reported itself as an exhausted one")
	}
}

// Telemetry reads Model() for the call in flight, so a chain that has not yet
// answered must still name a model rather than an empty string.
func TestModelBeforeAnyCallIsTheHead(t *testing.T) {
	t.Parallel()
	c := build(t, Options{},
		member("primary", answering("head-model", "x")),
		member("backup", answering("backup-model", "y")))
	if got := c.Model(); got != "head-model" {
		t.Fatalf("Model() = %q, want the head's", got)
	}
	if got := fmt.Sprint(c.Keys()); got != "[primary backup]" {
		t.Fatalf("Keys() = %v", got)
	}
}

// --- walking the chain -------------------------------------------------

func TestHeadAnswersAndNothingElseIsCalled(t *testing.T) {
	t.Parallel()
	head, backup := answering("head-model", "from head"), answering("backup-model", "y")
	c := build(t, Options{}, member("primary", head), member("backup", backup))

	out, err := c.Complete(context.Background(), llm.Request{})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if out.Content != "from head" {
		t.Fatalf("Content = %q", out.Content)
	}
	if backup.calls.Load() != 0 {
		t.Fatal("the backup was called even though the head answered")
	}
	if got := c.Model(); got != "head-model" {
		t.Fatalf("Model() = %q", got)
	}
}

// The contract's reason for Model() being a method: telemetry reads it
// directly, and a chain reporting its own name instead of the model that
// answered makes the per-model token breakdown wrong.
func TestModelReportsTheMemberThatActuallyServed(t *testing.T) {
	t.Parallel()
	for _, kind := range []llm.ErrorKind{
		llm.KindRateLimit, llm.KindAuth, llm.KindServer, llm.KindTimeout,
	} {
		t.Run(kind.String(), func(t *testing.T) {
			t.Parallel()
			backup := answering("backup-model", "from backup")
			c := build(t, Options{},
				member("primary", failing("head-model", kind)),
				member("backup", backup))

			out, err := c.Complete(context.Background(), llm.Request{})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if out.Content != "from backup" {
				t.Fatalf("Content = %q", out.Content)
			}
			if got := c.Model(); got != "backup-model" {
				t.Fatalf("Model() = %q, want the model that answered", got)
			}
		})
	}
}

// An exhausted credential pool is a retryable failure like any other. Python
// needed a dedicated exception and a dedicated catch here; the classification
// carried on the error is enough.
func TestAnExhaustedPoolFallsThroughWithNoSpecialCase(t *testing.T) {
	t.Parallel()
	exhausted := &llm.Error{
		Kind: llm.KindRateLimit, Provider: "anthropic", Model: "head-model",
		Err: fmt.Errorf("all 2 credentials cooling: %w", credential.ErrExhausted),
	}
	c := build(t, Options{},
		member("primary", &fake{model: "head-model", err: exhausted}),
		member("backup", answering("backup-model", "ok")))

	out, err := c.Complete(context.Background(), llm.Request{})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if out.Content != "ok" || c.Model() != "backup-model" {
		t.Fatalf("Content = %q, Model = %q", out.Content, c.Model())
	}
}

// A malformed request or a content refusal will be refused identically by
// every member, so trying them is two more seconds and another log line
// saying the same thing.
func TestAFatalFailureStopsTheWalk(t *testing.T) {
	t.Parallel()
	backup := answering("backup-model", "y")
	c := build(t, Options{},
		member("primary", failing("head-model", llm.KindFatal)),
		member("backup", backup))

	_, err := c.Complete(context.Background(), llm.Request{})
	if backup.calls.Load() != 0 {
		t.Fatal("a fatal failure still walked to the next member")
	}
	// Returned as it came, so errors.As still reaches the backend's own
	// error with its provider, model and status intact.
	var classified *llm.Error
	if !errors.As(err, &classified) || classified.Model != "head-model" {
		t.Fatalf("err = %v, want the member's own classified error", err)
	}
	var chainErr *Error
	if errors.As(err, &chainErr) {
		t.Fatal("a fatal failure was dressed up as an exhausted chain")
	}
}

// KindOf answers KindFatal for anything it did not classify, and the chain
// must honour that: walking every member for a request none can serve turns
// one clear failure into N slow ones.
func TestAnUnclassifiedFailureIsTreatedAsFatal(t *testing.T) {
	t.Parallel()
	backup := answering("backup-model", "y")
	c := build(t, Options{},
		member("primary", &fake{model: "head-model", err: errors.New("something odd")}),
		member("backup", backup))

	_, err := c.Complete(context.Background(), llm.Request{})
	if backup.calls.Load() != 0 {
		t.Fatal("an unrecognised failure walked the whole chain")
	}
	if !strings.Contains(err.Error(), "something odd") {
		t.Fatalf("err = %v, want the original", err)
	}
}

func TestEveryMemberFailingReportsAnExhaustedChain(t *testing.T) {
	t.Parallel()
	c := build(t, Options{},
		member("primary", failing("head-model", llm.KindServer)),
		member("secondary", failing("mid-model", llm.KindRateLimit)),
		member("backup", failing("backup-model", llm.KindAuth)))

	_, err := c.Complete(context.Background(), llm.Request{})
	var chainErr *Error
	if !errors.As(err, &chainErr) {
		t.Fatalf("err = %v, want a *chain.Error", err)
	}
	if got := fmt.Sprint(chainErr.Attempted); got != "[primary secondary backup]" {
		t.Fatalf("Attempted = %v", got)
	}
	// The chain carries no kind of its own: the last member's classified
	// error is right there under Unwrap.
	if got := llm.KindOf(err); got != llm.KindAuth {
		t.Fatalf("KindOf = %s, want the last member's", got)
	}
	if !strings.Contains(err.Error(), "primary -> secondary -> backup") {
		t.Fatalf("Error() = %q, want the attempted order", err.Error())
	}
}

// --- the fallback hook -------------------------------------------------

// Python passed the literal string "next" at both call sites, so every
// ProviderFallback event ever published recorded to_provider_key="next": the
// one field that says which provider took over, naming no provider at all.
func TestFallbackHookNamesTheProviderThatTookOver(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var seen []Fallback
	opts := Options{OnFallback: func(f Fallback) {
		mu.Lock()
		defer mu.Unlock()
		seen = append(seen, f)
	}}
	c := build(t, opts,
		member("primary", failing("head-model", llm.KindRateLimit)),
		member("secondary", failing("mid-model", llm.KindServer)),
		member("backup", answering("backup-model", "ok")))

	if _, err := c.Complete(context.Background(), llm.Request{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(seen) != 2 {
		t.Fatalf("fired %d hooks, want one per failed member", len(seen))
	}
	if seen[0].From != "primary" || seen[0].To != "secondary" || seen[0].Kind != llm.KindRateLimit {
		t.Fatalf("first hand-off = %+v", seen[0])
	}
	if seen[1].From != "secondary" || seen[1].To != "backup" || seen[1].Kind != llm.KindServer {
		t.Fatalf("second hand-off = %+v", seen[1])
	}
	if seen[0].Err == nil || seen[1].Err == nil {
		t.Fatal("a hand-off carried no error")
	}
}

// On the last member the fallback is to nothing, and the hook must say so
// rather than naming a provider that does not exist.
func TestTheLastMembersHandOffNamesNoSuccessor(t *testing.T) {
	t.Parallel()
	var seen []Fallback
	c := build(t, Options{OnFallback: func(f Fallback) { seen = append(seen, f) }},
		member("primary", failing("head-model", llm.KindServer)),
		member("backup", failing("backup-model", llm.KindServer)))

	_, _ = c.Complete(context.Background(), llm.Request{})
	if len(seen) != 2 {
		t.Fatalf("fired %d hooks", len(seen))
	}
	if seen[1].To != "" {
		t.Fatalf("last hand-off To = %q, want empty", seen[1].To)
	}
}

func TestNoHookIsFineAndAFatalFiresNone(t *testing.T) {
	t.Parallel()
	fired := 0
	c := build(t, Options{OnFallback: func(Fallback) { fired++ }},
		member("primary", failing("head-model", llm.KindFatal)),
		member("backup", answering("backup-model", "y")))
	_, _ = c.Complete(context.Background(), llm.Request{})
	if fired != 0 {
		t.Fatalf("fired %d hooks for a failure that did not hand off", fired)
	}
}

// --- composition and concurrency --------------------------------------

// The chain is itself an llm.Provider, so a chain can be a member of one, and
// the classification has to survive both wrappers.
func TestAChainCanBeAMemberOfAChain(t *testing.T) {
	t.Parallel()
	inner := build(t, Options{},
		member("inner-primary", failing("inner-head", llm.KindRateLimit)),
		member("inner-backup", failing("inner-backup", llm.KindAuth)))
	outer := build(t, Options{},
		member("inner", inner),
		member("outer-backup", answering("outer-backup-model", "ok")))

	out, err := outer.Complete(context.Background(), llm.Request{})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if out.Content != "ok" {
		t.Fatalf("Content = %q", out.Content)
	}
	if got := outer.Model(); got != "outer-backup-model" {
		t.Fatalf("Model() = %q", got)
	}

	// And when everything fails, the innermost classification still reaches
	// the top through both Unwraps.
	deadOuter := build(t, Options{}, member("inner", inner))
	_, err = deadOuter.Complete(context.Background(), llm.Request{})
	if got := llm.KindOf(err); got != llm.KindAuth {
		t.Fatalf("KindOf = %s, want the innermost last failure", got)
	}
}

func TestConcurrentCallsAreSafe(t *testing.T) {
	t.Parallel()
	c := build(t, Options{OnFallback: func(Fallback) {}},
		member("primary", failing("head-model", llm.KindRateLimit)),
		member("backup", answering("backup-model", "ok")))

	var wg sync.WaitGroup
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Complete(context.Background(), llm.Request{}); err != nil {
				t.Error(err)
			}
			// Model() is read from the goroutine that just called
			// Complete — the race this is guarding.
			if got := c.Model(); got != "backup-model" {
				t.Errorf("Model() = %q", got)
			}
		}()
	}
	wg.Wait()
}
