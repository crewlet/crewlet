package github

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/crewlet/crewlet/internal/httpx"
)

// Creating one GitHub App per agent, from a manifest a person confirms.
//
// # Why a manifest at all
//
// A published app on the marketplace would let an operator install one thing
// and be done. This engine has none, and a per-operator app is the honest
// alternative: the engine writes a manifest describing the app it wants, the
// operator's browser POSTs it to GitHub, and GitHub hands back an app that
// belongs to that operator's organization. Nobody types a credential.
//
// # Why the browser, and not the engine
//
// The manifest is submitted as a FORM POST carrying the operator's own GitHub
// session cookie. There is no server-to-server equivalent: the whole point is
// that a person, signed in as somebody who may create apps on that
// organization, confirms what is being created. So this half of the flow is a
// page the dashboard renders and a redirect the engine receives, and it is
// the one place in this integration where the reconcile loop cannot act
// alone.
//
// # The credentials come back exactly once
//
// [ExchangeManifest] is the only moment GitHub will ever hand over the app's
// private key and webhook secret. There is no endpoint that reissues either.
// A crash between that response and the seal is unrecoverable: the app exists,
// the engine cannot authenticate as it, and the operator has to delete it and
// start again. That is why the exchange does nothing but decode, and why its
// caller seals before it does anything else.

// AppCreateTimeout bounds the manifest exchange.
//
// Longer than [ClientTimeout], because this is the one call whose failure
// costs an app: a timeout here leaves a created app whose key nobody holds,
// and the operator must delete it by hand. Thirty seconds is well past
// GitHub's own p99 for this endpoint and still short enough that a browser
// redirect does not appear to hang.
const AppCreateTimeout = 30 * time.Second

// RenewBefore is how long before expiry a held token is replaced.
//
// Ten minutes, against a one-hour life. Long enough that a slow pass, a
// retry and a clock a little out of step cannot land on the far side of
// expiry, and short enough that a token is not replaced on every tick.
const RenewBefore = 10 * time.Minute

// jwtLifetime is how long an app JWT is valid for.
//
// GitHub refuses anything over ten minutes and refuses a future `iat`
// outright, so this is nine, with the issue time backdated below.
const jwtLifetime = 9 * time.Minute

// jwtBackdate is how far the JWT's issue time is set back.
//
// GitHub compares `iat` against ITS clock, and refuses a token issued in the
// future. A machine a few seconds fast would otherwise have every call
// refused with a message about the clock that names nothing an operator can
// act on. Sixty seconds is what GitHub's own documentation recommends.
const jwtBackdate = time.Minute

// Manifest is the app this engine asks GitHub to create for one seat.
//
// Encoded exactly as GitHub's manifest schema, which is why the field names
// are its rather than this package's.
type Manifest struct {
	Name           string            `json:"name"`
	URL            string            `json:"url"`
	Description    string            `json:"description,omitempty"`
	Public         bool              `json:"public"`
	RedirectURL    string            `json:"redirect_url,omitempty"`
	SetupURL       string            `json:"setup_url,omitempty"`
	SetupOnUpdate  bool              `json:"setup_on_update,omitempty"`
	HookAttributes map[string]any    `json:"hook_attributes,omitempty"`
	DefaultEvents  []string          `json:"default_events,omitempty"`
	DefaultPerms   map[string]string `json:"default_permissions,omitempty"`
}

// ManifestEvents is what a seat's app subscribes to.
//
// WITHOUT THIS AN APP SUBSCRIBES TO NOTHING. It is not a refinement of the
// hook URL: an app with a perfectly good delivery address and no events
// receives nothing at all, and reports itself healthy while doing so.
//
// The set this engine's router acts on, and nothing wider. Pushes are
// excluded deliberately: the highest-volume event a busy repository produces,
// and the router drops every one.
var ManifestEvents = []string{
	"issues",
	"issue_comment",
	"pull_request",
	"pull_request_review",
	"pull_request_review_comment",
}

