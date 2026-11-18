package authapi_test

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
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
// The plaintext is not stored, so a successful sign-in is the one instant a
// stronger digest can be computed: the sign-in and the step-up rewrite it at
// the current cost once answered ([authapi.Service.Stop] waits for that work).
// ONLY UP: a raised cost arrives as a rolling upgrade, so rewriting verifiers a
// newer build wrote for merely DIFFERING would flap them all rollout long.
// The CONTROL is a verifier at the current cost: no rewrite, no record.
// Mutation: drop the rehash call from either route, or report stale whenever
// the cost differs, and the older-cost or newer-build row goes red.
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
// The rewrite must not borrow the refusal pad (400 ms from arrival, mostly
// spent by a full 64 MiB verification): the derivation would overrun it and the
// write fail on an expired context. The suite above runs at a cost a test can
// afford and cannot see that; this hasher is the SHIPPED cost
// ([credential.Default]) over a cheaper stored verifier, and the rewrite has
// budgets of its own: none of the request's, which ends when the sign-in
// answers. Mutation: run the rewrite on the request's context and the write
// meets a cancelled one, leaving the cheaper cost.
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
// The rewrite starts once the sign-in has answered, as nothing the answer
// carries depends on it. It is the surface's own work, so
// [authapi.Service.Stop] cancels it and returns; a goroutine nobody can stop
// would write to an engine being torn down.
//
// The rewrite's write never lands on its own here: the sign-in must answer
// while it is in flight, and a Stop with no time left must cut it with the
// verifier untouched. Mutation: run the rewrite inline
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

	if !isRehash(in.OpID) {
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
		if isRehash(op) {
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
	if ok, _, _ := credential.NewHasher(current, 1).Verify(t.Context(),
		pw.Verifier, password); !ok {
		t.Error("the stored verifier no longer verifies the password")
	}
}

// storeVerifierAt replaces the person's password verifier with one written
// under cost — an older cost a deployment has since raised, the current one,
// or a newer build's — and answers the verifier it stored.
func storeVerifierAt(t *testing.T, e *estate, cost credential.Params) string {
	t.Helper()
	verifier, err := credential.NewHasher(cost, 1).Hash(t.Context(), password)
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

// isRehash reports whether an operation id is a re-hash's: minted now in the
// engine's grammar and named for what it does — an id that carried no instant
// would be answered `unknown` without being published once the ledger had
// swept anything.
func isRehash(op string) bool {
	_, minted := statelog.OpMintedAt(op)
	return minted && strings.HasSuffix(op, ".rehash")
}

// budgetedRewrite records how much of its budget a re-hash's WRITE was handed.
type budgetedRewrite struct {
	*estate
	mu        sync.Mutex
	remaining time.Duration
	seen      bool
}

func (b *budgetedRewrite) SetCredentials(ctx context.Context,
	in iamdomain.CredentialSet) (statelog.Result, error) {

	if isRehash(in.OpID) {
		deadline, bounded := ctx.Deadline()
		b.mu.Lock()
		b.seen = bounded
		b.remaining = time.Until(deadline)
		b.mu.Unlock()
	}
	return b.estate.SetCredentials(ctx, in)
}

// A REWRITE'S WRITE IS HANDED ITS WHOLE BUDGET, WHATEVER THE DERIVATION BEFORE
// IT COST.
//
// The budget is the publisher's own resolve budget, sized for what the write
// waits on. Started before the argon2id derivation it would be spent by it, and
// on a loaded host the write would begin on an expired context and the verifier
// never be rewritten. It is bounded from where the write starts, measured
// against one derivation on this host so the case holds on fast and slow
// machines alike.
//
// Mutation: start the budget before the derivation again and the write is
// handed the budget less a derivation.
func TestARewritesWriteIsHandedItsWholeBudget(t *testing.T) {
	t.Parallel()
	start := time.Now()
	if _, err := credential.NewHasher(credential.Default(), 1).Hash(t.Context(),
		password); err != nil {
		t.Fatal(err)
	}
	derivation := time.Since(start)

	var writer *budgetedRewrite
	r := newSignInRigWith(t, func(o *authapi.Options) {
		o.Hasher = credential.NewHasher(credential.Default(), 1)
		writer = &budgetedRewrite{estate: o.Writer.(*estate)}
		o.Writer = writer
	})
	storeVerifierAt(t, r.estate, cheap)
	if got := r.login(t, "jane.doe", password, appCode(t, clock)); got != http.StatusOK {
		t.Fatalf("the sign-in answered %d", got)
	}
	r.svc.Stop(t.Context())
	writer.mu.Lock()
	defer writer.mu.Unlock()
	if !writer.seen {
		t.Fatal("no bounded rewrite was asked for; this case tests nothing")
	}
	// THE PUBLISHER'S OWN RESOLVE BUDGET is the rewrite's, by its doc.
	if floor := statelog.DefaultResolveBudget - derivation/2; writer.remaining < floor {
		t.Errorf("the rewrite's write was handed %s of its %s budget — a "+
			"derivation here takes %s, and the write's budget must not pay it",
			writer.remaining, statelog.DefaultResolveBudget, derivation)
	}
}
