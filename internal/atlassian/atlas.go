package atlassian

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/integration"
)

// Atlassian's half of the atlas (ADR-0138): every product of the company's
// organization, read whole under the organization API key the company
// already gave the engine. One listing, which is a query Atlassian models as
// a POST and changes nothing.
//
// A PRODUCT IS WHAT AN ACCOUNT DOES, NEVER AN ENTRY: a product on a site is a
// capability of that site's account, and one on no site a capability of the
// organization's. NOBODY IS READ: a row names who created a workspace and who
// owns it, and neither is decoded.

func init() { collect.Register(atlasSource{}) }

// atlasSource is Atlassian's [collect.Source].
type atlasSource struct{}

func (atlasSource) Kind() integration.Kind { return integration.KindAtlassian }

// partWorkspaces is the one part of Atlassian a read can be refused.
const partWorkspaces = "workspaces"

// accountKeyPrefix starts the key of every account Atlassian holds.
const accountKeyPrefix = string(atlas.VendorAtlassian) + "/"

// atlasAccess is what a read of the organization is made with.
type atlasAccess struct {
	client *Client
	calls  func() int
	org    string
	key    string
	// named is the hosts of the sites the settings already name.
	named map[string]bool
}

// openAtlas resolves the access a read is made with, and answers
// [integration.ErrNotConfigured] where the company connects no organization.
func openAtlas(in collect.Input) (atlasAccess, error) {
	if in.Config == nil {
		return atlasAccess{}, fmt.Errorf("atlassian: %w: no company document", integration.ErrNotConfigured)
	}
	cfg := in.Config.Integrations.Atlassian
	if cfg == nil {
		return atlasAccess{}, fmt.Errorf("atlassian: %w: there is no integrations.atlassian block",
			integration.ErrNotConfigured)
	}
	var org, key string
	if in.Env != nil {
		org = strings.TrimSpace(in.Env.Value(cfg.OrgID))
		key = strings.TrimSpace(in.Env.Value(cfg.APIKey))
	}
	if org == "" || key == "" {
		// A DATA CENTER COMPANY HAS NO ORGANIZATION, and nothing to list.
		return atlasAccess{}, fmt.Errorf("atlassian: %w: integrations.atlassian.org_id and api_key did not "+
			"both resolve", integration.ErrNotConfigured)
	}
	base := in.HTTP
	if base == nil {
		base = httpx.Client(ClientTimeout)
	}
	counted, calls := collect.Counted(base)
	access := atlasAccess{client: NewClient(ClientOptions{HTTP: counted}), calls: calls, org: org, key: key,
		named: map[string]bool{}}
	if j := in.Config.Integrations.Jira; j != nil {
		_, host := siteOf(j.ShareableBaseURL())
		access.named[strings.ToLower(host)] = true
	}
	if f := in.Config.Integrations.Confluence; f != nil {
		_, host := siteOf(cmp.Or(f.SiteURL, f.URL))
		access.named[strings.ToLower(host)] = true
	}
	return access, nil
}

// siteOf is a site's address as the atlas keeps it, its scheme and host and
// nothing else, and that host; both "" where the address names no host. A
// site has no path, and a path, a query or user information is where a copy
// of an address carries what it should not. THE HOST IS AS IT WAS WRITTEN,
// for the builder to judge before the grammar lowers it: a credential's
// shape can live in its case.
func siteOf(address string) (site, host string) {
	u, err := url.Parse(strings.TrimSpace(address))
	if err != nil || u.Hostname() == "" {
		return "", ""
	}
	return cmp.Or(u.Scheme, "https") + "://" + u.Host, u.Hostname()
}

