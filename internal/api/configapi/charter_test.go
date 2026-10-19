package configapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
)

// charterDoc is companyDoc with a charter written: the settings around it are
// what a charter write must leave exactly as they were.
const charterDoc = companyDoc + `
mission: Help friends split costs fairly.
vision: Splitting a bill is never the awkward part again.
policies:
  - Every amount is exact to the cent.
  - Work is tracked on the board.
`

// charterOf decodes a charter from a JSON value an answer carries.
func charterOf(t *testing.T, raw any) config.Charter {
	t.Helper()
	encoded, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	var ch config.Charter
	if err := json.Unmarshal(encoded, &ch); err != nil {
		t.Fatalf("not a charter: %s: %v", encoded, err)
	}
	return ch
}

// charterBody is a charter write's body.
func charterBody(t *testing.T, ch config.Charter) string {
	t.Helper()
	encoded, err := json.Marshal(ch)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

// readCharter is GET /config/charter, refusing anything but a 200.
func (s *surface) readCharter(t *testing.T) (config.Charter, string) {
	t.Helper()
	res := s.do(t, http.MethodGet, "/config/charter", "", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("GET /config/charter = %d %s", res.Code, res.Body)
	}
	return charterOf(t, decode(t, res)), res.Header().Get("ETag")
}

// putCharter is PUT /config/charter under tag.
func (s *surface) putCharter(t *testing.T, tag string, ch config.Charter, query string) (int, map[string]any, string) {
	t.Helper()
	headers := map[string]string{}
	if tag != "" {
		headers["If-Match"] = tag
	}
	res := s.do(t, http.MethodPut, "/config/charter"+query, charterBody(t, ch), headers)
	return res.Code, decode(t, res), res.Header().Get("ETag")
}

func TestTheCharterReadsAsItsThreeFieldsAndAContentTag(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, charterDoc)
	got, tag := s.readCharter(t)
	want := config.Charter{
		Mission:  "Help friends split costs fairly.",
		Vision:   "Splitting a bill is never the awkward part again.",
		Policies: []string{"Every amount is exact to the cent.", "Work is tracked on the board."},
	}
	if !got.Equal(want) {
		t.Errorf("GET /config/charter = %+v, want %+v", got, want)
	}
	if !strings.HasPrefix(tag, `"charter-`) || !strings.HasSuffix(tag, `"`) {
		t.Errorf("ETag = %q, want a strong tag of the charter", tag)
	}

	// THE TAG IS THE CONTENT'S: a revision that changes nothing about the
	// charter leaves it as it was.
	res := s.do(t, http.MethodPatch, "/config", `{"timezone":"Europe/Berlin"}`,
		map[string]string{"X-Summary": "move the clock"})
	if res.Code != http.StatusCreated {
		t.Fatalf("PATCH /config = %d %s", res.Code, res.Body)
	}
	if _, again := s.readCharter(t); again != tag {
		t.Errorf("a revision that left the charter alone moved its tag from %s to %s", tag, again)
	}
}

// A charter nobody wrote reads as empty values and an empty LIST, never a
// missing field or a null: a client reading it has one spelling to handle.
func TestACharterNobodyWroteReadsAsEmpty(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, companyDoc)
	res := s.do(t, http.MethodGet, "/config/charter", "", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("GET /config/charter = %d %s", res.Code, res.Body)
	}
	if body := strings.TrimSpace(res.Body.String()); body !=
		`{"mission":"","vision":"","policies":[]}` {
		t.Errorf("GET /config/charter = %s, want the three fields empty", body)
	}
}

func TestTheCharterOfAnUnconfiguredNodeIsNotThere(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	if res := s.do(t, http.MethodGet, "/config/charter", "", nil); res.Code != http.StatusNotFound ||
		decode(t, res)["error"] != string(httpjson.CodeNoActiveRevision) {
		t.Errorf("GET on an unconfigured node = %d %s, want 404 no_active_revision", res.Code, res.Body)
	}
	if code, body, _ := s.putCharter(t, "*", config.Charter{Policies: []string{}}, ""); code !=
		http.StatusConflict || body["error"] != string(httpjson.CodeNoActiveRevision) {
		t.Errorf("PUT on an unconfigured node = %d %v, want 409 no_active_revision", code, body)
	}
}

