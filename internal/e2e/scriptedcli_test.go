package e2e

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/learning"
	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent"
)

// THE SCRIPTED MODEL'S CLI: this test binary, re-executed.
//
// Every model call the engine makes goes through a coding CLI
// (internal/providers/llm/cliagent), so the golden company's model entry is a
// real one whose CLI is this binary ([scriptedModel.entry]). Re-executed with
// [scriptedCLIEnv] set, the binary does what a CLI does: it reads the
// transcript cliagent rendered on stdin and the system prompt from the file
// the entry's system channel names, and prints an answer. The ANSWER is the
// [scriptedModel]'s, fetched over loopback, because the script — which phase
// this is, which tool the executor calls next — is state in the test process,
// and a child launched per call keeps nothing between calls.
//
// What the two halves agree on is everything in this file: the entry that
// launches the child, the one request it posts, how the model reads the
// prompt the CLI backend rendered, and the stdout the backend reads back.

// scriptedCLIEnv makes a re-execution of this binary the scripted CLI rather
// than the suite, and its value is the address of the model that answers.
//
// ONE VARIABLE FOR BOTH, so there is no way to launch a child that is the CLI
// and does not know where its model is. It reaches the child through the
// entry's own `env`, which is the only route there is: the backend hands its
// child an allowlisted environment, never the engine's.
const scriptedCLIEnv = "CREWLET_E2E_SCRIPTED_CLI"

// scriptedSystemFlag and scriptedAppendFlag are the flags the entry passes a
// system prompt's file on — Claude Code's two channels, the first replacing
// the CLI's own prompt and the second kept after it — so the system prompt
// reaches the model on its own channel exactly as it does for the default
// agent, by the route the default agent's would take: a seat's rounds on the
// append channel, a side pass on the replace one ([cliprofile.Profile.RouteFor]).
const (
	scriptedSystemFlag = "--system-prompt-file"
	scriptedAppendFlag = "--append-system-prompt-file"
)

// The channels a [cliCall] names its system prompt arrived on.
const (
	channelReplace = "replace"
	channelAppend  = "append"
)

// scriptedCLIBinary is this test binary's own path, which the entry names as
// its CLI. Set once by [TestMain], before any case runs.
//
// [os.Executable] rather than os.Args[0]: the backend runs its child in the
// call's own working directory, so a binary launched by a relative path —
// `./e2e.test` — would name nothing there.
var scriptedCLIBinary string

// scriptedEntry is the providers.llm entry, under key, whose CLI is this binary
// answering from the model at endpoint and keeping its homes under stateDir —
// indented to sit under `providers: llm:`.
//
// `custom` RATHER THAN A VENDOR'S PROFILE WITH ITS BINARY SWAPPED, because
// `custom` ships nothing: every field the scripted CLI is driven by is written
// here, and nothing a vendor's profile carries — its sign-in, its spent-plan
// sentinels, its configuration directory — stands between the engine and a
// CLI that has none of them.
//
// THE FIELDS ARE CLAUDE CODE'S SHAPES where it has one: the model and the
// system prompt's file on flags, the transcript on stdin, and one JSON
// document out, its answer under `result` and its usage beside it. So the
// backend reads this CLI on the same paths the default agent's are read on,
// and the usage it charges is the figure each reply names ([replyUsage]),
// never an estimate a budget case could not reason about.
//
// complete_args NAMES NO TEST, as a FORK-BOMB GUARD rather than a selector:
// [TestMain] dispatches on [scriptedCLIEnv] alone, so a child launched without
// it falls through to the suite, and a filter matching nothing ends that child
// at once instead of running every case of the package again, once per call.
//
// GORACE is [procgrouptest.StandInRaceOptions]: a race-built child that exits 0
// otherwise sleeps a second on its way out, which every model call of every
// case would pay.
//
// max_concurrent and timeout_seconds are NOT set: a node here runs its model
// calls under the caps a deployment gets by default, and no case is about
// either.
//
// Every value is written with %q: a Go-quoted string is a YAML double-quoted
// scalar, every escape strconv.Quote writes being one YAML reads.
func scriptedEntry(key, endpoint, stateDir string) string {
	return fmt.Sprintf(`    %s:
      agent: custom
      model: claude-golden
      state_dir: %q
      env:
        %s: %q
        %s: %q
      overrides:
        binary: %q
        complete_args: ["-test.run=^$"]
        model_args: ["--model", "{model}"]
        system_prompt: {args: [%q, "{file}"]}
        append_system_prompt: {args: [%q, "{file}"]}
        prompt_mode: stdin
        output: json
        text_paths: [["result"]]
        error_paths: [["is_error"]]
        usage:
          input: [["usage", "input_tokens"]]
          output: [["usage", "output_tokens"]]
          cache_read: [["usage", "cache_read_input_tokens"]]
          cache_write: [["usage", "cache_creation_input_tokens"]]
`, key, stateDir,
		scriptedCLIEnv, endpoint,
		procgrouptest.GORACE, procgrouptest.StandInRaceOptions(),
		scriptedCLIBinary, scriptedSystemFlag, scriptedAppendFlag)
}

