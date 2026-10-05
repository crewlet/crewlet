package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	sdk "github.com/anthropics/anthropic-sdk-go"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/anthropic/claudemodel"
)

// --- harness -----------------------------------------------------------

// doctorAPI is an endpoint serving both routes the doctor reads: the Models
// API's record and the Messages API's round. Every request is recorded, so
// the shape of the round is asserted on the wire.
type doctorAPI struct {
	mu       sync.Mutex
	seen     []doctorRequest
	models   func(w http.ResponseWriter, id string)
	messages func(w http.ResponseWriter)
}

type doctorRequest struct {
	method, path, apiKey string
	body                 map[string]any
	raw                  []byte
}

func (a *doctorAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	a.mu.Lock()
	a.seen = append(a.seen, doctorRequest{
		method: r.Method, path: r.URL.Path, apiKey: r.Header.Get("X-Api-Key"), body: body, raw: raw,
	})
	a.mu.Unlock()
	switch {
	case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/v1/models/"):
		a.models(w, strings.TrimPrefix(r.URL.Path, "/v1/models/"))
	case r.Method == http.MethodPost && r.URL.Path == "/v1/messages":
		a.messages(w)
	default:
		http.NotFound(w, r)
	}
}

func (a *doctorAPI) requests(method string) []doctorRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []doctorRequest
	for _, r := range a.seen {
		if r.method == method {
			out = append(out, r)
		}
	}
	return out
}

func serveDoctor(t *testing.T, models func(http.ResponseWriter, string), messages func(http.ResponseWriter)) (*doctorAPI, string) {
	t.Helper()
	api := &doctorAPI{models: models, messages: messages}
	srv := httptest.NewServer(api)
	t.Cleanup(srv.Close)
	return api, srv.URL
}

// record is a Models API record. adaptive and enabled are the two thinking
// types; efforts are the levels the model takes, every other level reported
// unsupported.
func record(id string, maxTokens int, adaptive, enabled bool, efforts ...claudemodel.Effort) string {
	levels := map[string]any{"supported": len(efforts) > 0}
	for _, level := range claudemodel.Efforts {
		levels[string(level)] = map[string]any{"supported": slices.Contains(efforts, level)}
	}
	body, _ := json.Marshal(map[string]any{
		"id": id, "type": "model", "display_name": "Claude Test",
		"created_at": "2026-01-01T00:00:00Z", "max_tokens": maxTokens, "max_input_tokens": 1000000,
		"capabilities": map[string]any{
			"effort": levels,
			"thinking": map[string]any{"supported": adaptive || enabled, "types": map[string]any{
				"adaptive": map[string]any{"supported": adaptive},
				"enabled":  map[string]any{"supported": enabled},
			}},
		},
	})
	return string(body)
}

// currentRecord is what the API says about a current-generation model, in
// full agreement with the table.
func currentRecord(id string) string {
	return record(id, 128000, true, false, claudemodel.Efforts...)
}

func servesRecord(body string) func(http.ResponseWriter, string) {
	return func(w http.ResponseWriter, _ string) { writeJSON(w, http.StatusOK, body) }
}

func notFound(w http.ResponseWriter, _ string) {
	http.Error(w, "404 page not found", http.StatusNotFound)
}

// callsTheTool streams a round that calls name.
func callsTheTool(name string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		writeStream(w, streamOf(
			streamStart(),
			sseEvent{"content_block_start", fmt.Sprintf(`{"type":"content_block_start","index":0,`+
				`"content_block":{"type":"tool_use","id":"tu_1","name":%q,"input":{}}}`, name)},
			sseEvent{"content_block_delta", `{"type":"content_block_delta","index":0,` +
				`"delta":{"type":"input_json_delta","partial_json":"{\"ok\":true}"}}`},
			sseEvent{"content_block_stop", `{"type":"content_block_stop","index":0}`},
			streamEnd("tool_use"),
		)...)
	}
}

