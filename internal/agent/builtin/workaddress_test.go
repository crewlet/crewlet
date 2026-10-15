package builtin_test

import (
	"context"
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/sourcetree"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tracker"
)

// withDuplicate is the fake tracker holding a SECOND task under ENG-1: `dup`,
// which the key directory does not name, so it carries `key_collision` and is
// reached by its id. ENG-1 itself goes on opening the claimant, `i1`.
func withDuplicate(trk *fakeTracker, removed bool) *fakeTracker {
	dup := tracker.TaskDetail{
		Task: tracker.Task{
			ID: "dup", Key: "ENG-1", Project: "ENG", Title: "the duplicate",
			Status: tracker.StatusTodo, StatusGroup: tracker.GroupNotStarted,
			Version: 3,
		},
		KeyCollision: true, Complete: true,
	}
	if removed {
		dup.Task.Removed = &tracker.Tombstone{By: "cto"}
	}
	trk.tasks["dup"] = dup
	return trk
}

// addressSurface is the operator's catalogue — the one every write tool is
// served on — over a fake tracker and a trash that answers applied.
func addressSurface(t *testing.T, trk *fakeTracker, spy *trashSpy) *tools.Registry {
	t.Helper()
	reg := tools.NewRegistry()
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Work: builtin.WorkDeps{
			Reader: trk, Writer: trk.as, Merges: trk.merges, Moves: trk.moves,
			TrashWriter: func(builtin.Actor) builtin.TrashWriter { return spy },
			Actor:       operatorActor,
		},
		Authorize: builtin.Decide(chartLeads),
	}) {
		if err := reg.Register(tool, tools.OriginBuiltin); err != nil {
			t.Fatalf("register %s: %v", tool.Name(), err)
		}
	}
	return reg
}

// receiptFields decodes a tool's answer, failing the case on a refusal.
func receiptFields(t *testing.T, name string, got tools.Result) map[string]any {
	t.Helper()
	if got.Failed {
		t.Fatalf("%s failed: %s", name, got.Output)
	}
	var answer map[string]any
	if err := json.Unmarshal([]byte(got.Output), &answer); err != nil {
		t.Fatalf("%s answered %q, which is not a receipt: %v", name, got.Output, err)
	}
	return answer
}

// EVERY RECEIPT ABOUT THE DUPLICATE OF A KEY NAMES IT BY ITS ID.
//
// A task holding a key another task claimed first is reached only by its id,
// and every tool echoed its KEY: comment_on_work_item answered `item: ENG-1`,
// the receipts of an edit, a merge, a removal and a restore answered `key:
// ENG-1` and nothing else — so the caller's next call, built from what it was
// just told, landed on the claimant. Each now names the task in `item` by the
// rule every row follows, with the key beside it and the flag that explains
// the difference; and a task that answers to its key is still named by it.
//
// Mutation: echo the key in `item` from the receipt helper, or name a receipt
// from anything but the task it read, and the duplicate's case fails.
func TestEveryReceiptNamesAKeysDuplicateByItsID(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		tool    string
		args    map[string]any
		removed bool
	}{
		{builtin.UpdateWorkItemTool, map[string]any{"status": "in_progress"}, false},
		{builtin.CommentOnWorkTool, map[string]any{"body": "is this the one?"}, false},
		{tracker.MergeWorkItemTool, map[string]any{"into": "ENG-2"}, false},
		{tracker.RemoveWorkItemTool, map[string]any{}, false},
		{tracker.RestoreWorkItemTool, map[string]any{}, true},
	} {
		for ref, want := range map[string]struct {
			item      string
			collision bool
		}{
			"dup":   {"dup", true},
			"ENG-1": {"ENG-1", false},
		} {
			t.Run(c.tool+"/"+ref, func(t *testing.T) {
				t.Parallel()
				trk := withDuplicate(newFakeTracker(), c.removed)
				if c.removed && ref == "ENG-1" {
					claimant := trk.tasks["ENG-1"]
					claimant.Task.Removed = &tracker.Tombstone{By: "cto"}
					trk.tasks["ENG-1"] = claimant
				}
				args := map[string]any{"item": ref}
				for k, v := range c.args {
					args[k] = v
				}
				answer := receiptFields(t, c.tool, callNoTurn(t,
					addressSurface(t, trk, &trashSpy{}), c.tool, args))
				if answer["item"] != want.item {
					t.Errorf("%s about %s answered item %v, want %s — the "+
						"caller hands `item` back to its next call", c.tool,
						ref, answer["item"], want.item)
				}
				if answer["key"] != "ENG-1" {
					t.Errorf("%s about %s answered key %v, want ENG-1 beside "+
						"the address", c.tool, ref, answer["key"])
				}
				if got, _ := answer["key_collision"].(bool); got != want.collision {
					t.Errorf("%s about %s answered key_collision=%v, want %v",
						c.tool, ref, answer["key_collision"], want.collision)
				}
			})
		}
	}
}

