package authapi_test

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
	"github.com/crewlet/crewlet/internal/statelog"
)

// cheaper is a cost below [cheap], the one every rig's hasher runs: a verifier
// written under it is one a later cost raise left behind.
var cheaper = credential.Params{Memory: 32, Time: 1, Threads: 1, KeyLen: 32}

// stronger is a cost above [cheap] in every parameter that is a cost: a
// verifier written under it is one a NEWER build wrote, read by a node still
// running this one while a rolling upgrade is under way.
var stronger = credential.Params{Memory: 128, Time: 2, Threads: 1, KeyLen: 32}

// A PASSWORD IS RE-HASHED UP BY THE SIGN-IN THAT PRESENTS IT, AND NEVER DOWN.
//
// The parameters ride in the stored verifier so a cost raise is possible at
// all: the plaintext is not stored, and the one instant a stronger digest can
// be computed is a successful sign-in. The verification reported the stale
// verifier and every caller discarded the report, so raising the cost reached
// new passwords only and left every existing one at the old cost for ever.
// Both routes that present a password — the sign-in and the step-up — now
// rewrite it at the current cost, keeping the credential's id, once they have
// answered. [authapi.Service.Stop] waits for that work, which is what makes
// the store readable here — waited for, since the stop here is given all the
// time the case has.
//
// And ONLY UP. A cost moves only with a build, so a raised one arrives as a
// rolling upgrade, and a node still on this build meets the verifiers the
// newer one wrote: read as stale because they DIFFERED, each was rewritten
// here at this build's weaker cost, and back up on the next sign-in an
// upgraded node served — every person's verifier flapping for the whole
// rollout, part of it at the cost being retired. A verifier stronger than this
// build's is left exactly as it is, with no record asked for.
//
// The CONTROL is a verifier already at the current cost: no rewrite, and no
// record asked for — so the first row is about the weaker verifier and not
// about every sign-in writing one. Mutation: drop the rehash call from either
// route and its older-cost row keeps the old parameters; report a verifier
// stale whenever its cost differs and the newer build's row is rewritten down.
func TestAPasswordIsRehashedUpByTheSignInThatPresentsItAndNeverDown(t *testing.T) {
	t.Parallel()
	for _, route := range []struct {
		name string
		run  func(t *testing.T, stored credential.Params) (*estate, string)
	}{
		{"a sign-in", func(t *testing.T, stored credential.Params) (*estate, string) {
			r := newSignInRig(t)
			before := storeVerifierAt(t, r.estate, stored)
			if got := r.login(t, "jane.doe", password, appCode(t, clock)); got != http.StatusOK {
				t.Fatalf("the sign-in answered %d", got)
			}
			r.svc.Stop(t.Context())
			return r.estate, before
		}},
		{"a step-up", func(t *testing.T, stored credential.Params) (*estate, string) {
			r := newStepUpRig(t, session.RowValid)
			before := storeVerifierAt(t, r.estate, stored)
			if rec := r.stepUp(t); rec.Code != http.StatusOK {
				t.Fatalf("the step-up answered %d (%s)", rec.Code, rec.Body)
			}
			r.svc.Stop(t.Context())
			return r.estate, before
		}},
	} {
		for _, stored := range []struct {
			name      string
			cost      credential.Params
			rewritten bool
		}{
			{"verifier at an older cost", cheaper, true},
			{"verifier at the current cost (the control)", cheap, false},
			{"verifier at a newer build's stronger cost", stronger, false},
		} {
			t.Run(route.name+", "+stored.name, func(t *testing.T) {
				t.Parallel()
				e, before := route.run(t, stored.cost)
				assertRehashed(t, e, cheap, before, stored.rewritten)
			})
		}
	}
}

// A STALE VERIFIER IS REWRITTEN AT THE COST THAT SHIPS.
//
// The rewrite was given what was left of the refusal pad — four hundred
// milliseconds from arrival — after a full 64 MiB verification had spent most
// of it, and then ran a second full derivation past that deadline, so the write
// that followed ran on an expired context and failed: on real hardware the
// verifier was never rewritten, and every such sign-in paid for a second
// derivation. The suite above runs at a cost a test can afford and could not
// see it. This one's hasher is the SHIPPED cost ([credential.Default]) over a
// verifier stored at a cheaper one, and the rewrite has budgets of its own.
//
// Mutation: bound the rewrite by the refusal pad again and the verifier stays
// at the cheaper cost.
func TestAStaleVerifierIsRewrittenAtTheCostThatShips(t *testing.T) {
	t.Parallel()
	r := newSignInRigWith(t, func(o *authapi.Options) {
		o.Hasher = credential.NewHasher(credential.Default(), 1)
	})
	before := storeVerifierAt(t, r.estate, cheap)
	if got := r.login(t, "jane.doe", password, appCode(t, clock)); got != http.StatusOK {
		t.Fatalf("the sign-in answered %d", got)
	}
	r.svc.Stop(t.Context())
	assertRehashed(t, r.estate, credential.Default(), before, true)
}

