package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
func start(t *testing.T) *node {
	t.Helper()
	model := newScriptedModel(t)

	cfg, err := config.ParseCompany([]byte(fmt.Sprintf(companyDoc, model.url)))
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
		Sources:      queries.Sources{Events: e.Backends().Store.Events()},
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

	mu    sync.Mutex
	calls []string
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
	default:
		m.saw("execute")
		reply = textReply("Three PRs merged, one incident, zero regressions.")
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = io.WriteString(w, reply)
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
