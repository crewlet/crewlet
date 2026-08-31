package slack

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/httpx"
)

// The provisioning half of the Web API.
//
// A DIFFERENT CREDENTIAL FROM THE REST OF THIS PACKAGE, and that is the
// point: app management authenticates with an app-CONFIGURATION token, which
// belongs to the operator and rotates every twelve hours, while everything
// else uses a seat's bot token. Mixing them is how a run tries to create an
// app with a bot token and reads Slack's refusal as a broken manifest.

// ManifestTimeout bounds one manifest call.
//
// Longer than an ordinary request because apps.manifest.create and .update
// do server-side app provisioning and are markedly slower than a plain Web
// API method. Thirty seconds covers that with headroom and still fails fast
// enough for a command somebody is watching.
const ManifestTimeout = 30 * time.Second

// RateLimitBudget is the total wall-clock a manifest call will spend waiting
// out 429s.
//
// The manifest methods are Tier 1 — roughly one request a minute — and a
// sequential multi-agent run issues several back to back, so a single retry
// is not enough to ride out the burst. Three minutes fits two full Tier 1
// waits with headroom, and a genuinely stuck endpoint still fails in bounded
// time rather than hanging a command for ever.
const RateLimitBudget = 3 * time.Minute

// ConfigToken is an app-configuration access token and the refresh token
// that will mint the next one.
type ConfigToken struct {
	Token        string
	RefreshToken string
	// ExpiresAt is Slack's own expiry, or the zero time when the rotate
	// response carried none. Persisted so a later run can skip rotating
	// while the access token is still good — each rotation invalidates
	// the previous refresh token, so rotating needlessly is how an
	// operator ends up locked out of their own apps.
	ExpiresAt time.Time
}

// AppCredentials are what apps.manifest.create returns and NOTHING ELSE EVER
// DOES.
//
// Slack serves these once, at creation. A run that loses them cannot install
// the app, cannot verify its deliveries, and cannot re-read them from
// anywhere — the only recovery is deleting the app and making another. That
// is why the ledger is written before anything else uses them.
type AppCredentials struct {
	AppID         string
	ClientID      string
	ClientSecret  string
	SigningSecret string
}

// Install is the workspace install minted by the OAuth exchange.
type Install struct {
	BotToken  string
	AppID     string
	BotUserID string
	TeamID    string
}

// Admin talks to Slack's app-management surface.
type Admin struct{ http *http.Client }

// NewAdmin builds the provisioning client.
func NewAdmin(httpClient *http.Client) *Admin {
	if httpClient == nil {
		httpClient = httpx.Client(ManifestTimeout)
	}
	return &Admin{http: httpClient}
}

// RotateConfigToken exchanges a refresh token for a fresh access token.
//
// Slack's rotation is single-use in both directions: the call returns a new
// refresh token AND invalidates the one it was given, so a run that rotates
// and then fails to persist the result has locked the operator out. The
// caller records the pair before doing anything else with it.
func (a *Admin) RotateConfigToken(ctx context.Context, refresh string) (ConfigToken, error) {
	if strings.TrimSpace(refresh) == "" {
		return ConfigToken{}, fmt.Errorf("slack: no app-configuration refresh token")
	}
	var out struct {
		Token        string `json:"token"`
		RefreshToken string `json:"refresh_token"`
		Exp          int64  `json:"exp"`
	}
	if err := a.retrying(ctx, "tooling.tokens.rotate", "",
		map[string]any{"refresh_token": refresh}, &out); err != nil {
		return ConfigToken{}, err
	}
	token := ConfigToken{Token: out.Token, RefreshToken: out.RefreshToken}
	if out.Exp > 0 {
		token.ExpiresAt = time.Unix(out.Exp, 0).UTC()
	}
	return token, nil
}

// ValidateManifest checks a manifest without creating or changing anything.
func (a *Admin) ValidateManifest(ctx context.Context, configToken string, manifest map[string]any, appID string) error {
	encoded, err := encodeManifest(manifest)
	if err != nil {
		return err
	}
	body := map[string]any{"manifest": encoded}
	if appID != "" {
		body["app_id"] = appID
	}
	return a.retrying(ctx, "apps.manifest.validate", configToken, body, nil)
}

