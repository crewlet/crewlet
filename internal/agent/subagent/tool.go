package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/tools"
)

// The LLM-callable side of delegation.
//
// # One shape, not two
//
// The tool this replaces had two: a single spawn (`task_prompt` +
// `system_prompt`) and a batch (`tasks`), mutually exclusive in prose because
// no schema could say it. Serialisers flatten a `oneOf` into a payload
// carrying both branches, so the engine had to guess which the model meant —
// and the two branches were separate code paths that had to classify failures
// identically and did not.
//
// There is one shape now: `tasks`. A single delegation is a list of one,
// which costs the model four extra characters and removes a whole class of
// ambiguity.
//
// # Everything the model needs is named in the refusal
//
// Every refusal here names the task, says what is wrong, and says what to
// write instead — the available workers, the ids in this call, the tasks that
// form the cycle. A model given "invalid request" re-sends the same request.

// Tool is `delegate`, bound to ONE parent turn.
//
// Bound rather than registered globally: everything a call needs — the
// parent's surface, its remaining budget, its trace, its visible workers — is
// per-turn. The alternative is a global registration that reaches those
// values by hanging a map off the tool-call context and validating its shape
// at every call. A closure over the turn's Config makes that whole class of
// "engine config missing or malformed" failure unrepresentable.
type Tool struct{ cfg Config }

var _ tools.Callable = (*Tool)(nil)

// NewTool binds the tool to a turn's config.
func NewTool(cfg Config) *Tool { return &Tool{cfg: cfg} }

// Name is the tool's name in the registry.
func (t *Tool) Name() string { return ToolName }

// Description is what the model is told this tool does.
//
// THE ANTI-CREEP BOUNDS ARE IN THE PROSE, not only in the caps. A model that
// reads "delegate work" with no sense of what belongs here will delegate a
// two-line lookup it could have done with one tool call, and pay a whole
// worker's prompt for it. Saying what this is NOT for is what stops that.
func (t *Tool) Description() string {
	return "Hand narrowly-scoped work to one or more short-lived workers and " +
		"get their answers back in this same turn. Each worker runs its own " +
		"tool loop with a slice of YOUR tools, cannot delegate further, " +
		"cannot contact colleagues, and cannot write to any shared surface " +
		"(no posting, commenting or opening PRs) — it reads, reasons, and " +
		"reports back to you.\n\n" +
		"Use it when the work is genuinely separable: several independent " +
		"reads, a bounded research task, summarising something large, or a " +
		"gather-then-synthesise shape (give the synthesising task an " +
		"`after`). Do NOT use it for something you can do with one or two of " +
		"your own tool calls — a worker costs a whole prompt and a model " +
		"call — and do not use it to reach a colleague: that is what your " +
		"colleague tools are for." +
		t.workerLine()
}

// workerLine names the seat's templates in the tool description itself.
//
// In the DESCRIPTION as well as the system prompt, because the description is
// what a provider sends alongside the schema on every round, and a model
// picking a worker mid-loop reads that rather than scrolling back to a system
// prompt from ten rounds ago.
func (t *Tool) workerLine() string {
	names := config.WorkerNames(t.cfg.Workers)
	if len(names) == 0 {
		return "\n\nThis seat has no worker templates configured, so every task " +
			"needs its own `system_prompt`."
	}
	return "\n\nWorkers available to this seat: " + strings.Join(names, ", ") +
		". See `## Your workers` in your system prompt for what each is for."
}