// cliCall is what the scripted CLI posts for one invocation: the system prompt
// it was handed on its own channel, which channel that was, and the transcript
// it read on stdin.
type cliCall struct {
	System string `json:"system"`
	// Channel is [channelReplace] or [channelAppend], or empty where the
	// call carried no system prompt.
	Channel string `json:"channel,omitempty"`
	Prompt  string `json:"prompt"`
}

// runScriptedCLI is the scripted CLI: it posts the call to the model at
// endpoint, prints the model's answer as its own stdout, and returns the exit
// status.
//
// A FAILURE IS THE CLI'S OWN, on stderr with a non-zero exit, which the
// backend reports as the CLI failing — the reason reaches the engine's log
// rather than a wait in some case timing out with nothing said.
func runScriptedCLI(endpoint string) int {
	fail := func(format string, args ...any) int {
		fmt.Fprintf(os.Stderr, "scripted CLI: "+format+"\n", args...)
		return 1
	}
	prompt, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fail("reading the prompt: %v", err)
	}
	// ABSENT IS NO SYSTEM PROMPT, not a fault: the backend passes no flag
	// for a request that carries none, since a CLI handed an empty one
	// would replace its default with nothing. BOTH IS: a call takes one
	// route.
	var system []byte
	channel := ""
	for flag, name := range map[string]string{scriptedSystemFlag: channelReplace, scriptedAppendFlag: channelAppend} {
		i := slices.Index(os.Args, flag)
		if i < 0 {
			continue
		}
		if channel != "" {
			return fail("handed a system prompt on both channels")
		}
		if i+1 >= len(os.Args) {
			return fail("%s names no file", flag)
		}
		if system, err = os.ReadFile(os.Args[i+1]); err != nil {
			return fail("reading the system prompt: %v", err)
		}
		channel = name
	}
	body, err := json.Marshal(cliCall{System: string(system), Channel: channel, Prompt: string(prompt)})
	if err != nil {
		return fail("encoding the call: %v", err)
	}
	// NO DEADLINE OF ITS OWN: the request is bounded by the call's, since
	// the backend kills this whole process at the entry's wall-clock cap.
	resp, err := httpx.Client(0).Post(endpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return fail("asking the model: %v", err)
	}
	defer resp.Body.Close()
	answer, err := io.ReadAll(resp.Body)
	if err != nil {
		return fail("reading the model's answer: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		return fail("the model answered %d: %s", resp.StatusCode, bytes.TrimSpace(answer))
	}
	if _, err := os.Stdout.Write(answer); err != nil {
		return fail("printing the answer: %v", err)
	}
	return 0
}

// readCall reads the one request the scripted CLI posts for a call, and the
// tools its prompt offers — answering the CLI with why, and reporting false,
// when either cannot be read. The CLI prints that on stderr and exits non-zero,
// so the reason reaches the engine's log as the call's failure.
func readCall(w http.ResponseWriter, r *http.Request) (cliCall, map[string]bool, bool) {
	var call cliCall
	if err := json.NewDecoder(r.Body).Decode(&call); err != nil {
		http.Error(w, "the scripted CLI posted no call: "+err.Error(), http.StatusBadRequest)
		return cliCall{}, nil, false
	}
	offered, err := offeredTools(call.Prompt)
	if err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return cliCall{}, nil, false
	}
	return call, offered, true
}

// catalogueOpening opens the tool catalogue [cliagent.RenderTools] puts in a
// prompt that offers tools: its heading and the fence its JSON list sits in.
const catalogueOpening = "## Available tools\n\n```json\n"

