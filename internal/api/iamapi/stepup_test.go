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

// EVERY DIRECTORY WRITE ASKS FOR A RECENT PROOF, IN THE WINDOW ITS GESTURE
// NEEDS — and no read asks for one.
//
// Walked over every route the surface mounts, with a caller holding every
// grant whose proof is two hours old: a write is refused `step_up_required`
// naming its window before its handler runs, a read is served. Every directory
// write asks inside `step_up_sensitive` — each changes who holds authority or
// how they prove it, hands over a bearer value, or cannot be taken back —
// except ending your own sessions, which asks for nothing because it is the
// first thing somebody does on finding an intruder in their account. The map
// is held against the mount in BOTH directions, so a route added without
// deciding its window fails here.
func TestEveryDirectoryWriteAsksForAProofInItsWindow(t *testing.T) {
	t.Parallel()
	want := map[string]iam.Recency{
		"GET /iam/people":                  iam.RecencyAny,
		"POST /iam/people":                 iam.RecencySensitive,
		"GET /iam/people/{id}":             iam.RecencyAny,
		"PATCH /iam/people/{id}":           iam.RecencySensitive,
		"DELETE /iam/people/{id}":          iam.RecencySensitive,
		"POST /iam/invitations":            iam.RecencySensitive,
		"GET /iam/people/{id}/sessions":    iam.RecencyAny,
		"DELETE /iam/people/{id}/sessions": iam.RecencyAny,
		"POST /iam/people/{id}/mfa/reset":  iam.RecencySensitive,
		"GET /iam/credentials":             iam.RecencyAny,
		"POST /iam/credentials":            iam.RecencySensitive,
		"DELETE /iam/credentials/{id}":     iam.RecencySensitive,
		"POST /iam/invalidate-all":         iam.RecencySensitive,
		"GET /iam/check":                   iam.RecencyAny,
		"POST /iam/bootstrap-code":         iam.RecencySensitive,
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
		ReauthAt:          time.Now().Add(-time.Hour),
		SensitiveReauthAt: time.Now().Add(-105 * time.Minute)}
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

// A PROOF FORTY MINUTES OLD MAY NOT CHANGE ANYBODY'S GRANTS, AND ONE A MINUTE
// OLD MAY.
//
// Forty minutes is inside `step_up` and outside `step_up_sensitive`, so it is
// the proof that tells the two windows apart: an edit of somebody's grants is
// refused naming the sensitive window, and never reaches the writer. The
// control is the same administrator a minute after proving who they are.
func TestAProofFortyMinutesOldMayNotChangeAnybodysGrants(t *testing.T) {
	t.Parallel()
	edit := func(p iam.Principal) (answered, []string) {
		r := newRig(t)
		got := r.as(p, http.MethodPatch, "/iam/people/"+bob.String(),
			map[string]any{"grants": []string{string(iam.GrantStateRead)}})
		return got, r.writer.calls
	}
	older := administrator()
	proof := time.Now().Add(-40 * time.Minute)
	older.ReauthAt, older.SensitiveReauthAt = proof.Add(time.Hour),
		proof.Add(15*time.Minute)
	got, calls := edit(older)
	if got.status != http.StatusForbidden ||
		got.body["error"] != string(httpjson.CodeStepUpRequired) ||
		got.body[authz.DetailWindow] != string(iam.RecencySensitive) {
		t.Errorf("a grant edit on a proof forty minutes old answered %d %v, "+
			"want 403 step_up_required naming step_up_sensitive", got.status,
			got.body)
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
