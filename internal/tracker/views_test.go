package tracker_test

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/tracker"
)

// A view fixture the cases vary. The container is the seeded project, which is
// the only one [newRoundTrip] has applied.
func aView(id string, mutate func(*tracker.View)) tracker.View {
	view := tracker.View{
		ID:        id,
		Name:      "My work",
		Type:      tracker.ViewList,
		Container: tracker.Container{Kind: tracker.ContainerProject, ID: "ENG"},
		Params:    map[string]string{"assignee": "ana", "sort": "-updated"},
	}
	if mutate != nil {
		mutate(&view)
	}
	return view
}

// strip reads one container's view strip as one viewer.
func (r *roundTrip) strip(container tracker.Container, viewer string) tracker.ViewListing {
	r.t.Helper()
	listing, err := r.reader.Views(r.t.Context(), tracker.ViewQuery{
		Container: container, Viewer: viewer, Level: statelog.ReadStale,
	})
	if err != nil {
		r.t.Fatalf("Views(%s %s, %q): %v", container.Kind, container.ID, viewer, err)
	}
	return listing
}

func stripKeys(l tracker.ViewListing) []string {
	out := make([]string, 0, len(l.Views))
	for _, row := range l.Views {
		out = append(out, row.Key)
	}
	return out
}

// A SAVED VIEW THAT CANNOT BE RUN IS REFUSED AT THE SAVE.
//
// Every one of these is discovered by whoever opens the view otherwise —
// weeks later, with no way to tell a typo from a grammar change — and the
// refusal a caller would have got for running the query is the refusal they
// get for saving it.
func TestAViewThatCouldNotBeRunIsRefusedAtTheSave(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	cases := []struct {
		name   string
		mutate func(*tracker.View)
		want   string
	}{
		{"no id", func(v *tracker.View) { v.ID = "" }, "names no view id"},
		{"no name", func(v *tracker.View) { v.Name = "   " }, "has no name"},
		{"a name too long for its tab", func(v *tracker.View) {
			v.Name = strings.Repeat("x", tracker.MaxViewName+1)
		}, "shorten the name"},
		{"a fourth shape", func(v *tracker.View) { v.Type = "gantt" }, "not a view shape"},
		{"a container kind that is not one", func(v *tracker.View) {
			v.Container = tracker.Container{Kind: "team", ID: "eng"}
		}, "not a container a view can belong to"},
		{"a workspace with an id", func(v *tracker.View) {
			v.Container = tracker.Container{Kind: tracker.ContainerWorkspace, ID: "ENG"}
		}, "carries no id"},
		{"a project with none", func(v *tracker.View) {
			v.Container = tracker.Container{Kind: tracker.ContainerProject}
		}, "with no id"},
		{"a stuffed parameter map", func(v *tracker.View) {
			v.Params = map[string]string{}
			for i := range tracker.MaxViewParamKeys + 1 {
				v.Params["f.field"+string(rune('a'+i%26))+string(rune('a'+i/26))] = "x"
			}
		}, "query parameters and"},
		{"a parameter nothing parses", func(v *tracker.View) {
			v.Params = map[string]string{"asignee": "ana"}
		}, "does not parse"},
		{"a value the grammar refuses", func(v *tracker.View) {
			v.Params = map[string]string{"status": "shipped"}
		}, "does not parse"},
	}
	for i, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := r.writer.WriteView(t.Context(), "op-bad-"+tc.name,
				aView("v-bad", tc.mutate))
			if err == nil {
				t.Fatalf("case %d saved a view that cannot be run", i)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("refusal %q does not say %q", err, tc.want)
			}
		})
	}

	// THE CONTROL: the same fixture, unmutated, saves. Without it every
	// case above would pass against a verb that refused everything.
	if _, err := r.writer.WriteView(t.Context(), "op-good", aView("v-good", nil)); err != nil {
		t.Fatalf("the control view was refused: %v", err)
	}
}

