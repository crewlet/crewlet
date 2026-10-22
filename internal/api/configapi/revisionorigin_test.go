package configapi_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
)

// A FLEET REVISION THIS NODE CANNOT OPEN SAYS WHERE IT CAME FROM AND WHAT FIXES
// IT, on every route that reads it.
//
// A node answers the fleet's revision from the body the pointer carries while
// it holds no row for it, and a body it cannot open was told in words written
// for its own copy: a key "no longer in the keyring" (the deployment holds it,
// this node was never given it), and the keyring package's own error as the
// detail. Mutation: tell every unreadable revision as this node's own, and the
// code and the hint here go wrong.
func TestAFleetRevisionThisNodeCannotOpenSaysWhereItCameFrom(t *testing.T) {
	t.Parallel()
	plane := coordmemory.NewFleet()
	author := newSurfaceWith(t, func(o *configapi.Options) { o.Plane = plane })
	author.seed(t, companyDoc)
	// ITS OWN KEYRING, sharing the fleet: a node never given the key the
	// fleet seals under, and holding no row for the fleet's revision.
	stranger := newSurfaceWith(t, func(o *configapi.Options) { o.Plane = plane })

	for _, req := range []struct{ method, path, body string }{
		{http.MethodGet, "/config", ""},
		{http.MethodGet, "/config/references", ""},
		{http.MethodGet, "/config/charter", ""},
		{http.MethodPatch, "/config", `{"mission": "x"}`},
	} {
		res := stranger.do(t, req.method, req.path, req.body, summaryHeader)
		body := decode(t, res)
		if res.Code != http.StatusConflict || body["error"] != "unreadable_fleet_revision" {
			t.Errorf("%s %s = %d %v, want 409 unreadable_fleet_revision",
				req.method, req.path, res.Code, body)
			continue
		}
		detail, _ := body["detail"].(string)
		hint, _ := body["hint"].(string)
		if strings.Contains(detail, "secrets:") || strings.Contains(detail, "decrypt") {
			t.Errorf("%s %s: the detail is the keyring's own error: %q", req.method, req.path, detail)
		}
		if !strings.Contains(hint, "secrets.keys") || strings.Contains(hint, "no longer") {
			t.Errorf("%s %s: the hint does not say which setting the key goes in, or says "+
				"the deployment lost it: %q", req.method, req.path, hint)
		}
	}
	// THE SOCKET'S QUESTION reads the same error, marked as the fleet's.
	_, err := stranger.service().Document(t.Context())
	var unreadable *configapi.UnreadableRevisionError
	if !errors.As(err, &unreadable) || !unreadable.Fleet {
		t.Fatalf("Document on a fleet revision this node cannot open = %v, want a "+
			"fleet UnreadableRevisionError", err)
	}
	// AND THIS NODE'S OWN COPY under a key its keyring lost keeps its own
	// code: held here, the revision is this node's history to restore.
	own := newSurface(t)
	own.seedSealedUnder(t, companyDoc, testCipher(t))
	if body := decode(t, own.do(t, http.MethodGet, "/config", "", nil)); body["error"] != "unreadable_revision" {
		t.Errorf("this node's own copy answered %v, want unreadable_revision", body)
	}
}

// A FLEET REVISION PUT ON THE POINTER WITHOUT A SEAL is told as one that came
// from outside the fleet, never as a copy an older build stored.
func TestAnUnsealedFleetRevisionIsToldAsTheFleets(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	if _, err := s.plane.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: "plaintext-on-the-pointer", Summary: "unsealed",
		Payload: companyJSON(t, companyDoc), At: pinned,
		Origin: coord.RevisionOrigin{Author: "somebody", AuthorKind: iam.ActorOperator,
			Source: "broker", CreatedAt: pinned},
	}); err != nil {
		t.Fatalf("activate: %v", err)
	}
	res := s.do(t, http.MethodGet, "/config", "", nil)
	if body := decode(t, res); res.Code != http.StatusConflict ||
		body["error"] != "unsealed_fleet_revision" {
		t.Errorf("GET /config = %d %v, want 409 unsealed_fleet_revision", res.Code, body)
	}
	// THIS NODE'S OWN unsealed copy keeps its own code.
	own := newSurface(t)
	own.seedUnsealed(t, companyDoc)
	if body := decode(t, own.do(t, http.MethodGet, "/config", "", nil)); body["error"] != "unsealed_revision" {
		t.Errorf("this node's own unsealed copy answered %v, want unsealed_revision", body)
	}
}