// ManifestOptions is what building one seat's manifest needs.
type ManifestOptions struct {
	// Seat is the agent this app belongs to, for the name and description.
	Seat string

	// Name is the app's name at GitHub. GitHub requires it to be unique
	// ACROSS ALL OF GITHUB and caps it at 34 characters, so it cannot be
	// derived from the seat alone: two companies with an `sre-lead` would
	// collide on the second one. [AppName] builds one that does not.
	Name string

	// DeliveryURL is where GitHub posts this app's events. Empty creates
	// the app with delivery switched OFF rather than pointed nowhere,
	// because an app delivering into a void looks healthy at GitHub and
	// receives nothing here.
	DeliveryURL string

	// RedirectURL is where GitHub returns the browser with the one-time
	// code. Required: without it the code is displayed to the operator
	// instead of being sent here, and the flow cannot complete.
	RedirectURL string

	// SetupURL is where GitHub returns the browser after the INSTALL,
	// which is a separate act from creating the app.
	SetupURL string

	// Tier is how much this seat may do, which decides the permissions the
	// app is created with. An app cannot be widened later without every
	// operator re-approving it, so this is the one field worth getting
	// right at creation.
	Tier Tier

	// Homepage is the `url` field, which GitHub requires. It is the only
	// required field in the whole manifest.
	Homepage string
}

// AppName is the name GitHub registers a seat's app under.
//
// GLOBALLY UNIQUE AND AT MOST 34 CHARACTERS, which is GitHub's rule and the
// one constraint that shapes this. A name built from the seat alone collides
// the second time any two companies both have an `sre-lead`, and the failure
// arrives as a manifest rejection an operator cannot do anything about.
//
// So the company's own name leads, the seat follows, and the whole is cut to
// fit. Cut on a RUNE boundary through textcut, because a name sliced through
// a multi-byte character is rejected by GitHub as malformed rather than as
// too long.
func AppName(company, seat string) string {
	parts := make([]string, 0, 2)
	for _, part := range []string{company, seat} {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			parts = append(parts, trimmed)
		}
	}
	name := strings.Join(parts, " ")
	if name == "" {
		name = "Crewlet agent"
	}
	return cut(name, 34)
}

// cut shortens to n runes without splitting one.
func cut(s string, n int) string {
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return strings.TrimSpace(string(runes[:n]))
}

// BuildManifest describes the app one seat needs.
func BuildManifest(opts ManifestOptions) Manifest {
	// BORN POINTING AT THE ENGINE, so an app never spends its first
	// minutes delivering nowhere. With no address the hook is created
	// INACTIVE rather than pointed at a placeholder, because an inactive
	// hook is a state the operator can see and a wrong URL is not.
	hook := map[string]any{"active": false}
	if url := strings.TrimSpace(opts.DeliveryURL); url != "" {
		hook = map[string]any{"url": url, "active": true}
	}
	home := strings.TrimSpace(opts.Homepage)
	if home == "" {
		home = "https://crewlet.ai"
	}
	seat := strings.TrimSpace(opts.Seat)
	return Manifest{
		Name:           opts.Name,
		URL:            home,
		Description:    "Crewlet agent " + seat + ", acting as itself on GitHub.",
		Public:         false,
		RedirectURL:    strings.TrimSpace(opts.RedirectURL),
		SetupURL:       strings.TrimSpace(opts.SetupURL),
		SetupOnUpdate:  true,
		HookAttributes: hook,
		DefaultEvents:  ManifestEvents,
		DefaultPerms:   opts.Tier.Permissions(),
	}
}

// ActionURL is where the operator's browser POSTs the manifest.
//
// THE ACCOUNT MATTERS. An app registered under a person's own account cannot
// be installed on the organization that owns the repositories, so an
// organization has to be sent to its own registration page. Getting this
// wrong produces an app that exists, belongs to the wrong account, and cannot
// be installed anywhere useful.
func ActionURL(webBase, org string) string {
	base := strings.TrimRight(strings.TrimSpace(webBase), "/")
	if base == "" {
		base = defaultWebBase
	}
	if owner := strings.TrimSpace(org); owner != "" {
		return base + "/organizations/" + url.PathEscape(owner) + "/settings/apps/new"
	}
	return base + "/settings/apps/new"
}

