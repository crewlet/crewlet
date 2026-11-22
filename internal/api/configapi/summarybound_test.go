package configapi_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/coord"
)

// A SUMMARY IS HELD TO WHAT THE ACTIVATION POINTER CARRIES, MEASURED AS THE
// POINTER ENCODES IT, AND REFUSED WHOLE, NEVER CUT.
//
// Every probe on every node reads the pointer, so it is small, and the summary
// is most of it. A summary at the bound is stored; one whose raw bytes fit but
// whose encoding does not (a backslash is two bytes in the pointer) is refused
// `400 summary_too_long`, naming both sizes, a dry run alike, before anything
// is stored. The summary is no longer counted against the document's bound:
// the body rides its own key.
//
// Mutation: measure the summary raw, and the backslashes are stored; drop the
// bound, and they are stored too.
func TestASummaryPastWhatThePointerCarriesIsRefused(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	if res := s.do(t, http.MethodPut, "/config", companyDoc,
		map[string]string{"X-Summary": strings.Repeat("s", coord.MaxSummaryBytes)}); res.Code != http.StatusCreated {
		t.Fatalf("a write whose summary is at the bound = %d %s, want 201", res.Code, res.Body)
	}
	before := s.revisionCount(t)
	escaped := strings.Repeat(`\`, coord.MaxSummaryBytes/2+1)
	for _, path := range []string{"/config?dry_run=true", "/config"} {
		res := s.do(t, http.MethodPut, path, strings.Replace(companyDoc, "Acme", "Other", 1),
			map[string]string{"X-Summary": escaped})
		if res.Code != http.StatusBadRequest {
			t.Fatalf("PUT %s with a summary of %d backslashes = %d %s, want 400", path,
				len(escaped), res.Code, res.Body)
		}
		body := decode(t, res)
		if body["error"] != string(httpjson.CodeSummaryTooLong) ||
			body["summary_bytes"] != float64(2*len(escaped)) ||
			body["max_summary_bytes"] != float64(coord.MaxSummaryBytes) {
			t.Errorf("PUT %s: the refusal does not say how long the summary is as the pointer "+
				"encodes it, and the bound: %s", path, res.Body)
		}
	}
	if after := s.revisionCount(t); after != before {
		t.Errorf("a refused summary stored %d revisions, want none", after-before)
	}
}
