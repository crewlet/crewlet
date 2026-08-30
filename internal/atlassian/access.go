package atlassian

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// The PRODUCT plane: what an agent can actually do, asked AS the agent.
//
// # Asked as it, never about it
//
// Both products report the CALLER's own access, and that is the only reading
// that accounts for every permission scheme, project role, group grant and
// space restriction at once. Asking the admin plane "what does this account
// hold in ENG" has no answer — the admin plane does not know about projects.
//
// # Every call here is a GET
//
// Crewlet does not place an agent in a project or a space: adding an account
// to a project role or a space permission is refused to an API token outright
// and available only to a Forge app on a paid plan. What this package can do
// is say TRUTHFULLY what access an agent ended up with, so an operator is
// never guessing — and reporting a fact it cannot change is a better answer
// than a write that silently does nothing.

var (
	// ErrContainerNotVisible means the agent cannot see the project or
	// space. Both products hide what a caller may not read, so one that
	// does not exist and one the agent has no access to answer alike — and
	// they are the same problem to an operator anyway.
	ErrContainerNotVisible = errors.New("the project or space does not exist, or this agent cannot see it")

	// ErrCredentialRefused means the product refused the agent's own
	// credential.
	ErrCredentialRefused = errors.New("the agent's credential was refused")
)

// groupPageSize is how many groups one membership page carries. Confluence's
// own maximum for the endpoint.
const groupPageSize = 200

// ProductClient reads one product as one agent.
type ProductClient struct {
	t       *Transport
	product Product
}

// NewProductClient builds a reader for one product on one site, as one
// agent's credential.
//
// The gateway is passed in rather than assumed so a test can serve both
// planes from one server, and so a run against a Cloud site behind Atlassian's
// gateway and a run against the site's own host are the same code.
func NewProductClient(gateway string, product Product, cloudID string, cred Credential, client *http.Client) (*ProductClient, error) {
	if !product.Valid() {
		return nil, fmt.Errorf("atlassian: unknown product %q — it is one of %s",
			product, strings.Join(ProductNames(), ", "))
	}
	if strings.TrimSpace(cloudID) == "" {
		return nil, errors.New("atlassian: no cloud id, so there is no site to read")
	}
	t, err := NewTransport(string(product),
		ProductBase(gateway, product, cloudID),
		AuthHeader(strings.TrimSpace(cred.Email), strings.TrimSpace(cred.Token)),
		client)
	if err != nil {
		return nil, err
	}
	return &ProductClient{t: t, product: product}, nil
}

// read runs one product GET and maps the two refusals a caller acts on.
func (c *ProductClient) read(ctx context.Context, path string, params url.Values, out any) error {
	err := c.t.Do(ctx, http.MethodGet, path, params, nil, out)
	switch StatusOf(err) {
	case http.StatusUnauthorized, http.StatusForbidden:
		return fmt.Errorf("%w: %w", ErrCredentialRefused, err)
	case http.StatusNotFound:
		return fmt.Errorf("%w: %w", ErrContainerNotVisible, err)
	}
	return err
}

// Me is the account id this credential authenticates as, in this product.
//
// # It is asked per PRODUCT, and that is not redundant
//
// On Cloud both products answer the same account id, so a second call
// confirms what the first said. It is asked anyway, because the answer is not
// only the id: a credential minted with Jira scopes only is REFUSED by
// Confluence, and that refusal is the one signal that a seat's products have
// grown since its token was minted. Token scopes cannot be read back from
// Atlassian at all, so exercising the credential is the only test there is —
// and it tests the thing rather than a remembered claim about it.
//
// On Data Center the two answer different things (Jira's `name`, Confluence's
// `userKey`), which is the other reason this is per product: registering one
// product's answer under the other's namespace is a misroute nothing detects.
func (c *ProductClient) Me(ctx context.Context) (string, error) {
	switch c.product {
	case ProductConfluence:
		var out struct {
			AccountID string `json:"accountId"`
			Username  string `json:"username"`
			UserKey   string `json:"userKey"`
		}
		if err := c.read(ctx, "/user/current", nil, &out); err != nil {
			return "", err
		}
		return firstOf(out.AccountID, out.Username, out.UserKey), nil
	default:
		var out struct {
			AccountID string `json:"accountId"`
			Name      string `json:"name"`
		}
		if err := c.read(ctx, "/myself", nil, &out); err != nil {
			return "", err
		}
		return firstOf(out.AccountID, out.Name), nil
	}
}

