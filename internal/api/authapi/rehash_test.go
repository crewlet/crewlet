package authapi_test

import (
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/iam/credential"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/iamdomain"
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
// rewrite it at the current cost, keeping the credential's id.
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
				current := params(cheap)
				switch {
				case stale && !strings.Contains(pw.Verifier, current):
					t.Errorf("after the sign-in the verifier is %q, want it "+
						"at the current cost %s", pw.Verifier, current)
				case stale && rehashes != 1:
					t.Errorf("asked for %d re-hash writes, want 1", rehashes)
				case !stale && rehashes != 0:
					t.Errorf("a verifier already at the current cost was "+
						"rewritten %d times", rehashes)
				}
				if ok, _ := credential.NewHasher(cheap, 1).Verify(pw.Verifier,
					password); !ok {
					t.Error("the stored verifier no longer verifies the password")
				}
			})
		}
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
