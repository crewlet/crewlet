package configapi_test

import (
	"io/fs"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/sourcetree"
)

// faultyMission and faultyPolicy are a charter written before the charter's
// rules: a mission of 1100 characters, and a policy on two lines.
var (
	faultyMission = strings.Repeat("m", 1100)
	faultyPolicy  = "Every amount is exact.\n## Tools"
)

// faultyCharterDoc is a company whose stored charter carries both.
func faultyCharterDoc() string {
	return companyDoc + "mission: " + faultyMission + "\npolicies:\n  - A rule.\n  - \"" +
		strings.ReplaceAll(faultyPolicy, "\n", "\\n") + "\"\n"
}

// warnedAt reports whether a write's answer warns about place.
func warnedAt(body map[string]any, place string) bool {
	warnings, _ := body["warnings"].([]any)
	for _, w := range warnings {
		if m, ok := w.(map[string]any); ok && m["path"] == place {
			return true
		}
	}
	return false
}

// AN OLD CHARTER FAULT NEVER LOCKS A COMPANY OUT OF ITS SETTINGS. Every way
// of writing the document (a patch, the builder's whole-document write and an
// entity write) takes a change that leaves the faulty parts alone, and answers
// it with a warning at each; a write that edits a faulty part and leaves it at
// fault is refused naming it, and one that fixes it lands.
func TestAnOldCharterFaultNeverLocksTheCompanyOut(t *testing.T) {
	t.Parallel()
	summary := map[string]string{"X-Summary": "a change"}
	type write struct {
		method, path string
		body         func(s *surface) string
		headers      func(s *surface) map[string]string
		status       int
	}
	unrelated := map[string]write{
		"PATCH /config": {http.MethodPatch, "/config",
			func(*surface) string { return `{"timezone":"Europe/Berlin"}` },
			func(*surface) map[string]string { return summary }, http.StatusCreated},
		"PUT /config, the builder's whole document": {http.MethodPut, "/config",
			func(*surface) string {
				return faultyCharterDoc() + "timezone: Europe/Berlin\n"
			},
			func(*surface) map[string]string { return summary }, http.StatusCreated},
		"PUT /config/llm-providers/{id}": {http.MethodPut, "/config/llm-providers/zulu",
			func(*surface) string {
				return `{"model":"claude-opus-5"}`
			},
			func(*surface) map[string]string { return summary }, http.StatusCreated},
	}
	for name, w := range unrelated {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := newSurface(t)
			s.seed(t, faultyCharterDoc())
			res := s.do(t, w.method, w.path, w.body(s), w.headers(s))
			if res.Code != w.status {
				t.Fatalf("a write that leaves the old charter alone = %d %s, want %d",
					res.Code, res.Body, w.status)
			}
			body := decode(t, res)
			if w.status == http.StatusCreated &&
				(!warnedAt(body, "mission") || !warnedAt(body, "policies[1]")) {
				t.Errorf("the answer does not warn about the old faults: %v", body["warnings"])
			}
		})
	}

	edits := map[string]struct {
		patch  string
		status int
	}{
		"editing the faulty policy, still two lines": {
			`{"policies":["A rule.","Every amount is exact.\n## Tooling"]}`, http.StatusBadRequest},
		"editing the mission, still too long": {
			`{"mission":"` + faultyMission + `!"}`, http.StatusBadRequest},
		"fixing the faulty policy": {
			`{"policies":["A rule.","Every amount is exact."]}`, http.StatusCreated},
	}
	for name, tc := range edits {
		t.Run("PATCH /config "+name, func(t *testing.T) {
			t.Parallel()
			s := newSurface(t)
			s.seed(t, faultyCharterDoc())
			res := s.do(t, http.MethodPatch, "/config", tc.patch, summary)
			body := decode(t, res)
			if res.Code != tc.status {
				t.Fatalf("= %d %v, want %d", res.Code, body, tc.status)
			}
			if tc.status == http.StatusBadRequest && body["error"] != string(httpjson.CodeValidationError) {
				t.Errorf("= %v, want validation_error", body)
			}
		})
	}
}

