package builtin_test

import (
	"cmp"
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// The key and the version every unknownWriter result holds: what a tool that
// reported a receipt beside an unknown outcome would print.
const (
	receiptKey     = "ZZZ-77"
	receiptVersion = 9901
)

// unknownWriter answers every tracker write `unknown` with a nil error — what a
// write whose acknowledgement was lost returns, and what one whose operation
// this node's ledger cannot vouch for returns WITHOUT having published
// anything. The rest of each result is a receipt's (a key, a version), so a
// tool that reports it anyway is caught reporting it.
type unknownWriter struct {
	*fakeTracker
	unvouched bool
	ops       []string
}

func (u *unknownWriter) answer(opID string) (tracker.WriteResult, error) {
	u.ops = append(u.ops, opID)
	return tracker.WriteResult{Key: receiptKey, Result: statelog.Result{
		Outcome: statelog.OutcomeUnknown, OpID: opID, Unvouched: u.unvouched,
		Version: receiptVersion,
	}}, nil
}

func (u *unknownWriter) UpdateTask(_ context.Context, opID, _, _ string, _ uint64,
	_ tracker.TaskPatch, _ tracker.ChangeKind, _ *tracker.Notify) (tracker.WriteResult, error) {
	return u.answer(opID)
}

func (u *unknownWriter) MergeDuplicates(_ context.Context, opID, _, _ string, _ bool,
	_ *tracker.Notify) (tracker.WriteResult, error) {
	return u.answer(opID)
}

func (u *unknownWriter) MoveTaskToProject(_ context.Context, opID, _, _ string,
	_ *tracker.Notify) (tracker.WriteResult, error) {
	return u.answer(opID)
}

func (u *unknownWriter) RemoveTask(_ context.Context, opID, _, _ string, _ bool,
	_ *tracker.Notify) (tracker.WriteResult, error) {
	return u.answer(opID)
}

func (u *unknownWriter) RestoreTask(_ context.Context, opID, _, _ string,
	_ *tracker.Notify) (tracker.WriteResult, error) {
	return u.answer(opID)
}

func (u *unknownWriter) WriteView(_ context.Context, opID string,
	_ tracker.View) (tracker.WriteResult, error) {
	return u.answer(opID)
}

func (u *unknownWriter) WriteTypes(_ context.Context, opID string,
	_ []tracker.TaskType) (tracker.WriteResult, error) {
	return u.answer(opID)
}

func (u *unknownWriter) WriteFields(_ context.Context, opID string,
	_ []tracker.FieldDef) (tracker.WriteResult, error) {
	return u.answer(opID)
}

func (u *unknownWriter) WritePriorities(_ context.Context, opID, _ string, _ []string,
	_ *uint64, _ tracker.PersonAuthority) (tracker.WriteResult, error) {
	return u.answer(opID)
}

func (u *unknownWriter) WritePins(_ context.Context, opID, _ string,
	_ tracker.PinGesture) (tracker.WriteResult, error) {
	return u.answer(opID)
}

func (u *unknownWriter) MarkInbox(_ context.Context, opID, _ string,
	_ tracker.InboxGesture) (tracker.WriteResult, error) {
	return u.answer(opID)
}

// PlaceTask answers both steps of a drag unknown.
func (u *unknownWriter) PlaceTask(_ context.Context, opID string, _ tracker.Place,
	_ *tracker.Notify) (tracker.PlaceResult, error) {
	lane, _ := u.answer(statelog.StepOpID(opID, "lane"))
	return tracker.PlaceResult{Lane: lane, Version: receiptVersion,
		Unplaced: statelog.ErrUnavailable}, nil
}

func (u *unknownWriter) WriteProject(_ context.Context, opID, _ string,
	_ tracker.ProjectEdit, _ tracker.ProjectAuthority) (tracker.WriteResult, error) {
	return u.answer(opID)
}

func (u *unknownWriter) WriteTags(_ context.Context, opID, _ string,
	_ tracker.TagEdit, _ tracker.TagAuthority) (tracker.WriteResult, error) {
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
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{Work: deps}) {
		if err := reg.Register(tool, tools.OriginBuiltin); err != nil {
			t.Fatalf("register %s: %v", tool.Name(), err)
		}
	}
	return reg
}

// receipt is what an answer about a write that may not exist must never carry:
// the fields of a success, and the key and version the fake's result holds.
// It is looked for in the answer with its operations taken out
// ([withoutOperations]), which are random hex that spells a version now and
// then.
var receipt = []string{`"version"`, `"comment_id"`, `"key"`, `"id"`, `"outcome"`,
	`"position"`, receiptKey, strconv.Itoa(receiptVersion), "NOT made"}

