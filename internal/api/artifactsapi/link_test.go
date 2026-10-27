package artifactsapi_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/artifactsapi"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifactsdomain"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/worklet"
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
	bob := person("bob", iam.GrantWorklet)
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

	dana := person("dana", iam.GrantWorklet)
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
// it: a caller who may read the artifact and does not use Worklet is
// refused naming the verb as `action`, and nothing is read or written.
//
// Mutation: drop mayUpdateWorkItem from the handler and the link lands.
func TestALinkNeedsAWriteOfTheWorkItem(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	w := g.do(person("bob"), "POST", at(page, "/work-items"), linkENG12, keyed()...)
	if w.Code != http.StatusForbidden || errorOf(t, w) != "unauthorized" {
		t.Fatalf("a link by a viewer without Worklet: %d %s", w.Code, w.Body)
	}
	var refusal map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &refusal); err != nil ||
		refusal[operator.KeyAction] != string(authz.ActionWorkUpdate) {

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
	jane := person("jane", iam.GrantWorklet)
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

	g.work.down = artifactsapi.ErrWorkletNotUp
	if w := g.do(jane, "POST", at(page, "/work-items"), linkENG12, keyed()...); w.Code !=
		http.StatusServiceUnavailable || errorOf(t, w) != "no_active_revision" {
		t.Errorf("a link on a node with no company yet: %d %s", w.Code, w.Body)
	}
	g.work.down = artifactsapi.ErrNoWorklet
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

// trackerAt is a position on the tracker's own log.
func trackerAt(seq uint64) statelog.Position {
	return statelog.Position{Stream: worklet.Domain{}.Stream().Name, Generation: 1, Seq: seq}
}

// linkAfter is a link body naming ENG-12 and the position its create landed at.
func linkAfter(after string) string {
	return `{"work_item":"ENG-12","after":"` + after + `"}`
}

// A LINK SENT RIGHT AFTER THE ITEM WAS FILED WAITS FOR THE CREATE. On a node
// that has applied the tracker's log through 8, a link naming the create at 9
// is held at the tracker's own floor rather than answered from a node that
// cannot see the item yet; nothing is written while it is held, and once the
// node applies 9 the link lands. A link without `after` on the same node reads
// at the dashboard's level with no floor, and is not held.
//
// Mutation: drop the floor from the read (pass decisionRead whatever the body
// says) and the link answers before the create is applied, so the held read
// is never seen; drop `after` from the body and the link is refused
// `invalid_body`.
func TestALinkAfterItsCreateWaitsForTheTrackerToApplyIt(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	applied := g.work.behind(t, trackerAt(8))
	jane := person("jane", iam.GrantWorklet)

	if w := g.do(jane, "POST", at(page, "/work-items"), linkENG12, keyed()...); w.Code != http.StatusOK {
		t.Fatalf("a link with no after on a node behind: %d %s", w.Code, w.Body)
	}
	if got := g.work.floors(); len(got) != 1 || !got[0].MinPosition.IsZero() ||
		got[0].Level != statelog.DefaultReadLevel(statelog.SurfaceDashboard) {
		t.Fatalf("a link with no after read the tracker at %+v, want the dashboard's level and no floor", got)
	}

	created := trackerAt(9)
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		answered <- g.do(jane, "POST", at(page, "/work-items"), linkAfter(created.String()), keyed()...)
	}()
	select {
	case held := <-applied.waiting:
		if held != created {
			t.Fatalf("the link's read waited for %v, want the create's %v", held, created)
		}
	case w := <-answered:
		t.Fatalf("the link answered %d %s before the tracker applied the create", w.Code, w.Body)
	case <-time.After(statelog.ReadBudget):
		t.Fatal("the link's read never waited for the create")
	}
	if calls := g.writes.all(); len(calls) != 1 {
		t.Fatalf("a link held at the tracker's floor wrote %+v", calls[1:])
	}

	applied.advance(created)
	w := <-answered
	if w.Code != http.StatusOK || fields(t, w)["work_item"] != "ENG-12" {
		t.Fatalf("the link once the create was applied: %d %s", w.Code, w.Body)
	}
	if calls := g.writes.all(); len(calls) != 2 || calls[1].arg != "ENG-12" {
		t.Fatalf("the held link wrote %+v", calls)
	}
	if got := g.work.floors(); len(got) != 2 || got[1].MinPosition != created {
		t.Errorf("the held link read the tracker at %+v, want floored at %v", got, created)
	}
}

