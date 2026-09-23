package setupapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/setupapi"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// recordingMux mounts on a real mux and remembers every pattern, so a walk
// reads the routes this surface ACTUALLY serves rather than a list beside them.
type recordingMux struct {
	*http.ServeMux
	patterns []string
}

func (m *recordingMux) Handle(pattern string, h http.Handler) {
	m.patterns = append(m.patterns, pattern)
	m.ServeMux.Handle(pattern, h)
}

// CONNECTING AN INTEGRATION ASKS FOR A RECENT PROOF, AND READING ONE DOES NOT.
//
// Every route the surface mounts, as a person holding every grant whose proof
// is two hours old: a write — a submission, a pass, a disconnect, an app — is
// refused `step_up_required` naming `step_up` before its handler runs, and a
// read is served. Walked over the mount, so a route added later is held to the
// same line without being listed here.
func TestConnectingAnIntegrationAsksForARecentProof(t *testing.T) {
	t.Parallel()
	svc := newService(t, setupapi.Options{ExternalBase: fixtureExternalBase,
		Now: func() time.Time { return pinned }})
	mux := &recordingMux{ServeMux: http.NewServeMux()}
	if err := svc.Routes(mux); err != nil {
		t.Fatalf("Routes: %v", err)
	}
	proof := time.Now().Add(-2 * time.Hour)
	stale := iam.Principal{ID: uuid.New(), Login: "jane.doe",
		Kind: iam.KindPerson, Stage: iam.StageActive, Grants: iam.AllGrants,
		ReauthAt: proof.Add(time.Hour), SensitiveReauthAt: proof.Add(15 * time.Minute)}
	writes := 0
	for _, pattern := range mux.patterns {
		method, path, _ := strings.Cut(pattern, " ")
		path = strings.NewReplacer("{kind}", "gitlab", "{id}", "a-run").Replace(path)
		req := httptest.NewRequest(method, path, strings.NewReader("{}"))
		req = req.WithContext(iam.WithPrincipal(req.Context(), stale))
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
