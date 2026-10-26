package configapi_test

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/store"
)

// Every answer this surface gives is about the FLEET's revision, the one the
// activation pointer names, and never about the revision this node boots on
// (configplane's package doc, ADR-0057). These cases hold the node in the
// states where the two differ — a tick still applying, a revision this node
// refused, no pointer at all — and read and write through the surface.

// boot makes a revision this node holds what it boots on, as its reconciler
// does once the engine has applied it.
func (s *surface) boot(t *testing.T, revisionID string) {
	t.Helper()
	revision, found, err := s.configs.Get(t.Context(), revisionID)
	if err != nil || !found {
		t.Fatalf("boot on %s: not in the history (found=%v err=%v)", revisionID, found, err)
	}
	revision.ActivatedAt = pinned
	if err := s.configs.Serve(t.Context(), revision); err != nil {
		t.Fatalf("boot on %s: %v", revisionID, err)
	}
}

// fleetCopy is a revision as the fleet's pointer carries it: sealed under the
// surface's keyring, with the origin and the instant every copy needs.
func (s *surface) fleetCopy(t *testing.T, id, doc string) store.Revision {
	t.Helper()
	payload, err := secrets.Seal(s.cipher, companyJSON(t, doc))
	if err != nil {
		t.Fatal(err)
	}
	return store.Revision{
		ID: id, Payload: payload, Summary: "from the fleet",
		CreatedBy: "maya", CreatedByKind: iam.ActorOperator, Source: "api",
		CreatedAt: pinned, ActivatedAt: pinned,
	}
}

// offline is a revision an offline command writes: the fleet has activated
// nothing of it yet.
func (s *surface) offline(t *testing.T, id, doc string) store.Revision {
	t.Helper()
	revision := s.fleetCopy(t, id, doc)
	revision.ActivatedAt = time.Time{}
	revision.Source = "cli"
	return revision
}

// bootedOn is the revision this node boots on, or "" for none.
func (s *surface) bootedOn(t *testing.T) string {
	t.Helper()
	booted, found, err := s.configs.Boot(t.Context())
	if err != nil {
		t.Fatalf("Boot: %v", err)
	}
	if !found {
		return ""
	}
	return booted.ID
}

// etagOf is GET /config's entity-tag, unquoted.
func (s *surface) etagOf(t *testing.T) string {
	t.Helper()
	res := s.do(t, http.MethodGet, "/config", "", nil)
	if res.Code != http.StatusOK {
		t.Fatalf("GET /config = %d: %s", res.Code, res.Body)
	}
	return strings.Trim(res.Header().Get("ETag"), `"`)
}

// activeInList is the revision the history listing marks `is_active`, or ""
// for none.
func (s *surface) activeInList(t *testing.T) string {
	t.Helper()
	res := s.do(t, http.MethodGet, "/config/revisions", "", nil)
	var revisions []map[string]any
	if err := json.Unmarshal(res.Body.Bytes(), &revisions); err != nil {
		t.Fatalf("GET /config/revisions = %d %s: %v", res.Code, res.Body, err)
	}
	active := ""
	for _, row := range revisions {
		if row["is_active"] == true {
			if active != "" {
				t.Errorf("the history marks both %s and %v active", active, row["revision_id"])
			}
			active, _ = row["revision_id"].(string)
		}
	}
	return active
}

// renamed is companyDoc under another name, so two revisions differ.
func renamed(name string) string {
	return strings.Replace(companyDoc, "name: Acme\n", "name: "+name+"\n", 1)
}