// A READ OF NO COMPANY SAYS HOW TO START ONE. The code's own message ends "The
// detail says how.", and the reads answered it with no detail at all.
// Mutation: answer the bare code again, and every row fails.
func TestAReadOfNoCompanySaysHowToStartOne(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	for _, path := range []string{"/config", "/config/references", "/config/charter",
		"/config/mcp-servers/notion"} {
		res := s.do(t, http.MethodGet, path, "", nil)
		body := decode(t, res)
		detail, _ := body["detail"].(string)
		if res.Code != http.StatusNotFound || body["error"] != "no_active_revision" ||
			!strings.Contains(detail, "PUT /config") || !strings.Contains(detail, "crewlet config import") {
			t.Errorf("GET %s = %d %v, want 404 no_active_revision with a detail naming "+
				"PUT /config and `crewlet config import`", path, res.Code, body)
		}
	}
}

// movingPlane answers its first read of the pointer with a revision whose body
// is gone by the time it is asked for: another activation landed between the
// two reads.
type movingPlane struct {
	coord.Plane
	mu    sync.Mutex
	stale *coord.Activation
	// stuck keeps answering the stale revision, as a store whose pointer
	// names a body it does not carry would.
	stuck bool
}

func (p *movingPlane) Target(ctx context.Context) (coord.Activation, bool, error) {
	p.mu.Lock()
	stale := p.stale
	if !p.stuck {
		p.stale = nil
	}
	p.mu.Unlock()
	if stale != nil {
		return *stale, true, nil
	}
	return p.Plane.Target(ctx)
}

// A POINTER THAT MOVES BETWEEN ITS TWO READS IS READ AGAIN, AND ONE THAT NAMES A
// BODY IT DOES NOT CARRY IS REPORTED.
//
// The fleet's revision is read in two steps on a node that has not kept it yet:
// the pointer, then the body it carries; and the pointer moves on between them
// whenever an activation lands there, which is a later revision to answer. A
// pointer that keeps naming a body it does not carry is not moving, and asking
// it again for ever would hang the request. Mutation: drop the bound, and the
// stuck row never answers; treat the first moved read as the fault, and the
// moving row answers 500.
func TestAPointerThatMovesBetweenItsTwoReadsIsReadAgain(t *testing.T) {
	t.Parallel()
	for _, stuck := range []bool{false, true} {
		moving := &movingPlane{stuck: stuck}
		s := newSurfaceWith(t, func(o *configapi.Options) {
			moving.Plane = o.Plane
			o.Plane = moving
		})
		current := s.seed(t, companyDoc)
		target, _, err := moving.Plane.Target(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		stale := func() *coord.Activation {
			return &coord.Activation{
				Epoch: target.Epoch - 1, RevisionID: "replaced-before-this-node-met-it",
				At: target.At.Add(-time.Minute), Origin: target.Origin,
			}
		}
		moving.mu.Lock()
		moving.stale = stale()
		moving.mu.Unlock()

		answered := make(chan *httptest.ResponseRecorder, 1)
		go func() { answered <- s.do(t, http.MethodGet, "/config", "", nil) }()
		var res *httptest.ResponseRecorder
		select {
		case res = <-answered:
		case <-time.After(30 * time.Second):
			t.Fatalf("GET /config on a pointer that stuck=%v has not answered in 30s", stuck)
		}
		switch {
		case !stuck && (res.Code != http.StatusOK || res.Header().Get("ETag") != `"`+current+`"`):
			t.Errorf("GET /config across a moved pointer = %d naming %s, want 200 naming %s: %s",
				res.Code, res.Header().Get("ETag"), current, res.Body)
		case stuck && res.Code != http.StatusInternalServerError:
			t.Errorf("GET /config on a pointer naming no body = %d, want 500: %s", res.Code, res.Body)
		}
		// A DIFF AGAINST IT IS THE SAME FAULT, never a revision the caller
		// asked about that is not there.
		if stuck {
			diff := s.do(t, http.MethodGet, "/config/revisions/"+current+"/diff", "", nil)
			if diff.Code != http.StatusInternalServerError {
				t.Errorf("a diff against a pointer naming no body = %d, want 500: %s",
					diff.Code, diff.Body)
			}
		}
	}
}
