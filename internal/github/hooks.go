package github

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
)

// Registering the inbound webhook.
//
// # One hook on the organization, or one per repository
//
// GitHub will deliver an organization's every repository event through a
// single ORG hook, which is the difference between one registration and
// fifty on a company with fifty repositories — and, more importantly,
// between a new repository routing on the day it is created and routing
// whenever somebody remembers to re-run the provisioner.
//
// It is not always available: an org hook needs `admin:org_hook`, which a
// fine-grained token cannot carry at all and a classic token only carries if
// whoever minted it ticked the box. So the mode is a tri-state and the
// default falls back — see [config.ContainerWebhookMode].

// WebhookEvents are the deliveries this integration's parser reads.
//
// NAMED EXPLICITLY rather than subscribing to `*`. A wildcard hook delivers
// every push, every star, every fork and every check_run — on a busy
// repository that is thousands of deliveries a day, each one verified,
// stored, deduped and routed to nobody. The set here is exactly what
// [Parser.Parse] has a case for, so a subscription that grew past the parser
// would be visible as a list this file and that switch disagree on.
var WebhookEvents = []string{
	EventIssues,
	EventPullRequest,
	EventIssueComment,
	EventReview,
	EventReviewNote,
	EventWorkflowRun,
}

// Webhook is one registered hook, as GitHub reports it.
type Webhook struct {
	ID     int64
	URL    string
	Active bool
	Events []string
}

// hookWire is GitHub's own shape, whose delivery URL lives one level down
// inside `config`.
type hookWire struct {
	ID     int64    `json:"id"`
	Active bool     `json:"active"`
	Events []string `json:"events"`
	Config struct {
		URL string `json:"url"`
	} `json:"config"`
}

func (h hookWire) webhook() Webhook {
	return Webhook{ID: h.ID, URL: h.Config.URL, Active: h.Active, Events: h.Events}
}

// hookBody is the request shape for creating and updating a hook.
type hookBody struct {
	Name   string     `json:"name,omitempty"`
	Active bool       `json:"active"`
	Events []string   `json:"events"`
	Config hookConfig `json:"config"`
}

type hookConfig struct {
	URL string `json:"url"`
	// ContentType must be json: GitHub's DEFAULT is form-encoded, which
	// delivers the payload as a urlencoded `payload=` field. Every
	// verifier and parser here reads a JSON body, so a hook created
	// without this line signs and delivers something nothing can decode.
	ContentType string `json:"content_type"`
	Secret      string `json:"secret,omitempty"`
	// InsecureSSL is GitHub's own spelling of "verify the certificate",
	// inverted and stringly typed. Always "0" — a hook that skipped
	// verification would carry the webhook secret to whoever answered on
	// that address.
	InsecureSSL string `json:"insecure_ssl"`
}

// hookName is the value GitHub requires in the `name` field for a webhook,
// which is a fixed string rather than a label.
//
// It reads like a name and is not one: GitHub's hooks API once served
// several service integrations and kept the field. "web" is the only value
// it accepts for a webhook, and there is nowhere to put a human label.
const hookName = "web"

func newHookBody(target, secret string) hookBody {
	return hookBody{
		Name: hookName, Active: true, Events: WebhookEvents,
		Config: hookConfig{
			URL: target, ContentType: "json", Secret: secret, InsecureSSL: "0",
		},
	}
}

// OrgWebhooks lists an organization's hooks.
func (c *Client) OrgWebhooks(ctx context.Context, org string) ([]Webhook, error) {
	return c.listWebhooks(ctx, "/orgs/"+org+"/hooks")
}

// RepoWebhooks lists a repository's hooks.
func (c *Client) RepoWebhooks(ctx context.Context, owner, repo string) ([]Webhook, error) {
	return c.listWebhooks(ctx, "/repos/"+owner+"/"+repo+"/hooks")
}

// hookPageSize is GitHub's per_page maximum.
//
// GitHub's documented 20-hooks limit is PER EVENT per target, not per target,
// so a full page is not evidence that the listing is complete — which is why
// this walks rather than trusting one request.
const hookPageSize = 100

// hookWalkCeiling stops a walk that is not converging. A target needing more
// than this many requests' worth of hooks is not one Crewlet provisions into.
//
// It is a non-convergence guard, not a hard cap, so it is compared with > and
// not >=: at >= a target holding EXACTLY this many hooks is refused by an
// error saying it has "more than" this many, when its next page is empty and
// the walk would have finished. The cost of the looser test is one extra page.
const hookWalkCeiling = 1000

