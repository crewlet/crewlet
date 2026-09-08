package github_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/crewlet/crewlet/internal/github"
)

// testKey is a real RSA key, because the JWT this signs is verified by
// GitHub with a real signature check and a stub would prove nothing about
// the one thing that can be wrong here.
func testKey(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	block := &pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}
	return key, string(pem.EncodeToMemory(block))
}

// AN APP NAME IS GLOBALLY UNIQUE ACROSS GITHUB AND CAPPED AT 34 CHARACTERS.
//
// A name built from the seat alone collides the second time two companies
// both have an `sre-lead`, and the failure arrives as a manifest rejection an
// operator can do nothing about. The company leads so the collision is with
// another company of the same name rather than with every other customer.
func TestAnAppNameFitsGitHubsGlobalNamespace(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct{ company, seat, want string }{
		"short enough":   {"Acme", "sre-lead", "Acme sre-lead"},
		"no company":     {"", "sre-lead", "sre-lead"},
		"nothing at all": {"", "", "Crewlet agent"},
	} {
		if got := github.AppName(tc.company, tc.seat); got != tc.want {
			t.Errorf("%s: AppName = %q, want %q", name, got, tc.want)
		}
	}
	long := github.AppName(strings.Repeat("company", 6), "sre-lead")
	if len([]rune(long)) > 34 {
		t.Errorf("a long name is %d runes, past GitHub's 34", len([]rune(long)))
	}
	// CUT ON A RUNE BOUNDARY. A name sliced through a multi-byte character
	// is refused by GitHub as malformed rather than as too long, which
	// names nothing an operator can act on.
	wide := github.AppName(strings.Repeat("π", 40), "seat")
	if !json.Valid([]byte(`"` + wide + `"`)) {
		t.Errorf("a cut name is not valid text: %q", wide)
	}
}

// AN APP WITH NO EVENTS RECEIVES NOTHING AND REPORTS ITSELF HEALTHY.
//
// default_events is not a refinement of the hook URL: without it GitHub
// subscribes the app to nothing at all, so the manifest must carry the set
// this engine's router acts on.
func TestAManifestSubscribesToTheEventsTheRouterActsOn(t *testing.T) {
	t.Parallel()
	m := github.BuildManifest(github.ManifestOptions{
		Seat: "sre-lead", Name: "Acme sre-lead",
		DeliveryURL: "https://engine.example.com/webhooks/github/sre-lead",
		RedirectURL: "https://engine.example.com/webhooks/github/app-callback",
		Tier:        github.TierReview,
	})
	if len(m.DefaultEvents) == 0 {
		t.Fatal("the manifest subscribes to nothing")
	}
	for _, want := range []string{"pull_request", "issue_comment"} {
		if !strings.Contains(strings.Join(m.DefaultEvents, ","), want) {
			t.Errorf("the manifest does not subscribe to %q", want)
		}
	}
	// PUSHES ARE EXCLUDED. The highest-volume event a busy repository
	// produces, and the router drops every one.
	if strings.Contains(strings.Join(m.DefaultEvents, ","), "push") {
		t.Error("the manifest subscribes to pushes the router discards")
	}
	// THE TIER DECIDES THE PERMISSIONS, at creation, because an app cannot
	// be widened later without every operator re-approving it.
	if m.DefaultPerms["contents"] != "read" || m.DefaultPerms["pull_requests"] != "write" {
		t.Errorf("a review app was created with %v", m.DefaultPerms)
	}
	if m.Public {
		t.Error("a per-operator app was created public")
	}
}

