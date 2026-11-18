package builtin_test

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/inbox"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/worklet"
)

// The key and the version every unknownWriter result holds: what a tool that
// reported a receipt beside an unknown outcome would print.
const (
	receiptKey     = "ZZZ-77"
	receiptVersion = 9901
)

// unknownWriter answers every tracker write `unknown` with a nil error, as a
// lost acknowledgement or an operation this node's ledger cannot vouch for
// (having published nothing) does. Each result still holds a receipt (a key, a version), so a tool that
// reports it anyway is caught.
type unknownWriter struct {
	*fakeTracker
	unvouched bool
	ops       []string
}

func (u *unknownWriter) answer(opID string) (worklet.WriteResult, error) {
	u.ops = append(u.ops, opID)
	return worklet.WriteResult{Key: receiptKey, Result: statelog.Result{
		Outcome: statelog.OutcomeUnknown, OpID: opID, Unvouched: u.unvouched,
		Version: receiptVersion,
	}}, nil
}

func (u *unknownWriter) UpdateTask(_ context.Context, opID, _, _ string, _ uint64,
	_ worklet.TaskPatch, _ worklet.ChangeKind, _ *worklet.Notify) (worklet.WriteResult, error) {
	return u.answer(opID)
}

func (u *unknownWriter) MergeDuplicates(_ context.Context, opID, _, _ string, _ bool,
	_ *worklet.Notify) (worklet.WriteResult, error) {
	return u.answer(opID)
}

func (u *unknownWriter) MoveTaskToProject(_ context.Context, opID, _, _ string,
	_ *worklet.Notify) (worklet.WriteResult, error) {
	return u.answer(opID)
}

func (u *unknownWriter) RemoveTask(_ context.Context, opID, _, _ string, _ bool,
	_ *worklet.Notify) (worklet.WriteResult, error) {
	return u.answer(opID)
}

func (u *unknownWriter) RestoreTask(_ context.Context, opID, _, _ string, _ bool,
	_ *worklet.Notify) (worklet.WriteResult, error) {
	return u.answer(opID)
}

func (u *unknownWriter) WriteView(_ context.Context, opID string,
	_ worklet.View, _ worklet.ViewPrior) (worklet.WriteResult, error) {
	return u.answer(opID)
}

func (u *unknownWriter) DeleteView(_ context.Context, opID, _ string,
	_ worklet.ViewPrior) (worklet.WriteResult, error) {
	return u.answer(opID)
}

// ViewPrior holds every view a case names, as a shared tab on ENG: a save
// naming an id no view holds is refused before it writes anything.
func (u *unknownWriter) ViewPrior(context.Context, string) (worklet.ViewPrior, error) {
	return worklet.ViewPrior{Held: true,
		Container: worklet.Container{Kind: worklet.ContainerProject, ID: "ENG"}}, nil
}

func (u *unknownWriter) WriteTypes(_ context.Context, opID string,
	_ []worklet.TaskType) (worklet.WriteResult, error) {
	return u.answer(opID)
}

func (u *unknownWriter) WriteFields(_ context.Context, opID string,
	_ []worklet.FieldDef) (worklet.WriteResult, error) {
	return u.answer(opID)
}

func (u *unknownWriter) WritePriorities(_ context.Context, opID, _ string, _ []string,
	_ *uint64, _ worklet.PersonAuthority) (worklet.WriteResult, error) {
	return u.answer(opID)
}

func (u *unknownWriter) WritePins(_ context.Context, opID, _ string,
	_ worklet.PinGesture, _ worklet.PersonAuthority) (worklet.WriteResult, error) {
	return u.answer(opID)
}

func (u *unknownWriter) MarkInbox(_ context.Context, opID, _ string, _ statelog.Position,
	_ inbox.Gesture[worklet.Reason], _ worklet.PersonAuthority) (worklet.WriteResult, error) {
	return u.answer(opID)
}

