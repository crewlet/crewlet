package secretsapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/secretsapi"
	"github.com/crewlet/crewlet/internal/authz"
	coordmem "github.com/crewlet/crewlet/internal/coord/memory"
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

// EVERY CREDENTIAL WRITE ASKS FOR A RECENT PROOF, REVEALING ONE ASKS FOR A
// MORE RECENT ONE, AND THE LISTING ASKS FOR NONE.
//
// Walked over every route the surface mounts, as a person holding every grant.
// A proof forty minutes old is inside `step_up` and outside
// `step_up_sensitive`: it writes and it may not reveal. Two hours old, every
// write is refused `step_up_required` naming `step_up` before its handler
// runs, and the reads — the listing and one secret's metadata — are served.
func TestACredentialWriteAsksForAProofAndARevealForARecentOne(t *testing.T) {
	t.Parallel()
	fleet := coordmem.NewFleet()
	svc, err := secretsapi.New(secretsapi.Options{
		Fleet: fleet, Cipher: cipherFor(t, "k1"), ActiveKeyID: "k1",
		Now: func() time.Time { return clock },
	})
	if err != nil {
		t.Fatalf("secretsapi.New: %v", err)
	}
	mux := &recordingMux{ServeMux: http.NewServeMux()}
	if err := svc.Routes(mux); err != nil {
		t.Fatalf("Routes: %v", err)
	}
	provedAgo := func(age time.Duration) iam.Principal {
		at := time.Now().Add(-age)
		return iam.Principal{ID: uuid.New(), Login: "jane.doe",
			Kind: iam.KindPerson, Stage: iam.StageActive, Grants: iam.AllGrants,
			ReauthAt: at.Add(time.Hour), SensitiveReauthAt: at.Add(15 * time.Minute)}
	}
	serve := func(p iam.Principal, method, path, body string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req = req.WithContext(iam.WithPrincipal(req.Context(), p))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		out := map[string]any{}
		_ = json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	steppedUp := func(code int, body map[string]any, window iam.Recency) bool {
		return code == http.StatusForbidden &&
			body["error"] == string(httpjson.CodeStepUpRequired) &&
			body[authz.DetailWindow] == string(window)
	}

	// SEEDED FRESH, so the reveal below has a value to be refused.
	if code, body := serve(provedAgo(time.Minute), http.MethodPut,
		"/secrets/KEY", "a-value"); code != http.StatusOK {
		t.Fatalf("seed: %d %v", code, body)
	}

	stale := provedAgo(2 * time.Hour)
	for _, pattern := range mux.patterns {
		method, path, _ := strings.Cut(pattern, " ")
		path = strings.ReplaceAll(path, "{name}", "KEY")
		code, body := serve(stale, method, path, "another-value")
		read := method == http.MethodGet
		switch {
		case read && body["error"] == string(httpjson.CodeStepUpRequired):
			t.Errorf("the read %s asked for a step-up (%d %v)", pattern, code, body)
		case !read && !steppedUp(code, body, iam.RecencyStepUp):
			t.Errorf("the write %s with a proof two hours old answered %d %v, "+
				"want 403 step_up_required naming step_up", pattern, code, body)
		}
	}

	// THE REVEAL IS THE SENSITIVE ONE, asked on top of the listing's verb.
	older := provedAgo(40 * time.Minute)
	if code, body := serve(older, http.MethodGet, "/secrets/KEY?reveal=true",
		""); !steppedUp(code, body, iam.RecencySensitive) {
		t.Errorf("a reveal on a proof forty minutes old answered %d %v, want "+
			"403 step_up_required naming step_up_sensitive", code, body)
	}
	if code, body := serve(older, http.MethodPut, "/secrets/KEY",
		"a-third-value"); code != http.StatusOK {
		t.Errorf("a write on a proof forty minutes old answered %d %v, want "+
			"200: it asks inside step_up, which is an hour", code, body)
	}
	// THE CONTROL: a fresh proof reveals.
	if code, body := serve(provedAgo(time.Minute), http.MethodGet,
		"/secrets/KEY?reveal=true", ""); code != http.StatusOK {
		t.Errorf("a reveal on a proof a minute old answered %d %v", code, body)
	}
}
