package derive

import (
	"errors"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
)

// Settings is the company settings as the atlas reads them: ALWAYS the
// redacting copy ([config.Company.Redact]), held where no caller can swap it
// for the live document, so a credential cannot reach a claim even through a
// bug in a derivation that reads the wrong field.
type Settings struct {
	company    *config.Company
	revision   string
	deployment Deployment
}

// Deployment is the node settings the atlas reads beside the company's: the
// addresses the deployment answers at. Two addresses, never the node file:
// the node file holds the keyring and the API tokens, and has no redacting copy.
type Deployment struct {
	// APIAddress is api.external_url, where a browser reaches the deployment.
	APIAddress string
	// PublicAddress is api.public.external_url, where vendors reach it; ""
	// where it shares the API's.
	PublicAddress string
}

// FromSettings is the settings the atlas reads from c at revision, redacted
// here and nowhere else.
func FromSettings(c *config.Company, revision string, d Deployment) Settings {
	return Settings{company: c.Redact(), revision: revision, deployment: d}
}

// Revision is the settings revision the claims are read from.
func (s Settings) Revision() string { return s.revision }

// Location is the company's clock, which its days of use are counted on.
func (s Settings) Location() *time.Location {
	if s.company == nil {
		return time.UTC
	}
	return s.company.Location()
}

// ToolServers is the name of every tool server the agents may use.
func (s Settings) ToolServers() []string {
	if s.company == nil {
		return nil
	}
	var out []string
	for i := range s.company.MCPServers {
		if m := &s.company.MCPServers[i]; m.IsEnabled() {
			out = append(out, m.Name)
		}
	}
	return out
}

// SSHHost is the server the settings declare under id, as the redacted copy
// holds it: what a connection test was run against is judged by it.
func (s Settings) SSHHost(id string) (config.SSHHost, bool) {
	if s.company == nil {
		return config.SSHHost{}, false
	}
	return s.company.Integrations.SSH.Host(id)
}

// Claims is every claim the settings make, as of at: the accounts of the
// integrations they connect, the zones they declare, the servers, the tool
// servers, the models, the sandbox and the addresses the deployment answers at.
func (s Settings) Claims(at time.Time) Derived { return s.derive(at).out }

// Accounts is the account each connected integration is, as of at: where the
// engine's records about an integration (its status, its agents' accounts)
// attach.
func (s Settings) Accounts(at time.Time) map[integration.Kind]atlas.Ref { return s.derive(at).accounts }

func (s Settings) derive(at time.Time) *builder {
	b := newBuilder(atlas.SettingsSource(s.revision), at)
	b.accounts = map[integration.Kind]atlas.Ref{}
	c := s.company
	if c == nil {
		return b
	}
	zones := map[string]atlas.Ref{}
	for _, kind := range integration.Kinds {
		if derive := integrationRows[kind]; derive != nil {
			b.kind = kind
			derive(b, c, zones)
		}
	}
	b.kind = ""
	for _, address := range []struct{ where, raw, name string }{
		{"api.external_url", s.deployment.APIAddress, "Crewlet"},
		{"api.public.external_url", s.deployment.PublicAddress, "Crewlet for vendors"},
	} {
		deploymentAddress(b, address.where, address.raw, address.name, zones)
	}
	models(b, c)
	toolServers(b, c)
	sandbox(b, c)
	if c.Code.Backend == config.CodeGitlet {
		b.account("code.backend", atlas.VendorGitlet, "", "", "Gitlet", true,
			atlas.CapabilityCodeHosting, atlas.CapabilityCI)
	}
	return b
}

// integrationRows derives each integration's claims from its settings. Every
// integration kind has a row: an integration the engine connects is an
// account the company holds, or for SSH the servers it declares.
var integrationRows = map[integration.Kind]func(b *builder, c *config.Company, zones map[string]atlas.Ref){
	integration.KindSlack:        slack,
	integration.KindMattermost:   mattermost,
	integration.KindJira:         jira,
	integration.KindConfluence:   confluence,
	integration.KindGitHub:       github,
	integration.KindGitLab:       gitlab,
	integration.KindDatadog:      datadog,
	integration.KindCloudflare:   cloudflare,
	integration.KindScaleway:     scaleway,
	integration.KindExpo:         expo,
	integration.KindDigitalOcean: digitalocean,
	integration.KindAWS:          aws,
	integration.KindGoogleCloud:  googleCloud,
	integration.KindSSH:          sshHosts,
	integration.KindAtlassian:    atlassian,
}

