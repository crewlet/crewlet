package engine_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/configplane"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/kv/kvtest"
	"github.com/crewlet/crewlet/internal/engine"
)

// pointerLinkRate is the pace the cases hold a leaf's link to, each way: 160
// KiB a second, about five times the slowest a link is allowed, so a body at
// its bound takes about fifty seconds to cross back: past a probe's 1.875 s and
// a read's flat 5 s, far inside what it is given at the slowest pace.
const pointerLinkRate = 160 << 10

// bodyRoom is how far short of what one key holds a case's body is: a
// kibibyte, so the body is at its bound and never past it.
const bodyRoom = 1 << 10

// A RECONCILER ACROSS A SLOW LINK APPLIES A REVISION AT ITS BOUND. A node
// without `data` reads the pointer and the body across its leaf link: the
// pointer, small, at the read term, and the body, about fifty seconds here,
// given the time its recorded size takes to arrive. When the body rode the
// pointer, the tick's read of the pointer carried the whole document under a
// flat five seconds, and failed every tick.
//
// Mutation: read the body at the read term, and the tick fails.
func TestAReconcilerAcrossASlowLinkAppliesARevisionAtItsBound(t *testing.T) {
	t.Parallel()
	across := kvtest.SlowLeaf(t, pointerLinkRate)
	e := newEngine(t, engine.Options{})
	p := planeFor(t, e, func(o *engine.ReconcilerOptions) { o.Fleet = across.Leaf })
	body := kvtest.SealedAtTheBound(t, e.Cipher(), yamlToJSON(t, grownCompanyDoc), bodyRoom)
	published, err := across.Member.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: uuid.NewString(), Summary: "a company at its bound", Payload: body,
		At: pinnedNow, Origin: anOrigin})
	if err != nil {
		t.Fatalf("activate a company of %d bytes sealed through the member: %v", len(body), err)
	}
	if err := p.recon.Tick(t.Context()); err != nil {
		t.Fatalf("a tick across the leaf's link: %v", err)
	}
	if got := p.recon.Applied(); got != published.Epoch {
		t.Errorf("the node applied epoch %d, want %d", got, published.Epoch)
	}
	if got := dayCeiling(e.Company()); got != grownDayCeiling {
		t.Errorf("the node runs a company with a day ceiling of %d, want the activated one's %d",
			got, grownDayCeiling)
	}
}

// A POSTURE ACROSS A SLOW LINK IS READ, NOT ASSUMED. Every probe and heartbeat
// reads the activation pointer inside internal/engine's ProbeReadBudget; with
// the body in it, a node across a leaf's link could not read it in time while a
// large company was active, and its posture fell back to a fail-open serve it
// had not concluded. The pointer alone crosses in milliseconds.
//
// Mutation: carry the body in the pointer again, and the read errors.
func TestAPostureAcrossASlowLinkIsReadNotAssumed(t *testing.T) {
	t.Parallel()
	across := kvtest.SlowLeaf(t, pointerLinkRate)
	e := newEngine(t, engine.Options{})
	p := planeFor(t, e, func(o *engine.ReconcilerOptions) { o.Fleet = across.Leaf })
	if _, err := across.Member.Activate(t.Context(), coord.ActivationRequest{
		RevisionID: uuid.NewString(), Summary: "a company at its bound",
		Payload: kvtest.AtTheBound(t, []byte(`{"name":"Acme"}`), bodyRoom), At: pinnedNow,
		Origin: anOrigin}); err != nil {
		t.Fatalf("activate a body at its bound through the member: %v", err)
	}
	posture, err := p.recon.ReadPosture(t.Context())
	if err != nil {
		t.Fatalf("the posture across the leaf's link was not read: %v", err)
	}
	if posture != configplane.PostureWait {
		t.Errorf("a node that has not applied the epoch it just read concludes %s, want %s",
			posture, configplane.PostureWait)
	}
}
