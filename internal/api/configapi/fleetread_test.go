package configapi_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/stream"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/iam"
)

// THE ACTIVE REVISION IS THE FLEET'S, read where the fleet keeps it.
//
// Every read on this surface that answers "the active revision" asks the
// activation pointer, never this node's own copy of it: the copy trails the
// pointer by however long an apply takes, and a GET /config read from it after
// a write's 201 named the revision that write had replaced.

// failingMarks is a revision history whose local activation always fails, the
// way a node's own disk refuses a write after the fleet has taken one.
type failingMarks struct{ configapi.RevisionStore }

func (failingMarks) Activate(context.Context, string, time.Time) (string, error) {
	return "", errors.New("the disk refused the write")
}

// A 2XX WRITE IS WHAT THE NEXT READ ON ITS NODE ANSWERS, even when the node's
// own copy of the active revision could not be moved.
//
// The local mark is best effort, because the fleet has already taken the
// write and nothing can take that back. Read from the copy, a failed mark
// left GET /config answering the replaced revision until this node's
// reconciler applied the write, and a write built on that answer lost its
// compare-and-set to a revision its caller had never been shown.
//
// Mutation: read the active revision from the node's own copy again, and the
// read after the 201 names the base.
func TestAWritesRevisionIsReadBackWhenThisNodeCouldNotMarkIt(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	base := s.seed(t, companyDoc)
	s.svc.WrapRevisions(func(real configapi.RevisionStore) configapi.RevisionStore {
		return failingMarks{real}
	})

	res := s.do(t, http.MethodPatch, "/config", `{"mission": "read me back"}`, summaryHeader)
	if res.Code != http.StatusCreated {
		t.Fatalf("PATCH = %d: %s", res.Code, res.Body)
	}
	written, _ := decode(t, res)["revision_id"].(string)
	if held, _, err := s.configs.Active(t.Context()); err != nil || held.ID != base {
		t.Fatalf("the premise: this node's copy is %s (err %v), want it left on the base %s",
			held.ID, err, base)
	}

	read := s.do(t, http.MethodGet, "/config", "", nil)
	if got := read.Header().Get("ETag"); read.Code != http.StatusOK || got != `"`+written+`"` {
		t.Fatalf("GET /config = %d naming %s, want 200 naming the write's %s", read.Code, got, written)
	}
	if !strings.Contains(read.Body.String(), "read me back") {
		t.Errorf("GET /config answered the write's tag over another document: %s", read.Body)
	}
	// AND THE NEXT WRITE IS BUILT ON IT: the tag the read handed out is the
	// one the pointer's compare-and-set expects.
	next := s.do(t, http.MethodPatch, "/config", `{"mission": "and again"}`,
		map[string]string{"X-Summary": "again", "If-Match": `"` + written + `"`})
	if next.Code != http.StatusCreated {
		t.Errorf("a write built on what the read answered = %d, want 201: %s", next.Code, next.Body)
	}
}

// A WRITE ON ONE NODE IS WHAT A READ ON ANOTHER ANSWERS, at once.
//
// The other node meets the revision first through the pointer, which carries
// its body, and its reconciler keeps a copy a moment later. Read from that
// node's own store, the read named the previous revision until then, and a
// write built there lost its compare-and-set to a revision nobody had shown
// its author. Its history marks the revision the pointer names, and reads it
// by id, before the reconciler has kept it.
func TestAReadOnAnotherNodeAnswersTheWriteAtOnce(t *testing.T) {
	t.Parallel()
	plane := coordmemory.NewFleet()
	first := newSurfaceWith(t, func(o *configapi.Options) { o.Plane = plane })
	// ITS OWN STORE, and the fleet's one pointer and keyring.
	second := newSurfaceWith(t, func(o *configapi.Options) {
		o.Plane, o.Cipher = plane, first.cipher
	})
	base := first.seed(t, companyDoc)

	res := first.do(t, http.MethodPatch, "/config", `{"mission": "written on the first node"}`,
		summaryHeader)
	if res.Code != http.StatusCreated {
		t.Fatalf("PATCH on the first node = %d: %s", res.Code, res.Body)
	}
	written, _ := decode(t, res)["revision_id"].(string)

	read := second.do(t, http.MethodGet, "/config", "", nil)
	if got := read.Header().Get("ETag"); read.Code != http.StatusOK || got != `"`+written+`"` {
		t.Fatalf("GET /config on the second node = %d naming %s, want 200 naming %s: %s",
			read.Code, got, written, read.Body)
	}
	byID := second.do(t, http.MethodGet, "/config/revisions/"+written, "", nil)
	if byID.Code != http.StatusOK || decode(t, byID)["is_active"] != true {
		t.Errorf("GET /config/revisions/%s on the second node = %d %s, want 200 and active",
			written, byID.Code, byID.Body)
	}
	// AND THE HISTORY LISTS IT, ACTIVE, before the reconciler keeps it: a
	// listing of this node's rows alone left the active revision out and
	// marked nothing, so a screen said "none active" beside the document
	// GET /config had just answered.
	if row := historyRow(t, second, written); row == nil || row["is_active"] != true {
		t.Errorf("the second node's history does not list %s as active: %v", written, row)
	}
	diff := second.do(t, http.MethodGet, "/config/revisions/"+base+"/diff", "", nil)
	if diff.Code != http.StatusNotFound {
		t.Errorf("a diff of a revision the second node never met = %d, want 404: %s",
			diff.Code, diff.Body)
	}

	// THE HISTORY MARKS THE POINTER'S REVISION, not this node's flag. The
	// second node holds one revision of its own, marked active in its store,
	// which the fleet has moved past.
	own := second.seed(t, companyDoc)
	if again := first.do(t, http.MethodPatch, "/config", `{"mission": "the fleet moved on"}`,
		summaryHeader); again.Code != http.StatusCreated {
		t.Fatalf("PATCH on the first node again = %d: %s", again.Code, again.Body)
	}
	listing := second.do(t, http.MethodGet, "/config/revisions", "", nil)
	var rows []map[string]any
	if err := json.Unmarshal(listing.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode the listing: %v: %s", err, listing.Body)
	}
	for _, row := range rows {
		if row["revision_id"] == own && row["is_active"] != false {
			t.Errorf("the second node's history marks its own %s active after the fleet moved on: %v",
				own, row)
		}
	}
}

