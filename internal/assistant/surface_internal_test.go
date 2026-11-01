package assistant

import (
	"net/http"
	"testing"

	"github.com/crewlet/crewlet/internal/mcp"
)

// A ROUTE'S REFUSAL CARRIES A CLASS: the route's own code where the API spells
// it as one, else the class its status means, and never the empty class, which
// a caller with no model behind the surface would read as a failure nobody
// named.
//
// Mutation: drop the code lookup in routeClass and a 409 stale_version reads
// as conflict; drop the status switch and a 404 not_found_page reads as an
// internal error.
func TestARouteRefusalCarriesItsClass(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		status int
		code   string
		want   mcp.Refusal
	}{
		{http.StatusConflict, "stale_version", mcp.RefusalStaleVersion},
		{http.StatusForbidden, "forbidden", mcp.RefusalForbidden},
		{http.StatusNotFound, "run_not_found", mcp.RefusalNotFound},
		{http.StatusForbidden, "csrf_origin", mcp.RefusalForbidden},
		{http.StatusConflict, "surface_busy", mcp.RefusalConflict},
		{http.StatusServiceUnavailable, "draining", mcp.RefusalUnavailable},
		{http.StatusUnprocessableEntity, "invalid_input", mcp.RefusalInvalid},
		{http.StatusBadRequest, "http_400", mcp.RefusalInvalid},
		{http.StatusInternalServerError, "http_500", mcp.RefusalInternalError},
		{http.StatusOK, "http_200", mcp.RefusalInternalError},
	} {
		got := routeClass(tc.status, tc.code)
		if got != tc.want || !got.Valid() {
			t.Errorf("routeClass(%d, %q) = %q, want %q", tc.status, tc.code, got, tc.want)
		}
	}
}