// PermissionsIn reports what this agent may do in one container.
//
// accountID is the agent's own, needed only by Confluence — see
// [ProductClient.spacePermissions].
func (c *ProductClient) PermissionsIn(ctx context.Context, container, accountID string) (map[string]bool, error) {
	if c.product == ProductConfluence {
		return c.spacePermissions(ctx, container, accountID)
	}
	var out struct {
		Permissions map[string]struct {
			HavePermission bool `json:"havePermission"`
		} `json:"permissions"`
	}
	params := url.Values{
		"projectKey":  {container},
		"permissions": {strings.Join(PermissionQuery(ProductJira), ",")},
	}
	if err := c.read(ctx, "/mypermissions", params, &out); err != nil {
		return nil, err
	}
	held := make(map[string]bool, len(out.Permissions))
	for name, p := range out.Permissions {
		held[name] = p.HavePermission
	}
	return held, nil
}

// spacePermissions reads back what this agent holds on one space.
//
// # Confluence has no mypermissions, and the substitute has a trap in it
//
// The space's own permission list is read instead and filtered to this agent.
// Filtering has to cover BOTH halves of how a principal gets access: granted
// to them by name, and granted to a GROUP they belong to. Almost every real
// grant is the second kind, so matching only the account id reports an agent
// that works perfectly as having no access at all — and the operator's next
// move is to grant permissions it already had.
func (c *ProductClient) spacePermissions(ctx context.Context, spaceKey, accountID string) (map[string]bool, error) {
	groups, err := c.groups(ctx, accountID)
	if err != nil {
		return nil, err
	}
	var out struct {
		Permissions []struct {
			// Reading and writing disagree: a grant is POSTED as key and
			// target, and read back as operation and targetType.
			Operation struct {
				Operation  string `json:"operation"`
				TargetType string `json:"targetType"`
			} `json:"operation"`
			Subjects struct {
				User struct {
					Results []struct {
						AccountID string `json:"accountId"`
					} `json:"results"`
				} `json:"user"`
				Group struct {
					Results []struct {
						Name string `json:"name"`
					} `json:"results"`
				} `json:"group"`
			} `json:"subjects"`
		} `json:"permissions"`
	}
	params := url.Values{"expand": {"permissions"}}
	if err := c.read(ctx, "/space/"+url.PathEscape(spaceKey), params, &out); err != nil {
		return nil, err
	}

	// The space lists EVERY principal's grants, so each is matched to this
	// agent: a colleague's permission is not the agent's.
	held := map[string]bool{}
	for _, entry := range out.Permissions {
		mine := false
		for _, user := range entry.Subjects.User.Results {
			if user.AccountID == accountID {
				mine = true
				break
			}
		}
		if !mine {
			for _, group := range entry.Subjects.Group.Results {
				if groups[group.Name] {
					mine = true
					break
				}
			}
		}
		if mine {
			held[entry.Operation.Operation+":"+entry.Operation.TargetType] = true
		}
	}
	return held, nil
}

// groups names the groups this agent belongs to.
//
// Asked AS the agent rather than of the directory, so it needs no admin
// credential. The account is named explicitly because the endpoint requires
// it: without one it answers 400, and it rejects the older username and
// userkey spellings outright.
func (c *ProductClient) groups(ctx context.Context, accountID string) (map[string]bool, error) {
	if strings.TrimSpace(accountID) == "" {
		return nil, errors.New(
			"atlassian: no account id, so this agent's group memberships " +
				"cannot be read — and almost every space permission is granted " +
				"to a group rather than to an account")
	}
	groups := map[string]bool{}
	start := 0
	for range listPageLimit {
		var out struct {
			Results []struct {
				Name string `json:"name"`
			} `json:"results"`
		}
		params := url.Values{
			"accountId": {accountID},
			"limit":     {strconv.Itoa(groupPageSize)},
			"start":     {strconv.Itoa(start)},
		}
		if err := c.read(ctx, "/user/memberof", params, &out); err != nil {
			return nil, err
		}
		for _, group := range out.Results {
			groups[group.Name] = true
		}
		if len(out.Results) < groupPageSize {
			return groups, nil
		}
		start += len(out.Results)
	}
	// Stopping short would drop a group and report permissions the agent
	// HOLDS as missing, which is the bug this call exists to prevent.
	return nil, fmt.Errorf(
		"%w: the group listing did not end after %d pages", ErrUnexpected, listPageLimit)
}

