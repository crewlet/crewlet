package atlassian_test

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/atlas/derive"
	"github.com/crewlet/crewlet/internal/atlassian"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/redact/redacttest"
)

// atlasNow is the instant every atlas read here is made at.
var atlasNow = time.Date(2026, 11, 13, 7, 0, 0, 0, time.UTC)

// Values the fake carries in fields the read never decodes.
const (
	canaryPerson = "atlas-canary-person"
	canaryEmail  = "atlas-canary-person@acme.example"
	canaryText   = "atlas-canary-label"
)

// The fixture's organization, its key, and the keys of its entries.
const (
	atlasOrg     = "0a1b2c3d-0000-4000-8000-00000000a71a"
	atlasKey     = "organization-key-value"
	atlasCloud   = "11111111-0000-4000-8000-000000000001"
	sandboxCloud = "11111111-0000-4000-8000-000000000002"
	orgKey       = "atlassian/" + atlasOrg
	siteKey      = "atlassian/acme.atlassian.net"
	sandboxKey   = "atlassian/acme-sandbox.atlassian.net"
	workspaces   = "POST /admin/v2/orgs/" + atlasOrg + "/workspaces"
)

// orgFake is an organization as the atlas reads it: the workspace listing,
// one page of size rows at a time, behind the organization's key.
type orgFake struct {
	rows []map[string]any
	// size is the rows a page holds; zero is all. link is whether the next
	// link is an address carrying the cursor rather than the cursor itself.
	size int
	link bool
	// refuse answers the listing with a status; limited answers 429 first,
	// this many times, naming wait seconds.
	refuse  int
	limited int
	wait    int

	mu    sync.Mutex
	calls []string
}

// workspace is one row, with the fields the read never decodes filled in.
func workspace(product, id, typeKey, kind, host string) map[string]any {
	return map[string]any{"id": "ari:cloud:" + product + "::" + id, "type": "workspace",
		"attributes": map[string]any{"typeKey": typeKey, "type": kind, "hostUrl": host, "name": canaryText,
			"owner": canaryPerson, "createdBy": canaryEmail, "labels": []string{canaryText}, "status": "online",
			"avatars": []string{"https://avatars.acme.example/" + canaryPerson}},
		"relationships": map[string]any{"owner": canaryEmail}}
}

// acme is a company with Jira and Confluence on one site, a sandbox site
// with Jira alone, Bitbucket and a product this build does not know.
func acme() *orgFake {
	return &orgFake{rows: []map[string]any{
		workspace("jira-software", "site/"+atlasCloud, "jira-software", "Jira Software", "https://acme.atlassian.net"),
		workspace("jira-servicedesk", "site/"+atlasCloud, "jira-servicedesk", "Jira Service Management",
			"https://acme.atlassian.net"),
		workspace("confluence", "site/"+atlasCloud, "confluence", "Confluence", "https://acme.atlassian.net/wiki"),
		workspace("jira-software", "site/"+sandboxCloud, "jira-software", "Jira Software",
			"https://Acme-Sandbox.atlassian.net/"),
		workspace("bitbucket", "workspace/22222222-0000-4000-8000-000000000001", "bitbucket", "Bitbucket",
			"https://bitbucket.org/acme"),
		workspace("unheardof", "workspace/22222222-0000-4000-8000-000000000002", "unheardof", "Unheard Of", ""),
	}}
}

