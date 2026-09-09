package confluence

import (
	"context"
	"net/http"
	"net/url"
	"strings"
)

// Webhook administration, on both deployments, at /rest/webhooks/1.0.
//
// # What was established against a live Cloud site, and what it means
//
// Confluence Cloud has no webhook page in its administration UI and no
// documented REST surface for registering one: CONFCLOUD-36613 has asked for
// exactly that since 2015. The endpoint below nonetheless answers on Cloud
// (200 to a listing, 201 to a registration, 204 to a delete, with an ordinary
// API token over Basic auth) and DELIVERS. Every fact in this file was
// measured rather than read, because nothing to read exists:
//
//   - A delivery carries NO signature. The registration accepts a "secret"
//     field and silently ignores it; the request that arrives has
//     Content-Type, X-B3 tracing headers and the proxy's X-Forwarded ones,
//     and nothing else. Jira's twin endpoint signs and says so (isSigned in
//     its response); Confluence's response has no such field, and now we
//     know why.
//   - Userinfo in the registered URL (https://user:pass@host/) is stored
//     verbatim and NOT turned into an Authorization header on delivery.
//   - A query string in the registered URL IS delivered verbatim. It is the
//     one channel through which anything secret reaches the receiver, which
//     is why the Cloud target below carries its token that way.
//   - "excludeBody" is ignored: the body arrives regardless.
//   - The endpoint validates no event names. A registration for an event
//     Confluence will never emit answers 201 and never fires, so "accepted"
//     is not evidence of anything.
//   - The payload carries no event name at all. Which event fired is known
//     only from which hook was registered for it, so the engine registers
//     ONE HOOK PER EVENT with the event in the path and lets the route stamp
//     it back onto the body.
//
// # Undocumented means undocumented
//
// Atlassian has never stated this endpoint's support status, and can change
// or remove it without notice. The failure mode for a self-hosted engine is
// every deployment breaking at once with nobody to ask. That is stated in
// the docs beside the feature rather than hidden here, and the Forge relay
// remains the supported route for anyone who would rather not carry the risk.

// hooksPath is the webhook administration prefix, relative to the client's
// base (which already carries /wiki on a Cloud site).
const hooksPath = "/rest/webhooks/1.0/webhook"

// WebhookEvents are the events the engine's hooks subscribe to.
//
// EXACTLY the set [Parser.Parse] routes, and that is the invariant worth
// keeping: a hook subscribed to more delivers payloads the parser drops,
// which is bandwidth and an audit row per irrelevant workspace change; a
// hook subscribed to less is an event class that silently never arrives. On
// Cloud there is one hook per entry here, because the payload names no event.
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
// # Why the token rides in the URL
//
// Because it is the only place Confluence Cloud will carry one. It attaches
// no signature, drops userinfo, and honours no header field on registration;
// the query string is delivered verbatim and nothing else is. The engine
// compares it constant-time, exactly as it does the fixed header Datadog
// sends, and for the same reason: a provider that cannot sign a body leaves
// a shared token as the strongest check available.
//
// The consequences are the same as Datadog's and are stated in the docs: a
// replayed delivery is indistinguishable from a fresh one, and anyone holding
// the token can forge an event. It is treated as a signing key, rotated like
// one, and never logged. The route reads it from the query and NEVER echoes
// the query string into a log line or an event row.
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
