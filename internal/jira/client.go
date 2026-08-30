package jira

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/crewlet/crewlet/internal/atlassian"
)

// The REST client.
//
// ONE credential, the engine's — the org-wide read account named by
// integrations.jira. A seat's own Jira work happens through its MCP server
// under its own token; what the engine reads here is what ROUTING needs and
// the payload does not carry: who is watching an issue.
//
// # Two deployments, one client
//
// Jira Cloud serves /rest/api/3 and names people by accountId. Jira Data
// Center serves /rest/api/2 — v3 is a 404 there — and names them by name.
// Asking an operator to declare which they run would be a knob whose only
// honest answer is already in the address they gave, so it is DERIVED: a
// cloud id, or a host under Atlassian's own domains, is Cloud; anything else
// is Data Center. [ClientOptions.Deployment] overrides the derivation for the
// one case it cannot see — a Cloud site behind a vanity domain.

// Deployment is which Jira an address names.
type Deployment string

const (
	// Cloud is Atlassian-hosted Jira: /rest/api/3, accountId identities.
	Cloud Deployment = "cloud"
	// DataCenter is self-hosted Jira Data Center or Server:
	// /rest/api/2, username identities.
	DataCenter Deployment = "data-center"
)

// Valid reports whether the deployment is one this build speaks.
func (d Deployment) Valid() bool { return d == Cloud || d == DataCenter }

// APIPath is the REST prefix this deployment serves.
//
// Cloud for the zero value, which is what [DeploymentOf] answers for an
// address it cannot place: a v3 call against Data Center 404s with the
// version in the path, which names the problem, while a v2 call against
// Cloud succeeds against a DEPRECATED surface that returns different
// identity fields — a silent misroute rather than a loud miss.
func (d Deployment) APIPath() string {
	if d == DataCenter {
		return "/rest/api/2"
	}
	return "/rest/api/3"
}

// DeploymentOf derives the deployment from a base address.
//
// The host list is [atlassian.CloudHosts], shared with the knowledge base,
// because the two had drifted: this one carried .atlassian.com and
// Confluence's did not, so the same Cloud gateway address was Cloud to the
// tracker and Data Center to the wiki — which selects a different REST
// version and a different identity field on one product only.
func DeploymentOf(base string) Deployment {
	if atlassian.IsCloud(base) {
		return Cloud
	}
	return DataCenter
}

// ClientTimeout bounds one request. Shared with the knowledge base and the
// provisioner: they talk to the same host, on the same terms.
const ClientTimeout = atlassian.ClientTimeout

// Client is one authenticated Jira session.
type Client struct {
	t      *atlassian.Transport
	deploy Deployment
}

// ClientOptions configure a [Client].
type ClientOptions struct {
	// URL is the REST base: a Cloud gateway, or an instance address.
	URL string

	// Email switches authentication to Basic base64(email:token), which
	// is what Cloud requires. Empty sends a bearer token, which is what a
	// Data Center PAT and a service account want.
	Email string

	// Token is the API token or PAT.
	Token string

	// Deployment overrides the derivation from URL. The zero value
	// derives, which is right for every address that names itself.
	Deployment Deployment

	HTTP *http.Client
}

// NewClient builds a client.
func NewClient(opts ClientOptions) (*Client, error) {
	base := strings.TrimRight(strings.TrimSpace(opts.URL), "/")
	if base == "" {
		return nil, fmt.Errorf("jira: no instance url")
	}
	token := strings.TrimSpace(opts.Token)
	if token == "" {
		return nil, fmt.Errorf("jira: no api token")
	}
	deploy := opts.Deployment
	if !deploy.Valid() {
		deploy = DeploymentOf(base)
	}
	t, err := atlassian.NewTransport("jira", base,
		atlassian.AuthHeader(strings.TrimSpace(opts.Email), token), opts.HTTP)
	if err != nil {
		return nil, err
	}
	return &Client{t: t, deploy: deploy}, nil
}

