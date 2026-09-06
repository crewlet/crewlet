package subagent

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/config"
)

// The task graph one `delegate` call runs.
//
// # Why a graph and not a list
//
// A flat batch could only express work that is entirely independent. The
// shape a turn actually wants is one step further: gather from three places,
// then synthesise the three answers. With a flat batch the parent had to make
// that two calls and carry the first call's results into the second's prompts
// by hand — which meant the parent's own model had to read three answers,
// re-type them into a new prompt, and pay for both.
//
// `after` closes that. A task naming another waits for it, and receives its
// SUBMITTED FIELDS in its own first user message. The parent writes the shape
// of the work once and reads one report.
//
// # Waves, not a scheduler
//
// Execution is by topological wave: every task whose dependencies have
// settled runs, bounded by the call's parallelism, then the next wave. It is
// not the tightest possible schedule — a long task in wave one holds back a
// short one in wave two that did not depend on it — and that is a deliberate
// trade. The tight version needs a per-task readiness queue and a completion
// fan-in, and its failure modes (a task started while its dependency was
// mid-write, a deadlock when a wave's last worker is queued behind a
// dependent) are exactly the ones nobody would find in review. A wave
// boundary is a barrier, and a barrier is checkable.
//
// # Determinism is a property, not an accident
//
// The same graph run twice, with the same deadline, must produce the same
// statuses. Two rules buy that: a task whose dependency did not succeed is
// classified `skipped_dependency_failed` BEFORE the deadline is consulted, so
// a call that ran out of time reports the broken chain rather than a
// scattering of timeouts; and results are returned in INPUT order, whatever
// order the waves finished in.

// Task is one node of the graph.
type Task struct {
	// ID is the parent's own name for this task. It is how the parent
	// pairs an answer with the question — by name rather than by
	// position, because a graph reorders and an index does not survive
	// that — and it is what a dependent names in After.
	ID string

	// Worker names a template from the seat's visible set. Mutually
	// exclusive with SystemPrompt: one of the two says who is doing this.
	Worker string

	// SystemPrompt is an ad-hoc worker's persona, for work that does not
	// justify a template. The runtime preamble is appended to it either
	// way.
	SystemPrompt string

	// Prompt is the concrete task, seeded as the worker's first user
	// message. Always the parent's, never the template's: the template
	// holds what does not change and this is what does.
	Prompt string

	// Tools narrows the template's allowlist for this task. Nil takes the
	// template's; a non-nil value REPLACES it, so a parent can hand a
	// research worker one repository's tools without editing config.
	Tools []string

	// Model overrides the template's provider key for this task.
	Model string

	// MaxTurns overrides the template's round cap. Clamped to the
	// runtime's, never refused.
	MaxTurns int

	// Output overrides the template's answer schema.
	Output map[string]any

	// After names tasks that must SUCCEED before this one runs. Their
	// submitted fields are injected into this task's first user message.
	After []string
}

// Request is one delegate call: a graph of tasks.
//
// One shape rather than the single-plus-batch pair it replaces. Two shapes
// meant a mutual-exclusion rule the schema could not express, a hybrid
// payload whenever a serialiser flattened the oneOf, and two code paths that
// had to classify failures identically and did not.
type Request struct{ Tasks []Task }

// PlanError is a request the engine refused to start.
//
// Its own type because the tool answers it to the MODEL, which can fix it:
// every message here names the task and says what to write instead.
type PlanError struct{ Reason string }

func (e *PlanError) Error() string { return e.Reason }

// resolved is one validated task plus everything the template contributed.
type resolved struct {
	Task
	systemPrompt string
	tools        []string
	model        string
	maxTurns     int
	output       map[string]any
	// order is the task's position in the request, which is the order
	// results come back in.
	order int
	// wave is the topological depth: 0 for a task with no dependencies.
	wave int
}

