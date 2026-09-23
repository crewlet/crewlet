package configapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// A CONFIGURATION WRITE ASKS FOR A RECENT PROOF, AND A READ DOES NOT.
//
// As a person holding every grant whose proof is two hours old: every way of
// changing the company document — a whole replacement, a patch, a reload, a
// revert and a write of every entity this surface writes — is refused
// `step_up_required` naming `step_up` before anything is read, and the reads
// are served. The routes are this surface's own two verbs, so the table's walk
// in internal/authz holds every other route mounted on them; this holds the
// surface to having mounted each on the right one.
func TestAConfigurationWriteAsksForARecentProof(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	proof := time.Now().Add(-2 * time.Hour)
	stale := iam.Principal{ID: uuid.New(), Login: "jane.doe",
		Kind: iam.KindPerson, Stage: iam.StageActive, Grants: iam.AllGrants,
		ReauthAt: proof.Add(time.Hour), SensitiveReauthAt: proof.Add(15 * time.Minute)}
	serve := func(method, path string) (int, map[string]any) {
		req := httptest.NewRequest(method, path, strings.NewReader(companyJSONDoc))
		req = req.WithContext(iam.WithPrincipal(req.Context(), stale))
		rec := httptest.NewRecorder()
		s.mux.ServeHTTP(rec, req)
		body := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body
	}
	writes := [][2]string{
		{http.MethodPut, "/config"},
		{http.MethodPatch, "/config"},
		{http.MethodPost, "/config/reload"},
		{http.MethodPost, "/config/revisions/a-revision/revert"},
	}
	for _, kind := range configapi.WritableEntityKinds() {
		writes = append(writes, [2]string{http.MethodPut, "/config/" + kind + "/an-id"})
	}
	for _, w := range writes {
		code, body := serve(w[0], w[1])
		if code != http.StatusForbidden ||
			body["error"] != string(httpjson.CodeStepUpRequired) ||
			body[authz.DetailWindow] != string(iam.RecencyStepUp) {
			t.Errorf("%s %s with a proof two hours old answered %d %v, want "+
				"403 step_up_required naming step_up", w[0], w[1], code, body)
		}
	}
	for _, path := range []string{"/config", "/config/references",
		"/config/revisions"} {
		if _, body := serve(http.MethodGet, path); body["error"] ==
			string(httpjson.CodeStepUpRequired) {
			t.Errorf("the read GET %s asked for a step-up", path)
		}
	}
}
