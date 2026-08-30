package atlassian

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// The ADMIN plane: the organization's own API key, and everything it can do
// that no product credential can.
//
// # The routes are not all in Atlassian's reference, and that is stated
//
// Two of them were read off the admin console's own traffic rather than a
// published document: the licence grant (`.../service-accounts/invite`) and
// the token mint (`/users/{id}/manage/api-tokens`). Atlassian's reference
// documents its token endpoints as read-and-revoke only, and documents
// service accounts under a DIFFERENT service (`api-access`) that answers the
// collection with "Request failed to match any route".
//
// They are used anyway, because the alternative is asking an operator to
// create and paste one credential per agent by hand, for ever. What that
// costs is honesty about it: every refusal below is a NAMED error rather than
// a status, the docs page says plainly that these are console-derived, and a
// withdrawal shows up as a refusal an operator can read rather than a
// half-provisioned company.

// AdminBaseURL is Atlassian's organization admin API.
const AdminBaseURL = "https://api.atlassian.com"

// serviceAccountsPath is the collection this integration lives on.
//
// Note the service: account-management, not api-access. api-access is what
// Atlassian's reference documents service accounts under, and it serves only
// /count — the collection answers "Request failed to match any route".
// account-management is what the admin console itself calls.
//
// The two also take different credentials. api-access accepts a key holding
// the read:service-accounts:admin scope pair; account-management rejects a
// SCOPED key with 403 whatever scopes it holds, and needs an organization API
// key created without scopes at all.
const serviceAccountsPath = "/admin/account-management/v1/orgs/%s/service-accounts"

// invitePath grants service accounts product access.
//
// It is the only route that works: a service account is not a directory user,
// so group membership answers USER_NOT_FOUND, and the role-assignment
// endpoints are read-only.
const invitePath = serviceAccountsPath + "/invite"

// apiTokensPath mints, lists and revokes one account's API tokens.
//
//nolint:gosec // a URL template, not a credential
const apiTokensPath = "/users/%s/manage/api-tokens"

// workspacesPath lists the products in an organization. A POST because
// Atlassian models it as a query, and it is how the site an agent works in is
// DISCOVERED rather than asked for.
const workspacesPath = "/admin/v3/orgs/%s/workspaces"

// listPageSize is Atlassian's documented maximum for the service-account
// collection. It rejects anything larger with a 400 rather than clamping.
const listPageSize = 80

// listPageLimit bounds how far a listing follows its own next links.
//
// A RUNAWAY GUARD, not a ceiling: at 80 per page it allows 4000 service
// accounts, which is far past any organization that could pay for them.
// Exceeding it is an ERROR rather than a short answer, because a listing that
// stopped early would report accounts that exist as missing — and the repair
// for "missing" is to create a second identity on top of a live one.
const listPageLimit = 50

// DefaultTokenExpiryDays is how long a minted token lasts unless the company
// says otherwise.
//
// Atlassian caps an API token at 365 days and will not mint one that outlives
// that. 300 is deliberately short of the cap: nothing in Crewlet renews a
// credential on a schedule, so the only thing standing between a company and
// a silent fleet-wide 401 is somebody re-running the command — and two months
// of slack is the difference between noticing at the next provisioning run
// and noticing when the agents stop answering. An operator whose policy is
// tighter sets integrations.atlassian.token_expiry_days.
const DefaultTokenExpiryDays = 300

// MaxTokenExpiryDays is Atlassian's own cap, refused here rather than at the
// vendor so an operator learns it from the config error rather than from a
// failed run half way through a company.
const MaxTokenExpiryDays = 365