// PlaceTask answers both steps of a drag unknown.
func (u *unknownWriter) PlaceTask(_ context.Context, opID string, _ worklet.Place,
	_ *worklet.Notify) (worklet.PlaceResult, error) {
	lane, _ := u.answer(statelog.StepOpID(opID, "lane"))
	return worklet.PlaceResult{Lane: lane, Version: receiptVersion,
		Unplaced: statelog.ErrUnavailable}, nil
}

func (u *unknownWriter) WriteProject(_ context.Context, opID, _ string,
	_ worklet.ProjectEdit, _ worklet.ProjectAuthority) (worklet.WriteResult, error) {
	return u.answer(opID)
}

func (u *unknownWriter) WriteTags(_ context.Context, opID, _ string,
	_ worklet.TagEdit, _ worklet.TagAuthority) (worklet.WriteResult, error) {
	return u.answer(opID)
}

// deps is every write side of the work surface, each answering unknown.
func (u *unknownWriter) deps() builtin.WorkDeps {
	return builtin.WorkDeps{
		Reader:          u.fakeTracker,
		Writer:          func(builtin.Actor) builtin.WorkWriter { return u },
		Dependencies:    u.depends,
		Merges:          func(builtin.Actor) builtin.WorkMerger { return u },
		Moves:           func(builtin.Actor) builtin.WorkMover { return u },
		Placer:          func(builtin.Actor) builtin.WorkPlacer { return u },
		TrashWriter:     func(builtin.Actor) builtin.TrashWriter { return u },
		ViewWriter:      func(builtin.Actor) builtin.ViewWriter { return u },
		CatalogueWriter: func(builtin.Actor) builtin.CatalogueWriter { return u },
		PersonWriter:    func(builtin.Actor) builtin.PersonWriter { return u },
		ProjectWriter:   func(builtin.Actor) builtin.ProjectWriter { return u },
	}
}

// operatorSurface is the operator's catalogue over an unknownWriter.
func (u *unknownWriter) operatorSurface(t *testing.T) *tools.Registry {
	t.Helper()
	deps := u.deps()
	deps.Actor = operatorActor
	reg := tools.NewRegistry()
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Work: deps, Authorize: builtin.Decide(chartLeads),
	}) {
		if err := reg.Register(tool, tools.OriginBuiltin); err != nil {
			t.Fatalf("register %s: %v", tool.Name(), err)
		}
	}
	return reg
}

// receipt is what an answer about a write that may not exist must never carry:
// the fields of a success, and the fake result's key and version. It is looked
// for with the answer's operations taken out ([withoutOperations]).
var receipt = []string{`"version"`, `"comment_id"`, `"key"`, `"id"`, `"outcome"`,
	`"position"`, receiptKey, strconv.Itoa(receiptVersion), "NOT made"}

// unknownOperatorCalls is one call to every tracker write on the operator's
// surface. A fresh table per call, not a package variable: parallel tests hand
// these to tools, and a shared map races once a tool writes its arguments.
func unknownOperatorCalls() map[string]map[string]any {
	return map[string]map[string]any{
		builtin.UpdateWorkItemTool: {"item": "ENG-1", "status": "done"},
		builtin.CommentOnWorkTool:  {"asks": false, "item": "ENG-1", "body": "done, see the PR", "ask": "eng"},
		worklet.MergeWorkItemTool:  {"item": "ENG-2", "into": "ENG-1"},
		worklet.MoveWorkItemTool:   {"item": "ENG-1", "project": "OPS"},
		worklet.PlaceWorkItemTool: {"item": "ENG-1", "before": "ENG-2",
			"status": "in_progress", "if_match": 7},
		worklet.RemoveWorkItemTool:     {"item": "ENG-1"},
		worklet.RestoreWorkItemTool:    {"item": "ENG-1"},
		worklet.SaveWorkViewTool:       {"container": "project:ENG", "name": "Mine", "type": "list"},
		worklet.DeleteWorkViewTool:     {"id": "v-1"},
		worklet.SetPrioritiesTool:      {"items": []any{"ENG-1"}},
		worklet.SetPinsTool:            {"views": map[string]any{"add": []any{"v-1"}}},
		worklet.MarkInboxTool:          {"primary_reasons": []any{"mention"}},
		worklet.WriteWorkCatalogueTool: {"types": []any{map[string]any{"slug": "bug", "name": "Bug"}}},
		worklet.WriteProjectTool:       {"project": "ENG", "tags_add": []any{map[string]any{"slug": "regression"}}},
	}
}

