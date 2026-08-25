package jira

import (
	"context"
	"net/http"
	"strings"
)

// The webhook half of the client.
//
// # A different REST surface, and only one deployment has it
//
// Webhook administration lives at /rest/webhooks/1.0, outside the versioned
// /rest/api tree, which is why it does not go through [Client.api]. And it
// answers only on Data Center: on Cloud a dynamic webhook belongs to an
// APP — a Connect or Forge installation — so an API token gets a refusal no
// matter what it is allowed to do elsewhere. The reconcile checks the
// deployment before it calls, rather than reading a 401 here as a bad
// credential and sending an operator to rotate a token that is fine.

// hooksPath is the Data Center webhook administration prefix.
const hooksPath = "/rest/webhooks/1.0/webhook"

// WebhookEvents are the events the engine's hook subscribes to.
//
// EXACTLY what [Parser.Parse] routes, and that is the invariant worth
// keeping: a hook subscribed to more delivers payloads the parser drops,
// which is bandwidth and an audit row per irrelevant workspace change; a
// hook subscribed to less is an event class that silently never arrives.
var WebhookEvents = []string{
	"jira:issue_created",
	"jira:issue_updated",
	"jira:issue_deleted",
	"comment_created",
	"comment_updated",
	"comment_deleted",
}

// Webhook is one registered inbound hook.
type Webhook struct {
	// ID is the identifier the update and delete paths take. Jira Data
	// Center reports it only as the tail of `self`, so it is parsed from
	// there rather than read from a field that does not exist.
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
	// ExcludeBody must be false or the delivery carries no issue at all,
	// which the parser reads as an event naming nobody.
	ExcludeBody bool `json:"excludeBody"`
	// Secret is the HMAC key Jira signs the body with, sent as
	// X-Hub-Signature. Jira Data Center gained it in 9.x; an instance
	// that predates it ignores the field and delivers unsigned, which
	// the webhook edge then refuses — a loud failure rather than an
	// unverified acceptance.
	Secret string `json:"secret,omitempty"`
}

func (w webhookWire) hook() Webhook {
	return Webhook{
		ID: idOf(w.Self), Name: w.Name, URL: w.URL,
		Events: w.Events, Enabled: w.Enabled,
	}
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

// CreateWebhook registers a hook pointing at target, signed with secret.
func (c *Client) CreateWebhook(ctx context.Context, name, target, secret string) (Webhook, error) {
	body := webhookWire{
		Name: name, URL: target, Events: WebhookEvents,
		Enabled: true, ExcludeBody: false, Secret: secret,
	}
	var out webhookWire
	if err := c.do(ctx, http.MethodPost, hooksPath, nil, body, &out); err != nil {
		return Webhook{}, err
	}
	return out.hook(), nil
}

// UpdateWebhook brings an existing hook in line.
func (c *Client) UpdateWebhook(ctx context.Context, id, name, target, secret string) (Webhook, error) {
	body := webhookWire{
		Name: name, URL: target, Events: WebhookEvents,
		Enabled: true, ExcludeBody: false, Secret: secret,
	}
	var out webhookWire
	if err := c.do(ctx, http.MethodPut, hooksPath+"/"+id, nil, body, &out); err != nil {
		return Webhook{}, err
	}
	if out.Self == "" {
		// Some builds answer an update with an empty body. The hook is
		// the one that was addressed either way, and reporting an empty
		// one would make a successful converge look like a no-op.
		return Webhook{ID: id, Name: name, URL: target, Events: WebhookEvents, Enabled: true}, nil
	}
	return out.hook(), nil
}

// DeleteWebhook removes a hook.
func (c *Client) DeleteWebhook(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, hooksPath+"/"+id, nil, nil, nil)
}
