package googleplay_test

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/crewlet/crewlet/internal/growth"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
)

// How Google answers the request a case fails. A held answer ends only once
// the act has: the bubble's clock reaches the apply bound first, and a handler
// returning at the bound's end would race the caller's context with an answer.
const (
	unavailable = iota + 1 // Google fails to serve it
	outlasted              // nothing comes back
	taken                  // Google acts and answers, the answer never ending
)

// A PLAY ACT IS NOT SENT UNTIL ITS CHANGE LEAVES: an edit changes nothing
// until its commit is sent, so a staged step or validation Google fails, a
// staged step or an edit's opening the apply bound cuts, and a context ended
// before the commit's request was made each answer the act not sent, commit
// nothing, and record what cut them, never an unknown outcome; so does one
// request whose context ended before it left. A commit Google took whose
// answer was cut off is never told as not sent: what it did is unknown.
//
// Mutation: drop a not-sent mark in Apply or commitEdit, classify before
// marking in failEarly, or record a step the bound cut as Google's failing.
func TestAPlayActIsNotSentUntilItsChangeLeaves(t *testing.T) {
	t.Parallel()
	halt := map[string]any{"app": "com.acme.app", "track": "production"}
	listing := map[string]any{"app": "com.acme.app", "language": "en-US", "title": "Acme Pro", "reason": "rename"}
	draft := map[string]any{"operation": "androidpublisher.monetization.subscriptions.patch",
		"path":  map[string]any{"packageName": "com.acme.app", "productId": "pro"},
		"query": map[string]any{"updateMask": "listings", "regionsVersion.version": "2025/03"},
		"body":  map[string]any{"listings": []any{}}}
	for _, tc := range []struct {
		name, tool string
		args       map[string]any
		// at is the request made under Apply that Google answers as answer:
		// "METHOD path-suffix".
		at     string
		answer int
		// endAfter ends the apply's context as the token for the request after
		// the one it names is minted, just before that request is made.
		endAfter string
		// change is the request that makes the change, "METHOD path part",
		// and reached how many of it Google took.
		change  string
		reached int
		notSent bool
		failure growth.Failure
	}{
		{name: "a staged step Google fails", tool: "google_play_halt_release", args: halt,
			at: "PATCH /tracks/production", answer: unavailable,
			change: "POST :commit", notSent: true, failure: growth.FailureVendorUnavailable},
		{name: "a staged step the apply bound cuts", tool: "google_play_halt_release", args: halt,
			at: "PATCH /tracks/production", answer: outlasted,
			change: "POST :commit", notSent: true, failure: growth.FailureTimedOut},
		{name: "a validation Google fails", tool: "google_play_halt_release", args: halt,
			at: "POST :validate", answer: unavailable,
			change: "POST :commit", notSent: true, failure: growth.FailureVendorUnavailable},
		{name: "a context ended before the commit's request", tool: "google_play_halt_release", args: halt,
			endAfter: "POST :validate",
			change:   "POST :commit", notSent: true, failure: growth.FailureEngineError},
		{name: "an approved edit whose opening's answer was cut off", tool: "google_play_update_listing",
			args: listing, at: "POST /edits", answer: taken,
			change: "POST :commit", notSent: true, failure: growth.FailureTimedOut},
		{name: "one request whose context ended before it left", tool: "google_play_operate", args: draft,
			endAfter: "GET /subscriptions/pro",
			change:   "PATCH /subscriptions/pro", notSent: true, failure: growth.FailureEngineError},
		{name: "a commit Google took whose answer was cut off", tool: "google_play_halt_release", args: halt,
			at: "POST :commit", answer: taken,
			change: "POST :commit", reached: 1, failure: growth.FailureUnknownOutcome},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				// ARMED ONLY UNDER APPLY: the ask's own reads are made whole.
				var armed, ending atomic.Bool
				released := make(chan struct{})
				f := newFakePlay()
				hc := httpxtest.InProcess(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch {
					case armed.Load() && named(tc.at, r) && tc.answer == unavailable:
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusServiceUnavailable)
						_, _ = w.Write([]byte(`{"error": {"code": 503, "message": "Backend error."}}`))
					case armed.Load() && named(tc.at, r):
						if tc.answer == taken {
							f.serve(w, r)
						}
						<-released
					default:
						if armed.Load() && named(tc.endAfter, r) {
							ending.Store(true)
						}
						f.serve(w, r)
					}
				}))
				w := newWorldOn(t, "operate", f, hc)
				ctx, end := context.WithCancel(t.Context())
				defer end()
				w.tokens.minting = func() {
					if ending.Load() {
						end()
					}
				}
				_, err := w.answerOn(ctx, t, tc.tool, tc.args, true, func() { armed.Store(true) })
				close(released)
				if err == nil || errors.Is(err, growth.ErrNotSent) != tc.notSent {
					t.Errorf("the act answered %v, want an error not sent: %v", err, tc.notSent)
				}
				method, part, _ := strings.Cut(tc.change, " ")
				if got := len(w.sent(method, part)); got != tc.reached {
					t.Errorf("Google took %d of %s, want %d", got, tc.change, tc.reached)
				}
				w.audit.mu.Lock()
				defer w.audit.mu.Unlock()
				step, last := w.audit.steps[len(w.audit.steps)-1], w.audit.records[len(w.audit.records)-1]
				if step != growth.StepFailed || last.Failure != tc.failure {
					t.Errorf("the access log ends %s with %q, want failed with %q", step, last.Failure, tc.failure)
				}
			})
		})
	}
}

// named reports whether r is the request at names, "METHOD path-suffix"; ""
// names none.
func named(at string, r *http.Request) bool {
	method, suffix, ok := strings.Cut(at, " ")
	return ok && r.Method == method && strings.HasSuffix(r.URL.Path, suffix)
}