// restatingTools are THE TWO THAT STATE A VALUE: they mint a fresh operation
// per call, so they take no op_id and name the operation they wrote under.
var restatingTools = map[string]bool{
	worklet.WriteWorkCatalogueTool: true, worklet.WriteProjectTool: true,
}

// EVERY TRACKER WRITE WHOSE OUTCOME IS UNKNOWN IS ANSWERED AS UNKNOWN, on
// every surface, and never with a receipt: a comment_id tells the asker the
// seat answered, a `version` goes back as `if_match` naming a version the item
// may never have had, and a view id names a view that may not exist.
func TestEveryTrackerWriteWhoseOutcomeIsUnknownIsAnsweredAsUnknown(t *testing.T) {
	t.Parallel()
	operatorCalls := unknownOperatorCalls()
	for _, unvouched := range []bool{false, true} {
		for name, args := range operatorCalls {
			u := &unknownWriter{fakeTracker: newFakeTracker(), unvouched: unvouched}
			reg := u.operatorSurface(t)
			got := callNoTurn(t, reg, name, args)
			if len(u.ops) == 0 {
				t.Errorf("%s (unvouched %v) wrote nothing, so this case shows "+
					"nothing: %s", name, unvouched, got.Output)
				continue
			}
			checkUnknownAnswer(t, name, unvouched, got, u.ops)
			if restatingTools[name] {
				if !strings.Contains(got.Output, u.ops[0]) ||
					!strings.Contains(got.Output, "harmless") {
					t.Errorf("%s does not name operation %s, or say a repeat is "+
						"harmless: %s", name, u.ops[0], got.Output)
				}
				continue
			}
			// THE op_id IT NAMES IS THE ONE THAT FINISHES IT: the same call
			// brought back under it derives the same ids.
			op := answeredOp(t, got)
			again := map[string]any{"op_id": op}
			for k, v := range args {
				again[k] = v
			}
			callNoTurn(t, reg, name, again)
			if len(u.ops) != 2 || u.ops[1] != u.ops[0] ||
				!strings.HasPrefix(u.ops[0], op+".") {
				t.Errorf("%s (unvouched %v) named op_id %s, and brought back it "+
					"wrote under %q — not the same operation", name, unvouched,
					op, u.ops)
			}
		}
	}

	// AND A SEAT, whose repeat is the same operation because its ids derive
	// from its turn.
	seatCalls := map[string]map[string]any{
		builtin.UpdateWorkItemTool: operatorCalls[builtin.UpdateWorkItemTool],
		builtin.CommentOnWorkTool:  operatorCalls[builtin.CommentOnWorkTool],
		worklet.MergeWorkItemTool:  operatorCalls[worklet.MergeWorkItemTool],
		worklet.MoveWorkItemTool:   operatorCalls[worklet.MoveWorkItemTool],
	}
	for _, unvouched := range []bool{false, true} {
		for name, args := range seatCalls {
			u := &unknownWriter{fakeTracker: newFakeTracker(), unvouched: unvouched}
			deps := u.deps()
			// THE ROSTER the seat's ask names a colleague agent seat on: an
			// agent's question to a person would need a decision (ADR-0100).
			deps.Seats = askable
			reg := tools.NewRegistry()
			if _, err := builtin.Register(reg, gated(builtin.Deps{
				Work: deps,
			})); err != nil {
				t.Fatalf("register: %v", err)
			}
			got := callWork(t, reg, name, args)
			if len(u.ops) == 0 {
				t.Errorf("%s (unvouched %v) wrote nothing: %s", name, unvouched, got.Output)
				continue
			}
			checkUnknownAnswer(t, name, unvouched, got, u.ops)
			if !strings.Contains(got.Output, u.ops[0]) {
				t.Errorf("%s does not name operation %s: %s", name, u.ops[0], got.Output)
			}
			if !strings.Contains(got.Output, "exactly the same arguments") {
				t.Errorf("%s (unvouched %v) does not say the same call is the "+
					"same operation: %s", name, unvouched, got.Output)
			}
			if unvouched && !strings.Contains(got.Output, "that is safe") {
				t.Errorf("%s's unvouched unknown does not say the repeat is safe "+
					"and answers the same way: %s", name, got.Output)
			}
		}
	}
}

