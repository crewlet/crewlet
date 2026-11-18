package confluence

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

// Webhook administration, on both deployments, at /rest/webhooks/1.0.
//
// Cloud documents no REST surface for registering a webhook (CONFCLOUD-36613),
// yet this endpoint answers and DELIVERS with an API token over Basic auth.
// Measured on a live site: a delivery carries NO signature (a registered
// "secret" is ignored); userinfo in the URL is never sent as Authorization,
// but a query string IS delivered verbatim; "excludeBody" is ignored; no event
// names are validated, so a 201 is no evidence the hook fires; the payload
// names no event ([WebhookEvents]). Atlassian can remove the endpoint without
// notice; the docs state that risk and name the Forge relay as the supported
// route.

// hooksPath is the webhook administration prefix, relative to the client's
// base (which already carries /wiki on a Cloud site).
const hooksPath = "/rest/webhooks/1.0/webhook"

// WebhookEvents are the events the engine's hooks subscribe to.
//
// EXACTLY the set [Parser.Parse] routes, and that is the invariant worth
// keeping: a hook subscribed to more delivers payloads the parser drops,
// which is bandwidth and an audit row per irrelevant workspace change; a
// hook subscribed to less is an event class that silently never arrives. On
// Cloud there is ONE HOOK PER EVENT with the event in the path, because the
// payload names none; the route stamps it back onto the body.
var WebhookEvents = []string{
	"page_created",
	"page_updated",
	"page_trashed",
	"page_removed",
	"blog_created",
	"blog_updated",
	"comment_created",
	"comment_updated",
}

// HookNamePrefix is what every hook this engine registers is named under, so
// a converge can find its own without touching anything an operator made by
// hand.
const HookNamePrefix = "crewlet:"

// HookName is the name a hook for one event is registered under.
func HookName(event string) string { return HookNamePrefix + event }

// Webhook is one registered inbound hook.
type Webhook struct {
	// ID is the identifier the update and delete paths take, parsed from
	// the tail of `self` because neither deployment reports it as a
	// field.
	ID      string
	Name    string
	URL     string
	Events  []string
	Enabled bool
}

// webhookWire is the payload shape, in both directions.
type webhookWire struct {
	Self    string   `json:"self,omitempty"`
	Name    string   `json:"name"`
	URL     string   `json:"url"`
	Events  []string `json:"events"`
	Enabled bool     `json:"enabled"`
	// ExcludeBody is sent false so a Data Center instance, which honours
	// it, delivers the page. Cloud ignores the field either way.
	ExcludeBody bool `json:"excludeBody"`
	// Secret is honoured by Data Center, which signs with it, and ignored
	// by Cloud, which does not sign at all. Sent on both so a Data Center
	// hook registered through this path verifies, and so the Cloud route
	// can never mistake its presence for a guarantee.
	Secret string `json:"secret,omitempty"`
}

func (w webhookWire) hook() Webhook {
	return Webhook{ID: idOf(w.Self), Name: w.Name, URL: w.URL, Events: w.Events, Enabled: w.Enabled}
}

// idOf is the identifier at the tail of a `self` link.
func idOf(self string) string {
	if i := strings.LastIndex(self, "/"); i >= 0 {
		return self[i+1:]
	}
	return self
}

// Webhooks lists the instance's registered hooks.
func (c *Client) Webhooks(ctx context.Context) ([]Webhook, error) {
	var rows []webhookWire
	if err := c.do(ctx, http.MethodGet, hooksPath, nil, nil, &rows); err != nil {
		return nil, err
	}
	out := make([]Webhook, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.hook())
	}
	return out, nil
}

// CreateWebhook registers a hook for exactly the given events.
func (c *Client) CreateWebhook(ctx context.Context, name, target string, events []string, secret string) (Webhook, error) {
	body := webhookWire{Name: name, URL: target, Events: events, Enabled: true, Secret: secret}
	var out webhookWire
	if err := c.do(ctx, http.MethodPost, hooksPath, nil, body, &out); err != nil {
		return Webhook{}, err
	}
	return out.hook(), nil
}

// UpdateWebhook brings an existing hook in line.
func (c *Client) UpdateWebhook(ctx context.Context, id, name, target string, events []string, secret string) (Webhook, error) {
	body := webhookWire{Name: name, URL: target, Events: events, Enabled: true, Secret: secret}
	var out webhookWire
	if err := c.do(ctx, http.MethodPut, hooksPath+"/"+url.PathEscape(id), nil, body, &out); err != nil {
		return Webhook{}, err
	}
	if out.Self == "" {
		// An update answered with an empty body. The hook is the one that
		// was addressed either way, and reporting an empty one would make
		// a successful converge look like a no-op.
		return Webhook{ID: id, Name: name, URL: target, Events: events, Enabled: true}, nil
	}
	return out.hook(), nil
}

// DeleteWebhook removes a hook.
func (c *Client) DeleteWebhook(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, hooksPath+"/"+url.PathEscape(id), nil, nil, nil)
}

// CloudWebhookTarget is the delivery address one Cloud hook is registered
// with: the engine's Cloud route for that event, carrying the shared token in
// the query.
//
// The token rides in the URL because the query string is the only channel
// Cloud delivers verbatim. It is compared constant-time, as Datadog's fixed
// header is: the strongest check a provider that cannot sign leaves. A replay
// is indistinguishable from a fresh delivery and the holder can forge events,
// so it is a signing key, rotated like one; the route NEVER echoes the query
// string into a log line or an event row.
func CloudWebhookTarget(base, token, event string) string {
	base = strings.TrimRight(strings.TrimSpace(base), "/")
	if base == "" {
		return ""
	}
	q := url.Values{"token": {token}}
	return base + "/webhooks/confluence/" + url.PathEscape(event) + "?" + q.Encode()
}

// SameTarget reports whether a registered URL is this event's Cloud target,
// token and all.
//
// Compared as PARSED URLS rather than strings, because Confluence stores what
// it was given verbatim and a base with a trailing slash, or a query written
// in a different key order, would otherwise read as a hook pointing
// somewhere else and be re-registered every pass.
func SameTarget(registered, want string) bool {
	if !SameAddress(registered, want) {
		return false
	}
	a, errA := url.Parse(registered)
	b, errB := url.Parse(want)
	if errA != nil || errB != nil {
		return false
	}
	return a.Query().Get("token") == b.Query().Get("token")
}

// SameAddress is [SameTarget] without the token, for the Data Center hook.
//
// A Data Center registration carries no token in its URL — it is signed
// instead — so comparing one would refuse every match. Everything else about
// the comparison is the same, and it is the half that matters: the Data
// Center path compared raw strings with !=, so a registration Confluence
// stored with a trailing slash read as a hook pointing somewhere else and was
// re-created every pass.
func SameAddress(registered, want string) bool {
	a, err := url.Parse(registered)
	if err != nil {
		return false
	}
	b, err := url.Parse(want)
	if err != nil {
		return false
	}
	return a.Scheme == b.Scheme && a.Host == b.Host &&
		strings.TrimRight(a.Path, "/") == strings.TrimRight(b.Path, "/")
}
