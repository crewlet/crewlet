package config_test

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
)

// signInCompany is a company document setting one sign-in value.
func signInCompany(t *testing.T, key, value string) (*config.Company, error) {
	t.Helper()
	return config.ParseCompany([]byte("name: Acme\nsign_in:\n  " + key + ": " + value + "\n"))
}

// THE SIGN-IN POLICY IS THE COMPANY'S, defaults where it says nothing, and each
// value is refused outside the engine's range at its path, with the range.
// Mutation: drop a sign-in row from settingRanges and its refusal validates.
func TestTheSignInPolicyIsTheCompanysAndHeldToTheEnginesRange(t *testing.T) {
	t.Parallel()
	unset, err := config.ParseCompany([]byte("name: Acme\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := config.SignInPolicy{
		SecondFactor: iam.SecondFactorRequired, MinPasswordLength: iam.MinPasswordChars,
		SessionLifetime: 7 * 24 * time.Hour, IdleTimeout: 12 * time.Hour, StepUp: time.Hour,
	}
	if got := unset.SignIn.Policy(); got != want {
		t.Errorf("a company saying nothing signs people in under %+v, want %+v", got, want)
	}
	for _, tc := range []struct {
		key           string
		lowest, most  float64
		read          func(config.SignInPolicy) float64
		inside, words string
	}{
		{"min_password_length", 12, 256, func(p config.SignInPolicy) float64 { return float64(p.MinPasswordLength) }, "16", "between 12 and 256"},
		{"session_lifetime_seconds", 3600, 2592000, func(p config.SignInPolicy) float64 { return p.SessionLifetime.Seconds() }, "86400", "between 3600 and 2592000"},
		{"idle_timeout_seconds", 900, 2592000, func(p config.SignInPolicy) float64 { return p.IdleTimeout.Seconds() }, "1800", "between 900 and 2592000"},
		{"step_up_seconds", 300, 86400, func(p config.SignInPolicy) float64 { return p.StepUp.Seconds() }, "900", "between 300 and 86400"},
	} {
		c, err := signInCompany(t, tc.key, tc.inside)
		if err != nil {
			t.Errorf("sign_in.%s: %s was refused: %v", tc.key, tc.inside, err)
		} else if got := tc.read(c.SignIn.Policy()); strconv.FormatFloat(got, 'f', -1, 64) != tc.inside {
			t.Errorf("sign_in.%s: %s reads as %v", tc.key, tc.inside, got)
		}
		for _, v := range []float64{tc.lowest - 1, tc.most + 1} {
			_, err := signInCompany(t, tc.key, strconv.FormatFloat(v, 'f', -1, 64))
			problems := config.Problems(err)
			if len(problems) != 1 || problems[0].Path != "sign_in."+tc.key ||
				problems[0].Range == nil || !strings.Contains(problems[0].Message, tc.words) {
				t.Errorf("sign_in.%s: %v answered %+v, want one refusal at its path naming %s",
					tc.key, v, problems, tc.words)
			}
		}
	}
	optional, err := signInCompany(t, "require_second_factor", "false")
	if err != nil || optional.SignIn.Policy().SecondFactor != iam.SecondFactorOptional {
		t.Errorf("require_second_factor: false reads as %v (%v)", optional.SignIn.Policy().SecondFactor, err)
	}
}

// A NODE HOLDS THE COMPANY WITHIN ITS OPERATOR'S LIMITS, and an unset limit
// holds nothing back.
// Mutation: return the company's policy unheld and the strict row fails.
func TestANodeHoldsTheCompanysSignInPolicyWithinItsLimits(t *testing.T) {
	t.Parallel()
	loose, err := config.ParseCompany([]byte("name: Acme\nsign_in:\n" +
		"  require_second_factor: false\n  min_password_length: 12\n" +
		"  session_lifetime_seconds: 2592000\n  idle_timeout_seconds: 2592000\n" +
		"  step_up_seconds: 86400\n"))
	if err != nil {
		t.Fatal(err)
	}
	unlimited := config.APIAuth{Limits: config.SignInLimits{SecondFactor: iam.SecondFactorOptional}}
	if got, want := unlimited.SignInPolicy(loose), loose.SignIn.Policy(); got != want {
		t.Errorf("a node limiting nothing enforces %+v, want the company's %+v", got, want)
	}
	strict := config.APIAuth{Limits: config.SignInLimits{
		MinPasswordLength: 20, MaxSessionLifetimeRaw: "24h", MaxIdleTimeoutRaw: "1h",
		MaxStepUpRaw: "10m",
	}}
	want := config.SignInPolicy{
		SecondFactor: iam.SecondFactorRequired, MinPasswordLength: 20,
		SessionLifetime: 24 * time.Hour, IdleTimeout: time.Hour, StepUp: 10 * time.Minute,
	}
	if got := strict.SignInPolicy(loose); got != want {
		t.Errorf("a strict node enforces %+v, want %+v", got, want)
	}
	if got := strict.SignInPolicy(nil); got.SessionLifetime != 24*time.Hour ||
		got.SecondFactor != iam.SecondFactorRequired {
		t.Errorf("a strict node with no company enforces %+v, want the defaults held", got)
	}
}

// A WRITE PAST THIS NODE'S LIMIT IS REFUSED AT ITS PATH, and a value the write
// did not change is left alone.
// Mutation: drop the changed check and the unrelated write is refused.
func TestAWritePastThisNodesSignInLimitIsRefused(t *testing.T) {
	t.Parallel()
	limits := config.SignInLimits{MaxSessionLifetimeRaw: "168h", MinPasswordLength: 14}
	prior, _ := config.ParseCompany([]byte("name: Acme\n"))
	for _, tc := range []struct {
		key, value, path, said string
	}{
		{"session_lifetime_seconds", "1209600", config.SignInSessionLifetimeKey, "at most 7 days"},
		{"min_password_length", "12", config.SignInPasswordKey, "at least 14 characters"},
		{"require_second_factor", "false", config.SignInSecondFactorKey, "requires two-step verification"},
	} {
		next, err := signInCompany(t, tc.key, tc.value)
		if err != nil {
			t.Fatal(err)
		}
		problems := config.Problems(limits.Admit(next, prior))
		if len(problems) != 1 || problems[0].Path != tc.path ||
			!strings.Contains(problems[0].Said, tc.said) {
			t.Errorf("%s: %s past the limit answered %+v, want one refusal at %s saying %q",
				tc.key, tc.value, problems, tc.path, tc.said)
		}
	}
	next, _ := signInCompany(t, "session_lifetime_seconds", "1209600")
	if r := config.Problems(limits.Admit(next, prior))[0].Range; r == nil || r.Max == nil ||
		*r.Max != 604800 || !r.OperatorMax {
		t.Errorf("the refusal's range is %+v, want at most 604800 marked as the operator's", r)
	}
	if err := limits.Admit(next, next); err != nil {
		t.Errorf("a write leaving a value past the limit as it was is refused: %v", err)
	}
	inside, _ := signInCompany(t, "session_lifetime_seconds", "86400")
	if err := limits.Admit(inside, prior); err != nil {
		t.Errorf("a lifetime inside the limit is refused: %v", err)
	}
}

// A WEAKENING SIGN-IN CHANGE WARNS, saying what it allows; a strengthening one
// warns of nothing.
// Mutation: compare the wrong direction and both halves flip.
func TestAWeakeningSignInChangeWarnsAndAStrengtheningOneDoesNot(t *testing.T) {
	t.Parallel()
	prior, _ := config.ParseCompany([]byte("name: Acme\n"))
	for _, tc := range []struct{ key, value, says string }{
		{"require_second_factor", "false", "password alone"},
		{"session_lifetime_seconds", "1209600", "14 days instead of 7 days"},
		{"idle_timeout_seconds", "86400", "1 day instead of 12 hours"},
		{"step_up_seconds", "7200", "2 hours instead of 1 hour"},
	} {
		next, _ := signInCompany(t, tc.key, tc.value)
		warnings := config.SignInWeakens(next, prior)
		if len(warnings) != 1 || warnings[0].Path != "sign_in."+tc.key ||
			!strings.Contains(warnings[0].Message, tc.says) {
			t.Errorf("weakening %s answered %+v, want one warning saying %q", tc.key, warnings, tc.says)
		}
		if back := config.SignInWeakens(prior, next); len(back) != 0 {
			t.Errorf("strengthening %s back warned: %+v", tc.key, back)
		}
	}
	shorter, _ := signInCompany(t, "min_password_length", "16")
	if w := config.SignInWeakens(prior, shorter); len(w) != 1 || !strings.Contains(w[0].Message, "12 characters instead of 16") {
		t.Errorf("lowering the password floor answered %+v", w)
	}
}
