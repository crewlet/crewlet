package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/iam"
)

// AN ASSISTED PRINCIPAL READS THE OVERVIEW AS ITS PERSON DOES.
//
// The four mirrors are decided by the push table rather than the authority
// table, and none carries a credential, so ADR-0052's parity has nothing to
// take away: through an assistant read grant each is built and served exactly
// as the same person's own request is, for the same audience. Mutation: put an
// assistant gate back in serveFrom and the assisted principal is refused.
func TestAnAssistedPrincipalReadsTheOverviewAsItsPerson(t *testing.T) {
	t.Parallel()
	person := iam.Principal{ID: uuid.New(), Login: "jo.park", Kind: iam.KindPerson,
		Seat: "analyst", Stage: iam.StageActive, Grants: []iam.Grant{iam.GrantAdmin}}
	assisted := person
	assisted.Via, assisted.AssistScope = iam.AssistName(uuid.New()), iam.AssistScopeRead
	a := &App{stream: mirrorService(t)}
	for _, route := range a.overviewRoutes() {
		codes := map[bool]int{}
		for _, p := range []iam.Principal{person, assisted} {
			built := false
			handler := a.serveFrom(route.kind, func(stream.Audience) any {
				built = true
				return stream.AgentRows{}
			})
			req := httptest.NewRequest(http.MethodGet, "/", nil)
			req = req.WithContext(iam.WithPrincipal(req.Context(), p))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			_, isAssisted := p.Assisted()
			codes[isAssisted] = rec.Code
			if rec.Code != http.StatusOK || !built {
				t.Errorf("%s (assisted %v) answered %d, built %v; want served", route.pattern,
					isAssisted, rec.Code, built)
			}
		}
		if codes[true] != codes[false] {
			t.Errorf("%s answered the assistant %d and its person %d", route.pattern,
				codes[true], codes[false])
		}
	}
}