// AN APP WITH NOWHERE TO DELIVER IS CREATED SWITCHED OFF, not pointed at a
// placeholder: an inactive hook is a state the operator can see, and a wrong
// URL is one that looks healthy at GitHub and arrives nowhere.
func TestAnAppWithNoAddressIsCreatedWithDeliveryOff(t *testing.T) {
	t.Parallel()
	off := github.BuildManifest(github.ManifestOptions{Seat: "s", Tier: github.TierReadOnly})
	if active, _ := off.HookAttributes["active"].(bool); active {
		t.Error("an app with no delivery address was created active")
	}
	if _, has := off.HookAttributes["url"]; has {
		t.Error("an app with no delivery address was given one anyway")
	}
	on := github.BuildManifest(github.ManifestOptions{
		Seat: "s", Tier: github.TierReadOnly, DeliveryURL: "https://e.example.com/w",
	})
	if active, _ := on.HookAttributes["active"].(bool); !active {
		t.Error("an app with an address was created inactive")
	}
}

// AN ORGANIZATION'S APP MUST BE REGISTERED ON THE ORGANIZATION.
//
// An app registered under a person's own account cannot be installed on the
// organization that owns the repositories, so the wrong action URL produces
// an app that exists, belongs to the wrong account, and is useless.
func TestTheManifestIsPostedToTheAccountThatWillOwnTheApp(t *testing.T) {
	t.Parallel()
	if got := github.ActionURL("", "acme"); got != "https://github.com/organizations/acme/settings/apps/new" {
		t.Errorf("an organization posts to %q", got)
	}
	if got := github.ActionURL("", ""); got != "https://github.com/settings/apps/new" {
		t.Errorf("a personal account posts to %q", got)
	}
	// Enterprise Server has its own host, and the app is created there.
	if got := github.ActionURL("https://ghe.example.com", "acme"); !strings.HasPrefix(got, "https://ghe.example.com/") {
		t.Errorf("an Enterprise host posts to %q", got)
	}
}

// THE CODE IS THE AUTHENTICATION, and it is single use. A spent or expired
// one is the failure a retry cannot fix, so it is told apart from a transient
// one: the operator has to create the app again.
func TestASpentManifestCodeIsToldApartFromAFailure(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnprocessableEntity)
	}))
	defer srv.Close()
	if _, err := github.ExchangeManifest(context.Background(), srv.URL, "code"); err == nil {
		t.Fatal("a spent code was accepted")
	} else if !strings.Contains(err.Error(), "used or has expired") {
		t.Errorf("a spent code reported as %v", err)
	}
}

// THE CONVERSION IS THE ONLY TIME GITHUB HANDS OVER THE KEY, so an answer
// missing it is refused loudly rather than stored as a half-built app that
// can never authenticate and can never be repaired.
func TestAConversionWithoutAKeyIsRefused(t *testing.T) {
	t.Parallel()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":42,"slug":"acme-sre-lead"}`))
	}))
	defer srv.Close()
	if _, err := github.ExchangeManifest(context.Background(), srv.URL, "code"); err == nil {
		t.Fatal("an app with no private key was accepted")
	}
}

// THE ASSERTION IS SIGNED AS THE APP, backdated, and inside GitHub's ten
// minute ceiling. A machine a few seconds fast otherwise has every call
// refused for a clock, with a message naming nothing an operator can act on.
func TestTheAppAssertionIsBackdatedAndShortLived(t *testing.T) {
	t.Parallel()
	key, keyPEM := testKey(t)
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	signed, err := github.AppJWT(99, keyPEM, now)
	if err != nil {
		t.Fatal(err)
	}
	claims := jwt.RegisteredClaims{}
	// Verified against the SAME instant it was signed at, because this
	// asserts what GitHub's clock would see, not what this machine's does.
	if _, err := jwt.ParseWithClaims(signed, &claims, func(*jwt.Token) (any, error) {
		return &key.PublicKey, nil
	}, jwt.WithTimeFunc(func() time.Time { return now })); err != nil {
		t.Fatalf("GitHub would refuse this assertion: %v", err)
	}
	if claims.Issuer != "99" {
		t.Errorf("the assertion is issued by %q, not the app", claims.Issuer)
	}
	if !claims.IssuedAt.Before(now) {
		t.Error("the assertion is not backdated, so a fast clock refuses it")
	}
	if life := claims.ExpiresAt.Sub(claims.IssuedAt.Time); life > 10*time.Minute {
		t.Errorf("the assertion lives %s, past GitHub's ceiling", life)
	}
}