// A SIGN-IN NEVER WAITS FOR ITS VERIFIER'S REWRITE, AND STOPPING ENDS IT.
//
// The person is waiting for a session, not for a stronger digest: the rewrite
// starts once the sign-in has answered and nothing the answer carries depends
// on it. And it is the surface's own work, so [authapi.Service.Stop] cancels
// it and returns — a goroutine nobody can stop would go on writing to an engine
// the node is tearing down.
//
// Here the rewrite's write never lands on its own: the sign-in must have
// answered while it is still in flight, and a Stop with no time left must cut
// it and return with the verifier untouched. Mutation: run the rewrite inline
// and the sign-in answers only once it has ended; leave it outside Stop's
// reach and Stop never returns.
func TestASignInNeverWaitsForItsVerifiersRewrite(t *testing.T) {
	t.Parallel()
	held := &heldRewrite{started: make(chan struct{}), returned: make(chan struct{})}
	r := newSignInRigWith(t, func(o *authapi.Options) {
		held.estate = o.Writer.(*estate)
		o.Writer = held
	})
	before := storeVerifierAt(t, r.estate, cheaper)

	answered := make(chan int, 1)
	go func() { answered <- r.login(t, "jane.doe", password, appCode(t, clock)) }()
	select {
	case got := <-answered:
		if got != http.StatusOK {
			t.Fatalf("the sign-in answered %d", got)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("the sign-in waited for its verifier's rewrite")
	}
	select {
	case <-held.started:
	case <-time.After(30 * time.Second):
		t.Fatal("no rewrite was ever asked for; this case tests nothing")
	}
	select {
	case <-held.returned:
		t.Fatal("the sign-in answered only once its verifier's rewrite had " +
			"ended — the person waited for a digest instead of a session")
	default:
	}

	// A STOP WITH NO TIME LEFT, as one past the listener's grace is: it
	// must cut the rewrite rather than wait for a write that never lands.
	cut, cancel := context.WithCancel(t.Context())
	cancel()
	stopped := make(chan struct{})
	go func() {
		r.svc.Stop(cut)
		close(stopped)
	}()
	select {
	case <-stopped:
	case <-time.After(30 * time.Second):
		t.Fatal("Stop did not end a rewrite in flight")
	}
	r.estate.mu.Lock()
	after := r.estate.person.Credentials[0].Verifier
	r.estate.mu.Unlock()
	if after != before {
		t.Error("a rewrite Stop cancelled went on to store its verifier")
	}
}

// heldRewrite is an estate whose re-hash writes never land on their own: they
// wait for their context, as a write to a broker that never answers does.
// started closes when one begins and returned when it gives up.
type heldRewrite struct {
	*estate
	started, returned chan struct{}
}

func (h *heldRewrite) SetCredentials(ctx context.Context,
	in iamdomain.CredentialSet) (statelog.Result, error) {

	if !strings.HasPrefix(in.OpID, "rehash:") {
		return h.estate.SetCredentials(ctx, in)
	}
	close(h.started)
	defer close(h.returned)
	<-ctx.Done()
	return statelog.Result{Outcome: statelog.OutcomeUnknown, OpID: in.OpID}, ctx.Err()
}

// assertRehashed holds the estate's password to the cost it should be at now:
// rewritten at the current cost by exactly one re-hash write, or — where it was
// not to be rewritten — the very verifier it held before, with no re-hash
// write asked for at all.
func assertRehashed(t *testing.T, e *estate, current credential.Params,
	before string, rewritten bool) {

	t.Helper()
	e.mu.Lock()
	defer e.mu.Unlock()
	pw := e.person.Credentials[0]
	if pw.Method != iamdomain.MethodPassword || pw.ID != "pw" {
		t.Fatalf("the password credential is now %+v — a re-hash "+
			"keeps the credential and changes its verifier", pw)
	}
	rehashes := 0
	for _, op := range e.credentialOps {
		if strings.HasPrefix(op, "rehash:") {
			rehashes++
		}
	}
	switch {
	case rewritten && !strings.Contains(pw.Verifier, params(current)):
		t.Errorf("after the sign-in the verifier is %q, want it "+
			"at the current cost %s", pw.Verifier, params(current))
	case rewritten && rehashes != 1:
		t.Errorf("asked for %d re-hash writes, want 1", rehashes)
	case !rewritten && rehashes != 0:
		t.Errorf("a verifier that was not weaker than the current cost "+
			"was rewritten %d times", rehashes)
	case !rewritten && pw.Verifier != before:
		t.Errorf("the verifier moved from %q to %q with no re-hash "+
			"asked for", before, pw.Verifier)
	}
	if ok, _, _ := credential.NewHasher(current, 1).Verify(t.Context(), "",
		pw.Verifier, password); !ok {
		t.Error("the stored verifier no longer verifies the password")
	}
}

// storeVerifierAt replaces the person's password verifier with one written
// under cost — an older cost a deployment has since raised, the current one,
// or a newer build's — and answers the verifier it stored.
func storeVerifierAt(t *testing.T, e *estate, cost credential.Params) string {
	t.Helper()
	verifier, err := credential.NewHasher(cost, 1).Hash(t.Context(), "", password)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(verifier, params(cost)) {
		t.Fatalf("the stored verifier %q is not at %s; this case tests nothing",
			verifier, params(cost))
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.person.Credentials[0].Verifier = verifier
	return verifier
}

// params is how a verifier spells its cost.
func params(p credential.Params) string {
	return "m=" + strconv.Itoa(int(p.Memory)) + ",t=" + strconv.Itoa(int(p.Time)) +
		",p=" + strconv.Itoa(int(p.Threads))
}
