package api_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
)

// A LEAD READS A REPORT'S RECORD THROUGH THE APP, because the app is handed the
// chart the decision asks.
//
// The personal questions — somebody's inbox, their day, their person record —
// are the caller's own, their lead's, or the admin grant's. Every rule piece of
// that was tested where it lives; what went untested was the WIRING, and the
// wiring was missing: `crewlet run` built the query surface with no chart at
// all, so every lead's read of a report's record was UNKNOWN on every node for
// the life of the process, answered 503 and retried for ever. Composed through
// api.App, the way a caller reaches it, a chart that answers is a 200 and a
// surface holding none is the 503 — and the second row is what `api.New` now
// refuses to be built as.
func TestALeadReadsAReportsInboxThroughTheApp(t *testing.T) {
	t.Parallel()
	const token = "a-lead-holding-state-read-and-nothing-else"
	b := config.DefaultBootstrap()
	// STATE READ ALONE, so the lead relation is the only way in: a token
	// carrying fleet:operate would be admitted by the admin path before
	// the chart was asked, and the row would assert nothing about it.
	authorize(&b, config.APIToken{ID: "lead", Token: token,
		Grants: []iam.Grant{iam.GrantStateRead}})
	// The lead's token is bound to seat ana, the way `crewlet iam bind`
	// binds it — the fakes are the inbox test's.
	seats := map[string]string{iam.TokenLogin("lead"): "ana"}
	bound := auth.SeatBindings{Directory: inboxBindings(seats), Chart: inboxChart(seats)}
	for _, c := range []struct {
		name  string
		chart authz.Chart
		want  int
	}{
		{"a chart that answers", leadOf{lead: "ana", report: "bo"}, http.StatusOK},
		{"a chart that answers no", leadOf{lead: "ana", report: "cy"}, http.StatusForbidden},
		{"a surface holding no chart", authz.NoChart{}, http.StatusServiceUnavailable},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			a := newApp(t, api.Options{
				Bootstrap:    &b,
				SeatBindings: bound,
				Sources:      queries.Sources{Work: stubWorkReader{}, Chart: c.chart},
			})
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodGet, "/work/inbox?handle=bo", nil)
			req.Header.Set("Authorization", "Bearer "+token)
			a.ServeHTTP(rec, req)
			if rec.Code != c.want {
				t.Errorf("GET /work/inbox?handle=bo as bo's lead = %d, want %d\n%s",
					rec.Code, c.want, rec.Body.String())
			}
		})
	}
}

// leadOf is a chart holding exactly one management relation.
type leadOf struct{ lead, report string }

func (c leadOf) Leads(_ context.Context, actor, subject string) (bool, error) {
	return actor == c.lead && subject == c.report, nil
}

func (leadOf) LeadsProject(context.Context, string, string) (bool, error) {
	return false, nil
}

func (leadOf) LeadsUnit(context.Context, string, string) (bool, error) {
	return false, nil
}

func (leadOf) LeadsContainer(context.Context, string, string) (bool, error) {
	return false, nil
}
