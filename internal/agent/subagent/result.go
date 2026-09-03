package subagent

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/structured"
	"github.com/crewlet/crewlet/internal/agent/toolloop"
)

// How a worker answers, and how that answer is classified.
//
// # A worker submits, it does not narrate
//
// A worker used to end by writing prose, and the parent's only option was to
// read it. That is fine for "summarise this page" and useless for anything
// the parent then has to ACT on: an executor that asked three workers to
// check three repositories got back three paragraphs and had to spend its own
// model call turning them into three verdicts — a call that could get the
// parsing wrong, on prose that had no obligation to be parseable.
//
// So a worker ends the way every other phase in this engine ends: by CALLING
// A TOOL with typed arguments (internal/agent/structured). What comes back is
// fields the parent can index, and the shape of those fields is declared in
// the worker's template where a founder can read it.
//
// # An absent submission is never synthesised
//
// A worker that produced prose and never called submit_result has NOT
// answered, and the engine says so — status `no_result`, with the prose
// attached. Filling in a result from the transcript would be the engine
// putting words in the worker's mouth on the one question the parent asked,
// and a dependent task fed a fabricated answer produces a confident wrong
// one. The prose is still handed back, because eight rounds of work are worth
// reading even when the last step was skipped.

// SubmitTool is the name a worker calls to answer.
//
// Deliberately not `submit_work`: a worker and an executor answer different
// questions with different schemas, and one name for two shapes is how a
// model that has seen both prompts submits the wrong one.
const SubmitTool = "submit_result"

// Status is what became of one task. It rides the Result, the tool's report
// to the parent and the phase event, so it is a wire string rather than an
// internal enum.
type Status string

const (
	// StatusOK — the worker submitted a result. The ONLY status a
	// dependent task will run on.
	StatusOK Status = "ok"

	// StatusNoResult — the worker ran and never submitted. Its prose is
	// in Text; nothing is invented to stand in for the missing fields.
	StatusNoResult Status = "no_result"

	// StatusSkipped — a task this one declared `after` did not succeed,
	// so it never ran. Classified BEFORE any deadline reading: a task
	// skipped because its input never arrived is not a task that ran out
	// of time, and reporting it as a timeout sends the parent to retry
	// with fewer workers when the answer is to fix the dependency.
	StatusSkipped Status = "skipped_dependency_failed"

	// StatusNeverStarted — the call's deadline landed while this task was
	// queued behind max_parallel. Its own status because it is the one
	// failure a parent can retry unchanged.
	StatusNeverStarted Status = "never_started"

	// StatusTimedOut — the task's own cap or the call's expired.
	StatusTimedOut Status = "timed_out"

	// StatusBudget — the call's shared token slice refused a charge.
	StatusBudget Status = "budget_exhausted"

	// StatusCancelled — the parent turn was torn down under it.
	StatusCancelled Status = "cancelled"

	// StatusFailed — anything else the loop returned: a provider that
	// would not answer, a surface that broke, a contained panic.
	StatusFailed Status = "failed"
)

// Succeeded reports whether this task produced the structured answer it was
// asked for.
//
// The ONE question dependents turn on, and deliberately strict: `no_result`
// is prose where fields were promised, and a dependent handed prose in place
// of the fields its prompt describes answers confidently about nothing.
func (s Status) Succeeded() bool { return s == StatusOK }

// Failed reports whether the task ended badly enough to be worth the parent's
// attention. A `no_result` counts: the worker did not answer the question.
func (s Status) Failed() bool { return s != StatusOK }

// resultPayload is what a worker submits.
//
// Two shapes behind one tool. With no declared schema the worker fills
// [defaultResultSchema] and the whole submission IS the answer; with one, the
// submission is that schema's own fields. Either way the decoder keeps the
// arguments as a map, because the parent reads them as data and the engine
// has nothing to do with their meaning.
type resultPayload struct {
	// Fields is the submission verbatim.
	Fields map[string]any
}

// defaultResultSchema is what a worker with no declared output answers.
//
// Two fields rather than one bare string: `result` is the answer and `notes`
// is everything a worker wants to say ABOUT the answer — what it could not
// check, what it assumed, where it stopped. Folded into one field those
// caveats end up inside the value the parent is about to use.
var defaultResultSchema = map[string]any{
	"type": "object",
	"properties": map[string]any{
		"result": map[string]any{
			"type": "string",
			"description": "Your answer to the task, complete on its own. The " +
				"agent that delegated this reads only what you submit here — it " +
				"cannot see your tool calls or your working.",
		},
		"notes": map[string]any{
			"type": "string",
			"description": "Anything about the answer rather than in it: what you " +
				"could not check, what you assumed, where you stopped.",
		},
	},
	"required": []any{"result"},
}

// submitDescription is the contract as the worker receives it.
const submitDescription = "Submit your answer and end. Call this exactly once, when " +
	"you are done. The agent that delegated this task reads ONLY these fields — " +
	"your tool calls, your reasoning and any prose you wrote are not passed on. " +
	"If you could not finish, submit what you do have and say why in the fields " +
	"the schema gives you; submitting a partial answer is worth far more than " +
	"submitting nothing."

// newSubmitTool builds one worker's submission tool over its declared schema.
//
// The schema is the WORKER'S, so two workers in one call publish different
// shapes under one tool name — which is fine, because each runs in its own
// message history and neither ever sees the other's definition.
func newSubmitTool(schema map[string]any) *structured.Tool[resultPayload] {
	if len(schema) == 0 {
		schema = defaultResultSchema
	}
	return structured.New(SubmitTool, submitDescription, schema, decodeResult(schema))
}

