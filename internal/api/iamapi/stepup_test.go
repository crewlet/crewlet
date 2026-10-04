package iamapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/httpjson"
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

// EVERY DIRECTORY WRITE ASKS FOR A RECENT PROOF — and no read asks for one.
//
// Walked over every route the surface mounts, with a caller holding every
// grant whose proof is two hours old: a write is refused `step_up_required`
// naming `step_up` before its handler runs, a read is served. Ending your own
// sessions asks for nothing, because it is the first thing somebody does on
// finding an intruder in their account (the other arm of that route has a case
// of its own below). The map is held against the mount in BOTH directions, so a
// route added without deciding whether it asks for a proof fails here.
func TestEveryDirectoryWriteAsksForAProof(t *testing.T) {
	t.Parallel()
	want := map[string]iam.Recency{
		"GET /iam/people":                  iam.RecencyAny,
		"POST /iam/people":                 iam.RecencyStepUp,
		"GET /iam/people/{id}":             iam.RecencyAny,
		"PATCH /iam/people/{id}":           iam.RecencyStepUp,
		"DELETE /iam/people/{id}":          iam.RecencyStepUp,
		"POST /iam/invitations":            iam.RecencyStepUp,
		"GET /iam/people/{id}/sessions":    iam.RecencyAny,
		"DELETE /iam/people/{id}/sessions": iam.RecencyAny,
		"POST /iam/people/{id}/mfa/reset":  iam.RecencyStepUp,
		"GET /iam/credentials":             iam.RecencyAny,
		"POST /iam/credentials":            iam.RecencyStepUp,
		"DELETE /iam/credentials/{id}":     iam.RecencyStepUp,
		"POST /iam/invalidate-all":         iam.RecencyStepUp,
		"GET /iam/check":                   iam.RecencyAny,
		"GET /iam/seats":                   iam.RecencyAny,
		"GET /iam/node-tokens":             iam.RecencyAny,
		"GET /iam/audit":                   iam.RecencyAny,
	}
	r := newRig(t)
	mux := &recordingMux{ServeMux: http.NewServeMux()}
	if err := r.service.Routes(mux); err != nil {
		t.Fatalf("mount: %v", err)
	}
	for pattern := range want {
		if !slices.Contains(mux.patterns, pattern) {
			t.Errorf("%s is decided here and the surface does not mount it", pattern)
		}
	}
	stale := iam.Principal{ID: alice, Login: "alice.admin",
		Kind: iam.KindPerson, Stage: iam.StageActive, Grants: iam.AllGrants,
		ReauthAt: time.Now().Add(-time.Hour)}
	for _, pattern := range mux.patterns {
		window, decided := want[pattern]
		if !decided {
			t.Errorf("the surface mounts %s and nothing here says which proof "+
				"it asks for", pattern)
			continue
		}
		method, path, _ := strings.Cut(pattern, " ")
		path = strings.ReplaceAll(path, "{id}", alice.String())
		req := httptest.NewRequest(method, path, strings.NewReader("{}"))
		req = req.WithContext(iam.WithPrincipal(req.Context(), stale))
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		body := decodeBody(t, rec)
		steppedUp := rec.Code == http.StatusForbidden &&
			body["error"] == string(httpjson.CodeStepUpRequired)
		switch {
		case window.Demands() && !steppedUp:
			t.Errorf("%s with a proof two hours old answered %d %v, want 403 "+
				"step_up_required", pattern, rec.Code, body["error"])
		case window.Demands() && body[authz.DetailWindow] != string(window):
			t.Errorf("%s named the window %v, want %s", pattern,
				body[authz.DetailWindow], window)
		case !window.Demands() && steppedUp:
			t.Errorf("%s asked a caller holding every grant to step up, and "+
				"it is decided to need no proof", pattern)
		}
	}
}

// decodeBody reads one answer's JSON body, or an empty map for none.
func decodeBody(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	out := map[string]any{}
	if rec.Body.Len() == 0 {
		return out
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("the answer is not JSON: %v (%s)", err, rec.Body)
	}
	return out
}

