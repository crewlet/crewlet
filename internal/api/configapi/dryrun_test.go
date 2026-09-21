package configapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/store"
)

// A dry run is a write that stores, activates and publishes nothing. These
// cases hold it to that with fakes that COUNT the four calls a write makes,
// wrapped around the real store and plane so everything else a request does is
// exactly what production runs.

// countingRevisions counts every revision stored, and every one this node
// marked as its active revision.
type countingRevisions struct {
	configapi.RevisionStore
	inserts      atomic.Int32
	markedActive atomic.Int32
}

func (c *countingRevisions) Insert(ctx context.Context, r store.Revision) (string, error) {
	c.inserts.Add(1)
	return c.RevisionStore.Insert(ctx, r)
}

func (c *countingRevisions) Activate(ctx context.Context, revisionID string, at time.Time) (string, error) {
	c.markedActive.Add(1)
	return c.RevisionStore.Activate(ctx, revisionID, at)
}

// countingPlane counts every activation.
type countingPlane struct {
	coord.Plane
	activations atomic.Int32
}

func (c *countingPlane) Activate(ctx context.Context, req coord.ActivationRequest) (coord.Activation, error) {
	c.activations.Add(1)
	return c.Plane.Activate(ctx, req)
}

// countingQueue counts every event published.
type countingQueue struct{ publishes atomic.Int32 }

func (c *countingQueue) Publish(context.Context, string, *events.Event) error {
	c.publishes.Add(1)
	return nil
}

// counted is a surface with its three write paths counted.
type counted struct {
	*surface
	revisions *countingRevisions
	plane     *countingPlane
	queue     *countingQueue
}

func newCountedSurface(t *testing.T) *counted {
	t.Helper()
	c := &counted{
		revisions: &countingRevisions{},
		plane:     &countingPlane{Plane: coordmemory.NewFleet()},
		queue:     &countingQueue{},
	}
	c.surface = newSurfaceWith(t, func(o *configapi.Options) {
		o.Plane, o.Queue = c.plane, c.queue
	})
	c.svc.WrapRevisions(func(real configapi.RevisionStore) configapi.RevisionStore {
		c.revisions.RevisionStore = real
		return c.revisions
	})
	return c
}

// forget zeroes the counts, for a case that had to set the fleet up through
// the same plane it counts.
func (c *counted) forget() {
	c.revisions.inserts.Store(0)
	c.revisions.markedActive.Store(0)
	c.plane.activations.Store(0)
	c.queue.publishes.Store(0)
}

// writeCounts is what a surface has written: revisions stored, revisions this
// node marked active, fleet activations, and events published.
type writeCounts struct{ stored, markedActive, activated, published int32 }

// oneWrite is exactly what a write that lands makes.
var oneWrite = writeCounts{1, 1, 1, 1}

// writes is how many of each the surface has made.
func (c *counted) writes() writeCounts {
	return writeCounts{
		c.revisions.inserts.Load(), c.revisions.markedActive.Load(),
		c.plane.activations.Load(), c.queue.publishes.Load(),
	}
}

// A DRY RUN STORES, ACTIVATES AND PUBLISHES NOTHING, valid or not.
//
// The one property of a dry run that must never regress, because the dashboard
// sends one on every edit: a check that stored would mint a revision and an
// epoch per keystroke, and every node would re-apply the company each time.
// Each case is a request that reaches the point where a write would store, or
// the refusal that stops it before; the history is compared as well as the
// counts, so a store reached some other way is caught too.
func TestADryRunStoresActivatesAndPublishesNothing(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, method, body string
		seeded             bool
		want               int
	}{
		{"a valid put", http.MethodPut, companyDoc, true, http.StatusOK},
		{"a valid put creating the company", http.MethodPut, companyJSONDoc, false, http.StatusOK},
		{"a put the validator refuses", http.MethodPut, strings.Replace(companyDoc, "llm: zulu", "llm: nowhere", 1), true, http.StatusBadRequest},
		{"a put that does not parse", http.MethodPut, "name: [", true, http.StatusBadRequest},
		{"a valid patch", http.MethodPatch, `{"mission": "checked"}`, true, http.StatusOK},
		{"a patch the validator refuses", http.MethodPatch, `{"roles": [{"name": "CEO", "llm": "nowhere"}]}`, true, http.StatusBadRequest},
		{"a patch naming an unknown key", http.MethodPatch, `{"missionn": "typo"}`, true, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newCountedSurface(t)
			if tc.seeded {
				s.seed(t, companyDoc, nil)
			}
			before := s.do(t, http.MethodGet, "/config/revisions", "", nil).Body.String()

			res := s.do(t, tc.method, "/config?dry_run=true", tc.body, nil)
			if res.Code != tc.want {
				t.Fatalf("%s dry run = %d, want %d: %s", tc.method, res.Code, tc.want, res.Body)
			}
			if got := s.writes(); got != (writeCounts{}) {
				t.Errorf("a dry run wrote %+v, want nothing", got)
			}
			if after := s.do(t, http.MethodGet, "/config/revisions", "", nil).Body.String(); after != before {
				t.Errorf("a dry run changed the history:\nbefore %s\nafter  %s", before, after)
			}
			if _, found, err := s.plane.Target(t.Context()); err != nil || found {
				t.Errorf("a dry run moved the fleet pointer (found=%v err=%v)", found, err)
			}
		})
	}

	// AND THE FAKES CAN COUNT. The same valid requests without the
	// parameter make exactly one of each, or every zero above proves nothing.
	for _, method := range []string{http.MethodPut, http.MethodPatch} {
		s := newCountedSurface(t)
		s.seed(t, companyDoc, nil)
		body := companyDoc
		if method == http.MethodPatch {
			body = `{"mission": "written"}`
		}
		res := s.do(t, method, "/config", body, summaryHeader)
		if res.Code != http.StatusCreated {
			t.Fatalf("%s write = %d, want 201: %s", method, res.Code, res.Body)
		}
		if got := s.writes(); got != oneWrite {
			t.Errorf("a %s write counted %+v, want one of each", method, got)
		}
	}
}