// A TOKEN IS SCOPED DOWN AT MINT TIME, which is where the tier becomes real.
// The app may hold more than the tier asks for, because a person installed it
// and a person can widen it, so a read-only seat must not be able to write
// even on an installation that could.
func TestATokenCarriesOnlyWhatTheTierAsksFor(t *testing.T) {
	t.Parallel()
	_, keyPEM := testKey(t)
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Error("the mint call did not authenticate as the app")
		}
		_, _ = w.Write([]byte(`{"token":"ghs_x","expires_at":"2026-09-07T13:00:00Z"}`))
	}))
	defer srv.Close()

	client, err := github.NewAppClient(7, keyPEM, srv.URL, func() time.Time {
		return time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	})
	if err != nil {
		t.Fatal(err)
	}
	token, err := client.MintToken(context.Background(), 11, github.TierReadOnly, []string{"acme/platform"})
	if err != nil {
		t.Fatal(err)
	}
	perms, _ := got["permissions"].(map[string]any)
	if perms["contents"] != "read" {
		t.Errorf("a read-only token asked for contents=%v", perms["contents"])
	}
	if perms["pull_requests"] != "read" {
		t.Errorf("a read-only token asked to write pull requests: %v", perms["pull_requests"])
	}
	// THE BARE NAME, because the endpoint scopes within the installation's
	// own account and refuses a qualified one.
	repos, _ := got["repositories"].([]any)
	if len(repos) != 1 || repos[0] != "platform" {
		t.Errorf("repositories = %v, want the bare name", repos)
	}
	if token.Token != "ghs_x" {
		t.Errorf("token = %q", token.Token)
	}
}

// A TOKEN IS REPLACED BEFORE IT EXPIRES, not after. A pass that discovers
// expiry at the call site has already failed the work it was doing.
func TestATokenIsRenewedBeforeItExpires(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	fresh := github.InstallationToken{Token: "t", ExpiresAt: now.Add(time.Hour)}
	if !fresh.Fresh(now) {
		t.Error("an hour of life is not fresh")
	}
	nearly := github.InstallationToken{Token: "t", ExpiresAt: now.Add(github.RenewBefore / 2)}
	if nearly.Fresh(now) {
		t.Error("a token inside the renewal window reports fresh")
	}
	if (github.InstallationToken{ExpiresAt: now.Add(time.Hour)}).Fresh(now) {
		t.Error("a token with no value reports fresh")
	}
}

// A PRIVATE APP CANNOT BE INSTALLED FROM github.com/apps/{slug}.
//
// That public route exists only for PUBLIC apps, and every app this engine
// creates is private: the manifest sets `public: false`, because an agent's
// identity is that company's business and a public app is listed for anyone
// to install. Sending an operator there gave them a 404 on the one click the
// whole flow depends on.
func TestTheInstallLinkGoesToTheAppsOwnSettings(t *testing.T) {
	t.Parallel()
	got := github.InstallURL("", "acme", "acme-sre-lead")
	want := "https://github.com/organizations/acme/settings/apps/" +
		"acme-sre-lead/installations"
	if got != want {
		t.Errorf("InstallURL = %q, want %q", got, want)
	}
	// THE PUBLIC ROUTE IS THE BUG, so it must not come back.
	if strings.Contains(got, "github.com/apps/") {
		t.Errorf("the install link uses the public route, which 404s for a private app: %q", got)
	}
	// A personal account manages its apps under its own settings.
	if got := github.InstallURL("", "", "some-app"); got != "https://github.com/settings/apps/some-app/installations" {
		t.Errorf("a user-owned app installs at %q", got)
	}
	// No slug means no link, because a broken one costs the trip to find out.
	if got := github.InstallURL("", "acme", ""); got != "" {
		t.Errorf("an app with no slug was given a link: %q", got)
	}
}
