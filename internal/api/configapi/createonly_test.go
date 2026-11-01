package configapi_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/secrets"
)

// A node whose own store is empty is not a node whose FLEET is unconfigured.
//
// It reaches that state legitimately: it joined a fleet and has not reconciled
// yet. Its GET /config used to answer 404 from its empty store, which is what
// the dashboard's create flow opens on, so "create the company" on that node
// stored a company built on nothing and activated it unconditionally over the
// one the fleet was running. The company name changes, so every seat id
// derived from it changes, and all of their memory is orphaned, with nothing
// refusing any of it. Every answer is the fleet's pointer now, so the node
// answers the fleet's company, and a write on it is built on that.

// laggingNode is a surface with an empty store whose fleet is running a
// company, and the revision id the fleet is on.
func laggingNode(t *testing.T) (*counted, string) {
	t.Helper()
	s := newCountedSurface(t)
	payload, err := secrets.Seal(s.cipher, companyJSON(t, companyDoc))
	if err != nil {
		t.Fatal(err)
	}
	published, err := s.plane.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: "the-fleets-company", Summary: "written on another node",
		Payload: payload, At: pinned, ExpectAbsent: true,
		Origin: coord.RevisionOrigin{Author: "maya", AuthorKind: "operator", Source: "api", CreatedAt: pinned},
	})
	if err != nil {
		t.Fatalf("the fleet's own activation: %v", err)
	}
	if _, held, err := s.configs.Get(t.Context(), published.RevisionID); err != nil || held {
		t.Fatalf("the node already holds the fleet's revision (held=%v err=%v)", held, err)
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

// AND A WRITE ON IT IS BUILT ON THE FLEET'S REVISION, which it keeps first.
//
// The base of every write is the pointer, so a node that has not caught up
// edits the company the fleet runs rather than one built on nothing: the write
// names it as its parent, keeps a copy of it so the parent is a row of this
// node's history, and lands only while the pointer still names it.
//
// Mutation: build the write on what this node boots on and it is built on
// nothing, and refused.
func TestAWriteOnANodeThatHasNotCaughtUpIsBuiltOnTheFleetsRevision(t *testing.T) {
	t.Parallel()
	s, fleetRevision := laggingNode(t)

	if got := s.do(t, http.MethodGet, "/config", "", nil); got.Code != http.StatusOK ||
		got.Header().Get("ETag") != `"`+fleetRevision+`"` {
		t.Fatalf("GET /config = %d ETag %s, want the fleet's %s", got.Code,
			got.Header().Get("ETag"), fleetRevision)
	}
	res := s.do(t, http.MethodPut, "/config", companyJSONDoc, summaryHeader)
	if res.Code != http.StatusCreated {
		t.Fatalf("PUT = %d, want 201: %s", res.Code, res.Body)
	}
	written, _ := decode(t, res)["revision_id"].(string)
	revision, held, err := s.configs.Get(t.Context(), written)
	if err != nil || !held || revision.ParentID != fleetRevision {
		t.Fatalf("the write is %+v (held=%v err=%v), want it built on %s",
			revision, held, err, fleetRevision)
	}
	if _, held, err := s.configs.Get(t.Context(), fleetRevision); err != nil || !held {
		t.Errorf("the write's base is not kept in this node's history (held=%v err=%v)", held, err)
	}
	if target, _, err := s.plane.Target(t.Context()); err != nil || target.RevisionID != written {
		t.Errorf("the fleet's activation is %+v (err %v), want the write %s", target, err, written)
	}
}

// assertKeptInert checks that a write refused at the activation left its
// revision in the history and made it neither the company nor what this node
// boots on — and that the refusal names the node whose history alone holds it.
//
// INERT IS THE WHOLE POINT, and it is a property of this node as much as of
// the fleet: a loser this node booted on would be served after its next start,
// and offered to the fleet over the company that won.
//
// AND IT IS ONE NODE'S. Only an activation naming the revision would have
// carried it to the others, and this one lost: a client behind a shared address
// sent to diff or revert it reached a node that answers 404 and concluded the
// work was gone. So the body names the node, and the hint sends both requests
// there. Mutation: leave the node out of [configapi.KeptRevision] or the hint,
// and every caller of this goes red.
func (s *surface) assertKeptInert(t *testing.T, refusal map[string]any) {
	t.Helper()
	stored, _ := refusal["stored_revision_id"].(string)
	if stored == "" {
		t.Fatalf("the refusal does not name the revision it stored: %v", refusal)
	}
	if refusal["node"] != thisNode {
		t.Errorf("node = %v, want %s, whose history alone holds %s", refusal["node"],
			thisNode, stored)
	}
	if hint, _ := refusal["hint"].(string); !strings.Contains(hint, "node "+thisNode+"'s history alone") ||
		!strings.Contains(hint, "/config/revisions/"+stored+"/revert") {
		t.Errorf("the hint does not send the revert of %s to node %s alone: %q", stored,
			thisNode, hint)
	}
	revision, found, err := s.configs.Get(t.Context(), stored)
	if err != nil || !found {
		t.Fatalf("the refused write's revision is not in the history (found=%v err=%v)", found, err)
	}
	if revision.Boots {
		t.Errorf("the refused write's revision %s is what this node boots on", stored)
	}
	if active, found, err := s.active(t.Context()); err == nil && found && active.ID == stored {
		t.Errorf("the company is the revision the fleet refused: %s", stored)
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
	if history, err := s.configs.List(t.Context(), 10, 0); err != nil || len(history) != 0 {
		t.Errorf("a refused write stored a revision (history=%d err=%v)", len(history), err)
	}
}

// AND ONE THE COORDINATION STORE CANNOT ANSWER IS A 503 TO COME BACK FROM, on
// every read and write here: the fleet's pointer is what each of them is
// about, and a store between raft leaders answers again in seconds. A 500
// reads as a broken install; a 503 with a Retry-After is a client told when.
//
// Mutation: drop the ErrUnavailable case from Service.fail and these are 500s.
func TestAConfigRequestTheFleetCannotAnswerIs503(t *testing.T) {
	t.Parallel()
	s := newSurfaceWith(t, func(o *configapi.Options) {
		o.Plane = mutePlane{Plane: o.Plane, err: fmt.Errorf("%w: no leader", coord.ErrUnavailable)}
	})
	for _, req := range []struct{ method, path, body string }{
		{http.MethodGet, "/config", ""},
		{http.MethodGet, "/config/revisions", ""},
		{http.MethodPut, "/config", companyJSONDoc},
	} {
		res := s.do(t, req.method, req.path, req.body, summaryHeader)
		if res.Code != http.StatusServiceUnavailable || res.Header().Get("Retry-After") != "2" {
			t.Errorf("%s %s = %d Retry-After %q, want 503 after 2: %s", req.method, req.path,
				res.Code, res.Header().Get("Retry-After"), res.Body)
			continue
		}
		if code := decode(t, res)["error"]; code != "unavailable" {
			t.Errorf("%s %s answered %v, want unavailable", req.method, req.path, code)
		}
	}
}

// A WRITE STORED AND THEN UNANSWERED IS AN UNKNOWN OUTCOME, never "nothing was
// written".
//
// The revision is stored first, and the activation after it can go unanswered
// two ways: the publish never reached the coordination store, or it landed and
// its acknowledgement was lost. This node cannot tell the two apart, and an
// operator told that nothing was written sends the write again — a second
// revision and a second epoch over one that may already be the company. So the
// answer says the outcome is unknown, names the revision it stored and the node
// whose history alone holds it, and carries no operation id, which a config
// write has none of.
//
// Mutations: answer the commit's ErrUnavailable as Service.fail does, and both
// cases lose the outcome and the stored revision; leave the node out of the
// body or the hint, and both lose it.
func TestAWriteStoredAndThenUnansweredIsAnUnknownOutcome(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		landed bool
	}{
		{"the publish never reached the store", false},
		{"the publish landed and its answer was lost", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newSurfaceWith(t, func(o *configapi.Options) {
				o.Plane = unansweredPlane{Plane: o.Plane, landed: tc.landed}
			})
			res := s.do(t, http.MethodPut, "/config", companyJSONDoc, summaryHeader)
			if res.Code != http.StatusServiceUnavailable || res.Header().Get("Retry-After") != "2" {
				t.Fatalf("PUT = %d Retry-After %q, want 503 after 2: %s", res.Code,
					res.Header().Get("Retry-After"), res.Body)
			}
			body := decode(t, res)
			if body["error"] != "unavailable" || body["outcome"] != "unknown" {
				t.Errorf("refusal = %v, want unavailable with an unknown outcome", body)
			}
			if _, said := body["op_id"]; said {
				t.Errorf("refusal = %v carries an op_id, which a config write has none of", body)
			}
			stored, _ := body["stored_revision_id"].(string)
			if _, found, err := s.configs.Get(t.Context(), stored); err != nil || !found {
				t.Fatalf("the refusal names %q, which is not in the history (found=%v "+
					"err=%v): %v", stored, found, err, body)
			}
			if hint, _ := body["hint"].(string); !strings.Contains(hint, "GET /config") ||
				!strings.Contains(hint, "/config/revisions/"+stored+"/revert") {
				t.Errorf("the hint does not say how to tell whether %s landed: %q", stored, hint)
			}
			// AND WHICH NODE HOLDS IT: the revision is in this node's history
			// alone, so its diff and its revert answer here and 404 anywhere
			// else, and a client behind a shared address has to be told.
			if body["node"] != thisNode {
				t.Errorf("node = %v, want %s, the one node whose history holds %s",
					body["node"], thisNode, stored)
			}
			if hint, _ := body["hint"].(string); !strings.Contains(hint, "node "+thisNode+"'s history alone") ||
				!strings.Contains(hint, "/config/revisions/"+stored+"/diff") {
				t.Errorf("the hint does not send the diff and the revert to node %s: %q",
					thisNode, hint)
			}
			active, found, err := s.active(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if landed := found && active.ID == stored; landed != tc.landed {
				t.Errorf("the fleet runs the stored revision: %v, want %v", landed, tc.landed)
			}
		})
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
	racing.seal(t, s)

	res := s.do(t, http.MethodPut, "/config", companyJSONDoc,
		map[string]string{"X-Summary": "create", "If-None-Match": "*"})
	if res.Code != http.StatusPreconditionFailed {
		t.Fatalf("PUT = %d, want 412: %s", res.Code, res.Body)
	}
	body := decode(t, res)
	if body["error"] != "already_configured" || body["current_revision_id"] != racing.winner {
		t.Errorf("refusal = %v, want already_configured naming %s", body, racing.winner)
	}
	// The revision is kept, inert, as every lost race keeps it, and the
	// company is the one that appeared.
	s.assertKeptInert(t, body)
	if got := s.do(t, http.MethodGet, "/config", "", nil); got.Code != http.StatusOK ||
		got.Header().Get("ETag") != `"`+racing.winner+`"` {
		t.Errorf("GET /config after the refusal = %d ETag %s, want the company that "+
			"appeared, %s: %s", got.Code, got.Header().Get("ETag"), racing.winner, got.Body)
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
	racing.seal(t, s)

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

// racingPlane is a plane with no activation until a write asks for one, when
// another company is activated first: the window between a precondition and
// the compare-and-set that closes it.
type racingPlane struct {
	coord.Plane
	winner string
	// body is the winner's sealed document, which the case sets once its
	// surface holds the keyring.
	body []byte
	// unreadableAfter makes the pointer unreadable once the activation has
	// been refused, so the refusal cannot say what won.
	unreadableAfter bool
	lost            atomic.Bool
}

func (p *racingPlane) Activate(ctx context.Context, req coord.ActivationRequest) (coord.Activation, error) {
	if p.lost.CompareAndSwap(false, true) {
		if _, err := p.Plane.Activate(ctx, coord.ActivationRequest{
			RevisionID: p.winner, Summary: "appeared", Payload: p.body, At: pinned,
			ExpectAbsent: true,
			Origin: coord.RevisionOrigin{Author: "maya", AuthorKind: "operator",
				Source: "api", CreatedAt: pinned},
		}); err != nil {
			return coord.Activation{}, err
		}
	}
	return p.Plane.Activate(ctx, req)
}

func (p *racingPlane) Target(ctx context.Context) (coord.Activation, bool, error) {
	if p.lost.Load() && p.unreadableAfter {
		return coord.Activation{}, false, errors.New("the coordination store stopped answering")
	}
	return p.Plane.Target(ctx)
}

// seal is the winner's body, sealed under the case's own keyring.
func (p *racingPlane) seal(t *testing.T, s *surface) {
	t.Helper()
	body, err := secrets.Seal(s.cipher, companyJSON(t, companyDoc))
	if err != nil {
		t.Fatal(err)
	}
	p.body = body
}

// mutePlane is a plane whose activation pointer cannot be read.
type mutePlane struct {
	coord.Plane
	err error
}

// unansweredPlane is a coordination store whose activations go unanswered:
// each publish fails as one whose acknowledgement never came, having landed
// first where landed says so.
type unansweredPlane struct {
	coord.Plane
	landed bool
}

func (p unansweredPlane) Activate(ctx context.Context, req coord.ActivationRequest) (coord.Activation, error) {
	if p.landed {
		if _, err := p.Plane.Activate(ctx, req); err != nil {
			return coord.Activation{}, err
		}
	}
	return coord.Activation{}, fmt.Errorf("%w: publish the activation: no answer",
		coord.ErrUnavailable)
}

func (p mutePlane) Target(context.Context) (coord.Activation, bool, error) {
	return coord.Activation{}, false, p.err
}