// THE ANSWER SAYS THE WRITE IS VALID, WHAT IT WAS CHECKED AGAINST, AND WHAT IT
// WOULD PRODUCE.
//
// base_revision_id is how a client learns that the revision moved between its
// read and its check without a second request; the warnings and the derived
// hierarchy are what a builder draws before the operator saves.
func TestADryRunAnswersTheBaseTheWarningsAndTheDerivedHierarchy(t *testing.T) {
	t.Parallel()
	s := newCountedSurface(t)
	base := s.seed(t, companyDoc, nil)

	// `manages` carries a seat's HANDLE or a unit's KEY, so the live entry
	// is `cto` and the dangling one is a well-formed handle nothing answers
	// to. WELL-FORMED MATTERS: a value shaped like a display name can never
	// resolve under any chart and is refused outright, so a fixture written
	// that way would be a 400 rather than the dangling-reference warning
	// this case is about.
	res := s.do(t, http.MethodPatch, "/config?dry_run=true",
		`{"roles": [{"name": "CEO", "handle": "ceo", "llm": "zulu", "manages": ["cto", "ghost"]},
		            {"name": "CTO", "handle": "cto", "llm": "zulu"}]}`, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("dry run = %d, want 200: %s", res.Code, res.Body)
	}
	var answer struct {
		Valid    bool             `json:"valid"`
		Base     string           `json:"base_revision_id"`
		Warnings []config.Warning `json:"warnings"`
		Derived  config.Derived   `json:"derived"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &answer); err != nil {
		t.Fatalf("decode: %v (%s)", err, res.Body)
	}
	if !answer.Valid || answer.Base != base {
		t.Errorf("valid = %v, base = %q, want true and %q", answer.Valid, answer.Base, base)
	}
	if len(answer.Warnings) != 1 || answer.Warnings[0].Kind != config.WarningDanglingReference ||
		answer.Warnings[0].Path != "roles[0].manages[1]" || answer.Warnings[0].Seat != "ceo" {
		t.Errorf("warnings = %+v, want the dangling manages entry, located", answer.Warnings)
	}
	if len(answer.Derived.Seats) != 2 || answer.Derived.Seats[1].Manager != "ceo" ||
		answer.Derived.Seats[1].Path != "roles[1]" {
		t.Errorf("derived = %+v, want the CTO reporting to the CEO, with its path", answer.Derived)
	}

	// Creating the company is checked against nothing, and says so.
	empty := newCountedSurface(t)
	created := empty.do(t, http.MethodPut, "/config?dry_run=true", companyJSONDoc,
		map[string]string{"If-None-Match": "*"})
	if created.Code != http.StatusOK {
		t.Fatalf("create dry run = %d, want 200: %s", created.Code, created.Body)
	}
	body := decode(t, created)
	if got, present := body["base_revision_id"]; !present || got != "" {
		t.Errorf("base_revision_id = %v (present=%v), want the empty string", got, present)
	}
	if warnings, ok := body["warnings"].([]any); !ok || len(warnings) != 0 {
		t.Errorf("warnings = %v, want an empty list rather than null", body["warnings"])
	}
}

// A CHECK OF A DRAFT THAT CHANGES NOTHING IS VALID.
//
// The first thing the dashboard's builder sends on every load is a check of a
// draft with no operations in it, which is an empty merge patch: it is how the
// lens gets the warnings and the hierarchy of the company as it stands. An
// empty patch object is therefore not the empty BODY a write refuses, and a
// check that answered invalid_patch would open the builder on a problem
// nobody caused.
func TestACheckOfADraftWithNoChangesIsValid(t *testing.T) {
	t.Parallel()
	s := newCountedSurface(t)
	s.seed(t, companyDoc, nil)

	res := s.do(t, http.MethodPatch, "/config?dry_run=true", `{}`, nil)
	if res.Code != http.StatusOK {
		t.Fatalf("a check of an empty patch = %d, want 200: %s", res.Code, res.Body)
	}
	if derived, ok := decode(t, res)["derived"].(map[string]any); !ok || derived["seats"] == nil {
		t.Errorf("the check carries no hierarchy for the company as it stands: %s", res.Body)
	}
	if got := s.writes(); got != (writeCounts{}) {
		t.Errorf("a check wrote %+v", got)
	}
	// A body with nothing in it at all is still a patch that says nothing,
	// and a write of it would mint an epoch every node reconciles onto.
	empty := s.do(t, http.MethodPatch, "/config?dry_run=true", "", nil)
	if empty.Code != http.StatusBadRequest || decode(t, empty)["error"] != "invalid_patch" {
		t.Errorf("a check of an empty body = %d %s, want 400 invalid_patch", empty.Code, empty.Body)
	}
}

// DRY_RUN IS READ FIRST, AND ONLY TRUE OR FALSE.
//
// A mistyped parameter must be reported as the parameter: guessed one way it
// stores what the caller meant to check, guessed the other it stores nothing
// and says so. Read before the media type, the body and the summary, so it is
// the refusal a caller with several mistakes sees first.
func TestTheDryRunParameterIsReadFirstAndOnlyTrueOrFalse(t *testing.T) {
	t.Parallel()
	s := newCountedSurface(t)
	s.seed(t, companyDoc, nil)

	for _, query := range []string{"dry_run=yes", "dry_run=1", "dry_run=", "dry_run", "dry_run=TRUE", "dry_run=true&dry_run=true"} {
		for _, tc := range []struct {
			method  string
			headers map[string]string
		}{
			// No summary, a body that does not parse, and on the patch a
			// media type the route refuses: every later check would fail.
			{http.MethodPut, nil},
			{http.MethodPatch, map[string]string{"Content-Type": "text/plain"}},
		} {
			res := s.do(t, tc.method, "/config?"+query, "name: [", tc.headers)
			if res.Code != http.StatusBadRequest {
				t.Errorf("%s ?%s = %d, want 400: %s", tc.method, query, res.Code, res.Body)
				continue
			}
			if got := decode(t, res)["error"]; got != "invalid_query" {
				t.Errorf("%s ?%s error = %v, want invalid_query", tc.method, query, got)
			}
		}
	}

	// false is a write, so it needs its summary like one.
	res := s.do(t, http.MethodPatch, "/config?dry_run=false", `{"mission": "x"}`, nil)
	if res.Code != http.StatusBadRequest || decode(t, res)["error"] != "summary_required" {
		t.Errorf("dry_run=false with no summary = %d %s, want 400 summary_required", res.Code, res.Body)
	}
	res = s.do(t, http.MethodPatch, "/config?dry_run=false", `{"mission": "x"}`, summaryHeader)
	if res.Code != http.StatusCreated {
		t.Errorf("dry_run=false with a summary = %d, want 201: %s", res.Code, res.Body)
	}
	if got := s.writes(); got != oneWrite {
		t.Errorf("dry_run=false counted %+v, want the one write", got)
	}
}

// A DRY RUN NEEDS NO SUMMARY, AND STILL LIFTS ONE OUT.
//
// Nothing is stored, so there is nothing to record a summary on. A `_summary`
// left in the document would be refused by the strict reader as an unknown
// field, and the check would then disagree with the write it stands for.
func TestADryRunNeedsNoSummaryAndLiftsOneOut(t *testing.T) {
	t.Parallel()
	s := newCountedSurface(t)
	s.seed(t, companyDoc, nil)

	for name, tc := range map[string]struct{ method, body string }{
		"put with none":      {http.MethodPut, companyDoc},
		"put with a key":     {http.MethodPut, "_summary: checked\n" + companyDoc},
		"patch with none":    {http.MethodPatch, `{"mission": "checked"}`},
		"patch with its key": {http.MethodPatch, `{"_summary": "checked", "mission": "checked"}`},
	} {
		res := s.do(t, tc.method, "/config?dry_run=true", tc.body, nil)
		if res.Code != http.StatusOK {
			t.Errorf("%s = %d, want 200: %s", name, res.Code, res.Body)
		}
	}
	// A key that is not a string is still the body's mistake, placed at it.
	res := s.do(t, http.MethodPatch, "/config?dry_run=true", `{"_summary": ["no"], "mission": "x"}`, nil)
	if res.Code != http.StatusBadRequest {
		t.Fatalf("a non-string _summary = %d, want 400: %s", res.Code, res.Body)
	}
	problems := problemsOf(t, res)
	if len(problems) != 1 || problems[0].Path != "_summary" || problems[0].Kind != "shape" {
		t.Errorf("problems = %+v, want one shape problem at _summary", problems)
	}
}

// A DRY RUN KEEPS EACH ROUTE'S CHECK ORDER, so it is refused for the same
// reasons, in the same order, as the write it stands for.
func TestADryRunIsRefusedForWhatTheWriteIsRefusedFor(t *testing.T) {
	t.Parallel()

	unconfigured := newCountedSurface(t)
	res := unconfigured.do(t, http.MethodPatch, "/config?dry_run=true", `{"mission": "x"}`, nil)
	if res.Code != http.StatusConflict || decode(t, res)["error"] != "no_active_revision" {
		t.Errorf("a patch check with nothing active = %d %s, want 409 no_active_revision", res.Code, res.Body)
	}

	s := newCountedSurface(t)
	s.seed(t, companyDoc, nil)
	stale := s.do(t, http.MethodPatch, "/config?dry_run=true", `{"mission": "x"}`,
		map[string]string{"If-Match": `"a-revision-that-moved-on"`})
	if stale.Code != http.StatusConflict || decode(t, stale)["error"] != "revision_advanced" {
		t.Errorf("a stale patch check = %d %s, want 409 revision_advanced", stale.Code, stale.Body)
	}
	created := s.do(t, http.MethodPut, "/config?dry_run=true", companyDoc,
		map[string]string{"If-None-Match": "*"})
	if created.Code != http.StatusPreconditionFailed || decode(t, created)["error"] != "already_configured" {
		t.Errorf("a create check on a configured node = %d %s, want 412 already_configured",
			created.Code, created.Body)
	}
	if got := s.writes(); got != (writeCounts{}) {
		t.Errorf("refused checks counted %+v, want nothing", got)
	}
}

// EVERY WRITE ANSWERS WHAT IT PRODUCED: its revision and epoch, its warnings
// and the hierarchy the engine derives from it.
//
// A reload and a revert re-activate a stored company under the runnable rules
// only, so theirs is the answer that can carry an admission warning: a company
// stored before a rule, which runs and which a write keeping it would be
// refused for.
func TestEveryWriteAnswersItsWarningsAndDerivedHierarchy(t *testing.T) {
	t.Parallel()
	s := newCountedSurface(t)
	first := s.seedStored(t, duplicateNamesDoc, func(map[string]any) {})

	for _, tc := range []struct{ name, method, path, body string }{
		{"reload", http.MethodPost, "/config/reload", ""},
		{"put", http.MethodPut, "/config", companyDoc},
		{"patch", http.MethodPatch, "/config", `{"mission": "answered"}`},
		{"entity", http.MethodPut, "/config/roles/ceo", `{"name": "CEO", "handle": "ceo", "llm": "zulu"}`},
		{"revert", http.MethodPost, "/config/revisions/" + first + "/revert", ""},
	} {
		res := s.do(t, tc.method, tc.path, tc.body, summaryHeader)
		if res.Code != http.StatusCreated {
			t.Fatalf("%s = %d, want 201: %s", tc.name, res.Code, res.Body)
		}
		var answer struct {
			RevisionID string           `json:"revision_id"`
			Epoch      int64            `json:"epoch"`
			Warnings   []config.Warning `json:"warnings"`
			Derived    *config.Derived  `json:"derived"`
		}
		if err := json.Unmarshal(res.Body.Bytes(), &answer); err != nil {
			t.Fatalf("%s: decode: %v", tc.name, err)
		}
		if answer.RevisionID == "" || answer.Epoch == 0 || answer.Warnings == nil ||
			answer.Derived == nil || len(answer.Derived.Seats) == 0 {
			t.Errorf("%s answered %s, want a revision, an epoch, a warnings list and the hierarchy",
				tc.name, res.Body)
		}
		var kinds []string
		for _, w := range answer.Warnings {
			kinds = append(kinds, w.Kind)
		}
		wantAdmission := tc.name == "reload" || tc.name == "revert"
		if slices.Contains(kinds, config.WarningAdmission) != wantAdmission {
			t.Errorf("%s warnings = %+v, admission warnings wanted: %v", tc.name, answer.Warnings, wantAdmission)
		}
	}
}

// problemsOf decodes a refusal's problems.
func problemsOf(t *testing.T, res interface{ Result() *http.Response }) []config.Problem {
	t.Helper()
	var body struct {
		Problems []config.Problem `json:"problems"`
	}
	if err := json.NewDecoder(res.Result().Body).Decode(&body); err != nil {
		t.Fatalf("decode the refusal: %v", err)
	}
	return body.Problems
}
