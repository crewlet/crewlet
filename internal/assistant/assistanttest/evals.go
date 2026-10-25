package assistanttest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/assistant/wire"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// EVALUATION CASES: `crewlet assistant eval -suite DIR` and the engine's own
// suite run them alike.
//
// A case is one question a person asks in a company the case describes as the
// answers its requests receive, and what the turn must and must not do. It
// runs through the real orchestrator and the curated catalogue over this
// package's rig, so a case measures the assistant a node runs, and never the
// API's authority, which the deterministic suites in internal/api hold.
//
// The file format is documented for operators in docs/reference/cli.md
// ("crewlet assistant eval"); the field tags below are its source.

// Case is one evaluation case, as a suite file states it.
type Case struct {
	// Name is the case's name, the file's own name when the file gives none.
	Name     string            `json:"name"`
	Person   CasePerson        `json:"person"`
	Page     *wire.PageContext `json:"page"`
	Question string            `json:"question"`
	Answers  []CaseAnswer      `json:"answers"`
	Script   []CaseRound       `json:"script"`
	Expect   Expect            `json:"expect"`
}

// CasePerson is who asks: the permissions they hold and the writes the
// authority table would let them propose.
type CasePerson struct {
	Apps []iam.Grant `json:"apps"`
	Acts []string    `json:"acts"`
}

// CaseAnswer is what one route answers: a pattern in net/http's grammar
// ("GET /work/{id}"), a status (200 when left out) and a JSON body.
type CaseAnswer struct {
	Route  string `json:"route"`
	Status int    `json:"status"`
	Body   any    `json:"body"`
}

// CaseRound is one round the scripted model answers with: a tool call, or
// prose. Exactly one of Call and Say.
type CaseRound struct {
	Call string         `json:"call"`
	Args map[string]any `json:"args"`
	Say  string         `json:"say"`
}

// Expect is what the turn must do. Every field is optional, and a case
// expecting nothing is refused, since it could not fail.
type Expect struct {
	// Calls are tools the turn calls, in this order, others allowed
	// between them.
	Calls []string `json:"calls"`
	// Never are tools the turn must not call.
	Never []string `json:"never"`
	// Proposals is how many proposals the turn ends with.
	Proposals *int `json:"proposals"`
	// State is how the turn ends.
	State wire.TurnState `json:"state"`
	// Says and NeverSays are text the answer contains, and must not,
	// ignoring case.
	Says      []string `json:"says"`
	NeverSays []string `json:"never_says"`
}

// empty reports whether e expects nothing.
func (e Expect) empty() bool {
	return len(e.Calls) == 0 && len(e.Never) == 0 && e.Proposals == nil && e.State == "" &&
		len(e.Says) == 0 && len(e.NeverSays) == 0
}

// LoadSuite reads every `*.yaml` case in dir, in name order, each checked.
func LoadSuite(dir string) ([]Case, error) {
	paths, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, fmt.Errorf("assistant eval: %s: %w", dir, err)
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("assistant eval: %s holds no *.yaml case", dir)
	}
	sort.Strings(paths)
	cases := make([]Case, 0, len(paths))
	names := map[string]string{}
	for _, path := range paths {
		c, err := loadCase(path)
		if err != nil {
			return nil, err
		}
		if other, seen := names[c.Name]; seen {
			return nil, fmt.Errorf("assistant eval: %s and %s are both named %q; give one "+
				"another name", other, path, c.Name)
		}
		names[c.Name] = path
		cases = append(cases, c)
	}
	return cases, nil
}

// loadCase reads one case file.
//
// THROUGH JSON, so the file's members are the wire's own names
// (`source_revision`, never a YAML spelling of the Go field) and its numbers
// are what a model's arguments carry; and STRICTLY, so a misspelt member is
// an error naming the file rather than an expectation silently not checked.
func loadCase(path string) (Case, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Case{}, fmt.Errorf("assistant eval: %w", err)
	}
	var doc any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return Case{}, fmt.Errorf("assistant eval: %s: %w", path, err)
	}
	asJSON, err := json.Marshal(doc)
	if err != nil {
		return Case{}, fmt.Errorf("assistant eval: %s: %w", path, err)
	}
	dec := json.NewDecoder(bytes.NewReader(asJSON))
	dec.DisallowUnknownFields()
	var c Case
	if err := dec.Decode(&c); err != nil {
		return Case{}, fmt.Errorf("assistant eval: %s: %w", path, err)
	}
	if c.Name == "" {
		c.Name = strings.TrimSuffix(filepath.Base(path), ".yaml")
	}
	if err := c.Validate(); err != nil {
		return Case{}, fmt.Errorf("assistant eval: %s: %w", path, err)
	}
	return c, nil
}