// AN OPERATION WHOSE ID SPELLS THE RECEIPT IS NOT A RECEIPT, and a receipt
// printed right beside it still is.
//
// An operation id is random hex ([statelog.NewOpID]), so a minted id can spell
// the fake's version. This pins that case on every run, for every tool an
// operator can bring an operation back to, and three doctored answers the
// check must still refuse: the version printed after the id, the version GLUED
// to the id's last character (only removal by exact value reads it; a pattern
// would eat it with the id), and an operation the writer was never handed.
func TestAnOperationWhoseIDSpellsTheReceiptIsNotReadAsOne(t *testing.T) {
	t.Parallel()
	// spelled is an operation whose last group spells the version.
	const spelled = "01a102c5-54a2-7eb9-a00e-b9901477cd19"
	version := strconv.Itoa(receiptVersion)
	if !strings.Contains(spelled, version) {
		t.Fatalf("%s does not spell the fake's version %s, so this case shows "+
			"nothing", spelled, version)
	}
	for _, unvouched := range []bool{false, true} {
		for name, args := range unknownOperatorCalls() {
			if restatingTools[name] {
				continue // they take no op_id, so their operation is never chosen
			}
			u := &unknownWriter{fakeTracker: newFakeTracker(), unvouched: unvouched}
			reg := u.operatorSurface(t)
			// THE SAME CALL, brought back under an operation named for it
			// whose uuid spells the version.
			op := spelled + answeredOp(t, callNoTurn(t, reg, name, args))[len(spelled):]
			again := map[string]any{"op_id": op}
			for k, v := range args {
				again[k] = v
			}
			got := callNoTurn(t, reg, name, again)
			if !strings.Contains(got.Output, op) {
				t.Errorf("%s (unvouched %v) does not name operation %s, which it "+
					"was brought back under, so this case shows nothing: %s",
					name, unvouched, op, got.Output)
				continue
			}
			checkUnknownAnswer(t, name, unvouched, got, u.ops)

			for _, beside := range []string{" at version " + version, version} {
				leaked := got
				leaked.Output = strings.Replace(got.Output, op, op+beside, 1)
				if !slices.Contains(unknownAnswerProblems(leaked, unvouched, u.ops),
					"carries "+version+" beside an unknown outcome") {
					t.Errorf("%s (unvouched %v): version %s printed as %q after "+
						"operation %s is not read as a receipt: %s", name, unvouched,
						version, beside, op, leaked.Output)
				}
			}
			stranger := got
			stranger.Output = strings.Replace(got.Output, op, strangerOp, 1)
			if !slices.Contains(unknownAnswerProblems(stranger, unvouched, u.ops),
				"names "+strangerUUID+", which it was not made under") {
				t.Errorf("%s (unvouched %v): an answer naming operation %s, which the "+
					"writer was never handed, is not refused: %s", name, unvouched,
					strangerOp, stranger.Output)
			}
		}
	}
}

// checkUnknownAnswer is what every one of those answers has in common, where
// ops are the operations the writer was handed for it.
func checkUnknownAnswer(t *testing.T, name string, unvouched bool, got tools.Result,
	ops []string) {

	t.Helper()
	for _, problem := range unknownAnswerProblems(got, unvouched, ops) {
		t.Errorf("%s (unvouched %v) %s: %s", name, unvouched, problem, got.Output)
	}
}