// A POINTER THIS NODE CANNOT READ IS NO ANSWER, never this node's copy.
//
// "What is the fleet running" asked of a store that cannot be reached is
// unknown, and the copy it would otherwise fall back on is exactly the answer
// that trails: a 503 that says to come back, as every condition of the node
// does.
func TestAnUnreadablePointerIsUnavailableRatherThanTheCopy(t *testing.T) {
	t.Parallel()
	down := &switchablePlane{}
	s := newSurfaceWith(t, func(o *configapi.Options) {
		down.Plane = o.Plane
		o.Plane = down
	})
	s.seed(t, companyDoc)
	cause := fmt.Errorf("%w: no leader", coord.ErrUnavailable)
	down.fail(cause)

	// THE SOCKET'S TIME TO COME BACK, for one cause: a read is told as the
	// socket's config questions and the REST query route tell it
	// ([stream.UnavailableOf]). It was the write surfaces' two seconds here
	// while the socket said five.
	want := strconv.Itoa(stream.UnavailableOf(cause).RetryAfter)
	for _, path := range []string{"/config", "/config/revisions", "/config/references"} {
		res := s.do(t, http.MethodGet, path, "", nil)
		if res.Code != http.StatusServiceUnavailable || res.Header().Get("Retry-After") != want {
			t.Errorf("GET %s with the pointer unreadable = %d (Retry-After %q), want 503 "+
				"with the socket's %s: %s", path, res.Code, res.Header().Get("Retry-After"),
				want, res.Body)
		}
	}
}

// switchablePlane is a plane whose pointer stops answering when told to.
type switchablePlane struct {
	coord.Plane
	mu  sync.Mutex
	err error
}

func (p *switchablePlane) fail(err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.err = err
}

func (p *switchablePlane) Target(ctx context.Context) (coord.Activation, bool, error) {
	p.mu.Lock()
	err := p.err
	p.mu.Unlock()
	if err != nil {
		return coord.Activation{}, false, err
	}
	return p.Plane.Target(ctx)
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

// A POINTER THAT MOVES BETWEEN ITS TWO READS IS READ AGAIN, AND ONE THAT
// NAMES A BODY IT DOES NOT CARRY IS REPORTED.
//
// The active revision is read in two steps on a node that has not kept it yet
// — the pointer, then the body it carries — and the pointer moves on between
// them whenever an activation lands there. That is a later revision to
// answer. A pointer that keeps naming a body it does not carry is not moving,
// and asking it again for ever hung the request.
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
		moving.stale = &coord.Activation{
			Epoch: target.Epoch - 1, RevisionID: "replaced-before-this-node-met-it",
			At: target.At.Add(-time.Minute), Origin: target.Origin,
		}

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
		// asked about that is not there: the error wrapped "no such
		// revision", and the diff answered 404 blaming its target.
		if stuck {
			moving.stale = &coord.Activation{
				Epoch: target.Epoch - 1, RevisionID: "replaced-before-this-node-met-it",
				At: target.At.Add(-time.Minute), Origin: target.Origin,
			}
			diff := s.do(t, http.MethodGet, "/config/revisions/"+current+"/diff", "", nil)
			if diff.Code != http.StatusInternalServerError {
				t.Errorf("a diff against a pointer naming no body = %d, want 500: %s",
					diff.Code, diff.Body)
			}
		}
	}
}

// historyRow is the row GET /config/revisions on s answers for id, or nil.
func historyRow(t *testing.T, s *surface, id string) map[string]any {
	t.Helper()
	return historyRows(t, s, "")[id]
}

