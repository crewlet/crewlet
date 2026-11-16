package catalog_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/assistant/cards"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/assistant/exec"
	"github.com/crewlet/crewlet/internal/provenance"
)

// Each trail sends its own filters (actions: source=operator), is fenced by its
// trust, and a zero position cursor ends the identity trail.
func TestTheAuditLogIsReadAsTheTrailItIs(t *testing.T) {
	t.Parallel()
	tool := mustTool(t, "list_audit_events")
	for _, tc := range []struct {
		args map[string]any
		path string
		want string
	}{
		{nil, "/events", "limit=25&source=operator"},
		{map[string]any{"by": "jane-founder", "failed": true, "limit": 5}, "/events",
			"actor=jane-founder&failed=true&limit=5&source=operator"},
		{map[string]any{"trail": "identity", "about": "ana", "event": "session_opened"},
			"/iam/audit", "event=session_opened&limit=25&person=ana"},
	} {
		req, err := tool.Request(tc.args)
		if err != nil {
			t.Fatal(err)
		}
		if req.Path != tc.path || req.Query.Encode() != tc.want {
			t.Errorf("list_audit_events %v asks %s?%s, want %s?%s", tc.args, req.Path,
				req.Query.Encode(), tc.path, tc.want)
		}
	}
	shape := func(args map[string]any, answer any) catalog.Result {
		body, _ := json.Marshal(answer)
		res, err := catalog.Shape(tool, args, exec.Response{Status: http.StatusOK, Body: body},
			provenance.NewBoundary(), evals.Epoch, cards.Options{})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	actions := shape(nil, map[string]any{"events": []any{map[string]any{"id": "e1",
		"type": "operator_acted", "timestamp": "2026-11-14T09:00:00Z", "actor": "jane-founder",
		"summary": "jane-founder ran update_work_item: applied"}},
		"next": map[string]any{"before_id": "e1"}})
	if actions.Trust != provenance.TrustExternal ||
		!strings.Contains(actions.Model, "ran update_work_item: applied") ||
		!actions.Cards[0].Table.More {
		t.Errorf("the actions trail is fenced %q and reads %s", actions.Trust, actions.Model)
	}
	row := map[string]any{"id": "h1", "op": "session_opened", "actor": "ana",
		"summary": "ana signed in", "at": "2026-11-14T09:00:00Z"}
	for next, more := range map[int]bool{0: false, 412: true} {
		identity := shape(map[string]any{"trail": "identity"},
			map[string]any{"events": []any{row}, "next": next, "position": "7"})
		if identity.Trust != provenance.TrustColleague || identity.Cards[0].Table.More != more {
			t.Errorf("the identity trail at next %d is fenced %q, more %v; want colleague, %v",
				next, identity.Trust, identity.Cards[0].Table.More, more)
		}
	}
}