var (
	// ErrUnscopedKeyRequired means the organization key was refused.
	//
	// It covers a 403 AND a 404 on a GET, because the collection exists for
	// any credential that may see it: a 404 there means the key cannot
	// reach the service rather than that the organization has no accounts.
	ErrUnscopedKeyRequired = errors.New(
		"atlassian refused the organization API key. The service-account " +
			"admin API needs a key created WITHOUT SCOPES at " +
			"admin.atlassian.com — a scoped key is refused with 403 whatever " +
			"scopes it holds, which is the same answer as no permission at all")

	// ErrAccountNotReady means Atlassian created the account but does not
	// yet see it in the directory, so it cannot be granted a licence.
	//
	// A DELAY, not a failure: a service account becomes grantable a short
	// time after it is created, routinely longer than the rest of one seat's
	// provisioning takes.
	ErrAccountNotReady = errors.New("the service account is not visible in the directory yet")

	// ErrTokenNotReturned means Atlassian accepted the mint and returned no
	// token value. The value exists only in that response, so there is
	// nothing to retry against and the credential that now exists cannot be
	// used by anything.
	ErrTokenNotReturned = errors.New("atlassian did not return a token value")

	// ErrQuotaExceeded means the organization has no room for another
	// service account or another product licence.
	ErrQuotaExceeded = errors.New(
		"the Atlassian organization has no room for another service account " +
			"or product licence — free one, or raise the allowance " +
			"(Atlassian Guard raises it)")

	// ErrNoSite means the organization has no online Jira or Confluence
	// product, so there is nothing for agents to work in.
	ErrNoSite = errors.New("the Atlassian organization has no online site")

	// ErrManySites means the organization has more than one site. Choosing
	// one would point every agent somewhere the operator did not pick.
	ErrManySites = errors.New("the Atlassian organization has more than one site")

	// ErrUnexpected means Atlassian answered in a way this client cannot
	// make sense of, so continuing would act on a half-read answer.
	ErrUnexpected = errors.New("atlassian answered unexpectedly")
)

// ServiceAccount is one provisioned identity as Atlassian reports it.
//
// TWO ids, and they are not interchangeable. ID addresses the account in the
// account-management collection (create, delete); AtlassianID is the account
// id every other surface uses — the token routes, the licence grant, and the
// account id a webhook payload names the agent by. Using one where the other
// belongs answers 404 for an account that is perfectly alive.
type ServiceAccount struct {
	ID          string `json:"id"`
	AtlassianID string `json:"atlassianId"`
	Email       string `json:"email"`
	DisplayName string `json:"displayName"`
	Status      string `json:"status"`
}

// MintedToken is a freshly created API token.
//
// Its value is returned ONCE, at creation, and is never retrievable again —
// which is the whole reason a provisioning run records write-through and
// revokes what it cannot record.
type MintedToken struct {
	ID    string `json:"id"`
	Token string `json:"token"`
}

// AgentToken is one credential on an account, as Atlassian lists it.
//
// The value is never returned again after minting, so this carries only what
// is needed to RECOGNISE and revoke one. In particular the scopes are not
// here: they cannot be read back at all, which is why a provisioning run
// detects a stale scope set by exercising the credential rather than by
// remembering what it asked for.
type AgentToken struct {
	ID    string `json:"id"`
	Label string `json:"label"`
}

// Site is one Atlassian site in an organization.
type Site struct {
	CloudID string
	URL     string
	Name    string
}

// AdminClient calls the organization admin API with an organization key.
type AdminClient struct {
	t *Transport
}

// AdminOptions configure an [AdminClient].
type AdminOptions struct {
	// BaseURL is the admin API root. Empty takes [AdminBaseURL]; a test
	// points it at its own server.
	BaseURL string
	// Key is the organization API key, created without scopes.
	Key  string
	HTTP *http.Client
}

// NewAdminClient builds the admin-plane client.
func NewAdminClient(opts AdminOptions) (*AdminClient, error) {
	base := strings.TrimSpace(opts.BaseURL)
	if base == "" {
		base = AdminBaseURL
	}
	key := strings.TrimSpace(opts.Key)
	if key == "" {
		return nil, errors.New("atlassian: no organization API key")
	}
	t, err := NewTransport("atlassian", base, "Bearer "+key, opts.HTTP)
	if err != nil {
		return nil, err
	}
	return &AdminClient{t: t}, nil
}

// Gateway is the host product reads go through, which is the same host the
// admin API is served from. Exported so a test can point both planes at one
// server.
func (c *AdminClient) Gateway() string { return c.t.Base }