// unknownAnswerProblems is everything wrong with got as the answer to a write
// whose outcome is unknown, made under ops — none, for a right one. A list
// rather than a report, so a case can hold the check itself to a doctored
// answer.
func unknownAnswerProblems(got tools.Result, unvouched bool, ops []string) []string {
	if !got.Failed {
		return []string{"answered an unknown outcome as a receipt"}
	}
	said := withoutOperations(got.Output, ops)
	var problems []string
	// AND THE CAUSE SAYS SO: a surface answering in status codes reads it, not
	// the sentence, or renders an unknown as a refusal ("NOT made").
	var outcome *builtin.UnknownOutcome
	if !errors.As(got.Cause, &outcome) || !errors.Is(got.Cause, builtin.ErrOutcomeUnknown) {
		problems = append(problems, fmt.Sprintf("carries cause %v, want an UnknownOutcome",
			got.Cause))
	} else if outcome.Unvouched != unvouched || outcome.OpID == "" {
		problems = append(problems, fmt.Sprintf("names %+v as its unknown outcome", outcome))
	}
	for _, leak := range receipt {
		if strings.Contains(said, leak) {
			problems = append(problems, "carries "+leak+" beside an unknown outcome")
		}
	}
	if !strings.Contains(said, "is unknown") ||
		!strings.Contains(said, "do not report it as done") {
		problems = append(problems, "never says the outcome is unknown")
	}
	if unvouched != strings.Contains(said, "this node cannot tell") {
		problems = append(problems, fmt.Sprintf("says this node cannot tell %v",
			!unvouched))
	}
	// AN ID LEFT OVER is an operation the writer was never handed: a pointer
	// the reader cannot use, and the one thing that could hide a receipt, since
	// only this call's ids are taken out before it is looked for.
	for _, id := range uuidPattern.FindAllString(said, -1) {
		problems = append(problems, "names "+id+", which it was not made under")
	}
	return problems
}

// uuidPattern is the head of an operation id, in the form [statelog.NewOpID]
// writes it.
var uuidPattern = regexp.MustCompile(
	`[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`)

// strangerOp is an operation in the engine's grammar that no call in these
// tests is ever made under, and strangerUUID its head — what the check names.
const (
	strangerUUID = "01a102c5-0000-7000-8000-000000000000"
	strangerOp   = strangerUUID + ".place_work_item.stranger"
)

// withoutOperations is an answer with every operation it may name taken out:
// each id the writer was handed, and each gesture one of them is a step of —
// the call's own operation, which is what an operator is told to bring back.
//
// An id the answer must carry is random hex ([statelog.NewOpID]) that can
// spell the version; nothing a write reports is inside an id minted before
// it, so taking them out hides no leak, and an id the writer was NOT handed
// stays and is refused ([unknownAnswerProblems]).
//
// BY VALUE, NEVER BY PATTERN, so a version printed right beside an id is still
// read; and only ids the engine's grammar minted ([statelog.OpMintedAt]), since
// a test literal like "op" has no random part and would take words with it.
func withoutOperations(output string, ops []string) string {
	var named []string
	for _, op := range ops {
		if _, minted := statelog.OpMintedAt(op); !minted {
			continue
		}
		// The step's own id, then each gesture above it, down to the bare
		// uuid — which holds no dot, so the walk ends there.
		for id := op; ; {
			named = append(named, id)
			step := strings.LastIndex(id, ".")
			if step < 0 {
				break
			}
			id = id[:step]
		}
	}
	// LONGEST FIRST, or a gesture taken out of a step's id would leave the
	// step's tail behind.
	slices.SortFunc(named, func(a, b string) int { return cmp.Compare(len(b), len(a)) })
	for _, id := range named {
		output = strings.ReplaceAll(output, id, "<operation>")
	}
	return output
}