func (o *orgFake) serve(w http.ResponseWriter, r *http.Request) {
	o.mu.Lock()
	defer o.mu.Unlock()
	call := r.Method + " " + r.URL.Path
	o.calls = append(o.calls, call)
	w.Header().Set("Content-Type", "application/json")
	fail := func(status int, message string) {
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": message})
	}
	switch {
	case r.Header.Get("Authorization") != "Bearer "+atlasKey:
		fail(http.StatusUnauthorized, "Unauthorized")
		return
	case call != workspaces:
		fail(http.StatusNotFound, "no route")
		return
	case o.limited > 0:
		o.limited--
		w.Header().Set("Retry-After", strconv.Itoa(o.wait))
		fail(http.StatusTooManyRequests, "Too many requests")
		return
	case o.refuse != 0:
		fail(o.refuse, "refused by the stand-in")
		return
	}
	var body struct {
		Cursor string `json:"cursor"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		fail(http.StatusBadRequest, "the listing is a query and takes a body")
		return
	}
	from, _ := strconv.Atoi(body.Cursor)
	to := len(o.rows)
	if o.size > 0 {
		to = min(from+o.size, len(o.rows))
	}
	out := map[string]any{"data": o.rows[from:to], "meta": map[string]int{"total": len(o.rows)}}
	if to < len(o.rows) {
		next := strconv.Itoa(to)
		if o.link {
			next = "https://api.atlassian.com" + r.URL.Path + "?cursor=" + next
		}
		out["links"] = map[string]string{"next": next}
	}
	_ = json.NewEncoder(w).Encode(out)
}

// company is a company connected to the fake's organization, working in
// Jira and Confluence on its one site.
func company() *config.Company {
	c := &config.Company{Name: "Acme"}
	c.Integrations.Atlassian = &config.Atlassian{OrgID: atlasOrg, APIKey: "${ATLASSIAN_KEY}"}
	c.Integrations.Jira = &config.Jira{CloudID: atlasCloud, SiteURL: "https://acme.atlassian.net"}
	c.Integrations.Confluence = &config.Confluence{CloudID: atlasCloud, SiteURL: "https://acme.atlassian.net/wiki"}
	return c
}

func (o *orgFake) input(t *testing.T, edit ...func(*config.Company)) collect.Input {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(o.serve))
	t.Cleanup(srv.Close)
	c := company()
	for _, e := range edit {
		e(c)
	}
	env := config.NewResolver(config.MapSource{"ATLASSIAN_KEY": atlasKey})
	return collect.Input{Config: c, Now: atlasNow, Env: env, HTTP: &http.Client{Transport: httpxtest.Rewrite(t, srv)}}
}

func atlassianSource(t *testing.T) collect.Source {
	t.Helper()
	src, ok := collect.Lookup(integration.KindAtlassian)
	if !ok {
		t.Fatal("no Atlassian source is registered")
	}
	return src
}

func (o *orgFake) read(t *testing.T, edit ...func(*config.Company)) collect.Read {
	t.Helper()
	read, err := atlassianSource(t).Collect(t.Context(), o.input(t, edit...))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return read
}

// atlasView is claims as the atlas folds them.
func atlasView(claims []atlas.Claim) atlas.View {
	return atlas.Join(atlas.Fold(claims, atlasNow), atlas.Joined{}, atlasNow)
}

func atlasEntry(t *testing.T, v atlas.View, key string) atlas.Entry {
	t.Helper()
	e, ok := v.Find(atlas.MustRef(atlas.KindAccount, key).ID())
	if !ok {
		t.Fatalf("%s is not in the read", key)
	}
	return e
}

// atlasFacts is every value e holds for f, sorted.
func atlasFacts(e atlas.Entry, f atlas.Fact) []string {
	var out []string
	for _, fv := range e.Facts {
		if fv.Fact == f {
			out = append(out, fv.Value.Text)
		}
	}
	slices.Sort(out)
	return out
}

// THE READ CLAIMS THE ORGANIZATION AND EVERY SITE AS AN ACCOUNT, AND SAYS WHAT
// EACH PRODUCT MAKES OF THE ACCOUNT IT IS ON, as closed words: a product on a
// site is a capability of the site's account, one on no site a capability of
// the organization's, and one this build does not know says nothing. It is
// one listing under the organization's key. Nothing a row carries beside the
// mapped fields reaches the read: nobody who created or owns a workspace, no
// name, no label, no avatar.
//
// Mutation: decode a workspace's owner or its creator into a claim, and a
// canary reaches the read; say a product by its key, and a product this build
// does not know becomes a value.
func TestTheAtlasReadsTheOrganizationsSitesAndProducts(t *testing.T) {
	t.Parallel()
	o := acme()
	read := o.read(t)
	if !slices.Equal(o.calls, []string{workspaces}) || read.Calls != 1 || len(read.Withheld) != 0 {
		t.Fatalf("the read made %v, counted %d calls and withheld %+v", o.calls, read.Calls, read.Withheld)
	}
	v := atlasView(read.Claims)
	if len(v.Entries) != 3 {
		t.Fatalf("the read holds %d entries, want the organization and two sites", len(v.Entries))
	}
	if org := atlasEntry(t, v, orgKey); !slices.Equal(atlasFacts(org, atlas.FactCapability), []string{"code_hosting"}) ||
		len(org.Facts) != 1 {
		t.Errorf("the organization: %+v", org.Facts)
	}
	// The site the settings name is described and never named.
	site := atlasEntry(t, v, siteKey)
	if !slices.Equal(atlasFacts(site, atlas.FactCapability), []string{"knowledge", "tracker"}) ||
		!slices.Equal(atlasFacts(site, atlas.FactAccountRef), []string{atlasCloud}) ||
		!slices.Equal(atlasFacts(site, atlas.FactVendor), []string{"atlassian"}) ||
		len(atlasFacts(site, atlas.FactName)) != 0 || len(atlasFacts(site, atlas.FactSite)) != 0 {
		t.Errorf("the site the engine works in: %+v", site.Facts)
	}
	// A site the settings do not name is named and placed by the read.
	sandbox := atlasEntry(t, v, sandboxKey)
	if sandbox.Name != "acme-sandbox.atlassian.net" ||
		!slices.Equal(atlasFacts(sandbox, atlas.FactSite), []string{"https://acme-sandbox.atlassian.net"}) ||
		!slices.Equal(atlasFacts(sandbox, atlas.FactCapability), []string{"tracker"}) ||
		!slices.Equal(atlasFacts(sandbox, atlas.FactAccountRef), []string{sandboxCloud}) {
		t.Errorf("the sandbox site: %q %+v", sandbox.Name, sandbox.Facts)
	}
	out, err := json.Marshal(read)
	if err != nil {
		t.Fatal(err)
	}
	for _, canary := range []string{canaryPerson, canaryEmail, canaryText, atlasKey, "unheardof", "bitbucket.org", "@"} {
		if strings.Contains(string(out), canary) {
			t.Errorf("%q reached the read", canary)
		}
	}
}

// THE READ MEETS THE SETTINGS ON THE ORGANIZATION AND ON EACH SITE THE ENGINE
// WORKS IN: both are keyed as the settings key them, and the read gives
// neither a name or an address, so the accounts a company connected are one
// entry each and the two voices dispute nothing.
//
// Mutation: name every site from its host, and the site Jira and Confluence
// work in sits in Review for a name nobody chose; key a site by its address
// as Atlassian wrote it, and a host in capitals is a second account.
func TestTheAtlassianReadAndTheSettingsNameTheSameAccounts(t *testing.T) {
	t.Parallel()
	read := acme().read(t)
	settings := derive.FromSettings(company(), "r1", derive.Deployment{}).Claims(atlasNow)
	v := atlasView(append(slices.Clone(settings.Claims), read.Claims...))
	if len(v.Entries) != 3 {
		t.Fatalf("%d entries, want the organization and two sites", len(v.Entries))
	}
	for _, key := range []string{orgKey, siteKey} {
		if e := atlasEntry(t, v, key); e.Review || len(e.Sources) != 2 {
			t.Errorf("%s: review %v, sources %+v", key, e.Review, e.Sources)
		}
	}
	if site := atlasEntry(t, v, siteKey); !slices.Equal(atlasFacts(site, atlas.FactCapability),
		[]string{"knowledge", "tracker"}) {
		t.Errorf("the site: %+v", site.Facts)
	}
}

// A LISTING ATLASSIAN REFUSES THE KEY IS A FINDING AND RETIRES NOTHING: a key
// created with scopes that leave the listing out leaves the products unread,
// the standing says which key to use in the engine's own words, and no site
// an earlier read recorded is missed or retired, however long it lasts.
//
// Mutation: read the refused listing as an empty one, and every site but the
// ones the settings name is retired a day after somebody rotates the key.
func TestAListingAtlassianRefusesIsAFindingAndRetiresNothing(t *testing.T) {
	t.Parallel()
	whole := acme().read(t)
	o := acme()
	o.refuse = http.StatusForbidden
	read := o.read(t)
	if len(read.Unread) != 1 || read.Unread[0].Part != "workspaces" ||
		!strings.Contains(read.Unread[0].Detail, "integrations.atlassian.api_key") ||
		!strings.Contains(read.Unread[0].Detail, "without scopes") ||
		strings.Contains(read.Unread[0].Detail, "refused by the stand-in") {
		t.Fatalf("the refused listing: %+v", read.Unread)
	}
	if len(read.Claims) != 1 || read.Claims[0].Subject.Key != orgKey {
		t.Fatalf("the refused read claims %+v, want the organization alone", read.Claims)
	}
	status := collect.Status{Kind: integration.KindAtlassian}
	for _, at := range []time.Time{atlasNow.Add(time.Hour), atlasNow.Add(30 * time.Hour), atlasNow.Add(60 * time.Hour)} {
		plan := collect.Decide(whole.Claims, read, status, at)
		if len(plan.Records) != 0 || len(plan.Status.Missing) != 0 || len(plan.Status.Unread) != 1 {
			t.Fatalf("at %s the refusal recorded %+v, missed %+v and found %+v", at, plan.Records,
				plan.Status.Missing, plan.Status.Unread)
		}
		status = plan.Status
	}
	// The same read, were the listing whole and empty, retires both sites.
	empty := read
	empty.Unread = nil
	first := collect.Decide(whole.Claims, empty, collect.Status{Kind: integration.KindAtlassian}, atlasNow.Add(time.Hour))
	if plan := collect.Decide(whole.Claims, empty, first.Status, atlasNow.Add(26*time.Hour)); plan.Status.Retired != 2 {
		t.Errorf("the control retired %d sites, want 2", plan.Status.Retired)
	}
}

// A SITE THAT IS GONE IS RETIRED AFTER TWO COMPLETE READS A DAY APART, an
// unchanged read records nothing, and a product somebody removes from a site
// is withdrawn at the next read.
//
// Mutation: retire on the first read that misses a site, and one page
// Atlassian served short removes a site from the atlas.
func TestAnAtlassianSiteTwoReadsMissIsRetired(t *testing.T) {
	t.Parallel()
	whole := acme().read(t)
	status := collect.Status{Kind: integration.KindAtlassian}
	if plan := collect.Decide(whole.Claims, whole, status, atlasNow.Add(time.Hour)); len(plan.Records) != 0 {
		t.Fatalf("an unchanged read recorded %+v", plan.Records)
	}
	o := acme()
	// Without the sandbox site, and without Confluence on the main one.
	o.rows = slices.Delete(o.rows, 2, 4)
	read := o.read(t)
	first := collect.Decide(whole.Claims, read, status, atlasNow.Add(time.Hour))
	if len(first.Records) != 1 || first.Records[0].Entry.Key != siteKey || len(first.Records[0].Withdrawn) != 1 ||
		first.Status.Retired != 0 || len(first.Status.Missing) != 1 {
		t.Fatalf("the first read recorded %+v and retired %d", first.Records, first.Status.Retired)
	}
	second := collect.Decide(whole.Claims, read, first.Status, atlasNow.Add(26*time.Hour))
	retired := slices.IndexFunc(second.Records, func(r collect.EntryRead) bool { return r.Retires })
	if second.Status.Retired != 1 || retired < 0 || second.Records[retired].Entry.Key != sandboxKey {
		t.Fatalf("the second miss retired %d with %+v", second.Status.Retired, second.Records)
	}
}

// NO CREDENTIAL SHAPE REACHES THE ATLAS FROM ATLASSIAN: every shape the
// redaction pass knows, planted in a workspace's identifier, its product key,
// its address and the fields the read never decodes, leaves no piece of
// itself in the read, and what the mapped fields refused is counted.
//
// Mutation: say a product's key as it came, keep an address's user
// information or its path, lower a host before the builder judges it, or
// skip the builder's grammar on a key, and a shape reaches the read.
func TestNoCredentialShapeReachesTheAtlasFromAtlassian(t *testing.T) {
	t.Parallel()
	for _, shape := range redacttest.Shapes() {
		s := shape.Sample
		o := &orgFake{rows: []map[string]any{
			workspace("jira-software", "site/"+s, s, s, "https://acme.atlassian.net"),
			workspace("confluence", "site/"+atlasCloud, "confluence", s, "https://"+s+".atlassian.net/wiki"),
			workspace(s, "workspace/"+s, s, s, s),
			workspace("jira-software", "site/"+sandboxCloud, "jira-"+s, "Jira Software",
				"https://ops:"+s+"@acme-sandbox.atlassian.net/"+s+"?token="+s+"#"+s),
		}}
		for _, row := range o.rows {
			attrs := row["attributes"].(map[string]any)
			attrs["name"], attrs["owner"], attrs["createdBy"], attrs["labels"] = s, s, s, []string{s}
		}
		read := o.read(t)
		out, err := json.Marshal(read)
		if err != nil {
			t.Fatal(err)
		}
		// In any case: a host is lowered on its way to a key, and a shape
		// lowered is still the credential's body.
		lowered := strings.ToLower(string(out))
		for _, piece := range []string{s[len(s)/2:], s[len(s)/4 : len(s)/4+12]} {
			if at := strings.Index(lowered, strings.ToLower(piece)); at >= 0 {
				t.Errorf("%s: a piece of the shape reached the read near %q", shape.Name,
					string(out)[max(0, at-120):at])
			}
		}
		// A site's cloud id is refused where it sat, whatever else is.
		if len(read.Withheld) < 1 {
			t.Errorf("%s: nothing withheld, want the planted cloud id: %+v", shape.Name, read.Withheld)
		}
		// What sat beside a credential is still read: an address is reduced
		// to its host, without the user information, path and query that
		// carried one. A shape with a space in it makes the address no
		// address at all, and its site is keyed by its cloud id.
		if strings.ContainsAny(s, " \n") {
			atlasEntry(t, atlasView(read.Claims), "atlassian/"+sandboxCloud)
			continue
		}
		v := atlasView(read.Claims)
		if sandbox := atlasEntry(t, v, sandboxKey); !slices.Equal(atlasFacts(sandbox, atlas.FactCapability),
			[]string{"tracker"}) || !slices.Equal(atlasFacts(sandbox, atlas.FactSite),
			[]string{"https://acme-sandbox.atlassian.net"}) {
			t.Errorf("%s: the site beside the credential: %+v", shape.Name, sandbox.Facts)
		}
	}
}

// A LISTING THAT DID NOT FINISH IS NO READ AT ALL: a server fault and a rate
// limit past what a call waits fail the read whole, so nothing is retired on
// a listing cut short, and a key Atlassian does not know is a rejected
// credential, never a missing scope.
//
// Mutation: read a dead key as a refusal, and a revoked key is reported as
// one created with the wrong scopes.
func TestAListingThatFailsOrIsRateLimitedFailsTheAtlassianRead(t *testing.T) {
	t.Parallel()
	for name, fault := range map[string]func(*orgFake, *config.Company){
		"a server fault": func(o *orgFake, _ *config.Company) { o.refuse = http.StatusBadGateway },
		// A wait longer than one call takes, so the limit is handed back.
		"the rate limit": func(o *orgFake, _ *config.Company) { o.limited, o.wait = 1, 60 },
		"a dead key":     func(_ *orgFake, c *config.Company) { c.Integrations.Atlassian.APIKey = "another" },
	} {
		o := acme()
		read, err := atlassianSource(t).Collect(t.Context(), o.input(t, func(c *config.Company) { fault(o, c) }))
		if err == nil || errors.Is(err, integration.ErrNotConfigured) || len(read.Claims) != 0 {
			t.Errorf("%s: the read answered %d claims and %v", name, len(read.Claims), err)
		}
		if rejected := name == "a dead key"; errors.Is(err, integration.ErrCredentialRejected) != rejected {
			t.Errorf("%s: a rejected credential is %v, want %v: %v", name, !rejected, rejected, err)
		}
		if name == "the rate limit" && (atlassian.Status(err) != http.StatusTooManyRequests ||
			!strings.Contains(err.Error(), "asked to wait 1m0s")) {
			t.Errorf("the limit handed back does not say what Atlassian asked: %v", err)
		}
	}
}

// THE LISTING IS WALKED TO ITS END, WAITING OUT A SHORT RATE LIMIT: every
// page is read, whether its next link is the cursor itself or an address
// carrying it, and a limit Atlassian asks a short wait for is waited out
// inside the call. The site the agents work in is found past the first page.
//
// Mutation: read the first page alone, and an organization with many
// products loses every site past it, and its agents the site they work in.
func TestTheAtlassianListingIsWalkedToItsEnd(t *testing.T) {
	t.Parallel()
	for _, link := range []bool{false, true} {
		o := acme()
		o.size, o.link, o.limited = 2, link, 2
		read := o.read(t)
		if v := atlasView(read.Claims); len(v.Entries) != 3 || read.Calls != 5 {
			t.Errorf("link %v: a limited, paged listing read %d entries in %d calls, want 3 in 5", link,
				len(v.Entries), read.Calls)
		}
	}
	// Jira's site is on the second page, behind Bitbucket and a product on
	// no site.
	o := acme()
	slices.Reverse(o.rows)
	o.size = 2
	srv := httptest.NewServer(http.HandlerFunc(o.serve))
	t.Cleanup(srv.Close)
	site, err := atlassian.NewClient(atlassian.ClientOptions{BaseURL: srv.URL}).DiscoverSite(t.Context(), atlasKey, atlasOrg)
	if err != nil || site.CloudID != atlasCloud || site.HostURL != "https://acme.atlassian.net" {
		t.Errorf("the site past the first page: %+v, %v", site, err)
	}
	// A next link that carries no cursor is an error, never an end.
	bad := &orgFake{rows: acme().rows, size: 2}
	broken := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[],"links":{"next":"https://api.atlassian.com/admin/v2/next"}}`))
	}))
	t.Cleanup(broken.Close)
	if _, err := atlassian.NewClient(atlassian.ClientOptions{BaseURL: broken.URL}).Workspaces(t.Context(), atlasKey, atlasOrg); err == nil {
		t.Errorf("a next link with no cursor ended the listing of %d rows", len(bad.rows))
	}
}