// ONE VIEW PER CONTAINER IS THE DEFAULT, and the applier is what makes it true.
//
// The rule is about the ROWS: a second view claiming the default has to take
// it from the first in the same transaction, or a tab strip draws two active
// tabs and a reader needs a tie-break rule nobody wrote down.
func TestOnlyOneViewInAContainerIsTheDefault(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	eng := tracker.Container{Kind: tracker.ContainerProject, ID: "ENG"}

	for _, id := range []string{"v-1", "v-2"} {
		if _, err := r.writer.WriteView(t.Context(), "op-"+id,
			aView(id, func(v *tracker.View) { v.Name = id; v.Default = true })); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
		r.drain()
	}

	var defaults []string
	for _, row := range r.strip(eng, "").Views {
		if row.Default {
			defaults = append(defaults, row.ID)
		}
	}
	if len(defaults) != 1 || defaults[0] != "v-2" {
		t.Fatalf("the default is %v, want exactly [v-2]", defaults)
	}

	// AND WITHDRAWING ONE TAKES NOTHING FROM ANYBODY ELSE: saving v-2
	// with the flag off leaves the container with no default rather than
	// handing it back to v-1, which is a gesture the person did not make.
	if _, err := r.writer.WriteView(t.Context(), "op-v-2-off",
		aView("v-2", func(v *tracker.View) { v.Name = "v-2" })); err != nil {
		t.Fatalf("withdraw the default: %v", err)
	}
	r.drain()
	for _, row := range r.strip(eng, "").Views {
		if row.Default {
			t.Fatalf("view %s is the default after the only one was withdrawn", row.ID)
		}
	}
}

// A PERSONAL VIEW IS PRIVATE TO ITS OWNER, enforced in the query rather than
// filtered afterwards.
func TestAPersonalViewReachesNobodyElsesStrip(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	eng := tracker.Container{Kind: tracker.ContainerProject, ID: "ENG"}

	for _, spec := range []struct{ id, owner string }{
		{"v-shared", ""}, {"v-ana", "ana"}, {"v-bob", "bob"},
	} {
		if _, err := r.writer.WriteView(t.Context(), "op-"+spec.id,
			aView(spec.id, func(v *tracker.View) {
				v.Name, v.Owner = spec.id, spec.owner
			})); err != nil {
			t.Fatalf("save %s: %v", spec.id, err)
		}
		r.drain()
	}

	for viewer, want := range map[string][]string{
		"ana": {"v-ana", "v-shared"},
		"bob": {"v-bob", "v-shared"},
		"":    {"v-shared"},
	} {
		var saved []string
		for _, row := range r.strip(eng, viewer).Views {
			if !row.Builtin {
				saved = append(saved, row.ID)
			}
		}
		if len(saved) != len(want) {
			t.Fatalf("viewer %q sees %v, want %v", viewer, saved, want)
		}
		for _, id := range want {
			if !containsString(saved, id) {
				t.Fatalf("viewer %q sees %v, want %v", viewer, saved, want)
			}
		}
	}
}

// A PROTECTED VIEW IS ITS OWNER'S, which is what stops a shared board being
// rearranged under everybody.
func TestAProtectedViewRefusesEveryoneButItsOwner(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	// Written by ana, whom the harness's own writer is.
	if _, err := r.writer.WriteView(t.Context(), "op-guarded",
		aView("v-guarded", func(v *tracker.View) {
			v.Owner, v.Protected = "ana", true
		})); err != nil {
		t.Fatalf("save the protected view: %v", err)
	}
	r.drain()

	bob := r.writer.As("bob", tracker.AuthorHuman, tracker.Provenance{})
	_, err := bob.WriteView(t.Context(), "op-guarded-bob",
		aView("v-guarded", func(v *tracker.View) {
			v.Owner, v.Protected, v.Name = "ana", true, "bob was here"
		}))
	if err == nil {
		t.Fatal("bob edited a view protected for ana")
	}
	if !errors.Is(err, statelog.ErrConflict) {
		t.Fatalf("the refusal is %v, want an ErrConflict a caller can branch on", err)
	}
	if !strings.Contains(err.Error(), "ana") {
		t.Fatalf("the refusal %q does not name whom to ask", err)
	}

	// ITS OWNER STILL MAY, or "protected" would mean frozen.
	if _, err := r.writer.WriteView(t.Context(), "op-guarded-ana",
		aView("v-guarded", func(v *tracker.View) {
			v.Owner, v.Protected, v.Name = "ana", true, "ana renamed it"
		})); err != nil {
		t.Fatalf("ana could not edit her own protected view: %v", err)
	}
}

