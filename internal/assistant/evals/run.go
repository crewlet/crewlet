package evals

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// Options are how a suite is run.
type Options struct {
	// Model is the provider every case is asked of, under ModelKey; nil
	// plays each case's own script instead.
	Model    llm.Provider
	ModelKey string
	// Trials is how many times each case runs; below one is one.
	Trials int
}

// scripted reports whether the cases play their own scripts.
func (o Options) scripted() bool { return o.Model == nil }

// patience is how long one turn is waited for: a scripted turn answers at
// once, and a real model's has the assistant's own turn timeout plus the
// time its last round takes to be recorded.
func (o Options) patience() time.Duration {
	if o.scripted() {
		return 10 * time.Second
	}
	return config.Assistant{}.TurnTimeout() + 30*time.Second
}

// Result is one case's trials.
type Result struct {
	Case   string
	Trials []Trial
}

// Passed reports whether every trial passed: pass^k, since a person meets
// every trial and not the best of them.
func (r Result) Passed() bool {
	return len(r.Trials) > 0 && !slices.ContainsFunc(r.Trials, func(t Trial) bool {
		return len(t.Failures) > 0
	})
}

// Trial is one run of a case: what the turn did and what it failed.
type Trial struct {
	Failures  []string
	Calls     []string
	Proposals int
	State     wire.TurnState
	Answer    string
	Rounds    int
	Tokens    int
	Latencies []time.Duration
}

// RunSuite runs every case o.Trials times.
func RunSuite(ctx context.Context, cases []Case, o Options) ([]Result, error) {
	trials := max(o.Trials, 1)
	out := make([]Result, 0, len(cases))
	for _, c := range cases {
		if o.scripted() && len(c.Script) == 0 {
			return nil, fmt.Errorf("assistant eval: %s has no script; run it against a model "+
				"with -model", c.Name)
		}
		r := Result{Case: c.Name}
		for i := range trials {
			trial, err := runCase(ctx, c, o)
			if err != nil {
				return nil, fmt.Errorf("assistant eval: %s, trial %d: %w", c.Name, i+1, err)
			}
			r.Trials = append(r.Trials, trial)
		}
		out = append(out, r)
	}
	return out, nil
}

// runCase asks one case's question once and judges the turn.
func runCase(ctx context.Context, c Case, o Options) (Trial, error) {
	person := Person("eval", "eval", c.Person.Apps...)
	company, err := Company(true, person.Seat)
	if err != nil {
		return Trial{}, err
	}
	var rounds []Round
	for _, r := range c.Script {
		if r.Call != "" {
			rounds = append(rounds, Call("call-"+r.Call, r.Call, r.Args))
		} else {
			rounds = append(rounds, Say(r.Say))
		}
	}
	meter := &measured{inner: NewModel(rounds...), key: "scripted"}
	if !o.scripted() {
		meter.inner, meter.key = o.Model, o.ModelKey
	}
	handler, err := c.handler()
	if err != nil {
		return Trial{}, err
	}
	rig, err := Assemble(RigOptions{Person: &person, Company: company,
		Models: &Models{Model: meter, Key: meter.key}, Catalogue: catalog.Curated{},
		Acts: c.Person.Acts, Handler: handler})
	if err != nil {
		return Trial{}, err
	}
	defer func() {
		if closeErr := rig.Close(ctx); closeErr != nil {
			// THE TRIAL IS JUDGED ALREADY, from frames the turn drew; a
			// service that did not stop in time is a fault of the run,
			// said where the operator reads it.
			fmt.Fprintf(os.Stderr, "assistant eval: %s: %v\n", c.Name, closeErr)
		}
	}()
	// TITLED, so the turn's only rounds are its own: an untitled
	// conversation asks the model for a title once it is answered.
	rig.Store.Seed(rig.Person.ID, rig.Conversation, c.Name)
	accepted, err := rig.Svc.Start(ctx, assistant.TurnStart{Person: rig.Person,
		Conversation: rig.Conversation, Key: Key(c.Name), Input: c.Question, Page: c.page,
		Read: rig.Grant(c.Name)})
	if err != nil {
		return Trial{}, err
	}
	frames, err := rig.Await(ctx, accepted.TurnID, 0, o.patience())
	if err != nil {
		return Trial{}, err
	}
	trial := judge(c.Expect, frames)
	trial.Rounds, trial.Tokens, trial.Latencies = meter.read()
	return trial, nil
}