// offeredTools names the tools a prompt actually offers.
//
// THE TOOL DEFINITIONS, not the prose. What distinguishes the phases is what
// each can invoke — submit_work, submit_review, or neither — and that is the
// catalogue the backend renders from the request's tools, not the names the
// prompt happens to mention: matching the whole text read the same names out
// of prompts that merely mention them, which is exactly how the first version
// of this answered the reviewer with a work submission.
//
// READ FROM THE END. A prompt that offers tools ENDS with
// [cliagent.RenderContract], and the catalogue is the last thing before it;
// one that offers none carries neither. So a catalogue quoted inside a message
// — which sits above both — is never read as one.
//
// AN ERROR WHERE THE CONTRACT IS THERE AND NO CATALOGUE CAN BE READ, which is
// the backend's rendering having moved under this reader: answered as nothing
// offered, every phase would fall through to a prose round and each case would
// wait out its budget for a submission that never comes.
func offeredTools(prompt string) (map[string]bool, error) {
	offered := map[string]bool{}
	body, offers := strings.CutSuffix(strings.TrimSpace(prompt),
		strings.TrimSpace(cliagent.RenderContract()))
	if !offers {
		return offered, nil
	}
	at := strings.LastIndex(body, catalogueOpening)
	if at < 0 {
		return nil, fmt.Errorf("the prompt ends in the response contract with no %q "+
			"catalogue before it: the CLI backend's rendering moved, and "+
			"offeredTools has to move with it", strings.TrimSpace(catalogueOpening))
	}
	var tools []struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(strings.NewReader(body[at+len(catalogueOpening):])).
		Decode(&tools); err != nil {
		return nil, fmt.Errorf("reading the tool catalogue: %w", err)
	}
	if len(tools) == 0 {
		return nil, errors.New("the tool catalogue lists no tools")
	}
	for _, tool := range tools {
		offered[tool.Name] = true
	}
	return offered, nil
}

// sectionHeading is a heading [cliagent.RenderPrompt] opens a section of the
// transcript with: a message's role, a tool result naming its call, or the
// catalogue and the contract that close a prompt offering tools.
//
// THE ROLES ARE LOWERCASE AND ALONE ON THEIR LINE, which is what keeps a
// heading INSIDE a message — a page's "## Staging runbook" in a search result —
// from cutting the section it sits in short.
var sectionHeading = regexp.MustCompile(
	`(?m)^## (?:system|user|assistant|tool result: .+|Available tools|Response contract)$`)

// toolResultPrefix is how [cliagent.RenderPrompt] heads a tool result.
const toolResultPrefix = "## tool result: "

// toolResultsIn is every tool result a prompt hands the model back, in order:
// the body of each `## tool result: <tool> (call <id>)` section.
func toolResultsIn(prompt string) []string {
	heads := sectionHeading.FindAllStringIndex(prompt, -1)
	var out []string
	for i, head := range heads {
		if !strings.HasPrefix(prompt[head[0]:head[1]], toolResultPrefix) {
			continue
		}
		end := len(prompt)
		if i+1 < len(heads) {
			end = heads[i+1][0]
		}
		out = append(out, strings.TrimSpace(prompt[head[1]:end]))
	}
	return out
}

// replyUsage is the usage block one scripted answer reports.
type replyUsage struct{ input, output, cacheRead, cacheWrite int }

// The usage each scripted answer reports. NAMED rather than inlined in the
// JSON, because a case that reasons about a budget has to know what each call
// costs, and a figure restated in a comment beside a literal goes stale the
// first time the literal moves — the budget case's own comment said 150 and
// 130 long after the fixture began reporting cache tokens.
var (
	toolUseUsage   = replyUsage{input: 120, output: 30, cacheRead: 80, cacheWrite: 15}
	textReplyUsage = replyUsage{input: 90, output: 40, cacheRead: 60, cacheWrite: 25}
)

// tokens is what the engine charges for one such answer: the CLI backend
// counts the prompt whole, both cache figures folded into its input, and the
// output beside it — so every one of them is counted (cliagent's completion).
func (u replyUsage) tokens() int { return u.input + u.cacheRead + u.cacheWrite + u.output }