// EVERY READ AFTER A WRITE ANSWERS THE WRITE, on a node that has not applied it
// (#316).
//
// The reconciler applies a revision some time after the pointer names it, and
// on a node in the middle of applying the revision before, that is a whole
// apply later. Answered from what the node boots on, every read in that window
// named the revision before the write: the operator's next `If-Match`, naming
// the write, was refused as stale, and the revision list said the write was not
// the company. The read after a write is the pointer's, and the write moves
// nothing this node boots on: only its reconciler does, once it has applied it.
//
// Mutations: answer `current` from the boot row and the reads name the first
// revision; move the boot row in the write's commit and this node boots on a
// revision it never applied.
func TestEveryReadAfterAWriteAnswersTheWrite(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	first := s.seed(t, companyDoc)
	s.boot(t, first)

	res := s.do(t, http.MethodPut, "/config", renamed("Acme Two"), summaryHeader)
	if res.Code != http.StatusCreated {
		t.Fatalf("PUT = %d, want 201: %s", res.Code, res.Body)
	}
	second, _ := decode(t, res)["revision_id"].(string)

	// EVERY read, however many, and from as many callers at once as a
	// dashboard and an operator make: a read that moved anything would show
	// as two answers.
	var wg sync.WaitGroup
	for range 20 {
		wg.Go(func() {
			got := s.do(t, http.MethodGet, "/config", "", nil)
			if tag := strings.Trim(got.Header().Get("ETag"), `"`); tag != second {
				t.Errorf("GET /config after the write answers %s, want the write %s", tag, second)
			}
		})
	}
	wg.Wait()
	if active := s.activeInList(t); active != second {
		t.Errorf("the history marks %s as the company, want the write %s", active, second)
	}
	if booted := s.bootedOn(t); booted != first {
		t.Errorf("the write made this node boot on %s, want %s until its engine "+
			"applies the write", booted, first)
	}

	patched := s.do(t, http.MethodPatch, "/config", `{"mission": "next"}`,
		map[string]string{"X-Summary": "the next edit", "If-Match": `"` + second + `"`})
	if patched.Code != http.StatusCreated {
		t.Fatalf("PATCH If-Match the write = %d, want 201: %s", patched.Code, patched.Body)
	}
}

// A WRITE THE WRITER'S OWN NODE REFUSES STAYS CORRECTABLE THROUGH ITS API (S2).
//
// A revision that lands and then fails to apply on the node that served it —
// an unset `${VAR}`, a provider this build has no profile for — leaves that
// node running the one before. Every write built on what the node runs lost
// the compare-and-set against the pointer, which names the refused revision,
// so every correction answered 409 and nothing the API could send would ever
// clear it: the surface that writes the fix was the one surface that could
// not. Built on the pointer, each correction lands, and each is built on the
// one before it.
//
// Mutation: answer `current` from the boot row and the first correction is
// refused.
func TestAWriteTheWriterRefusesStaysCorrectableThroughItsAPI(t *testing.T) {
	t.Parallel()
	s := newSurface(t)
	served := s.seed(t, companyDoc)
	s.boot(t, served)
	refused := s.seed(t, renamed("Acme Refused"))
	if s.bootedOn(t) != served {
		t.Fatalf("the premise: this node boots on %s, not the %s it served", s.bootedOn(t), served)
	}

	if tag := s.etagOf(t); tag != refused {
		t.Fatalf("GET /config = ETag %s, want the fleet's %s", tag, refused)
	}
	base := refused
	for _, write := range []struct {
		name, method, path, body string
		headers                  map[string]string
	}{
		{"a patch naming the company", http.MethodPatch, "/config", `{"mission": "fixed"}`,
			map[string]string{"X-Summary": "fix", "If-Match": `"` + refused + `"`}},
		{"a whole document", http.MethodPut, "/config", renamed("Acme Fixed"), summaryHeader},
		{"a reload", http.MethodPost, "/config/reload", "", nil},
	} {
		res := s.do(t, write.method, write.path, write.body, write.headers)
		if res.Code != http.StatusCreated {
			t.Fatalf("%s = %d, want 201: %s", write.name, res.Code, res.Body)
		}
		written, _ := decode(t, res)["revision_id"].(string)
		revision, held, err := s.configs.Get(t.Context(), written)
		if err != nil || !held || revision.ParentID != base {
			t.Errorf("%s stored %+v (held=%v err=%v), want it built on %s",
				write.name, revision.ID, held, err, base)
		}
		base = written
	}
	if s.bootedOn(t) != served {
		t.Errorf("a write moved what this node boots on to %s", s.bootedOn(t))
	}
}