// Parameters is the tool's JSON Schema.
func (t *Tool) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"tasks": map[string]any{
				"type":     "array",
				"minItems": 1,
				"description": "The work to hand out. Tasks with no `after` run " +
					"concurrently; a task with `after` waits for those tasks and is " +
					"given their answers. Results come back in this same order.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"id": map[string]any{
							"type": "string",
							"description": "Your name for this task, unique within the " +
								"call. Results are returned under it, and `after` " +
								"references it.",
						},
						"worker": map[string]any{
							"type": "string",
							"description": "A worker template from your `## Your workers` " +
								"list. Use one where it fits — it carries a tested prompt, " +
								"a tool set and the shape of the answer. Mutually " +
								"exclusive with `system_prompt`.",
						},
						"system_prompt": map[string]any{
							"type": "string",
							"description": "An inline worker's persona and standing " +
								"instructions, for work no template covers. The runtime " +
								"appends the mandatory preamble. Mutually exclusive with " +
								"`worker`.",
						},
						"prompt": map[string]any{
							"type": "string",
							"description": "The concrete task, seeded as the worker's first " +
								"user message. Say what DONE looks like — the worker cannot " +
								"see your conversation and cannot ask you anything.",
						},
						"tools": map[string]any{
							"type":  "array",
							"items": map[string]any{"type": "string"},
							"description": "Narrow the worker's tools for this task. Omit to " +
								"take the template's. Must be a subset of your own tools; " +
								"`" + ToolName + "`, colleague tools and anything that writes " +
								"to a shared surface are rejected regardless.",
						},
						"after": map[string]any{
							"type":  "array",
							"items": map[string]any{"type": "string"},
							"description": "Task ids in this same call that must SUCCEED " +
								"first. Their submitted answers are given to this task. If " +
								"one of them fails, this task is skipped rather than run on " +
								"missing input.",
						},
						"max_turns": map[string]any{
							"type": "integer",
							"description": "Tool-round cap for this worker. Capped at the " +
								"runtime maximum; asking for more is silently clamped.",
						},
						"model": map[string]any{
							"type": "string",
							"description": "A configured providers.llm key to run this task " +
								"on. Omit to take the seat's own worker model.",
						},
					},
					"required": []any{"id", "prompt"},
				},
			},
		},
		"required": []any{"tasks"},
	}
}

// callArgs is the tool's argument shape. The struct tags ARE the schema
// above; decoding through JSON rather than field by field keeps one
// definition of the mapping instead of two that can disagree.
type callArgs struct {
	Tasks []taskArgs `json:"tasks"`
}

type taskArgs struct {
	ID           string    `json:"id"`
	Worker       string    `json:"worker"`
	SystemPrompt string    `json:"system_prompt"`
	Prompt       string    `json:"prompt"`
	Tools        []string  `json:"tools"`
	After        []string  `json:"after"`
	MaxTurns     turnCount `json:"max_turns"`
	Model        string    `json:"model"`
}

// turnCount accepts a round cap sent as a number OR as a string.
//
// Models send `"max_turns": "5"` often enough that refusing it costs a real
// fraction of calls, and the refusal reaches the model as a schema error it
// cannot tell from a semantic one. Accepting both is not leniency about the
// value — an unparseable string is still an error — it is leniency about
// which JSON type a serialiser chose.
type turnCount int

func (t *turnCount) UnmarshalJSON(b []byte) error {
	var n int
	if err := json.Unmarshal(b, &n); err == nil {
		*t = turnCount(n)
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("max_turns must be a number, got %s", string(b))
	}
	if strings.TrimSpace(s) == "" {
		*t = 0
		return nil
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return fmt.Errorf("max_turns must be a number, got %q", s)
	}
	*t = turnCount(n)
	return nil
}

// Call plans and runs the delegate call.
func (t *Tool) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	var parsed callArgs
	if err := remarshal(args, &parsed); err != nil {
		return failed(err.Error()), nil
	}

	req := Request{Tasks: make([]Task, 0, len(parsed.Tasks))}
	for _, a := range parsed.Tasks {
		req.Tasks = append(req.Tasks, Task{
			ID: a.ID, Worker: a.Worker, SystemPrompt: a.SystemPrompt,
			Prompt: a.Prompt, Tools: a.Tools, After: a.After,
			MaxTurns: int(a.MaxTurns), Model: a.Model,
		})
	}

	results, err := Run(ctx, t.cfg, req)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return tools.Result{}, ctxErr
		}
		// A PLAN REFUSAL IS A FAILED TOOL RESULT, not an engine error: it
		// names the task and says what to write instead, and the model is
		// the one that can act on it. An engine error would end the round
		// with the executor never seeing why.
		return failed(err.Error()), nil
	}
	if cancelled(results) {
		// The turn is being torn down. Reporting this to the model is
		// noise on a conversation that is about to stop; the loop's own
		// context check is what ends it.
		return tools.Result{}, context.Cause(ctx)
	}
	// The tool itself SUCCEEDED even when tasks failed: per-task outcomes
	// ride inside the payload so the parent can pick out which need a
	// retry. Marking the whole call failed would tell it to throw away the
	// siblings that worked.
	return tools.Result{Output: renderResults(results)}, nil
}