// listWebhooks walks a target's hooks TO EXHAUSTION.
//
// One page was requested and whatever came back was taken as the whole set.
// Every caller uses this to decide whether Crewlet's own hook already exists,
// so a hook past the first page read as absent — and the reconcile then
// created a duplicate on every run, each delivering the same event again.
func (c *Client) listWebhooks(ctx context.Context, path string) ([]Webhook, error) {
	out := []Webhook{}
	for page := 1; ; page++ {
		var rows []hookWire
		params := url.Values{
			"per_page": {strconv.Itoa(hookPageSize)},
			"page":     {strconv.Itoa(page)},
		}
		if err := c.get(ctx, path, params, &rows); err != nil {
			return nil, err
		}
		for _, row := range rows {
			out = append(out, row.webhook())
		}
		if len(rows) < hookPageSize {
			return out, nil
		}
		if len(out) > hookWalkCeiling {
			return nil, fmt.Errorf(
				"github: %s has more than %d webhooks, which is not a target "+
					"Crewlet provisions into", path, hookWalkCeiling)
		}
	}
}

// CreateOrgWebhook registers one hook covering every repository in an
// organization.
func (c *Client) CreateOrgWebhook(ctx context.Context, org, target, secret string) (Webhook, error) {
	return c.createWebhook(ctx, "/orgs/"+org+"/hooks", target, secret)
}

// CreateRepoWebhook registers one repository's hook.
func (c *Client) CreateRepoWebhook(ctx context.Context, owner, repo, target, secret string) (Webhook, error) {
	return c.createWebhook(ctx, "/repos/"+owner+"/"+repo+"/hooks", target, secret)
}

func (c *Client) createWebhook(ctx context.Context, path, target, secret string) (Webhook, error) {
	var out hookWire
	if err := c.do(ctx, http.MethodPost, path, nil,
		newHookBody(target, secret), &out); err != nil {
		return Webhook{}, err
	}
	return out.webhook(), nil
}

// UpdateOrgWebhook converges an organization hook onto this build's events
// and secret.
func (c *Client) UpdateOrgWebhook(ctx context.Context, org string, id int64, target, secret string) (Webhook, error) {
	return c.updateWebhook(ctx, "/orgs/"+org+"/hooks/"+strconv.FormatInt(id, 10), target, secret)
}

// UpdateRepoWebhook converges a repository hook the same way.
func (c *Client) UpdateRepoWebhook(ctx context.Context, owner, repo string, id int64, target, secret string) (Webhook, error) {
	return c.updateWebhook(ctx,
		"/repos/"+owner+"/"+repo+"/hooks/"+strconv.FormatInt(id, 10), target, secret)
}

func (c *Client) updateWebhook(ctx context.Context, path, target, secret string) (Webhook, error) {
	// PATCH, and the whole config every time: GitHub replaces the config
	// object wholesale rather than merging it, so a patch that sent only
	// the events would clear the secret and leave a hook signing nothing.
	var out hookWire
	if err := c.do(ctx, http.MethodPatch, path, nil,
		newHookBody(target, secret), &out); err != nil {
		return Webhook{}, err
	}
	return out.webhook(), nil
}

// DeleteOrgWebhook removes an organization hook.
func (c *Client) DeleteOrgWebhook(ctx context.Context, org string, id int64) error {
	return c.do(ctx, http.MethodDelete,
		"/orgs/"+org+"/hooks/"+strconv.FormatInt(id, 10), nil, nil, nil)
}

// DeleteRepoWebhook removes a repository hook.
func (c *Client) DeleteRepoWebhook(ctx context.Context, owner, repo string, id int64) error {
	return c.do(ctx, http.MethodDelete,
		"/repos/"+owner+"/"+repo+"/hooks/"+strconv.FormatInt(id, 10), nil, nil, nil)
}

// Repo is a repository as the API reports it.
type Repo struct {
	FullName string `json:"full_name"`
	Private  bool   `json:"private"`
	Archived bool   `json:"archived"`
	// Permissions is what THIS credential may do, which is how a run
	// learns it cannot register a hook before it tries.
	Permissions struct {
		Admin bool `json:"admin"`
		Push  bool `json:"push"`
		Pull  bool `json:"pull"`
	} `json:"permissions"`
}

// RepoOf reads one repository.
func (c *Client) RepoOf(ctx context.Context, owner, repo string) (Repo, error) {
	var out Repo
	if err := c.get(ctx, "/repos/"+owner+"/"+repo, nil, &out); err != nil {
		return Repo{}, err
	}
	return out, nil
}