// EVERY CONTAINER HAS THREE VIEWS WITHOUT ANYBODY SAVING ONE, and a sprinting
// project has five.
//
// That is what makes "required views" moot: a fresh project needs no setup
// gesture and nothing has to guard against somebody deleting the last view.
func TestAContainerHasItsViewsBeforeAnybodySavesOne(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	three := []string{tracker.ViewKeyList, tracker.ViewKeyBoard, tracker.ViewKeyCalendar}
	for _, container := range []tracker.Container{
		{Kind: tracker.ContainerWorkspace},
		{Kind: tracker.ContainerProject, ID: "ENG"},
		{Kind: tracker.ContainerUnit, ID: "engineering"},
		{Kind: tracker.ContainerPerson, ID: "ana"},
	} {
		got := stripKeys(r.strip(container, ""))
		if len(got) != len(three) {
			t.Fatalf("%s %s has %v, want %v", container.Kind, container.ID, got, three)
		}
		for i, key := range three {
			if got[i] != key {
				t.Fatalf("%s %s has %v, want %v", container.Kind, container.ID, got, three)
			}
		}
		// AND EVERY IMPLICIT ROW SAYS SO, because a caller renders it
		// from its key rather than editing, protecting or ranking it.
		for _, row := range r.strip(container, "").Views {
			if !row.Builtin || row.ID != "" {
				t.Fatalf("%s is not marked implicit: builtin=%v id=%q",
					row.Key, row.Builtin, row.ID)
			}
		}
	}

	// A SPRINT POLICY ADDS TWO, and only where there is one.
	if _, err := r.writer.WriteDocument(t.Context(), "op-sprinting",
		tracker.ProjectSubject("ENG"), "", tracker.Project{
			V: 1, Key: "ENG", Name: "Engineering",
			Sprints:   &tracker.SprintPolicy{},
			CreatedAt: wednesday, UpdatedAt: wednesday,
		}, tracker.ChangeProjectCreated, nil); err != nil {
		t.Fatalf("turn sprints on: %v", err)
	}
	r.drain()

	got := stripKeys(r.strip(tracker.Container{
		Kind: tracker.ContainerProject, ID: "ENG"}, ""))
	for _, key := range []string{tracker.ViewKeySprint, tracker.ViewKeyBacklog} {
		if !containsString(got, key) {
			t.Fatalf("a sprinting project's strip is %v, want %s in it", got, key)
		}
	}
	// AND THE WORKSPACE STILL HAS THREE: a project's policy is not the
	// company's.
	if got := stripKeys(r.strip(tracker.Container{
		Kind: tracker.ContainerWorkspace}, "")); len(got) != 3 {
		t.Fatalf("the workspace strip grew with a project's policy: %v", got)
	}
}

// EVERY IMPLICIT VIEW'S OWN PARAMETERS PARSE.
//
// The five are rendered from this package rather than saved, so [checkView]
// never sees them — which is exactly why they need their own case: a tab that
// opens on a parse error is the failure the save-time parse exists to prevent,
// and the implicit ones are the tabs every company meets first.
func TestEveryImplicitViewsQueryParses(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	if _, err := r.writer.WriteDocument(t.Context(), "op-sprinting",
		tracker.ProjectSubject("ENG"), "", tracker.Project{
			V: 1, Key: "ENG", Name: "Engineering",
			Sprints:   &tracker.SprintPolicy{},
			CreatedAt: wednesday, UpdatedAt: wednesday,
		}, tracker.ChangeProjectCreated, nil); err != nil {
		t.Fatalf("turn sprints on: %v", err)
	}
	r.drain()

	strip := r.strip(tracker.Container{Kind: tracker.ContainerProject, ID: "ENG"}, "")
	if len(strip.Views) != 5 {
		t.Fatalf("a sprinting project has %d views, want 5", len(strip.Views))
	}
	for _, row := range strip.Views {
		params := make(tracker.MapParams, len(row.Params)+1)
		for key, value := range row.Params {
			params[key] = value
		}
		params["container"] = "project:ENG"
		if _, err := tracker.ParseQuery(params, wednesday, time.UTC); err != nil {
			t.Fatalf("the %s tab's own query does not parse: %v", row.Key, err)
		}
	}
}

