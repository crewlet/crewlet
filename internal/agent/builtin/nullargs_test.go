package builtin_test

import (
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	llm "github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/tools"
)

// A NULL REACHES NO BUILTIN AS A VALUE, on the path every call takes
// (ADR-0059): the surface's entry makes the call's canonical arguments,
// leaving a null out, before tools.Invoke checks the call and runs the tool.
// A builtin names what a call changes by the arguments it sends, and handed a
// null it read one as a value: update_work_item unassigned the item for a
// null `assignee`, and save_page emptied the page for a null `body` and, for a
// null `title`, asked a rename (refusing a colleague who leads nothing) and
// renamed the page "<nil>". The controls send each argument as a value, which
// is a change, so an argument left alone is told apart from one the tool
// stopped reading.
//
// Mutation: hand a null on from the entry (toolargs.Of) and each case makes
// the change it was not asked for.
func TestANullReachesNoBuiltinAsAValue(t *testing.T) {
	t.Parallel()
	t.Run("update_work_item's assignee", func(t *testing.T) {
		t.Parallel()
		trk := newFakeTracker()
		reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})
		surface := tools.NewSurface("execute", reg.Snapshot(),
			[]string{builtin.UpdateWorkItemTool}).ForTurn(workTurn(t))
		for _, assignee := range []any{nil, ""} {
			got, err := surface.Execute(everyGrant(), llm.ToolCall{ID: "c1",
				Name: builtin.UpdateWorkItemTool, Arguments: map[string]any{
					"item": "ENG-1", "priority": "high", "assignee": assignee,
				}})
			if err != nil || got.Failed {
				t.Fatalf("assignee %#v: (%q, %v)", assignee, got.Output, err)
			}
		}
		if len(trk.patched) != 2 {
			t.Fatalf("%d updates reached the tracker, want 2", len(trk.patched))
		}
		if a := trk.patched[0].Assignee; a != nil {
			t.Errorf("a null assignee reached the tracker as %q", *a)
		}
		if a := trk.patched[1].Assignee; a == nil || *a != "" {
			t.Errorf("an empty assignee reached the tracker as %v, not an unassignment, "+
				"so the case asserts nothing", a)
		}
	})

	t.Run("save_page's body and title", func(t *testing.T) {
		t.Parallel()
		kb := newFakeKB()
		reg := tools.NewRegistry()
		if _, err := builtin.Register(reg, builtin.Deps{
			Pages:     builtin.PageDeps{Reader: kb, Writer: kb},
			Authorize: builtin.Decide(chartRefuses),
		}); err != nil {
			t.Fatalf("register: %v", err)
		}
		surface := tools.NewSurface("execute", reg.Snapshot(),
			[]string{builtin.SavePageTool}).ForTurn(workTurn(t))
		save := func(args map[string]any) (bool, string) {
			t.Helper()
			args["page"], args["base_version"] = "p1", 4
			got, err := surface.Execute(colleagueCaller(), llm.ToolCall{ID: "c1",
				Name: builtin.SavePageTool, Arguments: args})
			if err != nil {
				t.Fatalf("save_page: %v", err)
			}
			return got.Failed, got.Output
		}
		if failed, out := save(map[string]any{"message": "tidied", "body": nil, "title": nil}); failed {
			t.Fatalf("a colleague's save with a null body and title was refused: %s", out)
		}
		if len(kb.saved) != 1 || kb.saved[0].Body != nil || len(kb.renames) != 0 {
			t.Fatalf("a null body and title saved %+v and renamed %+v, want the page "+
				"left as it was", kb.saved, kb.renames)
		}
		// THE CONTROLS: an empty body is a change, and naming a title asks a
		// rename this colleague is refused.
		if failed, out := save(map[string]any{"body": ""}); failed ||
			len(kb.saved) != 2 || kb.saved[1].Body == nil {
			t.Fatalf("an empty body did not reach the store as a change (%s), so the "+
				"case asserts nothing: %+v", out, kb.saved)
		}
		if failed, _ := save(map[string]any{"title": "Deploy Guide"}); !failed || len(kb.renames) != 0 {
			t.Fatalf("the colleague renamed the page by naming a title, so the case " +
				"asserts nothing")
		}
	})
}