// A CREATE OR A MOVE THAT LANDED ON A KEY ANOTHER TASK HOLDS NAMES ITS ID.
//
// A create after a counter restore is what PRODUCES a duplicate, and its
// receipt is the first thing that names it; a move mints its new key from the
// target's counter the same way. Both receipts named the key alone.
//
// Mutation: build either receipt's `item` from the key, or drop the flag the
// writer reported, and this fails.
func TestACreateOrAMoveOntoAHeldKeyNamesItsID(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	trk.createAnswer = &tracker.WriteResult{Key: "ENG-1", KeyCollision: true,
		Result: statelog.Result{Outcome: statelog.OutcomeApplied,
			Position: statelog.Position{Stream: "S", Generation: 1, Seq: 11},
			Version:  11}}
	answer := receiptFields(t, builtin.CreateWorkItemTool, callNoTurn(t,
		addressSurface(t, trk, &trashSpy{}), builtin.CreateWorkItemTool,
		map[string]any{"title": "file it", "project": "ENG"}))
	if id, _ := answer["id"].(string); id == "" || answer["item"] != id ||
		answer["key_collision"] != true {
		t.Errorf("a create that took a held key answered item %v, id %v, "+
			"key_collision %v — want its id as the item, flagged",
			answer["item"], answer["id"], answer["key_collision"])
	}

	moves := &collidingMoves{fakeTracker: newFakeTracker()}
	reg := tools.NewRegistry()
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Work: builtin.WorkDeps{
			Reader: moves.fakeTracker, Writer: moves.as,
			Moves: func(builtin.Actor) builtin.WorkMover { return moves },
			Actor: operatorActor,
		},
		Authorize: builtin.Decide(chartLeads),
	}) {
		if err := reg.Register(tool, tools.OriginBuiltin); err != nil {
			t.Fatalf("register %s: %v", tool.Name(), err)
		}
	}
	answer = receiptFields(t, tracker.MoveWorkItemTool, callNoTurn(t, reg,
		tracker.MoveWorkItemTool, map[string]any{"item": "ENG-1", "project": "OPS"}))
	if answer["item"] != "i1" || answer["key"] != "OPS-3" ||
		answer["key_collision"] != true || answer["moved_from"] != "ENG-1" {
		t.Errorf("a move onto a held key answered %v — want item i1 beside "+
			"key OPS-3, flagged, moved from ENG-1", answer)
	}
}

// collidingMoves is a move whose new key another task already holds.
type collidingMoves struct{ *fakeTracker }

func (c *collidingMoves) MoveTaskToProject(ctx context.Context, opID, taskID,
	target string, notify *tracker.Notify) (tracker.WriteResult, error) {

	got, err := c.fakeTracker.MoveTaskToProject(ctx, opID, taskID, target, notify)
	got.KeyCollision = true
	return got, err
}

