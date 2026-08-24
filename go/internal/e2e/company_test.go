package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/observe"
)

// A golden company: two seats, one scripted model, one embedded broker.
//
// `anthropic` with a base_url rather than a `mock` provider type, deliberately.
// A mock type would be test-only machinery in shipped config — a thing an
// operator could set and a thing this suite could get right while the real
// backends were wrong. Pointing a REAL backend at a scripted endpoint tests the
// wire format, the credential pool, the tool-call decoding and the fallback
// chain, all of which sit between the model and everything asserted here.
const companyDoc = `
name: Nimbus
providers:
  llm:
    scripted:
      type: anthropic
      model: claude-golden
      base_url: %s
      api_keys: ["${CREWLET_TEST_KEY}"]
roles:
  - name: CEO
    handle: ceo
    llm: scripted
  - name: Founder
    kind: human
    contact:
      slack_user_id: U0FOUNDER
turn_engine:
  max_iterations: 1
  max_tool_rounds: 3
  plan_max_tool_rounds: 3
`

// tickInterval is the shared tick's cadence here. Short enough that the spend
// rollup lands inside a test, long enough not to drown the capture in health
// frames.
const tickInterval = 25 * time.Millisecond

// node is a running merged node: engine and API in one process, exactly as
// `crewlet run` assembles them.
type node struct {
	engine *engine.Engine
	app    *api.App
	server *httptest.Server
	model  *scriptedModel
}

// start stands one up.
//
// Everything real: a real embedded stream on a real temp directory, a real
// store, the real API in front of them, and the real observability pipeline
// wired the way cmd/crewlet wires it. The one stub is the vendor endpoint.
func start(t *testing.T) *node { return startWith(t, nil) }

// startWith stands a node up over a company document the caller may amend, for
// the cases whose subject is a config field rather than a turn.
func startWith(t *testing.T, amend func(doc string) string) *node {
	t.Helper()
	model := newScriptedModel(t)

	doc := fmt.Sprintf(companyDoc, model.url)
	if amend != nil {
		doc = amend(doc)
	}
	cfg, err := config.ParseCompany([]byte(doc))
	if err != nil {
		t.Fatalf("company config: %v", err)
	}
	boot := config.DefaultBootstrap()
	boot.Store.Path = filepath.Join(t.TempDir(), "crewlet.db")
	boot.Stream.StoreDir = filepath.Join(t.TempDir(), "stream")

	e, err := engine.New(t.Context(), engine.Options{Bootstrap: &boot, Company: cfg})
	if err != nil {
		t.Fatalf("engine.New: %v", err)
	}
	t.Cleanup(func() { e.Stop(context.Background()) })
	if err := e.Start(t.Context()); err != nil {
		t.Fatalf("engine.Start: %v", err)
	}

	app := api.New(api.Options{
		Bootstrap:    &boot,
		QueueBackend: e.Backends().Queue.Backend(),
		Sources: queries.Sources{
			Events:  e.Backends().Store.Events(),
			Company: func() *config.Company { return cfg },
		},
		// The shared tick, sped up. It owns the spend rollup — deliberately,
		// so aggregating never runs on the engine's own goroutine mid-turn —
		// and at the production five seconds a test whose turn finishes in
		// 300ms would never see one, and would then "pass" on the snapshot
		// alone while the push path went unexercised.
		HealthInterval: tickInterval,
	})
	app.SetConfigured(true)
	app.Start(t.Context())
	t.Cleanup(app.Stop)

	projector := observe.NewProjector(e.Backends().Queue, app.Stream())
	if err := projector.Start(t.Context()); err != nil {
		t.Fatalf("projector: %v", err)
	}
	t.Cleanup(func() { projector.Stop(context.Background()) })

	srv := httptest.NewServer(app)
	t.Cleanup(srv.Close)

	return &node{engine: e, app: app, server: srv, model: model}
}

// scriptedModel is an Anthropic Messages endpoint that answers by PHASE.
//
// Keyed on the tools the request offers rather than on call ORDER: a phase that
// retries, a rescue, or an extension all send another request, and a script
// that counted would answer the second Plan call with Execute's reply. What
// distinguishes the phases is what they can invoke — submit_plan, submit_review,
// or neither — which is exactly the fact the runner varies.
type scriptedModel struct {
	url string

	mu      sync.Mutex
	calls   []string
	offered []string
	// systems is the SYSTEM prompt of every call, which is where the
	// Plan-phase prefetch's blocks land. Captured because "the block was
	// rendered" and "the model was shown it" are different claims and only
	// the second one matters.
	systems []string
}

func newScriptedModel(t *testing.T) *scriptedModel {
	t.Helper()
	m := &scriptedModel{}
	srv := httptest.NewServer(http.HandlerFunc(m.serve))
	t.Cleanup(srv.Close)
	m.url = srv.URL
	return m
}