// A VIEW'S CREATION FACTS ARE THE STORED ROW'S, and its rank is too.
//
// A save that carried either would let a second writer re-attribute a view
// somebody else made, or drag a shared strip's arrangement by editing one tab.
func TestASaveCannotReattributeOrRerankAView(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	eng := tracker.Container{Kind: tracker.ContainerProject, ID: "ENG"}

	if _, err := r.writer.WriteView(t.Context(), "op-first", aView("v-1", nil)); err != nil {
		t.Fatalf("save: %v", err)
	}
	r.drain()
	before := r.strip(eng, "").Views

	bob := r.writer.As("bob", tracker.AuthorHuman, tracker.Provenance{})
	if _, err := bob.WriteView(t.Context(), "op-second",
		aView("v-1", func(v *tracker.View) {
			v.Name, v.CreatedBy = "bob renamed it", "bob"
			v.CreatedAt = wednesday.Add(time.Hour)
			v.Rank = tracker.Rank("a0")
		})); err != nil {
		t.Fatalf("bob's save: %v", err)
	}
	r.drain()

	after := r.strip(eng, "").Views
	if len(before) != len(after) {
		t.Fatalf("the strip is %d rows, was %d", len(after), len(before))
	}
	var got tracker.ViewRow
	for _, row := range after {
		if row.ID == "v-1" {
			got = row
		}
	}
	if got.Name != "bob renamed it" {
		t.Fatalf("the rename did not land: %q", got.Name)
	}
	// THE RANK IS THE STRIP'S, so a save carrying one it did not mint
	// leaves the arrangement where it was.
	var want tracker.Rank
	for _, row := range before {
		if row.ID == "v-1" {
			want = row.Rank
		}
	}
	if got.Rank != want {
		t.Fatalf("the rank moved to %q, was %q", got.Rank, want)
	}
}

// A NEW VIEW LANDS AT THE END OF THE STRIP, never in front of whatever the
// person arranged.
func TestANewViewLandsAtTheEndOfTheStrip(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	eng := tracker.Container{Kind: tracker.ContainerProject, ID: "ENG"}

	for _, id := range []string{"v-1", "v-2", "v-3"} {
		if _, err := r.writer.WriteView(t.Context(), "op-"+id,
			aView(id, func(v *tracker.View) { v.Name = id })); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
		r.drain()
	}

	var saved []string
	var last tracker.Rank
	for _, row := range r.strip(eng, "").Views {
		if row.Builtin {
			continue
		}
		if row.Rank <= last {
			t.Fatalf("view %s ranks %q, which is not after %q", row.ID, row.Rank, last)
		}
		last = row.Rank
		saved = append(saved, row.ID)
	}
	want := []string{"v-1", "v-2", "v-3"}
	for i, id := range want {
		if i >= len(saved) || saved[i] != id {
			t.Fatalf("the strip is %v, want %v", saved, want)
		}
	}
}

// A VIEWER'S PINS COME FIRST FOR THEM AND NOBODY ELSE.
//
// Two orders, concatenated rather than sorted together: within each half the
// rank is the arrangement somebody made, and one sort over a pinned flag would
// silently re-rank the shared strip for one reader.
func TestAPinOrdersOneViewersStripAndNoOthers(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)
	eng := tracker.Container{Kind: tracker.ContainerProject, ID: "ENG"}

	for _, id := range []string{"v-1", "v-2", "v-3"} {
		if _, err := r.writer.WriteView(t.Context(), "op-"+id,
			aView(id, func(v *tracker.View) { v.Name = id })); err != nil {
			t.Fatalf("save %s: %v", id, err)
		}
		r.drain()
	}
	if _, err := r.writer.WriteDocument(t.Context(), "op-ana",
		tracker.PersonSubject("ana"), "", tracker.Person{
			V: 1, Handle: "ana", PinnedViews: []string{"v-3"},
		}, tracker.ChangePersonUpdated, nil); err != nil {
		t.Fatalf("pin v-3 for ana: %v", err)
	}
	r.drain()

	var ana []string
	for _, row := range r.strip(eng, "ana").Views {
		if !row.Builtin {
			if row.ID == "v-3" && !row.Pinned {
				t.Fatal("ana's pinned view does not say it is pinned")
			}
			ana = append(ana, row.ID)
		}
	}
	if len(ana) == 0 || ana[0] != "v-3" {
		t.Fatalf("ana's strip is %v, want the pin first", ana)
	}

	var bob []string
	for _, row := range r.strip(eng, "bob").Views {
		if !row.Builtin {
			if row.Pinned {
				t.Fatalf("view %s is pinned for bob, and the pin is ana's", row.ID)
			}
			bob = append(bob, row.ID)
		}
	}
	if len(bob) == 0 || bob[0] != "v-1" {
		t.Fatalf("bob's strip is %v, want the rank order", bob)
	}
}