func TestACharterWriteIsANewRevisionOfTheWholeDocument(t *testing.T) {
	t.Parallel()
	pub := &nudgeRecorder{}
	s := newSurfaceWith(t, func(o *configapi.Options) { o.Queue = pub })
	first := s.seed(t, charterDoc)
	_, tag := s.readCharter(t)

	next := config.Charter{
		Mission:  "Help groups split shared costs fairly.",
		Vision:   "Splitting a bill is never the awkward part again.",
		Policies: []string{"Work is tracked on the board.", "Every amount is exact to the cent.", "Design before code."},
	}
	code, body, newTag := s.putCharter(t, tag, next, "")
	if code != http.StatusOK {
		t.Fatalf("PUT /config/charter = %d %v", code, body)
	}
	if body["changed"] != true || body["revision_id"] == "" || body["revision_id"] == first ||
		body["epoch"] == nil {
		t.Errorf("the answer is %v, want a changed charter with its new revision and epoch", body)
	}
	if got := charterOf(t, body["charter"]); !got.Equal(next) {
		t.Errorf("the answer's charter is %+v, want %+v", got, next)
	}
	stored, readTag := s.readCharter(t)
	if !stored.Equal(next) {
		t.Errorf("the document now carries %+v, want %+v (policies in the order sent)", stored, next)
	}
	if newTag == tag || newTag != readTag {
		t.Errorf("the write answered ETag %s, a read answers %s and the old was %s: the "+
			"write's tag must be the next write's precondition", newTag, readTag, tag)
	}

	// THE REST OF THE DOCUMENT IS UNTOUCHED, its credentials included.
	doc := s.activeDocument(t)
	for _, kept := range []string{"sk-literal", "${ROTATED}", signingSecret, "gitlab.example.com"} {
		if !strings.Contains(doc, kept) {
			t.Errorf("the charter write lost %q from the document: %s", kept, doc)
		}
	}

	// AUDITED AS EVERY CONFIGURATION WRITE IS: a revision naming its author,
	// its credential and what it changed, and the activation on the feed.
	active, _, err := s.active(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if active.ID != body["revision_id"] || active.ParentID != first ||
		active.CreatedBy != "token:ops" || active.OperatorID != "token:ops" ||
		active.Summary != "edited the company's mission and policies" {
		t.Errorf("the revision is %+v, want revision %v built on %s by token:ops, "+
			"summarised as the mission and policies edit", active, body["revision_id"], first)
	}
	if len(pub.sent) != 1 {
		t.Fatalf("published %d events, want the activation", len(pub.sent))
	}
	if ev, _ := pub.sent[0].Data.(*types.ConfigRevisionActivated); ev == nil ||
		ev.RevisionSummary != active.Summary || ev.CreatedBy != "token:ops" ||
		ev.RevisionSource != "api" {
		t.Errorf("the activation event is %+v", pub.sent[0].Data)
	}
}

// A SUMMARY THE CALLER NAMES IS THE ONE RECORDED; one left out is derived.
func TestACharterWriteRecordsTheSummaryItWasGiven(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, charterDoc)
	_, tag := s.readCharter(t)
	res := s.do(t, http.MethodPut, "/config/charter",
		`{"mission":"New mission.","vision":"","policies":[]}`,
		map[string]string{"If-Match": tag, "X-Summary": "rewrite the principles after the offsite"})
	if res.Code != http.StatusOK {
		t.Fatalf("PUT = %d %s", res.Code, res.Body)
	}
	active, _, err := s.active(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if active.Summary != "rewrite the principles after the offsite" {
		t.Errorf("summary = %q, want the caller's", active.Summary)
	}
}

func TestACharterWriteNeedsAPrecondition(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, charterDoc)
	before := s.revisionCount(t)
	code, body, _ := s.putCharter(t, "", config.Charter{Mission: "x", Policies: []string{}}, "")
	if code != http.StatusPreconditionRequired || body["error"] != string(httpjson.CodeBadIfMatch) {
		t.Errorf("a write with no If-Match = %d %v, want 428 bad_if_match", code, body)
	}
	if after := s.revisionCount(t); after != before {
		t.Errorf("a refused write stored %d revisions", after-before)
	}
}