func (m *scriptedModel) serve(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	offered := offeredTools(raw)
	m.mu.Lock()
	names := make([]string, 0, len(offered))
	for n := range offered {
		names = append(names, n)
	}
	sort.Strings(names)
	m.offered = append(m.offered, strings.Join(names, "+"))
	m.systems = append(m.systems, systemPrompt(raw))
	m.mu.Unlock()

	// Keyed on the tool that DISTINGUISHES each phase, in the order that
	// makes each one unambiguous. submit_* first, because those name one
	// phase each; onboarding is then the pass that offers mark_onboarded
	// and neither submit tool.
	var reply string
	switch {
	case offered["submit_review"]:
		// Checked FIRST. Review's system prompt embeds Plan's tool log as
		// evidence, so the string "submit_plan" appears in a Review
		// request too — matching on the whole body answered Review with a
		// plan, which never submitted, which rescued into a second round.
		// Measured: [plan execute plan plan plan].
		m.saw("review")
		reply = toolUse("submit_review", map[string]any{
			"decision":       "done",
			"final_artifact": "Three PRs merged, one incident, zero regressions.",
		})
	case offered["submit_plan"]:
		m.saw("plan")
		reply = toolUse("submit_plan", map[string]any{
			"decision":  "plan",
			"reasoning": "Answer the founder with the weekly numbers.",
			// No tools: this company has no delivery tool registered, and
			// naming one would make the delivery gate judge a phantom.
			"tools_needed":     []string{},
			"steps":            []map[string]string{{"intent": "reply", "approach": "state the numbers"}},
			"success_criteria": []string{"the founder has the numbers"},
		})
	case offered["mark_onboarded"]:
		// The first-turn pass, before Plan and on its own budget. A model
		// that never marked would burn all ten rounds and retry next turn
		// — correct behaviour, and thirty seconds of it in a test, which
		// is how this case came to exist.
		m.saw("onboarding")
		reply = toolUse("mark_onboarded", map[string]any{
			"notes": "Read the team pages; deploys go out on Thursdays.",
		})
	case auxiliaryPass(raw) != "":
		// AN AUXILIARY PASS, not a phase. The prefetch's memory filter,
		// knowledge query and episode summary all reach this same
		// endpoint with no tools offered, and counting them as Execute
		// made the phase list report work that never happened — and made
		// "the first execute call" name a prompt the executor never saw.
		pass := auxiliaryPass(raw)
		m.saw("aux:" + pass)
		reply = textReply(auxiliaryAnswer(pass))
	default:
		m.saw("execute")
		reply = textReply("Three PRs merged, one incident, zero regressions.")
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, reply)
}

// auxiliaryPass names which prefetch pass a request is, or "" for a phase.
//
// Keyed on each pass's own system prompt, which is the only thing that
// distinguishes them: they share an endpoint, a model and an empty tool
// list. Matching on a phrase each prompt OPENS with, so a later edit to the
// guidance below it cannot silently turn every pass back into "execute".
func auxiliaryPass(raw []byte) string {
	system := systemPrompt(raw)
	switch {
	case strings.Contains(system, "memory-relevance filter"):
		return "memory"
	case strings.Contains(system, "into a search query for"):
		return "knowledge"
	case strings.Contains(system, "compress an AI agent's record"):
		return "recall"
	default:
		return ""
	}
}

// auxiliaryAnswer is what each pass gets back.
//
// Real answers rather than empty ones, because an empty answer is the
// degraded path and a fixture that always takes it proves the degradation
// works and nothing else.
func auxiliaryAnswer(pass string) string {
	switch pass {
	case "memory":
		// Every candidate, so a memory written by a test reaches the
		// prompt it was written for.
		return "[0, 1, 2, 3, 4, 5, 6, 7]"
	case "knowledge":
		return "staging login redirect proxy"
	default:
		return ""
	}
}

// offeredTools names the tools a Messages request actually offers.
//
// The TOOL DEFINITIONS, not the prose. What distinguishes the phases is what
// each can invoke — submit_plan, submit_review, or neither — and that is a
// structured field. Substring-matching the body reads the same names out of
// prompts that merely mention them, which is exactly how the first version of
// this answered Review with a plan.
func offeredTools(raw []byte) map[string]bool {
	var req struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	out := map[string]bool{}
	if json.Unmarshal(raw, &req) != nil {
		return out
	}
	for _, t := range req.Tools {
		out[t.Name] = true
	}
	return out
}

// offers records what each request offered, for diagnosis.
func (m *scriptedModel) offers() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.offered...)
}

func (m *scriptedModel) saw(phase string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.calls = append(m.calls, phase)
}

func (m *scriptedModel) seen() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.calls...)
}

// systemPrompts is what the model was actually shown, per call.
func (m *scriptedModel) systemPrompts() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.systems...)
}

// systemPrompt reads a Messages request's system block, which the SDK sends
// either as a string or as a list of content parts.
func systemPrompt(raw []byte) string {
	var asString struct {
		System string `json:"system"`
	}
	if json.Unmarshal(raw, &asString) == nil && asString.System != "" {
		return asString.System
	}
	var asParts struct {
		System []struct {
			Text string `json:"text"`
		} `json:"system"`
	}
	if json.Unmarshal(raw, &asParts) != nil {
		return ""
	}
	var b strings.Builder
	for _, part := range asParts.System {
		b.WriteString(part.Text)
	}
	return b.String()
}

// toolUse renders a Messages response whose content is one tool call.
func toolUse(name string, input map[string]any) string {
	args, _ := json.Marshal(input)
	return fmt.Sprintf(`{
		"id":"msg_1","type":"message","role":"assistant","model":"claude-golden",
		"content":[{"type":"tool_use","id":"call_1","name":%q,"input":%s}],
		"stop_reason":"tool_use",
		"usage":{"input_tokens":120,"output_tokens":30}
	}`, name, args)
}

// textReply renders a Messages response that is plain prose — a phase that
// finished without calling anything.
func textReply(text string) string {
	body, _ := json.Marshal(text)
	return fmt.Sprintf(`{
		"id":"msg_2","type":"message","role":"assistant","model":"claude-golden",
		"content":[{"type":"text","text":%s}],
		"stop_reason":"end_turn",
		"usage":{"input_tokens":90,"output_tokens":40}
	}`, body)
}

// waitFor polls until cond holds, failing the test if it never does.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}