// answersInProse streams a round that writes text and calls nothing.
func answersInProse(text string) func(http.ResponseWriter) {
	return func(w http.ResponseWriter) {
		if text == "" {
			writeStream(w, streamOf(streamStart(), streamEnd("end_turn"))...)
			return
		}
		writeStream(w, streamOf(streamStart(), textBlock(0, text), streamEnd("end_turn"))...)
	}
}

func refuses(t *testing.T) func(http.ResponseWriter) {
	return func(http.ResponseWriter) { t.Error("the doctor sent a round it was told not to") }
}

func doctorProvider(t *testing.T, baseURL, model string, mutate func(*Config)) *Provider {
	t.Helper()
	return newProvider(t, baseURL, func(c *Config) {
		c.Model = model
		if mutate != nil {
			mutate(c)
		}
	})
}

// --- the round ---------------------------------------------------------

// THE DOCTOR'S ROUND IS A PHASE'S ROUND. It certifies that a model asked to
// call a tool calls it — which is what every phase that finishes by a call
// rests on — so it must be asked the way a phase asks: the tool offered, no
// choice forced (a 400 on Opus 5.5), the entry's own adaptive thinking,
// streamed, with only the effort lowered.
func TestTheDoctorsRoundIsTheShapeAPhaseSends(t *testing.T) {
	t.Parallel()
	api, url := serveDoctor(t, servesRecord(currentRecord("claude-opus-5-5")), callsTheTool(smokeTool))
	p := doctorProvider(t, url, "claude-opus-5-5", nil)

	d := p.Diagnose(context.Background(), DiagnoseOptions{Key: "claude", Smoke: true})
	if !d.Healthy() {
		t.Fatalf("problems on a model that agrees with the table and calls the tool: %v", d.Problems)
	}
	if !strings.HasPrefix(d.Smoke, "ok — called "+smokeTool+" (streamed)") {
		t.Errorf("smoke = %q", d.Smoke)
	}
	if len(d.Drift) != 0 {
		t.Errorf("drift on a record that agrees with the table: %v", d.Drift)
	}

	rounds := api.requests(http.MethodPost)
	if len(rounds) != 1 {
		t.Fatalf("%d rounds, want one", len(rounds))
	}
	body := rounds[0].body
	if _, forced := body["tool_choice"]; forced {
		t.Errorf("the round forced a tool choice, which no phase does: %v", body["tool_choice"])
	}
	if got := dig(t, body, "thinking", "type"); got != "adaptive" {
		t.Errorf("thinking.type = %v, want the entry's adaptive thinking", got)
	}
	if got := dig(t, body, "thinking", "display"); got != "summarized" {
		t.Errorf("thinking.display = %v", got)
	}
	if got := dig(t, body, "output_config", "effort"); got != "low" {
		t.Errorf("effort = %v, want low: a one-call answer is worth no more", got)
	}
	if body["stream"] != true {
		t.Errorf("stream = %v, want the round streamed as a phase's is", body["stream"])
	}
	if _, ok := body["temperature"]; ok {
		t.Errorf("a temperature was sent to a model that refuses one")
	}
	tools, _ := body["tools"].([]any)
	if len(tools) != 1 || tools[0].(map[string]any)["name"] != smokeTool {
		t.Errorf("tools = %v, want %s offered", tools, smokeTool)
	}
	if !bytes.Contains(rounds[0].raw, []byte("Call the "+smokeTool+" tool")) {
		t.Errorf("the instruction does not name the tool: %s", rounds[0].raw)
	}
}

// AN ENDPOINT THAT DOES NOT STREAM still passes, and says so: phases fall back
// to a unary call per round, which works, and an operator watching an empty
// live view wants to know why.
func TestTheDoctorSaysWhenTheEndpointDoesNotStream(t *testing.T) {
	t.Parallel()
	_, url := serveDoctor(t, notFound, func(w http.ResponseWriter) {
		writeJSON(w, http.StatusOK, `{"id":"msg_1","type":"message","role":"assistant",`+
			`"model":"claude-test","content":[{"type":"tool_use","id":"tu_1","name":"`+smokeTool+
			`","input":{"ok":true}}],"stop_reason":"tool_use","usage":{"input_tokens":10,"output_tokens":5}}`)
	})
	d := doctorProvider(t, url, "claude-opus-5-5", nil).
		Diagnose(context.Background(), DiagnoseOptions{Key: "claude", Smoke: true})
	if !d.Healthy() || !strings.Contains(d.Smoke, "does not stream") {
		t.Fatalf("smoke = %q, problems %v", d.Smoke, d.Problems)
	}
}