// THE CONFLICT CARRIES THE CHARTER NOW, so an editor shows the person their
// draft beside what replaced it, and the tag that keeps their draft.
func TestAStaleCharterDraftIsRefusedWithTheCharterNow(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, charterDoc)
	_, tag := s.readCharter(t)

	theirs := config.Charter{Mission: "Their mission.", Policies: []string{"Their rule."}}
	if code, body, _ := s.putCharter(t, tag, theirs, ""); code != http.StatusOK {
		t.Fatalf("the other editor's write = %d %v", code, body)
	}
	_, nowTag := s.readCharter(t)
	before := s.revisionCount(t)

	mine := config.Charter{Mission: "My mission.", Policies: []string{}}
	code, body, header := s.putCharter(t, tag, mine, "")
	if code != http.StatusPreconditionFailed || body["error"] != string(httpjson.CodeVersionMoved) {
		t.Fatalf("a stale draft = %d %v, want 412 version_moved", code, body)
	}
	if got := charterOf(t, body["current"]); !got.Equal(theirs) {
		t.Errorf("the refusal carries %+v, want the charter now, %+v", got, theirs)
	}
	if body["etag"] != nowTag || header != nowTag {
		t.Errorf("the refusal names tag %v (header %s), want the charter's now, %s",
			body["etag"], header, nowTag)
	}
	if after := s.revisionCount(t); after != before {
		t.Errorf("a refused draft stored %d revisions", after-before)
	}

	// AND THE TAG IT NAMED KEEPS THE DRAFT.
	if code, body, _ := s.putCharter(t, nowTag, mine, ""); code != http.StatusOK {
		t.Errorf("the draft resent with the tag the refusal named = %d %v", code, body)
	}
}

// A WRITE ELSEWHERE IN THE DOCUMENT IS NO CONFLICT: the precondition is the
// charter's, not the revision's.
func TestAnUnrelatedWriteDoesNotConflictACharterDraft(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, charterDoc)
	_, tag := s.readCharter(t)
	if res := s.do(t, http.MethodPatch, "/config", `{"timezone":"Europe/Berlin"}`,
		map[string]string{"X-Summary": "move the clock"}); res.Code != http.StatusCreated {
		t.Fatalf("PATCH /config = %d %s", res.Code, res.Body)
	}
	draft := config.Charter{Mission: "A draft started before the clock moved.", Policies: []string{}}
	if code, body, _ := s.putCharter(t, tag, draft, ""); code != http.StatusOK {
		t.Fatalf("the draft = %d %v, want it written", code, body)
	}
	if doc := s.activeDocument(t); !strings.Contains(doc, "Europe/Berlin") {
		t.Errorf("the charter write undid the clock: %s", doc)
	}
}

// interleavedPlane lets another writer activate a revision the first time a
// charter write activates, the way a write landing on another node in the
// same instant does.
type interleavedPlane struct {
	coord.Plane
	// raced is set before the other writer runs, because that writer's own
	// activation comes back through this plane and must pass straight on.
	raced atomic.Bool
	other func(context.Context)
}

func (p *interleavedPlane) Activate(ctx context.Context, req coord.ActivationRequest) (coord.Activation, error) {
	// ONCE THE CASE HAS SAID WHAT RACES, so the seed's own activation
	// passes straight through.
	if p.other != nil && p.raced.CompareAndSwap(false, true) {
		p.other(ctx)
	}
	return p.Plane.Activate(ctx, req)
}

