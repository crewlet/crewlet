package googlecloud

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/googleauth"
)

// Google's API hosts. Fixed rather than configurable, so the provisioner's
// token only ever goes to Google. The token hosts are [googleauth]'s.
const (
	iamAPI            = "https://iam.googleapis.com/v1"
	resourceManagerV1 = "https://cloudresourcemanager.googleapis.com/v1"
	resourceManagerV3 = "https://cloudresourcemanager.googleapis.com/v3"
	serviceUsageAPI   = "https://serviceusage.googleapis.com/v1"
	cloudAssetAPI     = "https://cloudasset.googleapis.com/v1"
)

// Paging: listings ask for pageSize rows and stop after pageCeiling pages with
// an error, since a short read would read as rows that do not exist.
const (
	pageSize    = 100
	pageCeiling = 100
)

// policyWrites bounds the re-reads of a policy somebody else wrote between
// this client's read and its write (409): a busy policy settles in one or two.
const policyWrites = 5

// gone reports a read or a delete of something Google no longer has.
func gone(err error) bool { return googleauth.Status(err) == http.StatusNotFound }

// accountNotYet reports Google refusing a binding because email's account is
// not visible to the policy yet: the 400 a new account answers on another
// project's policy for up to minutes after it was made. Only that account's.
func accountNotYet(err error, email string) bool {
	api, ok := errors.AsType[*googleauth.APIError](err)
	return ok && email != "" && api.Status == http.StatusBadRequest &&
		strings.Contains(api.Message, email) && strings.Contains(api.Message, "does not exist")
}

// accountQuota reports a create refused because the identity project holds as
// many service accounts as its quota allows.
func accountQuota(err error) bool {
	api, ok := errors.AsType[*googleauth.APIError](err)
	return ok && (api.Status == http.StatusTooManyRequests || api.Code == "RESOURCE_EXHAUSTED" ||
		strings.Contains(strings.ToLower(api.Message), "maximum number of service accounts"))
}

// domainRestricted reports a grant the organization's domain restricted
// sharing policy refused for the agent's account.
func domainRestricted(err error) bool {
	api, ok := errors.AsType[*googleauth.APIError](err)
	return ok && (strings.Contains(api.Message, "allowedPolicyMember") ||
		strings.Contains(api.Message, "do not belong to a permitted customer"))
}

// Client talks to Google as the provisioner, through the company's Google
// identity ([googleauth.Connection]).
type Client struct {
	auth *googleauth.Connection
}

// NewClient builds a client, refusing an input that would fail every call
// with a misleading refusal ([googleauth.New]).
func NewClient(opts googleauth.Options) (*Client, error) {
	auth, err := googleauth.New(opts)
	if err != nil {
		return nil, err
	}
	return &Client{auth: auth}, nil
}

// Connection is the company's Google identity this client reaches Google
// through.
func (c *Client) Connection() *googleauth.Connection { return c.auth }

// clock is the time this client reads and waits by.
func (c *Client) clock() googleauth.Clock { return c.auth.Clock() }

// setClock makes this client read and wait by clk, for a test.
func (c *Client) setClock(clk googleauth.Clock) { c.auth = c.auth.WithClock(clk) }

// Provisioner is the provisioner's email.
func (c *Client) Provisioner() string { return c.auth.Provisioner() }

// AtlasAccount is the atlas's service account: crewlet-atlas in the identity
// project, which the identity block makes where the company asks for it.
func (c *Client) AtlasAccount() string { return AtlasAccountIn(c.auth.IdentityProject()) }

// IdentityProject is the project the agents' accounts are made in: the
// provisioner's own, which holds nothing else.
func (c *Client) IdentityProject() string { return c.auth.IdentityProject() }

// member is a service account as a policy binding names it.
func member(email string) string { return "serviceAccount:" + email }

// call is one request as the provisioner.
func (c *Client) call(ctx context.Context, method, address string, body, out any) error {
	return c.auth.CallAs(ctx, c.auth.Provisioner(), method, address, body, out)
}

// ServiceAccount is one service account, as Google reports it.
type ServiceAccount struct {
	Name        string `json:"name"`
	ProjectID   string `json:"projectId"`
	UniqueID    string `json:"uniqueId"`
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
	Disabled    bool   `json:"disabled"`
}

// accountURL is one account of the identity project, by email or unique ID.
func (c *Client) accountURL(account string) string {
	return iamAPI + "/projects/" + url.PathEscape(c.auth.IdentityProject()) + "/serviceAccounts/" + url.PathEscape(account)
}