// decodeResult validates a submission against the parts of its schema the
// engine itself depends on.
//
// REQUIRED FIELDS ONLY, and that is the whole of it. Types, formats and
// enums are the provider's to enforce — it has the schema and does so before
// the call is even made — but a missing required field arrives as an absent
// map key, which every reader downstream would silently read as an empty
// string. Rejecting it here bounces the submission back to the worker, which
// is the one failure a model reliably fixes.
func decodeResult(schema map[string]any) func(map[string]any) (resultPayload, error) {
	required := schemaRequired(schema)
	return func(args map[string]any) (resultPayload, error) {
		var missing []string
		for _, name := range required {
			v, ok := args[name]
			if !ok || isBlank(v) {
				missing = append(missing, name)
			}
		}
		if len(missing) > 0 {
			return resultPayload{}, fmt.Errorf(
				"these fields are required and are missing or empty: %s",
				strings.Join(missing, ", "))
		}
		return resultPayload{Fields: args}, nil
	}
}

// isBlank treats an empty string as absent.
//
// A model that has nothing for a field submits "" rather than omitting it,
// and a required field holding an empty string is the same absence with an
// extra step — the parent reads a key that is there and means nothing.
func isBlank(v any) bool {
	s, ok := v.(string)
	return ok && strings.TrimSpace(s) == ""
}

// schemaRequired reads the `required` list, tolerating both encodings a
// config round trip produces (YAML decodes to []any, a Go literal to
// []string).
func schemaRequired(schema map[string]any) []string {
	switch req := schema["required"].(type) {
	case []string:
		return req
	case []any:
		out := make([]string, 0, len(req))
		for _, v := range req {
			if s, ok := v.(string); ok {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

// Result is one task's record: everything the caller's phase event needs and
// everything the parent's model is told.
//
// It is produced on EVERY path a task can take, including the ones that never
// reached a model and the ones that never ran at all. A caller can publish
// from it unconditionally.
type Result struct {
	// ID is the task's own id, as the parent wrote it. It is how the
	// parent pairs an answer with the question it asked — by NAME rather
	// than by position, because a graph reorders and a list index does
	// not survive that.
	ID string

	// Worker is the template this task ran, or "" for an ad-hoc one.
	Worker string

	// Status says what became of it. See the constants above.
	Status Status

	// Output is the submitted fields, present exactly when Status is ok.
	Output map[string]any

	// Text is the worker's prose: its answer before the submission on a
	// finished task, or its partial transcript when it was cut off. Kept
	// on every path, because rounds the parent paid for are worth reading
	// even when the last step was skipped.
	Text string

	Rounds       int
	InputTokens  int
	OutputTokens int

	// Model is what actually served the calls; ProviderKey is the config
	// key its chain was resolved under.
	Model       string
	ProviderKey string

	// ToolsAvailable is what the task could call when it finished,
	// activations included. Rejected is what it asked for and did not
	// get, in request order.
	ToolsAvailable []string
	Rejected       []string

	Executions []toolloop.Execution

	// Narration is what the worker thought and said in each round, keyed on
	// the same round number its Executions carry. Without it a worker's phase
	// event ships tool calls with nothing that asked for them: a consumer
	// interleaves the two lists on that shared number, and one list alone
	// leaves it to fall back on the joined Text — where every round's
	// reasoning after the first renders as the worker's answer, `<think>`
	// tags and all. It is the same contract the turn's own phases publish.
	Narration []toolloop.Narration

	// SystemPrompt and UserPrompt are what the worker was actually sent.
	SystemPrompt string
	UserPrompt   string

	// Error is why it stopped, when it did. Empty on ok and on no_result.
	Error string
}

// Tokens is the task's total spend.
func (r Result) Tokens() int { return r.InputTokens + r.OutputTokens }

// Failed reports whether this task is worth the parent's attention.
func (r Result) Failed() bool { return r.Status.Failed() }

// TimedOut narrows a failure to the wall-clock cases, which is the one class
// a parent can usefully retry with fewer tasks.
func (r Result) TimedOut() bool { return r.Status == StatusTimedOut }

// Answer renders what a dependent task is shown, and what the parent reads.
//
// The SUBMISSION where there is one, as JSON: it is already structured, the
// parent asked for that shape, and re-flattening it into prose would undo the
// whole reason the worker submitted rather than narrated. Prose otherwise,
// which is what a `no_result` has.
func (r Result) Answer() string {
	if r.Status == StatusOK && len(r.Output) > 0 {
		if blob, err := json.Marshal(r.Output); err == nil {
			return string(blob)
		}
		// A submission that will not re-marshal cannot happen through the
		// tool path (it arrived as JSON), so this is a caller that built a
		// Result by hand. Fall through to the prose rather than returning
		// silence.
	}
	return r.Text
}

// classify turns a finished loop's error into a status and a message.
//
// The CAUSE decides, never the error's shape: a provider's own HTTP client
// returns DeadlineExceeded for its own reasons, and reporting somebody
// else's timeout as the worker's cap sends an operator to raise a limit that
// was never reached.
func classify(kind, reason string, err error) (Status, string) {
	switch {
	case kind == KindTimeout:
		return StatusTimedOut, reason
	case kind == KindCancelled:
		return StatusCancelled, reason
	case kind != "":
		return StatusFailed, reason
	case errors.Is(err, toolloop.ErrBudgetExhausted):
		var be *toolloop.BudgetError
		if errors.As(err, &be) && be.Scope == ScopeSubagent {
			// The call's own slice, not the seat's cap. Its own status so
			// nobody goes looking for a company budget that never ran out.
			return StatusBudget, ledger.Elide(err.Error(), errorLimit)
		}
		return StatusFailed, ledger.Elide(err.Error(), errorLimit)
	default:
		return StatusFailed, ledger.Elide(err.Error(), errorLimit)
	}
}