// A PROOF TWO HOURS OLD MAY NOT CHANGE ANYBODY'S GRANTS, AND ONE A MINUTE OLD
// MAY.
//
// An edit of somebody's grants is refused naming the window, and never reaches
// the writer. The control is the same administrator a minute after proving
// who they are.
func TestAStaleProofMayNotChangeAnybodysGrants(t *testing.T) {
	t.Parallel()
	edit := func(p iam.Principal) (answered, []string) {
		r := newRig(t)
		got := r.as(p, http.MethodPatch, "/iam/people/"+bob.String(),
			map[string]any{"grants": []string{string(iam.GrantStateRead)}})
		return got, r.writer.calls
	}
	older := administrator()
	older.ReauthAt = time.Now().Add(-2 * time.Hour).Add(time.Hour)
	got, calls := edit(older)
	if got.status != http.StatusForbidden ||
		got.body["error"] != string(httpjson.CodeStepUpRequired) ||
		got.body[authz.DetailWindow] != string(iam.RecencyStepUp) {
		t.Errorf("a grant edit on a proof two hours old answered %d %v, "+
			"want 403 step_up_required naming step_up", got.status, got.body)
	}
	if len(calls) != 0 {
		t.Errorf("the refused edit reached the writer: %v", calls)
	}
	if got, calls := edit(administrator()); got.status != http.StatusOK ||
		len(calls) == 0 {
		t.Errorf("the same edit a minute after proving answered %d %v with "+
			"writes %v, so the refusal above says nothing", got.status,
			got.body, calls)
	}
}

// ENDING A COLLEAGUE'S SESSIONS ASKS THE ADMINISTRATOR FOR A RECENT PROOF, AND
// ENDING YOUR OWN ASKS FOR NONE — through the route, on one verb.
//
// The walk above names the caller's own id in every path, so it sees only the
// self arm of `DELETE /iam/people/{id}/sessions`; this is the other arm. An
// administrator whose proof is two hours old is refused `step_up_required`
// naming `step_up`, and the revocation — which would end every
// session and every machine token the colleague holds — never reaches the
// writer. The controls: the same administrator a minute after proving, and
// the colleague ending their own sessions on a week-old cookie.
func TestEndingAColleaguesSessionsAsksTheAdministratorForAProof(t *testing.T) {
	t.Parallel()
	aged := func(p iam.Principal, age time.Duration) iam.Principal {
		p.ReauthAt = time.Now().Add(-age).Add(time.Hour)
		return p
	}
	end := func(p iam.Principal, whose string) (answered, []string) {
		r := newRig(t)
		got := r.as(p, http.MethodDelete, "/iam/people/"+whose+"/sessions", nil)
		return got, r.writer.calls
	}
	got, calls := end(aged(administrator(), 2*time.Hour), bob.String())
	if got.status != http.StatusForbidden ||
		got.body["error"] != string(httpjson.CodeStepUpRequired) ||
		got.body[authz.DetailWindow] != string(iam.RecencyStepUp) {
		t.Errorf("a stale administrator ending a colleague's sessions answered "+
			"%d %v, want 403 step_up_required naming step_up", got.status, got.body)
	}
	if len(calls) != 0 {
		t.Errorf("the refused revocation reached the writer: %v", calls)
	}
	if got, calls := end(administrator(), bob.String()); got.status != http.StatusOK ||
		!slices.Contains(calls, "revoke") {
		t.Errorf("the same administrator a minute after proving answered %d %v "+
			"with writes %v, so the refusal above says nothing", got.status,
			got.body, calls)
	}
	if got, calls := end(aged(ordinary(), 7*24*time.Hour), bob.String()); got.status != http.StatusOK ||
		!slices.Contains(calls, "revoke") {
		t.Errorf("a person ending their own sessions on a week-old cookie "+
			"answered %d %v with writes %v, want it served with no proof asked",
			got.status, got.body, calls)
	}
}