// unknownOperatorCalls is one call to every tracker write on the operator's
// surface. A fresh table on every call rather than a package variable: two
// parallel tests hand these arguments to tools, and one shared map would be a
// data race the moment a tool wrote to its arguments.
func unknownOperatorCalls() map[string]map[string]any {
	return map[string]map[string]any{
		builtin.UpdateWorkItemTool: {"item": "ENG-1", "status": "done"},
		builtin.CommentOnWorkTool:  {"item": "ENG-1", "body": "done, see the PR", "ask": "eng"},
		tracker.MergeWorkItemTool:  {"item": "ENG-2", "into": "ENG-1"},
		tracker.MoveWorkItemTool:   {"item": "ENG-1", "project": "OPS"},
		tracker.PlaceWorkItemTool: {"item": "ENG-1", "before": "ENG-2",
			"status": "in_progress", "if_match": 7},
		tracker.RemoveWorkItemTool:     {"item": "ENG-1"},
		tracker.RestoreWorkItemTool:    {"item": "ENG-1"},
		tracker.SaveWorkViewTool:       {"container": "project:ENG", "name": "Mine", "type": "list"},
		tracker.SetPrioritiesTool:      {"items": []any{"ENG-1"}},
		tracker.SetPinsTool:            {"views": map[string]any{"add": []any{"v-1"}}},
		tracker.MarkInboxTool:          {"primary_reasons": []any{"mention"}},
		tracker.WriteWorkCatalogueTool: {"types": []any{map[string]any{"slug": "bug", "name": "Bug"}}},
		tracker.WriteProjectTool:       {"project": "ENG", "tags_add": []any{map[string]any{"slug": "regression"}}},
	}
}

// restatingTools are THE TWO THAT STATE A VALUE: they mint a fresh operation
// per call, so they take no op_id and name the operation they wrote under.
var restatingTools = map[string]bool{
	tracker.WriteWorkCatalogueTool: true, tracker.WriteProjectTool: true,
}

// EVERY TRACKER WRITE WHOSE OUTCOME IS UNKNOWN IS ANSWERED AS UNKNOWN, on
// every surface, and never with a receipt.
//
// Only the create and the page tools held the rule. A comment the engine
// answered `unknown` without publishing — its operation minted before this
// node's ledger lost rows, a seat on a backlog turn just after its node adopted
// a snapshot — came back as a comment_id with its mentions and its ask, and the
// seat told whoever asked that it had answered. An update handed back a
// `version` a model passes as `if_match`, naming a version the item may never
// have had, and a saved view an id for a view that may not exist.
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
			// THE op_id IT NAMES IS THE ONE THAT FINISHES IT: brought back
			// with the same call, every write derives the id it derived
			// the first time.
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
		tracker.MergeWorkItemTool:  operatorCalls[tracker.MergeWorkItemTool],
		tracker.MoveWorkItemTool:   operatorCalls[tracker.MoveWorkItemTool],
	}
	for _, unvouched := range []bool{false, true} {
		for name, args := range seatCalls {
			u := &unknownWriter{fakeTracker: newFakeTracker(), unvouched: unvouched}
			deps := u.deps()
			reg := tools.NewRegistry()
			if _, err := builtin.Register(reg, builtin.Deps{
				Work: deps, LeadsProject: leadAlways,
			}); err != nil {
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
// An operation id is random hex ([statelog.NewOpID]) and every decimal digit
// is a hex digit, so a minted id spells the fake's version now and then, and
// the test above, which mints a fresh id for every operator call, read one as
// the version leaking a run or two in every few hundred. This pins that case on
// EVERY run, under the id it happened on, for every tool an operator can bring
// an operation back to — and pins the other side with it, in three doctored
// answers the check must still refuse: the version printed after the id, the
// version GLUED to the id's last character, which only a removal by exact value
// reads correctly (a pattern that ate the id's run of characters would eat the
// version with it), and an operation the writer was never handed.
func TestAnOperationWhoseIDSpellsTheReceiptIsNotReadAsOne(t *testing.T) {
	t.Parallel()
	// spelled is the operation that test failed on in CI: its last group is
	// b9901477cd19.
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
	// AN ID LEFT OVER is an operation the writer was never handed. Named in an
	// answer it is a pointer the reader cannot use — and here it is also the
	// one thing that would let the false positive above back in, since only the
	// ids this call wrote under are taken out before the receipt is looked for.
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
// # Why a receipt is looked for only in what is left
//
// Because an operation id is random hex the answer is REQUIRED to carry
// ([statelog.NewOpID]), and every decimal digit is a hex digit, so now and then
// a minted id spells "9901" somewhere, and a check over the whole answer read
// that as the version leaking. Nothing a write reports can be inside an id
// minted before the write was made, so taking the ids out hides no leak — and
// an id the writer was NOT handed is never taken out, and is itself refused
// ([unknownAnswerProblems]).
//
// BY VALUE, NEVER BY PATTERN: only the exact ids this call wrote under come
// out, so a version printed right beside one is still read. And only ids the
// engine's grammar minted ([statelog.OpMintedAt]): a test's literal like "op"
// carries no random part to take out, and taking it out would take words
// with it.
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
// dependency sequence's last commit is usually another item's — a blocker's
// mirror — and its version, handed back as `if_match` on this item, was refused
// as stale.
func TestADependencyOnlyUpdateReportsTheItemsOwnVersion(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	trk.dependAnswer = &tracker.DependencyResult{
		WriteResult: tracker.WriteResult{Result: statelog.Result{
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
// (the fake's version 12, at S@1:12) and the dependency's authored
// `waiting_on` commit lands on the same item's subject after it (88), with a
// blocker's mirror last of all (90). The answer carried the patch's 12 and
// its position: sent back as `if_match`, 12 was refused as stale by the
// call's own second write, and a caller settling at S@1:12 read its own
// dependencies back missing.
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
			trk.dependAnswer = &tracker.DependencyResult{
				WriteResult: tracker.WriteResult{Result: statelog.Result{
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