// A STRIP READ NAMES A CONTAINER THE WRITE COULD HAVE PRODUCED.
//
// The spellings a save refuses come back as an EMPTY strip otherwise, which
// reads exactly like a container nobody has saved a view in.
func TestAViewReadRefusesTheSpellingsTheWriteRefuses(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	for _, tc := range []struct {
		name      string
		container tracker.Container
		want      string
	}{
		{"no kind", tracker.Container{}, "not a container"},
		{"a kind that is not one", tracker.Container{Kind: "team", ID: "eng"}, "not a container"},
		{"a workspace with an id", tracker.Container{
			Kind: tracker.ContainerWorkspace, ID: "ENG"}, "carries no id"},
		{"a project with none", tracker.Container{
			Kind: tracker.ContainerProject}, "with no id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := r.reader.Views(t.Context(), tracker.ViewQuery{
				Container: tc.container, Level: statelog.ReadStale,
			})
			if err == nil {
				t.Fatal("a strip came back for a container no write could produce")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("the refusal %q does not say %q", err, tc.want)
			}
		})
	}

	// AND A READ WITH NO LEVEL IS THE FOURTH STATE THIS PACKAGE REFUSES:
	// a surface resolves its own default before it reads.
	if _, err := r.reader.Views(t.Context(), tracker.ViewQuery{
		Container: tracker.Container{Kind: tracker.ContainerWorkspace},
	}); err == nil {
		t.Fatal("a view read with no read level was answered")
	}
}

func containsString(list []string, want string) bool {
	for _, got := range list {
		if got == want {
			return true
		}
	}
	return false
}

// A VIEW THAT MOVES NAMES THE STRIP IT LEAVES.
//
// A save may change a view's container, and then the apply writes the row OUT
// of one strip and INTO another. A scope naming only the destination let a
// write into the strip it left slip past a deferral that covers it — the rule
// Writer.UpdateTask states verbatim for a project move, said about a view.
func TestAViewThatMovesNamesBothStrips(t *testing.T) {
	t.Parallel()
	r := newRoundTrip(t)

	view := aView("v-move", nil)
	view.Container = tracker.Container{Kind: tracker.ContainerProject, ID: "ENG"}
	if _, err := r.writer.WriteView(t.Context(), "op-here", view); err != nil {
		t.Fatalf("save: %v", err)
	}
	r.drain()

	view.Container = tracker.Container{Kind: tracker.ContainerProject, ID: "OPS"}
	if _, err := r.writer.WriteView(t.Context(), "op-there", view); err != nil {
		t.Fatalf("move: %v", err)
	}
	r.drain()

	scope := r.scopeOfLastRecord()
	for _, want := range []string{"ENG", "OPS"} {
		term := tracker.ScopeTerm{Kind: tracker.TermContainer, ID: want}.Path()
		// THE CONTAINER COVERS THE TERM, not the other way round: the
		// record names the OBJECT inside each strip, which is the
		// narrowest honest term, and a read scoped to the strip
		// intersects it because the object's path nests under it.
		var found bool
		for _, path := range scope.Paths {
			if statelog.Covers(term, path) {
				found = true
			}
		}
		if !found {
			t.Errorf("the move carries the scope %v, none of which sits under "+
				"%s — a read of that strip would never wait for this record",
				scope.Paths, term)
		}
	}
}
