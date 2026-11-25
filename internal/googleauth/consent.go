package googleauth

import (
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
)

// Google's consent endpoints (https://developers.google.com/identity/protocols/oauth2/web-server).
// Fixed rather than configurable, so a code, a refresh token and the
// company's client secret only ever go to Google.
const (
	ConsentIssuer       = "https://accounts.google.com"
	ConsentAuthorizeURL = "https://accounts.google.com/o/oauth2/v2/auth"
	ConsentTokenURL     = "https://oauth2.googleapis.com/token"
	ConsentRevokeURL    = "https://oauth2.googleapis.com/revoke"
)

// ConsentParams are the authorization request's own parameters: a refresh
// token (`access_type=offline`), asked for on every sign-in (`prompt=consent`,
// so a second sign-in is issued one too), and nothing granted earlier added
// to it (`include_granted_scopes=false`), so the grant is the scopes asked.
func ConsentParams() url.Values {
	return url.Values{"access_type": {"offline"}, "prompt": {"consent"},
		"include_granted_scopes": {"false"}}
}

// Consent is one Google product that refuses service accounts, so the company
// reaches it through a person's consent grant, sealed and renewed by the
// engine (ADR-0178); or one whose seats sign in as their own identities
// (Play, Analytics), where the grant is an admin's that only adds each seat's
// identity at the product and signs no seat's call.
type Consent struct {
	// Kind is the product's integration kind, and its grant's name.
	Kind integration.Kind

	// Name is what a person calls it ("AdSense").
	Name string

	// Scopes is the set of scopes a sign-in asks for under c, each in full:
	// what the seats' levels need now (reading alone, or changes too). The
	// grant is kept only as exactly that set, so Google's granular consent
	// can never grant part of it.
	Scopes func(c *config.Company) []string

	// Client is the company's own OAuth client as c configures it (the
	// block's `oauth.client_id` and `oauth.client_secret`), false where c
	// does not turn the product on.
	Client func(c *config.Company) (ConsentClient, bool)
}

// ConsentClient is the company's own OAuth client, from a Google Cloud
// project it controls. Secret is the configured value, normally a `${VAR}`,
// resolved at each use so a rotated secret is the one sent.
type ConsentClient struct {
	ID, Secret string
}

// consents is every registered consent product, appended to from each
// product package's init and read after.
//
//nolint:gochecknoglobals // a registry its products fill at init
var consents struct {
	sync.Mutex
	products []Consent
}

// RegisterConsent adds a consent product. It panics on one with no kind,
// name, scope or client, or one registered twice: a programming error the
// build must not ship.
func RegisterConsent(p Consent) {
	consents.Lock()
	defer consents.Unlock()
	switch {
	case p.Kind == "" || p.Name == "" || p.Scopes == nil || p.Client == nil:
		panic(fmt.Sprintf("googleauth: consent product %q needs its kind, name, scopes and client", p.Kind))
	case slices.ContainsFunc(consents.products, func(q Consent) bool { return q.Kind == p.Kind }):
		panic(fmt.Sprintf("googleauth: consent product %q registered twice", p.Kind))
	}
	consents.products = append(consents.products, p)
}

// ConsentOf is the registered consent product of kind.
func ConsentOf(kind integration.Kind) (Consent, bool) {
	consents.Lock()
	defer consents.Unlock()
	for _, p := range consents.products {
		if p.Kind == kind {
			return p, true
		}
	}
	return Consent{}, false
}

// ErrScopeNotGranted is a sign-in whose grant is not exactly the scopes asked
// for: a person unticked one on Google's granular consent screen, or Google
// added another.
var ErrScopeNotGranted = errors.New("the sign-in did not grant exactly the scopes asked for; " +
	"sign in again and leave every access ticked")

// ConsentScopes is the scopes p's sign-in asks for under c, refused where it
// names none or a scope that is no single word.
func ConsentScopes(p Consent, c *config.Company) ([]string, error) {
	scopes := p.Scopes(c)
	if len(scopes) == 0 || slices.ContainsFunc(scopes, func(s string) bool {
		return s == "" || strings.ContainsAny(s, " \t")
	}) {
		return nil, fmt.Errorf("googleauth: %s names no scopes to sign in for", p.Name)
	}
	return slices.Clone(scopes), nil
}

// CheckConsentScope refuses a token answer whose granted scopes are not
// exactly asked, in any order. Silence is refused too: Google always names
// what it granted, and granular consent means one cannot assume the request
// was granted.
func CheckConsentScope(asked, granted []string) error {
	a, g := slices.Sorted(slices.Values(asked)), slices.Sorted(slices.Values(granted))
	if len(a) == 0 || !slices.Equal(slices.Compact(a), slices.Compact(g)) {
		return ErrScopeNotGranted
	}
	return nil
}

// ProjectNumber is the Cloud project number an OAuth client ID names
// (`<number>-<id>.apps.googleusercontent.com`), false for an ID of another
// shape. Revoking one token at Google revokes every grant the person made to
// any client of that project, so the engine compares projects before it does.
func ProjectNumber(clientID string) (string, bool) {
	rest, ok := strings.CutSuffix(clientID, ".apps.googleusercontent.com")
	if !ok {
		return "", false
	}
	number, id, ok := strings.Cut(rest, "-")
	if !ok || number == "" || id == "" || strings.ContainsFunc(number, func(r rune) bool { return r < '0' || r > '9' }) {
		return "", false
	}
	return number, true
}

// The window in which a refused renewal is read as an app left in Testing:
// Google expires the refresh tokens of an external app in Testing after seven
// days (https://developers.google.com/identity/protocols/oauth2), so a refusal
// half a day either side of that is most likely it.
const (
	testingFrom = 156 * time.Hour // 6.5 days
	testingTo   = 180 * time.Hour // 7.5 days
)

// TestingModeHint is what a person reads beside a renewal refused for good,
// when it came within [testingFrom] and [testingTo] of the sign-in.
const TestingModeHint = "This often means the Google OAuth app is in Testing. Publish it, then sign in again."

// LooksLikeTestingMode reports whether a renewal refused at failed, for a
// grant signed in at signedIn, fits an app in Testing.
func LooksLikeTestingMode(signedIn, failed time.Time) bool {
	age := failed.Sub(signedIn)
	return age >= testingFrom && age <= testingTo
}

// SignInClient is the OAuth client a consent product signs in as under c: own,
// its block's oauth, else the company's sign_in_client; nothing set is a
// client with no ID, which the sign-in refuses by name.
func SignInClient(c *config.Company, own *config.GoogleOAuthClient) ConsentClient {
	var cl *config.GoogleOAuthClient
	if c != nil {
		cl = c.Integrations.GoogleSignInClient(own)
	}
	if cl == nil {
		return ConsentClient{}
	}
	return ConsentClient{ID: cl.ClientID, Secret: cl.ClientSecret}
}
