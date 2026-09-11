package datadog

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/textcut"
)

// ClientTimeout bounds one call to Datadog.
//
// Datadog's own documented p99 for the v2 API is under two seconds, and every
// call here is made inside a reconcile pass that already has a deadline of
// its own. Ten seconds is slack for a slow region, not a budget to spend.
const ClientTimeout = 10 * time.Second

// Datadog's regions.
//
// A key issued in one region is refused by every other, and the hostname is
// the only thing that distinguishes them — so a company's site is CHECKED
// against this list rather than trusted. A typo would otherwise be a
// credential that authenticates nowhere, reported as a rejected key.
//
//nolint:gochecknoglobals // an immutable list, not state
var sites = []string{
	"datadoghq.com",
	"us3.datadoghq.com",
	"us5.datadoghq.com",
	"datadoghq.eu",
	"ap1.datadoghq.com",
	"ap2.datadoghq.com",
	"ddog-gov.com",
}

// Sites is every region this build serves, in the order the setup form
// offers them.
func Sites() []string { return slices.Clone(sites) }

// DefaultSite is the region offered when a company names none.
//
// US1, which is Datadog's own default and where most organizations are. It is
// a SUGGESTION in the form rather than a value anything falls back to: a call
// built against the wrong region fails as a rejected credential, so an engine
// that guessed silently would report a key as bad when the region was.
const DefaultSite = "datadoghq.com"

// KnownSite reports whether a site is one Datadog actually serves.
func KnownSite(site string) bool { return slices.Contains(sites, normalizeSite(site)) }

func normalizeSite(site string) string {
	return strings.TrimSpace(strings.ToLower(site))
}

// Credentials are the pair every Datadog call carries.
//
// TWO KEYS, NOT ONE, and they are not interchangeable: the API key says which
// organization, and the APPLICATION key says which user acts. Datadog refuses
// a v2 write carrying only the first, with a message that names neither.
type Credentials struct {
	APIKey string
	AppKey string
}

// APIError is a call Datadog refused.
type APIError struct {
	Method string
	Path   string
	Status int
	Detail string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("datadog: %s %s: %d: %s", e.Method, e.Path, e.Status, e.Detail)
}

// Status reports the HTTP status a call was refused with, or 0 when the
// failure was not an API error.
//
// The same accessor every other third-party app client exports, for the same
// reason: a caller deciding what a refusal MEANS needs the number, and the
// meaning is decided once, in [integration.Reject], rather than per app.
func Status(err error) int {
	var api *APIError
	if errors.As(err, &api) {
		return api.Status
	}
	return 0
}

// Client talks to one Datadog region.
type Client struct {
	site string
	http *http.Client
}

// ClientOptions builds a client.
type ClientOptions struct {
	// Site is the region hostname, e.g. datadoghq.eu.
	Site string
	// HTTP overrides the transport, for tests.
	HTTP *http.Client
}

// NewClient builds a client for one region.
//
// It REFUSES an unknown site rather than accepting one: every later call is
// built from this hostname, so a typo becomes a credential that authenticates
// nowhere and reports as a rejected key.
func NewClient(opts ClientOptions) (*Client, error) {
	site := normalizeSite(opts.Site)
	if site == "" {
		return nil, errors.New("datadog: no site")
	}
	if !KnownSite(site) {
		return nil, fmt.Errorf(
			"datadog: %q is not a region Datadog serves — it is one of %s",
			opts.Site, strings.Join(sites, ", "))
	}
	client := opts.HTTP
	if client == nil {
		client = httpx.Client(ClientTimeout)
	}
	return &Client{site: site, http: client}, nil
}

// Site is the region this client talks to.
func (c *Client) Site() string { return c.site }

func (c *Client) endpoint(path string) string {
	return "https://api." + c.site + path
}