// A RACE LOST TO ANOTHER WRITER'S UNRELATED CHANGE IS TRIED AGAIN rather than
// handed to the person as a conflict about nothing they wrote; the retry is
// judged against the charter again, and lands on the other writer's revision.
func TestACharterWriteThatLosesARaceToAnUnrelatedWriteIsTriedAgain(t *testing.T) {
	t.Parallel()
	plane := &interleavedPlane{}
	s := newSurfaceWith(t, func(o *configapi.Options) {
		plane.Plane = o.Plane
		o.Plane = plane
	})
	s.seed(t, charterDoc)
	_, tag := s.readCharter(t)
	var racer string
	plane.other = func(ctx context.Context) {
		applied, err := s.service().Apply(ctx, configapi.ApplyRequest{
			Patch: []byte(`{"timezone":"Europe/Berlin"}`), Summary: "move the clock",
			By: iam.Actor{Name: "token:ops", Kind: iam.ActorOperator, OperatorID: "token:ops"},
		})
		if err != nil {
			t.Errorf("the racing write: %v", err)
		}
		racer = applied.RevisionID
	}
	draft := config.Charter{Mission: "Written while the clock moved.", Policies: []string{}}
	code, body, _ := s.putCharter(t, tag, draft, "")
	if code != http.StatusOK {
		t.Fatalf("the draft = %d %v, want it written on the second attempt", code, body)
	}
	active, _, err := s.active(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if active.ParentID != racer {
		t.Errorf("the charter revision is built on %s, want the racing write's %s", active.ParentID, racer)
	}
	doc := s.activeDocument(t)
	if !strings.Contains(doc, "Europe/Berlin") || !strings.Contains(doc, "Written while the clock moved.") {
		t.Errorf("the document keeps only one of the two writes: %s", doc)
	}
}

// THE CHARTER'S BOUNDS, at the byte and at the count, and the blank and the
// repeated policy, each refused naming its place.
func TestACharterPastItsBoundsIsRefusedNamingThePlace(t *testing.T) {
	t.Parallel()
	many := make([]string, config.MaxPolicies+1)
	for i := range many {
		many[i] = "Rule " + strings.Repeat("x", i+1)
	}
	for name, tc := range map[string]struct {
		charter config.Charter
		path    string
	}{
		"a mission one character long": {config.Charter{
			Mission: strings.Repeat("é", config.MaxMissionChars+1), Policies: []string{}}, "mission"},
		"a mission on two lines": {config.Charter{
			Mission: "Split costs.\n## Tools", Policies: []string{}}, "mission"},
		"an emoji mission one character long": {config.Charter{
			Mission: strings.Repeat("🚀", config.MaxMissionChars+1), Policies: []string{}}, "mission"},
		"a vision one character long": {config.Charter{
			Vision: strings.Repeat("v", config.MaxVisionChars+1), Policies: []string{}}, "vision"},
		"one policy too many": {config.Charter{Policies: many}, "policies"},
		"a policy one character long": {config.Charter{
			Policies: []string{"ok", strings.Repeat("p", config.MaxPolicyChars+1)}}, "policies[1]"},
		"a policy on two lines": {config.Charter{
			Policies: []string{"ok", "One rule.\nAnother."}}, "policies[1]"},
		"a blank policy": {config.Charter{Policies: []string{"ok", "   "}}, "policies[1]"},
		"a repeated policy": {config.Charter{
			Policies: []string{"Say it once.", "Other.", "  Say it once. "}}, "policies[2]"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newSurface(t)
			s.seed(t, charterDoc)
			_, tag := s.readCharter(t)
			before := s.revisionCount(t)
			code, body, _ := s.putCharter(t, tag, tc.charter, "")
			if code != http.StatusBadRequest || body["error"] != string(httpjson.CodeValidationError) {
				t.Fatalf("= %d %v, want 400 validation_error", code, body)
			}
			problems, _ := body["problems"].([]any)
			var paths []string
			for _, p := range problems {
				if m, ok := p.(map[string]any); ok {
					paths = append(paths, m["path"].(string))
				}
			}
			if !slices.Contains(paths, tc.path) {
				t.Errorf("the refusal names %v, want %s among them", paths, tc.path)
			}
			if after := s.revisionCount(t); after != before {
				t.Errorf("a refused charter stored %d revisions", after-before)
			}
		})
	}
}

// AT THE BOUNDS IS INSIDE THEM, in characters: a mission of 1024 emoji is 4096
// bytes and is written, and so are CJK policies at theirs.
func TestACharterAtItsBoundsIsWritten(t *testing.T) {
	t.Parallel()
	policies := make([]string, config.MaxPolicies)
	for i := range policies {
		policies[i] = strings.Repeat(string(rune('漢'+i)), config.MaxPolicyChars)
	}
	s := newSurface(t)
	s.seed(t, charterDoc)
	_, tag := s.readCharter(t)
	at := config.Charter{
		// Four bytes each: 4096 bytes, and 1024 characters, which is the bound.
		Mission:  strings.Repeat("🚀", config.MaxMissionChars),
		Vision:   strings.Repeat("v", config.MaxVisionChars),
		Policies: policies,
	}
	if code, body, _ := s.putCharter(t, tag, at, ""); code != http.StatusOK {
		t.Fatalf("a charter at every bound = %d %v, want it written", code, body)
	}
}