// CreateApp creates one app from a manifest and returns its credentials.
func (a *Admin) CreateApp(ctx context.Context, configToken string, manifest map[string]any) (AppCredentials, error) {
	var out struct {
		AppID       string `json:"app_id"`
		Credentials struct {
			ClientID      string `json:"client_id"`
			ClientSecret  string `json:"client_secret"`
			SigningSecret string `json:"signing_secret"`
		} `json:"credentials"`
	}
	encoded, err := encodeManifest(manifest)
	if err != nil {
		return AppCredentials{}, err
	}
	if err := a.retrying(ctx, "apps.manifest.create", configToken,
		map[string]any{"manifest": encoded}, &out); err != nil {
		return AppCredentials{}, err
	}
	return AppCredentials{
		AppID:         out.AppID,
		ClientID:      out.Credentials.ClientID,
		ClientSecret:  out.Credentials.ClientSecret,
		SigningSecret: out.Credentials.SigningSecret,
	}, nil
}

// UpdateApp pushes a manifest onto an app that already exists.
func (a *Admin) UpdateApp(ctx context.Context, configToken, appID string, manifest map[string]any) error {
	if appID == "" {
		return fmt.Errorf("slack: apps.manifest.update: no app id")
	}
	encoded, err := encodeManifest(manifest)
	if err != nil {
		return err
	}
	return a.retrying(ctx, "apps.manifest.update", configToken, map[string]any{
		"app_id": appID, "manifest": encoded,
	}, nil)
}

// Exchange turns the temporary code from an operator's authorize click into
// the app's bot token.
//
// FORM-ENCODED and unauthenticated, unlike every other call here: the client
// id and secret ARE the credentials, carried in the body, which is what the
// OAuth exchange is specified as.
func (a *Admin) Exchange(ctx context.Context, clientID, clientSecret, code, base string) (Install, error) {
	form := url.Values{
		"client_id":     {clientID},
		"client_secret": {clientSecret},
		"code":          {code},
		"redirect_uri":  {OAuthRedirectURL(base)},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		APIBase+"/oauth.v2.access", strings.NewReader(form.Encode()))
	if err != nil {
		return Install{}, fmt.Errorf("slack: oauth.v2.access: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := a.http.Do(req)
	if err != nil {
		return Install{}, fmt.Errorf("slack: oauth.v2.access: %w", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Install{}, fmt.Errorf("slack: oauth.v2.access: %w", err)
	}
	var out struct {
		AccessToken string `json:"access_token"`
		BotUserID   string `json:"bot_user_id"`
		AppID       string `json:"app_id"`
		Team        struct {
			ID string `json:"id"`
		} `json:"team"`
	}
	if err := decode("oauth.v2.access", raw, &out); err != nil {
		return Install{}, err
	}
	if out.AccessToken == "" {
		return Install{}, fmt.Errorf(
			"slack: oauth.v2.access returned no bot token — the install was " +
				"approved for a user token rather than a bot one, which means " +
				"the app's manifest carries no bot scopes")
	}
	return Install{
		BotToken: out.AccessToken, AppID: out.AppID,
		BotUserID: out.BotUserID, TeamID: out.Team.ID,
	}, nil
}

// retrying runs one call, waiting out rate limits inside [RateLimitBudget].
//
// SLEEPS RATHER THAN FAILING, because a Tier 1 refusal is not about the
// request: the same call succeeds unchanged after the wait, and failing
// would make a seven-agent run impossible to complete at all. The budget is
// what keeps a stuck endpoint from hanging the command for ever.
func (a *Admin) retrying(ctx context.Context, method, token string, body, out any) error {
	deadline := time.Now().Add(RateLimitBudget)
	for {
		err := call(ctx, a.http, method, token, body, out)
		var limited *RateLimited
		if !errors.As(err, &limited) {
			return err
		}
		wait := limited.RetryAfter
		if time.Now().Add(wait).After(deadline) {
			return fmt.Errorf(
				"slack: %s stayed rate limited for %s — Slack's app-management "+
					"methods allow about one request a minute, so a run over "+
					"many seats needs to be spread out or resumed: %w",
				method, RateLimitBudget, err)
		}
		log.InfoContext(ctx, "slack_rate_limited", "method", method, "waiting", wait.String())
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// encodeManifest renders a manifest the way the manifest methods take it: as
// a JSON-encoded STRING in the `manifest` field, not as a nested object.
// It RETURNS THE ERROR rather than "{}". apps.manifest.update REPLACES an
// app's manifest, so pushing an empty object would strip every scope, event
// subscription and redirect URL from a live app — and the call would report
// success. Unreachable today, since Manifest builds only marshalable types,
// but "unreachable" is not a reason to encode the worst possible fallback.
func encodeManifest(manifest map[string]any) (string, error) {
	encoded, err := json.Marshal(manifest)
	if err != nil {
		return "", fmt.Errorf("slack: encode manifest: %w", err)
	}
	return string(encoded), nil
}

func digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}