// do makes one call, and is the only place a Datadog request is built.
func (c *Client) do(
	ctx context.Context, method, path string, creds Credentials, body, out any,
) error {
	var reader io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("datadog: encode %s %s: %w", method, path, err)
		}
		reader = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.endpoint(path), reader)
	if err != nil {
		return fmt.Errorf("datadog: build %s %s: %w", method, path, err)
	}
	req.Header.Set("DD-API-KEY", creds.APIKey)
	req.Header.Set("DD-APPLICATION-KEY", creds.AppKey)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	res, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("datadog: %s %s: %w", method, path, err)
	}
	defer func() { _ = res.Body.Close() }()

	payload, err := io.ReadAll(io.LimitReader(res.Body, maxResponse))
	if err != nil {
		return fmt.Errorf("datadog: read %s %s: %w", method, path, err)
	}
	if res.StatusCode >= 300 {
		return &APIError{
			Method: method, Path: path, Status: res.StatusCode,
			Detail: detailOf(res.Header.Get("Content-Type"), payload),
		}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(payload, out); err != nil {
		return fmt.Errorf("datadog: decode %s %s: %w", method, path, err)
	}
	return nil
}

// maxResponse bounds what one answer may be.
//
// A list page is a few hundred accounts at most. The cap is what stops a
// proxy's HTML error page, or a region answering something unexpected, from
// being read into memory whole.
const maxResponse = 4 << 20

// maxDetail bounds what a refusal contributes to an error string.
//
// 2048 bytes, matching the github and gitlab clients, and it is not a
// cosmetic limit. This error becomes Finding.Detail, which
// [integration.Observe] stores WITHOUT truncation into a State the fleet
// writes to one coordination key shared with every other integration —
// [integration.MaxLastErrorLength]'s own doc names this exact hazard, "a
// client that pastes a response body into its error is a megabyte", and
// Finding.Detail is the field its guard does not cover. Datadog's client was
// that client: a non-JSON refusal put up to 4 MiB of HTML into it.
const maxDetail = 2048

// detailOf pulls Datadog's own message out of a refusal, so an operator
// reads what Datadog said rather than a status code.
//
// BOUNDED, because the answer to a call that failed is exactly the answer
// least likely to be the JSON this expects — see [maxDetail]. Cut through
// [textcut] rather than by slicing, so a multi-byte rune straddling the limit
// does not become invalid UTF-8 that a JSON encoder silently substitutes.
func detailOf(contentType string, payload []byte) string {
	var body struct {
		Errors []string `json:"errors"`
	}
	if err := json.Unmarshal(payload, &body); err == nil && len(body.Errors) > 0 {
		return textcut.Ellipsis(strings.Join(body.Errors, "; "), maxDetail)
	}
	// ANYTHING ELSE THROUGH [httpx.Refusal], rather than verbatim: a
	// refusal that is not the JSON this expects is most often an HTML
	// page from a gateway, and pasting one into an error puts a rendered
	// document in a log around a sentence nobody can find.
	detail := httpx.Refusal(contentType, payload)
	if detail == "" {
		return "no detail"
	}
	return textcut.Ellipsis(detail, maxDetail)
}

// Org is the organization a credential pair belongs to.
type Org struct {
	Name   string
	Public string
}

// VerifyCredentials reads the organization the keys authenticate as.
//
// THE CONNECT-TIME CHECK, and it earns its call: a key pair that is wrong is
// otherwise discovered by the first provisioning pass, minutes later, as a
// refusal on a screen nobody is watching. It also names the ORGANIZATION,
// which is the one thing an operator can check to see they connected the
// account they meant to.
//
// `/api/v1/org` rather than a v2 route, and it is not a preference: this was
// `/api/v2/current_user/orgs`, which does not exist. Datadog answers a made-up
// path with 404 rather than 401, so a correct key pair failed verification and
// the pass reported the credentials refused. There is no v2 equivalent that
// names the organization; the v1 route is current, not deprecated, and takes
// the same key pair.
func (c *Client) VerifyCredentials(ctx context.Context, creds Credentials) (Org, error) {
	var body struct {
		Orgs []struct {
			Name     string `json:"name"`
			PublicID string `json:"public_id"`
		} `json:"orgs"`
	}
	if err := c.do(ctx, http.MethodGet, "/api/v1/org", creds, nil, &body); err != nil {
		return Org{}, err
	}
	if len(body.Orgs) == 0 {
		return Org{}, errors.New(
			"datadog: the keys authenticated and named no organization, which " +
				"is an answer this build cannot act on")
	}
	first := body.Orgs[0]
	return Org{Name: first.Name, Public: first.PublicID}, nil
}