var (
	errUnreadConnection = errors.New("not the stack's connection: five ARNs joined by commas")
	errNoHost           = errors.New("not an address with a host")
)

// account claims the company's account at vendor: keyed by name, the most
// specific identifier the settings carry, or by the vendor alone where they
// carry none; id is the vendor's own account id, where the settings carry one.
func (b *builder) account(where string, vendor atlas.Vendor, name, id, label string, on bool,
	capabilities ...atlas.Capability) (atlas.Ref, bool) {
	key := string(vendor)
	if name = strings.TrimSpace(name); name != "" && !b.reference(where, name) {
		key += "/" + name
	}
	r, ok := b.ref(where, atlas.KindAccount, key)
	if !ok {
		return r, false
	}
	if b.kind != "" {
		b.accounts[b.kind] = r
	}
	b.says(where, r, atlas.FactName, label)
	b.says(where, r, atlas.FactVendor, string(vendor))
	b.says(where, r, atlas.FactIntegration, onOff(on))
	b.says(where, r, atlas.FactAccountRef, id)
	for _, capability := range capabilities {
		b.says(where, r, atlas.FactCapability, string(capability))
	}
	return r, true
}

// hostOf is the host an address names, "" where it names none.
func hostOf(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

func slack(b *builder, c *config.Company, _ map[string]atlas.Ref) {
	if c.Integrations.Slack == nil {
		return
	}
	b.account("integrations.slack", atlas.VendorSlack, "", "", "Slack", true, atlas.CapabilityChat)
}

func mattermost(b *builder, c *config.Company, _ map[string]atlas.Ref) {
	m := c.Integrations.Mattermost
	if m == nil {
		return
	}
	const where = "integrations.mattermost.url"
	if r, ok := b.account(where, atlas.VendorMattermost, hostOf(m.URL), "", "Mattermost", m.Enabled,
		atlas.CapabilityChat); ok {
		b.says(where, r, atlas.FactSite, m.URL)
	}
}

// atlassianSite claims an Atlassian site the tracker or the knowledge base
// works in: keyed by its host, and carrying its cloud id, which is what makes
// Jira and Confluence on one site one account.
func atlassianSite(b *builder, where, site, cloudID, label string, capability atlas.Capability) {
	host := hostOf(site)
	if host == "" {
		host = strings.ToLower(strings.TrimSpace(cloudID))
	}
	if r, ok := b.account(where, atlas.VendorAtlassian, host, cloudID, label, true, capability); ok {
		b.says(where, r, atlas.FactSite, site)
	}
}

func jira(b *builder, c *config.Company, _ map[string]atlas.Ref) {
	if j := c.Integrations.Jira; j != nil {
		atlassianSite(b, "integrations.jira", j.ShareableBaseURL(), j.CloudID, "Jira", atlas.CapabilityTracker)
	}
}

func confluence(b *builder, c *config.Company, _ map[string]atlas.Ref) {
	f := c.Integrations.Confluence
	if f == nil {
		return
	}
	site := f.SiteURL
	if site == "" {
		site = f.URL
	}
	atlassianSite(b, "integrations.confluence", site, f.CloudID, "Confluence", atlas.CapabilityKnowledge)
}

func atlassian(b *builder, c *config.Company, _ map[string]atlas.Ref) {
	if a := c.Integrations.Atlassian; a != nil {
		b.account("integrations.atlassian.org_id", atlas.VendorAtlassian, a.OrgID, a.OrgID, "Atlassian", true,
			atlas.CapabilityIdentity)
	}
}

// forge claims a code host's account and the repositories the settings name
// on it: host is the forge's, group the organisation or group, paths each
// repository's path on the host.
func forge(b *builder, where string, vendor atlas.Vendor, web, group, label string, on bool, paths []string) {
	host := hostOf(web)
	name := host
	if group = strings.ToLower(strings.TrimSpace(group)); group != "" {
		name += "/" + group
	}
	account, ok := b.account(where, vendor, name, group, label, on, atlas.CapabilityCodeHosting, atlas.CapabilityCI)
	if !ok {
		return
	}
	b.says(where, account, atlas.FactSite, web)
	for i, p := range paths {
		at := where + ".repos[" + strconv.Itoa(i) + "]"
		if !strings.Contains(p, "/") && group != "" {
			p = group + "/" + p
		}
		// THE KEY THE FORGE'S OWN READ AND A MIRROR'S ADDRESS REDUCE TO, so a
		// forge served under a path of its host still names one entry.
		key, err := atlas.ForgeRepositoryKey(web, p)
		if err != nil {
			if !b.reference(at, p) {
				b.withhold(at, err)
			}
			continue
		}
		if r, ok := b.ref(at, atlas.KindRepository, key); ok {
			b.relates(at, r, atlas.RelationBelongsTo, account)
		}
	}
}

func github(b *builder, c *config.Company, _ map[string]atlas.Ref) {
	g := c.Integrations.GitHub
	if g == nil {
		return
	}
	var org string
	var repos []string
	if p := g.Provisioning; p != nil {
		org, repos = p.Org, p.Repos
	}
	forge(b, "integrations.github", atlas.VendorGitHub, g.WebURL(), org, "GitHub", g.Enabled, repos)
}

func gitlab(b *builder, c *config.Company, _ map[string]atlas.Ref) {
	g := c.Integrations.GitLab
	if g == nil {
		return
	}
	var group string
	var projects []string
	if p := g.Provisioning; p != nil {
		group, projects = p.Group, p.Projects
	}
	forge(b, "integrations.gitlab", atlas.VendorGitLab, g.URL, group, "GitLab", g.Enabled, projects)
}

func datadog(b *builder, c *config.Company, _ map[string]atlas.Ref) {
	d := c.Integrations.Datadog
	if d == nil {
		return
	}
	var site string
	if d.Provisioning != nil {
		site = d.Provisioning.Site
	}
	b.account("integrations.datadog.provisioning.site", atlas.VendorDatadog, site, "", "Datadog", d.Enabled,
		atlas.CapabilityMonitoring, atlas.CapabilityAlerting)
}

func cloudflare(b *builder, c *config.Company, zones map[string]atlas.Ref) {
	f := c.Integrations.Cloudflare
	if f == nil {
		return
	}
	var id string
	if f.Provisioning != nil {
		id = f.Provisioning.AccountID
	}
	account, ok := b.account("integrations.cloudflare.provisioning.account_id", atlas.VendorCloudflare, id, id,
		"Cloudflare", f.Enabled, atlas.CapabilityDNS, atlas.CapabilityCDN)
	if !ok {
		return
	}
	for i, zone := range f.ZoneNames() {
		where := "integrations.cloudflare.provisioning.zones[" + strconv.Itoa(i) + "]"
		r, ok := b.ref(where, atlas.KindDomain, config.CloudflareZoneKey(zone))
		if !ok {
			continue
		}
		zones[r.Key] = r
		b.relates(where, r, atlas.RelationServedBy, account)
		b.relates(where, r, atlas.RelationBelongsTo, account)
	}
}

func scaleway(b *builder, c *config.Company, _ map[string]atlas.Ref) {
	s := c.Integrations.Scaleway
	if s == nil {
		return
	}
	var id string
	if s.Provisioning != nil {
		id = config.ScalewayID(s.Provisioning.OrganizationID)
	}
	b.account("integrations.scaleway.provisioning.organization_id", atlas.VendorScaleway, id, id, "Scaleway",
		s.Enabled, atlas.CapabilityCompute)
}

func expo(b *builder, c *config.Company, _ map[string]atlas.Ref) {
	e := c.Integrations.Expo
	if e == nil {
		return
	}
	var name string
	if e.Provisioning != nil {
		name = strings.ToLower(e.Provisioning.AccountName)
	}
	b.account("integrations.expo.provisioning.account_name", atlas.VendorExpo, name, name, "Expo", e.Enabled,
		atlas.CapabilityAppDelivery)
}

func digitalocean(b *builder, c *config.Company, _ map[string]atlas.Ref) {
	d := c.Integrations.DigitalOcean
	if d == nil {
		return
	}
	var team string
	if d.Provisioning != nil {
		// A NAME WITH A SPACE IS NO KEY: a team is keyed by its name's slug,
		// as the read of the team keys it. A reference stays one, unresolved.
		if team = d.Provisioning.Team; !strings.Contains(team, "${") {
			team = config.DigitalOceanTeamRef(team)
		}
	}
	b.account("integrations.digitalocean.provisioning.team", atlas.VendorDigitalOcean, team, team, "DigitalOcean",
		d.Enabled, atlas.CapabilityCompute)
}

func googleCloud(b *builder, c *config.Company, _ map[string]atlas.Ref) {
	g := c.Integrations.GoogleCloud
	if g == nil {
		return
	}
	var project string
	if g.Provisioning != nil {
		project, _ = config.GoogleCloudServiceAccount(g.Provisioning.Provisioner)
	}
	b.account("integrations.google_cloud.provisioning.provisioner", atlas.VendorGoogleCloud, project, project,
		"Google Cloud", g.Enabled, atlas.CapabilityCompute)
}

func aws(b *builder, c *config.Company, _ map[string]atlas.Ref) {
	a := c.Integrations.AWS
	if a == nil {
		return
	}
	const where = "integrations.aws.provisioning.connection"
	var id string
	if p := a.Provisioning; p != nil && p.Connection != "" && !b.reference(where, p.Connection) {
		conn, err := config.ParseAWSConnection(p.Slug, p.HomeRegion, p.Connection)
		if err != nil {
			b.withhold(where, errUnreadConnection)
		} else {
			id = conn.Account
		}
	}
	r, ok := b.account(where, atlas.VendorAWS, id, id, "AWS", a.Enabled, atlas.CapabilityCompute)
	if !ok {
		return
	}
	for _, region := range a.RegionList() {
		b.says("integrations.aws.provisioning.regions", r, atlas.FactRegion, region)
	}
}

func sshHosts(b *builder, c *config.Company, _ map[string]atlas.Ref) {
	s := c.Integrations.SSH
	if s == nil {
		return
	}
	for i, h := range s.Hosts {
		where := "integrations.ssh.hosts[" + strconv.Itoa(i) + "]"
		r, ok := b.ref(where, atlas.KindHost, atlas.HostSSHPrefix+"/"+h.ID)
		if !ok {
			continue
		}
		b.says(where, r, atlas.FactName, h.ID)
		b.says(where+".address", r, atlas.FactAddress, h.Address)
		b.says(where+".port", r, atlas.FactPort, strconv.Itoa(h.PortOrDefault()))
		b.says(where+".containers", r, atlas.FactContainers, yesNo(h.Containers))
	}
}

// deploymentAddress claims the host name the deployment answers at, in the
// zone the settings declare above it.
func deploymentAddress(b *builder, where, raw, label string, zones map[string]atlas.Ref) {
	host := hostOf(raw)
	if strings.TrimSpace(raw) == "" || b.reference(where, raw) {
		return
	}
	if host == "" {
		b.withhold(where, errNoHost)
		return
	}
	r, ok := b.ref(where, atlas.KindHostname, host)
	if !ok {
		return
	}
	b.says(where, r, atlas.FactName, label)
	b.says(where, r, atlas.FactURL, raw)
	if zone, ok := zoneOf(r.Key, zones); ok {
		b.relates(where, r, atlas.RelationInZone, zone)
	}
}

// zoneOf is the declared zone a host name sits in: the longest one it ends with.
func zoneOf(host string, zones map[string]atlas.Ref) (atlas.Ref, bool) {
	var best atlas.Ref
	for name, r := range zones {
		if (host == name || strings.HasSuffix(host, "."+name)) && len(name) > len(best.Key) {
			best = r
		}
	}
	return best, best.Key != ""
}

// harnessVendors is the vendor whose models each harness reaches. A harness
// that drives several vendors' models, or one the engine runs with a person's
// own configuration, reaches a vendor the settings do not name: other.
var harnessVendors = map[config.Harness]atlas.Vendor{
	config.HarnessClaudeCode:  atlas.VendorAnthropic,
	config.HarnessCodex:       atlas.VendorOpenAI,
	config.HarnessGeminiCLI:   atlas.VendorGoogle,
	config.HarnessQwenCode:    atlas.VendorOther,
	config.HarnessOpenCode:    atlas.VendorOther,
	config.HarnessCursorAgent: atlas.VendorCursor,
	config.HarnessCopilot:     atlas.VendorGitHub,
	config.HarnessGrok:        atlas.VendorXAI,
	config.HarnessMuseCode:    atlas.VendorOther,
	config.HarnessKimiCode:    atlas.VendorOther,
	config.HarnessHermes:      atlas.VendorOther,
	config.HarnessPi:          atlas.VendorOther,
	config.HarnessCustom:      atlas.VendorOther,
}

// signIns is how each sign-in mode reads in the atlas.
var signIns = map[config.HarnessAuthMode]string{
	config.AuthSubscription: "subscription",
	config.AuthAPIKey:       "api_key",
	config.AuthInheritEnv:   "inherit_env",
	config.AuthEnv:          "env",
}

func models(b *builder, c *config.Company) {
	for i, key := range c.Providers.ProviderOrder() {
		p := c.Providers.LLM[key]
		where := "providers.llm." + key
		r, ok := b.ref(where, atlas.KindModel, atlas.ModelLLMPrefix+"/"+key)
		if !ok {
			continue
		}
		b.says(where, r, atlas.FactName, key)
		b.says(where, r, atlas.FactVendor, string(harnessVendors[config.Harness(p.HarnessName())]))
		b.says(where+".harness", r, atlas.FactHarness, p.HarnessName())
		b.says(where+".model", r, atlas.FactModel, p.Model)
		b.says(where+".auth.mode", r, atlas.FactSignIn, signIns[p.ResolvedAuthMode()])
		b.says(where, r, atlas.FactChainPosition, strconv.Itoa(i+1))
	}
	e := c.Providers.Embeddings
	if e == nil {
		return
	}
	const where = "providers.embeddings"
	r, ok := b.ref(where, atlas.KindModel, atlas.ModelEmbeddings)
	if !ok {
		return
	}
	vendor := atlas.VendorOther
	if e.Type == config.EmbeddingOpenAI || e.Type == "" {
		vendor = atlas.VendorOpenAI
	}
	b.says(where, r, atlas.FactName, "Embeddings")
	b.says(where, r, atlas.FactVendor, string(vendor))
	b.says(where+".model", r, atlas.FactModel, e.Model)
	b.says(where+".base_url", r, atlas.FactURL, e.BaseURL)
}

// toolServers claims every tool server the agents may use. A stdio server is
// known by its command's base name ALONE: its arguments are not a secret
// position, and an argument is where a person passes a token by mistake.
func toolServers(b *builder, c *config.Company) {
	for i := range c.MCPServers {
		m := &c.MCPServers[i]
		if !m.IsEnabled() {
			continue
		}
		where := "mcp_servers[" + strconv.Itoa(i) + "]"
		r, ok := b.ref(where, atlas.KindToolServer, atlas.ToolServerPrefix+"/"+m.Name)
		if !ok {
			continue
		}
		b.says(where, r, atlas.FactName, m.Name)
		b.says(where+".transport", r, atlas.FactTransport, string(m.Kind()))
		b.says(where+".shared", r, atlas.FactShared, yesNo(m.IsShared()))
		if m.Kind() == config.TransportHTTP {
			b.says(where+".url", r, atlas.FactURL, m.URL)
			continue
		}
		if command := strings.TrimSpace(m.Command); command != "" && !b.reference(where+".command", command) {
			b.says(where+".command", r, atlas.FactCommand, path.Base(command))
		}
	}
}

// sandbox claims the vendor the code sandbox runs boxes at. A local sandbox
// runs on the engine's own nodes, which are the engine's components rather
// than an account of the company's.
func sandbox(b *builder, c *config.Company) {
	s := c.Providers.Sandbox
	if s == nil || s.E2B == nil {
		return
	}
	const where = "providers.sandbox.e2b.domain"
	b.account(where, atlas.VendorE2B, strings.ToLower(s.E2B.Domain), "", "E2B", true, atlas.CapabilityCompute)
}