// cliOutput is what the scripted CLI prints for one answer: the document
// [scriptedEntry]'s text_paths, error_paths and usage paths read.
func cliOutput(result string, usage replyUsage) string {
	out, _ := json.Marshal(map[string]any{
		"result":   result,
		"is_error": false,
		"usage": map[string]int{
			"input_tokens":                usage.input,
			"output_tokens":               usage.output,
			"cache_read_input_tokens":     usage.cacheRead,
			"cache_creation_input_tokens": usage.cacheWrite,
		},
	})
	return string(out)
}

// toolUse is an answer whose content is one tool call, in the response
// envelope the backend's contract asks for ([cliagent.ParseEnvelope]).
func toolUse(name string, input map[string]any) string {
	envelope, _ := json.Marshal(map[string]any{
		"message":    "",
		"tool_calls": []map[string]any{{"name": name, "arguments": input}},
	})
	return cliOutput("```json\n"+string(envelope)+"\n```", toolUseUsage)
}

// textReply is an answer that is plain prose — a phase that finished without
// calling anything, or an auxiliary pass's answer, which the backend hands
// back whole.
func textReply(text string) string {
	return cliOutput(text, textReplyUsage)
}

// THE MODEL READS A PROMPT AS THE BACKEND RENDERS IT — checked against the
// backend's own renderer, so a change to how a transcript is flattened fails
// here, by name, rather than as every case here waiting out its budget for a
// phase that never submits.
//
// What it reads is what the phases are keyed on: the tools a request OFFERS,
// never a catalogue some message quotes, and each tool result whole, a heading
// inside it included, up to the section after it.
func TestTheScriptedModelReadsThePromptTheBackendRenders(t *testing.T) {
	t.Parallel()
	quoted := "the reviewer was shown this:\n\n" + catalogueOpening +
		`[{"name":"submit_work"}]` + "\n```"
	result := "Filed ENG-1.\n\n## Staging runbook\n\nDeploys go out on Thursdays."
	req := llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleUser, Content: quoted},
			{Role: llm.RoleAssistant, ToolCalls: []llm.ToolCall{{
				ID: "cli_0", Name: "create_work_item", Arguments: map[string]any{"title": "x"},
			}}},
			{Role: llm.RoleTool, Name: "create_work_item", ToolCallID: "cli_0", Content: result},
			{Role: llm.RoleUser, Content: "Now review the turn."},
		},
		Tools: []llm.ToolDef{
			{Name: "submit_review", Description: "Record the review."},
			{Name: "create_work_item", Description: "File a work item."},
		},
	}
	prompt, err := cliagent.RenderPrompt(req)
	if err != nil {
		t.Fatalf("RenderPrompt: %v", err)
	}
	offered, err := offeredTools(prompt)
	if err != nil {
		t.Fatalf("offeredTools: %v", err)
	}
	if want := map[string]bool{"submit_review": true, "create_work_item": true}; !maps.Equal(offered, want) {
		t.Errorf("the prompt offers %v, want the request's own tools %v", offered, want)
	}
	if got := toolResultsIn(prompt); !slices.Equal(got, []string{result}) {
		t.Errorf("the tool results read %q, want the one result whole: %q", got, result)
	}

	// A prompt that offers nothing offers nothing, whatever it quotes.
	req.Tools = nil
	if prompt, err = cliagent.RenderPrompt(req); err != nil {
		t.Fatalf("RenderPrompt with no tools: %v", err)
	}
	if offered, err := offeredTools(prompt); err != nil || len(offered) != 0 {
		t.Errorf("a prompt offering no tools offers %v (err %v)", offered, err)
	}
}

