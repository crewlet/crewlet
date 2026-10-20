package artifactsapi_test

import (
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/artifactsapi"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/iam"
)

const linkENG12 = `{"work_item":"ENG-12"}`

// A LINK NEEDS A READ OF THE ARTIFACT, at any role on it. A viewer links what
// they can see; a stranger is answered exactly as for an artifact nothing
// holds, before the tracker is asked anything; and an administrator with no
// relation links on the admin path, which is recorded as an elevated read and
// stamped on the record.
//
// Mutation: admit the link as `update_artifact` and the viewer is refused;
// skip the elevated read's record and the administrator's link leaves none.
func TestALinkNeedsAReadOfTheArtifact(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	bob := person("bob", iam.GrantTracker)
	w := g.do(bob, "POST", at(page, "/work-items"), linkENG12, keyed()...)
	if w.Code != http.StatusOK {
		t.Fatalf("a viewer's link: %d %s", w.Code, w.Body)
	}
	if f := fields(t, w); f["artifact"] != page.String() || f["work_item"] != "ENG-12" ||
		f["outcome"] != "applied" || f["op_id"] == "" {
		t.Errorf("the link answered %v", f)
	}
	calls := g.writes.all()
	if len(calls) != 1 || calls[0].verb != "link" || calls[0].id != page || calls[0].arg != "ENG-12" {
		t.Fatalf("the link wrote %+v", calls)
	}
	if a := calls[0].c.Authority; a.Actor.Name != "bob" || a.Role != artifacts.RoleViewer ||
		a.WorkItem != "ENG-12" || a.Origin != artifacts.OriginDashboard || a.Elevated {
		t.Errorf("the link was stamped %+v", a)
	}

	dana := person("dana", iam.GrantTracker)
	stranger := g.do(dana, "POST", at(page, "/work-items"), linkENG12, keyed()...)
	nothing := g.do(dana, "POST", at(unknown, "/work-items"), linkENG12, keyed()...)
	if stranger.Code != http.StatusNotFound || stranger.Body.String() != nothing.Body.String() {
		t.Errorf("a stranger's link is answered %d %s and an unknown artifact's %d %s",
			stranger.Code, stranger.Body, nothing.Code, nothing.Body)
	}

	ada := person("ada", iam.GrantAdmin)
	if w := g.do(ada, "POST", at(page, "/work-items"), linkENG12, keyed()...); w.Code != http.StatusOK {
		t.Fatalf("an administrator's link: %d %s", w.Code, w.Body)
	}
	if len(g.marks.elevated) != 1 {
		t.Errorf("an administrator's link recorded %d elevated reads, want 1", len(g.marks.elevated))
	}
	if calls := g.writes.all(); len(calls) != 2 || !calls[1].c.Authority.Elevated {
		t.Errorf("the administrator's link was not stamped elevated: %+v", calls)
	}
	if got := g.work.reads(); len(got) != 2 {
		t.Errorf("the tracker was asked %v, want only the two admitted links' items", got)
	}
}

// A LINK NEEDS A WRITE OF THE WORK ITEM, decided as `update_work_item` decides
// it: a caller who may read the artifact and does not use the tracker is
// refused naming the verb, and nothing is read or written.
//
// Mutation: drop mayUpdateWorkItem from the handler and the link lands.
func TestALinkNeedsAWriteOfTheWorkItem(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	w := g.do(person("bob"), "POST", at(page, "/work-items"), linkENG12, keyed()...)
	if w.Code != http.StatusForbidden || errorOf(t, w) != "unauthorized" {
		t.Fatalf("a link by a viewer without the tracker: %d %s", w.Code, w.Body)
	}
	if !strings.Contains(w.Body.String(), "update_work_item") {
		t.Errorf("the refusal does not name the verb it refused: %s", w.Body)
	}
	if calls, asked := g.writes.all(), g.work.reads(); len(calls) != 0 || len(asked) != 0 {
		t.Errorf("a refused link wrote %+v and asked the tracker %v", calls, asked)
	}
}

