package configapi_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/coord"
)

// A node whose own store is empty is not a node whose FLEET is unconfigured.
//
// It reaches that state legitimately: it joined a fleet and has not reconciled
// yet, or the best-effort copy of the pointer into its own database failed. Its
// GET /config answers 404, which is what the dashboard's create flow opens on,
// so "create the company" on that node used to store a company built on nothing
// and activate it unconditionally over the one the fleet was running. The
// company name changes, so every seat id derived from it changes, and all of
// their memory is orphaned, with nothing refusing any of it.

// laggingNode is a surface with an empty store whose fleet is running a
// company, and the revision id the fleet is on.
func laggingNode(t *testing.T) (*counted, string) {
	t.Helper()
	s := newCountedSurface(t)
	published, err := s.plane.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: "the-fleets-company", Summary: "written on another node",
		Payload: []byte(`{"name":"Acme"}`), At: pinned,
	})
	if err != nil {
		t.Fatalf("the fleet's own activation: %v", err)
	}
	if _, found, err := s.configs.Active(t.Context()); err != nil || found {
		t.Fatalf("the node's store is not empty (found=%v err=%v)", found, err)
	}
	// The fleet's own activation went through the counted plane, and what a
	// case measures is what the REQUEST does.
	s.forget()
	return s, published.RevisionID
}

// A CREATE-ONLY WRITE IS REFUSED BY THE FLEET'S POINTER, and says which
// revision it lost to.
func TestACreateOnlyWriteIsRefusedByTheFleetsActivation(t *testing.T) {
	t.Parallel()
	for _, query := range []string{"", "?dry_run=true"} {
		s, fleetRevision := laggingNode(t)
		res := s.do(t, http.MethodPut, "/config"+query, companyJSONDoc,
			map[string]string{"X-Summary": "create the company", "If-None-Match": "*"})
		if res.Code != http.StatusPreconditionFailed {
			t.Fatalf("PUT %s = %d, want 412: %s", query, res.Code, res.Body)
		}
		body := decode(t, res)
		if body["error"] != "already_configured" || body["current_revision_id"] != fleetRevision {
			t.Errorf("PUT %s refusal = %v, want already_configured naming %s",
				query, body, fleetRevision)
		}
		if got := s.writes(); got != (writeCounts{}) {
			t.Errorf("PUT %s stored or activated something: %+v", query, got)
		}
		if target, _, err := s.plane.Target(t.Context()); err != nil || target.RevisionID != fleetRevision {
			t.Errorf("the fleet's activation moved to %+v (err %v)", target, err)
		}
	}
}

// AND SO IS A WRITE BUILT ON NOTHING THAT SENT NO PRECONDITION.
//
// Every write names what it was built on as the activation's expectation, and
// a write with no base was built on an empty store. Unconditional there means
// "replace whatever the fleet is running with a company nobody has seen", so
// it is a create-only compare-and-set like any other, and the loser is told.
func TestAWriteBuiltOnNothingIsRefusedWhenTheFleetHasOne(t *testing.T) {
	t.Parallel()
	s, fleetRevision := laggingNode(t)

	res := s.do(t, http.MethodPut, "/config", companyJSONDoc, summaryHeader)
	if res.Code != http.StatusConflict {
		t.Fatalf("PUT = %d, want 409: %s", res.Code, res.Body)
	}
	body := decode(t, res)
	if body["error"] != "revision_advanced" || body["current_revision_id"] != fleetRevision {
		t.Errorf("refusal = %v, want revision_advanced naming %s", body, fleetRevision)
	}
	// The revision is kept, inert, as every other lost race keeps it.
	s.assertKeptInert(t, body)
	if target, _, err := s.plane.Target(t.Context()); err != nil || target.RevisionID != fleetRevision {
		t.Errorf("the fleet's activation moved to %+v (err %v)", target, err)
	}
}

// assertKeptInert checks that a write refused at the activation left its
// revision in the history and did not make it this node's company.
//
// INERT IS THE WHOLE POINT, and it is a property of this node as much as of
// the fleet. A node's active revision is what it serves from GET /config and
// what it offers the fleet at its next start whenever it is newer than the
// pointer, so a loser left active here was served straight after its refusal
// and published one restart later, over the company that won.
func (s *surface) assertKeptInert(t *testing.T, refusal map[string]any) {
	t.Helper()
	stored, _ := refusal["stored_revision_id"].(string)
	if stored == "" {
		t.Fatalf("the refusal does not name the revision it stored: %v", refusal)
	}
	revision, found, err := s.configs.Get(t.Context(), stored)
	if err != nil || !found {
		t.Fatalf("the refused write's revision is not in the history (found=%v err=%v)", found, err)
	}
	if revision.Active {
		t.Errorf("the refused write's revision %s is this node's active revision", stored)
	}
	if active, found, err := s.configs.Active(t.Context()); err == nil && found && active.ID == stored {
		t.Errorf("this node serves the revision the fleet refused: %s", stored)
	}
}

// AND THE FIRST COMPANY STILL LANDS when the fleet has nothing, or the guard
// above is just a way of refusing every first import.
func TestTheFirstCompanyLandsOnAFleetWithNoActivation(t *testing.T) {
	t.Parallel()
	for name, headers := range map[string]map[string]string{
		"create-only":   {"X-Summary": "create", "If-None-Match": "*"},
		"unconditional": {"X-Summary": "create"},
	} {
		s := newCountedSurface(t)
		res := s.do(t, http.MethodPut, "/config", companyJSONDoc, headers)
		if res.Code != http.StatusCreated {
			t.Fatalf("%s PUT = %d, want 201: %s", name, res.Code, res.Body)
		}
		target, found, err := s.plane.Target(t.Context())
		if err != nil || !found || target.RevisionID == "" {
			t.Errorf("%s: the fleet was not pointed at the new revision (found=%v err=%v)",
				name, found, err)
		}
	}
}

