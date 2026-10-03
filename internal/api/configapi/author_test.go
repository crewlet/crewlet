package configapi_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/api/auth"
	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/store"
)

// AN OPERATOR'S WRITE READS AS AN OPERATOR'S — IN THE HISTORY AND ON THE
// POINTER EVERY PEER ADOPTS FROM.
//
// The history answer used to carry the label alone, and the audit screen
// filled in the kind by assuming. The kind is recorded at the write now, where
// the surface KNOWS it is a person's credential, and it rides the pointer so a
// peer's copy of the revision says the same.
func TestAnOperatorsWriteIsRecordedAsTheOperators(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	s.seed(t, companyDoc, nil)

	req := httptest.NewRequest(http.MethodPatch, "/config", strings.NewReader(`{"mission":"ship it"}`))
	req.Header.Set("X-Summary", "a mission")
	req = req.WithContext(auth.WithOperator(req.Context(), "maya"))
	res := httptest.NewRecorder()
	s.mux.ServeHTTP(res, req)
	if res.Code != http.StatusCreated {
		t.Fatalf("PATCH = %d: %s", res.Code, res.Body.String())
	}

	listing := s.do(t, http.MethodGet, "/config/revisions", "", nil)
	var revisions []map[string]any
	if err := json.Unmarshal(listing.Body.Bytes(), &revisions); err != nil {
		t.Fatal(err)
	}
	if len(revisions) == 0 {
		t.Fatal("no revisions listed")
	}
	newest := revisions[0]
	if newest["created_by"] != "maya" || newest["created_by_kind"] != "operator" {
		t.Errorf("the history names (%v, %v), want (maya, operator)",
			newest["created_by"], newest["created_by_kind"])
	}

	target, found, err := s.plane.Target(t.Context())
	if err != nil || !found {
		t.Fatalf("target: found=%v err=%v", found, err)
	}
	if target.Origin.Author != "maya" || target.Origin.AuthorKind != "operator" ||
		target.Origin.Source != "api" {
		t.Errorf("the pointer carries origin %+v, want maya, operator, api — "+
			"every peer adopts the revision from it", target.Origin)
	}
}

// THE ENGINE'S OWN WRITE READS AS THE NODE'S. The reconcile loop writes
// through this same surface, and recorded under the HTTP defaults its reload
// after sealing a credential read as an operator's on the audit screen.
func TestTheEnginesWriteIsRecordedAsTheNodes(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	s.seed(t, companyDoc, nil)

	applied, err := s.service().Reload(t.Context(), "reload after sealing",
		store.Author{Name: "reconcile loop", Kind: store.AuthorNode})
	if err != nil {
		t.Fatalf("Reload: %v", err)
	}
	got, found, err := s.configs.Get(t.Context(), applied.RevisionID)
	if err != nil || !found {
		t.Fatalf("get: found=%v err=%v", found, err)
	}
	if got.CreatedBy != "reconcile loop" || got.CreatedByKind != store.AuthorNode {
		t.Errorf("stored as (%q, %q), want (reconcile loop, node)", got.CreatedBy, got.CreatedByKind)
	}
	target, _, err := s.plane.Target(t.Context())
	if err != nil || target.Origin.AuthorKind != "node" {
		t.Errorf("the pointer's origin kind = %q (err %v), want node", target.Origin.AuthorKind, err)
	}
}

// A WRITE THAT DOES NOT SAY WHAT WROTE IT IS NOT STORED. Every caller of the
// service states an author; one that forgot would otherwise put a row in the
// history that a reader has to guess about.
func TestAWriteWithNoAuthorKindIsRefused(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	s.seed(t, companyDoc, nil)
	if _, err := s.service().Apply(t.Context(), configapi.ApplyRequest{
		Patch: []byte(`{"mission":"x"}`), Summary: "s", Author: store.Author{Name: "who"},
	}); err == nil {
		t.Fatal("a write with no author kind was stored and activated")
	}
}
