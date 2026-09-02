package config

import (
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strings"
)

// Worker templates: the reusable delegates a seat's executor can hand work to.
//
// # Why a template rather than a prompt written per call
//
// The executor could always write a worker's whole system prompt inline, and
// it still can. What it could not do was write the SAME one twice. Every
// spawn was a fresh improvisation: a different tool list, a different notion
// of what "done" meant, a different shape of answer coming back. That is
// expensive in a way nothing measured — the parent paid for a prompt it had
// already written on the previous turn, and read back prose it then had to
// re-parse into whatever it needed.
//
// A template moves the stable half into config, where a founder can read it,
// edit it live and version it with the rest of the company: the persona, the
// tool grant, the model, and the SHAPE OF THE ANSWER. What stays per call is
// the only thing that genuinely varies — the task.
//
// # The template is a request, not a grant
//
// A template naming a tool does not confer it. Every name still passes
// through the sub-agent package's Permit, against the PARENT'S own live tool
// list and the engine-control denylist, so a template cannot widen a seat's
// reach beyond what the seat already has. That is deliberate: `workers:` is
// founder-owned Tier B config, and Tier B must never be a privilege
// escalation path. A template that names a tool its caller lacks simply has
// that name rejected, and the rejection is reported.

// workerKey is the template-name grammar.
//
// The same slug shape as a seat handle, and for the same reason: the name is
// what an executor types into a `delegate` call, so it has to be something a
// model can reproduce exactly from having read it once. Mixed case and spaces
// are where that goes wrong.
var workerKey = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)

// Worker is one reusable delegate template.
type Worker struct {
	// Description is what the EXECUTOR is told this worker is for. It is
	// the only part of the template that reaches the parent's prompt, so
	// it is written for the model choosing between workers rather than for
	// the operator: "reads sources and reports findings with citations"
	// beats "the research worker".
	//
	// Required, because a worker nobody can tell apart from another is a
	// worker the executor picks by name-similarity alone.
	Description string `yaml:"description" json:"description" js:"required" desc:"What this worker is for, written for the executor choosing one."`

	// SystemPrompt is the worker's persona and standing instructions — the
	// half of a delegate prompt that does not change per task.
	//
	// The runtime preamble (no nesting, no colleague contact, how to
	// answer) is appended to it, so a template never has to restate the
	// boundary and cannot weaken it by forgetting to.
	SystemPrompt string `yaml:"system_prompt" json:"system_prompt" js:"required" desc:"The worker's persona and standing instructions. The runtime preamble is appended."`

	// Tools is the allowlist this worker asks for. Empty means it runs
	// with no tools at all, which is the right shape for a summariser or a
	// drafter — a worker with no tools cannot go looking for anything, and
	// that bound is worth having on purpose.
	//
	// ABSENT AND EMPTY ARE THE SAME THING here, unlike a delegate task's
	// own `tools`, where nil means "take the template's". A template has
	// nothing to inherit from, so there is no third state to express — and
	// the store round trip normalises `[]` to absent, which would make the
	// difference unrepresentable even if there were.
	//
	// Every name is still filtered through the worker grant at spawn time;
	// see this file's own header.
	Tools []string `yaml:"tools,omitempty" json:"tools,omitempty" desc:"Tools this worker asks for. Still filtered against the caller's own tools at spawn."`

	// Model is the providers.llm key this worker runs on. Empty takes the
	// seat's llm_subagent chain, which is the ordinary case: an operator
	// who has already pointed fan-out work at a cheap model does not want
	// to repeat that per template.
	Model string `yaml:"model,omitempty" json:"model,omitempty" desc:"providers.llm key for this worker; empty takes the seat's llm_subagent chain."`

	// MaxTurns is this worker's tool-round cap. Zero takes the runtime's,
	// and a value above the runtime ceiling is CLAMPED rather than
	// refused — the same rule a per-task request follows, so a template
	// and a task cannot disagree about what an over-ask means.
	MaxTurns int `yaml:"max_turns,omitempty" json:"max_turns,omitempty" js:"min=0" desc:"Tool rounds this worker may run; 0 takes the runtime cap."`

	// Output is the JSON Schema the worker's `submit_result` publishes,
	// so the answer comes back as fields rather than prose.
	//
	// The point is what the PARENT does next. A worker that returns
	// `{"verdict": "...", "citations": [...]}` hands the executor
	// something it can act on directly; one that returns three paragraphs
	// hands it a re-parsing job that costs another model call to get
	// wrong. Absent takes the default result shape — see
	// internal/agent/subagent.
	//
	// A map rather than a typed schema builder because it IS a JSON Schema
	// and the model receives it verbatim; anything else would be a second
	// grammar to keep in step with the first.
	Output map[string]any `yaml:"output,omitempty" json:"output,omitempty" desc:"JSON Schema for this worker's structured answer. Absent takes the default shape."`
}

