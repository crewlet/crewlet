package workapi_test

import (
	"net/http"
	"strconv"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/api/workapi"
)

// A ROUTE IS SERVED BY THE HALVES ITS REQUEST FINDS, NOT BY THE ONES THE
// SURFACE WAS BUILT WITH: the halves come up with a node's FIRST COMPANY,
// after its API is serving. One surface answers three things over its life:
//
//   - before any company: `503 no_active_revision`, reconcile poll as
//     Retry-After (a waiting client is served);
//   - a half this company does not run: the route's ABSENCE, the mux's own
//     bytes (no wait brings it);
//   - the halves up: the route, served.
//
// Mutations: read the halves once in New, or swap the 503 and the absence, and
// a phase answers another's.
func TestARouteIsServedByTheHalvesItsRequestFinds(t *testing.T) {
	t.Parallel()
	r := newRig(t, chart{})
	// THE SOURCE MOVES UNDER A SURFACE BUILT ONCE, as a node's does: nothing
	// here rebuilds or remounts it between the phases.
	const (
		noCompany = iota
		pagesOnly
		both
	)
	var phase atomic.Int32
	full := r.halves()
	halves := func() (workapi.Halves, bool) {
		switch phase.Load() {
		case noCompany:
			return workapi.Halves{}, false
		case pagesOnly:
			return workapi.Halves{Pages: full.Pages, PageStore: full.PageStore}, true
		}
		return full, true
	}
	svc, err := workapi.New(workapi.Options{
		Chart: chart{}, Halves: halves,
		Operator: r.operator(chart{}, halves), Audit: r.audit,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	r.mux = http.NewServeMux()
	if err := svc.Routes(r.mux); err != nil {
		t.Fatalf("Routes: %v", err)
	}
	create := map[string]any{"title": "rotate the key", "project": "ENG"}

	got := r.do(as(admin("ana")), http.MethodPost, "/worklet/items", create)
	if got.status != http.StatusServiceUnavailable ||
		got.body["error"] != string(httpjson.CodeNoActiveRevision) {
		t.Fatalf("before any company the route answered %d %v, want 503 %s",
			got.status, got.body, httpjson.CodeNoActiveRevision)
	}
	if want := strconv.Itoa(httpjson.RetrySeconds(httpjson.NoActiveRevisionRetry)); got.header.Get("Retry-After") != want {
		t.Errorf("the 503 says Retry-After %q, want %s — the reconcile poll a "+
			"node that has not met its company takes the revision on",
			got.header.Get("Retry-After"), want)
	}
	if len(r.writes.created) != 0 {
		t.Fatalf("a node with no company wrote: %v", r.writes.created)
	}

	phase.Store(pagesOnly)
	got = r.do(as(admin("ana")), http.MethodPost, "/worklet/items", create)
	if got.status != http.StatusNotFound || got.body["error"] != string(httpjson.CodeNoRoute) {
		t.Fatalf("a tracker route on a company whose tracker is a vendor's "+
			"answered %d %v, want the route's absence: 404 %s", got.status, got.body,
			httpjson.CodeNoRoute)
	}

	phase.Store(both)
	got = r.do(as(admin("ana")), http.MethodPost, "/worklet/items", create)
	if got.status != http.StatusOK {
		t.Fatalf("with the halves up the route answered %d %v, want it served",
			got.status, got.body)
	}
	if len(r.writes.created) != 1 {
		t.Errorf("the served request wrote %v, want the one create", r.writes.created)
	}
}