// Collect reads the organization's products and reports them as claims in
// the integration's voice. It writes nothing at Atlassian.
func (atlasSource) Collect(ctx context.Context, in collect.Input) (collect.Read, error) {
	access, err := openAtlas(in)
	if err != nil {
		return collect.Read{}, err
	}
	b := collect.NewBuilder(integration.KindAtlassian, in.Now)
	// THE KEY THE SETTINGS GIVE THE ORGANIZATION, so the two name one entry.
	org, ok := b.Ref("org", atlas.KindAccount, accountKeyPrefix+access.org)
	if !ok {
		return collect.Read{}, fmt.Errorf("atlassian: the organization %q is not an entry the atlas can name",
			access.org)
	}
	workspaces, err := access.client.Workspaces(ctx, access.key, access.org)
	switch {
	case err == nil:
		readWorkspaces(b, org, access.named, workspaces)
	case Status(err) == http.StatusForbidden:
		// A KEY ATLASSIAN KNOWS AND REFUSES THE LISTING was created with
		// scopes that leave it out, which is a finding: a key it does not
		// know answers 401.
		b.Unread(collect.Unread{Part: partWorkspaces,
			Detail: "The organization's products were not read: Atlassian refused the key in " +
				"integrations.atlassian.api_key. Use a key created without scopes, which is the one the " +
				"agents' accounts need as well.",
			Holds: func(atlas.Claim) bool { return true }})
	default:
		// ANYTHING ELSE IS A READ THAT DID NOT FINISH, Atlassian's rate limit
		// included, and fails whole: a listing cut short would retire what
		// it did not reach.
		return collect.Read{}, fmt.Errorf("atlassian: list the organization's products: %w",
			integration.Reject(err, Status(err)))
	}
	b.Calls(access.calls())
	return b.Read(), nil
}

// readWorkspaces says what each product makes of the account it is on: a
// site's, by the key the settings give a site, or the organization's.
func readWorkspaces(b *collect.Builder, org atlas.Ref, named map[string]bool, workspaces []Workspace) {
	slices.SortFunc(workspaces, func(a, z Workspace) int { return cmp.Compare(a.ID, z.ID) })
	for _, w := range workspaces {
		where := "workspaces[" + w.ID + "]"
		capability := string(capabilityOf(w.Product()))
		cloudID := w.CloudID()
		if cloudID == "" {
			b.Says(where+".typeKey", org, atlas.FactCapability, capability)
			continue
		}
		// A SITE WITH NO ADDRESS IS KEYED BY ITS CLOUD ID, as the settings
		// key one whose address they do not carry.
		address, host := siteOf(w.HostURL)
		site, ok := b.Ref(where+".hostUrl", atlas.KindAccount, accountKeyPrefix+cmp.Or(host, cloudID))
		if !ok {
			continue
		}
		b.Says(where+".id", site, atlas.FactVendor, string(atlas.VendorAtlassian))
		b.Says(where+".id", site, atlas.FactAccountRef, cloudID)
		b.Says(where+".typeKey", site, atlas.FactCapability, capability)
		// THE SETTINGS NAME THE SITES THE ENGINE WORKS IN, and two names
		// would be a disagreement for a person to settle; any other site is
		// named and placed here.
		if host != "" && !named[strings.ToLower(host)] {
			// THE KEY'S OWN HOST, which the grammar has judged and lowered.
			b.Says(where+".hostUrl", site, atlas.FactName, strings.TrimPrefix(site.Key, accountKeyPrefix))
			b.Says(where+".hostUrl", site, atlas.FactSite, address)
		}
	}
}

// capabilityOf is what a product says its account does, "" for one that
// says nothing: every Jira is a tracker, by its key's first word, and a
// product this build does not know names no capability. A switch, so the
// table costs nothing at init.
func capabilityOf(product string) atlas.Capability {
	family, _, _ := strings.Cut(product, "-")
	switch family {
	case "jira":
		return atlas.CapabilityTracker
	case "confluence":
		return atlas.CapabilityKnowledge
	case "bitbucket":
		return atlas.CapabilityCodeHosting
	case "opsgenie":
		return atlas.CapabilityAlerting
	}
	return ""
}
