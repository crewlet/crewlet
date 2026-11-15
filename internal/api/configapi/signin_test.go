package configapi_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/config"
)

// A SIGN-IN VALUE PAST THIS NODE'S LIMIT IS REFUSED AT ITS DOOR, check and save
// alike, in a person's words; a value inside it saves (ADR-0152).
// Mutation: drop the limits from the write path and the month is stored.
func TestASignInValuePastThisNodesLimitIsRefusedAtItsDoor(t *testing.T) {
	t.Parallel()
	s := newSurfaceWith(t, func(o *configapi.Options) {
		o.SignInLimits = config.SignInLimits{MaxSessionLifetimeRaw: "168h"}
	})
	s.seed(t, companyDoc)
	for _, path := range []string{"/config?dry_run=true", "/config"} {
		res := s.do(t, http.MethodPatch, path, `{"sign_in": {"session_lifetime_seconds": 2592000}}`,
			map[string]string{"X-Summary": "Sessions last a month"})
		if res.Code != http.StatusBadRequest {
			t.Fatalf("PATCH %s past the limit = %d, want 400: %s", path, res.Code, res.Body)
		}
		var answer struct {
			Problems []config.Problem `json:"problems"`
		}
		if err := json.Unmarshal(res.Body.Bytes(), &answer); err != nil {
			t.Fatalf("decode: %v (%s)", err, res.Body)
		}
		if len(answer.Problems) != 1 || answer.Problems[0].Path != config.SignInSessionLifetimeKey ||
			!strings.Contains(answer.Problems[0].Said, "at most 7 days") ||
			answer.Problems[0].Range == nil || !answer.Problems[0].Range.OperatorMax {
			t.Errorf("PATCH %s answered %+v, want one refusal at the lifetime saying the "+
				"operator's 7 days, with the operator's range", path, answer.Problems)
		}
	}
	res := s.do(t, http.MethodPatch, "/config", `{"sign_in": {"session_lifetime_seconds": 86400}}`,
		map[string]string{"X-Summary": "Sessions last a day"})
	if res.Code != http.StatusCreated {
		t.Errorf("a lifetime inside the limit = %d, want 201: %s", res.Code, res.Body)
	}
}

// A WEAKENING SIGN-IN CHANGE IS CHECKED WITH A WARNING IT INTRODUCES, which a
// screen asks a person to confirm; the company as it stands raises none.
// Mutation: drop config.SignInWeakens from the write path and none is answered.
func TestAWeakeningSignInChangeIsWarnedOfIt(t *testing.T) {
	t.Parallel()
	s := newCountedSurface(t)
	s.seed(t, companyDoc)
	warned := func(body string) []string {
		t.Helper()
		res := s.do(t, http.MethodPatch, "/config?dry_run=true", body, nil)
		if res.Code != http.StatusOK {
			t.Fatalf("dry run of %s = %d, want 200: %s", body, res.Code, res.Body)
		}
		var answer struct {
			Warnings []struct {
				config.Warning
				Introduced bool `json:"introduced"`
			} `json:"warnings"`
		}
		if err := json.Unmarshal(res.Body.Bytes(), &answer); err != nil {
			t.Fatalf("decode: %v (%s)", err, res.Body)
		}
		var out []string
		for _, w := range answer.Warnings {
			if strings.HasPrefix(w.Path, "sign_in.") && w.Introduced {
				out = append(out, w.Message)
			}
		}
		return out
	}
	if got := warned(`{"sign_in": {"step_up_seconds": 14400}}`); len(got) != 1 ||
		!strings.Contains(got[0], "4 hours instead of 1 hour") {
		t.Errorf("lengthening the step-up window warns %q, want what it allows", got)
	}
	if got := warned(`{"sign_in": {"step_up_seconds": 600}}`); len(got) != 0 {
		t.Errorf("shortening the step-up window warns %q", got)
	}
	if got := warned(`{}`); len(got) != 0 {
		t.Errorf("a check of the company as it stands warns %q", got)
	}
}