// THE SCRIPTED CLI ANSWERS THE BACKEND THE GOLDEN COMPANY BUILDS — built from
// the document as every node here builds it ([engine.BuildCLIAgent]) and called
// as a phase and an auxiliary pass call it. Every case in this package stands
// on this round trip, and a break in it — the marker not reaching the child,
// the system prompt off its channel, a usage path the backend no longer reads,
// an answer the backend rewrites — reaches them only as a turn that never
// ends, so it is held here, where it can say which.
func TestTheScriptedCLIAnswersTheBackendTheCompanyBuilds(t *testing.T) {
	t.Parallel()
	model := newScriptedModel(t)
	cfg, err := config.ParseCompany([]byte(goldenCompany(model)))
	if err != nil {
		t.Fatalf("company config: %v", err)
	}
	provider, err := engine.BuildCLIAgent("scripted", cfg.Providers.LLM["scripted"],
		config.NewResolver(nodeEnvironment(nil)))
	if err != nil {
		t.Fatalf("BuildCLIAgent: %v", err)
	}
	ctx := llm.WithSeat(t.Context(), "ceo")

	// A PHASE: a system prompt, a conversation, and a tool to finish with,
	// sent as the tool loop sends a seat's round.
	const system = "You are the CEO of Nimbus. You answer to the founder."
	work, err := provider.Complete(ctx, llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: system},
			{Role: llm.RoleUser, Content: "How did the week go?"},
		},
		Tools: []llm.ToolDef{{Name: "submit_work", Description: "Finish the work."}},
		Agent: true,
	})
	if err != nil {
		t.Fatalf("an executor round: %v", err)
	}
	if len(work.ToolCalls) != 1 || work.ToolCalls[0].Name != "submit_work" ||
		work.ToolCalls[0].Arguments["outcome"] != "delivered" {
		t.Errorf("the executor round came back as %+v, want the scripted submit_work", work.ToolCalls)
	}
	charged(t, "a tool-use answer", work, toolUseUsage)
	if got := model.seen(); !slices.Equal(got, []string{"execute"}) {
		t.Errorf("the model recorded %v for one executor round", got)
	}
	// ON ITS OWN CHANNEL, WHOLE, and nowhere else: the system prompt every
	// prompt case here reads is the one the CLI was handed apart from the
	// transcript — on the append channel, after the note that says the CLI's
	// own prompt stays in force and the seat's is the role.
	got := model.systemPrompts()
	if len(got) != 1 || !strings.HasSuffix(got[0], system) || got[0] == system {
		t.Errorf("the model was shown the system prompt %q, want %q after the harness note", got, system)
	}
	model.mu.Lock()
	transcript := model.prompts[0]
	model.mu.Unlock()
	if strings.Contains(transcript, system) {
		t.Errorf("the system prompt rode the transcript as well as its own channel:\n%s", transcript)
	}

	// AN AUXILIARY PASS: no tools, and an answer handed back whole — a JSON
	// object included, which is the persist decider's whole contract.
	persist, err := provider.Complete(ctx, llm.Request{
		Messages: []llm.Message{
			{Role: llm.RoleSystem, Content: learning.PersistSystemPrompt},
			{Role: llm.RoleUser, Content: "The founder asked for the weekly numbers, plainly."},
		},
	})
	if err != nil {
		t.Fatalf("an auxiliary pass: %v", err)
	}
	if want := auxiliaryAnswer("persist"); persist.Content != want || len(persist.ToolCalls) != 0 {
		t.Errorf("the persist pass came back as %q with calls %+v, want %q whole",
			persist.Content, persist.ToolCalls, want)
	}
	charged(t, "a text answer", persist, textReplyUsage)
	if got := model.seen(); !slices.Equal(got, []string{"execute", "aux:persist"}) {
		t.Errorf("the model recorded %v, want the executor round and the persist pass", got)
	}
	// Whole and alone, on the replace channel: a side pass has no use for
	// the CLI's own prompt.
	if got := model.systemPrompts(); len(got) != 2 || got[1] != strings.TrimSpace(learning.PersistSystemPrompt) {
		t.Errorf("the persist pass was shown %q, want its own prompt alone", got)
	}
	if bad := model.misrouted(); len(bad) > 0 {
		t.Errorf("system prompts on the wrong channel: %s", strings.Join(bad, "; "))
	}
}

// charged fails the test unless completion carries exactly what the scripted
// answer reported: the counts every budget case here reasons from.
func charged(t *testing.T, what string, completion *llm.Completion, usage replyUsage) {
	t.Helper()
	if completion.InputTokens != usage.input+usage.cacheRead+usage.cacheWrite ||
		completion.OutputTokens != usage.output ||
		completion.CacheRead != usage.cacheRead || completion.CacheWrite != usage.cacheWrite {
		t.Errorf("%s was counted in=%d out=%d cache_read=%d cache_write=%d, want the "+
			"%+v the CLI reported", what, completion.InputTokens, completion.OutputTokens,
			completion.CacheRead, completion.CacheWrite, usage)
	}
	if got := completion.InputTokens + completion.OutputTokens; got != usage.tokens() {
		t.Errorf("%s costs %d tokens, want the %d the budget cases count it as",
			what, got, usage.tokens())
	}
}
