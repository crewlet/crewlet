package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/assistant/catalog"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/iam"
)

// AN ASSISTED PRINCIPAL READS ONLY THE OVERVIEW ROUTES THE CATALOGUE READS.
//
// The four mirrors are decided by the push table rather than the authority
// table, so the assistant column never sees them, and they are not registry
// questions, so the registry's gate does not reach them either. Each mirror
// therefore asks the catalogue itself: through an assistant grant the roster
// and the org chart are served, the tool registry and the whole snapshot are
// refused `assist_refused` before anything is built, and an app wired with no
// gate serves the assistant none of them. The same person at their keyboard
// reads all four. Mutation: drop the check from serveFrom and the snapshot is
// served to the assistant.
func TestAnAssistedPrincipalReadsOnlyTheOverviewTheCatalogueReads(t *testing.T) {
	t.Parallel()
	person := iam.Principal{ID: uuid.New(), Login: "jo.park", Kind: iam.KindPerson,
		Seat: "analyst", Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantAdmin}}
	assisted := person
	assisted.Via, assisted.AssistScope = iam.AssistName(uuid.New()), iam.AssistScopeRead
	for _, gate := range []func(string) bool{catalog.ReadsRoute, nil} {
		a := &App{stream: mirrorService(t), assistRoute: gate}
		for _, route := range a.overviewRoutes() {
			for _, p := range []iam.Principal{person, assisted} {
				built := false
				handler := a.serveFrom(route.pattern, route.kind, func(stream.Audience) any {
					built = true
					return stream.AgentRows{}
				})
				req := httptest.NewRequest(http.MethodGet, "/", nil)
				req = req.WithContext(iam.WithPrincipal(req.Context(), p))
				rec := httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
				_, isAssisted := p.Assisted()
				want := !isAssisted || (gate != nil && catalog.ReadsRoute(route.pattern))
				if served := rec.Code == http.StatusOK; served != want || built != want {
					t.Errorf("%s (assisted %v, gated %v) answered %d, built %v; want served %v",
						route.pattern, isAssisted, gate != nil, rec.Code, built, want)
					continue
				}
				if !want {
					var body map[string]any
					_ = json.Unmarshal(rec.Body.Bytes(), &body)
					if body[authz.DetailReason] != string(authz.ReasonAssistRefused) {
						t.Errorf("%s refused the assistant naming %v", route.pattern,
							body[authz.DetailReason])
					}
				}
			}
		}
	}
}