// Container styles. What somebody changes to widen or narrow access differs
// per style, so a report has to say which it is before its link is useful.
const (
	// StyleTeamManaged is a Jira project whose access is one setting on the
	// project itself.
	StyleTeamManaged = "team-managed"
	// StyleCompanyManaged is a Jira project governed by a permission
	// scheme, which is shared and usually applies to other projects too —
	// so widening it for this agent widens it for them as well.
	StyleCompanyManaged = "company-managed"
	// StyleSpace is a Confluence space, whose access is a grid of
	// permissions per person and group.
	StyleSpace = "space"
)

// Settings is where and how a person changes who can do what in one container.
type Settings struct {
	URL   string
	Style string
}

// jiraRoutes map a project's type to the section of Jira that owns its access
// settings. The route differs per type and a wrong guess lands somebody on an
// error page, so it is READ from the project rather than assumed.
var jiraRoutes = map[string]string{
	"software":     "software",
	"business":     "core",
	"service_desk": "servicedesk",
}

// SettingsFor resolves where a container's access is changed.
//
// It is the end of every permission report that names a problem, so it is
// worth being exact about: the route differs per project type and a guess
// lands an operator on an error page, having told them the link was the fix.
//
// Called only when there IS something to fix — a clean container has nobody
// to send anywhere, and asking anyway would spend one request per container
// per run to produce a link nothing prints.
func (c *ProductClient) SettingsFor(ctx context.Context, siteURL, container string) (Settings, error) {
	siteURL = strings.TrimRight(strings.TrimSpace(siteURL), "/")
	if siteURL == "" {
		// The API gateway is not a place a browser can go, so a link built
		// from it looks right and opens nothing. No link is the honest
		// answer; the report says which container instead.
		return Settings{Style: c.style()}, nil
	}
	if c.product == ProductConfluence {
		return Settings{
			URL:   siteURL + "/wiki/spaces/" + container + "/settings/permissions",
			Style: StyleSpace,
		}, nil
	}
	var out struct {
		ProjectTypeKey string `json:"projectTypeKey"`
		// Atlassian still calls a team-managed project next-gen on the
		// wire, years after renaming it everywhere a person can see.
		Style string `json:"style"`
	}
	if err := c.read(ctx, "/project/"+url.PathEscape(container), nil, &out); err != nil {
		return Settings{}, err
	}
	route, ok := jiraRoutes[out.ProjectTypeKey]
	if !ok {
		return Settings{}, fmt.Errorf("%w: unknown Jira project type %q",
			ErrUnexpected, out.ProjectTypeKey)
	}
	style := StyleCompanyManaged
	if out.Style == "next-gen" {
		style = StyleTeamManaged
	}
	return Settings{
		URL:   siteURL + "/jira/" + route + "/projects/" + container + "/settings/access",
		Style: style,
	}, nil
}

// style is the container style this product always has, for the case where
// there is no site URL to build a link from.
func (c *ProductClient) style() string {
	if c.product == ProductConfluence {
		return StyleSpace
	}
	return ""
}

// firstOf is the first non-empty string, which is how both products' identity
// payloads are read: which field is populated is the deployment's choice, and
// a reader that knew only one would come back empty on the other — silently,
// because "nobody" is a legitimate answer.
func firstOf(values ...string) string {
	for _, v := range values {
		if v = strings.TrimSpace(v); v != "" {
			return v
		}
	}
	return ""
}
