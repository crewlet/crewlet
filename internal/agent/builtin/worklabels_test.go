package builtin_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// labelSurface is one surface's registry over the fake tracker, with the
// project writer the inline declaration goes through.
func labelSurface(t *testing.T, trk *fakeTracker, operator bool) *tools.Registry {
	t.Helper()
	deps := builtin.WorkDeps{
		Reader: trk, Writer: trk.as, Dependencies: trk.depends,
		ProjectWriter: func(builtin.Actor) builtin.ProjectWriter { return trk },
	}
	if !operator {
		return workRegistry(t, deps)
	}
	deps.Actor = operatorActor
	reg := tools.NewRegistry()
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{Work: deps}) {
		if err := reg.Register(tool, tools.OriginBuiltin); err != nil {
			t.Fatalf("register %s: %v", tool.Name(), err)
		}
	}
	return reg
}

// declarationStopped is the error the tracker's EnsureTags answers when its
// declaration's outcome is unknown: [tracker.ErrStepUnresolved], or
// [tracker.ErrStepUnvouched] where this node's ledger cannot vouch for it.
func declarationStopped(unvouched bool) error {
	sentinel := tracker.ErrStepUnresolved
	if unvouched {
		sentinel = tracker.ErrStepUnvouched
	}
	return fmt.Errorf("tracker: declare [regression] in ENG: tracker: whether "+
		"the declaration of [regression] in ENG landed is unknown (operation "+
		"step-op-of-the-declaration): %w", sentinel)
}

// A WRITE STOPPED AT ITS OWN LABEL DECLARATION IS ANSWERED UNDER ITS OWN TOOL,
// as the write it did not make, with the repeat that finishes both.
//
// The declaration runs BEFORE the task's append, because the create refuses a
// label its project has not declared. An unknown declaration was answered
// under write_project — "write_project stopped part of the way through … call
// write_project again with exactly the same arguments" — which is a tool the
// caller never called: that call answered "changes nothing", or declared the
// tag and answered without error, which the text named as the point the work
// could be reported done. The item had never been filed. An operator was
// handed the create's op_id for a tool that takes none.
func TestAWriteStoppedAtItsLabelDeclarationIsAnsweredUnderItsOwnTool(t *testing.T) {
	t.Parallel()
	calls := map[string]struct {
		args    map[string]any
		notMade string
		look    string
	}{
		builtin.CreateWorkItemTool: {
			args: map[string]any{"title": "the regression", "project": "ENG",
				"labels": []any{"regression"}, "labels_create_missing": true},
			notMade: "The work item was NOT filed by this call.",
			look:    "look for it with list_work_items",
		},
		builtin.UpdateWorkItemTool: {
			args: map[string]any{"item": "ENG-1", "labels": []any{"regression"},
				"labels_create_missing": true},
			notMade: "Nothing it asked of ENG-1 was written by this call.",
			look:    "read ENG-1 with get_work_item",
		},
	}
	for name, tc := range calls {
		for _, operator := range []bool{false, true} {
			for _, unvouched := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/operator=%v/unvouched=%v", name, operator,
					unvouched), func(t *testing.T) {
					t.Parallel()
					trk := newFakeTracker()
					trk.ensureErr = declarationStopped(unvouched)
					reg := labelSurface(t, trk, operator)
					call := func(args map[string]any) tools.Result {
						if operator {
							return callNoTurn(t, reg, name, args)
						}
						return callWork(t, reg, name, args)
					}
					args := map[string]any{}
					for k, v := range tc.args {
						args[k] = v
					}
					op := statelog.NewOpID(time.Now(), name)
					if operator {
						args["op_id"] = op
					}

					got := call(args)
					checkStoppedAtLabels(t, name, got, tc.notMade)
					if len(trk.created) != 0 || len(trk.patched) != 0 {
						t.Fatalf("the call carried on past its unknown declaration: "+
							"created %d, patched %d", len(trk.created), len(trk.patched))
					}
					if len(trk.opIDs) != 1 {
						t.Fatalf("the declaration wrote under %d operations", len(trk.opIDs))
					}
					declaration := trk.opIDs[0]

					// THE REPEAT IS THIS CALL, under THIS caller's own terms.
					again := name + " again with exactly the same arguments, " +
						"before calling it with any others"
					named := declaration
					if operator {
						again = fmt.Sprintf("%s again with exactly the same "+
							"arguments and `op_id` %q", name, op)
						named = op
						if strings.Contains(got.Output, declaration) {
							t.Errorf("the operator is shown the declaration's step "+
								"id %s, which as an op_id is a new call: %s",
								declaration, got.Output)
						}
					}
					if !strings.Contains(got.Output, again) {
						t.Errorf("the answer does not say %q: %s", again, got.Output)
					}
					if !strings.Contains(got.Output, "operation "+named+")") {
						t.Errorf("the answer does not name operation %s: %s", named,
							got.Output)
					}
					if unvouched {
						// THE REPEAT HERE STOPS AT THE SAME STEP, so the
						// step that moves it is named: a declaration made
						// again, which is harmless.
						//
						// AND WHAT THAT REPEAT THEN DOES WITH ITS OWN
						// WRITE: every step of one call dates from the
						// same instant, so it is unvouched too — answered
						// from what this node holds of it, or unknown
						// again, which means looking.
						wants := []string{
							"stops at this step",
							`write_project on ENG with tags_add: [{"slug": "regression"}]`,
							"harmless",
							"cannot vouch for it either",
							"answers `unknown` again",
							tc.look,
						}
						if operator {
							wants = append(wants, "another node's operator MCP")
						} else if strings.Contains(got.Output, "another node") {
							t.Errorf("a seat, which cannot choose a node, is "+
								"sent to another: %s", got.Output)
						}
						for _, want := range wants {
							if !strings.Contains(got.Output, want) {
								t.Errorf("the unvouched answer lacks %q: %s", want,
									got.Output)
							}
						}
					} else if !strings.Contains(got.Output,
						"answers the declaration with what landed") {
						t.Errorf("the answer does not say the repeat answers the "+
							"declaration: %s", got.Output)
					}

					// AND THAT REPEAT IS WHAT FINISHES IT: the declaration
					// is the same operation, and the write follows it.
					trk.ensureErr = nil
					if operator {
						args["op_id"] = op
					}
					if done := call(args); done.Failed {
						t.Fatalf("the repeat failed: %s", done.Output)
					}
					if len(trk.opIDs) < 2 || trk.opIDs[1] != declaration {
						t.Errorf("the repeat declared under %v, the first call under "+
							"%s — a different operation", trk.opIDs[1:], declaration)
					}
					if len(trk.created)+len(trk.patched) != 1 {
						t.Errorf("the repeat made %d writes after the declaration, "+
							"want 1", len(trk.created)+len(trk.patched))
					}
				})
			}
		}
	}
}

