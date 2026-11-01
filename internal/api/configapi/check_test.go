package configapi_test

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// policyRecorder is a mux that keeps the policy every route was mounted with.
type policyRecorder struct {
	*http.ServeMux
	policies map[string]authz.Policy
}

func (m *policyRecorder) MountedWith(pattern string, p authz.Policy) {
	m.policies[pattern] = p
}

// wildcard is one `{name}` or `{name...}` of a pattern.
var wildcard = regexp.MustCompile(`\{[^}]+\}`)

// EVERY WRITE THAT READS `dry_run` DECLARES IT, AND EVERY ONE THAT DECLARES IT
// READS IT (ADR-0055).
//
// The declaration is what the Dashboard Assistant's preview and a read grant's
// guard trust, so it is held to the handler BEHAVIOURALLY: every write this
// surface mounts is sent `dry_run=maybe`, which a handler reading the flag
// refuses first, as `400 invalid_query` naming it, and any other handler does
// not. A write that declares a summary is a write that declares a check, since
// the summary is what only the write records. Mutation: drop Checked() from a
// route, or declare it on the reload, and this names the route.
func TestEveryWriteThatReadsDryRunDeclaresIt(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	s.seed(t, companyJSONDoc)
	mux := &policyRecorder{ServeMux: http.NewServeMux(), policies: map[string]authz.Policy{}}
	if err := s.service().Routes(mux); err != nil {
		t.Fatal(err)
	}
	caller := iam.Principal{
		ID: uuid.NewSHA1(auth.TokenNamespace, []byte("ops")), Login: iam.TokenLogin("ops"),
		Kind: iam.KindMachine, Stage: iam.StageActive, Grants: iam.AllGrants,
		ReauthAt: time.Now().Add(time.Hour),
	}
	declared := 0
	for pattern, policy := range mux.policies {
		method, path, _ := strings.Cut(pattern, " ")
		if method == http.MethodGet || method == http.MethodHead || method == http.MethodOptions {
			continue
		}
		if policy.Summary && !policy.DryRun {
			t.Errorf("%s declares a summary and no check", pattern)
		}
		target := wildcard.ReplaceAllString(path, "probe") + "?dry_run=maybe"
		req := httptest.NewRequest(method, target, strings.NewReader("{}"))
		req = req.WithContext(iam.WithPrincipal(req.Context(), caller))
		res := httptest.NewRecorder()
		mux.ServeHTTP(res, req)
		reads := res.Code == http.StatusBadRequest &&
			strings.Contains(res.Body.String(), "invalid_query") &&
			strings.Contains(res.Body.String(), "dry_run")
		if reads != policy.DryRun {
			t.Errorf("%s declares a check %v and its handler reads dry_run %v: %d %s",
				pattern, policy.DryRun, reads, res.Code, res.Body)
		}
		if policy.DryRun {
			declared++
		}
	}
	if declared < 3 {
		t.Errorf("only %d writes declare a check; the walk is not reading the surface", declared)
	}
}