// ServiceAccount reads one account of the identity project by its email or
// unique ID, and false where Google has none.
func (c *Client) ServiceAccount(ctx context.Context, account string) (ServiceAccount, bool, error) {
	var sa ServiceAccount
	err := c.call(ctx, http.MethodGet, c.accountURL(account), nil, &sa)
	switch {
	case gone(err):
		return ServiceAccount{}, false, nil
	case err != nil:
		return ServiceAccount{}, false, err
	}
	return sa, true, nil
}

// ServiceAccounts is every account of the identity project.
func (c *Client) ServiceAccounts(ctx context.Context) ([]ServiceAccount, error) {
	base := iamAPI + "/projects/" + url.PathEscape(c.auth.IdentityProject()) + "/serviceAccounts"
	return pages(ctx, c, base, url.Values{}, func(page json.RawMessage) ([]ServiceAccount, string, error) {
		var resp struct {
			Accounts []ServiceAccount `json:"accounts"`
			Next     string           `json:"nextPageToken"`
		}
		err := json.Unmarshal(page, &resp)
		return resp.Accounts, resp.Next, err
	})
}

// CreateServiceAccount makes an account in the identity project.
func (c *Client) CreateServiceAccount(ctx context.Context, id, displayName, description string) (ServiceAccount, error) {
	var sa ServiceAccount
	err := c.call(ctx, http.MethodPost, iamAPI+"/projects/"+url.PathEscape(c.auth.IdentityProject())+"/serviceAccounts",
		map[string]any{"accountId": id, "serviceAccount": map[string]string{
			"displayName": displayName, "description": description,
		}}, &sa)
	if err == nil && (sa.UniqueID == "" || sa.Email == "") {
		return ServiceAccount{}, errors.New("googlecloud: the create answered with no unique ID or email")
	}
	return sa, err
}

// DisableServiceAccount turns an account off: Google refuses every token it
// holds from then on.
func (c *Client) DisableServiceAccount(ctx context.Context, account string) error {
	return c.call(ctx, http.MethodPost, c.accountURL(account)+":disable", map[string]any{}, nil)
}

// EnableServiceAccount turns an account back on, with its roles as they were.
func (c *Client) EnableServiceAccount(ctx context.Context, account string) error {
	return c.call(ctx, http.MethodPost, c.accountURL(account)+":enable", map[string]any{}, nil)
}

// DeleteServiceAccount deletes an account; one already gone is the state
// asked for.
func (c *Client) DeleteServiceAccount(ctx context.Context, account string) error {
	if err := c.call(ctx, http.MethodDelete, c.accountURL(account), nil, nil); err != nil && !gone(err) {
		return err
	}
	return nil
}

// AccountPolicy is the allow policy on one account of the identity project:
// who may act as it.
func (c *Client) AccountPolicy(ctx context.Context, account string) (Policy, error) {
	var pol Policy
	err := c.call(ctx, http.MethodPost, c.accountURL(account)+":getIamPolicy", map[string]any{}, &pol)
	return pol, err
}

// EditAccountPolicy applies edit to an account's policy, re-reading where
// somebody else wrote it in between, and writes only where edit changed it.
// The version is written back as read, so a condition on it survives.
func (c *Client) EditAccountPolicy(ctx context.Context, account string, edit func(*Policy) bool) error {
	return c.editPolicy(ctx, account, func(ctx context.Context) (Policy, error) {
		return c.AccountPolicy(ctx, account)
	}, func(ctx context.Context, pol Policy) error {
		return c.call(ctx, http.MethodPost, c.accountURL(account)+":setIamPolicy", map[string]any{"policy": pol}, nil)
	}, edit)
}

// ProjectPolicy is one project's allow policy, at version 3 so its
// conditions, the provisioner's own included, are read whole.
func (c *Client) ProjectPolicy(ctx context.Context, project string) (Policy, error) {
	var pol Policy
	err := c.call(ctx, http.MethodPost, resourceManagerV1+"/projects/"+url.PathEscape(project)+":getIamPolicy",
		map[string]any{"options": map[string]int{"requestedPolicyVersion": 3}}, &pol)
	return pol, err
}