// validate holds the rules a template must satisfy before any turn can name
// it.
//
// AT LOAD, not at spawn. A malformed template discovered at spawn time costs
// the turn that happened to reach it first — hours after the edit, on
// whichever seat drew the short straw — and reports as a delegate failure
// rather than as the config error it is.
func (w Worker) validate(path string, providers map[string]struct{}, ceiling int) error {
	var p problems
	if strings.TrimSpace(w.Description) == "" {
		p.add(at(path, "description"), ErrMissing,
			"a worker needs a description — it is the only thing an executor "+
				"reads when choosing between workers")
	}
	if strings.TrimSpace(w.SystemPrompt) == "" {
		p.add(at(path, "system_prompt"), ErrMissing,
			"a worker needs a system_prompt — it is the persona the template exists to hold")
	}
	if w.MaxTurns < 0 {
		p.add(at(path, "max_turns"), ErrOutOfRange, "must not be negative, got %d", w.MaxTurns)
	} else if ceiling > 0 && w.MaxTurns > ceiling {
		// Named rather than silently clamped, because a template is
		// EDITED: an operator who writes 200 and gets 40 has been
		// overruled by a number nothing in the file mentions, and will
		// write it again next time.
		p.add(at(path, "max_turns"), ErrOutOfRange,
			"must not exceed turn_engine.delegation.max_turns_ceiling (%d), got %d",
			ceiling, w.MaxTurns)
	}
	for i, name := range w.Tools {
		if strings.TrimSpace(name) == "" {
			p.add(idx(at(path, "tools"), i), ErrMissing, "a tool name must not be empty")
		}
	}
	if w.Model != "" && providers != nil {
		if _, ok := providers[w.Model]; !ok {
			p.add(at(path, "model"), ErrUnknownValue,
				"names provider %q, which providers.llm does not configure — "+
					"configured: %s", w.Model, keyList(providers))
		}
	}
	p.wrap(validateOutputSchema(at(path, "output"), w.Output))
	return p.err()
}

// outputSchemaLimits are the bounds a worker's declared output schema has to
// fit.
//
// A schema is not free: it is sent with every round of the worker's loop, and
// a model asked to fill a hundred fields fills them badly. These are the
// bounds at which a structured answer is still an answer rather than a form.
const (
	// maxOutputProperties is how many top-level fields a result may have.
	// Twelve is the point past which a submission stops being "the answer
	// in fields" and becomes a record the worker is transcribing.
	maxOutputProperties = 12
	// maxOutputDepth bounds nesting. Three levels holds an object of
	// arrays of objects, which covers every shape a worker's answer has
	// wanted; deeper is a document, and a document belongs in prose.
	maxOutputDepth = 3
)