// AN UPDATE WHOSE CHANGE IS UNKNOWN WRITES NONE OF ITS DEPENDENCIES. They wait
// for it: the same call made again answers the change first and writes them
// after, once — and a dependency written beside a change nobody can vouch for
// is half a call reported as neither half.
func TestAnUnknownUpdateStopsBeforeItsDependencies(t *testing.T) {
	t.Parallel()
	u := &unknownWriter{fakeTracker: newFakeTracker()}
	got := callWork(t, workRegistry(t, u.deps()), builtin.UpdateWorkItemTool,
		map[string]any{
			"item": "ENG-1", "status": "done",
			"waiting_on": map[string]any{"add": []any{"ENG-2"}},
		})
	if len(u.depended) != 0 {
		t.Errorf("the dependencies were written behind an unknown change: %+v",
			u.depended)
	}
	if !got.Failed || !strings.Contains(got.Output, "were NOT written") {
		t.Errorf("the answer does not say the dependencies were not written: %s",
			got.Output)
	}
}

// AN UPDATE THAT ONLY CHANGES DEPENDENCIES REPORTS THE ITEM'S OWN VERSION. The
// dependency sequence's last commit is usually a blocker's mirror, whose
// version would be refused as stale as `if_match` on this item.
func TestADependencyOnlyUpdateReportsTheItemsOwnVersion(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	trk.dependAnswer = &worklet.DependencyResult{
		WriteResult: worklet.WriteResult{Result: statelog.Result{
			Outcome:  statelog.OutcomeApplied,
			Position: statelog.Position{Stream: "S", Generation: 1, Seq: 44},
			Version:  44,
		}},
	}
	// THE DEPENDENCY RESULT'S VERSION IS THIS ITEM'S OWN — the tracker
	// folds every commit into it — and 44 is the blocker's mirror's.
	trk.dependAnswer.Version = 42
	got := callWork(t, workRegistry(t, builtin.WorkDeps{
		Reader: trk, Writer: trk.as, Dependencies: trk.depends,
	}), builtin.UpdateWorkItemTool, map[string]any{
		"item": "ENG-1", "waiting_on": map[string]any{"add": []any{"ENG-2"}},
	})
	if got.Failed {
		t.Fatalf("the update failed: %s", got.Output)
	}
	if v, _ := answerOf(t, got)["version"].(float64); v != 42 {
		t.Errorf("the update answered version %v, want the item's own 42 — 44 "+
			"is the blocker's mirror", answerOf(t, got)["version"])
	}
	// AND THE CALL'S OWN POSITION AND OUTCOME, which with no patch are the
	// dependency step's alone.
	if answer := answerOf(t, got); answer["position"] != "S@1:44" ||
		answer["outcome"] != "applied" {
		t.Errorf("the update answered %v at %v, want applied at S@1:44",
			answer["outcome"], answer["position"])
	}
}

// AN UPDATE THAT CHANGES THE ITEM AND ITS DEPENDENCIES REPORTS THE ITEM'S
// NEWEST VERSION, AND ITS OWN CALL'S LATEST POSITION. The patch lands first
// (version 12, at S@1:12), the dependency's `waiting_on` commit on the same
// item after it (88), a blocker's mirror last (90). Reporting the patch's 12
// would be stale as `if_match`, and settling at S@1:12 would read the call's
// own dependencies back missing.
func TestAnUpdateThatAlsoChangesDependenciesReportsTheItemsNewestVersion(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		task        int64
		wantVersion float64
	}{
		"the dependency wrote this item":   {task: 88, wantVersion: 88},
		"the dependency wrote other items": {task: 0, wantVersion: 12},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			trk := newFakeTracker()
			trk.dependAnswer = &worklet.DependencyResult{
				WriteResult: worklet.WriteResult{Result: statelog.Result{
					Outcome:  statelog.OutcomePending,
					Position: statelog.Position{Stream: "S", Generation: 1, Seq: 90},
					Version:  90,
				}},
			}
			// This item's own version after the dependency: zero where it
			// landed nothing on this item's subject.
			trk.dependAnswer.Version = tc.task
			got := callWork(t, workRegistry(t, builtin.WorkDeps{
				Reader: trk, Writer: trk.as, Dependencies: trk.depends,
			}), builtin.UpdateWorkItemTool, map[string]any{
				"item": "ENG-1", "status": "done",
				"waiting_on": map[string]any{"add": []any{"ENG-2"}},
			})
			if got.Failed || len(trk.patched) != 1 || len(trk.depended) != 1 {
				t.Fatalf("the update did not make both writes (%d patches, %d "+
					"dependency changes): %s", len(trk.patched), len(trk.depended),
					got.Output)
			}
			answer := answerOf(t, got)
			if v, _ := answer["version"].(float64); v != tc.wantVersion {
				t.Errorf("the update answered version %v, want the item's newest "+
					"%v", answer["version"], tc.wantVersion)
			}
			// THE LATER POSITION, AND ITS OUTCOME: the dependency's last
			// commit is what this call is durable at, and it is not
			// applied here yet.
			if answer["position"] != "S@1:90" || answer["outcome"] != "pending" {
				t.Errorf("the update answered %v at %v, want pending at S@1:90 — "+
					"its own call's last write", answer["outcome"], answer["position"])
			}
		})
	}
}

