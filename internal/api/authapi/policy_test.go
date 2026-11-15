package authapi_test

import (
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/authapi"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
)

// EACH SIGN-IN READS THE COMPANY'S POLICY AS IT STANDS THEN, so a change an
// administrator saves applies to the next sign-in with no restart (ADR-0152):
// a second factor required since the last sign-in restricts the next one, and
// the session it opens lives the lifetime in force.
//
// Mutation: read the policy once when the surface is built and the second
// sign-in is whole and lives a week.
func TestASignInReadsTheCompanysPolicyAsItStandsThen(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	policy := config.DefaultSignInPolicy()
	policy.SecondFactor = iam.SecondFactorOptional
	r := newSignInRigWith(t, func(o *authapi.Options) {
		o.SignIn = func() config.SignInPolicy {
			mu.Lock()
			defer mu.Unlock()
			return policy
		}
	})
	passwordOnly(r.estate)

	r.signIn(t, "jane.doe", password, "")
	mu.Lock()
	policy.SecondFactor, policy.SessionLifetime = iam.SecondFactorRequired, 2*time.Hour
	mu.Unlock()
	r.signIn(t, "jane.doe", password, "")

	starts := r.estate.starts
	if len(starts) != 2 {
		t.Fatalf("opened %d sessions, want 2", len(starts))
	}
	if starts[0].EnrolmentOnly || !starts[1].EnrolmentOnly {
		t.Errorf("the sessions opened enrolment-only %v then %v, want whole and then "+
			"restricted once the company required a second factor",
			starts[0].EnrolmentOnly, starts[1].EnrolmentOnly)
	}
	if got := starts[1].AbsoluteExpiresAt; !got.Equal(clock.Add(2 * time.Hour)) {
		t.Errorf("the second session ends at %s, want the two-hour lifetime now in force (%s)",
			got, clock.Add(2*time.Hour))
	}
}