// User is a Datadog user or service account.
type User struct {
	ID       string
	Email    string
	Name     string
	Disabled bool

	// Title is Datadog's job-title field, which this engine uses as the
	// one durable place to record that IT disabled an account.
	//
	// AT THE VENDOR, BESIDE THE THING IT DESCRIBES. The obvious home is
	// the surface's own status row, and that row is FORGOTTEN the moment
	// the disconnect succeeds — destroyed on the success path of the very
	// operation that would write it. A field on the account survives the
	// disconnect, a fleet losing its coordination store, and a restore
	// from backup, and it cannot drift from the account it is about.
	//
	// The title rather than the name: matching reads the name and the
	// email, and a marker in either would be a second meaning loaded onto
	// a field that already has one. Nothing else sets a job title on a
	// service account.
	Title string
}

// DisconnectedTitle marks an account this engine disabled during a teardown.
//
// IT IS WHAT LETS A CONNECT UNDO A DISCONNECT. Re-enabling is otherwise a
// decision this pass may not make — an account a PERSON disabled at Datadog
// was disabled for a reason, and a pass that quietly reversed it would fight
// that gesture on every tick. With provenance the two cases are different
// facts rather than one ambiguous bit.
const DisconnectedTitle = "crewlet:disconnected"

// ListServiceAccounts reads the service accounts whose email matches query.
//
// FILTERED AT THE VENDOR rather than here: an organization may hold thousands
// of users, and walking all of them to find the handful this engine made
// spends a rate limit on rows nobody reads.
//
// TO EXHAUSTION, which the sibling enumerations in this tree already are and
// this one was not: [atlassian.Client.ListServiceAccounts] gives the reason
// in the same words — "stopping short would report an account that exists as
// missing and this engine would then create a second identity on top of a
// live one" — and this listing feeds both a create path and a decommission.
// One page was not even 100 service accounts: Datadog's filter is a free-text
// substring match, so every PERSON whose address is under the same domain
// consumes a slot on page one and is then dropped by the check below.
func (c *Client) ListServiceAccounts(
	ctx context.Context, creds Credentials, query string,
) ([]User, error) {
	out := []User{}
	err := c.walk(ctx, creds, "/api/v2/users", query, func(page usersPage) {
		for _, row := range page.Data {
			if !row.Attributes.ServiceAcc {
				// A PERSON matched the filter. Returning them would let
				// a caller looking for its own accounts adopt somebody's
				// real user and then disable it.
				continue
			}
			out = append(out, User{
				ID: row.ID, Email: row.Attributes.Email,
				Name: row.Attributes.Name, Disabled: row.Attributes.Disabled,
				Title: row.Attributes.Title,
			})
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// usersPage is one page of Datadog's user listing. Roles decode into it too:
// the fields a role does not have stay zero, and only Name is read for one.
type usersPage struct {
	Data []struct {
		ID         string `json:"id"`
		Attributes struct {
			Email      string `json:"email"`
			Name       string `json:"name"`
			Title      string `json:"title"`
			Disabled   bool   `json:"disabled"`
			ServiceAcc bool   `json:"service_account"`
		} `json:"attributes"`
	} `json:"data"`
	Meta struct {
		Page struct {
			// TotalFilteredCount is the size of the FILTERED set, which
			// is what this walk is enumerating. TotalCount is the whole
			// organization and would make the loop ask for pages the
			// filter can never fill.
			TotalFilteredCount int `json:"total_filtered_count"`
			TotalCount         int `json:"total_count"`
		} `json:"page"`
	} `json:"meta"`
}

// walk reads a filtered v2 collection to exhaustion, handing each page to fn.
//
// A SHORT READ IS INDISTINGUISHABLE FROM A COMPLETE ONE, which is the whole
// reason this exists: both callers treat "not in the answer" as "does not
// exist at Datadog", and one then creates a second identity on top of a live
// account while the other refuses the whole pass claiming the organization
// has no role by that name.
//
// The stop condition is a short page rather than the reported total, with the
// total used only as a sanity bound: a page that comes back smaller than what
// was asked for is the end of the collection on every JSON:API implementation,
// and trusting a count instead would loop for ever against a server that
// reports one it will not serve.
func (c *Client) walk(
	ctx context.Context, creds Credentials, path, query string, fn func(usersPage),
) error {
	for page := 0; ; page++ {
		if page > listWalkCeiling {
			return fmt.Errorf(
				"datadog: %s did not finish enumerating after %d pages of %d, "+
					"which is not an organization Crewlet provisions into — "+
					"narrow integrations.datadog.provisioning.email_domain or "+
					"the role name so the filter matches fewer rows",
				path, listWalkCeiling, listPageSize)
		}
		full := fmt.Sprintf("%s?filter=%s&page[size]=%d&page[number]=%d",
			path, url.QueryEscape(query), listPageSize, page)
		var body usersPage
		if err := c.do(ctx, http.MethodGet, full, creds, nil, &body); err != nil {
			return err
		}
		fn(body)
		if len(body.Data) < listPageSize {
			return nil
		}
	}
}

// listPageSize is what one listing asks for. Datadog's documented maximum is
// 100; a larger value is clamped server-side, which would make a partial walk
// look like a complete one.
const listPageSize = 100

// listWalkCeiling stops a walk that is not converging.
//
// 50 pages, so 5000 rows at [listPageSize] — the same number
// [gitlab.userWalkCeiling] stops at, and for the same reason: it is an
// enumeration a decommission decides from, so it must RAISE rather than
// return a short list. A truncated read reaching a caller is what creates a
// duplicate identity on top of a live account.
const listWalkCeiling = 50

// Role is a Datadog role, which is how permission is granted.
type Role struct {
	ID   string
	Name string
}

// ListRoles reads the roles whose name matches query.
//
// PAGED, for a sharper reason than the listing above. Datadog's filter is a
// substring match on the role name, so what comes back is every role
// CONTAINING the configured one, and the caller needs an EXACT match among
// them — [roleIDOf] refuses the whole pass when it finds none, with an error
// telling the operator their organization has no role by that name. Read one
// page deep, a large organization with a short role name got that accusation
// about a role that exists, and every seat in the company went unprovisioned.
func (c *Client) ListRoles(ctx context.Context, creds Credentials, query string) ([]Role, error) {
	out := []Role{}
	err := c.walk(ctx, creds, "/api/v2/roles", query, func(page usersPage) {
		for _, row := range page.Data {
			out = append(out, Role{ID: row.ID, Name: row.Attributes.Name})
		}
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// CreateServiceAccount provisions one agent's identity ALREADY HOLDING its
// role.
//
// The role is passed at creation rather than granted after, and that is not
// tidiness: an account that exists for a moment without one inherits the
// organization's default role, and a pass interrupted between the two calls
// leaves an agent holding whatever that default grants. Datadog accepts both
// in one request, so there is no window to be caught in.
func (c *Client) CreateServiceAccount(
	ctx context.Context, creds Credentials, email, name, roleID string,
) (User, error) {
	attributes := map[string]any{
		"email":           email,
		"name":            name,
		"service_account": true,
	}
	payload := map[string]any{
		"data": map[string]any{
			"type":       "users",
			"attributes": attributes,
			"relationships": map[string]any{
				"roles": map[string]any{
					"data": []map[string]string{{"id": roleID, "type": "roles"}},
				},
			},
		},
	}
	var body struct {
		Data struct {
			ID         string `json:"id"`
			Attributes struct {
				Email string `json:"email"`
				Name  string `json:"name"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodPost, "/api/v2/service_accounts", creds, payload, &body); err != nil {
		return User{}, err
	}
	return User{
		ID: body.Data.ID, Email: body.Data.Attributes.Email, Name: body.Data.Attributes.Name,
	}, nil
}

// AppKey is an application key belonging to a service account.
type AppKey struct {
	ID   string
	Name string
	// Key is the secret, and Datadog returns it EXACTLY ONCE, on creation.
	// Every later read answers with the id and the name only.
	Key string
}

// CreateAppKey mints the key one agent authenticates with.
//
// THE VALUE IS RETURNED ONCE. Datadog never shows it again, so a pass that
// creates a key and then fails to record it has stranded a credential that
// can only be deleted and remade. Every caller here writes it to the sink
// before doing anything else.
func (c *Client) CreateAppKey(
	ctx context.Context, creds Credentials, accountID, name string,
) (AppKey, error) {
	payload := map[string]any{
		"data": map[string]any{
			"type":       "application_keys",
			"attributes": map[string]any{"name": name},
		},
	}
	path := "/api/v2/service_accounts/" + url.PathEscape(accountID) + "/application_keys"
	var body struct {
		Data struct {
			ID         string `json:"id"`
			Attributes struct {
				Key  string `json:"key"`
				Name string `json:"name"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodPost, path, creds, payload, &body); err != nil {
		return AppKey{}, err
	}
	return AppKey{
		ID: body.Data.ID, Name: body.Data.Attributes.Name, Key: body.Data.Attributes.Key,
	}, nil
}

// DeleteAppKey revokes one application key.
//
// THE OTHER HALF OF MINTING. Datadog shows a key's value exactly once, so a
// key this engine created and then could not record is a credential that
// exists, is held by nobody, and which nothing will ever remember to remove
// — the state [provision.TokenSink]'s own contract legislates against: "a run
// that mints three tokens and cannot persist the third has to revoke all
// three".
//
// A 404 is success, matching [Client.DeleteWebhook]: a key already gone is
// the state this call is asking for, and the rollback it belongs to must be
// safe to repeat.
func (c *Client) DeleteAppKey(
	ctx context.Context, creds Credentials, accountID, keyID string,
) error {
	path := "/api/v2/service_accounts/" + url.PathEscape(accountID) +
		"/application_keys/" + url.PathEscape(keyID)
	err := c.do(ctx, http.MethodDelete, path, creds, nil, nil)
	if Status(err) == http.StatusNotFound {
		return nil
	}
	return err
}

// ListAppKeys reads one account's keys, without their values.
func (c *Client) ListAppKeys(ctx context.Context, creds Credentials, accountID string) ([]AppKey, error) {
	path := "/api/v2/service_accounts/" + url.PathEscape(accountID) + "/application_keys"
	var body struct {
		Data []struct {
			ID         string `json:"id"`
			Attributes struct {
				Name string `json:"name"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := c.do(ctx, http.MethodGet, path, creds, nil, &body); err != nil {
		return nil, err
	}
	out := make([]AppKey, 0, len(body.Data))
	for _, row := range body.Data {
		out = append(out, AppKey{ID: row.ID, Name: row.Attributes.Name})
	}
	return out, nil
}

// DisableUser deactivates a service account.
//
// DISABLED, NOT DELETED, and Datadog's own API is why: deleting a user
// detaches it from everything it did, so dashboards, monitors and notebooks
// it authored lose their author. A disabled service account keeps what it
// made and can do nothing more, which is what decommissioning one means.
func (c *Client) DisableUser(ctx context.Context, creds Credentials, userID string) error {
	return c.do(ctx, http.MethodDelete, "/api/v2/users/"+url.PathEscape(userID), creds, nil, nil)
}

// MarkDisconnected records that THIS engine is the one disabling an account.
//
// Written BEFORE the disable, so a run interrupted between the two leaves a
// marked account that is still enabled — harmless, and the next teardown
// finishes it — rather than a disabled one with no provenance, which is the
// state that can never be undone automatically.
func (c *Client) MarkDisconnected(ctx context.Context, creds Credentials, userID string) error {
	return c.updateUser(ctx, creds, userID, map[string]any{"title": DisconnectedTitle})
}

// EnableUser undoes a disable and clears the marker in one request.
//
// ONE REQUEST, because two would have a window in which the account is live
// and still marked — and a teardown running in that window would read the
// marker, skip its own mark, and disable an account it had not recorded.
func (c *Client) EnableUser(ctx context.Context, creds Credentials, userID string) error {
	return c.updateUser(ctx, creds, userID,
		map[string]any{"disabled": false, "title": ""})
}

// updateUser patches one account's attributes.
func (c *Client) updateUser(
	ctx context.Context, creds Credentials, userID string, attributes map[string]any,
) error {
	payload := map[string]any{"data": map[string]any{
		"type": "users", "id": userID, "attributes": attributes,
	}}
	return c.do(ctx, http.MethodPatch,
		"/api/v2/users/"+url.PathEscape(userID), creds, payload, nil)
}

// webhookPath is the Webhooks integration's configuration collection.
//
// THERE IS NO LIST ROUTE. Datadog answers a GET on the collection with 405,
// so a webhook is only ever addressed by name: convergence reads the one name
// this company owns rather than diffing a set, and a webhook somebody else
// made under another name is invisible here, which is the correct blindness.
const webhookPath = "/api/v1/integration/webhooks/configuration/webhooks"

// Webhook is one entry in Datadog's Webhooks integration.
//
// It is the whole of the inbound half: a monitor whose message names
// `@webhook-<Name>` is posted to URL with Payload rendered into its body and
// CustomHeaders attached, and nothing else at Datadog decides whether an
// alert reaches this engine.
type Webhook struct {
	// Name is the primary key AND the handle an operator writes in a
	// monitor message. Renaming one is therefore not an edit but a new
	// webhook plus every monitor that named the old one going nowhere.
	Name string `json:"name"`

	// URL is where Datadog posts, which is this deployment's public base
	// plus the inbound path.
	URL string `json:"url"`

	// Payload is the body template Datadog renders. Empty means Datadog
	// posts NOTHING, which is why [WebhookPayload] is written here rather
	// than left to a default that does not exist.
	Payload string `json:"payload,omitempty"`

	// CustomHeaders is a JSON object ENCODED AS A STRING, which is
	// Datadog's own shape for the field rather than a choice made here.
	// It carries the shared token, and Datadog hands it back in full on a
	// read: anyone holding an application key for this organization can
	// read the token, so it is a delivery check rather than a secret in
	// the sense the sealed store means.
	CustomHeaders string `json:"custom_headers,omitempty"`

	// EncodeAs is "json" or "form". Always json here: the route parses a
	// JSON body, and a form-encoded delivery arrives as a body the parser
	// reads as empty.
	EncodeAs string `json:"encode_as,omitempty"`
}

// Webhook reads one webhook definition by name.
//
// THREE-VALUED, and the third value is the point: a webhook that is absent is
// something to create, and a read that failed is not. Collapsing the two into
// an empty struct would have a rate-limited pass create a second definition
// over a healthy one on every tick.
func (c *Client) Webhook(
	ctx context.Context, creds Credentials, name string,
) (Webhook, bool, error) {
	var out Webhook
	err := c.do(ctx, http.MethodGet,
		webhookPath+"/"+url.PathEscape(name), creds, nil, &out)
	switch {
	case err == nil:
		return out, true, nil
	case Status(err) == http.StatusNotFound:
		return Webhook{}, false, nil
	default:
		return Webhook{}, false, err
	}
}

// CreateWebhook registers a new definition.
func (c *Client) CreateWebhook(ctx context.Context, creds Credentials, hook Webhook) error {
	return c.do(ctx, http.MethodPost, webhookPath, creds, hook, nil)
}

// UpdateWebhook rewrites the definition named by hook.Name.
//
// The WHOLE shape is sent rather than the fields that differ: Datadog's
// update takes the same body as its create, and a partial write is how the
// payload template survives an address change while the token does not.
func (c *Client) UpdateWebhook(ctx context.Context, creds Credentials, hook Webhook) error {
	return c.do(ctx, http.MethodPut,
		webhookPath+"/"+url.PathEscape(hook.Name), creds, hook, nil)
}

// DeleteWebhook removes a definition.
//
// A webhook that is already gone is a SUCCESS. Teardown is repeated after a
// partial failure, and refusing the second attempt would leave a disconnect
// stuck on work that is already done.
func (c *Client) DeleteWebhook(ctx context.Context, creds Credentials, name string) error {
	err := c.do(ctx, http.MethodDelete,
		webhookPath+"/"+url.PathEscape(name), creds, nil, nil)
	if Status(err) == http.StatusNotFound {
		return nil
	}
	return err
}
