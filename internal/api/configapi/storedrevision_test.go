package configapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// A stored revision is not a submitted document. It was valid under the build
// that wrote it, and a later build, or an older peer still activating during a
// rolling upgrade, can leave one in the store that this build refuses. Every
// case here pins one half of the same rule: such a revision stays READABLE and
// REPLACEABLE through this surface, and nothing that would RUN it accepts it.

// refusedByThisBuild makes a stored document this build's validator refuses:
// a seat naming a model provider the company does not configure.
func refusedByThisBuild(document map[string]any) {
	roles, _ := document["roles"].([]any)
	seat, _ := roles[1].(map[string]any)
	seat["llm"] = "nonexistent"
}

// A REVISION THIS BUILD REFUSES IS STILL SERVED.
//
// When the stored-form reader validated, this revision answered 500 on every
// read, so the builder and an operator's own tooling could not even load the
// document they needed to fix.
func TestAStoredRevisionThisBuildRefusesIsStillServed(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	id := s.seedStored(t, companyDoc, refusedByThisBuild)

	for _, path := range []string{
		"/config",
		"/config?format=yaml",
		"/config/revisions/" + id,
		"/config/revisions/" + id + "/diff?against=active",
		"/config/references",
		"/config/roles/cto",
	} {
		res := s.do(t, http.MethodGet, path, "", nil)
		if res.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want 200: %s", path, res.Code, res.Body)
		}
	}
	res := s.do(t, http.MethodGet, "/config", "", nil)
	if !strings.Contains(res.Body.String(), "nonexistent") {
		t.Errorf("GET /config did not serve the stored document as it is: %s", res.Body)
	}
}

// A CORRECTED PUT REPLACES IT.
//
// The PUT opens the stored revision as its prior, to restore the masks the
// caller was shown. A prior that failed validation used to fail the open, so
// the one write that fixes the company answered 500.
func TestACorrectedPutReplacesARevisionThisBuildRefuses(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	s.seedStored(t, companyDoc, refusedByThisBuild)

	res := s.do(t, http.MethodPut, "/config", companyDoc,
		map[string]string{"X-Summary": "correct the model"})
	if res.Code != http.StatusCreated {
		t.Fatalf("PUT = %d, want 201: %s", res.Code, res.Body)
	}
	if document := s.activeDocument(t); strings.Contains(document, "nonexistent") {
		t.Errorf("the corrected document was not stored: %s", document)
	}
}

// A CORRECTING PATCH REPLACES IT, AND ONE THAT CORRECTS NOTHING IS REFUSED.
//
// Both halves, because a PATCH that merely opened the prior leniently and then
// skipped validation would pass the first and store a company no node runs.
func TestAPatchReplacesARevisionThisBuildRefusesOnlyWhenItCorrectsIt(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	s.seedStored(t, companyDoc, refusedByThisBuild)

	unrelated := s.do(t, http.MethodPatch, "/config", `{"mission": "unrelated"}`, summaryHeader)
	if unrelated.Code != http.StatusBadRequest {
		t.Fatalf("a PATCH leaving the company invalid = %d, want 400: %s",
			unrelated.Code, unrelated.Body)
	}
	if got := decode(t, unrelated)["error"]; got != "validation_error" {
		t.Errorf("error = %v, want validation_error", got)
	}

	fixed := s.do(t, http.MethodPatch, "/config",
		`{"roles": [{"name": "CEO", "handle": "ceo", "llm": "zulu"},
		            {"name": "CTO", "handle": "cto", "llm": "zulu"}]}`,
		summaryHeader)
	if fixed.Code != http.StatusCreated {
		t.Fatalf("a correcting PATCH = %d, want 201: %s", fixed.Code, fixed.Body)
	}
	if document := s.activeDocument(t); strings.Contains(document, "nonexistent") {
		t.Errorf("the correction did not land: %s", document)
	}
}

// RELOAD AND REVERT ACTIVATE, SO THEY VALIDATE.
//
// Opening a revision holds it to no rule, and both of these re-publish what
// they opened for every node to apply. Revert used to report a revision that
// failed validation as sealed under a missing key, because the open that
// validated was the only thing that could fail.
func TestReloadAndRevertRefuseARevisionThisBuildCannotRun(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	refused := s.seedStored(t, companyDoc, refusedByThisBuild)

	reload := s.do(t, http.MethodPost, "/config/reload", "", nil)
	if reload.Code != http.StatusBadRequest {
		t.Fatalf("reload = %d, want 400: %s", reload.Code, reload.Body)
	}
	if got := decode(t, reload)["error"]; got != "validation_error" {
		t.Errorf("reload error = %v, want validation_error", got)
	}

	s.seed(t, companyDoc, nil)
	revert := s.do(t, http.MethodPost, "/config/revisions/"+refused+"/revert", "", nil)
	if revert.Code != http.StatusBadRequest {
		t.Fatalf("revert = %d, want 400: %s", revert.Code, revert.Body)
	}
	body := decode(t, revert)
	if body["error"] != "validation_error" {
		t.Errorf("revert error = %v, want validation_error", body["error"])
	}
	if detail, _ := body["detail"].(string); !strings.Contains(detail, "nonexistent") {
		t.Errorf("the refusal does not name what is wrong: %v", body)
	}
	if document := s.activeDocument(t); strings.Contains(document, "nonexistent") {
		t.Errorf("a refused revert changed the active revision: %s", document)
	}
}

// A PATCH OVER A PEER-EXTENDED DOCUMENT RESTORES ITS MASKS BEFORE VALIDATING.
//
// The strict reader refuses the merged document for the peer's field, and the
// fallback reads it with the stored-form decoder. That fallback validated, and
// validation runs before the masks are restored, so any PATCH carrying a roles
// array read from GET /config (every seat's literal credential masked) was
// refused as an invalid patch naming the masks.
func TestAPatchOverAPeerExtendedDocumentRestoresMasksBeforeValidating(t *testing.T) {
	t.Parallel()
	const literal = "per-seat-literal-token"
	doc := companyDoc + `
mcp_servers:
  - name: tracker
    command: tracker-mcp
    shared: false
`
	doc = strings.Replace(doc, "    handle: ceo\n    llm: zulu\n",
		"    handle: ceo\n    llm: zulu\n    mcp_env:\n      tracker: {TOKEN: "+literal+"}\n", 1)
	s := newSurface(t, nil)
	s.seedStored(t, doc, func(document map[string]any) {
		document["a_setting_from_a_newer_build"] = map[string]any{"depth": 3}
	})

	read := s.do(t, http.MethodGet, "/config", "", nil)
	if read.Code != http.StatusOK {
		t.Fatalf("GET /config = %d: %s", read.Code, read.Body)
	}
	var served map[string]any
	if err := json.Unmarshal(read.Body.Bytes(), &served); err != nil {
		t.Fatal(err)
	}
	roles, err := json.Marshal(map[string]any{"roles": served["roles"]})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(roles), "__redacted__") {
		t.Fatalf("the roles carry no mask, so this proves nothing: %s", roles)
	}

	res := s.do(t, http.MethodPatch, "/config", string(roles), summaryHeader)
	if res.Code != http.StatusCreated {
		t.Fatalf("PATCH = %d, want 201: %s", res.Code, res.Body)
	}
	document := s.activeDocument(t)
	if !strings.Contains(document, literal) {
		t.Errorf("the masked credential was not restored: %s", document)
	}
	if !strings.Contains(document, "a_setting_from_a_newer_build") {
		t.Errorf("the peer's field was dropped: %s", document)
	}
}
