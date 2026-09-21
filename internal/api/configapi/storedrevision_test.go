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
// a delegate template naming a model provider the company does not configure.
//
// A SETTINGS RULE, deliberately. It used to be a SEAT naming a missing
// provider, which was the same rule one field along — but a seat is the org
// chart's now and a stored revision carries none, so the fixture was making a
// document this surface can no longer be given. A worker template is the same
// shape of mistake in the half a revision still holds.
func refusedByThisBuild(document map[string]any) {
	document["workers"] = map[string]any{
		"researcher": map[string]any{
			"description":   "reads sources and reports findings with citations",
			"system_prompt": "You research.",
			"model":         "nonexistent",
		},
	}
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
		`{"workers": {"researcher": {"description": "reads sources and reports
		    findings with citations", "system_prompt": "You research.",
		    "model": "zulu"}}}`,
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

// duplicateNamesDoc breaks both admission rules and no runnable one: two units
// called "Platform" in different departments, each holding a seat called
// "Engineer" on its own explicit handle. A build before those rules admitted
// it, and a company running on it runs.
//
// IT IS A PRE-SPLIT REVISION, which is the second thing it is for here: it
// carries `units:` inside the stored document, which is exactly the shape this
// door now refuses to be given BACK.
const duplicateNamesDoc = companyDoc + `
units:
  - name: Engineering
    children:
      - name: Platform
        roles:
          - {name: Engineer, handle: platform-engineer, llm: zulu}
  - name: Product
    children:
      - name: Platform
        roles:
          - {name: Engineer, handle: product-engineer, llm: zulu}
`

// oldChartDoc is a revision written before the chart's split, breaking no
// rule: it is what a patch that names only settings has to keep working over.
const oldChartDoc = companyDoc + `
units:
  - name: Engineering
    id: engineering
    roles:
      - {name: Engineer, handle: engineer, llm: zulu}
`

// A STORED REVISION CARRYING AN OLD CHART IS SERVED AND RE-ACTIVATED, AND
// EVERY WRITE THAT WOULD CARRY IT FORWARD IS REFUSED NAMING THE CHART.
//
// # What this case is now about
//
// It used to be about the two ADMISSION RULES on duplicate names: a company
// written before them stays readable and repairable, and no write may carry a
// duplicate forward. Those rules are the CHART's now, and they are checked
// where a chart is written — [config.Company.ValidateAdmission] over an
// authored file, and the chart's own batch validator over a record. This
// surface stopped deciding them, so a test asserting them here would be
// asserting a rule no code at this door runs.
//
// What is left is the half this door DOES decide, and it is the more
// important half: a pre-split revision is still a real revision. It is served,
// it is diffable, it is re-activatable — and the moment a caller sends any of
// it BACK, the write is refused by name, because storing it again would write
// a chart into a settings revision every node then refuses to apply.
func TestAnOldChartIsServedAndNeverWrittenBack(t *testing.T) {
	t.Parallel()

	t.Run("served", func(t *testing.T) {
		t.Parallel()
		s := newSurface(t, nil)
		s.seedStored(t, duplicateNamesDoc, func(map[string]any) {})
		// THE UNIT IS ADDRESSED BY ITS KEY, and this document declares
		// none — so the key is the one minted from its name when the
		// revision is decoded, which is what makes a company written
		// before the id rule addressable at all.
		for _, path := range []string{"/config", "/config?format=yaml", "/config/units/platform"} {
			if res := s.do(t, http.MethodGet, path, "", nil); res.Code != http.StatusOK {
				t.Errorf("GET %s = %d, want 200: %s", path, res.Code, res.Body)
			}
		}
	})

	t.Run("a PUT sending it back is refused naming the chart", func(t *testing.T) {
		t.Parallel()
		s := newSurface(t, nil)
		s.seedStored(t, duplicateNamesDoc, func(map[string]any) {})

		kept := s.do(t, http.MethodPut, "/config", duplicateNamesDoc,
			map[string]string{"X-Summary": "keep"})
		if kept.Code != http.StatusBadRequest {
			t.Fatalf("a PUT carrying the old chart = %d, want 400: %s", kept.Code, kept.Body)
		}
		body := decode(t, kept)
		if body["error"] != "chart_not_writable_here" {
			t.Errorf("error = %v, want chart_not_writable_here: %v", body["error"], body)
		}
		// THE SETTINGS ALONE STILL LAND, which is what makes the
		// refusal a redirection rather than a wall: the operator's own
		// half of the document is writable at this door exactly as it
		// always was.
		res := s.do(t, http.MethodPut, "/config", companyDoc,
			map[string]string{"X-Summary": "the settings alone"})
		if res.Code != http.StatusCreated {
			t.Fatalf("a settings-only PUT = %d, want 201: %s", res.Code, res.Body)
		}
	})

	t.Run("a PATCH naming a unit is refused and one naming a setting lands", func(t *testing.T) {
		t.Parallel()
		// A PRE-SPLIT REVISION THAT BREAKS NO RULE, unlike the duplicate
		// one above: a patch is validated as the merged document, so a
		// base carrying an admission break is refused for THAT whatever
		// the patch says — which would make the second half of this case
		// pass for the wrong reason.
		s := newSurface(t, nil)
		s.seedStored(t, oldChartDoc, func(map[string]any) {})

		chart := s.do(t, http.MethodPatch, "/config",
			`{"units": [{"name": "Engineering", "id": "engineering"}]}`, summaryHeader)
		if chart.Code != http.StatusBadRequest {
			t.Fatalf("a PATCH naming units = %d, want 400: %s", chart.Code, chart.Body)
		}
		if got := decode(t, chart)["error"]; got != "chart_not_writable_here" {
			t.Errorf("error = %v, want chart_not_writable_here", got)
		}
		// AND THE PATCH IS JUDGED ON WHAT THE CALLER SENT: this base
		// still holds a chart, and a patch that does not name it is
		// accepted with the chart carried through untouched. Judging
		// the MERGE would refuse an operator editing a mission for a
		// chart they neither sent nor can see.
		res := s.do(t, http.MethodPatch, "/config", `{"mission": "ship it"}`, summaryHeader)
		if res.Code != http.StatusCreated {
			t.Fatalf("a settings-only PATCH = %d, want 201: %s", res.Code, res.Body)
		}
		if document := s.activeDocument(t); !strings.Contains(document, "ship it") {
			t.Errorf("the patch did not land: %s", document)
		}
	})

	// A reload and a revert are applies, held to the runnable rules only: a
	// credential rotation on a company carrying an old chart must work,
	// because those two re-publish bytes that are already stored rather than
	// writing anything a caller sent.
	t.Run("a reload and a revert of it are accepted", func(t *testing.T) {
		t.Parallel()
		s := newSurface(t, nil)
		stored := s.seedStored(t, duplicateNamesDoc, func(map[string]any) {})
		if res := s.do(t, http.MethodPost, "/config/reload", "", nil); res.Code != http.StatusCreated {
			t.Fatalf("reload = %d, want 201: %s", res.Code, res.Body)
		}
		// Moved on through the API, so the fleet pointer and the store agree
		// on what the revert is built over.
		if res := s.do(t, http.MethodPut, "/config", companyDoc,
			map[string]string{"X-Summary": "move on"}); res.Code != http.StatusCreated {
			t.Fatalf("PUT = %d, want 201: %s", res.Code, res.Body)
		}
		res := s.do(t, http.MethodPost, "/config/revisions/"+stored+"/revert", "", nil)
		if res.Code != http.StatusCreated {
			t.Fatalf("revert = %d, want 201: %s", res.Code, res.Body)
		}
	})
}

// mustJSON renders a value as JSON, which is also valid YAML.
func mustJSON(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// A PATCH OVER A PEER-EXTENDED DOCUMENT RESTORES ITS MASKS BEFORE VALIDATING.
//
// The strict reader refuses the merged document for the peer's field, and the
// fallback reads it with the stored-form decoder. That fallback validated, and
// validation runs before the masks are restored, so any PATCH carrying a list
// read from GET /config (every member's literal credential masked) was refused
// as an invalid patch naming the masks.
func TestAPatchOverAPeerExtendedDocumentRestoresMasksBeforeValidating(t *testing.T) {
	t.Parallel()
	const literal = "per-server-literal-token"
	doc := companyDoc + `
mcp_servers:
  - name: tracker
    command: tracker-mcp
    shared: false
    env: {TOKEN: ` + literal + `}
`
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
	servers, err := json.Marshal(map[string]any{"mcp_servers": served["mcp_servers"]})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(servers), "__redacted__") {
		t.Fatalf("the servers carry no mask, so this proves nothing: %s", servers)
	}

	res := s.do(t, http.MethodPatch, "/config", string(servers), summaryHeader)
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
