package configapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/store"
)

// A dry run is a write that stores, activates and publishes nothing. These
// cases hold it to that with fakes that COUNT the three calls a write makes,
// wrapped around the real store and plane so everything else a request does is
// exactly what production runs.

// countingRevisions counts every revision stored.
type countingRevisions struct {
	configapi.RevisionStore
	inserts atomic.Int32
}

func (c *countingRevisions) InsertActive(ctx context.Context, r store.Revision) (string, error) {
	c.inserts.Add(1)
	return c.RevisionStore.InsertActive(ctx, r)
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

// writes is how many stores, activations and publishes the surface has made.
func (c *counted) writes() [3]int32 {
	return [3]int32{c.revisions.inserts.Load(), c.plane.activations.Load(), c.queue.publishes.Load()}
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
			if got := s.writes(); got != [3]int32{} {
				t.Errorf("a dry run made %d stores, %d activations and %d publishes, want none",
					got[0], got[1], got[2])
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
		if got := s.writes(); got != [3]int32{1, 1, 1} {
			t.Errorf("a %s write counted %v, want one store, one activation, one publish", method, got)
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

	res := s.do(t, http.MethodPatch, "/config?dry_run=true",
		`{"roles": [{"name": "CEO", "handle": "ceo", "llm": "zulu", "manages": ["CTO", "Ghost"]},
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
	if got := s.writes(); got != [3]int32{1, 1, 1} {
		t.Errorf("dry_run=false counted %v, want the one write", got)
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

// A DRY RUN ON A PROCESS THAT CANNOT ACTIVATE IS REFUSED AS THE WRITE IS.
//
// Refused before validation, in the write and the check alike: a check that
// answered a clean 200 here would promise a save that then fails with 503,
// and one that answered the document's problems would send the operator to
// fix a document no fix makes writable on this process.
func TestADryRunWithoutAControlPlaneIsRefusedAsTheWriteIs(t *testing.T) {
	t.Parallel()
	s := newSurfaceWith(t, func(o *configapi.Options) { o.Plane = nil })
	s.seed(t, companyDoc, nil)

	invalid := strings.Replace(companyDoc, "llm: zulu", "llm: nowhere", 1)
	for _, tc := range []struct{ name, method, query, body string }{
		{"a valid put check", http.MethodPut, "?dry_run=true", companyDoc},
		{"an invalid put check", http.MethodPut, "?dry_run=true", invalid},
		{"an invalid put", http.MethodPut, "", invalid},
		{"a valid patch check", http.MethodPatch, "?dry_run=true", `{"mission": "x"}`},
		{"an invalid patch check", http.MethodPatch, "?dry_run=true", `{"roles": [{"name": "CEO", "llm": "nowhere"}]}`},
		{"an invalid patch", http.MethodPatch, "", `{"roles": [{"name": "CEO", "llm": "nowhere"}]}`},
	} {
		res := s.do(t, tc.method, "/config"+tc.query, tc.body, summaryHeader)
		if res.Code != http.StatusServiceUnavailable {
			t.Errorf("%s = %d, want 503: %s", tc.name, res.Code, res.Body)
			continue
		}
		if got := decode(t, res)["error"]; got != "no_control_plane" {
			t.Errorf("%s error = %v, want no_control_plane", tc.name, got)
		}
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
	if got := s.writes(); got != [3]int32{} {
		t.Errorf("refused checks counted %v, want nothing", got)
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