// A FLEET POINTER THAT CANNOT BE READ IS NOT "THERE IS NO COMPANY".
//
// The three-valued answer again: "the fleet has nothing", "the fleet has one"
// and "I could not ask" are different facts, and guessing the first on an
// unreachable store is exactly the write this guard exists to refuse.
func TestACreateOnlyWriteRefusesAnUnreadableFleetPointer(t *testing.T) {
	t.Parallel()
	unreadable := errors.New("the coordination store is unreachable")
	s := newSurfaceWith(t, func(o *configapi.Options) {
		o.Plane = mutePlane{Plane: o.Plane, err: unreadable}
	})

	res := s.do(t, http.MethodPut, "/config", companyJSONDoc,
		map[string]string{"X-Summary": "create", "If-None-Match": "*"})
	if res.Code != http.StatusInternalServerError {
		t.Fatalf("PUT = %d, want 500: %s", res.Code, res.Body)
	}
	if _, found, err := s.configs.Active(t.Context()); err != nil || found {
		t.Errorf("a refused write stored a revision (found=%v err=%v)", found, err)
	}
}

// A COMPANY THAT APPEARS AFTER THE PRECONDITION IS STILL A 412.
//
// The precondition reads the fleet's pointer before the document is built,
// and the activation is the compare-and-set that catches whatever happened
// since. A create-only write that loses THERE is not a lost edit to re-derive
// and send again: a company exists, which is what the caller asked to be told.
func TestACreateOnlyWriteLosingAtTheActivationIsAlreadyConfigured(t *testing.T) {
	t.Parallel()
	racing := &racingPlane{winner: "the-company-that-appeared"}
	s := newSurfaceWith(t, func(o *configapi.Options) {
		racing.Plane = o.Plane
		o.Plane = racing
	})

	res := s.do(t, http.MethodPut, "/config", companyJSONDoc,
		map[string]string{"X-Summary": "create", "If-None-Match": "*"})
	if res.Code != http.StatusPreconditionFailed {
		t.Fatalf("PUT = %d, want 412: %s", res.Code, res.Body)
	}
	body := decode(t, res)
	if body["error"] != "already_configured" || body["current_revision_id"] != racing.winner {
		t.Errorf("refusal = %v, want already_configured naming %s", body, racing.winner)
	}
	// The revision is kept, inert, as every lost race keeps it: the node
	// still has no company of its own, so it answers as it did before.
	s.assertKeptInert(t, body)
	if got := s.do(t, http.MethodGet, "/config", "", nil); got.Code != http.StatusNotFound {
		t.Errorf("GET /config after the refusal = %d, want 404 as before: %s", got.Code, got.Body)
	}
}

// AND ONE WHOSE WINNER CANNOT BE READ NAMES NONE, rather than an empty id a
// client would build a revision URL out of.
func TestACreateOnlyWriteLosingToAnUnreadableWinnerNamesNone(t *testing.T) {
	t.Parallel()
	racing := &racingPlane{winner: "the-company-that-appeared", unreadableAfter: true}
	s := newSurfaceWith(t, func(o *configapi.Options) {
		racing.Plane = o.Plane
		o.Plane = racing
	})

	res := s.do(t, http.MethodPut, "/config", companyJSONDoc,
		map[string]string{"X-Summary": "create", "If-None-Match": "*"})
	if res.Code != http.StatusPreconditionFailed {
		t.Fatalf("PUT = %d, want 412: %s", res.Code, res.Body)
	}
	body := decode(t, res)
	if body["error"] != "already_configured" {
		t.Errorf("refusal = %v, want already_configured", body)
	}
	if current, present := body["current_revision_id"]; present {
		t.Errorf("current_revision_id = %q, want it left out when the winner could not be read", current)
	}
	s.assertKeptInert(t, body)
}

// racingPlane is a plane with no activation until it refuses one: the window
// between a precondition and the compare-and-set that closes it.
type racingPlane struct {
	coord.Plane
	winner string
	// unreadableAfter makes the pointer unreadable once the activation has
	// been refused, so the refusal cannot say what won.
	unreadableAfter bool
	lost            atomic.Bool
}

func (p *racingPlane) Activate(context.Context, coord.ActivationRequest) (coord.Activation, error) {
	p.lost.Store(true)
	return coord.Activation{}, fmt.Errorf("%w: a company was activated first",
		coord.ErrActivationRaced)
}

func (p *racingPlane) Target(context.Context) (coord.Activation, bool, error) {
	switch {
	case !p.lost.Load():
		return coord.Activation{}, false, nil
	case p.unreadableAfter:
		return coord.Activation{}, false, errors.New("the coordination store stopped answering")
	}
	return coord.Activation{RevisionID: p.winner}, true, nil
}

// mutePlane is a plane whose activation pointer cannot be read.
type mutePlane struct {
	coord.Plane
	err error
}

func (p mutePlane) Target(context.Context) (coord.Activation, bool, error) {
	return coord.Activation{}, false, p.err
}