// validateOutputSchema refuses a schema the submission tool could not use.
//
// A SUBSET, deliberately. The engine does not implement JSON Schema — it
// hands the map to the provider, which does — so this checks only the
// properties the engine itself depends on: that it is an object schema with
// named properties, that every `required` entry is one of them, and that it
// is small enough to send on every round. A keyword this does not know is
// passed through untouched, because the provider may well support it and
// refusing it here would make the engine the bottleneck on somebody else's
// schema support.
func validateOutputSchema(path string, schema map[string]any) error {
	if len(schema) == 0 {
		return nil
	}
	var p problems
	if t, ok := schema["type"].(string); !ok || t != "object" {
		p.add(at(path, "type"), ErrUnknownValue,
			`must be "object" — a submission's arguments are named fields, and a `+
				`provider given a non-object schema for a tool call rejects the call`)
	}
	props, ok := schema["properties"].(map[string]any)
	if !ok || len(props) == 0 {
		p.add(at(path, "properties"), ErrMissing,
			"an output schema needs at least one property — an empty one asks "+
				"the worker to submit nothing, which is what leaving `output` out already does")
	}
	if len(props) > maxOutputProperties {
		p.add(at(path, "properties"), ErrOutOfRange,
			"has %d fields; at most %d — the schema is sent on every round of the "+
				"worker's loop, and a model asked for more fields than this fills them badly",
			len(props), maxOutputProperties)
	}
	for _, name := range requiredNames(schema) {
		if _, ok := props[name]; !ok {
			p.add(at(path, "required"), ErrUnknownValue,
				"requires %q, which is not one of its properties — a provider "+
					"validating the submission would refuse every call", name)
		}
	}
	if d := schemaDepth(schema, 0); d > maxOutputDepth {
		p.add(path, ErrOutOfRange,
			"nests %d levels deep; at most %d — deeper than that is a document, "+
				"and a document is what prose is for", d, maxOutputDepth)
	}
	return p.err()
}