// CreatedApp is what GitHub returns when a manifest is converted.
//
// THE PEM AND THE WEBHOOK SECRET ARE RETURNED ONCE. GitHub has no endpoint
// that reissues either, so whatever reads this must seal both before doing
// anything that can fail.
type CreatedApp struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	Slug          string `json:"slug"`
	NodeID        string `json:"node_id"`
	HTMLURL       string `json:"html_url"`
	PEM           string `json:"pem"`
	WebhookSecret string `json:"webhook_secret"`
	ClientID      string `json:"client_id"`
	ClientSecret  string `json:"client_secret"`
	Owner         struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"owner"`
}

// ErrCodeSpent reports a manifest code GitHub has already converted or
// expired. Distinct because it is the one failure a retry cannot fix: the
// code is single use and lives one hour, so the operator has to start again.
var ErrCodeSpent = errors.New("github: this app-creation code has been used or has expired")

// ExchangeManifest turns the one-time code into an app.
//
// UNAUTHENTICATED, which is GitHub's design: the code IS the authentication,
// which is why it is single use, why it expires in an hour, and why the state
// that travels beside it has to be verified by the caller rather than trusted.
//
// This function decodes and returns. It deliberately does not log, retry or
// wrap the response body: the body carries the private key of an app, and the
// ordinary reflex of logging a response on error would put the root credential
// of every agent's identity into a log file.
func ExchangeManifest(ctx context.Context, apiBase, code string) (*CreatedApp, error) {
	if strings.TrimSpace(code) == "" {
		return nil, errors.New("github: no app-creation code")
	}
	base := strings.TrimRight(strings.TrimSpace(apiBase), "/")
	if base == "" {
		base = defaultAPIBase
	}
	target := base + "/app-manifests/" + url.PathEscape(strings.TrimSpace(code)) + "/conversions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, nil)
	if err != nil {
		return nil, fmt.Errorf("github: build the conversion request: %w", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", APIVersion)

	res, err := httpx.Client(AppCreateTimeout).Do(req)
	if err != nil {
		return nil, fmt.Errorf("github: convert the app manifest: %w", err)
	}
	defer func() { _ = res.Body.Close() }()

	switch {
	case res.StatusCode == http.StatusUnprocessableEntity, res.StatusCode == http.StatusNotFound:
		return nil, ErrCodeSpent
	case res.StatusCode < 200 || res.StatusCode > 299:
		// NAMED, NEVER QUOTED. The body of a successful call carries a
		// private key, and a failure path that prints the body is one
		// refactor away from printing a successful one.
		return nil, fmt.Errorf("github: convert the app manifest: %s", res.Status)
	}

	app := new(CreatedApp)
	if err := json.NewDecoder(res.Body).Decode(app); err != nil {
		return nil, fmt.Errorf("github: decode the created app: %w", err)
	}
	if app.ID == 0 || strings.TrimSpace(app.PEM) == "" {
		return nil, errors.New(
			"github: the conversion returned no app id or no private key, and " +
				"neither can be asked for again; delete the app at GitHub and " +
				"create it once more")
	}
	return app, nil
}

// InstallURL is where the operator installs a seat's app.
//
// THROUGH THE APP'S OWN SETTINGS, not through github.com/apps/{slug}. That
// public route exists only for PUBLIC apps, and every app this engine creates
// is private: the manifest sets `public: false`, because an agent's identity
// is that company's business and a public app is listed for anyone to
// install. Sending an operator to the public route gave them a 404 on the one
// click the whole flow depends on.
//
// BUILT FROM THE SLUG THE CONVERSION RETURNED, never from the name that was
// requested: GitHub slugifies a name and will disambiguate a collision, so
// the app that exists may not be the one whose name was asked for.
func InstallURL(webBase, org, slug string) string {
	if manage := ManageURL(webBase, org, slug); manage != "" {
		return manage + "/installations"
	}
	return ""
}

// ManageURL is the app's own settings page, where a person deletes it.
//
// DELETING AN APP IS NOT AN API CALL. GitHub offers no endpoint for it at
// any permission: an app is deleted from its settings page by somebody signed
// in as its owner. So a teardown can UNINSTALL an app, which is what revokes
// its access, and then has to hand the operator a link for the rest.
//
// The account matters here for the same reason it does at creation: an
// organization's app is managed under the organization's settings, and a
// link to a personal page opens somebody else's list.
func ManageURL(webBase, org, slug string) string {
	base := strings.TrimRight(strings.TrimSpace(webBase), "/")
	if base == "" {
		base = defaultWebBase
	}
	name := strings.TrimSpace(slug)
	if name == "" {
		return ""
	}
	if owner := strings.TrimSpace(org); owner != "" {
		return base + "/organizations/" + url.PathEscape(owner) + "/settings/apps/" + url.PathEscape(name)
	}
	return base + "/settings/apps/" + url.PathEscape(name)
}

// AppJWT signs the assertion that authenticates as the app itself.
//
// AS THE APP, NOT AS AN INSTALLATION. This is what lists installations and
// mints installation tokens, and it can do nothing else: it cannot read a
// repository or write a comment. GET /user answers 403 for it, which is why
// seat identity is resolved from the installation rather than from /user.
func AppJWT(appID int64, pem string, now time.Time) (string, error) {
	key, err := jwt.ParseRSAPrivateKeyFromPEM([]byte(strings.ReplaceAll(pem, `\n`, "\n")))
	if err != nil {
		return "", fmt.Errorf("github: read the app private key: %w", err)
	}
	return signJWT(appID, key, now)
}

func signJWT(appID int64, key *rsa.PrivateKey, now time.Time) (string, error) {
	issued := now.Add(-jwtBackdate)
	token := jwt.NewWithClaims(jwt.SigningMethodRS256, jwt.RegisteredClaims{
		Issuer:    strconv.FormatInt(appID, 10),
		IssuedAt:  jwt.NewNumericDate(issued),
		ExpiresAt: jwt.NewNumericDate(issued.Add(jwtLifetime)),
	})
	signed, err := token.SignedString(key)
	if err != nil {
		return "", fmt.Errorf("github: sign the app assertion: %w", err)
	}
	return signed, nil
}

// Installation is one installation of an app on an account.
type Installation struct {
	ID          int64             `json:"id"`
	Permissions map[string]string `json:"permissions"`
	Events      []string          `json:"events"`
	Account     struct {
		Login string `json:"login"`
		Type  string `json:"type"`
	} `json:"account"`
	RepositorySelection string `json:"repository_selection"`
	SuspendedAt         string `json:"suspended_at"`
}

// Suspended reports an installation the account has switched off. Its tokens
// are refused, and the fix is a person's, so it is worth telling apart from
// an installation that is simply gone.
func (i Installation) Suspended() bool { return strings.TrimSpace(i.SuspendedAt) != "" }

// InstallationToken is a credential for one installation, and it expires.
type InstallationToken struct {
	Token       string            `json:"token"`
	ExpiresAt   time.Time         `json:"expires_at"`
	Permissions map[string]string `json:"permissions"`
}

// Fresh reports a token with enough life left to be worth using.
func (t InstallationToken) Fresh(now time.Time) bool {
	return strings.TrimSpace(t.Token) != "" && t.ExpiresAt.After(now.Add(RenewBefore))
}

// AppClient talks to GitHub as an APP, using the assertion rather than a
// token. Separate from [Client] because the two authenticate differently and
// can do almost disjoint things.
type AppClient struct {
	base  string
	appID int64
	pem   string
	http  *http.Client
	now   func() time.Time
}

// NewAppClient builds a client for one seat's app.
func NewAppClient(appID int64, pem, apiBase string, now func() time.Time) (*AppClient, error) {
	if appID == 0 {
		return nil, errors.New("github: no app id")
	}
	if strings.TrimSpace(pem) == "" {
		return nil, errors.New("github: no app private key")
	}
	base := strings.TrimRight(strings.TrimSpace(apiBase), "/")
	if base == "" {
		base = defaultAPIBase
	}
	if now == nil {
		now = func() time.Time { return time.Now().UTC() }
	}
	return &AppClient{
		base: base, appID: appID, pem: pem,
		http: httpx.Client(ClientTimeout), now: now,
	}, nil
}

// Installations lists where this app is installed.
//
// THE AUTHORITY ON WHETHER AN INSTALLATION STILL EXISTS. A stored id that
// this does not return has been uninstalled by the account, which is a thing
// an operator does at GitHub and tells nobody about. Reading it back is the
// only way the engine learns.
func (c *AppClient) Installations(ctx context.Context) ([]Installation, error) {
	var out []Installation
	if err := c.call(ctx, http.MethodGet, "/app/installations?per_page=100", nil, &out); err != nil {
		return nil, err
	}
	return out, nil
}

// Installation reads one installation back.
func (c *AppClient) Installation(ctx context.Context, id int64) (*Installation, error) {
	out := new(Installation)
	path := "/app/installations/" + strconv.FormatInt(id, 10)
	if err := c.call(ctx, http.MethodGet, path, nil, out); err != nil {
		return nil, err
	}
	return out, nil
}

// MintToken issues a token for one installation, scoped to the tier.
//
// SCOPED DOWN AT MINT TIME, which is where the tier becomes real. The app may
// hold more than the tier asks for, because a person installed it and a
// person can widen it; the token carries only what the tier lists, so a
// read-only seat cannot write even on an installation that could.
//
// Repositories narrow it further where the seat names any. Empty means every
// repository the installation covers, which is what the operator chose when
// they installed it.
func (c *AppClient) MintToken(
	ctx context.Context, installationID int64, tier Tier, repos []string,
) (*InstallationToken, error) {
	body := map[string]any{"permissions": tier.Permissions()}
	if names := repoNames(repos); len(names) > 0 {
		body["repositories"] = names
	}
	out := new(InstallationToken)
	path := "/app/installations/" + strconv.FormatInt(installationID, 10) + "/access_tokens"
	if err := c.call(ctx, http.MethodPost, path, body, out); err != nil {
		return nil, err
	}
	if strings.TrimSpace(out.Token) == "" {
		return nil, errors.New("github: the token endpoint returned no token")
	}
	return out, nil
}

// Uninstall removes this app's installation, which is what actually revokes
// its access. Deleting the record here and leaving the installation would
// leave an app able to act with nothing watching it.
func (c *AppClient) Uninstall(ctx context.Context, installationID int64) error {
	path := "/app/installations/" + strconv.FormatInt(installationID, 10)
	return c.call(ctx, http.MethodDelete, path, nil, nil)
}

// repoNames takes the bare repository names GitHub's token endpoint wants.
//
// It accepts `owner/name` because that is how a person writes one and how the
// config stores it, and sends `name`, because the endpoint scopes within the
// installation's own account and refuses a qualified name.
func repoNames(repos []string) []string {
	out := make([]string, 0, len(repos))
	for _, repo := range repos {
		name := strings.TrimSpace(repo)
		if name == "" {
			continue
		}
		if _, after, found := strings.Cut(name, "/"); found {
			name = after
		}
		if name != "" {
			out = append(out, name)
		}
	}
	return out
}

// call performs one request signed as the app.
//
// THE ASSERTION IS MINTED PER CALL rather than held. It lives nine minutes,
// so a held one would need an expiry check on every use, and signing is
// cheaper than the round trip it precedes.
func (c *AppClient) call(ctx context.Context, method, path string, body, out any) error {
	assertion, err := AppJWT(c.appID, c.pem, c.now())
	if err != nil {
		return err
	}
	var reader *strings.Reader
	if body != nil {
		encoded, encodeErr := json.Marshal(body)
		if encodeErr != nil {
			return fmt.Errorf("github: encode %s %s: %w", method, path, encodeErr)
		}
		reader = strings.NewReader(string(encoded))
	}
	var req *http.Request
	if reader != nil {
		req, err = http.NewRequestWithContext(ctx, method, c.base+path, reader)
	} else {
		req, err = http.NewRequestWithContext(ctx, method, c.base+path, nil)
	}
	if err != nil {
		return fmt.Errorf("github: build %s %s: %w", method, path, err)
	}
	req.Header.Set("Authorization", "Bearer "+assertion)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", APIVersion)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("github: %s %s: %w", method, path, err)
	}
	defer func() { _ = res.Body.Close() }()

	if res.StatusCode < 200 || res.StatusCode > 299 {
		return &APIError{Method: method, Path: path, Status: res.StatusCode}
	}
	if out == nil {
		return nil
	}
	if err := json.NewDecoder(res.Body).Decode(out); err != nil {
		return fmt.Errorf("github: decode %s %s: %w", method, path, err)
	}
	return nil
}