// URL is the REST base this client reads.
func (c *Client) URL() string { return c.t.Base }

// Deployment is which Jira this client speaks to.
func (c *Client) Deployment() Deployment { return c.deploy }

// APIError is a refusal from the instance, typed.
//
// An alias rather than a type of its own: one Atlassian account reaches Jira,
// Confluence and the organization admin API, and a caller that has to tell
// "this credential cannot see it" from "the credential is wrong" should not
// need three errors.As branches to do it.
type APIError = atlassian.APIError

// do runs one request against a path already carrying its own prefix.
func (c *Client) do(ctx context.Context, method, path string, params url.Values, in, out any) error {
	return c.t.Do(ctx, method, path, params, in, out)
}

// api runs a request against the deployment's versioned REST surface.
func (c *Client) api(ctx context.Context, method, path string, params url.Values, in, out any) error {
	return c.do(ctx, method, c.deploy.APIPath()+path, params, in, out)
}

// user is the shape a person arrives in on both deployments.
//
// BOTH identity fields, because which one is populated is the deployment's
// choice and a reader that knew only one would come back empty on the other —
// silently, since "nobody" is a legitimate answer for an unassigned issue.
type user struct {
	AccountID   string `json:"accountId"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Email       string `json:"emailAddress"`
}

// id is the identity this deployment routes by.
func (u user) id() string { return firstOf(u.AccountID, u.Name) }

// Me is the identity this credential authenticates as.
//
// The boot identity check, and the whole of a seat's inbound routing: a Jira
// payload names people by account id, and nothing in the org model says
// which account a seat holds. A token that resolves proves the credential
// works AND names the account, which is what stops the engine registering a
// seat under an id somebody assumed.
func (c *Client) Me(ctx context.Context) (string, error) {
	var out user
	if err := c.api(ctx, http.MethodGet, "/myself", nil, nil, &out); err != nil {
		return "", err
	}
	return out.id(), nil
}

// WatchersOf implements [Watchers]: the accounts following an issue.
//
// ONE call, never a walk: Jira returns the whole watcher list in one
// response, and an issue with enough watchers to page is one where waking
// all of them is the wrong behaviour anyway.
func (c *Client) WatchersOf(ctx context.Context, issueKey string) ([]string, error) {
	if strings.TrimSpace(issueKey) == "" {
		return nil, fmt.Errorf("jira: no issue key")
	}
	var out struct {
		Watchers []user `json:"watchers"`
	}
	path := "/issue/" + url.PathEscape(issueKey) + "/watchers"
	if err := c.api(ctx, http.MethodGet, path, nil, nil, &out); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(out.Watchers))
	for _, w := range out.Watchers {
		if id := w.id(); id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// Project is one project as the reconcile reads it.
type Project struct {
	ID   string
	Key  string
	Name string
	// Lead is the account Jira itself considers the project's owner. The
	// reconcile compares it against the org's lead for the same project,
	// because a disagreement is a routing split nothing else reports:
	// Jira's own notifications go one way and the engine's go the other.
	Lead     string
	LeadName string
}

// ProjectOf reads one project by key.
func (c *Client) ProjectOf(ctx context.Context, key string) (Project, error) {
	if strings.TrimSpace(key) == "" {
		return Project{}, fmt.Errorf("jira: no project key")
	}
	var out struct {
		ID   string `json:"id"`
		Key  string `json:"key"`
		Name string `json:"name"`
		Lead user   `json:"lead"`
	}
	path := "/project/" + url.PathEscape(key)
	if err := c.api(ctx, http.MethodGet, path, nil, nil, &out); err != nil {
		return Project{}, err
	}
	return Project{
		ID: out.ID, Key: out.Key, Name: out.Name,
		Lead: out.Lead.id(), LeadName: out.Lead.DisplayName,
	}, nil
}