// A MODEL THAT ANSWERS IN PROSE FAILS THE DOCTOR, and an empty round is told
// apart from prose: one misread the instruction, the other said nothing.
func TestARoundWithoutTheCallFailsTheDoctor(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, said, want string
	}{
		{"prose", "Sure, ok is true.", "answered in prose"},
		{"silence", "", "no call to " + smokeTool + " and no text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, url := serveDoctor(t, notFound, answersInProse(tc.said))
			d := doctorProvider(t, url, "claude-opus-5-5", nil).
				Diagnose(context.Background(), DiagnoseOptions{Key: "claude", Smoke: true})
			if d.Healthy() {
				t.Fatal("a round that called nothing passed the doctor")
			}
			if !strings.HasPrefix(d.Smoke, "failed") || !strings.Contains(d.Smoke, tc.want) {
				t.Errorf("smoke = %q, want it to say %q", d.Smoke, tc.want)
			}
		})
	}
}

// A CALL TO SOME OTHER TOOL IS NOT THE CALL THAT WAS ASKED FOR.
func TestACallToTheWrongToolFailsTheDoctor(t *testing.T) {
	t.Parallel()
	_, url := serveDoctor(t, notFound, callsTheTool("something_else"))
	d := doctorProvider(t, url, "claude-opus-5-5", nil).
		Diagnose(context.Background(), DiagnoseOptions{Key: "claude", Smoke: true})
	if d.Healthy() {
		t.Fatalf("a call to another tool certified the channel: %q", d.Smoke)
	}
}

// -no-smoke SENDS NO ROUND, and still reads the Models API, which bills
// nothing.
func TestNoSmokeSendsNoRoundButStillReadsTheModel(t *testing.T) {
	t.Parallel()
	api, url := serveDoctor(t, servesRecord(currentRecord("claude-opus-5-5")), refuses(t))
	d := doctorProvider(t, url, "claude-opus-5-5", nil).
		Diagnose(context.Background(), DiagnoseOptions{Key: "claude", Smoke: false})
	if d.Smoke != "skipped (-no-smoke)" {
		t.Errorf("smoke = %q", d.Smoke)
	}
	if got := api.requests(http.MethodGet); len(got) != 1 {
		t.Fatalf("%d Models API reads, want one", len(got))
	}
	if !d.Healthy() {
		t.Errorf("problems: %v", d.Problems)
	}
}

// --- the Models API ----------------------------------------------------

// THE RECORD IS READ UNDER THE NAME THE TABLE READS, through the pool's key: a
// Bedrock or dated spelling is the model it spells.
func TestTheModelIsReadUnderItsNormalizedID(t *testing.T) {
	t.Parallel()
	api, url := serveDoctor(t, servesRecord(record("claude-opus-4-1", 32000, false, true)), refuses(t))
	d := doctorProvider(t, url, "us.anthropic.claude-opus-4-1-20250805-v1:0", nil).
		Diagnose(context.Background(), DiagnoseOptions{Key: "claude"})
	reads := api.requests(http.MethodGet)
	if len(reads) != 1 || reads[0].path != "/v1/models/claude-opus-4-1" {
		t.Fatalf("reads = %+v, want /v1/models/claude-opus-4-1", reads)
	}
	if reads[0].apiKey != "k1" {
		t.Errorf("x-api-key = %q, want the pool's key", reads[0].apiKey)
	}
	if d.Profile != "claude-opus-4-1" || !strings.HasPrefix(d.ModelsAPI, "served — Claude Test (claude-opus-4-1)") {
		t.Errorf("profile %q, models api %q", d.Profile, d.ModelsAPI)
	}
	if !d.Healthy() {
		t.Errorf("problems: %v", d.Problems)
	}
}

