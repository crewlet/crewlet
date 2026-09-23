package chartapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/chartapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/chart"
	"github.com/crewlet/crewlet/internal/iam"
)

// servingMux mounts on a real mux and remembers every pattern, so a walk
// reads the routes this surface ACTUALLY serves rather than a list beside them.
type servingMux struct {
	*http.ServeMux
	patterns []string
}

func (m *servingMux) Handle(pattern string, h http.Handler) {
	m.patterns = append(m.patterns, pattern)
	m.ServeMux.Handle(pattern, h)
}

// EVERY CHART WRITE ASKS FOR A RECENT PROOF — A LEAD'S EDIT OF THEIR OWN TEAM
// INCLUDED — AND NO READ DOES.
//
// As a person holding every grant whose proof is two hours old, over every
// route the surface mounts: a content edit, a rename, a structural batch and
// an import are refused `step_up_required` naming `step_up` before the body is
// read; every read — the chart, the history, the report, the import ledger a
// client polls — is served. A unit lead editing their team's purpose is
// changing what the company executes, which is why the relation rule's
// admission is not the end of the question.
func TestEveryChartWriteAsksForARecentProofAndNoReadDoes(t *testing.T) {
	t.Parallel()
	proof := time.Now().Add(-2 * time.Hour)
	stale := iam.Principal{ID: uuid.New(), Login: "jane.doe", Seat: "cto",
		Kind: iam.KindPerson, Stage: iam.StageActive, Grants: iam.AllGrants,
		ReauthAt: proof.Add(time.Hour), SensitiveReauthAt: proof.Add(15 * time.Minute)}
	svc, err := chartapi.New(chartapi.Options{
		Reader:    &reader{chart: nimbus()},
		Authority: func(string, chart.AuthorKind, []iam.Grant, chart.Provenance) chartapi.Writer { return &writer{} },
		Principal: resolved(func() iam.Principal { return stale }),
		Chart:     leads(),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	mux := &servingMux{ServeMux: http.NewServeMux()}
	if err := svc.Routes(mux); err != nil {
		t.Fatalf("Routes: %v", err)
	}
	writes := 0
	for _, pattern := range mux.patterns {
		method, path, _ := strings.Cut(pattern, " ")
		path = strings.NewReplacer("{key}", "engineering", "{handle}", "cto",
			"{revision}", "r1").Replace(path)
		req := httptest.NewRequest(method, path, strings.NewReader(`{"name":"x"}`))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		body := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		steppedUp := rec.Code == http.StatusForbidden &&
			body["error"] == string(httpjson.CodeStepUpRequired)
		if method == http.MethodGet {
			if steppedUp {
				t.Errorf("the read %s asked for a step-up", pattern)
			}
			continue
		}
		writes++
		if !steppedUp || body[authz.DetailWindow] != string(iam.RecencyStepUp) {
			t.Errorf("the write %s with a proof two hours old answered %d %v, "+
				"want 403 step_up_required naming step_up", pattern, rec.Code, body)
		}
	}
	if writes == 0 {
		t.Fatal("the surface mounted no write, so this walk certified nothing")
	}
}
