package configapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
)

// refusal is what a refused configuration document answers.
type refusal struct {
	Error    string           `json:"error"`
	Detail   string           `json:"detail"`
	Hint     string           `json:"hint"`
	Problems []config.Problem `json:"problems"`
	Derived  *config.Derived  `json:"derived"`
}

func refusalOf(t *testing.T, res *httptest.ResponseRecorder, status int, code string) refusal {
	t.Helper()
	if res.Code != status {
		t.Fatalf("status = %d, want %d: %s", res.Code, status, res.Body)
	}
	var body refusal
	if err := json.Unmarshal(res.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode the refusal: %v (%s)", err, res.Body)
	}
	if body.Error != code {
		t.Fatalf("error = %q, want %q: %s", body.Error, code, res.Body)
	}
	if body.Detail == "" || len(body.Problems) == 0 {
		t.Fatalf("the refusal carries no detail or no problems: %s", res.Body)
	}
	return body
}

// problemAt is the problem at path, or a failure naming what there was.
func problemAt(t *testing.T, body refusal, path string) config.Problem {
	t.Helper()
	var paths []string
	for _, p := range body.Problems {
		if p.Path == path {
			return p
		}
		paths = append(paths, p.Path)
	}
	t.Fatalf("no problem at %q; problems are at %q", path, paths)
	return config.Problem{}
}

// A REFUSED DOCUMENT SAYS WHERE, AS WELL AS WHAT.
//
// The detail is one line per failure for a person; the problems are the same
// failures located by path and segments and classified, for a client that puts
// each beside the field it is about. The derived hierarchy comes with every
// refusal of a document that parsed, because a person fixing a misspelled lead
// finds it in the chart it breaks, and with none that did not, because there is
// no hierarchy to report.
func TestARefusedDocumentCarriesLocatedProblems(t *testing.T) {
	t.Parallel()

	t.Run("a put the validator refuses", func(t *testing.T) {
		t.Parallel()
		s := newSurface(t, nil)
		s.seed(t, companyDoc, nil)
		res := s.do(t, http.MethodPut, "/config",
			strings.Replace(companyDoc, "handle: cto\n    llm: zulu", "handle: cto\n    llm: nowhere", 1), summaryHeader)
		body := refusalOf(t, res, http.StatusBadRequest, "validation_error")
		p := problemAt(t, body, "roles[1].llm")
		if p.Seat != "cto" || !reflect.DeepEqual(p.Segments, config.Path{"roles", 1, "llm"}) {
			t.Errorf("problem = %+v, want it about cto with its segments", p)
		}
		if !strings.Contains(body.Detail, p.Message) {
			t.Errorf("the problem's message %q is not a line of the detail %q", p.Message, body.Detail)
		}
		if body.Hint == "" || body.Derived == nil || len(body.Derived.Seats) != 2 {
			t.Errorf("hint = %q, derived = %+v, want both", body.Hint, body.Derived)
		}
	})

	t.Run("a put that does not parse", func(t *testing.T) {
		t.Parallel()
		s := newSurface(t, nil)
		res := s.do(t, http.MethodPut, "/config", companyDoc+"missionn: typo\n", summaryHeader)
		body := refusalOf(t, res, http.StatusBadRequest, "invalid_body")
		p := problemAt(t, body, "missionn")
		// The line in the text that was sent, counted from its first line.
		want := strings.Count(companyDoc, "\n") + 1
		if p.Kind != "unknown_field" || p.Line != want {
			t.Errorf("problem = %+v, want an unknown_field on line %d", p, want)
		}
		if body.Derived != nil {
			t.Errorf("a document that did not parse carries a hierarchy: %+v", body.Derived)
		}
	})

	t.Run("a patch naming an unknown key", func(t *testing.T) {
		t.Parallel()
		s := newSurface(t, nil)
		s.seed(t, companyDoc, nil)
		body := refusalOf(t, s.do(t, http.MethodPatch, "/config", `{"missionn": "typo"}`, summaryHeader),
			http.StatusBadRequest, "invalid_patch")
		if p := problemAt(t, body, "missionn"); p.Kind != "unknown_field" {
			t.Errorf("problem = %+v, want unknown_field", p)
		}
		if body.Derived != nil {
			t.Errorf("a patch that produced no document carries a hierarchy: %+v", body.Derived)
		}
	})

	// A failure found in the merged document names no line. The text a
	// line would count is the engine's own merge of the patch over the
	// stored document, which the caller never saw.
	t.Run("a patch whose merged document has the wrong shape", func(t *testing.T) {
		t.Parallel()
		s := newSurface(t, nil)
		s.seed(t, companyDoc, nil)
		body := refusalOf(t, s.do(t, http.MethodPatch, "/config", "roles: not-a-list\n", summaryHeader),
			http.StatusBadRequest, "invalid_patch")
		p := problemAt(t, body, "roles")
		if p.Line != 0 || strings.Contains(p.Message, "(line") || strings.Contains(body.Detail, "(line") {
			t.Errorf("problem = %+v, detail = %q, want no line counted in the engine's merge", p, body.Detail)
		}
	})

	// An entity's problems are placed in the WHOLE document it was spliced
	// into, which is the document validated.
	t.Run("an entity write the validator refuses", func(t *testing.T) {
		t.Parallel()
		s := newSurface(t, nil)
		s.seed(t, companyDoc, nil)
		res := s.do(t, http.MethodPut, "/config/roles/cto",
			`{"name": "CTO", "handle": "cto", "llm": "nowhere"}`, summaryHeader)
		body := refusalOf(t, res, http.StatusBadRequest, "validation_error")
		if p := problemAt(t, body, "roles[1].llm"); p.Seat != "cto" {
			t.Errorf("problem = %+v, want it about cto", p)
		}
		if body.Derived == nil {
			t.Error("the refusal carries no hierarchy")
		}
	})

	t.Run("an entity body this kind cannot read", func(t *testing.T) {
		t.Parallel()
		s := newSurface(t, nil)
		s.seed(t, companyDoc, nil)
		res := s.do(t, http.MethodPut, "/config/roles/cto", `{"name": "CTO", "gaol": "x"}`, summaryHeader)
		body := refusalOf(t, res, http.StatusBadRequest, "invalid_body")
		if !strings.Contains(body.Problems[0].Message, "gaol") {
			t.Errorf("problems = %+v, want the unknown field named", body.Problems)
		}
	})

	t.Run("a revert the validator refuses", func(t *testing.T) {
		t.Parallel()
		s := newSurface(t, nil)
		refused := s.seedStored(t, companyDoc, refusedByThisBuild)
		s.seed(t, companyDoc, nil)
		body := refusalOf(t, s.do(t, http.MethodPost, "/config/revisions/"+refused+"/revert", "", nil),
			http.StatusBadRequest, "validation_error")
		if p := problemAt(t, body, "roles[1].llm"); p.Seat != "cto" {
			t.Errorf("problem = %+v, want it about cto", p)
		}
		if !strings.Contains(body.Hint, refused) || body.Derived == nil {
			t.Errorf("hint = %q, derived = %+v, want the revision named and the hierarchy", body.Hint, body.Derived)
		}
	})
}