// checkStoppedAtLabels is what every stopped-at-its-labels answer shares.
func checkStoppedAtLabels(t *testing.T, name string, got tools.Result, notMade string) {
	t.Helper()
	if !got.Failed {
		t.Fatalf("%s answered a stopped declaration as a receipt: %s", name, got.Output)
	}
	if !strings.HasPrefix(got.Output, name+" stopped before ") {
		t.Errorf("the answer is not under %s: %s", name, got.Output)
	}
	if !strings.Contains(got.Output, notMade) {
		t.Errorf("the answer does not say %q: %s", notMade, got.Output)
	}
	for _, wrong := range []string{
		"write_project stopped", "write_project again", "write_project was refused",
		"ENG-9", `"key"`,
	} {
		if strings.Contains(got.Output, wrong) {
			t.Errorf("the answer carries %q: %s", wrong, got.Output)
		}
	}
}

// A DECLARATION REFUSED OVER A TAG IS ANSWERED UNDER THE CALLING TOOL, with
// the ways past it that fit a write of one item: this project's own tag in
// `labels`, or a declaration of one's own. It opened "write_project was
// refused" about a create, and named a remedy — "call write_project again" —
// for a call nobody had made.
func TestALabelDeclarationRefusedOverATagIsAnsweredUnderTheCallingTool(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		err  error
		want []string
	}{
		"a label the project already uses": {
			err: fmt.Errorf("tracker: declare [regression] in ENG: %w",
				&tracker.TagClash{Project: "ENG", Slug: "regression", Label: "regression",
					Other: tracker.Tag{Slug: "regressions", Label: "Regression"}}),
			want: []string{"was refused, and nothing was written",
				"give `labels` regressions instead of regression",
				`write_project on ENG with tags_add: [{"slug": "regression"`,
				"make this call again"},
		},
		"a project with no room for another tag": {
			err: fmt.Errorf("tracker: declare [regression] in ENG: %w",
				&tracker.TagsFull{Project: "ENG", Slug: "regression"}),
			want: []string{"was refused, and nothing was written",
				"leave regression out of `labels`", "tags_archive"},
		},
		"a declaration that could not be read or written": {
			err: errors.New("tracker: read ENG's tags: disk full"),
			want: []string{"was refused before ", "declaring its labels in ENG " +
				"did not land", "Nothing was written"},
		},
	} {
		for _, tool := range []string{builtin.CreateWorkItemTool, builtin.UpdateWorkItemTool} {
			t.Run(name+"/"+tool, func(t *testing.T) {
				t.Parallel()
				trk := newFakeTracker()
				trk.ensureErr = tc.err
				args := map[string]any{"title": "the regression", "project": "ENG",
					"labels": []any{"regression"}, "labels_create_missing": true}
				if tool == builtin.UpdateWorkItemTool {
					args = map[string]any{"item": "ENG-1",
						"labels": []any{"regression"}, "labels_create_missing": true}
				}
				got := callWork(t, labelSurface(t, trk, false), tool, args)
				if !got.Failed || !strings.HasPrefix(got.Output, tool+" was refused") {
					t.Fatalf("the refused declaration answered %q", got.Output)
				}
				if strings.Contains(got.Output, "write_project was refused") ||
					strings.Contains(got.Output, "write_project again") {
					t.Errorf("the answer is about a write_project call nobody "+
						"made: %s", got.Output)
				}
				for _, want := range tc.want {
					if !strings.Contains(got.Output, want) {
						t.Errorf("the answer lacks %q: %s", want, got.Output)
					}
				}
				if len(trk.created) != 0 || len(trk.patched) != 0 {
					t.Errorf("the call carried on past its refused declaration")
				}
			})
		}
	}
}