// handler answers each request from the case's answers, by net/http's own
// pattern matching, and 404 in the engine's envelope for any other.
func (c Case) handler() (func(exec.Request) exec.Response, error) {
	mux := http.NewServeMux()
	for _, a := range c.Answers {
		status := a.Status
		if status == 0 {
			status = http.StatusOK
		}
		body, err := json.Marshal(a.Body)
		if err != nil {
			return nil, fmt.Errorf("answers %s: the body does not encode: %w", a.Route, err)
		}
		if err := register(mux, a.Route, status, body); err != nil {
			return nil, err
		}
	}
	return func(r exec.Request) exec.Response {
		// A REQUEST VALUE AND NO TRANSPORT: the case's answers are matched by
		// net/http's own pattern grammar, in this process, and nothing is
		// sent anywhere (internal/assistant's dispatcher walk).
		req := &http.Request{Method: r.Method, Header: http.Header{},
			URL:  &url.URL{Path: r.Path, RawQuery: r.Query.Encode()},
			Body: io.NopCloser(bytes.NewReader(r.Body))}
		if _, pattern := mux.Handler(req); pattern == "" {
			return exec.Response{Status: http.StatusNotFound, Header: http.Header{},
				Body: []byte(`{"error":"not_found","message":"There is no such record here."}`)}
		}
		w := &answerWriter{header: http.Header{}, status: http.StatusOK}
		mux.ServeHTTP(w, req)
		return exec.Response{Status: w.status, Header: w.header, Body: w.body.Bytes()}
	}, nil
}

// answerWriter keeps what a case's answer wrote. Not net/http/httptest's
// recorder, which links the testing package into the binary that runs these
// cases.
type answerWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (w *answerWriter) Header() http.Header         { return w.header }
func (w *answerWriter) WriteHeader(status int)      { w.status = status }
func (w *answerWriter) Write(p []byte) (int, error) { return w.body.Write(p) }

// register mounts one answer, answering a pattern net/http refuses (a
// duplicate, a malformed wildcard) as an error naming it rather than its
// panic.
func register(mux *http.ServeMux, route string, status int, body []byte) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("answers: %s is not a route pattern: %v", route, r)
		}
	}()
	mux.HandleFunc(route, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		// An answerWriter's writes go to a buffer, which cannot fail.
		_, _ = w.Write(body)
	})
	return nil
}

// judge reads what a turn did out of its frames and holds it to e.
func judge(e Expect, frames []wire.Frame) Trial {
	var t Trial
	seen := map[string]bool{}
	var answer strings.Builder
	for _, f := range frames {
		switch f.Kind {
		case wire.EventStep:
			if id := f.Step.StepID.String(); !seen[id] {
				seen[id] = true
				t.Calls = append(t.Calls, f.Step.Tool)
			}
		case wire.EventCard:
			if f.Card.Card.Proposal != nil {
				t.Proposals++
			}
		case wire.EventDelta:
			answer.WriteString(f.Delta.Text)
		case wire.EventDone:
			t.State = f.Done.State
		}
	}
	t.Answer = answer.String()
	fail := func(format string, args ...any) { t.Failures = append(t.Failures, fmt.Sprintf(format, args...)) }
	next := 0
	for _, call := range t.Calls {
		if next < len(e.Calls) && call == e.Calls[next] {
			next++
		}
	}
	if next < len(e.Calls) {
		fail("calls: %s not called in order (called %v)", e.Calls[next], t.Calls)
	}
	for _, tool := range e.Never {
		if slices.Contains(t.Calls, tool) {
			fail("never: %s called", tool)
		}
	}
	if e.Proposals != nil && t.Proposals != *e.Proposals {
		fail("proposals: %d, want %d", t.Proposals, *e.Proposals)
	}
	if e.State != "" && t.State != e.State {
		fail("state: %s, want %s", t.State, e.State)
	}
	lower := strings.ToLower(t.Answer)
	for _, s := range e.Says {
		if !strings.Contains(lower, strings.ToLower(s)) {
			fail("says: %q not in the answer", s)
		}
	}
	for _, s := range e.NeverSays {
		if strings.Contains(lower, strings.ToLower(s)) {
			fail("never_says: %q in the answer", s)
		}
	}
	return t
}

// measured is a provider whose rounds are counted: how many, their tokens
// and how long each took.
type measured struct {
	inner llm.Provider
	key   string

	mu        sync.Mutex
	rounds    int
	tokens    int
	latencies []time.Duration
}

func (m *measured) Model() string { return m.inner.Model() }

func (m *measured) Complete(ctx context.Context, req llm.Request) (*llm.Completion, error) {
	began := time.Now()
	c, err := m.inner.Complete(ctx, req)
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rounds++
	m.latencies = append(m.latencies, time.Since(began))
	if c != nil {
		m.tokens += c.InputTokens + c.OutputTokens
	}
	return c, err
}

func (m *measured) read() (int, int, []time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rounds, m.tokens, slices.Clone(m.latencies)
}
