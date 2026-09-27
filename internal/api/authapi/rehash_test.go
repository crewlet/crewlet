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

// A PASSWORD UNDER AN OLDER COST IS RE-HASHED BY THE SIGN-IN THAT PRESENTS IT.
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
// The CONTROL is a verifier already at the current cost: no rewrite, and no
// record asked for — so the first half is about the stale verifier and not
// about every sign-in writing one. Mutation: drop the rehash call from either
// route and its stale case keeps the old parameters.
func TestAPasswordUnderAnOlderCostIsRehashedByTheSignInThatPresentsIt(t *testing.T) {
	t.Parallel()
	for _, route := range []struct {
		name string
		run  func(t *testing.T, stale bool) *estate
	}{
		{"a sign-in", func(t *testing.T, stale bool) *estate {
			r := newSignInRig(t)
			if stale {
				restale(t, r.estate)
			}
			if got := r.login(t, "jane.doe", password, appCode(t, clock)); got != http.StatusOK {
				t.Fatalf("the sign-in answered %d", got)
			}
			r.svc.Stop(t.Context())
			return r.estate
		}},
		{"a step-up", func(t *testing.T, stale bool) *estate {
			r := newStepUpRig(t, session.RowValid)
			if stale {
				restale(t, r.estate)
			}
			if rec := r.stepUp(t); rec.Code != http.StatusOK {
				t.Fatalf("the step-up answered %d (%s)", rec.Code, rec.Body)
			}
			r.svc.Stop(t.Context())
			return r.estate
		}},
	} {
		for _, stale := range []bool{true, false} {
			name := route.name + ", verifier at the current cost (the control)"
			if stale {
				name = route.name + ", verifier at an older cost"
			}
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				e := route.run(t, stale)
				assertRehashed(t, e, cheap, stale)
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
	if got := r.login(t, "jane.doe", password, appCode(t, clock)); got != http.StatusOK {
		t.Fatalf("the sign-in answered %d", got)
	}
	r.svc.Stop(t.Context())
	assertRehashed(t, r.estate, credential.Default(), true)
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
	restale(t, r.estate)
	before := r.estate.person.Credentials[0].Verifier

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

// assertRehashed holds the estate's password to the cost it should be at now,
// and to exactly as many re-hash writes as a stale verifier asks for.
func assertRehashed(t *testing.T, e *estate, current credential.Params, stale bool) {
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
	case stale && !strings.Contains(pw.Verifier, params(current)):
		t.Errorf("after the sign-in the verifier is %q, want it "+
			"at the current cost %s", pw.Verifier, params(current))
	case stale && rehashes != 1:
		t.Errorf("asked for %d re-hash writes, want 1", rehashes)
	case !stale && rehashes != 0:
		t.Errorf("a verifier already at the current cost was "+
			"rewritten %d times", rehashes)
	}
	if ok, _ := credential.NewHasher(current, 1).Verify(pw.Verifier,
		password); !ok {
		t.Error("the stored verifier no longer verifies the password")
	}
}

// restale replaces the person's password verifier with one written under
// [cheaper], as a deployment that has since raised its cost holds.
func restale(t *testing.T, e *estate) {
	t.Helper()
	old, err := credential.NewHasher(cheaper, 1).Hash(password)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(old, params(cheaper)) {
		t.Fatalf("the stale verifier %q is not at %s; this case tests nothing",
			old, params(cheaper))
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.person.Credentials[0].Verifier = old
}

// params is how a verifier spells its cost.
func params(p credential.Params) string {
	return "m=" + strconv.Itoa(int(p.Memory)) + ",t=" + strconv.Itoa(int(p.Time)) +
		",p=" + strconv.Itoa(int(p.Threads))
}