// A GATEWAY THAT DOES NOT SERVE /v1/models IS NOT A FAILURE. The entry's
// requests never read it, and the round is what says whether the model
// answers — so "not served" is reported and nothing is a problem.
func TestAnEndpointWithoutTheModelsAPIIsNotAFailure(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		models func(http.ResponseWriter, string)
	}{
		{"404", notFound},
		{"405", func(w http.ResponseWriter, _ string) {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}},
		{"501", func(w http.ResponseWriter, _ string) {
			http.Error(w, "not implemented", http.StatusNotImplemented)
		}},
		{"vendor not_found", func(w http.ResponseWriter, _ string) {
			writeJSON(w, http.StatusNotFound, apiError("not_found_error"))
		}},
		{"200 without a record", servesRecord(`{}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, url := serveDoctor(t, tc.models, callsTheTool(smokeTool))
			d := doctorProvider(t, url, "claude-opus-5-5", nil).
				Diagnose(context.Background(), DiagnoseOptions{Key: "claude", Smoke: true})
			if !strings.HasPrefix(d.ModelsAPI, "not served") {
				t.Errorf("models api = %q", d.ModelsAPI)
			}
			if !d.Healthy() {
				t.Errorf("an endpoint without the Models API was a problem: %v", d.Problems)
			}
		})
	}
}

// A MODELS API THAT FAILS FOR ANY OTHER REASON IS A PROBLEM: under -no-smoke
// it is the only thing that touched the endpoint, so a refused key or a dead
// endpoint would otherwise pass a deploy gate.
func TestAModelsAPIFailureIsAProblem(t *testing.T) {
	t.Parallel()
	for _, kind := range []string{"authentication_error", "overloaded_error"} {
		t.Run(kind, func(t *testing.T) {
			t.Parallel()
			status := http.StatusUnauthorized
			if kind == "overloaded_error" {
				status = 529
			}
			_, url := serveDoctor(t, func(w http.ResponseWriter, _ string) {
				writeJSON(w, status, apiError(kind))
			}, refuses(t))
			d := doctorProvider(t, url, "claude-opus-5-5", nil).
				Diagnose(context.Background(), DiagnoseOptions{Key: "claude"})
			if d.Healthy() || !strings.HasPrefix(d.ModelsAPI, "failed") {
				t.Fatalf("models api %q, problems %v", d.ModelsAPI, d.Problems)
			}
		})
	}
}

// decodeRecord is a Models API record as the SDK reads it.
func decodeRecord(t *testing.T, body string) *sdk.ModelInfo {
	t.Helper()
	var info sdk.ModelInfo
	if err := json.Unmarshal([]byte(body), &info); err != nil {
		t.Fatalf("record: %v", err)
	}
	return &info
}

// DRIFT IS JUDGED BY WHAT THE ENTRY SENDS. A disagreement that puts a field
// the model refuses on the wire is a problem — every call on the entry fails —
// while one where the table is merely more cautious is a note. And a field the
// record does not carry is not compared at all, or a gateway's thinner record
// would read as a model that takes nothing.
func TestDriftIsJudgedByWhatTheEntrySends(t *testing.T) {
	t.Parallel()
	low, medium, high, maxEffort := claudemodel.EffortLow, claudemodel.EffortMedium,
		claudemodel.EffortHigh, claudemodel.EffortMax
	opus55, _ := claudemodel.Lookup("claude-opus-5-5")
	haiku, _ := claudemodel.Lookup("claude-haiku-4-5")
	for _, tc := range []struct {
		name    string
		profile claudemodel.Profile
		effort  llm.Effort
		budget  int64
		record  string
		// want is each finding, "!" marking one that breaks.
		want []string
	}{
		{name: "agreement", profile: opus55, effort: llm.EffortHigh,
			record: currentRecord("claude-opus-5-5")},
		{name: "adaptive refused", profile: opus55, effort: llm.EffortHigh,
			record: record("x", 128000, false, true, claudemodel.Efforts...),
			want:   []string{"!adaptive"}},
		{name: "ceiling below the table's", profile: opus55, effort: llm.EffortHigh,
			record: record("x", 64000, true, false, claudemodel.Efforts...),
			want:   []string{"!max_tokens 128000"}},
		{name: "ceiling above the table's", profile: opus55, effort: llm.EffortHigh,
			record: record("x", 200000, true, false, claudemodel.Efforts...),
			want:   []string{"allows max_tokens 200000"}},
		{name: "a level above the entry's is never sent", profile: opus55, effort: llm.EffortHigh,
			record: record("x", 128000, true, false, low, medium, high, maxEffort),
			want:   []string{`effort "xhigh"`}},
		{name: "a level at or below the entry's is", profile: opus55, effort: llm.EffortMax,
			record: record("x", 128000, true, false, low, medium, high, maxEffort),
			want:   []string{`!effort "xhigh"`}},
		{name: "no effort at all", profile: opus55, effort: llm.EffortMedium,
			record: record("x", 128000, true, false),
			want:   []string{`!effort "low"`, `!effort "medium"`, `effort "high"`, `effort "xhigh"`, `effort "max"`}},
		{name: "an effort capability reported off with no levels", profile: opus55, effort: llm.EffortMedium,
			record: `{"id":"x","max_tokens":128000,"capabilities":{"effort":{"supported":false}}}`,
			want:   []string{`!effort "low"`, `!effort "medium"`, `effort "high"`, `effort "xhigh"`, `effort "max"`}},
		{name: "a budget model the API says thinks adaptively", profile: haiku,
			record: record("x", 64000, true, true),
			want:   []string{"thinks adaptively"}},
		{name: "a budget the model refuses", profile: haiku, budget: 2048,
			record: record("x", 64000, false, false),
			want:   []string{"!2048-token budget"}},
		{name: "no budget sends no thinking", profile: haiku,
			record: record("x", 64000, false, false)},
		{name: "a level the table does not list", profile: haiku,
			record: record("x", 64000, false, true, low),
			want:   []string{`offers effort "low"`}},
		{name: "a thin record compares only what it carries", profile: opus55, effort: llm.EffortHigh,
			record: `{"id":"x","max_tokens":128000}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := drift(tc.profile, tc.effort, tc.budget, decodeRecord(t, tc.record))
			if len(got) != len(tc.want) {
				t.Fatalf("findings = %v, want %v", got, tc.want)
			}
			for i, want := range tc.want {
				breaks := strings.HasPrefix(want, "!")
				want = strings.TrimPrefix(want, "!")
				if got[i].breaks != breaks || !strings.Contains(got[i].text, want) {
					t.Errorf("finding %d = %v, want breaks=%v containing %q", i, got[i], breaks, want)
				}
			}
		})
	}
}

