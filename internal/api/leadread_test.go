package api_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

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

// A REFUSAL ON AUTHORITY SAYS WHY, over REST as it does everywhere else: the
// rule's reason and the grants that would have admitted the caller, in the
// envelope beside the code. It said `{"error":"unauthorized"}` and nothing
// more, so a person refused a colleague's queue could not tell a missing
// relation from a missing grant — and the sentence that was meant to name the
// remedy existed only in a log line, naming a grant in words nothing held
// against the rule.
//
// AND OVER THE SOCKET, which is the dashboard's only data channel: the error
// frame carries the same two facts under the same keys. It carried the code
// alone — the app reduced the refusal to the question's name on its way to
// the socket — so the one transport the screens read was the one that could
// not say what would change the answer. A refused WATCH says it too.
func TestARefusedPersonalQuestionNamesItsReasonAndItsGrants(t *testing.T) {
	t.Parallel()
	const token = "a-colleague-holding-state-read-and-nothing-else"
	b := config.DefaultBootstrap()
	authorize(&b, config.APIToken{ID: "colleague", Token: token,
		Grants: []iam.Grant{iam.GrantStateRead}})
	a := newApp(t, api.Options{
		Bootstrap: &b,
		SeatBindings: auth.SeatBindings{
			Directory: inboxBindings{iam.TokenLogin("colleague"): "ana"},
			Chart:     inboxChart{iam.TokenLogin("colleague"): "ana"},
		},
		Sources: queries.Sources{Work: stubWorkReader{},
			Chart: leadOf{lead: "ana", report: "cy"}},
	})
	for _, c := range []struct {
		path   string
		reason string
		grants []any
	}{
		// Somebody else's queue: no relation, and the admin grant would
		// have admitted it.
		{"/work/inbox?handle=bo", string(authz.ReasonNotSelf),
			[]any{string(iam.GrantFleetOperate)}},
		// A question whose own grant the caller does not carry.
		{"/events", string(authz.ReasonNoGrant), []any{string(iam.GrantAuditRead)}},
	} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, c.path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		a.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("GET %s = %d, want 403\n%s", c.path, rec.Code, rec.Body.String())
			continue
		}
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatalf("GET %s: decode: %v", c.path, err)
		}
		if body["error"] != "unauthorized" || body["message"] == nil {
			t.Errorf("GET %s answered %v, want the unauthorized envelope", c.path, body)
		}
		if body[authz.DetailReason] != c.reason {
			t.Errorf("GET %s: reason = %v, want %q", c.path,
				body[authz.DetailReason], c.reason)
		}
		if !reflect.DeepEqual(body[authz.DetailGrants], c.grants) {
			t.Errorf("GET %s: grants = %v, want %v", c.path,
				body[authz.DetailGrants], c.grants)
		}
	}

	// THE SAME QUESTIONS OVER THE SOCKET.
	srv := httptest.NewServer(a)
	t.Cleanup(srv.Close)
	sock := dialInbox(t, srv.URL, token)
	for i, c := range []struct {
		frame  map[string]any
		reason string
		grants []any
	}{
		{map[string]any{"kind": "query", "id": 1, "what": "work_inbox",
			"params": map[string]any{"handle": "bo"}},
			string(authz.ReasonNotSelf), []any{string(iam.GrantFleetOperate)}},
		{map[string]any{"kind": "query", "id": 2, "what": "events"},
			string(authz.ReasonNoGrant), []any{string(iam.GrantAuditRead)}},
		// A WATCH of somebody else's inbox is decided by the same rule
		// and refused with the same two facts.
		{map[string]any{"kind": "watch", "seat": "bo"},
			string(authz.ReasonNotSelf), []any{string(iam.GrantFleetOperate)}},
	} {
		sock.send(t, c.frame)
		body := nextError(t, sock)
		if body["error"] != "unauthorized" {
			t.Errorf("socket frame %d answered %v, want unauthorized", i, body)
			continue
		}
		if body[authz.DetailReason] != c.reason {
			t.Errorf("socket frame %d: reason = %v, want %q", i,
				body[authz.DetailReason], c.reason)
		}
		if !reflect.DeepEqual(body[authz.DetailGrants], c.grants) {
			t.Errorf("socket frame %d: grants = %v, want %v", i,
				body[authz.DetailGrants], c.grants)
		}
	}
}

// nextError reads past every push to the next error frame, whole.
func nextError(t *testing.T, s *inboxSocket) map[string]any {
	t.Helper()
	for {
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		_, raw, err := s.conn.Read(ctx)
		cancel()
		if err != nil {
			t.Fatalf("read: %v", err)
		}
		var frame map[string]any
		if err := json.Unmarshal(raw, &frame); err != nil {
			t.Fatalf("decode %s: %v", raw, err)
		}
		if frame["kind"] == "error" {
			return frame
		}
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

// LeadsAnyone is [leadOf.Leads] asked of every subject.
func (c leadOf) LeadsAnyone(_ context.Context, actor string) (bool, error) {
	return actor == c.lead, nil
}
