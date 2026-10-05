package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/anthropic/claudemodel"
)

// --- the vendor's check, emulated ---------------------------------------

// vendor is a Messages endpoint that runs the conversation check the way the
// vendor documents it, on every request to a model that runs it: each thinking
// block is bound, when it is written, to the request's system prompt, its SET
// of tools (each definition whole, compared without its cache breakpoint and
// in no order) and every message before the block, thinking excluded — and to
// the thinking blocks before it, of which a later request may drop a run from
// the FRONT and nothing else. A replayed block whose binding differs is a
// problem the real API answers with a 400 on an enforced account.
//
// It is STRICTER than the vendor in one respect, deliberately: it holds every
// block to its binding whichever model wrote it, where the vendor drops a
// block the serving model cannot read before checking it, and an older
// model's block carries no binding at all. A conversation that passes here
// passes on any routing of models.
type vendor struct {
	t   *testing.T
	url string

	// plan is the content blocks answering request n (from 1). A thinking
	// block is written with the signature `"SIG"` (a redacted one with the
	// data `"SIG"`), and the vendor mints a unique one in its place.
	plan func(n int) []string

	mu       sync.Mutex
	minted   map[string]written
	requests []checked
}

// written is what one thinking block was bound to when it was minted.
type written struct {
	prefix string
	chain  []string // every thinking block before it, oldest first
}

// checked is one request, as the vendor read it.
type checked struct {
	model  string
	header string   // the system prompt and the tool set, as compared
	kept   []string // the thinking blocks replayed, in order
	wrote  []string // the thinking blocks the answer minted, in order
	// problems are what the check refused; always empty on a model that
	// runs no check.
	problems []string
	raw      []byte
}

func newVendor(t *testing.T, plan func(n int) []string) *vendor {
	t.Helper()
	v := &vendor{t: t, plan: plan, minted: map[string]written{}}
	srv := httptest.NewServer(v)
	t.Cleanup(srv.Close)
	v.url = srv.URL
	return v
}

func (v *vendor) seen() []checked {
	v.mu.Lock()
	defer v.mu.Unlock()
	return slices.Clone(v.requests)
}

func (v *vendor) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	var body struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
		System []struct {
			Text string `json:"text"`
		} `json:"system"`
		Tools    []json.RawMessage `json:"tools"`
		Messages []struct {
			Role    string            `json:"role"`
			Content []json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	profile, _ := claudemodel.Lookup(body.Model)

	v.mu.Lock()
	defer v.mu.Unlock()
	n := len(v.requests) + 1
	req := checked{model: body.Model, raw: raw}

	var system []string
	for _, block := range body.System {
		system = append(system, block.Text)
	}
	defs := make([]string, 0, len(body.Tools))
	for _, tool := range body.Tools {
		defs = append(defs, v.canonical(tool))
	}
	slices.Sort(defs)
	req.header = strings.Join(system, "\n") + "\x00" + strings.Join(defs, "\x00")

	var conversation, chain []string
	prefix := func() string { return req.header + "\x01" + strings.Join(conversation, "\x01") }
	for _, m := range body.Messages {
		conversation = append(conversation, "role:"+m.Role)
		for _, block := range m.Content {
			kind, id, blank := v.head(block)
			switch {
			case kind == "thinking" || kind == "redacted_thinking":
				req.kept = append(req.kept, id)
				if profile.PrefixBinding {
					req.problems = append(req.problems, v.check(id, prefix(), chain)...)
				}
				chain = append(chain, id)
			case kind == "text" && blank:
				// Ignored by the check, and refused on input anyway.
			default:
				conversation = append(conversation, v.canonical(block))
			}
		}
	}

	// The answer: each thinking block minted against the conversation as
	// this request carried it.
	conversation = append(conversation, "role:assistant")
	var out []string
	stop := "end_turn"
	for j, block := range v.plan(n) {
		kind, _, blank := v.head(json.RawMessage(strings.ReplaceAll(block, `"SIG"`, `""`)))
		switch {
		case kind == "thinking" || kind == "redacted_thinking":
			id := fmt.Sprintf("sig-%d-%d", n, j)
			block = strings.Replace(block, `"SIG"`, strconv.Quote(id), 1)
			v.minted[id] = written{prefix: prefix(), chain: slices.Clone(chain)}
			req.wrote = append(req.wrote, id)
			chain = append(chain, id)
		case kind == "text" && blank:
		default:
			conversation = append(conversation, v.canonical(json.RawMessage(block)))
		}
		if kind == "tool_use" {
			stop = "tool_use"
		}
		out = append(out, block)
	}
	v.requests = append(v.requests, req)
	if body.Stream {
		writeStream(w, asStream([]byte(messageOf(out, stop)))...)
		return
	}
	writeJSON(w, http.StatusOK, messageOf(out, stop))
}