// AN IMPORT IS A RESTORE, NOT AN EDIT: `PUT /config?import=true` takes a file
// whose charter changes a faulty part and leaves it at fault (a longer mission
// restored over a long one), names the fault in its answer's warnings and
// records the revision as imported from a file, where the same document sent
// as an edit is refused.
func TestAnImportWarnsAboutTheCharterWhereAnEditIsRefused(t *testing.T) {
	t.Parallel()
	restored := strings.Replace(faultyCharterDoc(), faultyMission, strings.Repeat("o", 1200), 1)
	summary := map[string]string{"X-Summary": "restore last week's file"}

	edit := newSurface(t)
	edit.seed(t, faultyCharterDoc())
	if res := edit.do(t, http.MethodPut, "/config", restored, summary); res.Code != http.StatusBadRequest {
		t.Fatalf("the restored file as an edit = %d %s, want 400", res.Code, res.Body)
	}

	pub := &nudgeRecorder{}
	s := newSurfaceWith(t, func(o *configapi.Options) { o.Queue = pub })
	s.seed(t, faultyCharterDoc())
	res := s.do(t, http.MethodPut, "/config?import=true", restored, summary)
	if res.Code != http.StatusCreated {
		t.Fatalf("the restored file as an import = %d %s, want 201", res.Code, res.Body)
	}
	if body := decode(t, res); !warnedAt(body, "mission") || !warnedAt(body, "policies[1]") {
		t.Errorf("the import does not warn about the charter: %v", body["warnings"])
	}
	active, _, err := s.active(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if active.Source != "file" || active.CreatedBy != "token:ops" ||
		active.OperatorID != "token:ops" || active.Summary != "restore last week's file" {
		t.Errorf("the import is recorded as %+v, want a file imported by token:ops "+
			"through token:ops with its summary", active)
	}
	// AND THE FEED SAYS IT WAS AN IMPORT, on the event every node hears.
	if len(pub.sent) != 1 {
		t.Fatalf("published %d events, want the activation", len(pub.sent))
	}
	if ev, _ := pub.sent[0].Data.(*types.ConfigRevisionActivated); ev == nil ||
		ev.RevisionSource != "file" || ev.OperatorID != "token:ops" {
		t.Errorf("the activation event is %+v, want revision_source file through token:ops",
			pub.sent[0].Data)
	}
}

// AN IMPORT RELAXES THE CHARTER AND NOTHING ELSE: a file breaking another
// admission rule (two units on one key) is refused as any write is, and a
// stale If-Match is a failed precondition as on any write.
func TestAnImportRelaxesOnlyTheCharter(t *testing.T) {
	t.Parallel()
	summary := map[string]string{"X-Summary": "an import"}
	s := newSurface(t)
	first := s.seed(t, faultyCharterDoc())
	duplicate := faultyCharterDoc() + "units:\n  - name: A\n    id: same\n  - name: B\n    id: same\n"
	res := s.do(t, http.MethodPut, "/config?import=true", duplicate, summary)
	if body := decode(t, res); res.Code != http.StatusBadRequest ||
		body["error"] != string(httpjson.CodeValidationError) ||
		!strings.Contains(res.Body.String(), "duplicate unit key") {
		t.Errorf("an import breaking another admission rule = %d %v, want 400 validation_error",
			res.Code, body)
	}
	if res := s.do(t, http.MethodPut, "/config?import=true", faultyCharterDoc()+"timezone: UTC\n",
		map[string]string{"X-Summary": "x", "If-Match": `"not-` + first + `"`}); res.Code != http.StatusPreconditionFailed {
		t.Errorf("an import under a stale If-Match = %d %s, want 412", res.Code, res.Body)
	}
	if n := s.revisionCount(t); n != 1 {
		t.Errorf("refused imports stored %d revisions", n-1)
	}
}

// AN IMPORT IS ADMIN'S: a lead the route admits for their own units, and a
// person holding every app but not admin, are refused one before the body is
// read, and the flag takes exactly true or false.
func TestAnImportIsAnAdministratorsAndItsFlagIsStrict(t *testing.T) {
	t.Parallel()
	s := leadSurface(t)
	before := s.revisionCount(t)
	res := doAs(t, s, platformLead(), http.MethodPut, "/config?import=true", "{not json",
		map[string]string{"X-Summary": "an import"})
	if res.Code != http.StatusForbidden || decode(t, res)["error"] != string(httpjson.CodeUnauthorized) {
		t.Errorf("a lead's import = %d %s, want 403 unauthorized", res.Code, res.Body)
	}
	member := iam.Principal{ID: uuid.New(), Login: "sam.member", Kind: iam.KindPerson,
		Stage: iam.StageActive, Seat: "designer", Grants: []iam.Grant{iam.GrantWorklet, iam.GrantDoclet},
		ReauthAt: time.Now().Add(time.Hour)}
	res = doAs(t, s, member, http.MethodPut, "/config?import=true", "{not json",
		map[string]string{"X-Summary": "an import"})
	if res.Code != http.StatusForbidden || decode(t, res)["error"] != string(httpjson.CodeUnauthorized) {
		t.Errorf("a member's import = %d %s, want 403 unauthorized", res.Code, res.Body)
	}
	for _, query := range []string{"import=yes", "import=true&import=true", "import="} {
		res := s.do(t, http.MethodPut, "/config?"+query, leadDoc, map[string]string{"X-Summary": "x"})
		if res.Code != http.StatusBadRequest || decode(t, res)["error"] != string(httpjson.CodeInvalidQuery) {
			t.Errorf("PUT /config?%s = %d %s, want 400 invalid_query", query, res.Code, res.Body)
		}
	}
	if after := s.revisionCount(t); after != before {
		t.Errorf("a refused import stored %d revisions", after-before)
	}
}

// THE DASHBOARD NEVER IMPORTS. Every write it makes is an edit (the org
// builder, the Principles editor, a setting), judged on what it changes; the
// import mode is the CLI's, for a restored file. A screen sending it would
// write a charter fault it was meant to refuse.
func TestTheDashboardNeverSendsAnImport(t *testing.T) {
	t.Parallel()
	read := 0
	err := sourcetree.Walk(clientsource.Tree(t), func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		source := strings.HasSuffix(path, ".ts") || strings.HasSuffix(path, ".tsx")
		if d.IsDir() || !source || strings.Contains(path, ".test.") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		read++
		if strings.Contains(string(raw), "import=") {
			t.Errorf("%s sends PUT /config's import mode, which is the CLI's alone", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if read < 100 {
		t.Fatalf("read %d dashboard sources, so the walk found the wrong tree", read)
	}
}