// plan validates a request against the seat's workers and the runtime bounds,
// and returns the tasks in dependency order.
//
// EVERYTHING UP FRONT. A graph that cannot run must not run half of itself:
// starting three tasks and then discovering the fourth names a cycle has
// already spent three workers' tokens on work whose consumer will never
// execute, and the parent gets a report it cannot act on.
func plan(req Request, workers map[string]config.Worker, limits Limits) ([]resolved, error) {
	switch {
	case len(req.Tasks) == 0:
		return nil, &PlanError{Reason: "delegate: `tasks` must contain at least one task"}
	case limits.MaxTasksPerCall > 0 && len(req.Tasks) > limits.MaxTasksPerCall:
		return nil, &PlanError{Reason: fmt.Sprintf(
			"delegate: %d tasks, but at most %d may run in one call — split the "+
				"work across turns, or delegate the parts you need now",
			len(req.Tasks), limits.MaxTasksPerCall)}
	}

	byID := make(map[string]int, len(req.Tasks))
	out := make([]resolved, 0, len(req.Tasks))
	for i, t := range req.Tasks {
		r, err := resolveTask(i, t, workers, limits)
		if err != nil {
			return nil, err
		}
		if _, dup := byID[r.ID]; dup {
			return nil, &PlanError{Reason: fmt.Sprintf(
				"delegate: two tasks share the id %q — ids are how results are "+
					"paired with tasks, so they must be unique within a call", r.ID)}
		}
		byID[r.ID] = i
		out = append(out, r)
	}

	for _, r := range out {
		for _, dep := range r.After {
			switch {
			case dep == r.ID:
				return nil, &PlanError{Reason: fmt.Sprintf(
					"delegate: task %q lists itself in `after`", r.ID)}
			case !hasID(byID, dep):
				return nil, &PlanError{Reason: fmt.Sprintf(
					"delegate: task %q waits on %q, which is not a task in this "+
						"call — `after` names ids from this same `tasks` list (%s)",
					r.ID, dep, strings.Join(idsOf(out), ", "))}
			}
		}
	}

	if err := assignWaves(out); err != nil {
		return nil, err
	}
	// Dependency order, then input order within a wave, so the semaphore
	// admits tasks in the order the parent wrote them.
	slices.SortStableFunc(out, func(a, b resolved) int {
		if a.wave != b.wave {
			return a.wave - b.wave
		}
		return a.order - b.order
	})
	return out, nil
}

func hasID(byID map[string]int, id string) bool { _, ok := byID[id]; return ok }

func idsOf(tasks []resolved) []string {
	out := make([]string, 0, len(tasks))
	for _, t := range tasks {
		out = append(out, t.ID)
	}
	return out
}

// resolveTask merges a task with its template and applies the runtime bounds.
func resolveTask(i int, t Task, workers map[string]config.Worker, limits Limits) (resolved, error) {
	r := resolved{Task: t, order: i}
	r.ID = strings.TrimSpace(t.ID)
	if r.ID == "" {
		return r, &PlanError{Reason: fmt.Sprintf(
			"delegate: tasks[%d] has no `id` — every task needs one, because "+
				"results are returned by id rather than by position", i)}
	}
	if strings.TrimSpace(t.Prompt) == "" {
		return r, &PlanError{Reason: fmt.Sprintf(
			"delegate: task %q has no `prompt` — that is the work itself", r.ID)}
	}

	named := strings.TrimSpace(t.Worker)
	ad := strings.TrimSpace(t.SystemPrompt)
	switch {
	case named != "" && ad != "":
		return r, &PlanError{Reason: fmt.Sprintf(
			"delegate: task %q sets both `worker` and `system_prompt` — name a "+
				"template or write a prompt, not both", r.ID)}
	case named == "" && ad == "":
		return r, &PlanError{Reason: fmt.Sprintf(
			"delegate: task %q sets neither `worker` nor `system_prompt` — one "+
				"of them says who is doing this. Available workers: %s",
			r.ID, availableWorkers(workers))}
	case named != "":
		w, ok := workers[named]
		if !ok {
			return r, &PlanError{Reason: fmt.Sprintf(
				"delegate: task %q names worker %q, which this seat does not "+
					"have. Available workers: %s", r.ID, named, availableWorkers(workers))}
		}
		r.systemPrompt = w.SystemPrompt
		r.tools = slices.Clone(w.Tools)
		r.model = w.Model
		r.maxTurns = w.MaxTurns
		r.output = w.Output
	default:
		r.systemPrompt = ad
	}

	// Per-task overrides. A nil Tools takes the template's; a non-nil one
	// REPLACES it, including an explicitly empty list — "this task needs
	// no tools" is a real thing to say and the only way to say it.
	if t.Tools != nil {
		r.tools = slices.Clone(t.Tools)
	}
	if t.Model != "" {
		r.model = t.Model
	}
	if t.MaxTurns > 0 {
		r.maxTurns = t.MaxTurns
	}
	if len(t.Output) > 0 {
		r.output = t.Output
	}
	r.maxTurns = clampTurns(r.maxTurns, limits.MaxTurns)
	return r, nil
}

func availableWorkers(workers map[string]config.Worker) string {
	if len(workers) == 0 {
		return "(none configured — use `system_prompt` to write one inline)"
	}
	return strings.Join(config.WorkerNames(workers), ", ")
}

