package googleauth_test

import (
	"errors"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googleauth"
)

// A CLIENT'S PROJECT IS ITS ID'S NUMERIC PREFIX, and an ID of any other shape
// names none: two clients are in one project only when both say so, since
// revoking at Google ends every grant the person made to that project.
//
// Mutation: accept a prefix holding a letter, or drop the suffix check, and a
// case goes red.
func TestAClientsProjectIsItsIDsNumericPrefix(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		id, want string
		ok       bool
	}{
		{"123456789012-abc123def456.apps.googleusercontent.com", "123456789012", true},
		{"42-x.apps.googleusercontent.com", "42", true},
		{"12a4-abc.apps.googleusercontent.com", "", false},
		{"-abc.apps.googleusercontent.com", "", false},
		{"123456789012-.apps.googleusercontent.com", "", false},
		{"123456789012-abc.example.com", "", false},
		{"123456789012", "", false},
		{"", "", false},
	} {
		got, ok := googleauth.ProjectNumber(tc.id)
		if got != tc.want || ok != tc.ok {
			t.Errorf("ProjectNumber(%q) = %q, %t; want %q, %t", tc.id, got, ok, tc.want, tc.ok)
		}
	}
}

// A GRANT IS KEPT ONLY AS EXACTLY THE ONE SCOPE ASKED: a scope unticked on
// Google's granular consent, one added, or silence is refused.
//
// Mutation: accept a granted list that merely contains the scope, or read
// silence as granted, and a case goes red.
func TestAGrantIsKeptOnlyAsExactlyTheOneScopeAsked(t *testing.T) {
	t.Parallel()
	const asked = "https://www.googleapis.com/auth/adsense.readonly"
	if err := googleauth.CheckConsentScope(asked, []string{asked}); err != nil {
		t.Fatalf("the scope asked was refused: %v", err)
	}
	for _, granted := range [][]string{nil, {}, {"https://www.googleapis.com/auth/admob.readonly"},
		{asked, "openid"}, {"adsense.readonly"}} {
		if err := googleauth.CheckConsentScope(asked, granted); !errors.Is(err, googleauth.ErrScopeNotGranted) {
			t.Errorf("granted %q was kept (%v)", granted, err)
		}
	}
}

// A REFUSAL ABOUT A WEEK AFTER THE SIGN-IN READS AS AN APP IN TESTING, half a
// day either side of the seven days Google gives one, and no other.
//
// Mutation: widen or narrow either end of the window, and a case goes red.
func TestARefusalAboutAWeekAfterTheSignInReadsAsAnAppInTesting(t *testing.T) {
	t.Parallel()
	signedIn := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		after time.Duration
		want  bool
	}{
		{156*time.Hour - time.Second, false},
		{156 * time.Hour, true},
		{168 * time.Hour, true},
		{180 * time.Hour, true},
		{180*time.Hour + time.Second, false},
		{time.Hour, false},
	} {
		if got := googleauth.LooksLikeTestingMode(signedIn, signedIn.Add(tc.after)); got != tc.want {
			t.Errorf("a refusal %s after the sign-in reads as Testing: %t, want %t", tc.after, got, tc.want)
		}
	}
}

// THE AUTHORIZATION REQUEST ASKS FOR A REFRESH TOKEN EVERY TIME and nothing
// granted earlier, so each sign-in is a grant of the one scope alone.
func TestTheAuthorizationRequestAsksForARefreshTokenEveryTime(t *testing.T) {
	t.Parallel()
	q := googleauth.ConsentParams()
	for name, want := range map[string]string{"access_type": "offline", "prompt": "consent",
		"include_granted_scopes": "false"} {
		if q.Get(name) != want {
			t.Errorf("%s = %q, want %q", name, q.Get(name), want)
		}
	}
}

// A CONSENT PRODUCT NAMES ONE SCOPE: one registered with two, or with none, is
// a programming error the build refuses.
func TestAConsentProductNamesOneScope(t *testing.T) {
	t.Parallel()
	client := func(*config.Company) (googleauth.ConsentClient, bool) { return googleauth.ConsentClient{}, false }
	for _, p := range []googleauth.Consent{
		{Kind: "consent_two_scopes", Name: "Two", Scope: "a b", Client: client},
		{Kind: "consent_no_scope", Name: "None", Client: client},
		{Kind: "consent_no_client", Name: "Client", Scope: "a"},
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Errorf("%s was registered", p.Kind)
				}
			}()
			googleauth.RegisterConsent(p)
		}()
	}
}
