package builtin_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A SAVED VIEW'S PARAMETERS ARE THE GRAMMAR'S, AND save_work_view SAYS SO.
//
// The stored map is what [tracker.ParseQuery] validates and what the expansion
// reads back, so it is the GRAMMAR's vocabulary — while list_work_items'
// arguments are the model's. The description said the opposite, so a model
// saving the query it had just run wrote this surface's four renamed
// arguments into a view and had every one of them refused as not a query
// parameter at all.
//
// The renames are a table now, and this is what makes it load-bearing: each
// entry has to be an argument this tool declares, a key the grammar accepts,
// AND the translation the tool actually performs.
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
			if !slices.Contains(tracker.QueryKeys, tc.key) {
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

// AND A SAVED VIEW CAN BE RUN, which is what list_work_views tells a model to
// do.
//
// The grammar has expanded `view` since it was written; this tool did not
// declare it, so the only way a seat could run somebody's saved view was to
// paste its parameters — in the grammar's names, which this tool forwards none
// of. The advice named no reachable gesture at all.
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

// A REFUSED `container` CARRIES THE VOCABULARY IT WOULD HAVE ACCEPTED.
//
// `container` is the one required argument of both view tools, and its grammar
// is the TRACKER's rather than this surface's — so a caller reaching for the
// project key it has been using all along (`ENG`) has nothing to derive
// `project:ENG` from unless the refusal spells it out. A refusal that only
// says no is one the next attempt repeats verbatim, which is why the four
// spellings travel IN it rather than only in a schema description already read
// past.
//
// The accepting case is here for the same reason: a parser that refused
// everything would satisfy every assertion above it.
func TestARefusedContainerNamesTheSpellingsItAccepts(t *testing.T) {
	t.Parallel()
	reg := operatorRegistry(t, newFakeTracker(), &personSpy{})
	for _, raw := range []string{"", "ENG", "project:", "workspace:ENG", "team:eng"} {
		t.Run("refused/"+raw, func(t *testing.T) {
			t.Parallel()
			got := callPlain(t, reg, tracker.ListWorkViewsTool,
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
		if got := callPlain(t, reg, tracker.ListWorkViewsTool,
			map[string]any{"container": "project:eng"}); got.Failed {
			t.Fatalf("project:eng was refused: %s", got.Output)
		}
	})
}

// THE SHAPES THE TOOL OFFERS ARE THE SHAPES THE ENGINE TAKES.
//
// `save_work_view`'s `type` enum and [tracker.ViewType.Valid] are two lists of
// the same set, written in two files, with nothing holding them together. They
// had already drifted: the enum offered three where the validator accepts
// four, so a seat could not save a `timeline` view although the write would
// have taken one, the refusal names it as one of the four, and the tracker
// ships a BUILTIN timeline that every reader can already see. The shape was
// reachable by reading and unreachable by writing, and the tool's own schema
// was the only thing claiming otherwise.
//
// A MODEL CANNOT DISCOVER WHAT THE ENUM OMITS. An enum is a closed set to
// whatever is reading it, so the omission is not a hint the model can work
// around — it is the whole of what that model believes the surface accepts.
//
// Checked in BOTH directions: an enum naming a shape the engine refuses would
// be the same defect the other way round, and it fails at the write instead of
// at the schema, which is worse.
func TestTheViewShapesTheToolOffersAreTheOnesTheEngineTakes(t *testing.T) {
	t.Parallel()

	// THE OPERATOR SURFACE, because that is the only registry this tool is in:
	// a saved view is furniture a person arranges, and no seat is given it.
	// Built from the real constructor rather than a literal, so a tool that
	// stops registering fails the guard below instead of passing silently.
	work := newFakeTracker()
	var offered []string
	for _, tool := range builtin.OperatorTools(builtin.OperatorDeps{
		Work: builtin.WorkDeps{
			Reader:     work,
			Writer:     work.as,
			ViewWriter: func(builtin.Actor) builtin.ViewWriter { return nil },
		},
	}) {
		if tool.Name() != tracker.SaveWorkViewTool {
			continue
		}
		props, _ := tool.Parameters()["properties"].(map[string]any)
		shape, _ := props["type"].(map[string]any)
		for _, v := range shape["enum"].([]string) {
			offered = append(offered, v)
		}
	}
	if len(offered) == 0 {
		t.Fatal("save_work_view declares no view shapes at all, so this gate certifies nothing")
	}

	for _, name := range offered {
		if !tracker.ViewType(name).Valid() {
			t.Errorf("the tool offers the shape %q and the engine refuses it at "+
				"the write — a model is told to send something that cannot land", name)
		}
	}
	// AND EVERY SHAPE THE ENGINE TAKES IS OFFERED. This is the direction the
	// drift actually went.
	for _, want := range []tracker.ViewType{
		tracker.ViewList, tracker.ViewBoard, tracker.ViewCalendar, tracker.ViewTimeline,
	} {
		if !slices.Contains(offered, string(want)) {
			t.Errorf("the engine accepts the shape %q and the tool does not offer it — "+
				"an enum is a closed set to the model reading it, so this shape is "+
				"unreachable however the model is asked", want)
		}
	}
}