// HTTP is the client this admin plane uses, so the product plane can share
// its transport rather than opening a second pool.
func (c *AdminClient) HTTP() *http.Client { return c.t.HTTP }

// call runs one admin request and maps Atlassian's refusals onto sentinels.
func (c *AdminClient) call(ctx context.Context, method, path string, in, out any) error {
	err := c.t.Do(ctx, method, path, nil, in, out)
	if err == nil {
		return nil
	}
	var api *APIError
	if !errors.As(err, &api) {
		return err
	}
	switch {
	case api.Status == http.StatusUnauthorized, api.Status == http.StatusForbidden:
		return fmt.Errorf("%w (%s %s)", ErrUnscopedKeyRequired, method, path)
	case api.Status == http.StatusNotFound && method == http.MethodGet:
		// The collection exists for any credential that may see it, so a
		// 404 on a read means the key cannot reach the service rather than
		// that the organization holds nothing.
		return fmt.Errorf("%w (%s %s)", ErrUnscopedKeyRequired, method, path)
	case api.Status == http.StatusPaymentRequired, api.Status == http.StatusConflict:
		// Atlassian answers a full organization with a payment or a
		// conflict status depending on WHY it is full, and both mean the
		// same thing to an operator.
		return fmt.Errorf("%w: %s", ErrQuotaExceeded, api.Detail)
	}
	return err
}

// ServiceAccounts lists the organization's existing accounts.
//
// It is what makes a re-run safe in the one case nothing else covers: an
// account deleted in admin.atlassian.com leaves a seat's recorded credential
// pointing at an identity that no longer exists, and nothing else notices —
// the agent is not failing, it is absent.
func (c *AdminClient) ServiceAccounts(ctx context.Context, orgID string) ([]ServiceAccount, error) {
	path := fmt.Sprintf(serviceAccountsPath, orgID) + "?limit=" + strconv.Itoa(listPageSize)
	accounts := []ServiceAccount{}
	for range listPageLimit {
		var out struct {
			Items []ServiceAccount `json:"items"`
			Links struct {
				Next string `json:"next"`
			} `json:"links"`
		}
		if err := c.call(ctx, http.MethodGet, path, nil, &out); err != nil {
			return nil, err
		}
		accounts = append(accounts, out.Items...)
		if out.Links.Next == "" {
			return accounts, nil
		}
		// The next link is ABSOLUTE. Only its path and query are kept, so
		// the call still goes through this client's own base — a test
		// server is not asked to answer for api.atlassian.com, and a
		// redirected host cannot be handed this organization's key.
		next, err := url.Parse(out.Links.Next)
		if err != nil {
			return nil, fmt.Errorf("atlassian: read the next page of service accounts: %w", err)
		}
		path = next.RequestURI()
	}
	return nil, fmt.Errorf(
		"%w: the service-account listing did not end after %d pages, so an "+
			"account that exists could be reported missing and provisioned "+
			"a second time", ErrUnexpected, listPageLimit)
}