// historyRows is a page of GET /config/revisions on s, by revision id, with
// query appended to the path.
func historyRows(t *testing.T, s *surface, query string) map[string]map[string]any {
	t.Helper()
	res := s.do(t, http.MethodGet, "/config/revisions"+query, "", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("GET /config/revisions%s = %d: %s", query, res.Code, res.Body)
	}
	var rows []map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &rows); err != nil {
		t.Fatalf("decode the listing: %v: %s", err, res.Body)
	}
	out := map[string]map[string]any{}
	for _, row := range rows {
		id, _ := row["revision_id"].(string)
		out[id] = row
	}
	return out
}

// THE ACTIVE REVISION IS LISTED WHERE ITS PLACE IN THE ORDER PUTS IT, page by
// page, before this node keeps it: newest first, so a write made after every
// revision this node holds opens the first page and pushes the rest one place
// down every later one. Checked to fail with the listing read from this
// node's rows alone.
func TestTheHistoryPagesARevisionThisNodeDoesNotHoldInItsPlace(t *testing.T) {
	t.Parallel()
	plane := coordmemory.NewFleet()
	first := newSurfaceWith(t, func(o *configapi.Options) { o.Plane = plane })
	second := newSurfaceWith(t, func(o *configapi.Options) {
		o.Plane, o.Cipher = plane, first.cipher
	})
	// THREE OF THE SECOND NODE'S OWN, then a write on the first.
	own := []string{second.seed(t, companyDoc), second.seed(t, companyDoc),
		second.seed(t, companyDoc)}
	first.seed(t, companyDoc)
	res := first.do(t, http.MethodPatch, "/config", `{"mission": "written elsewhere"}`,
		summaryHeader)
	if res.Code != http.StatusCreated {
		t.Fatalf("PATCH on the first node = %d: %s", res.Code, res.Body)
	}
	written, _ := decode(t, res)["revision_id"].(string)

	for _, page := range []struct {
		query string
		want  []string
	}{
		{"?limit=2", []string{written, own[2]}},
		{"?limit=2&offset=2", []string{own[1], own[0]}},
		{"?limit=2&offset=4", nil},
		{"?limit=3&offset=1", []string{own[2], own[1], own[0]}},
	} {
		rows := historyRows(t, second, page.query)
		if len(rows) != len(page.want) {
			t.Errorf("%s answered %d rows, want %v", page.query, len(rows), page.want)
			continue
		}
		for _, id := range page.want {
			if rows[id] == nil {
				t.Errorf("%s does not list %s: %v", page.query, id, rows)
			}
		}
		if row := rows[written]; row != nil && row["is_active"] != true {
			t.Errorf("%s lists the active revision as not active: %v", page.query, row)
		}
	}
}

// A FLEET REVISION THIS NODE CANNOT OPEN SAYS WHERE IT CAME FROM AND WHAT
// FIXES IT, on every route that reads it and on the socket's questions.
//
// Reads answer the pointer's body when this node has not kept it, and a body
// it cannot open was told in words written for its own copy: a key "this
// deployment no longer holds" — the deployment holds it, this node was never
// given it — and the keyring package's own error as the detail. Checked to
// fail with every unreadable revision answered as this node's own.
func TestAFleetRevisionThisNodeCannotOpenSaysWhereItCameFrom(t *testing.T) {
	t.Parallel()
	plane := coordmemory.NewFleet()
	author := newSurfaceWith(t, func(o *configapi.Options) { o.Plane = plane })
	author.seed(t, companyDoc)
	// ITS OWN KEYRING: a node never given the key the fleet seals under.
	stranger := newSurfaceWith(t, func(o *configapi.Options) { o.Plane = plane })

	for _, req := range []struct{ method, path, body string }{
		{http.MethodGet, "/config", ""},
		{http.MethodGet, "/config/references", ""},
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
		if !strings.Contains(hint, "secrets.keys") {
			t.Errorf("%s %s: the hint does not say which setting holds the key: %q",
				req.method, req.path, hint)
		}
	}

	// THE SOCKET'S QUESTION, under the same code and with words.
	_, err := stranger.service().Document(t.Context())
	var unreadable *configapi.UnreadableRevisionError
	if !errors.As(err, &unreadable) || !unreadable.Fleet {
		t.Fatalf("Document on a fleet revision this node cannot open = %v, want a "+
			"fleet UnreadableRevisionError", err)
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

// NO COMPANY IS TOLD AS THE FLEET'S: the pointer names none, which is "no
// company has been configured yet", never "this node runs none".
func TestNoCompanyIsToldAsTheFleets(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	for _, path := range []string{"/config", "/config/references"} {
		res := s.do(t, http.MethodGet, path, "", nil)
		body := decode(t, res)
		detail, _ := body["detail"].(string)
		if res.Code != http.StatusNotFound || body["error"] != "no_active_revision" ||
			!strings.Contains(detail, "No company has been configured yet") {
			t.Errorf("GET %s = %d %v, want 404 no_active_revision saying no company "+
				"has been configured", path, res.Code, body)
		}
	}
}
