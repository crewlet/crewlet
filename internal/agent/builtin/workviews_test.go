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