// requiredNames reads the `required` list, tolerating the two encodings a
// YAML round trip produces.
func requiredNames(schema map[string]any) []string {
	raw, ok := schema["required"].([]any)
	if !ok {
		if names, ok := schema["required"].([]string); ok {
			return names
		}
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// schemaDepth measures nesting through `properties` and `items`, the two
// keywords that carry a subschema in every shape a worker's answer takes.
func schemaDepth(node any, at int) int {
	m, ok := node.(map[string]any)
	if !ok || at > maxOutputDepth+1 {
		// Bounded rather than unbounded recursion: a hand-authored schema
		// cannot cycle (YAML anchors expand), but the depth answer stops
		// mattering past the limit and the guard costs nothing.
		return at
	}
	deepest := at
	if props, ok := m["properties"].(map[string]any); ok {
		for _, v := range props {
			deepest = max(deepest, schemaDepth(v, at+1))
		}
	}
	if items, ok := m["items"]; ok {
		deepest = max(deepest, schemaDepth(items, at+1))
	}
	return deepest
}

// keyList renders a set of configured names for an error message.
func keyList(set map[string]struct{}) string {
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// validateWorkers checks the company's templates and every role's visibility
// list against them.
func (c *Company) validateWorkers() error {
	var p problems

	providers := make(map[string]struct{}, len(c.Providers.LLM))
	for key := range c.Providers.LLM {
		providers[key] = struct{}{}
	}
	// An empty provider map is the documented authoring state — an org
	// chart written before the credentials exist — so the model rule is
	// skipped entirely rather than firing on every template. Same rule
	// roles' provider keys follow; see validateProviderKeys.
	if len(providers) == 0 {
		providers = nil
	}

	ceiling := c.TurnEngine.Delegation.MaxTurnsCeiling
	for _, name := range sortedKeys(c.Workers) {
		path := at("workers", name)
		if !workerKey.MatchString(name) {
			p.add(path, ErrUnknownValue,
				"worker names are lowercase slugs matching %s — an executor types "+
					"this name into a delegate call, so it has to be reproducible "+
					"from having read it once", workerKey.String())
		}
		p.wrap(c.Workers[name].validate(path, providers, ceiling))
	}

	// A role naming a worker that does not exist gets NOTHING at runtime:
	// the visibility filter drops the name and the executor is offered a
	// shorter list, with no signal anywhere that a template was meant to
	// be there. That is the same silent-typo shape a role's provider key
	// has, and it is refused for the same reason.
	for i, r := range c.Roles {
		p.wrap(validateWorkerRefs(idx("roles", i), r.Workers, c.Workers))
	}
	for i := range c.Units {
		p.wrap(c.Units[i].validateWorkerRefs(idx("units", i), c.Workers))
	}
	return p.err()
}

// validateWorkerRefs checks one seat's visibility list.
func validateWorkerRefs(path string, refs []string, defined map[string]Worker) error {
	var p problems
	for i, name := range refs {
		if _, ok := defined[name]; !ok {
			p.add(idx(at(path, "workers"), i), ErrUnknownValue,
				"names worker %q, which the top-level workers: block does not "+
					"define — defined: %s", name, workerList(defined))
		}
	}
	return p.err()
}

// validateWorkerRefs walks a unit's own seats and its descendants.
func (u *Unit) validateWorkerRefs(path string, defined map[string]Worker) error {
	var p problems
	for i, r := range u.Roles {
		p.wrap(validateWorkerRefs(idx(at(path, "roles"), i), r.Workers, defined))
	}
	for i := range u.Children {
		p.wrap(u.Children[i].validateWorkerRefs(idx(at(path, "children"), i), defined))
	}
	return p.err()
}

// workerList renders the defined template names for an error message.
func workerList(defined map[string]Worker) string {
	if len(defined) == 0 {
		return "(none — the workers: block is empty or absent)"
	}
	return strings.Join(sortedKeys(defined), ", ")
}

// WorkersFor resolves the templates one seat may delegate to, in name order.
//
// An EMPTY visibility list means every template, which is the useful default:
// a company that publishes three workers wants its seats using them, and
// requiring each seat to opt in turns a shared library into a per-seat
// copy-paste. Naming any narrows to exactly those.
func (c *Company) WorkersFor(handle string) map[string]Worker {
	seat, refs := c.seatWorkerRefs(handle)
	if !seat {
		return nil
	}
	if len(refs) == 0 {
		return c.Workers
	}
	out := make(map[string]Worker, len(refs))
	for _, name := range refs {
		if w, ok := c.Workers[name]; ok {
			out[name] = w
		}
	}
	return out
}

// seatWorkerRefs finds a seat by handle and returns its visibility list.
//
// It reports whether the seat was FOUND separately from what it declared,
// because those are different facts: a seat with no `workers:` sees every
// template, and a handle that names no seat sees nothing at all. Collapsing
// them would hand an unknown handle the whole library.
func (c *Company) seatWorkerRefs(handle string) (found bool, refs []string) {
	for i := range c.Roles {
		if c.Roles[i].IdentityKey() == handle {
			return true, c.Roles[i].Workers
		}
	}
	for i := range c.Units {
		if found, refs := unitWorkerRefs(&c.Units[i], handle); found {
			return true, refs
		}
	}
	return false, nil
}

func unitWorkerRefs(u *Unit, handle string) (bool, []string) {
	for i := range u.Roles {
		if u.Roles[i].IdentityKey() == handle {
			return true, u.Roles[i].Workers
		}
	}
	for i := range u.Children {
		if found, refs := unitWorkerRefs(&u.Children[i], handle); found {
			return true, refs
		}
	}
	return false, nil
}

// WorkerNames is the sorted names of a template map.
func WorkerNames(m map[string]Worker) []string { return sortedKeys(m) }

// CloneWorkers deep-copies a template map, so a caller holding one cannot
// mutate the live config underneath a running turn.
func CloneWorkers(m map[string]Worker) map[string]Worker {
	if m == nil {
		return nil
	}
	out := make(map[string]Worker, len(m))
	for name, w := range m {
		w.Tools = slices.Clone(w.Tools)
		w.Output = cloneAny(w.Output)
		out[name] = w
	}
	return out
}

// cloneAny deep-copies a decoded JSON/YAML value.
//
// The output schema is handed to a provider and, from there, to a model —
// but it is also held on the live config cell that a config apply replaces.
// A shallow copy leaves two turns sharing one map, and the second apply
// mutates the schema the first turn is mid-way through sending.
func cloneAny(v map[string]any) map[string]any {
	if v == nil {
		return nil
	}
	out := make(map[string]any, len(v))
	for k, val := range v {
		out[k] = cloneValue(val)
	}
	return out
}

func cloneValue(v any) any {
	switch t := v.(type) {
	case map[string]any:
		return cloneAny(t)
	case []any:
		out := make([]any, len(t))
		for i, item := range t {
			out[i] = cloneValue(item)
		}
		return out
	default:
		return v
	}
}

// DescribeWorker renders one template as the line an executor reads.
//
// Here rather than in the prompt package because the shape of the line is a
// property of the template — what a worker IS, said once — and two renderers
// would let the prompt and the delegate tool's own refusal message describe
// the same worker differently.
func DescribeWorker(name string, w Worker) string {
	line := "- `" + name + "` — " + firstLine(w.Description)
	var notes []string
	if len(w.Tools) > 0 {
		notes = append(notes, "tools: "+strings.Join(w.Tools, ", "))
	}
	if len(w.Output) > 0 {
		if names := propertyNames(w.Output); len(names) > 0 {
			notes = append(notes, "returns: "+strings.Join(names, ", "))
		}
	}
	if len(notes) == 0 {
		return line
	}
	return line + " (" + strings.Join(notes, "; ") + ")"
}

// propertyNames is a schema's top-level fields, sorted.
func propertyNames(schema map[string]any) []string {
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		return nil
	}
	return sortedKeys(props)
}

// firstLine keeps a description to one line wherever it is rendered into a
// list — a multi-line description would break the enumeration it sits in.
func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(line)
}

// Delegation is the runtime bounds every `delegate` call runs under.
//
// Nested inside turn_engine rather than sitting beside the templates,
// because these are CAPS the engine enforces and `workers:` is content a
// founder authors. One is ops-shaped and the other is org-shaped, and a
// single block holding both would have `workers.max_parallel` sitting in the
// same namespace as `workers.researcher`.
type Delegation struct {
	// MaxParallel caps how many workers run at once, across the whole
	// call rather than per wave. Three is the point where the parent is
	// still paying for overlapping I/O rather than for contention: every
	// worker is a model call, and a fourth concurrent one mostly buys
	// provider rate-limit errors on the subscription backends.
	MaxParallel int `yaml:"max_parallel,omitempty" json:"max_parallel,omitempty" js:"min=1" desc:"Workers running at once within one delegate call."`

	// MaxTasksPerCall bounds one call's graph. Eight is generous for the
	// shape this is for — a handful of independent reads, or a two-stage
	// gather-then-synthesise — and small enough that a model which has
	// misunderstood the tool cannot spend a turn's whole budget in one
	// call before anything is observable.
	MaxTasksPerCall int `yaml:"max_tasks_per_call,omitempty" json:"max_tasks_per_call,omitempty" js:"min=1" desc:"Tasks one delegate call may contain."`

	// MaxTurns caps one worker's tool rounds. A request above it is
	// clamped, never refused.
	MaxTurns int `yaml:"max_turns,omitempty" json:"max_turns,omitempty" js:"min=1" desc:"Tool rounds one worker may run."`

	// MaxTurnsCeiling bounds what a TEMPLATE may ask for. The per-call
	// request is clamped silently because it is a model's estimate; a
	// template is a person's edit, so an over-ask is refused at load with
	// this number named. 40 is the round-cap extension judge's ceiling for
	// a worker — see turn_engine.execute_max_tool_rounds_ceiling for the
	// same relationship one level up.
	MaxTurnsCeiling int `yaml:"max_turns_ceiling,omitempty" json:"max_turns_ceiling,omitempty" js:"min=1" desc:"Highest max_turns a worker template may declare."`

	// BudgetFraction is the share of the parent turn's REMAINING tokens
	// one delegate call may spend — the total across every worker in it,
	// not each. A fifth leaves the parent four fifths to finish the turn
	// the fan-out was supposed to serve, which is the whole point: a
	// worker that consumed the turn's budget has answered a question
	// nobody can now act on.
	BudgetFraction float64 `yaml:"budget_fraction,omitempty" json:"budget_fraction,omitempty" js:"min=0;max=1" desc:"Share of the parent's remaining budget one delegate call may spend."`

	// MinTokensPerTask floors each worker's share. A call whose slice
	// divided by its task count falls below this is refused UP FRONT: N
	// workers that each die mid-round have spent the whole slice and
	// produced nothing, which is strictly worse than not starting.
	MinTokensPerTask int `yaml:"min_tokens_per_task,omitempty" json:"min_tokens_per_task,omitempty" js:"min=0" desc:"Floor on one worker's token share; below it the call is refused."`

	// TaskTimeoutSeconds bounds one worker. 300 rather than the 120 a
	// sub-agent had, because a worker now runs a real tool loop against
	// real MCP servers and a single slow page read used to eat a quarter
	// of the old cap.
	TaskTimeoutSeconds float64 `yaml:"task_timeout_seconds,omitempty" json:"task_timeout_seconds,omitempty" js:"min=0" desc:"Wall-clock cap on one worker."`

	// CallTimeoutSeconds bounds a whole call, waves included. 900 is
	// MaxParallel-deep enough for the two-wave shape this is for — a
	// gather wave and a synthesis wave, each able to spend its own task
	// cap — without letting a graph of eight stragglers hold a seat for a
	// quarter of an hour each.
	CallTimeoutSeconds float64 `yaml:"call_timeout_seconds,omitempty" json:"call_timeout_seconds,omitempty" js:"min=0" desc:"Wall-clock cap on a whole delegate call, waves included."`
}

// DefaultDelegation is the shipped bounds. Each number's reasoning is at its
// field.
func DefaultDelegation() Delegation {
	return Delegation{
		MaxParallel:        3,
		MaxTasksPerCall:    8,
		MaxTurns:           20,
		MaxTurnsCeiling:    40,
		BudgetFraction:     0.2,
		MinTokensPerTask:   500,
		TaskTimeoutSeconds: 300,
		CallTimeoutSeconds: 900,
	}
}

func (d Delegation) validate(path string) error {
	var p problems

	positive := []struct {
		name  string
		value int
	}{
		{"max_parallel", d.MaxParallel},
		{"max_tasks_per_call", d.MaxTasksPerCall},
		{"max_turns", d.MaxTurns},
		{"max_turns_ceiling", d.MaxTurnsCeiling},
	}
	for _, f := range positive {
		if f.value < 1 {
			p.add(at(path, f.name), ErrOutOfRange,
				"must be at least 1, got %d — a cap of zero refuses every "+
					"delegate call", f.value)
		}
	}
	if d.MinTokensPerTask < 0 {
		p.add(at(path, "min_tokens_per_task"), ErrOutOfRange,
			"must not be negative, got %d", d.MinTokensPerTask)
	}
	timeouts := []struct {
		name  string
		value float64
	}{
		{"task_timeout_seconds", d.TaskTimeoutSeconds},
		{"call_timeout_seconds", d.CallTimeoutSeconds},
	}
	for _, f := range timeouts {
		if f.value <= 0 {
			p.add(at(path, f.name), ErrOutOfRange,
				"must be positive, got %v — a non-positive timeout expires "+
					"before the work starts", f.value)
		}
	}
	if f := d.BudgetFraction; f <= 0 || f > 1 {
		p.add(at(path, "budget_fraction"), ErrOutOfRange,
			"must be a fraction in (0, 1], got %v — it is a SHARE of the "+
				"parent's remaining budget, not a token count", f)
	}
	// A ceiling below the cap it bounds is a contradiction rather than a
	// tighter bound: every template that asked for the ordinary cap would
	// be refused at load, naming a ceiling the operator cannot satisfy
	// without going below the default.
	if d.MaxTurnsCeiling > 0 && d.MaxTurns > 0 && d.MaxTurnsCeiling < d.MaxTurns {
		p.add(at(path, "max_turns_ceiling"), ErrConflict,
			"is %d, below max_turns (%d) — a template asking for the ordinary "+
				"round cap would then be refused at load",
			d.MaxTurnsCeiling, d.MaxTurns)
	}
	if d.CallTimeoutSeconds > 0 && d.TaskTimeoutSeconds > d.CallTimeoutSeconds {
		p.add(at(path, "task_timeout_seconds"), ErrConflict,
			"is %v, above call_timeout_seconds (%v) — one worker could then "+
				"outlive the call that is waiting for it",
			d.TaskTimeoutSeconds, d.CallTimeoutSeconds)
	}
	return p.err()
}

// String renders the bounds for a log line naming what a call ran under.
func (d Delegation) String() string {
	return fmt.Sprintf("parallel=%d tasks<=%d turns<=%d fraction=%v task=%vs call=%vs",
		d.MaxParallel, d.MaxTasksPerCall, d.MaxTurns, d.BudgetFraction,
		d.TaskTimeoutSeconds, d.CallTimeoutSeconds)
}