// A GESTURE THAT STOPPED AT A STEP WHOSE OUTCOME IS UNKNOWN IS AN UNKNOWN TO A
// CALLER ANSWERING IN STATUS CODES, and never a refusal.
//
// A cause of the writer's error alone falls to the HTTP write surface's
// default, a refusal saying nothing was written, though earlier steps landed
// and the stopped one may have. The error stays reachable inside the cause,
// so [errors.Is] still answers whether this node can vouch for the step.
func TestAStoppedGestureCarriesAnUnknownOutcome(t *testing.T) {
	t.Parallel()
	for _, unvouched := range []bool{false, true} {
		sentinel := worklet.ErrStepUnresolved
		if unvouched {
			sentinel = worklet.ErrStepUnvouched
		}
		for name, run := range map[string]func(*testing.T) tools.Result{
			// A WALKING WRITE, stopped inside the tracker's own sequence.
			"a stopped update": func(t *testing.T) tools.Result {
				trk := newFakeTracker()
				trk.writeErr = fmt.Errorf("stopped: %w", sentinel)
				return callNoTurn(t, retrySurface(t, trk, nil),
					builtin.UpdateWorkItemTool,
					map[string]any{"item": "ENG-1", "priority": "urgent"})
			},
			// AND ONE STOPPED AT ITS OWN LABEL DECLARATION, before its
			// write — the same unknown, met one step earlier.
			"a stopped label declaration": func(t *testing.T) tools.Result {
				trk := newFakeTracker()
				trk.ensureErr = declarationStopped(unvouched)
				return callNoTurn(t, labelSurface(t, trk, true),
					builtin.CreateWorkItemTool, map[string]any{
						"title": "the regression", "project": "ENG",
						"labels": []any{"regression"}, "labels_create_missing": true,
					})
			},
		} {
			t.Run(fmt.Sprintf("%s/unvouched=%v", name, unvouched), func(t *testing.T) {
				t.Parallel()
				got := run(t)
				var outcome *builtin.UnknownOutcome
				switch {
				case !got.Failed:
					t.Fatalf("the stopped gesture answered as a receipt: %s", got.Output)
				case !errors.As(got.Cause, &outcome):
					t.Fatalf("the stopped gesture's cause is %v, want an "+
						"UnknownOutcome", got.Cause)
				case outcome.Unvouched != unvouched:
					t.Errorf("the cause says unvouched %v, want %v", outcome.Unvouched,
						unvouched)
				case outcome.OpID == "":
					t.Errorf("the cause names no operation: %+v", outcome)
				// THE SAME OPERATION IS OFFERED BACK where it finishes the
				// gesture here; unvouched, it would stop at the same step on
				// this node, so only the cause names it.
				case !unvouched && !strings.Contains(got.Output, outcome.OpID):
					t.Errorf("the cause names operation %q, which the answer does "+
						"not offer back: %s", outcome.OpID, got.Output)
				case !errors.Is(got.Cause, sentinel):
					t.Errorf("the writer's own error is not reachable in the cause %v",
						got.Cause)
				}
			})
		}
	}
}