// CreateServiceAccount provisions one agent's identity.
//
// The response carries NO credential — [AdminClient.MintToken] supplies that
// in a second call — and it carries the EMAIL Atlassian assigned, which is
// half of what the agent authenticates with.
func (c *AdminClient) CreateServiceAccount(ctx context.Context, orgID, displayName, description string) (*ServiceAccount, error) {
	body := map[string]any{"displayName": displayName, "description": description}
	account := new(ServiceAccount)
	if err := c.call(ctx, http.MethodPost, fmt.Sprintf(serviceAccountsPath, orgID), body, account); err != nil {
		var lost *TransportError
		if !errors.As(err, &lost) {
			// Atlassian ANSWERED and refused, so nothing was created and
			// there is nothing to go looking for.
			return nil, err
		}
		// NO ANSWER CAME BACK, which is ambiguous in the one direction that
		// costs something: the request may have landed and its response
		// been lost, in which case the account exists, this process holds
		// no id for it, and no rollback can reach it — a later run then
		// matches it by display name and adopts an account with no
		// credential. Naming it is all that can be done, and it is what
		// turns an invisible orphan into one search in admin.atlassian.com.
		return nil, fmt.Errorf("%w — if this request reached Atlassian, a "+
			"service account named %q now exists with no credential; find it "+
			"in admin.atlassian.com and delete it, or re-run to adopt it",
			err, displayName)
	}
	if account.AtlassianID == "" {
		// Everything downstream — the token mint, the licence grant, the
		// account id a webhook names this agent by — keys on this field.
		// An account created without one is unusable, and discovering that
		// at the mint would report the wrong failure.
		// THE ACCOUNT IS HANDED BACK ANYWAY, with the error. It exists, it
		// is billable, and it is this run's to undo — a caller that got
		// only the error would roll back without deleting it, and every
		// later run would then match it by display name and fail the same
		// seat again for ever.
		return account, fmt.Errorf(
			"%w: it created a service account and returned no atlassianId, "+
				"so there is no identity to mint a token for", ErrUnexpected)
	}
	return account, nil
}

// DeleteServiceAccount removes an identity.
//
// Destructive in a way no other vendor's is: the account owns the issues it
// reported and appears in every history it touched, and Atlassian offers no
// disable verb to reach for instead.
func (c *AdminClient) DeleteServiceAccount(ctx context.Context, orgID, accountID string) error {
	return c.call(ctx, http.MethodDelete, fmt.Sprintf(serviceAccountsPath+"/%s", orgID, accountID), nil, nil)
}

// MintToken creates an API token for one service account.
//
// # The LABEL is the caller's, and it is the only handle on the credential
//
// It is what a later run recognises its own credentials by, and — when a
// response is lost in transit — the only thing a rollback can find the
// credential with. So it is built once by the caller and sent verbatim
// rather than decorated here: a label this function stamped would differ
// from the one the caller remembers, and a cleanup that compares them would
// silently match nothing while reporting that it had revoked everything. It
// is not read back off the response either — the caller already knows what it
// sent, and a value echoed for its own sake is one more thing that can
// disagree.
//
// The expiry field is named `expiry`. Sending `expiresAt`, which is the name
// Atlassian uses on other surfaces, fails with INVALID_EXPIRY and reads as
// though no expiry had been sent at all.
func (c *AdminClient) MintToken(ctx context.Context, atlassianID, label string, scopes []string, lifetime time.Duration, now time.Time) (*MintedToken, error) {
	body := map[string]any{
		"label":  label,
		"scopes": scopes,
		"expiry": now.Add(lifetime).UTC().Format(time.RFC3339),
	}
	token := new(MintedToken)
	if err := c.call(ctx, http.MethodPost, fmt.Sprintf(apiTokensPath, atlassianID), body, token); err != nil {
		return nil, err
	}
	if token.Token == "" {
		// The credential may nonetheless EXIST, so the token comes back
		// with the error: its id is the direct handle on it, and the label
		// the caller sent is the fallback when even that is missing.
		return token, ErrTokenNotReturned
	}
	if token.ID == "" {
		// A LIVE CREDENTIAL WITH NO HANDLE ON IT. Everything that manages
		// one afterwards keys on the id: revoking this exact token on a
		// rollback, and — much worse — telling it apart from the ones it
		// replaced. retirePrevious skips the token whose id it was told to
		// keep, so an empty id makes the fresh credential match the retire
		// prefix like every other and be revoked seconds after it was
		// recorded. Refused here so the caller rolls the seat back by
		// label instead.
		return token, fmt.Errorf(
			"%w: it returned a credential with no id, so nothing could tell "+
				"it from the ones it replaces", ErrUnexpected)
	}
	return token, nil
}

// Tokens lists every credential on one service account — Crewlet's and
// anybody else's.
func (c *AdminClient) Tokens(ctx context.Context, atlassianID string) ([]AgentToken, error) {
	var tokens []AgentToken
	if err := c.call(ctx, http.MethodGet, fmt.Sprintf(apiTokensPath, atlassianID), nil, &tokens); err != nil {
		return nil, err
	}
	return tokens, nil
}