// EditProjectPolicy applies edit to a project's policy, re-reading where
// somebody else wrote it in between, and writes only where edit changed it.
//
// ALWAYS VERSION 3 WITH THE ETAG: a write that omits the version may replace a
// version 3 policy with version 1, dropping every condition, the
// provisioner's own containment included.
func (c *Client) EditProjectPolicy(ctx context.Context, project string, edit func(*Policy) bool) error {
	return c.editPolicy(ctx, project, func(ctx context.Context) (Policy, error) {
		return c.ProjectPolicy(ctx, project)
	}, func(ctx context.Context, pol Policy) error {
		pol.Version = 3
		return c.call(ctx, http.MethodPost, resourceManagerV1+"/projects/"+url.PathEscape(project)+":setIamPolicy",
			map[string]any{"policy": pol}, nil)
	}, edit)
}

// editPolicy is one read-modify-write of a policy, retried on 409.
func (c *Client) editPolicy(
	ctx context.Context, resource string,
	read func(context.Context) (Policy, error), write func(context.Context, Policy) error, edit func(*Policy) bool,
) error {
	for range policyWrites {
		pol, err := read(ctx)
		if err != nil {
			return err
		}
		if !edit(&pol) {
			return nil
		}
		err = write(ctx, pol)
		if googleauth.Status(err) == http.StatusConflict {
			continue
		}
		return err
	}
	return fmt.Errorf("googlecloud: the policy of %s kept changing under %d writes", resource, policyWrites)
}

// TestProjectPermissions is which of perms the provisioner holds on project.
func (c *Client) TestProjectPermissions(ctx context.Context, project string, perms []string) ([]string, error) {
	var out struct {
		Permissions []string `json:"permissions"`
	}
	err := c.call(ctx, http.MethodPost, resourceManagerV1+"/projects/"+url.PathEscape(project)+":testIamPermissions",
		map[string]any{"permissions": perms}, &out)
	return out.Permissions, err
}

// Project is one project the provisioner can read.
type Project struct {
	ProjectID   string `json:"projectId"`
	DisplayName string `json:"displayName"`
	State       string `json:"state"`
	// Parent is `organizations/<id>` or `folders/<id>`, empty for a project
	// in no organization.
	Parent string `json:"parent"`
}

// SearchProjects is every project the provisioner can read, active or not.
func (c *Client) SearchProjects(ctx context.Context) ([]Project, error) {
	return pages(ctx, c, resourceManagerV3+"/projects:search", url.Values{},
		func(page json.RawMessage) ([]Project, string, error) {
			var resp struct {
				Projects []Project `json:"projects"`
				Next     string    `json:"nextPageToken"`
			}
			err := json.Unmarshal(page, &resp)
			return resp.Projects, resp.Next, err
		})
}

// Role is one IAM role, predefined or a project's custom role.
type Role struct {
	Name        string   `json:"name"`
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Stage       string   `json:"stage"`
	Permissions []string `json:"includedPermissions"`
	Deleted     bool     `json:"deleted"`
}

// Role reads one role by its name (`roles/browser`,
// `projects/<id>/roles/crewletOperator`), and false where Google has none.
func (c *Client) Role(ctx context.Context, name string) (Role, bool, error) {
	var r Role
	err := c.call(ctx, http.MethodGet, iamAPI+"/"+name, nil, &r)
	switch {
	case gone(err):
		return Role{}, false, nil
	case err != nil:
		return Role{}, false, err
	}
	return r, true, nil
}

// EnabledServices is every API enabled on project, by its service name.
func (c *Client) EnabledServices(ctx context.Context, project string) (map[string]bool, error) {
	rows, err := pages(ctx, c, serviceUsageAPI+"/projects/"+url.PathEscape(project)+"/services",
		url.Values{"filter": {"state:ENABLED"}}, func(page json.RawMessage) ([]string, string, error) {
			var resp struct {
				Services []struct {
					Config struct {
						Name string `json:"name"`
					} `json:"config"`
				} `json:"services"`
				Next string `json:"nextPageToken"`
			}
			if err := json.Unmarshal(page, &resp); err != nil {
				return nil, "", err
			}
			names := make([]string, 0, len(resp.Services))
			for _, s := range resp.Services {
				names = append(names, s.Config.Name)
			}
			return names, resp.Next, nil
		})
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(rows))
	for _, name := range rows {
		out[name] = true
	}
	return out, nil
}

// enableBatch is the most services one batchEnable call names.
const enableBatch = 20