// Validate reports what is wrong with a case, naming the member.
func (c Case) Validate() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if strings.TrimSpace(c.Question) == "" {
		fail("question: the case asks nothing")
	}
	if c.Expect.empty() {
		fail("expect: the case expects nothing, so it cannot fail")
	}
	for i, app := range c.Person.Apps {
		if !app.Valid() {
			fail("person.apps[%d]: %q is not a permission (%v)", i, app, iam.AllGrants)
		}
	}
	if c.Page != nil {
		if err := c.Page.Validate(); err != nil {
			fail("page: %w", err)
		}
	}
	for i, a := range c.Answers {
		method, path, ok := strings.Cut(a.Route, " ")
		if !ok || method == "" || !strings.HasPrefix(path, "/") {
			fail("answers[%d].route: %q is not METHOD /path", i, a.Route)
		}
		if a.Status != 0 && (a.Status < 100 || a.Status > 599) {
			fail("answers[%d].status: %d is not an HTTP status", i, a.Status)
		}
	}
	for i, r := range c.Script {
		switch {
		case (r.Call == "") == (r.Say == ""):
			fail("script[%d]: give exactly one of call and say", i)
		case r.Call != "" && !known(r.Call):
			fail("script[%d].call: %q is not a catalogue tool", i, r.Call)
		}
	}
	for field, tools := range map[string][]string{"calls": c.Expect.Calls, "never": c.Expect.Never} {
		for _, tool := range tools {
			if !known(tool) {
				fail("expect.%s: %q is not a catalogue tool", field, tool)
			}
		}
	}
	if c.Expect.State != "" && !c.Expect.State.Valid() {
		fail("expect.state: %q is not a turn state (%v)", c.Expect.State, wire.TurnStates)
	}
	return errors.Join(errs...)
}

// known reports whether the catalogue has a tool named name.
func known(name string) bool {
	_, ok := catalog.Lookup(name)
	return ok
}

// EvalOptions are how a suite is run.
type EvalOptions struct {
	// Model is the provider every case is asked of, under ModelKey; nil
	// plays each case's own script instead.
	Model    llm.Provider
	ModelKey string
	// Trials is how many times each case runs; below one is one.
	Trials int
}

// scripted reports whether the cases play their own scripts.
func (o EvalOptions) scripted() bool { return o.Model == nil }

// patience is how long one turn is waited for: a scripted turn answers at
// once, and a real model's has the assistant's own turn timeout plus the
// time its last round takes to be recorded.
func (o EvalOptions) patience() time.Duration {
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
func RunSuite(ctx context.Context, cases []Case, o EvalOptions) ([]Result, error) {
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
func runCase(ctx context.Context, c Case, o EvalOptions) (Trial, error) {
	person := Person("eval", "eval", c.Person.Apps...)
	company, err := company(true, person.Seat)
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
	rig, err := assemble(RigOptions{Person: &person, Company: company,
		Models: &Models{Model: meter, Key: meter.key}, Catalogue: catalog.Curated{},
		Acts: c.Person.Acts, Handler: handler})
	if err != nil {
		return Trial{}, err
	}
	defer func() {
		if err := rig.close(); err != nil {
			// THE TRIAL IS JUDGED ALREADY, from frames the turn drew; a
			// service that did not stop in time is a fault of the run,
			// said where the operator reads it.
			fmt.Fprintf(os.Stderr, "assistant eval: %s: %v\n", c.Name, err)
		}
	}()
	// TITLED, so the turn's only rounds are its own: an untitled
	// conversation asks the model for a title once it is answered.
	rig.Store.Seed(rig.Person.ID, rig.Conversation, c.Name)
	accepted, err := rig.Svc.Start(ctx, assistant.TurnStart{Person: rig.Person,
		Conversation: rig.Conversation, Key: Key(c.Name), Input: c.Question, Page: c.Page,
		Read: rig.Grant(c.Name)})
	if err != nil {
		return Trial{}, err
	}
	frames, err := rig.follow(accepted.TurnID, 0, o.patience())
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
		target := r.Path
		if len(r.Query) > 0 {
			target += "?" + r.Query.Encode()
		}
		notFound := exec.Response{Status: http.StatusNotFound, Header: http.Header{},
			Body: []byte(`{"error":"not_found","message":"There is no such record here."}`)}
		req, err := http.NewRequest(r.Method, target, bytes.NewReader(r.Body))
		if err != nil {
			// A path the catalogue built that is no URL is a route no
			// answer names.
			return notFound
		}
		if _, pattern := mux.Handler(req); pattern == "" {
			return notFound
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
