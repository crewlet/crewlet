package slack

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/httpx"
)

// The Web API client.
//
// ONE PER SEAT, because a Slack app has one bot user and one token: there is
// no company-wide credential to fall back to, which is the deepest
// difference between this backend and every other one the engine serves.

// APIBase is the Web API root.
const APIBase = "https://slack.com/api"

// ClientTimeout bounds one ordinary request.
//
// Ten seconds: chat.postMessage and assistant.threads.setStatus are both
// fast, and the status call runs on a heartbeat that must not pile up behind
// a slow response. The manifest calls take their own, longer, budget — see
// [ManifestTimeout].
const ClientTimeout = 10 * time.Second

// Client is one authenticated Slack app.
type Client struct {
	token string
	http  *http.Client
}

// NewClient builds a client for one bot token.
func NewClient(token string, httpClient *http.Client) (*Client, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return nil, fmt.Errorf("slack: no bot token")
	}
	if httpClient == nil {
		httpClient = httpx.Client(ClientTimeout)
	}
	return &Client{token: token, http: httpClient}, nil
}

// APIError is a Slack method that answered `ok: false`, typed.
//
// Slack reports every failure as HTTP 200 with an error CODE in the body, so
// a caller deciding what a refusal means — `channel_not_found` is a config
// problem, `invalid_auth` is a revoked token, `ratelimited` is neither —
// would otherwise have to substring-match a field it could read directly.
type APIError struct {
	Method string
	Code   string
	// Messages are the per-field details Slack attaches to a manifest
	// validation failure, and empty everywhere else.
	Messages []string
}

func (e *APIError) Error() string {
	msg := "slack: " + e.Method + ": " + e.Code
	if len(e.Messages) > 0 {
		msg += "\n  - " + strings.Join(e.Messages, "\n  - ")
	}
	return msg
}

// call posts a JSON body to one Web API method and decodes the answer.
//
// The token is a PARAMETER rather than always the client's, because
// provisioning authenticates three different ways against the same host: a
// bot token, an app-configuration token, and nothing at all for the OAuth
// exchange, which carries its credentials in the body.
func call(ctx context.Context, httpClient *http.Client, method, token string, body, out any) error {
	payload, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("slack: encode %s: %w", method, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		APIBase+"/"+method, bytes.NewReader(payload))
	if err != nil {
		return fmt.Errorf("slack: %s: %w", method, err)
	}
	req.Header.Set("Content-Type", "application/json; charset=utf-8")
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("slack: %s: %w", method, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("slack: %s: %w", method, err)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return &RateLimited{Method: method, RetryAfter: retryAfter(resp)}
	}
	return decode(method, raw, out)
}