// A BREAKING DRIFT IS A PROBLEM THAT SAYS WHAT TO DO, and what to do depends
// on where the shape came from: a model the table reads (a stale table), a
// claude_model override (the wrong row named), or an id the table does not
// know (an alias that needs one).
func TestABreakingDriftNamesTheRemedyForItsProfile(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, model, claudeModel string
		budget                   int
		record                   string
		profile, remedy          string
	}{
		{name: "the table's own row", model: "claude-opus-5-5",
			record:  record("claude-opus-5-5", 64000, true, false, claudemodel.Efforts...),
			profile: "claude-opus-5-5", remedy: "capability table is out of date for claude-opus-5-5"},
		{name: "an override", model: "gw-haiku", claudeModel: "claude-haiku-4-5", budget: 2048,
			record:  record("gw-haiku", 64000, false, false),
			profile: "claude-haiku-4-5 (claude_model)", remedy: "claude_model names claude-haiku-4-5"},
		{name: "an unknown id", model: "claude-opus-9",
			record:  record("claude-opus-9", 64000, false, true, claudemodel.Efforts...),
			profile: "none — not in the capability table", remedy: "name that model with claude_model"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, url := serveDoctor(t, servesRecord(tc.record), refuses(t))
			d := doctorProvider(t, url, tc.model, func(c *Config) {
				c.ClaudeModel = tc.claudeModel
				c.ThinkingBudget = tc.budget
			}).Diagnose(context.Background(), DiagnoseOptions{Key: "claude"})
			if !strings.HasPrefix(d.Profile, tc.profile) {
				t.Errorf("profile = %q, want %q", d.Profile, tc.profile)
			}
			if len(d.Problems) != 1 || !strings.Contains(d.Problems[0], tc.remedy) {
				t.Fatalf("problems = %v, want one naming %q", d.Problems, tc.remedy)
			}
			if len(d.Drift) != 1 || !strings.HasPrefix(d.Drift[0], "PROBLEM: ") {
				t.Errorf("drift = %v", d.Drift)
			}
		})
	}
}