// NO REFUSAL REPEATS A CREDENTIAL IT RESTORED, NOR ITS LENGTH.
//
// A document read from GET /config carries masks, and a write restores each
// from the stored revision before validating. So the values a refusal judges
// are credentials the caller was never shown, and anything a message says
// about one (the value, a fragment of it, how long it is) is answered to
// whoever sent the masks back. Every credential here is a literal its rule
// refuses, so each is judged, and each judgement is checked for the value.
func TestNoRefusalRepeatsARestoredCredentialOrItsLength(t *testing.T) {
	t.Parallel()
	const (
		signing    = "not-a-whsec-SIGNINGLITERAL"
		datadog    = "DATADOGLITERAL"
		confluence = "CONFLUENCELITERAL"
	)
	s := newSurface(t, nil)
	s.seedStored(t, companyDoc, func(document map[string]any) {
		integrations := document["integrations"].(map[string]any)
		integrations["gitlab"].(map[string]any)["signing_secret"] = signing
		integrations["datadog"] = map[string]any{
			"enabled": true, "site": "datadoghq.com", "webhook_token": datadog, "route_to": "ceo",
		}
		integrations["confluence"] = map[string]any{
			"url": "https://example.atlassian.net/wiki", "webhook_token": confluence,
		}
	})
	read := s.do(t, http.MethodGet, "/config", "", nil)
	if read.Code != http.StatusOK {
		t.Fatalf("GET /config = %d: %s", read.Code, read.Body)
	}
	for _, literal := range []string{signing, datadog, confluence} {
		if strings.Contains(read.Body.String(), literal) {
			t.Fatalf("the read served the literal %q, so this proves nothing", literal)
		}
	}

	for name, res := range map[string]*httptest.ResponseRecorder{
		"a put of the read, checked": s.do(t, http.MethodPut, "/config?dry_run=true", read.Body.String(), nil),
		"a put of the read":          s.do(t, http.MethodPut, "/config", read.Body.String(), summaryHeader),
		"a patch beside them":        s.do(t, http.MethodPatch, "/config", `{"mission": "unrelated"}`, summaryHeader),
	} {
		body := refusalOf(t, res, http.StatusBadRequest, "validation_error")
		judged := map[string]bool{}
		for _, p := range body.Problems {
			judged[p.Path] = true
		}
		for _, path := range []string{
			"integrations.gitlab.signing_secret", "integrations.datadog.webhook_token",
			"integrations.confluence.webhook_token",
		} {
			if !judged[path] {
				t.Errorf("%s: nothing judged %s, so its message was never checked: %s", name, path, res.Body)
			}
		}
		text := res.Body.String()
		for _, literal := range []string{signing, datadog, confluence} {
			if strings.Contains(text, literal) {
				t.Errorf("%s repeats the restored credential %q: %s", name, literal, text)
			}
			length := strconv.Itoa(len(literal))
			for _, p := range body.Problems {
				for _, word := range strings.FieldsFunc(p.Message, func(r rune) bool { return r < '0' || r > '9' }) {
					if word == length && !strings.Contains(p.Message, "at least "+length) {
						t.Errorf("%s: %q names %s, the length of a restored credential", name, p.Message, length)
					}
				}
			}
		}
	}
}