// RevokeToken deletes one credential. The account keeps working through
// whatever others it holds.
func (c *AdminClient) RevokeToken(ctx context.Context, atlassianID, tokenID string) error {
	return c.call(ctx, http.MethodDelete, fmt.Sprintf(apiTokensPath, atlassianID)+"/"+tokenID, nil, nil)
}

// GrantLicence makes a service account a licensed user of one product on one
// site.
//
// Atlassian answers 202 and applies it ASYNCHRONOUSLY, so the account is not
// licensed the moment this returns — which is why a permission check that
// fails right after one of these is reported as still starting rather than as
// broken.
//
// A LICENCE IS BILLABLE. This is the one call in the package that costs the
// operator money on every new seat, which is why the per-seat product list is
// a config field rather than "every product the company configures".
func (c *AdminClient) GrantLicence(ctx context.Context, orgID, cloudID, atlassianID string, product Product) error {
	body := map[string]any{
		"userIds": []string{atlassianID},
		"permissionRules": []map[string]string{{
			"role":     MemberRoleARI(product),
			"resource": SiteResourceARI(product, cloudID),
		}},
	}
	err := c.call(ctx, http.MethodPost, fmt.Sprintf(invitePath, orgID), body, nil)

	// A just-created account is not grantable yet, and Atlassian says so
	// with a 404 that reads exactly like "no such account". Naming it lets
	// the caller WAIT instead of concluding the account failed to create.
	var api *APIError
	if errors.As(err, &api) && api.Status == http.StatusNotFound &&
		strings.Contains(api.Detail, "not found in the directory") {
		return ErrAccountNotReady
	}
	return err
}

// workspace is one product in an organization, as the admin API reports it.
type workspace struct {
	// ID is an ARI of the form ari:cloud:jira::site/<cloudId>, which is
	// where the cloud id comes from: no field carries it directly.
	ID         string `json:"id"`
	Attributes struct {
		Name    string `json:"name"`
		TypeKey string `json:"typeKey"`
		Status  string `json:"status"`
		HostURL string `json:"hostUrl"`
	} `json:"attributes"`
}

// DiscoverSite finds the organization's site, so an operator does not have to
// look up a cloud id by hand.
//
// [ErrNoSite] when the organization has no online product this build serves,
// and [ErrManySites] when it has more than one — picking for the operator
// would silently point every agent at a place they did not choose, and the
// symptom is an agent that authenticates perfectly into an empty instance.
func (c *AdminClient) DiscoverSite(ctx context.Context, orgID string) (*Site, error) {
	var out struct {
		Data []workspace `json:"data"`
	}
	if err := c.call(ctx, http.MethodPost, fmt.Sprintf(workspacesPath, orgID), map[string]any{}, &out); err != nil {
		return nil, err
	}
	sites := map[string]*Site{}
	for _, w := range out.Data {
		if gatewayHost[Product(w.Attributes.TypeKey)] == "" || w.Attributes.HostURL == "" {
			continue
		}
		if w.Attributes.Status != "" && w.Attributes.Status != "online" {
			continue
		}
		cloudID := cloudIDFromARI(w.ID)
		if cloudID == "" {
			continue
		}
		// KEYED BY CLOUD ID, so one site running both Jira and Confluence
		// is one site rather than two — which is the ordinary arrangement,
		// and counting it twice would refuse it as ambiguous.
		sites[cloudID] = &Site{
			CloudID: cloudID,
			URL:     strings.TrimRight(w.Attributes.HostURL, "/"),
			Name:    w.Attributes.Name,
		}
	}
	switch len(sites) {
	case 0:
		return nil, ErrNoSite
	case 1:
		for _, site := range sites {
			return site, nil
		}
	}
	return nil, ErrManySites
}

// cloudIDFromARI pulls the id out of ari:cloud:jira::site/<cloudId>.
func cloudIDFromARI(ari string) string {
	_, id, found := strings.Cut(ari, "::site/")
	if !found {
		return ""
	}
	return id
}