// assignWaves computes each task's topological depth and refuses a cycle.
//
// Kahn's algorithm, and the leftovers ARE the cycle: when no task has an
// unsatisfied-dependency count of zero and tasks remain, every one of those
// remaining is on or downstream of a cycle. Naming them is what makes the
// refusal actionable — "there is a cycle" sends the model to re-read a graph
// it just wrote, and it will write the same one again.
func assignWaves(tasks []resolved) error {
	index := make(map[string]int, len(tasks))
	for i, t := range tasks {
		index[t.ID] = i
	}
	remaining := make(map[string]int, len(tasks))
	for _, t := range tasks {
		remaining[t.ID] = len(t.After)
	}
	dependents := make(map[string][]string, len(tasks))
	for _, t := range tasks {
		for _, dep := range t.After {
			dependents[dep] = append(dependents[dep], t.ID)
		}
	}

	// SETTLED IS ITS OWN SET, not the absence of a key in `remaining`. A
	// Go map returns the zero value for a key that was deleted, so a
	// settled task reads as "no unmet dependencies" on the next pass and
	// is assigned a second, higher wave — which silently moved every
	// wave-0 task into the last wave and left dependents reading results
	// their producers had not written yet.
	settled := make(map[string]bool, len(tasks))
	for wave := 0; len(settled) < len(tasks); wave++ {
		var ready []string
		for _, t := range tasks {
			if !settled[t.ID] && remaining[t.ID] == 0 {
				ready = append(ready, t.ID)
			}
		}
		if len(ready) == 0 {
			var stuck []string
			for _, t := range tasks {
				if !settled[t.ID] {
					stuck = append(stuck, t.ID)
				}
			}
			return &PlanError{Reason: fmt.Sprintf(
				"delegate: these tasks wait on each other and none can start: %s — "+
					"`after` must describe a one-way flow of results",
				strings.Join(stuck, ", "))}
		}
		for _, id := range ready {
			tasks[index[id]].wave = wave
			settled[id] = true
			for _, dep := range dependents[id] {
				remaining[dep]--
			}
		}
	}
	return nil
}

// dependencyBudget caps what one task's injected dependency results may cost.
//
// 16 KB per dependency, because a submission is the parent's own declared
// shape and truncating it silently would hand a dependent half a JSON
// document — worse than none, since a model reads the fragment as the whole
// answer. Past the cap the answer is elided WITH the elision marked, so the
// worker can see something was cut and say so rather than reasoning over a
// gap it cannot detect.
const dependencyBudget = 16 << 10

// withDependencies prefixes a task's prompt with the answers it waited for.
//
// In the USER message rather than the system prompt: the system prompt is the
// worker's persona, which a template shares across every task that names it,
// and injecting per-task data there would give two tasks of one template two
// different prefixes — costing the provider's prompt cache the whole prefix
// on the second.
func withDependencies(prompt string, deps []Result) string {
	if len(deps) == 0 {
		return prompt
	}
	var b strings.Builder
	b.WriteString("## Results you were given\n\n")
	b.WriteString("These are the answers from the tasks this one waited for. " +
		"They are the input to your work.\n")
	for _, d := range deps {
		b.WriteString("\n### ")
		b.WriteString(d.ID)
		if d.Worker != "" {
			b.WriteString(" (worker: " + d.Worker + ")")
		}
		b.WriteString("\n")
		b.WriteString(ledger.Elide(d.Answer(), dependencyBudget))
		b.WriteString("\n")
	}
	b.WriteString("\n---\n\n")
	b.WriteString(prompt)
	return b.String()
}

// runner is what the wave executor calls to run one task. A field rather
// than a direct call so the graph's own behaviour — waves, skipping,
// ordering, determinism — is testable without a provider.
type runner func(ctx context.Context, r resolved, deps []Result) Result

