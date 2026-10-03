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
// SURFACE WAS BUILT WITH.
//
// # Why this is the case that matters
//
// A node's tracker and knowledge base come up with its FIRST COMPANY, which a
// node that booted with none meets at an apply — after its API is serving.
// The surface used to take its halves once, at construction, so such a node
// mounted none of these routes and served no /work and no /pages until it was
// restarted. Read per request, one surface built before the company answers
// three different things over its life:
//
//   - before any company: `503 no_active_revision` carrying the reconcile poll
//     as its Retry-After, because the halves are coming and a client that
//     waits is served;
//   - a half this company does not run: the route's ABSENCE, in the very bytes
//     the mux answers a route nothing registered with, because no wait brings
//     it;
//   - the halves up: the route, served.
//
// Mutations, each run: read the halves once in New and the third phase is the
// first's 503; answer an absent half with the 503 and the second phase reads
// as a node still waiting; answer the not-up case with the route's absence and
// a client waiting for its node's first company is told the route does not
// exist.
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

	got := r.do(as(admin("ana")), http.MethodPost, "/work/items", create)
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
	got = r.do(as(admin("ana")), http.MethodPost, "/work/items", create)
	if got.status != http.StatusNotFound || got.body["error"] != string(httpjson.CodeNoRoute) {
		t.Fatalf("a tracker route on a company whose tracker is a vendor's "+
			"answered %d %v, want the route's absence: 404 %s", got.status, got.body,
			httpjson.CodeNoRoute)
	}

	phase.Store(both)
	got = r.do(as(admin("ana")), http.MethodPost, "/work/items", create)
	if got.status != http.StatusOK {
		t.Fatalf("with the halves up the route answered %d %v, want it served",
			got.status, got.body)
	}
	if len(r.writes.created) != 1 {
		t.Errorf("the served request wrote %v, want the one create", r.writes.created)
	}
}
