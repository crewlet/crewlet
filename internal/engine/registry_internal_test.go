package engine

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/jira"
)

// AN EPOCH IS NEVER CURRENT BEFORE ITS PARTIES ARE INDEXED.
//
// The integration reconcile loop is armed before boot finishes and runs a pass
// at once, registering the seat identities a tracker or code host names people
// by into [Engine.Registry]. An epoch published with a nil registry panics that
// goroutine, which ends the process.
func TestAnEpochIsIndexedBeforeItIsPublished(t *testing.T) {
	t.Parallel()
	e, company := routingEngine(t, resolvingInstance(t))
	// routingEngine publishes the company; take that back, so the node starts
	// as boot does, with nothing current and nothing indexed.
	e.epoch.current.Store(nil)

	e.installEpoch(t.Context(), company)

	reg := e.Registry()
	if reg == nil {
		t.Fatal("an epoch was published with no party registry, so the first reader " +
			"that registers a seat identity dereferences nil")
	}
	if _, ok := reg.ByHandle("ceo"); !ok {
		t.Fatal("the registry published with the epoch does not index that epoch's seats")
	}

	// And the reader that panicked: a reconcile pass straight after boot.
	if found := e.resolveRouting(t.Context(), integration.KindJira, nil); len(found) != 0 {
		t.Fatalf("findings = %+v, want the seat resolved against the instance", found)
	}
	if _, ok := e.Registry().ByExternalID(jira.Backend, "agent-ceo"); !ok {
		t.Fatal("the pass resolved the seat but it never reached the live registry")
	}
}

// AN APPLY'S EARLIER INDEX IS KEPT, AND ONLY FOR THE EPOCH IT WAS BUILT FROM.
//
// An apply indexes the new company before rebuilding the vendor wiring, which
// registers seat identities into it, so publishing must not rebuild the
// registry over them. A registry built from any other company is not an index
// of the one published, however alike their documents are.
func TestPublishingKeepsTheIndexBuiltForThatEpoch(t *testing.T) {
	t.Parallel()
	e, company := routingEngine(t, "https://jira.example.com")
	e.epoch.current.Store(nil)

	e.refreshParties(t.Context(), company)
	built := e.Registry()
	if err := built.Register(jira.Backend, "agent-ceo", "ceo"); err != nil {
		t.Fatalf("register: %v", err)
	}
	e.installEpoch(t.Context(), company)
	if e.Registry() != built {
		t.Fatal("publishing rebuilt the registry the apply had already indexed, dropping " +
			"the seat identities its vendor wiring registered in between")
	}
	if _, ok := e.Registry().ByExternalID(jira.Backend, "agent-ceo"); !ok {
		t.Fatal("an identity registered before publishing is gone after it")
	}

	next, err := NewCompanyWith(company.Config, e.resolver())
	if err != nil {
		t.Fatalf("company: %v", err)
	}
	e.installEpoch(t.Context(), next)
	if e.Registry() == built || !e.indexes(next) {
		t.Fatal("a new epoch was published over the previous epoch's registry")
	}
}

// resolvingInstance is a tracker that names every credential's account.
func resolvingInstance(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/api/2/myself" {
			http.Error(w, "no such route", http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"name":"agent-ceo"}`)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}