func TestACharterBodyNamesAllThreeFieldsAndNothingElse(t *testing.T) {
	t.Parallel()
	for name, body := range map[string]string{
		"no policies":      `{"mission":"m","vision":"v"}`,
		"no mission":       `{"vision":"v","policies":[]}`,
		"a null vision":    `{"mission":"m","vision":null,"policies":[]}`,
		"an unknown field": `{"mission":"m","vision":"v","policies":[],"name":"Other"}`,
		"a number":         `{"mission":7,"vision":"v","policies":[]}`,
		"two values":       `{"mission":"m","vision":"v","policies":[]} {}`,
		"not JSON":         `mission: m`,
		"a policy object":  `{"mission":"m","vision":"v","policies":[{"text":"x"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newSurface(t)
			s.seed(t, charterDoc)
			_, tag := s.readCharter(t)
			res := s.do(t, http.MethodPut, "/config/charter", body, map[string]string{"If-Match": tag})
			if res.Code != http.StatusBadRequest || decode(t, res)["error"] != string(httpjson.CodeInvalidBody) {
				t.Errorf("= %d %s, want 400 invalid_body", res.Code, res.Body)
			}
		})
	}
}

// NOTHING TO WRITE IS WRITTEN AS NOTHING: a revision that changes nothing
// still re-activates on every node and rebuilds every seat. The comparison is
// of the TRIMMED draft, which is what would be stored.
func TestAnUnchangedCharterWritesNoRevision(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	first := s.seed(t, charterDoc)
	current, tag := s.readCharter(t)
	current.Mission = "  " + current.Mission + "\n"
	code, body, header := s.putCharter(t, tag, current, "")
	if code != http.StatusOK || body["changed"] != false || body["revision_id"] != first {
		t.Errorf("an unchanged charter = %d %v, want 200, unchanged, naming %s", code, body, first)
	}
	if header != tag {
		t.Errorf("ETag = %s, want the unchanged %s", header, tag)
	}
	if n := s.revisionCount(t); n != 1 {
		t.Errorf("the node holds %d revisions, want the one it was seeded with", n)
	}
}

// THE SPACE AN EDITOR LEAVES AROUND A VALUE IS NOT STORED, and an empty field
// leaves the document rather than staying in it as `""`.
func TestACharterIsStoredTrimmedAndAnEmptyFieldLeavesTheDocument(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, charterDoc)
	_, tag := s.readCharter(t)
	code, body, _ := s.putCharter(t, tag, config.Charter{
		Mission: "  Padded mission.\n", Vision: "   ", Policies: []string{" One rule. "},
	}, "")
	if code != http.StatusOK {
		t.Fatalf("= %d %v", code, body)
	}
	got, _ := s.readCharter(t)
	if want := (config.Charter{Mission: "Padded mission.", Policies: []string{"One rule."}}); !got.Equal(want) {
		t.Errorf("stored %+v, want %+v", got, want)
	}
	var stored map[string]any
	if err := json.Unmarshal([]byte(s.activeDocument(t)), &stored); err != nil {
		t.Fatal(err)
	}
	if _, kept := stored["vision"]; kept {
		t.Errorf("the document keeps an empty vision: %v", stored["vision"])
	}
}

func TestACharterDryRunStoresNothing(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, charterDoc)
	_, tag := s.readCharter(t)
	draft := config.Charter{Mission: "Checked, not written.", Policies: []string{}}
	code, body, _ := s.putCharter(t, tag, draft, "?dry_run=true")
	if code != http.StatusOK || body["valid"] != true || body["changed"] != true {
		t.Fatalf("a dry run = %d %v, want 200 valid and changed", code, body)
	}
	if got, _ := s.readCharter(t); got.Mission == draft.Mission {
		t.Error("a dry run wrote the charter")
	}
	if n := s.revisionCount(t); n != 1 {
		t.Errorf("a dry run stored %d revisions", n-1)
	}
	// AND A DRY RUN OF AN INVALID CHARTER IS REFUSED AS THE WRITE WOULD BE.
	code, body, _ = s.putCharter(t, tag, config.Charter{Policies: []string{""}}, "?dry_run=true")
	if code != http.StatusBadRequest || body["error"] != string(httpjson.CodeValidationError) {
		t.Errorf("a dry run of a blank policy = %d %v, want 400 validation_error", code, body)
	}
}

// THE CHARTER IS ADMIN'S, read and written: a person holding every app and
// not admin is refused both, naming admin.
func TestTheCharterIsAnAdministratorsToReadAndWrite(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, charterDoc)
	_, tag := s.readCharter(t)
	s.grants = slices.Clone(iam.Apps)
	for _, req := range []struct{ method, body string }{
		{http.MethodGet, ""},
		{http.MethodPut, `{"mission":"m","vision":"","policies":[]}`},
	} {
		res := s.do(t, req.method, "/config/charter", req.body, map[string]string{"If-Match": tag})
		body := decode(t, res)
		grants, _ := body["grants"].([]any)
		if res.Code != http.StatusForbidden || body["error"] != string(httpjson.CodeUnauthorized) ||
			!slices.Contains(grants, any(string(iam.GrantAdmin))) {
			t.Errorf("%s /config/charter without admin = %d %v, want 403 naming admin",
				req.method, res.Code, body)
		}
	}
}

// THE DASHBOARD BOUNDS A DRAFT WHERE THE ENGINE DOES, so a draft past one is
// marked before Save rather than refused after it, and one the engine takes is
// never refused by the screen.
func TestTheDashboardBoundsTheCharterAtTheEnginesCaps(t *testing.T) {
	t.Parallel()
	tree := clientsource.Tree(t)
	for name, engine := range map[string]int{
		"MISSION_MAX_CHARS": config.MaxMissionChars,
		"VISION_MAX_CHARS":  config.MaxVisionChars,
		"POLICY_MAX_CHARS":  config.MaxPolicyChars,
		"POLICIES_MAX":      config.MaxPolicies,
	} {
		raw, err := clientsource.Scalar(tree, name)
		if err != nil {
			t.Fatal(err)
		}
		if raw != strconv.Itoa(engine) {
			t.Errorf("the dashboard bounds %s at %s and the engine at %d: change it in "+
				"contract/config.ts to the engine's figure", name, raw, engine)
		}
	}
}

// THE DASHBOARD READS THE CHARTER AS THE ENGINE SENDS IT: every field the read
// answers, each required, and nothing else.
func TestTheDashboardReadsTheCharterTheEngineSends(t *testing.T) {
	t.Parallel()
	members, err := clientsource.Interface(clientsource.Tree(t), "Charter")
	if err != nil {
		t.Fatalf("%v: this gate cannot run without the dashboard's declaration", err)
	}
	var declared []string
	for _, m := range members {
		if m.Optional {
			t.Errorf("Charter.%s is optional, and the engine always sends it", m.Name)
		}
		declared = append(declared, m.Name)
	}
	s := newSurface(t)
	s.seed(t, companyDoc)
	res := s.do(t, http.MethodGet, "/config/charter", "", nil)
	var sent []string
	for key := range decode(t, res) {
		sent = append(sent, key)
	}
	slices.Sort(declared)
	slices.Sort(sent)
	if !slices.Equal(declared, sent) {
		t.Errorf("the dashboard declares Charter as %v and the engine sends %v", declared, sent)
	}
}

// A WRITE THAT KEEPS LOSING IS TOLD SO, after its bounded attempts: each lost
// attempt is a revision stored and left inert, so the bound is also how much
// history one save can leave behind.
func TestACharterWriteThatKeepsLosingIsToldSo(t *testing.T) {
	t.Parallel()
	losing := &losingPlane{}
	s := newSurfaceWith(t, func(o *configapi.Options) {
		losing.Plane = o.Plane
		o.Plane = losing
	})
	s.seed(t, charterDoc)
	losing.armed.Store(true)
	_, tag := s.readCharter(t)
	code, body, _ := s.putCharter(t, tag, config.Charter{Mission: "m", Policies: []string{}}, "")
	if code != http.StatusConflict || body["error"] != string(httpjson.CodeRevisionAdvanced) {
		t.Fatalf("= %d %v, want 409 revision_advanced", code, body)
	}
	if n := s.revisionCount(t); n != 1+3 {
		t.Errorf("the node holds %d revisions, want the seed and one inert revision "+
			"for each of three attempts", n)
	}
}

// losingPlane refuses every activation once armed, as a fleet whose pointer
// another writer moves before each of this one's lands.
type losingPlane struct {
	coord.Plane
	armed atomic.Bool
}

func (p *losingPlane) Activate(ctx context.Context, req coord.ActivationRequest) (coord.Activation, error) {
	if p.armed.Load() {
		return coord.Activation{}, fmt.Errorf("%w: another write landed first",
			coord.ErrActivationRaced)
	}
	return p.Plane.Activate(ctx, req)
}

// A SAVE SENT AGAIN IS ANSWERED, NOT REFUSED: the retry of a write whose
// answer was lost carries the tag it was built on, which no longer matches the
// charter it wrote, and the charter already saying what it asks for is the
// write having happened. It stores nothing the second time.
func TestASaveSentAgainIsAnsweredRatherThanRefused(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, charterDoc)
	_, tag := s.readCharter(t)
	draft := config.Charter{Mission: "Saved once.", Policies: []string{"One rule."}}
	code, first, written := s.putCharter(t, tag, draft, "")
	if code != http.StatusOK {
		t.Fatalf("the save = %d %v", code, first)
	}
	before := s.revisionCount(t)
	code, again, header := s.putCharter(t, tag, draft, "")
	if code != http.StatusOK || again["changed"] != false || again["revision_id"] != first["revision_id"] {
		t.Errorf("the same save sent again = %d %v, want 200 unchanged naming %v",
			code, again, first["revision_id"])
	}
	if header != written {
		t.Errorf("ETag = %s, want the saved charter's %s", header, written)
	}
	if after := s.revisionCount(t); after != before {
		t.Errorf("the save sent again stored %d revisions", after-before)
	}
}

// THE DASHBOARD READS A CHARTER CONFLICT AS THE ENGINE SENDS IT: every member
// CharterMoved declares is in the refusal, required, beside the envelope.
func TestTheDashboardReadsWhatACharterConflictSends(t *testing.T) {
	t.Parallel()
	members, err := clientsource.Interface(clientsource.Tree(t), "CharterMoved")
	if err != nil {
		t.Fatalf("%v: this gate cannot run without the dashboard's declaration", err)
	}
	s := newSurface(t)
	s.seed(t, charterDoc)
	_, tag := s.readCharter(t)
	if code, body, _ := s.putCharter(t, tag, config.Charter{Mission: "Theirs.", Policies: []string{}}, ""); code != http.StatusOK {
		t.Fatalf("the other editor's write = %d %v", code, body)
	}
	code, body, _ := s.putCharter(t, tag, config.Charter{Mission: "Mine.", Policies: []string{}}, "")
	if code != http.StatusPreconditionFailed {
		t.Fatalf("= %d %v, want the 412", code, body)
	}
	if len(members) != 2 {
		t.Fatalf("CharterMoved declares %v; this gate compares current and etag", members)
	}
	for _, m := range members {
		if m.Optional {
			t.Errorf("CharterMoved.%s is optional, and the engine always sends it", m.Name)
		}
		if _, sent := body[m.Name]; !sent {
			t.Errorf("the dashboard reads %s from a charter conflict, and the engine sends %v", m.Name, body)
		}
	}
	if _, ok := body["current"].(map[string]any); !ok {
		t.Errorf("current is %T, want the charter object", body["current"])
	}
}

// THE CHARTER'S ROUTE JUDGES ONLY WHAT IT CHANGES, as every write does: on a
// company whose mission and policy were written before the rules, an edit of
// the vision lands, an edit of either faulty part that leaves it at fault is
// refused, and an edit that fixes one lands.
func TestTheCharterRouteJudgesOnlyWhatItChanges(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		charter config.Charter
		status  int
	}{
		"the vision alone": {config.Charter{Mission: faultyMission, Vision: "A new vision.",
			Policies: []string{"A rule.", faultyPolicy}}, http.StatusOK},
		"the mission, still too long": {config.Charter{Mission: faultyMission + "!",
			Policies: []string{"A rule.", faultyPolicy}}, http.StatusBadRequest},
		"the policy, still two lines": {config.Charter{Mission: faultyMission,
			Policies: []string{"A rule.", faultyPolicy + "!"}}, http.StatusBadRequest},
		"the policy fixed": {config.Charter{Mission: faultyMission,
			Policies: []string{"A rule.", "Every amount is exact."}}, http.StatusOK},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newSurface(t)
			s.seed(t, faultyCharterDoc())
			code, body, _ := s.putCharter(t, "*", tc.charter, "")
			if code != tc.status {
				t.Fatalf("= %d %v, want %d", code, body, tc.status)
			}
			if code == http.StatusBadRequest && body["error"] != string(httpjson.CodeValidationError) {
				t.Errorf("= %v, want validation_error", body)
			}
		})
	}
}