// A WRITE NOBODY CAN SAY LANDED SENDS ITS CALLER TO THE DUPLICATE BY ITS ID.
//
// An unknown outcome is answered with what to read to find out — "Read ENG-1
// with get_work_item", "Read ENG-1's thread" — and for a duplicate that read
// opens the claimant, whose version and thread say nothing about whether this
// write landed. Every such pointer now names the address, and the prose
// naming the task names its id with the key beside it.
//
// Mutation: build any of these pointers from the key and this fails.
func TestAnUnknownWriteOnAKeysDuplicateSendsTheCallerToItsID(t *testing.T) {
	t.Parallel()
	for name, args := range map[string]map[string]any{
		builtin.UpdateWorkItemTool:  {"item": "dup", "status": "done"},
		builtin.CommentOnWorkTool:   {"item": "dup", "body": "done, see the PR"},
		tracker.MergeWorkItemTool:   {"item": "dup", "into": "ENG-2"},
		tracker.MoveWorkItemTool:    {"item": "dup", "project": "OPS"},
		tracker.RemoveWorkItemTool:  {"item": "dup"},
		tracker.RestoreWorkItemTool: {"item": "dup"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			// UNVOUCHED, the one unknown whose answer always says what to
			// read: this node cannot tell, so the caller has to look.
			u := &unknownWriter{fakeTracker: withDuplicate(newFakeTracker(),
				name == tracker.RestoreWorkItemTool), unvouched: true}
			got := callNoTurn(t, u.operatorSurface(t), name, args)
			if !got.Failed {
				t.Fatalf("%s answered an unknown outcome as a receipt: %s",
					name, got.Output)
			}
			if !strings.Contains(got.Output, "Read dup") {
				t.Errorf("%s does not send its caller to read dup: %s", name,
					got.Output)
			}
			if strings.Contains(got.Output, "Read ENG-1") {
				t.Errorf("%s sends its caller to read ENG-1, which opens the "+
					"claimant: %s", name, got.Output)
			}
			if !strings.Contains(got.Output, "dup (its key ENG-1 opens another task)") {
				t.Errorf("%s does not say which task it is about and why it "+
					"names an id: %s", name, got.Output)
			}
		})
	}
}

// EVERY ARGUMENT THAT OFFERS A KEY SAYS WHEN TO USE THE ID INSTEAD.
//
// A row's `key_collision` is half the rule; the other half is what a model
// does with it, and the only place a model reads that before composing a call
// is the argument's own description. Every one of them said "the item key
// (ENG-42) or its id", which a model reads as permission to type the key it
// was shown — onto the claimant.
//
// OVER BOTH CATALOGUES, a seat's and the operator's, because each serves tools
// the other does not. And it is held in BOTH directions: an argument offering
// a key must carry the sentence, and the arguments known to take a task must
// be among those found, so a description reworded past the pattern cannot
// pass this by no longer being seen.
func TestEveryArgumentOfferingAKeySaysWhenToUseTheID(t *testing.T) {
	t.Parallel()
	offersKey := regexp.MustCompile(`(?i)ENG-42|\bkeys? or (an |its )?ids?\b|\bids? or keys?\b`)
	schemas := map[string]map[string]any{}
	reg := tools.NewRegistry()
	if _, err := builtin.Register(reg, gated(fullDeps(t))); err != nil {
		t.Fatalf("Register: %v", err)
	}
	for _, name := range reg.Names() {
		entry, _ := reg.Lookup(name)
		schemas[name] = entry.Tool.Parameters()
	}
	for _, tool := range builtin.OperatorTools(operatorDeps(t)) {
		schemas[tool.Name()] = tool.Parameters()
	}
	seen := map[string]bool{}
	for name, schema := range schemas {
		walkArguments(name, schema, func(path, description string) {
			if !offersKey.MatchString(description) {
				return
			}
			seen[path] = true
			if !strings.Contains(description, "key_collision") {
				t.Errorf("%s offers a key and never says when to use the id: "+
					"%q — a model that types the key it was shown reaches the "+
					"task that claimed it first", path, description)
			}
		})
	}
	for _, path := range []string{
		"get_work_item.item", "update_work_item.item",
		"update_work_item.duplicate_of", "update_work_item.waiting_on",
		"update_work_item.blocking", "update_work_item.linked",
		"comment_on_work_item.item", "create_work_item.parent",
		"create_work_item.waiting_on", "list_work_items.parent",
		"merge_work_item.item", "merge_work_item.into", "move_work_item.item",
		"remove_work_item.item", "restore_work_item.item",
		"set_priorities.items", "task_activity.task",
	} {
		if !seen[path] {
			t.Errorf("%s takes a task and was not found offering a key, so this "+
				"gate no longer sees what it holds", path)
		}
	}
}