// A COMPANY THAT CONNECTS NO ORGANIZATION, OR WHOSE KEY DOES NOT RESOLVE, IS
// NOT CONFIGURED, which the duty reads as nothing to read rather than a
// fault: a Data Center company has no organization to list.
func TestAnUnconnectedAtlassianIsNotConfigured(t *testing.T) {
	t.Parallel()
	for name, edit := range map[string]func(*config.Company){
		"no block":        func(c *config.Company) { c.Integrations.Atlassian = nil },
		"no organization": func(c *config.Company) { c.Integrations.Atlassian.OrgID = " " },
		"no key":          func(c *config.Company) { c.Integrations.Atlassian.APIKey = "${ATLASSIAN_UNSET}" },
		"data center": func(c *config.Company) {
			c.Integrations.Atlassian = &config.Atlassian{Deployment: "data_center"}
		},
	} {
		o := acme()
		if _, err := atlassianSource(t).Collect(t.Context(), o.input(t, edit)); !errors.Is(err, integration.ErrNotConfigured) {
			t.Errorf("%s: %v, want not configured", name, err)
		}
		if len(o.calls) != 0 {
			t.Errorf("%s: the read called Atlassian: %v", name, o.calls)
		}
	}
	if _, err := atlassianSource(t).Collect(t.Context(), collect.Input{}); !errors.Is(err, integration.ErrNotConfigured) {
		t.Errorf("no document: %v, want not configured", err)
	}
}