// A READ NEVER WRITES THE NODE'S STORE, even of the revision it answers.
//
// A node that has not ticked since the pointer moved holds no row for the
// fleet's revision, and every read here answers it all the same, from the body
// the pointer carries. Keeping that copy is the reconciler's, which records
// what it saw: a read that kept it would race the tick with a row the tick did
// not write, from a request that may be no more than a dashboard refresh.
//
// Mutation: keep the pointer's revision from `current` and the history holds a
// row no write made.
func TestAReadNeverWritesTheNodeStore(t *testing.T) {
	t.Parallel()
	s, fleets := laggingNode(t)
	for _, path := range []string{
		"/config", "/config/revisions", "/config/revisions/" + fleets,
		"/config/revisions/" + fleets + "/diff?against=active", "/config/charter",
		"/config/references",
	} {
		if res := s.do(t, http.MethodGet, path, "", nil); res.Code != http.StatusOK {
			t.Errorf("GET %s = %d, want the fleet's revision answered: %s", path, res.Code, res.Body)
		}
	}
	if tag := s.etagOf(t); tag != fleets {
		t.Errorf("GET /config = ETag %s, want the fleet's %s", tag, fleets)
	}
	if got := s.writes(); got != (writeCounts{}) {
		t.Errorf("the reads wrote %+v", got)
	}
	if n := s.revisionCount(t); n != 0 {
		t.Errorf("the reads left %d revisions in this node's history, want none", n)
	}
	if _, held, err := s.configs.Get(t.Context(), fleets); err != nil || held {
		t.Errorf("a read kept the fleet's revision (held=%v err=%v)", held, err)
	}
}

// WITH NO POINTER, THE COMPANY IS WHAT THIS NODE KNOWS IT TO BE: the fleet's
// revision as it last saw it, or a change made here on that.
//
// The pointer is gone after a coordination store is rebuilt, and before any
// node has published one. What this node BOOTS on is not the answer: on a node
// that refused the fleet's revision it is the one before, and a write built on
// it would roll every node that runs the fleet's revision back.
//
// Mutation: answer the boot row from Restorable and the first two cases name
// the revision this node runs.
func TestWithNoPointerTheCompanyIsTheFleetsLastRevisionThisNodeSaw(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// set up this node's store; the plane has no pointer
		setup func(t *testing.T, s *surface)
		want  string
	}{
		{"the fleet's revision, though this node refused it", func(t *testing.T, s *surface) {
			if err := s.configs.Serve(t.Context(), s.fleetCopy(t, "runs", companyDoc)); err != nil {
				t.Fatal(err)
			}
			if err := s.configs.SawFleet(t.Context(), s.fleetCopy(t, "refused", renamed("Acme R"))); err != nil {
				t.Fatal(err)
			}
		}, "refused"},
		{"an offline change made on the fleet's revision", func(t *testing.T, s *surface) {
			if err := s.configs.Serve(t.Context(), s.fleetCopy(t, "runs", companyDoc)); err != nil {
				t.Fatal(err)
			}
			if err := s.configs.SawFleet(t.Context(), s.fleetCopy(t, "fleets", renamed("Acme R"))); err != nil {
				t.Fatal(err)
			}
			if _, err := s.configs.Offer(t.Context(), store.Offer{Base: "fleets"}, s.offline(t, "offered", renamed("Acme O"))); err != nil {
				t.Fatal(err)
			}
		}, "offered"},
		{"not an offline change made on what the fleet moved past", func(t *testing.T, s *surface) {
			if err := s.configs.SawFleet(t.Context(), s.fleetCopy(t, "fleets", renamed("Acme R"))); err != nil {
				t.Fatal(err)
			}
			if _, err := s.configs.Offer(t.Context(), store.Offer{Base: "older"}, s.offline(t, "offered", renamed("Acme O"))); err != nil {
				t.Fatal(err)
			}
		}, "fleets"},
		{"what this node runs, when it never saw a pointer", func(t *testing.T, s *surface) {
			if err := s.configs.Serve(t.Context(), s.fleetCopy(t, "runs", companyDoc)); err != nil {
				t.Fatal(err)
			}
		}, "runs"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newSurface(t)
			tc.setup(t, s)
			if tag := s.etagOf(t); tag != tc.want {
				t.Errorf("GET /config = ETag %s, want %s", tag, tc.want)
			}
			if active := s.activeInList(t); active != tc.want {
				t.Errorf("the history marks %q as the company, want %s", active, tc.want)
			}
			// A company exists, so creating one is refused.
			res := s.do(t, http.MethodPut, "/config", companyJSONDoc,
				map[string]string{"X-Summary": "create", "If-None-Match": "*"})
			if res.Code != http.StatusPreconditionFailed {
				t.Errorf("a create-only PUT = %d, want 412: %s", res.Code, res.Body)
			}
		})
	}

	t.Run("nothing at all", func(t *testing.T) {
		t.Parallel()
		s := newSurface(t)
		if res := s.do(t, http.MethodGet, "/config", "", nil); res.Code != http.StatusNotFound {
			t.Errorf("GET /config on a node that knows no company = %d, want 404: %s",
				res.Code, res.Body)
		}
	})
}