// cancelled reports a call every one of whose tasks died with the turn.
//
// EVERY one, not any: a single cancelled task inside a call that otherwise
// finished is a result the parent should still read, and swallowing the whole
// report for it would discard finished work.
func cancelled(results []Result) bool {
	for _, r := range results {
		if r.Status != StatusCancelled {
			return false
		}
	}
	return len(results) > 0
}

// taskReport is one task's entry in the tool result.
type taskReport struct {
	ID     string `json:"id"`
	Worker string `json:"worker,omitempty"`
	Status string `json:"status"`
	// Result is the submitted fields, present on `ok`.
	Result map[string]any `json:"result,omitempty"`
	// Text is the worker's prose, present when there is no submission to
	// stand in its place — a `no_result`, or a partial transcript from a
	// task that was cut off.
	Text          string   `json:"text,omitempty"`
	TurnsUsed     int      `json:"turns_used"`
	TokensUsed    int      `json:"tokens_used"`
	Model         string   `json:"model,omitempty"`
	RejectedTools []string `json:"rejected_tools,omitempty"`
	Error         string   `json:"error,omitempty"`
}

// renderResults is the tool's output: JSON, so the parent reads the per-task
// split as data rather than re-parsing a prose blob.
//
// The SUBMISSION and the PROSE never both appear. A task that answered
// structurally has its fields; one that did not has its prose, labelled by a
// status that says so. Sending both would invite the parent to reconcile two
// accounts of one task — and to prefer the prose, which is longer.
func renderResults(results []Result) string {
	reports := make([]taskReport, 0, len(results))
	for _, r := range results {
		rep := taskReport{
			ID: r.ID, Worker: r.Worker, Status: string(r.Status),
			TurnsUsed: r.Rounds, TokensUsed: r.Tokens(), Model: r.Model,
			RejectedTools: r.Rejected, Error: r.Error,
		}
		if r.Status == StatusOK {
			rep.Result = r.Output
		} else {
			rep.Text = r.Text
		}
		reports = append(reports, rep)
	}
	blob, err := json.Marshal(map[string]any{"tasks": reports})
	if err != nil {
		// Nothing in taskReport can refuse to marshal EXCEPT a submitted
		// field the schema let through, so this is reachable — and
		// returning an empty string would hand the parent silence for a
		// call that ran and cost tokens.
		log.Error("subagent_report_render_failed", "error", err)
		return fmt.Sprintf("%d workers ran; their results could not be rendered: %v",
			len(results), err)
	}
	return string(blob)
}

// remarshal moves a decoded argument map into a typed struct, via JSON
// because the arguments already came off the wire as JSON and the struct tags
// are the schema.
func remarshal(args map[string]any, into any) error {
	blob, err := json.Marshal(args)
	if err != nil {
		return fmt.Errorf("%s: arguments could not be re-encoded: %w", ToolName, err)
	}
	if err := json.Unmarshal(blob, into); err != nil {
		return fmt.Errorf("%s: arguments do not match the schema: %w", ToolName, err)
	}
	return nil
}

// failed is a tool refusal the model reads and can act on.
func failed(msg string) tools.Result { return tools.Result{Output: msg, Failed: true} }

// AsPlanError reports whether an error from [Run] is a refusal the model can
// fix, rather than a wiring failure.
//
// Exported for a caller that wants to log the two differently: a plan error
// is the model's to correct and belongs at info, while a wiring failure is
// the operator's and belongs at error.
func AsPlanError(err error) (*PlanError, bool) {
	var pe *PlanError
	ok := errors.As(err, &pe)
	return pe, ok
}
