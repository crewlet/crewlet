package setupapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/authz"
)

// A PASTE IS A CREDENTIAL WRITE, decided before its body is read: a caller the
// guard refuses secrets.write is answered 403 with a body past the limit, never
// the 413 a read of it would give. Every caller this build admits to the route
// but refuses secrets.write is refused here, so the guard is split by hand.
func TestAPasteIsRefusedACallerWithoutSecretsWriteBeforeItsBody(t *testing.T) {
	t.Parallel()
	s := &Service{guard: func(_ *http.Request, p authz.Policy) authz.Decision {
		return authz.Decision{Allowed: p.Action != authz.ActionSecretWrite, Reason: authz.ReasonEverybody}
	}}
	huge := `{"value": "` + strings.Repeat("x", MaxBody+1) + `"}`
	r := httptest.NewRequest(http.MethodPost, "/setup/integrations/expo/access/seats/qa/expo/credential",
		strings.NewReader(huge))
	r.SetPathValue("kind", "expo")
	r.SetPathValue("handle", "qa")
	r.SetPathValue("lane", "expo")
	w := httptest.NewRecorder()
	s.accessCredential(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("a paste without secrets.write = %d %s, want 403 before the body is read", w.Code, w.Body)
	}
	if strings.Contains(w.Body.String(), "xxxx") {
		t.Errorf("the refusal echoes the body: %s", w.Body)
	}
}
