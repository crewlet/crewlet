package slack

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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
		APIBase+"/"+method, strings.NewReader(string(payload)))
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
	return Identity{UserID: out.UserID, TeamID: out.TeamID, BotID: out.BotID}, nil
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