// EnableServices turns on each of names on project, enableBatch at a time.
// Google answers an operation; a caller reads the services back rather than
// polling it, since a read-back is what the next step waits on anyway.
func (c *Client) EnableServices(ctx context.Context, project string, names []string) error {
	for batch := range slices.Chunk(names, enableBatch) {
		var op struct {
			Done  bool `json:"done"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := c.call(ctx, http.MethodPost, serviceUsageAPI+"/projects/"+url.PathEscape(project)+"/services:batchEnable",
			map[string]any{"serviceIds": batch}, &op); err != nil {
			return err
		}
		if op.Done && op.Error != nil {
			return fmt.Errorf("googlecloud: turn on %s: %s", strings.Join(batch, ", "), op.Error.Message)
		}
	}
	return nil
}

// Asset is one resource Cloud Asset Inventory lists, as the atlas keeps it:
// its full resource name, its type, its name, where it is, its state, and
// for an instance its addresses. Nothing else of the answer is decoded.
type Asset struct {
	Name        string
	Type        string
	DisplayName string
	Location    string
	State       string
	Addresses   []string
}

// assetReadMask is every field a search asks for. THE MASK IS EXPLICIT AND
// CLOSED: unset, Google answers labels, tags and descriptions, and with
// versionedResources a resource's whole configuration, which for a function
// or a service holds its environment.
const assetReadMask = "name,assetType,displayName,location,state,additionalAttributes"

// assetPageSize is the most results Google answers a search page with.
const assetPageSize = 500

// SearchAssets is every resource of assetTypes in project, searched as the
// atlas's account. A project the account may not search is a 403.
func (c *Client) SearchAssets(ctx context.Context, project string, assetTypes []string) ([]Asset, error) {
	query := url.Values{"assetTypes": assetTypes, "readMask": {assetReadMask}}
	return pagesAs(ctx, c, c.AtlasAccount(), cloudAssetAPI+"/projects/"+url.PathEscape(project)+":searchAllResources",
		query, assetPageSize, func(page json.RawMessage) ([]Asset, string, error) {
			var resp struct {
				Results []struct {
					Name        string `json:"name"`
					AssetType   string `json:"assetType"`
					DisplayName string `json:"displayName"`
					Location    string `json:"location"`
					State       string `json:"state"`
					// ONLY TWO ATTRIBUTES ARE DECODED, and only as lists of
					// text: an instance's internal and external addresses.
					Attributes struct {
						InternalIPs []string `json:"internalIPs"`
						ExternalIPs []string `json:"externalIPs"`
					} `json:"additionalAttributes"`
				} `json:"results"`
				Next string `json:"nextPageToken"`
			}
			if err := json.Unmarshal(page, &resp); err != nil {
				return nil, "", err
			}
			out := make([]Asset, 0, len(resp.Results))
			for _, r := range resp.Results {
				a := Asset{Name: r.Name, Type: r.AssetType, DisplayName: r.DisplayName, Location: r.Location,
					State: r.State}
				if r.AssetType == AssetInstance {
					a.Addresses = append(append(a.Addresses, r.Attributes.InternalIPs...), r.Attributes.ExternalIPs...)
				}
				out = append(out, a)
			}
			return out, resp.Next, nil
		})
}

// pages reads a paged listing to its end, each page decoded by decode into its
// rows and the next page's token.
func pages[T any](
	ctx context.Context, c *Client, base string, query url.Values,
	decode func(json.RawMessage) ([]T, string, error),
) ([]T, error) {
	return pagesAs(ctx, c, c.auth.Provisioner(), base, query, pageSize, decode)
}

// pagesAs is [pages] as account, size rows a page.
func pagesAs[T any](
	ctx context.Context, c *Client, account, base string, query url.Values, size int,
	decode func(json.RawMessage) ([]T, string, error),
) ([]T, error) {
	var out []T
	next := ""
	for page := 0; ; page++ {
		if page == pageCeiling {
			return nil, fmt.Errorf("googlecloud: %s did not finish after %d pages", googleauth.PathOf(base), pageCeiling)
		}
		q := url.Values{}
		for k, v := range query {
			q[k] = v
		}
		q.Set("pageSize", strconv.Itoa(size))
		if next != "" {
			q.Set("pageToken", next)
		}
		var raw json.RawMessage
		if err := c.auth.CallAs(ctx, account, http.MethodGet, base+"?"+q.Encode(), nil, &raw); err != nil {
			return nil, err
		}
		if len(raw) == 0 {
			// AN EMPTY ANSWER IS AN EMPTY LISTING, which Google gives
			// a project with no row as `{}` or nothing at all.
			return out, nil
		}
		rows, token, err := decode(raw)
		if err != nil {
			return nil, fmt.Errorf("googlecloud: decode %s: %w", googleauth.PathOf(base), err)
		}
		out = append(out, rows...)
		if token == "" {
			return out, nil
		}
		next = token
	}
}