// A CREATE THIS NODE NEVER APPLIES IS THE TRACKER'S OWN `behind`. The read
// waits out the read budget and is refused as every tracker read floored past
// this node is: `503 unavailable`, naming the refusal, with its Retry-After,
// and nothing is linked.
//
// Mutation: answer the refusal with this surface's fixed sentence and the
// refusal's code is lost; drop the floor and the link lands on an item the
// node cannot vouch was filed after the caller's create.
func TestALinkWhoseCreateNeverAppliesIsRefusedBehind(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	g.work.behind(t, trackerAt(8))
	jane := person("jane", iam.GrantWorklet)

	began := time.Now()
	w := g.do(jane, "POST", at(page, "/work-items"), linkAfter(trackerAt(50).String()), keyed()...)
	if w.Code != http.StatusServiceUnavailable || errorOf(t, w) != "unavailable" {
		t.Fatalf("a link whose create never applied: %d %s", w.Code, w.Body)
	}
	if f := fields(t, w); f["refusal"] != string(statelog.RefuseBehind) ||
		!strings.Contains(f["detail"].(string), trackerAt(50).String()) {
		t.Errorf("the refusal says %v, want `behind` naming the position", f)
	}
	if w.Header().Get("Retry-After") == "" {
		t.Errorf("a `behind` refusal carries no Retry-After: %v", w.Header())
	}
	if waited := time.Since(began); waited < statelog.ReadBudget {
		t.Errorf("the link was refused after %v, before the read budget of %v", waited, statelog.ReadBudget)
	}
	if calls := g.writes.all(); len(calls) != 0 {
		t.Fatalf("a link refused behind wrote %+v", calls)
	}
}

// AN `after` THAT IS NOT A TRACKER POSITION IS THE REQUEST'S MISTAKE. One that
// is not a position at all is refused before the tracker is asked; one on
// another domain's log is refused by the tracker's own reader before it waits
// for anything, because every node would refuse it the same however long
// anybody waited. Both are `422 invalid`, and nothing is linked.
//
// Mutation: read a malformed `after` as no floor and the link lands; answer a
// position on another log as a refusal of this node and it is a `503`.
func TestALinkRefusesAnAfterThatIsNotATrackerPosition(t *testing.T) {
	t.Parallel()
	g := newRig(t, true)
	g.work.behind(t, trackerAt(8))
	jane := person("jane", iam.GrantWorklet)

	for _, after := range []string{"nine", "CREWLET_TRACKER_LOG@1", "CREWLET_TRACKER_LOG@x:9", "@1:9"} {
		w := g.do(jane, "POST", at(page, "/work-items"), linkAfter(after), keyed()...)
		if w.Code != http.StatusUnprocessableEntity || errorOf(t, w) != "invalid" ||
			!strings.Contains(w.Body.String(), "not a log position") {
			t.Errorf("a link after %q: %d %s", after, w.Code, w.Body)
		}
	}
	if asked := g.work.reads(); len(asked) != 0 {
		t.Errorf("a malformed after still asked the tracker %v", asked)
	}

	// THE ARTIFACT'S OWN POSITION is the likely mix-up: a create answers one
	// on the artifacts log, and it means nothing on the tracker's.
	foreign := statelog.Position{Stream: artifactsdomain.Domain{}.Stream().Name, Generation: 1, Seq: 50}
	began := time.Now()
	w := g.do(jane, "POST", at(page, "/work-items"), linkAfter(foreign.String()), keyed()...)
	if w.Code != http.StatusUnprocessableEntity || errorOf(t, w) != "invalid" ||
		!strings.Contains(w.Body.String(), worklet.Domain{}.Stream().Name) {
		t.Errorf("a link after a position on another log: %d %s", w.Code, w.Body)
	}
	if waited := time.Since(began); waited >= statelog.ReadBudget {
		t.Errorf("a position on another log was waited for (%v) before it was refused", waited)
	}
	if calls := g.writes.all(); len(calls) != 0 {
		t.Fatalf("refused links wrote %+v", calls)
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