// spyPlane records every activation a write asks for.
type spyPlane struct {
	coord.Plane
	mu        sync.Mutex
	requested []coord.ActivationRequest
}

func (p *spyPlane) Activate(ctx context.Context, req coord.ActivationRequest) (coord.Activation, error) {
	p.mu.Lock()
	p.requested = append(p.requested, req)
	p.mu.Unlock()
	return p.Plane.Activate(ctx, req)
}

// A WRITE WITH NO POINTER IS CREATE-ONLY AT THE POINTER (#320), whatever it was
// built on.
//
// A write that read no pointer was built on what this node knows, and expecting
// THAT at the compare-and-set names a revision the pointer does not: under the
// strict expectation it lands only while the pointer names it, which it never
// will, and under the old lax one a pointer recreated in between was replaced
// unconditionally. What the write saw was no pointer, so that is what it
// expects.
//
// Mutation: expect the base's id and the write races the absent pointer.
func TestAWriteWithNoPointerIsCreateOnly(t *testing.T) {
	t.Parallel()
	spy := &spyPlane{}
	s := newSurfaceWith(t, func(o *configapi.Options) {
		spy.Plane = o.Plane
		o.Plane = spy
	})
	known := s.fleetCopy(t, "known", companyDoc)
	if err := s.configs.Serve(t.Context(), known); err != nil {
		t.Fatal(err)
	}

	res := s.do(t, http.MethodPut, "/config", renamed("Acme Again"), summaryHeader)
	if res.Code != http.StatusCreated {
		t.Fatalf("PUT = %d, want 201: %s", res.Code, res.Body)
	}
	written, _ := decode(t, res)["revision_id"].(string)
	if revision, _, err := s.configs.Get(t.Context(), written); err != nil || revision.ParentID != known.ID {
		t.Errorf("the write is built on %q (err %v), want what this node knows, %s",
			revision.ParentID, err, known.ID)
	}
	spy.mu.Lock()
	defer spy.mu.Unlock()
	if len(spy.requested) != 1 {
		t.Fatalf("the write asked for %d activations, want one", len(spy.requested))
	}
	if req := spy.requested[0]; !req.ExpectAbsent || req.Expect != "" {
		t.Errorf("the write expected %q (absent=%v), want no pointer and nothing else",
			req.Expect, req.ExpectAbsent)
	}
}