// check is what the vendor says about one replayed block.
func (v *vendor) check(id, prefix string, chain []string) []string {
	was, ok := v.minted[id]
	switch {
	case !ok:
		return []string{id + ": not a block this vendor wrote"}
	case was.prefix != prefix:
		return []string{id + ": the system prompt, the tools or a message before it changed"}
	case len(chain) > len(was.chain) || !slices.Equal(chain, was.chain[len(was.chain)-len(chain):]):
		// Only a run dropped from the front leaves what precedes the
		// block a suffix of what preceded it when it was written.
		return []string{fmt.Sprintf("%s: written after %v, replayed after %v", id, was.chain, chain)}
	}
	return nil
}

// head is a block's type, its identity when it is thinking (a signature, or a
// redacted block's data), and whether it is text of nothing but whitespace.
func (v *vendor) head(block json.RawMessage) (kind, id string, blank bool) {
	var h struct {
		Type      string `json:"type"`
		Signature string `json:"signature"`
		Data      string `json:"data"`
		Text      string `json:"text"`
	}
	if err := json.Unmarshal(block, &h); err != nil {
		v.t.Errorf("the vendor was sent a block that is not JSON: %s", block)
	}
	id = h.Signature
	if h.Type == "redacted_thinking" {
		id = h.Data
	}
	return h.Type, id, strings.TrimSpace(h.Text) == ""
}

// canonical is a block or a definition as the check compares it: keys in one
// order, numbers as written, no cache breakpoint.
func (v *vendor) canonical(raw json.RawMessage) string {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var object map[string]any
	if err := dec.Decode(&object); err != nil {
		v.t.Errorf("the vendor was sent something that is not a JSON object: %s", raw)
	}
	delete(object, "cache_control")
	out, _ := json.Marshal(object)
	return string(out)
}

// passed fails unless every request passed the check.
func passed(t *testing.T, requests []checked) {
	t.Helper()
	for i, req := range requests {
		for _, problem := range req.problems {
			t.Errorf("request %d (%s) would be refused: %s", i+1, req.model, problem)
		}
	}
}

