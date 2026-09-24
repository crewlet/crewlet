package configapi_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/store"
)

// A WRITE KEEPS THE INSTANT THE POINTER PUBLISHED IT AT, not the one it asked
// for.
//
// The pointer publishes an instant later than the activation it replaces
// (coord.ActivationAt), so a write made on a node whose clock runs behind the
// node that activated last is published later than this node's clock says.
// The local copy's `activated_at` is what this node boots its chart with next
// time; left at the instant it asked for, a restart would stamp the chart with
// one older than the fleet's and walk nothing forward.
func TestAWriteKeepsThePointersInstant(t *testing.T) {
	t.Parallel()
	s := newSurface(t, nil)
	if res := s.do(t, http.MethodPut, "/config", companyJSONDoc,
		map[string]string{"X-Summary": "create"}); res.Code != http.StatusCreated {
		t.Fatalf("create: %d — %s", res.Code, res.Body)
	}
	base, found, err := s.configs.Active(t.Context())
	if err != nil || !found {
		t.Fatalf("active revision: %v (found=%v)", err, found)
	}
	// ANOTHER NODE, WITH A CLOCK AN HOUR AHEAD, re-activates the same
	// revision — the credential-rotation gesture.
	ahead := pinned.Add(time.Hour)
	if _, err = s.plane.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: base.ID, Payload: base.Payload, At: ahead,
	}); err != nil {
		t.Fatalf("the other node's activation: %v", err)
	}

	edited := strings.Replace(companyJSONDoc, `"name":"Acme"`, `"name":"Acme Renamed"`, 1)
	if res := s.do(t, http.MethodPut, "/config", edited,
		map[string]string{"X-Summary": "rename"}); res.Code != http.StatusCreated {
		t.Fatalf("rename: %d — %s", res.Code, res.Body)
	}
	target, _, err := s.plane.Target(t.Context())
	if err != nil {
		t.Fatalf("Target: %v", err)
	}
	if !target.At.After(ahead) {
		t.Fatalf("the premise: the rename is published at %s, not after the %s "+
			"it replaced", target.At, ahead)
	}
	active, _, err := s.configs.Active(t.Context())
	if err != nil {
		t.Fatalf("active revision: %v", err)
	}
	if active.ID != target.RevisionID ||
		store.EncodeTime(active.ActivatedAt) != store.EncodeTime(target.At) {
		t.Fatalf("this node holds %s activated at %s, and the pointer names %s at "+
			"%s — a restart would stamp the chart with an instant the fleet never "+
			"applied", active.ID, active.ActivatedAt, target.RevisionID, target.At)
	}
}
