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
	"github.com/crewlet/crewlet/internal/iamdomain"
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
// naming its window before its handler runs, a read is served. The windows are
// the design's: every directory write asks `step_up`, and `step_up_sensitive`
// is kept for the gestures that change an authority somebody already holds or
// how they prove it — an edit of their row, a second-factor reset — and for
// ending every session in the company. Ending your own sessions asks for
// nothing, because it is the first thing somebody does on finding an intruder
// in their account (the other arm of that route has a case of its own below),
// and revoking a credential asks the sensitive window from inside once it has
// read that the credential is how somebody signs in (a case below, too). The
// map is held against the mount in BOTH directions, so a route added without
// deciding its window fails here.
func TestEveryDirectoryWriteAsksForAProofInItsWindow(t *testing.T) {
	t.Parallel()
	want := map[string]iam.Recency{
		"GET /iam/people":                  iam.RecencyAny,
		"POST /iam/people":                 iam.RecencyStepUp,
		"GET /iam/people/{id}":             iam.RecencyAny,
		"PATCH /iam/people/{id}":           iam.RecencySensitive,
		"DELETE /iam/people/{id}":          iam.RecencyStepUp,
		"POST /iam/invitations":            iam.RecencyStepUp,
		"GET /iam/people/{id}/sessions":    iam.RecencyAny,
		"DELETE /iam/people/{id}/sessions": iam.RecencyAny,
		"POST /iam/people/{id}/mfa/reset":  iam.RecencySensitive,
		"GET /iam/credentials":             iam.RecencyAny,
		"POST /iam/credentials":            iam.RecencyStepUp,
		"DELETE /iam/credentials/{id}":     iam.RecencyStepUp,
		"POST /iam/invalidate-all":         iam.RecencySensitive,
		"GET /iam/check":                   iam.RecencyAny,
		"POST /iam/bootstrap-code":         iam.RecencyStepUp,
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

// REVOKING HOW SOMEBODY SIGNS IN ASKS THE SENSITIVE WINDOW; REVOKING A TOKEN
// ASKS THE ORDINARY ONE.
//
// `DELETE /iam/credentials/{id}` is mounted on the ordinary credential write,
// and once it has read which credential the id names it asks the sensitive
// verb for a password, a second factor, the recovery codes or a provider link:
// each of those is a second-factor reset by another door, and the reset
// itself asks the quarter-hour. So a proof forty minutes old withdraws a
// machine token and is refused a second factor naming `step_up_sensitive`,
// with nothing written; the control is the same caller a minute after proving.
// And a credential this node could not name when it chose the verb is not
// revoked on the ordinary one: the snapshot revokes a non-token only for a
// request the sensitive verb admitted.
//
// Mutation: drop the inner admission and the forty-minute proof revokes the
// factor; drop the snapshot's condition and the unnamed factor is revoked.
func TestRevokingHowSomebodySignsInAsksTheSensitiveWindow(t *testing.T) {
	t.Parallel()
	const factor, token = "018f3a9c-0000-7000-8000-0000000000f1",
		"018f3a9c-0000-7000-8000-0000000000f2"
	fortyMinutes := administrator()
	proof := time.Now().Add(-40 * time.Minute)
	fortyMinutes.ReauthAt, fortyMinutes.SensitiveReauthAt = proof.Add(time.Hour),
		proof.Add(15*time.Minute)
	revoke := func(p iam.Principal, id string, listed bool) (answered, *rig) {
		r := newRig(t)
		r.writer.held = []iamdomain.Credential{
			{ID: factor, Method: iamdomain.MethodTOTP},
			{ID: token, Method: iamdomain.MethodToken},
		}
		if listed {
			r.directory.creds = map[string][]iamdomain.CredentialRow{
				bob.String(): {
					{ID: factor, PersonID: bob.String(), Method: iamdomain.MethodTOTP},
					{ID: token, PersonID: bob.String(), Method: iamdomain.MethodToken},
				},
			}
		}
		got := r.as(p, http.MethodDelete,
			"/iam/credentials/"+id+"?person="+bob.String(), nil)
		return got, r
	}
	revokedIn := func(r *rig, id string) bool {
		for _, c := range r.writer.held {
			if c.ID == id {
				return !c.RevokedAt.IsZero()
			}
		}
		return false
	}

	got, r := revoke(fortyMinutes, factor, true)
	if got.status != http.StatusForbidden ||
		got.body["error"] != string(httpjson.CodeStepUpRequired) ||
		got.body[authz.DetailWindow] != string(iam.RecencySensitive) {
		t.Errorf("revoking a second factor on a proof forty minutes old "+
			"answered %d %v, want 403 step_up_required naming %s", got.status,
			got.body, iam.RecencySensitive)
	}
	if len(r.writer.calls) != 0 || revokedIn(r, factor) {
		t.Errorf("the refused revocation reached the writer: %v", r.writer.calls)
	}
	if got, r := revoke(fortyMinutes, token, true); got.status != http.StatusOK ||
		!revokedIn(r, token) {
		t.Errorf("revoking a machine token on the same proof answered %d %v, "+
			"want it revoked on the ordinary window", got.status, got.body)
	}
	if got, r := revoke(administrator(), factor, true); got.status != http.StatusOK ||
		!revokedIn(r, factor) {
		t.Errorf("revoking the factor a minute after proving answered %d %v, "+
			"so the refusal above says nothing", got.status, got.body)
	}
	if got, r := revoke(fortyMinutes, factor, false); revokedIn(r, factor) {
		t.Errorf("a factor this node could not name when it chose the verb was "+
			"revoked on the ordinary one (%d %v)", got.status, got.body)
	}
}

// ENDING A COLLEAGUE'S SESSIONS ASKS THE ADMINISTRATOR FOR A RECENT PROOF, AND
// ENDING YOUR OWN ASKS FOR NONE — through the route, on one verb.
//
// The walk above names the caller's own id in every path, so it sees only the
// self arm of `DELETE /iam/people/{id}/sessions`; this is the other arm. An
// administrator whose proof is two hours old is refused `step_up_required`
// naming the ordinary window, and the revocation — which would end every
// session and every machine token the colleague holds — never reaches the
// writer. The controls: the same administrator a minute after proving, and
// the colleague ending their own sessions on a week-old cookie.
func TestEndingAColleaguesSessionsAsksTheAdministratorForAProof(t *testing.T) {
	t.Parallel()
	aged := func(p iam.Principal, age time.Duration) iam.Principal {
		proof := time.Now().Add(-age)
		p.ReauthAt, p.SensitiveReauthAt = proof.Add(time.Hour),
			proof.Add(15*time.Minute)
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