// --- the report ----------------------------------------------------------

// THE REPORT CARRIES EVERY LINE, a note among them, and the problem list when
// there is one — the form the docs show.
func TestTheReportRendersEveryLine(t *testing.T) {
	t.Parallel()
	_, url := serveDoctor(t,
		servesRecord(record("claude-opus-5-5", 200000, true, false, claudemodel.Efforts...)),
		answersInProse("no"))
	d := doctorProvider(t, url, "claude-opus-5-5", nil).
		Diagnose(context.Background(), DiagnoseOptions{Key: "claude", Smoke: true})
	var out strings.Builder
	d.Render(&out)
	for _, want := range []string{
		"provider      : claude\n",
		"type          : anthropic\n",
		"model         : claude-opus-5-5\n",
		"profile       : claude-opus-5-5\n",
		"endpoint      : " + url + "\n",
		"keys          : 1 (",
		"request       : thinking adaptive (summarized), effort high, max_tokens 128000, never a temperature, " +
			"reasoning before a tool change shed\n",
		"models api    : served — Claude Test (claude-opus-5-5), max_tokens 200000, input 1000000\n",
		"drift         : note: the Models API allows max_tokens 200000",
		"smoke test    : failed — the model answered in prose",
		"problems:\n  - failed — the model answered in prose",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("report lacks %q:\n%s", want, out.String())
		}
	}
}

// AN ENTRY WITH NO KEY SAYS SO, rather than printing an empty bag: the round
// then fails with a 401 and the line beside it says why.
func TestAnEntryWithNoKeySaysSo(t *testing.T) {
	t.Parallel()
	_, url := serveDoctor(t, notFound, refuses(t))
	d := doctorProvider(t, url, "claude-opus-5-5", func(c *Config) { c.APIKeys = nil }).
		Diagnose(context.Background(), DiagnoseOptions{Key: "claude"})
	if !strings.HasPrefix(d.Keys, "none") {
		t.Errorf("keys = %q", d.Keys)
	}
}

// A BUDGET-ERA ENTRY'S REQUEST LINE SAYS WHETHER IT THINKS, because there it
// is the entry's choice rather than the model's.
func TestTheRequestLineFollowsTheProfile(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		model  string
		budget int
		want   string
	}{
		{"claude-haiku-4-5", 0, "no thinking (no reasoning_budget_tokens), no effort (the model takes none), max_tokens 64000"},
		{"claude-haiku-4-5", 2048, "thinking budget 2048, no effort (the model takes none), max_tokens 64000"},
		{"claude-opus-4-6", 0, "thinking adaptive (summarized), effort high, max_tokens 128000"},
		{"claude-mythos-5-1", 0, "thinking adaptive (summarized), effort high, max_tokens 128000, never a temperature"},
		{"claude-sonnet-5-5", 0, "thinking adaptive (summarized), effort high, max_tokens 128000, never a temperature, " +
			"reasoning before a tool change shed"},
	} {
		p := doctorProvider(t, "http://127.0.0.1:1", tc.model, func(c *Config) { c.ThinkingBudget = tc.budget })
		if got := p.shapeLine(); got != tc.want {
			t.Errorf("%s budget %d: request = %q, want %q", tc.model, tc.budget, got, tc.want)
		}
	}
}
