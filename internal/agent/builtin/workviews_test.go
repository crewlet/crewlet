package builtin_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/worklet"
)

// A saved view's parameters are the grammar's ([worklet.ParseQuery] validates
// them), not list_work_items' arguments, and save_work_view says so. Each entry
// of the renames table must be an argument this tool declares, a key the
// grammar accepts, and the translation the tool performs.
func TestEveryRenamedArgumentIsTheTranslationTheToolPerforms(t *testing.T) {
	t.Parallel()
	sentence := builtin.AliasSentence()
	for _, tc := range []struct {
		arg   string
		value any
		key   string
		want  string
	}{
		{"project", "eng", "container", "project:ENG"},
		{"text", "login", "q", "login"},
		{"label", "regression", "tag", "regression"},
		{"open_only", true, "status_group", "not_started,active"},
	} {
		t.Run(tc.arg, func(t *testing.T) {
			t.Parallel()
			if !strings.Contains(sentence, "`"+tc.arg+"` is `"+tc.key+"`") {
				t.Errorf("save_work_view's own sentence does not name %s -> %s: "+
					"%q — a model told the wrong vocabulary writes a view "+
					"every save refuses", tc.arg, tc.key, sentence)
			}
			if !slices.Contains(worklet.QueryKeys, tc.key) {
				t.Errorf("%q is not a query parameter, so a view storing it "+
					"is refused at the write", tc.key)
			}
			trk := newFakeTracker()
			reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})
			if got := callWork(t, reg, builtin.ListWorkItemsTool,
				map[string]any{tc.arg: tc.value}); got.Failed {
				t.Fatalf("the list failed: %s", got.Output)
			}
			if got := trk.params[tc.key]; got != tc.want {
				t.Errorf("the query carries %s=%q, want %q — the table names a "+
					"translation this tool does not perform", tc.key, got, tc.want)
			}
		})
	}
}

// A saved view can be run by its id, as list_work_views tells a model to: the
// tool forwards `view`, since pasting the view's parameters (in the grammar's
// names) reaches nothing.
func TestASavedViewCanBeRunByItsID(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})
	if got := callWork(t, reg, builtin.ListWorkItemsTool,
		map[string]any{"view": "v-7"}); got.Failed {
		t.Fatalf("running a saved view failed: %s", got.Output)
	}
	if got := trk.params["view"]; got != "v-7" {
		t.Fatalf("the query carries view=%q — a view a seat can list and "+
			"cannot open is a tab nobody can reach", got)
	}
}

// A refused `container` carries the four spellings it would accept: its
// grammar is the tracker's, so a caller holding `ENG` cannot derive
// `project:ENG`, and a refusal that only says no is repeated verbatim. The
// accepting case keeps a parser that refuses everything from passing.
func TestARefusedContainerNamesTheSpellingsItAccepts(t *testing.T) {
	t.Parallel()
	reg := operatorRegistry(t, newFakeTracker(), &personSpy{})
	for _, raw := range []string{"", "ENG", "project:", "workspace:ENG", "team:eng"} {
		t.Run("refused/"+raw, func(t *testing.T) {
			t.Parallel()
			got := callPlain(t, reg, worklet.ListWorkViewsTool,
				map[string]any{"container": raw})
			if !got.Failed {
				t.Fatalf("%q was accepted as a container — the strip it "+
					"renders is then somebody else's", raw)
			}
			for _, spelling := range []string{"workspace", "project:ENG",
				"unit:engineering", "person:ana"} {

				if !strings.Contains(got.Output, spelling) {
					t.Errorf("the refusal does not name %s: %q — a caller "+
						"told only that its container is wrong sends the "+
						"same one again", spelling, got.Output)
				}
			}
		})
	}
	t.Run("accepted", func(t *testing.T) {
		t.Parallel()
		if got := callPlain(t, reg, worklet.ListWorkViewsTool,
			map[string]any{"container": "project:eng"}); got.Failed {
			t.Fatalf("project:eng was refused: %s", got.Output)
		}
	})
}

// The shapes save_work_view's `type` enum offers are the ones
// [worklet.ViewType.Valid] takes. A model cannot discover what an enum omits
// (it is a closed set to its reader), so a missing shape is unreachable.
// Checked both ways: a shape the engine refuses fails at the write, worse.
func TestTheViewShapesTheToolOffersAreTheOnesTheEngineTakes(t *testing.T) {
	t.Parallel()

	// The operator surface, the only registry this tool is in, built from the
	// real constructor so a tool that stops registering fails the guard below.
	work := newFakeTracker()
	var offered []string
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Work: builtin.WorkDeps{
			Reader:     work,
			Writer:     work.as,
			ViewWriter: func(builtin.Actor) builtin.ViewWriter { return nil },
		},
		Authorize: builtin.Decide(chartLeads),
	}) {
		if tool.Name() != worklet.SaveWorkViewTool {
			continue
		}
		props, _ := tool.Parameters()["properties"].(map[string]any)
		shape, _ := props["type"].(map[string]any)
		offered = append(offered, shape["enum"].([]string)...)
	}
	if len(offered) == 0 {
		t.Fatal("save_work_view declares no view shapes at all, so this gate certifies nothing")
	}

	for _, name := range offered {
		if !worklet.ViewType(name).Valid() {
			t.Errorf("the tool offers the shape %q and the engine refuses it at "+
				"the write — a model is told to send something that cannot land", name)
		}
	}
	// Every shape the engine takes is offered: read from [worklet.ViewTypes],
	// not a literal, so a shape added to the engine is asserted at once.
	for _, want := range worklet.ViewTypes {
		if !slices.Contains(offered, string(want)) {
			t.Errorf("the engine accepts the shape %q and the tool does not offer it — "+
				"an enum is a closed set to the model reading it, so this shape is "+
				"unreachable however the model is asked", want)
		}
	}
}

// A board's row facts never reach a model's rows: a board view's `fields=`
// (labels, "blocks 3", spend) is a card's opt-in, and run through
// list_work_items it would put bytes nobody asked for in the model's context
// on every listing. The answer is byte-identical to the query without the view.
func TestTheBoardGetsSpendWithoutTheModelRowGrowing(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	trk.views = map[string]map[string]any{
		"v-board": {"fields": "spend,tags,dependents_count,open_asks"},
	}
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})

	plain := callWork(t, reg, builtin.ListWorkItemsTool, map[string]any{})
	if plain.Failed {
		t.Fatalf("the plain list failed: %s", plain.Output)
	}
	viaView := callWork(t, reg, builtin.ListWorkItemsTool,
		map[string]any{"view": "v-board"})
	if viaView.Failed {
		t.Fatalf("the list through the board's view failed: %s", viaView.Output)
	}
	if len(trk.query.RowFields) != 0 {
		t.Errorf("the tool read with fields=%v from the board's view — a "+
			"model's rows would carry a card's facts", trk.query.RowFields)
	}
	if viaView.Output != plain.Output {
		t.Errorf("the list through a board's view is not the plain list:\n"+
			"view:  %s\nplain: %s", viaView.Output, plain.Output)
	}
}