// A LINK NAMES AN ITEM THE TRACKER HOLDS, LIVE, AND LINKS ITS CURRENT KEY. An
// item nobody filed, a removed one, a key no tracker could have and a body
// naming none are refused, as is a trashed artifact; an id or a former key
// links under the item's key now; and a tracker this node does not run answers
// why.
func TestALinkNamesALiveWorkItemByItsKey(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	jane := person("jane", iam.GrantTracker)
	for _, c := range []struct {
		name, body string
		id         artifacts.ID
		status     int
		code       string
	}{
		{"an item nobody filed", `{"work_item":"ENG-404"}`, page, http.StatusUnprocessableEntity, "invalid"},
		{"a removed item", `{"work_item":"ENG-9"}`, page, http.StatusUnprocessableEntity, "invalid"},
		{"a key with a space", `{"work_item":"ENG 12"}`, page, http.StatusUnprocessableEntity, "invalid"},
		{"a key too long", `{"work_item":"` + strings.Repeat("E", artifacts.MaxWorkItemBytes+1) + `"}`,
			page, http.StatusUnprocessableEntity, "invalid"},
		{"no item", `{}`, page, http.StatusUnprocessableEntity, "invalid"},
		{"an unknown field", `{"work_item":"ENG-12","project":"ENG"}`, page, http.StatusBadRequest, "invalid_body"},
		{"a trashed artifact", linkENG12, binned, http.StatusConflict, "artifact_trashed"},
	} {
		if w := g.do(jane, "POST", at(c.id, "/work-items"), c.body, keyed()...); w.Code != c.status ||
			errorOf(t, w) != c.code {
			t.Errorf("%s: %d %s, want %d %s", c.name, w.Code, w.Body, c.status, c.code)
		}
	}
	if w := g.do(jane, "POST", at(page, "/work-items"), linkENG12); w.Code != http.StatusBadRequest {
		t.Errorf("a link with no Idempotency-Key: %d %s", w.Code, w.Body)
	}
	if calls := g.writes.all(); len(calls) != 0 {
		t.Fatalf("refused links wrote %+v", calls)
	}

	for _, ref := range []string{"task-12", "OPS-3"} {
		w := g.do(jane, "POST", at(page, "/work-items"), `{"work_item":"`+ref+`"}`, keyed()...)
		if w.Code != http.StatusOK || fields(t, w)["work_item"] != "ENG-12" {
			t.Errorf("a link naming %s: %d %s, want ENG-12 linked", ref, w.Code, w.Body)
		}
	}
	for _, c := range g.writes.all() {
		if c.arg != "ENG-12" || c.c.Authority.WorkItem != "ENG-12" {
			t.Errorf("a link wrote item %v stamped %q, want the current key", c.arg, c.c.Authority.WorkItem)
		}
	}

	g.work.down = artifactsapi.ErrTrackerNotUp
	if w := g.do(jane, "POST", at(page, "/work-items"), linkENG12, keyed()...); w.Code !=
		http.StatusServiceUnavailable || errorOf(t, w) != "no_active_revision" {
		t.Errorf("a link on a node with no company yet: %d %s", w.Code, w.Body)
	}
	g.work.down = artifactsapi.ErrNoTracker
	if w := g.do(jane, "POST", at(page, "/work-items"), linkENG12, keyed()...); w.Code !=
		http.StatusUnprocessableEntity || errorOf(t, w) != "invalid" {
		t.Errorf("a link in a company on another tracker: %d %s", w.Code, w.Body)
	}
	g.work.down = errors.New("the router has no data node")
	if w := g.do(jane, "POST", at(page, "/work-items"), linkENG12, keyed()...); w.Code !=
		http.StatusServiceUnavailable {
		t.Errorf("a link whose tracker could not be reached: %d %s", w.Code, w.Body)
	}
}

// THE SURFACE REFUSES TO BE BUILT WITHOUT A TRACKER: a link it could not check
// against the item would put an artifact on a card nobody filed.
func TestTheSurfaceRefusesToBeBuiltWithoutATracker(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	_, err := artifactsapi.New(artifactsapi.Options{
		Gate: artifactsapi.Gate{Heads: g.estate, Chart: chart{}, Limits: limits, Marks: g.marks,
			Roster: roster{}},
		Reads: g.estate, Writes: g.writes, Objects: g.objects, Ready: g.ready,
		Uploads: func() (artifactsapi.Pipeline, error) { return g.pipeline, nil },
	})
	if err == nil {
		t.Error("a surface with no tracker was built")
	}
}