// runGraph executes the planned tasks wave by wave and returns their results
// in INPUT order.
//
// Order matters more than it looks: the parent's model wrote the tasks as a
// list and reads the answers as one, so results ordered by completion would
// silently re-pair every answer with the wrong question.
func runGraph(ctx context.Context, tasks []resolved, maxParallel int, run runner) []Result {
	results := make([]Result, len(tasks))
	byID := make(map[string]*Result, len(tasks))
	for i := range tasks {
		results[i] = Result{ID: tasks[i].ID, Worker: tasks[i].Worker}
		byID[tasks[i].ID] = &results[i]
	}

	sem := make(chan struct{}, maxParallel)
	for _, wave := range wavesOf(tasks) {
		var wg sync.WaitGroup
		for _, i := range wave {
			task := tasks[i]

			// THE DEPENDENCY CHECK COMES FIRST, before the deadline is
			// read at all. A task whose input never arrived is skipped
			// whether or not the call also ran out of time, so the same
			// graph under the same deadline reports the same statuses —
			// and the parent is sent to fix the broken chain rather than
			// to retry a timeout that was never the problem.
			deps, ok := dependenciesOf(task, byID)
			if !ok {
				results[i].Status = StatusSkipped
				results[i].Error = skipReason(task, byID)
				continue
			}

			wg.Add(1)
			go func() {
				defer wg.Done()
				select {
				case sem <- struct{}{}:
					defer func() { <-sem }()
				case <-ctx.Done():
					results[i] = neverStarted(ctx, task)
					return
				}
				if ctx.Err() != nil {
					// The slot came free only BECAUSE the deadline killed
					// the task ahead, so both select cases were ready and
					// the choice between them was a coin flip. Re-checking
					// after the acquire is what makes "never started" a
					// fact rather than a scheduling accident.
					results[i] = neverStarted(ctx, task)
					return
				}
				results[i] = run(ctx, task, deps)
				results[i].ID, results[i].Worker = task.ID, task.Worker
			}()
		}

		// A BARRIER PER WAVE. The next wave's tasks read this one's
		// results, so starting any of them before every writer here has
		// returned is a data race on the slice — and, worse, a dependent
		// reading a zero Result would see status "" and treat an unfinished
		// task as one that answered nothing.
		wg.Wait()
	}

	// BACK INTO INPUT ORDER. The slice above is in wave order, because
	// that is what the barrier needs; the parent wrote a list and reads
	// the answers as one, so returning wave order would silently re-pair
	// every answer with a different question.
	ordered := make([]Result, len(results))
	for i, t := range tasks {
		ordered[t.order] = results[i]
	}
	return ordered
}

// wavesOf groups task indices by their topological depth, preserving input
// order within a wave.
func wavesOf(tasks []resolved) [][]int {
	var waves [][]int
	for i, t := range tasks {
		for len(waves) <= t.wave {
			waves = append(waves, nil)
		}
		waves[t.wave] = append(waves[t.wave], i)
	}
	return waves
}

// dependenciesOf collects a task's inputs, reporting whether every one
// succeeded.
func dependenciesOf(t resolved, byID map[string]*Result) ([]Result, bool) {
	if len(t.After) == 0 {
		return nil, true
	}
	deps := make([]Result, 0, len(t.After))
	for _, id := range t.After {
		dep, ok := byID[id]
		if !ok || !dep.Status.Succeeded() {
			return nil, false
		}
		deps = append(deps, *dep)
	}
	return deps, true
}

// skipReason names which dependency broke the chain and how.
//
// Named rather than "a dependency failed", because in a graph of eight the
// parent's next move is entirely determined by WHICH one: a timed-out gather
// is a retry, a `no_result` is a prompt the worker could not satisfy, and a
// skip is somebody else's problem one level up.
func skipReason(t resolved, byID map[string]*Result) string {
	var broken []string
	for _, id := range t.After {
		dep, ok := byID[id]
		switch {
		case !ok:
			broken = append(broken, id+" (missing)")
		case !dep.Status.Succeeded():
			broken = append(broken, id+" ("+string(dep.Status)+")")
		}
	}
	return "did not run: " + strings.Join(broken, ", ")
}

// neverStarted is the record for a task the call's deadline reached before it
// got a worker slot.
func neverStarted(ctx context.Context, t resolved) Result {
	kind, reason := stopReason(ctx)
	if kind == "" {
		// The select lost a race it should not be able to lose: Done fired
		// with no cause. Reported rather than left as a zero Result, which
		// would read as a task that ran and said nothing.
		reason = "the call stopped before this task started"
	}
	return Result{
		ID: t.ID, Worker: t.Worker,
		Status: StatusNeverStarted,
		Error:  "never started: " + reason,
	}
}

// RefusedError is a call that never started because its budget slice could
// not give every task a workable share.
//
// One error for the CALL rather than one synthetic failure per task, because
// the refusal is a property of the call. N copies of one sentence reads to a
// model as N independent failures, and the obvious reaction to that is to
// retry them individually — exactly what the floor exists to prevent.
type RefusedError struct {
	// Slice is the total the call would have shared, MinPerTask the floor
	// each task needed, and Tasks how many were asked for.
	Slice      int
	MinPerTask int
	Tasks      int
}

func (e *RefusedError) Error() string {
	return fmt.Sprintf(
		"delegate: refused: the budget slice is %d tokens, which cannot give "+
			"%d tasks the %d each they need to finish — delegate fewer tasks "+
			"or wait for budget",
		e.Slice, e.Tasks, e.MinPerTask)
}
