package builtin_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
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
	return tracker.WriteResult{Key: "ZZZ-77", Result: statelog.Result{
		Outcome: statelog.OutcomeUnknown, OpID: opID, Unvouched: u.unvouched,
		Version: 9901,
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
	_ tracker.View, _ tracker.ViewPrior) (tracker.WriteResult, error) {
	return u.answer(opID)
}

func (u *unknownWriter) ViewPrior(context.Context, string) (tracker.ViewPrior, error) {
	return tracker.ViewPrior{}, nil
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
	_ tracker.PersonAuthority) (tracker.WriteResult, error) {
	return u.answer(opID)
}

func (u *unknownWriter) WritePins(_ context.Context, opID, _ string, _ []string,
	_ []tracker.Favorite, _ tracker.PersonAuthority) (tracker.WriteResult, error) {
	return u.answer(opID)
}

func (u *unknownWriter) WriteInbox(_ context.Context, opID, _ string,
	_, _, _ []tracker.InboxEntry, _ []tracker.Reason,
	_ tracker.Position, _ tracker.PersonAuthority) (tracker.WriteResult, error) {
	return u.answer(opID)
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
// the fields of a success, and the key and version the fake's result holds.
var receipt = []string{`"version"`, `"comment_id"`, `"key"`, `"id"`, `"outcome"`,
	`"position"`, "ZZZ-77", "9901", "NOT made"}

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
	operatorCalls := map[string]map[string]any{
		builtin.UpdateWorkItemTool:     {"item": "ENG-1", "status": "done"},
		builtin.CommentOnWorkTool:      {"item": "ENG-1", "body": "done, see the PR", "ask": "eng"},
		tracker.MergeWorkItemTool:      {"item": "ENG-2", "into": "ENG-1"},
		tracker.MoveWorkItemTool:       {"item": "ENG-1", "project": "OPS"},
		tracker.RemoveWorkItemTool:     {"item": "ENG-1"},
		tracker.RestoreWorkItemTool:    {"item": "ENG-1"},
		tracker.SaveWorkViewTool:       {"container": "project:ENG", "name": "Mine", "type": "list"},
		tracker.SetPrioritiesTool:      {"items": []any{"ENG-1"}},
		tracker.SetPinsTool:            {"views": []any{"v-1"}},
		tracker.MarkInboxTool:          {"primary_reasons": []any{"mention"}},
		tracker.WriteWorkCatalogueTool: {"types": []any{map[string]any{"slug": "bug", "name": "Bug"}}},
		tracker.WriteProjectTool:       {"project": "ENG", "tags_add": []any{map[string]any{"slug": "regression"}}},
	}
	// THE TWO THAT STATE A VALUE mint a fresh operation per call, so they
	// take no op_id and name the operation they wrote under.
	restates := map[string]bool{
		tracker.WriteWorkCatalogueTool: true, tracker.WriteProjectTool: true,
	}
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
			checkUnknownAnswer(t, name, unvouched, got)
			if restates[name] {
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
			checkUnknownAnswer(t, name, unvouched, got)
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

// checkUnknownAnswer is what every one of those answers has in common.
func checkUnknownAnswer(t *testing.T, name string, unvouched bool, got tools.Result) {
	t.Helper()
	if !got.Failed {
		t.Errorf("%s (unvouched %v) answered an unknown outcome as a receipt: %s",
			name, unvouched, got.Output)
		return
	}
	// AND THE CAUSE SAYS SO, because a surface answering in status codes
	// reads that rather than the sentence: without it the HTTP write
	// surface rendered an unknown as a refusal, telling a client its change
	// was NOT made when it may well have been.
	var outcome *builtin.UnknownOutcome
	if !errors.As(got.Cause, &outcome) || !errors.Is(got.Cause, builtin.ErrOutcomeUnknown) {
		t.Errorf("%s (unvouched %v) carries cause %v, want an UnknownOutcome",
			name, unvouched, got.Cause)
	} else if outcome.Unvouched != unvouched || outcome.OpID == "" {
		t.Errorf("%s (unvouched %v) names %+v as its unknown outcome", name,
			unvouched, outcome)
	}
	for _, leak := range receipt {
		if strings.Contains(got.Output, leak) {
			t.Errorf("%s (unvouched %v) carries %s beside an unknown outcome: %s",
				name, unvouched, leak, got.Output)
		}
	}
	if !strings.Contains(got.Output, "is unknown") ||
		!strings.Contains(got.Output, "do not report it as done") {
		t.Errorf("%s (unvouched %v) never says the outcome is unknown: %s",
			name, unvouched, got.Output)
	}
	if unvouched != strings.Contains(got.Output, "this node cannot tell") {
		t.Errorf("%s (unvouched %v) says this node cannot tell %v: %s", name,
			unvouched, !unvouched, got.Output)
	}
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
		TaskVersion: 42,
	}
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
				TaskVersion: tc.task,
			}
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
// Its sentence already said so to a model; its CAUSE was the writer's error
// alone, which the HTTP write surface has no status for but its default — a
// refusal telling a client that nothing had been written, about a gesture
// whose earlier steps had landed and whose stopped step may have. The error
// stays reachable inside the cause, so whether this node can vouch for the
// step is still a question [errors.Is] answers.
func TestAStoppedGestureCarriesAnUnknownOutcome(t *testing.T) {
	t.Parallel()
	for _, unvouched := range []bool{false, true} {
		sentinel := tracker.ErrStepUnresolved
		if unvouched {
			sentinel = tracker.ErrStepUnvouched
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
				// gesture here; unvouched, it stops at the same step on
				// this node, so the answer sends the caller elsewhere and
				// only the cause still names what it was.
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
