package setupapi

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/store"
)

// A CONDITION OF THE NODE IS A 503 ON /setup TOO, beside what was sealed.
//
// A submission that met a store too busy to take its revision, one closed
// while the node adopts a snapshot, or a request out of time was answered
// `500 internal_error`, which said the engine had broken: the config surface
// answers each as the condition it is, and a screen reads one answer for one
// state whichever surface it wrote through. What the submission sealed before
// it stopped is still named, since nothing may point at those credentials yet;
// a fault keeps its 500, and names them too (the control).
//
// Mutation: drop the condition's arm from refuse and the busy store answers
// 500; hand it no extra detail and wrote_secrets goes missing.
func TestAConditionOnASetupWriteIsNotAFault(t *testing.T) {
	t.Parallel()
	busy := fmt.Errorf("configapi: store the config: %w", store.ErrBusy)
	said, _ := builtin.ConditionSentence(busy)
	sealed := setup.Result{Secrets: []string{"GITLAB_TOKEN"}}
	for name, c := range map[string]struct {
		err    error
		status int
		says   string
		retry  bool
	}{
		"a store busy with other writes": {busy, http.StatusServiceUnavailable, said, true},
		"a fault":                        {errors.New("gitlab: 422 token rejected"), http.StatusInternalServerError, "", false},
	} {
		rec := httptest.NewRecorder()
		(&Service{}).refuse(rec, httptest.NewRequest(http.MethodPost, "/setup/integrations/gitlab", nil),
			c.err, sealed)
		var body struct {
			Detail       string   `json:"detail"`
			WroteSecrets []string `json:"wrote_secrets"`
		}
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != c.status || (c.says != "" && body.Detail != c.says) {
			t.Errorf("%s answered %d %s, want %d with %q", name, rec.Code, rec.Body, c.status, c.says)
		}
		if retry := rec.Header().Get("Retry-After") != ""; retry != c.retry {
			t.Errorf("%s: Retry-After %q, want one: %v", name, rec.Header().Get("Retry-After"), c.retry)
		}
		if !slices.Equal(body.WroteSecrets, sealed.Secrets) {
			t.Errorf("%s names wrote_secrets %v, want what the submission sealed, %v", name,
				body.WroteSecrets, sealed.Secrets)
		}
		if text := rec.Body.String(); strings.Contains(text, "store the config") ||
			strings.Contains(text, "token rejected") {
			t.Errorf("%s carried its error's own text: %s", name, text)
		}
	}
}
