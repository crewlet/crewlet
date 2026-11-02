package authapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// ONLY A PERSON CONFIRMS OR CHANGES HOW THEY PROVE WHO THEY ARE.
//
// A principal that is no person — a session exchanged from a Tier A token, a
// service account — has no password to confirm and no second factor to enrol,
// whatever it is allowed. The refusal is the gesture's own rule rather than
// a missing proof, so it is `403 forbidden` with a sentence saying why, as
// `POST /auth/password` answers the same fact, and never `step_up_required`:
// that asks the caller for a proof none could give, and a client honouring it
// opened a step-up that refused the same caller again. Nothing is written.
// The control, a person whose proof is fresh, regenerates their codes.
//
// Mutation: answer the non-person arm of the step-up or of the proof routes
// `step_up_required` again and its row goes red.
func TestOnlyAPersonConfirmsOrChangesHowTheyProveIt(t *testing.T) {
	t.Parallel()
	writer := &credentialCounter{}
	mux := http.NewServeMux()
	buildWith(t, bootstrapFor(t), func(o *authapi.Options) { o.Writer = writer }).Routes(mux)
	machine := iam.Principal{
		ID: uuid.Must(uuid.NewV7()), Login: "token:ops", Kind: iam.KindMachine,
		Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantAdmin},
		ReauthAt: clock.Add(time.Minute),
	}
	for _, path := range []string{"/auth/step-up", "/auth/totp", "/auth/totp/recovery"} {
		req := httptest.NewRequest(http.MethodPost, path,
			strings.NewReader(`{"password":"a-password-long-enough","code":"123456"}`))
		req = req.WithContext(iam.WithPrincipal(req.Context(), machine))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		var body map[string]any
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		_, window := body[authz.DetailWindow]
		if rec.Code != http.StatusForbidden || body["error"] != string(httpjson.CodeForbidden) ||
			body["detail"] == nil || window {
			t.Errorf("%s by %s answered %d %v, want 403 forbidden saying why and "+
				"naming no window", path, machine.Login, rec.Code, body)
		}
	}
	if n := writer.count(); n != 0 {
		t.Errorf("the writer was asked to change credentials %d times by a "+
			"principal that is no person", n)
	}

	req := httptest.NewRequest(http.MethodPost, "/auth/totp/recovery", nil)
	req = req.WithContext(iam.WithPrincipal(req.Context(), iam.Principal{
		ID: uuid.Must(uuid.NewV7()), Login: "jane.doe", Kind: iam.KindPerson,
		Stage: iam.StageActive, ReauthAt: clock.Add(time.Minute),
	}))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || writer.count() != 1 {
		t.Errorf("a person whose proof is fresh answered %d with %d writes, "+
			"want 200 and one", rec.Code, writer.count())
	}
}