// walkArguments calls fn with every described property of a JSON Schema, at
// any depth, as `tool.property`.
func walkArguments(prefix string, schema map[string]any, fn func(path, description string)) {
	props, _ := schema["properties"].(map[string]any)
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		prop, _ := props[name].(map[string]any)
		path := prefix + "." + name
		if description, _ := prop["description"].(string); description != "" {
			fn(path, description)
		}
		walkArguments(path, prop, fn)
		if items, ok := prop["items"].(map[string]any); ok {
			walkArguments(path, items, fn)
		}
	}
}

// NOTHING HERE NAMES A TASK BY A KEY READ OFF A DETAIL, outside the functions
// that apply the address rule or that report a key as what it is.
//
// The bug this holds was a pattern rather than a line: about twenty answers
// and hints each echoed `before.Task.Key` where the caller needed the task's
// address, and each looked right beside the others. So the expression itself
// is what is refused — a new receipt or hint reaches for
// [tracker.TaskDetail.Address], [tracker.TaskDetail.Named] or the receipt
// helper, or it is declared here with the reason the key is the right thing.
//
// Both directions: an undeclared use fails, and so does a declared function
// that no longer reads one, because an allowance outliving its reason is how
// the next echo gets in under it.
func TestNoToolNamesATaskByAKeyOutsideTheAddressRule(t *testing.T) {
	t.Parallel()
	allowed := map[string]string{
		"ReceiptOf": "the receipt helper: it puts the key BESIDE the address " +
			"it computes, never in its place",
		"WorkDeps.parentParty": "a wake's TaskParty carries the key as data " +
			"beside the id it routes on, and shows it to nobody as a reference",
		"moveWorkItem.CallForTurn": "`moved_from` reports the key the item is " +
			"LEAVING — what it was called, not a reference to it",
	}
	dir := filepath.Join(sourcetree.Root(t), "internal", "agent", "builtin")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read %s: %v", dir, err)
	}
	found := map[string]bool{}
	read := 0
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") ||
			strings.HasSuffix(name, "_test.go") {
			continue
		}
		read++
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, name),
			nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			owner := funcName(fn)
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Key" {
					return true
				}
				inner, ok := sel.X.(*ast.SelectorExpr)
				if !ok || inner.Sel.Name != "Task" {
					return true
				}
				found[owner] = true
				if _, ok := allowed[owner]; !ok {
					t.Errorf("%s: %s reads a task's key off a detail — name the "+
						"task through its Address or Named, or ReceiptOf, or "+
						"declare here why the key is what this text means",
						name, owner)
				}
				return true
			})
		}
	}
	if read < 10 {
		t.Fatalf("read %d source files in %s, so this walk certifies nothing", read, dir)
	}
	for owner, why := range allowed {
		if !found[owner] {
			t.Errorf("%s is allowed to read a task's key (%s) and no longer "+
				"does — drop the allowance", owner, why)
		}
	}
}

// funcName is a function's name as the allowance spells it: `Recv.Name` for a
// method, `Name` for a function.
func funcName(fn *ast.FuncDecl) string {
	if fn.Recv == nil || len(fn.Recv.List) == 0 {
		return fn.Name.Name
	}
	recv := fn.Recv.List[0].Type
	if star, ok := recv.(*ast.StarExpr); ok {
		recv = star.X
	}
	if ident, ok := recv.(*ast.Ident); ok {
		return ident.Name + "." + fn.Name.Name
	}
	return fn.Name.Name
}