// callQuery is [call] for a method that will not read a JSON body.
//
// SLACK ACCEPTS JSON FOR SOME METHODS AND SILENTLY IGNORES IT FOR THE REST,
// which is the worst of the three possible behaviours: `bots.info` posted as
// JSON answers `{"ok":true}` with no bot object at all, so the parameter is
// dropped, the envelope says success, and the caller decodes an empty answer
// from a call that reported working. Measured against the live API.
//
// The parameters ride in the query string, which every read method accepts,
// and the body stays empty.
func callQuery(ctx context.Context, httpClient *http.Client,
	method, token string, params url.Values, out any,
) error {
	address := APIBase + "/" + method
	if encoded := params.Encode(); encoded != "" {
		address += "?" + encoded
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return fmt.Errorf("slack: %s: %w", method, err)
	}
	req.Header.Set("Accept", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("slack: %s: %w", method, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return fmt.Errorf("slack: %s: %w", method, err)
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return &RateLimited{Method: method, RetryAfter: retryAfter(resp)}
	}
	return decode(method, raw, out)
}

// decode reads Slack's ok/error envelope, then the caller's own fields.
//
// TWO PASSES over one body rather than one struct with the envelope
// embedded: every caller's shape would otherwise have to carry `ok` and
// `error` fields it never reads, and a caller that forgot them would treat a
// refusal as an empty success.
func decode(method string, raw []byte, out any) error {
	var envelope struct {
		OK     bool   `json:"ok"`
		Error  string `json:"error"`
		Errors []struct {
			Message string `json:"message"`
			Pointer string `json:"pointer"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return fmt.Errorf("slack: decode %s: %w", method, err)
	}
	if !envelope.OK {
		apiErr := &APIError{Method: method, Code: orNone(envelope.Error)}
		for _, detail := range envelope.Errors {
			if detail.Pointer != "" {
				apiErr.Messages = append(apiErr.Messages,
					detail.Message+" ("+detail.Pointer+")")
				continue
			}
			apiErr.Messages = append(apiErr.Messages, detail.Message)
		}
		return apiErr
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("slack: decode %s: %w", method, err)
	}
	return nil
}

// RateLimited is Slack asking the caller to wait.
//
// TYPED AND SEPARATE from [APIError], because it is the one refusal that is
// not about the request: the same call will succeed unchanged after the
// wait, and only the manifest methods retry it.
type RateLimited struct {
	Method     string
	RetryAfter time.Duration
}

func (e *RateLimited) Error() string {
	return fmt.Sprintf("slack: %s: rate limited, retry after %s", e.Method, e.RetryAfter)
}

// retryAfter reads Slack's own wait, defaulting to a Tier 1 cadence.
//
// Tier 1 is roughly one request a minute, which is what the manifest methods
// are — so a response with no header is assumed to want a full minute rather
// than a token pause that would just be refused again.
func retryAfter(resp *http.Response) time.Duration {
	if raw := resp.Header.Get("Retry-After"); raw != "" {
		if seconds, err := strconv.Atoi(strings.TrimSpace(raw)); err == nil && seconds > 0 {
			return time.Duration(seconds) * time.Second
		}
	}
	return TierOneCadence
}

// TierOneCadence is Slack's slowest published rate class, ~1 request a
// minute, and the wait assumed when a 429 names none.
const TierOneCadence = 60 * time.Second

// Identity is who a bot token authenticates as.
type Identity struct {
	UserID string
	TeamID string
	AppID  string
	// BotID is the B… identity, distinct from the U… user id: a legacy
	// bot_message echo carries only this one.
	BotID string
}

// AuthTest is the identity behind this client's token.
//
// The boot identity check, and the whole of a seat's own-message
// suppression: a Slack payload names this seat by its bot user id, and
// nothing in the org model declares it. A token that resolves proves the
// credential works AND names the account.
func (c *Client) AuthTest(ctx context.Context) (Identity, error) {
	var out struct {
		UserID string `json:"user_id"`
		TeamID string `json:"team_id"`
		BotID  string `json:"bot_id"`
	}
	if err := call(ctx, c.http, "auth.test", c.token, map[string]any{}, &out); err != nil {
		return Identity{}, err
	}
	id := Identity{UserID: out.UserID, TeamID: out.TeamID, BotID: out.BotID}
	// THE APP, WHICH auth.test DOES NOT SAY.
	//
	// It answers with the bot's user id, its team and its bot id, and no
	// app id at all: [Identity.AppID] was decoded from a response field
	// Slack does not send, so it was empty for every seat this engine has
	// ever wired, and nothing noticed because the one thing reading it
	// falls back to the delivery's own envelope. bots.info is where the id
	// lives, keyed on the bot id this call does return.
	//
	// BEST EFFORT, and never fatal: knowing the app is what lets a screen
	// say WHICH of an operator's apps this agent is and link to it. A seat
	// whose token works is a seat that works, and failing it over the
	// second request would trade a running agent for a label.
	if id.BotID != "" {
		app, err := c.AppOf(ctx, id.BotID)
		if err != nil {
			log.WarnContext(ctx, "slack_app_unknown", "bot", id.BotID,
				"error", err.Error(),
				"detail", "this seat works; nothing can say which app it is")
		}
		id.AppID = app
	}
	return id, nil
}

// AppOf is the app a bot user belongs to.
//
// It needs only `users:read`, which every agent's manifest grants, and the
// bot id [Client.AuthTest] returns.
func (c *Client) AppOf(ctx context.Context, botID string) (string, error) {
	if strings.TrimSpace(botID) == "" {
		return "", fmt.Errorf("slack: bots.info: no bot id")
	}
	var out struct {
		Bot struct {
			AppID string `json:"app_id"`
		} `json:"bot"`
	}
	if err := callQuery(ctx, c.http, "bots.info", c.token,
		url.Values{"bot": {botID}}, &out); err != nil {
		return "", err
	}
	if out.Bot.AppID == "" {
		// AN EMPTY ANSWER IS AN ERROR HERE, because Slack's is not: a
		// method that will not read the parameter answers ok with
		// nothing, and reported as success that is a seat silently
		// carrying no app for ever.
		return "", fmt.Errorf("slack: bots.info: %s named no app", botID)
	}
	return out.Bot.AppID, nil
}

// PostMessage sends a message, optionally into a thread.
//
// Returns the posted message's timestamp, which is its id — and is the
// thread anchor for anything posted under it.
func (c *Client) PostMessage(ctx context.Context, channel, thread, text string) (string, error) {
	if channel == "" {
		return "", fmt.Errorf("slack: chat.postMessage: no channel")
	}
	body := map[string]any{"channel": channel, "text": text}
	if thread != "" {
		body["thread_ts"] = thread
	}
	var out struct {
		TS string `json:"ts"`
	}
	if err := call(ctx, c.http, "chat.postMessage", c.token, body, &out); err != nil {
		return "", err
	}
	return out.TS, nil
}

// SetStatus raises, updates or clears the working indicator on a thread.
//
// assistant.threads.setStatus renders "*<agent> is thinking…*" under the
// thread's composer — the closest thing Slack offers a bot, since there is
// no public typing API for a granular app. An empty status clears it; Slack
// also clears it the moment the app posts into the thread, and expires any
// raised status on its own.
func (c *Client) SetStatus(ctx context.Context, channel, thread, status string) error {
	if channel == "" || thread == "" {
		return fmt.Errorf("slack: assistant.threads.setStatus: no conversation")
	}
	return call(ctx, c.http, "assistant.threads.setStatus", c.token, map[string]any{
		"channel_id": channel, "thread_ts": thread, "status": status,
	}, nil)
}