// THE EMULATED CHECK REFUSES WHAT THE VENDOR REFUSES, and accepts what it
// accepts — or every test below that it passes would certify nothing. Held
// to the vendor's own list: an unchanged history and a run dropped from the
// front or the end pass; a tool added, a message edited, a block removed from
// the middle and a removed block put back are each refused.
func TestTheEmulatedCheckRefusesWhatTheVendorRefuses(t *testing.T) {
	t.Parallel()
	v := newVendor(t, func(n int) []string {
		return []string{think("t"), call(fmt.Sprintf("tu-%d", n), "lookup")}
	})
	a := `{"name":"lookup","input_schema":{"type":"object"}}`
	b := `{"name":"fetch","input_schema":{"type":"object"}}`
	bCached := `{"input_schema":{"type":"object"},"name":"fetch","cache_control":{"type":"ephemeral"}}`
	c := `{"name":"note","input_schema":{"type":"object"}}`
	turn := func(n int, thinking bool) string {
		blocks := []string{call(fmt.Sprintf("tu-%d", n), "lookup")}
		if thinking {
			sig := strconv.Quote(fmt.Sprintf("sig-%d-0", n))
			blocks = append([]string{strings.Replace(think("t"), `"SIG"`, sig, 1)}, blocks...)
		}
		return `{"role":"assistant","content":[` + strings.Join(blocks, ",") + `]},` +
			`{"role":"user","content":[{"type":"tool_result","tool_use_id":"tu-` + strconv.Itoa(n) +
			`","content":"ok"}]}`
	}
	// send posts one request and answers its number and what the check
	// said about it.
	send := func(tools []string, opening string, turns ...string) (int, []string) {
		t.Helper()
		messages := append([]string{`{"role":"user","content":[{"type":"text","text":` +
			strconv.Quote(opening) + `}]}`}, turns...)
		body := `{"model":"claude-sonnet-5-5","max_tokens":1,"tools":[` + strings.Join(tools, ",") +
			`],"messages":[` + strings.Join(messages, ",") + `]}`
		resp, err := http.Post(v.url, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		_ = resp.Body.Close()
		seen := v.seen()
		return len(seen), seen[len(seen)-1].problems
	}
	// Three rounds over one tool set: sig-1-0, sig-2-0 and sig-3-0, each
	// written after the one before it.
	tools := []string{a, b}
	for _, history := range [][]string{nil, {turn(1, true)}, {turn(1, true), turn(2, true)}} {
		if _, problems := send(tools, "go", history...); len(problems) > 0 {
			t.Fatalf("an unchanged history was refused: %v", problems)
		}
	}
	all := []string{turn(1, true), turn(2, true), turn(3, true)}
	for _, tc := range []struct {
		name    string
		tools   []string
		opening string
		turns   []string
		refused bool
	}{
		{"unchanged", tools, "go", all, false},
		{"the same tools in another order", []string{bCached, a}, "go", all, false},
		{"a run off the front", tools, "go", []string{turn(1, false), turn(2, false), turn(3, true)}, false},
		{"a run off the end", tools, "go", []string{turn(1, true), turn(2, false), turn(3, false)}, false},
		{"a tool added", []string{a, b, c}, "go", all, true},
		{"a tool withdrawn", []string{a}, "go", all, true},
		{"a message edited", tools, "go!", all, true},
		{"a block from the middle", tools, "go", []string{turn(1, true), turn(2, false), turn(3, true)}, true},
	} {
		if _, problems := send(tc.tools, tc.opening, tc.turns...); (len(problems) > 0) != tc.refused {
			t.Errorf("%s: problems %v, want refused = %v", tc.name, problems, tc.refused)
		}
	}
	// A block put back: the next block is written while sig-1-0 is gone,
	// so a history that brings sig-1-0 back in front of it invalidates it.
	n, problems := send(tools, "go", turn(1, false), turn(2, true), turn(3, true))
	if len(problems) > 0 {
		t.Fatalf("a history with a run off the front was refused: %v", problems)
	}
	if _, problems := send(tools, "go", append(slices.Clone(all), turn(n, true))...); len(problems) == 0 {
		t.Errorf("sig-1-0 put back was accepted, though sig-%d-0 was written while it was gone", n)
	}
	if _, problems := send(tools, "go", turn(1, false), turn(2, true), turn(3, true), turn(n, true)); len(problems) > 0 {
		t.Errorf("the same history without sig-1-0 was refused: %v", problems)
	}
}

// --- a tool loop's conversation ------------------------------------------

// loop grows a conversation the way internal/agent/toolloop does: each round's
// answer is appended as [llm.Completion.Message], every call it made is
// answered, and a round that made none is followed by a user turn.
type loop struct{ msgs []llm.Message }

func opening() *loop {
	return &loop{msgs: []llm.Message{
		{Role: llm.RoleSystem, Content: "You are a seat."},
		{Role: llm.RoleUser, Content: "Do the work."},
	}}
}

func (l *loop) round(t *testing.T, p *Provider, tools []llm.ToolDef) *llm.Completion {
	t.Helper()
	out, err := p.Complete(context.Background(), llm.Request{Messages: l.msgs, Tools: tools})
	if err != nil {
		t.Fatalf("round %d: %v", l.rounds()+1, err)
	}
	l.msgs = append(l.msgs, out.Message())
	for _, call := range out.ToolCalls {
		l.msgs = append(l.msgs, llm.Message{Role: llm.RoleTool, ToolCallID: call.ID, Name: call.Name, Content: "ok"})
	}
	if len(out.ToolCalls) == 0 {
		l.msgs = append(l.msgs, llm.Message{Role: llm.RoleUser, Content: "Go on."})
	}
	return out
}

func (l *loop) rounds() int {
	n := 0
	for _, m := range l.msgs {
		if m.Role == llm.RoleAssistant {
			n++
		}
	}
	return n
}

// park sends the conversation through the JSON a suspended run is stored as
// (internal/agent/execstate) and back, as a resume reads it.
func (l *loop) park(t *testing.T) {
	t.Helper()
	blob, err := json.Marshal(l.msgs)
	if err != nil {
		t.Fatalf("park: %v", err)
	}
	var back []llm.Message
	if err := json.Unmarshal(blob, &back); err != nil {
		t.Fatalf("resume: %v", err)
	}
	l.msgs = back
}

// thinkingIn is every thinking block the conversation's turns carry, by
// signature, in order — the sequence a request keeps a run of.
func (l *loop) thinkingIn(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, m := range l.msgs {
		for _, block := range m.Raw {
			var h struct{ Type, Signature, Data string }
			if err := json.Unmarshal(block, &h); err != nil {
				t.Fatalf("raw block %s: %v", block, err)
			}
			switch h.Type {
			case "thinking":
				out = append(out, h.Signature)
			case "redacted_thinking":
				out = append(out, h.Data)
			}
		}
	}
	return out
}

func tool(name, description string) llm.ToolDef {
	return llm.ToolDef{Name: name, Description: description, Parameters: map[string]any{
		"type":       "object",
		"properties": map[string]any{"q": map[string]any{"type": "string"}},
		"required":   []any{"q"},
	}}
}

func think(text string) string {
	return `{"type":"thinking","thinking":` + strconv.Quote(text) + `,"signature":"SIG"}`
}

func say(text string) string { return `{"type":"text","text":` + strconv.Quote(text) + `}` }

func call(id, name string) string {
	return `{"type":"tool_use","id":` + strconv.Quote(id) + `,"name":` + strconv.Quote(name) + `,"input":{"q":"x"}}`
}

// boundTo is the binding of a request carrying this system prompt and these
// tools.
func boundTo(t *testing.T, system string, tools []llm.ToolDef) string {
	t.Helper()
	b, err := binding(system, formatTools(tools))
	if err != nil {
		t.Fatalf("binding: %v", err)
	}
	return b
}

// --- the binding ----------------------------------------------------------

// THE BINDING IS WHAT THE VENDOR COMPARES, AND NOTHING ELSE. Every edit it
// names — a tool added or withdrawn, a description or a schema reworded, the
// system prompt — moves it; the order of the tools, and so which of them
// carries the cache breakpoint, does not.
func TestTheBindingIsTheSystemPromptAndTheSetOfTools(t *testing.T) {
	t.Parallel()
	base := []llm.ToolDef{tool("lookup", "Look a thing up."), tool("fetch", "Fetch a page."), tool("note", "Note it.")}
	want := boundTo(t, "frame", base)

	same := map[string]struct {
		system string
		tools  []llm.ToolDef
	}{
		"another order": {"frame", []llm.ToolDef{base[2], base[0], base[1]}},
		"a fresh copy":  {"frame", []llm.ToolDef{tool("lookup", "Look a thing up."), tool("fetch", "Fetch a page."), tool("note", "Note it.")}},
	}
	for name, tc := range same {
		if got := boundTo(t, tc.system, tc.tools); got != want {
			t.Errorf("%s: the binding moved, so reasoning that is still valid would be shed", name)
		}
	}

	reschema := tool("fetch", "Fetch a page.")
	reschema.Parameters = map[string]any{"type": "object", "properties": map[string]any{"q": map[string]any{"type": "integer"}}}
	differs := map[string]struct {
		system string
		tools  []llm.ToolDef
	}{
		"a tool added":           {"frame", append(slices.Clone(base), tool("post", "Post it."))},
		"a tool withdrawn":       {"frame", base[:2]},
		"a description reworded": {"frame", []llm.ToolDef{base[0], tool("fetch", "Fetch one page."), base[2]}},
		"a schema changed":       {"frame", []llm.ToolDef{base[0], reschema, base[2]}},
		"a tool renamed":         {"frame", []llm.ToolDef{base[0], tool("get", "Fetch a page."), base[2]}},
		"the system prompt":      {"frame.", base},
		"no tools at all":        {"frame", nil},
	}
	for name, tc := range differs {
		if got := boundTo(t, tc.system, tc.tools); got == want {
			t.Errorf("%s: the binding did not move, so reasoning the vendor refuses would be replayed", name)
		}
	}
	// THE PARTS ARE LENGTH-PREFIXED, so a system prompt cannot pass for the
	// start of a definition: hashed bare, a request with system S and one
	// tool D is the same bytes as one whose system prompt is S followed by
	// D's canonical form and which offers no tools at all — and the
	// reasoning one wrote would be replayed into the other.
	lookup := base[0]
	raw, err := json.Marshal(formatTools([]llm.ToolDef{lookup})[0])
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	def, err := canonical(raw)
	if err != nil {
		t.Fatalf("canonical: %v", err)
	}
	for _, system := range []string{"", "frame"} {
		if boundTo(t, system, []llm.ToolDef{lookup}) == boundTo(t, system+string(def), nil) {
			t.Errorf("system %q with a tool binds like a system prompt that spells the tool out", system)
		}
	}
}

// EVERY TURN RECORDS THE BINDING OF THE REQUEST THAT WROTE IT, and the
// conversation carries it — through Completion.Message and through the JSON a
// parked run is stored as.
func TestATurnRecordsWhatItsRequestWasBoundTo(t *testing.T) {
	t.Parallel()
	v := newVendor(t, func(int) []string { return []string{think("t"), call("tu-1", "lookup")} })
	p := newProvider(t, v.url, func(c *Config) { c.Model = "claude-sonnet-5-5" })
	tools := []llm.ToolDef{tool("lookup", "Look a thing up.")}
	l := opening()
	out := l.round(t, p, tools)
	want := boundTo(t, "You are a seat.", tools)
	if out.Binding != want {
		t.Fatalf("completion binding = %q, want the request's %q", out.Binding, want)
	}
	l.park(t)
	if got := l.msgs[2].Binding; got != want {
		t.Fatalf("parked turn binding = %q, want %q", got, want)
	}
}

// --- what a request keeps -------------------------------------------------

// AN ACTIVATION SHEDS THE REASONING IT INVALIDATED, AND ONLY THAT. Round 2
// activates a tool, so round 3's request carries one definition rounds 1 and 2
// were not written under: both are replayed without their thinking — their
// text and calls exactly as written — and round 3's own reasoning, written
// under the new set, is kept by round 4. Before the shed, replaying them was a
// 400 on Sonnet 5.5 for every enforced account, on the ordinary discover →
// activate → call flow.
func TestAnActivationShedsTheReasoningItInvalidated(t *testing.T) {
	t.Parallel()
	v := newVendor(t, func(n int) []string {
		switch n {
		case 1:
			return []string{think("Look first."), call("tu-1", "lookup")}
		case 2:
			return []string{think("I need fetch."), say("Activating fetch."), call("tu-2", "activate_tool")}
		case 3:
			return []string{think("Now fetch."), call("tu-3", "fetch")}
		default:
			return []string{think("Done."), say("Done.")}
		}
	})
	p := newProvider(t, v.url, func(c *Config) { c.Model = "claude-sonnet-5-5" })
	before := []llm.ToolDef{tool("lookup", "Look a thing up."), tool("activate_tool", "Activate a tool.")}
	after := append(slices.Clone(before), tool("fetch", "Fetch a page."))

	l := opening()
	l.round(t, p, before)
	l.round(t, p, before)
	l.round(t, p, after)
	l.round(t, p, after)

	seen := v.seen()
	passed(t, seen)
	for i, want := range [][]string{
		nil,         // round 1: nothing to replay
		{"sig-1-0"}, // round 2: round 1's, written under the same tools
		nil,         // round 3: rounds 1 and 2 were written under the old set
		{"sig-3-0"}, // round 4: round 3's, written under the new one
	} {
		if got := seen[i].kept; !slices.Equal(got, want) {
			t.Errorf("round %d replayed thinking %v, want %v", i+1, got, want)
		}
	}
	// What was shed is the thinking and nothing else.
	replayedAsWritten(t, seen[3].raw, 1, []string{call("tu-1", "lookup")})
	replayedAsWritten(t, seen[3].raw, 3, []string{say("Activating fetch."), call("tu-2", "activate_tool")})
}

// A RESUME THAT RENDERS A DEFINITION DIFFERENTLY SHEDS THE REASONING BEFORE
// IT. A parked run's tools are rendered again from the registry it resumes
// against, and an MCP server that reworded one while the run was parked has
// changed the set every earlier block was written under.
func TestAResumeThatRewordsADefinitionShedsTheReasoningBeforeIt(t *testing.T) {
	t.Parallel()
	v := newVendor(t, func(n int) []string {
		return []string{think(fmt.Sprintf("Round %d.", n)), call(fmt.Sprintf("tu-%d", n), "lookup")}
	})
	p := newProvider(t, v.url, func(c *Config) { c.Model = "claude-opus-5-5" })
	parked := []llm.ToolDef{tool("lookup", "Look a thing up."), tool("run_sandbox", "Run code.")}
	resumed := []llm.ToolDef{tool("lookup", "Look a thing up, by id or by name."), tool("run_sandbox", "Run code.")}

	l := opening()
	l.round(t, p, parked)
	l.round(t, p, parked)
	l.park(t)
	l.round(t, p, resumed)
	l.round(t, p, resumed)

	seen := v.seen()
	passed(t, seen)
	if got := seen[2].kept; len(got) != 0 {
		t.Errorf("the first resumed round replayed %v, written under the parked definitions", got)
	}
	if got := seen[3].kept; !slices.Equal(got, []string{"sig-3-0"}) {
		t.Errorf("the second resumed round replayed %v, want only the resumed round's own", got)
	}
}

// THE SAME DEFINITIONS IN ANOTHER ORDER SHED NOTHING. The vendor compares the
// tools as a set, and a resume that activates in another order — or renders
// the active list differently — has changed nothing it checks. The cache
// breakpoint moving to whichever tool is now last is not a change either.
func TestTheSameDefinitionsInAnotherOrderKeepTheReasoning(t *testing.T) {
	t.Parallel()
	v := newVendor(t, func(n int) []string {
		return []string{think(fmt.Sprintf("Round %d.", n)), call(fmt.Sprintf("tu-%d", n), "lookup")}
	})
	p := newProvider(t, v.url, func(c *Config) { c.Model = "claude-fable-5-1" })
	a, b, c := tool("lookup", "Look a thing up."), tool("fetch", "Fetch a page."), tool("note", "Note it.")

	l := opening()
	l.round(t, p, []llm.ToolDef{a, b, c})
	l.round(t, p, []llm.ToolDef{a, b, c})
	l.park(t)
	l.round(t, p, []llm.ToolDef{c, a, b})

	seen := v.seen()
	passed(t, seen)
	if got := seen[2].kept; !slices.Equal(got, []string{"sig-1-0", "sig-2-0"}) {
		t.Errorf("replayed %v, want every block: nothing the vendor compares changed", got)
	}
}

// A TURN PARKED BEFORE BINDINGS WERE RECORDED HAS ITS THINKING SHED. Nothing
// can show its blocks were written under this request's tools, and a resumed
// run's own rounds all come after it, so shedding it drops a run from the
// front. Everything else in it goes back exactly as written.
func TestATurnParkedWithoutABindingHasItsThinkingShed(t *testing.T) {
	t.Parallel()
	v := newVendor(t, func(n int) []string {
		return []string{think(fmt.Sprintf("Round %d.", n)), call(fmt.Sprintf("tu-%d", n), "lookup")}
	})
	p := newProvider(t, v.url, func(c *Config) { c.Model = "claude-sonnet-5-5" })
	tools := []llm.ToolDef{tool("lookup", "Look a thing up.")}
	raw := make([]json.RawMessage, len(interleaved))
	for i, b := range interleaved {
		raw[i] = json.RawMessage(b)
	}
	l := opening()
	l.msgs = append(l.msgs,
		llm.Message{
			Role: llm.RoleAssistant, Content: "  First, <this>.  ",
			ToolCalls: []llm.ToolCall{{ID: "toolu_1", Name: "lookup"}},
			Origin:    llm.Origin{Provider: "anthropic", Model: "claude-sonnet-5-5"},
			Raw:       raw,
		},
		llm.Message{Role: llm.RoleTool, ToolCallID: "toolu_1", Content: "found"},
	)
	l.round(t, p, tools)
	l.round(t, p, tools)

	seen := v.seen()
	passed(t, seen)
	replayedAsWritten(t, seen[0].raw, 1, []string{interleaved[1], interleaved[3]})
	if got := seen[1].kept; !slices.Equal(got, []string{"sig-1-0"}) {
		t.Errorf("replayed %v, want the old turn's thinking shed and the new round's kept", got)
	}
}

// A TURN THAT CARRIES NO THINKING NEVER SETS THE CUT, whatever it was
// written under: it has nothing the vendor could refuse. Here a round under an
// activated tool thinks nothing, and a resume withdraws the tool again — so
// the first round's reasoning, written under exactly the set that is back, is
// replayed rather than shed behind a turn with nothing to shed.
func TestATurnWithNoThinkingNeverSetsTheCut(t *testing.T) {
	t.Parallel()
	v := newVendor(t, func(n int) []string {
		if n == 2 {
			return []string{say("Fetching."), call("tu-2", "fetch")}
		}
		return []string{think(fmt.Sprintf("Round %d.", n)), call(fmt.Sprintf("tu-%d", n), "lookup")}
	})
	p := newProvider(t, v.url, func(c *Config) { c.Model = "claude-opus-5-5" })
	base := []llm.ToolDef{tool("lookup", "Look a thing up.")}
	widened := append(slices.Clone(base), tool("fetch", "Fetch a page."))

	l := opening()
	l.round(t, p, base)
	l.round(t, p, widened)
	l.park(t)
	l.round(t, p, base)

	seen := v.seen()
	passed(t, seen)
	if got := seen[2].kept; !slices.Equal(got, []string{"sig-1-0"}) {
		t.Errorf("replayed %v, want round 1's reasoning: nothing after it carries thinking", got)
	}
}

// A MODEL THAT RUNS NO CHECK KEEPS EVERY BLOCK, because every block is still
// valid to it and shedding one would only lose what it reasoned. But a turn it
// writes while replaying reasoning a check would have shed was written in a
// conversation no checked request will send again, so it records no binding —
// and when the conversation moves to a model that checks (a chain's primary
// coming back), that turn is shed with everything before it rather than kept
// on the strength of a tool set that matched.
func TestAModelWithoutTheCheckKeepsEveryBlock(t *testing.T) {
	t.Parallel()
	v := newVendor(t, func(n int) []string {
		return []string{think(fmt.Sprintf("Round %d.", n)), say(" \n"), call(fmt.Sprintf("tu-%d", n), "lookup")}
	})
	unchecked := newProvider(t, v.url, func(c *Config) { c.Model = "claude-mythos-5-1" })
	checked := newProvider(t, v.url, func(c *Config) { c.Model = "claude-fable-5-1" })
	before := []llm.ToolDef{tool("lookup", "Look a thing up.")}
	after := append(slices.Clone(before), tool("fetch", "Fetch a page."))

	l := opening()
	l.round(t, unchecked, before)
	quiet := l.round(t, unchecked, before)
	l.round(t, checked, before)
	loud := l.round(t, unchecked, after)
	l.round(t, checked, after)

	seen := v.seen()
	passed(t, seen)
	// Nothing changed: the move to a model that checks keeps it all.
	if quiet.Binding == "" {
		t.Error("a turn written with nothing to shed records no binding, so a checking model sheds it for nothing")
	}
	if got := seen[2].kept; !slices.Equal(got, []string{"sig-1-0", "sig-2-0"}) {
		t.Errorf("the checking model was replayed %v, want both unchecked rounds' blocks", got)
	}
	// The tools changed: the unchecked model is still handed everything.
	if got := seen[3].kept; !slices.Equal(got, []string{"sig-1-0", "sig-2-0", "sig-3-0"}) {
		t.Errorf("the unchecked model was replayed %v, want every block", got)
	}
	if loud.Binding != "" {
		t.Error("a turn written while replaying invalidated reasoning records a binding a checking model would trust")
	}
	if got := seen[4].kept; len(got) != 0 {
		t.Errorf("the checking model was replayed %v, want everything up to the unchecked round shed", got)
	}
}

// --- the property ----------------------------------------------------------

// THE KEPT REASONING IS ALWAYS A RUN FROM THE END, AND THE CHECK ALWAYS
// PASSES — over generated conversations: tools activated, a resume that
// rewords, withdraws or reorders definitions, a set that comes back to an
// earlier one, rounds that think or do not, turns that are nothing but
// thinking or carry redacted thinking, and a chain moving between a model that
// checks and one that does not. Every request is held to four things:
//
//   - the vendor's check passes on every request to a model that runs it;
//   - what is replayed is the END of the conversation's thinking sequence,
//     never a sequence with a gap;
//   - a model without the check is replayed every block;
//   - nothing is shed without cause: a checked request whose system prompt
//     and tools are the previous checked request's keeps everything that one
//     kept, plus what it wrote.
func TestTheKeptReasoningIsAlwaysARunFromTheEnd(t *testing.T) {
	t.Parallel()
	for trial := range 150 {
		rng := rand.New(rand.NewPCG(uint64(trial), 0x5eed))
		t.Run(strconv.Itoa(trial), func(t *testing.T) {
			t.Parallel()
			run := newScript(rng)
			v := newVendor(t, run.answer)
			checks := newProvider(t, v.url, func(c *Config) { c.Model = checksModel })
			free := newProvider(t, v.url, func(c *Config) { c.Model = "claude-mythos-5-1" })
			mixed := trial%3 == 0

			l := opening()
			p := checks
			tools := run.tools()
			for range 3 + rng.IntN(10) {
				before := l.thinkingIn(t)
				l.round(t, p, tools)
				req := v.seen()[len(v.seen())-1]
				if !isSuffix(req.kept, before) {
					t.Fatalf("round %d replayed %v, not a run from the end of %v", l.rounds(), req.kept, before)
				}
				if p == free && !slices.Equal(req.kept, before) {
					t.Fatalf("round %d on a model with no check replayed %v of %v", l.rounds(), req.kept, before)
				}
				// Between rounds: what a phase does to its tools.
				switch roll := rng.IntN(10); {
				case roll < 2:
					tools = run.activate(tools)
				case roll < 3:
					l.park(t)
					tools = run.rerender(tools)
				case roll < 4:
					tools = run.revert()
				case roll < 5 && mixed:
					if p == checks {
						p = free
					} else {
						p = checks
					}
				}
			}

			seen := v.seen()
			passed(t, seen)
			// Nothing shed without cause.
			for i := 1; i < len(seen); i++ {
				prev, req := seen[i-1], seen[i]
				if prev.model != checksModel || req.model != checksModel || prev.header != req.header {
					continue
				}
				if want := append(slices.Clone(prev.kept), prev.wrote...); !slices.Equal(req.kept, want) {
					t.Errorf("request %d replayed %v with nothing changed since request %d, want %v",
						i+1, req.kept, i, want)
				}
			}
		})
	}
}

// checksModel is the model the property's checked rounds run on.
const checksModel = "claude-sonnet-5-5"

func isSuffix(kept, all []string) bool {
	return len(kept) <= len(all) && slices.Equal(kept, all[len(all)-len(kept):])
}

// script is one generated conversation's answers and tool sets.
type script struct {
	rng     *rand.Rand
	history [][]llm.ToolDef
	next    int
	calls   int
}

func newScript(rng *rand.Rand) *script { return &script{rng: rng} }

func (s *script) tools() []llm.ToolDef {
	set := []llm.ToolDef{tool("lookup", "Look a thing up."), tool("activate_tool", "Activate a tool.")}
	s.history = append(s.history, set)
	return set
}

func (s *script) remember(set []llm.ToolDef) []llm.ToolDef {
	s.history = append(s.history, set)
	return set
}

// activate adds a definition, as activate_tool does.
func (s *script) activate(set []llm.ToolDef) []llm.ToolDef {
	s.next++
	return s.remember(append(slices.Clone(set), tool(fmt.Sprintf("mcp_%d", s.next), "An MCP tool.")))
}

// rerender is a resume against a registry that may have moved: one
// definition reworded, one withdrawn, or the same set in another order.
func (s *script) rerender(set []llm.ToolDef) []llm.ToolDef {
	out := slices.Clone(set)
	switch s.rng.IntN(3) {
	case 0:
		i := s.rng.IntN(len(out))
		out[i] = tool(out[i].Name, out[i].Description+" Now reworded.")
	case 1:
		if len(out) > 1 {
			i := s.rng.IntN(len(out))
			out = slices.Delete(out, i, i+1)
		}
	default:
		s.rng.Shuffle(len(out), func(i, j int) { out[i], out[j] = out[j], out[i] })
	}
	return s.remember(out)
}

// revert is a set this conversation already used, back again.
func (s *script) revert() []llm.ToolDef {
	return s.remember(slices.Clone(s.history[s.rng.IntN(len(s.history))]))
}

// answer is a generated round: up to two thinking blocks (one maybe
// redacted), text that may be only whitespace, and usually a call — or
// nothing but thinking.
func (s *script) answer(n int) []string {
	var out []string
	for j := range s.rng.IntN(3) {
		if s.rng.IntN(5) == 0 {
			out = append(out, `{"type":"redacted_thinking","data":"SIG"}`)
			continue
		}
		out = append(out, think(fmt.Sprintf("Round %d, thought %d.", n, j)))
		if s.rng.IntN(3) == 0 {
			out = append(out, say(fmt.Sprintf("Between thoughts %d.", j)))
		}
	}
	switch s.rng.IntN(6) {
	case 0:
		out = append(out, say("\n"))
	case 1:
		out = append(out, say("Said."))
	}
	if s.rng.IntN(5) > 0 || len(out) == 0 {
		s.calls++
		out = append(out, call(fmt.Sprintf("tu-%d", s.calls), "lookup"))
	}
	return out
}
