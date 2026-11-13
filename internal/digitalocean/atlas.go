package digitalocean

import (
	"cmp"
	"context"
	"crypto/subtle"
	"fmt"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/redact"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// DigitalOcean's half of the atlas (ADR-0146): the team's Droplets, clusters,
// databases, load balancers, domains, apps and Spaces buckets, read with a
// token a person made for the atlas alone. No agent's token is ever read
// with, and a token proven able to change anything reads nothing.

func init() { collect.Register(atlasSource{}) }

// atlasSource is DigitalOcean's [collect.Source].
type atlasSource struct{}

func (atlasSource) Kind() integration.Kind { return integration.KindDigitalOcean }

// The key namespaces of the entries that live at DigitalOcean.
const (
	keyPrefix         = string(atlas.VendorDigitalOcean) + "/"
	dropletKeyPrefix  = keyPrefix + "droplet/"
	clusterKeyPrefix  = keyPrefix + "kubernetes/"
	balancerKeyPrefix = keyPrefix + "load-balancer/"
	databaseKeyPrefix = keyPrefix + "database/"
	spaceKeyPrefix    = keyPrefix + "space/"
)

// part is one listing of the read, which a token may lack the scope of.
type part struct {
	name, scope, what string
}

// The parts of the read, in the order they are made.
var (
	partDroplets  = part{"droplets", "droplet:read", "Droplets"}
	partClusters  = part{"kubernetes", "kubernetes:read", "Kubernetes clusters"}
	partDatabases = part{"databases", "database:read", "Managed databases"}
	partBalancers = part{"load_balancers", "load_balancer:read", "Load balancers"}
	partDomains   = part{"domains", "domain:read", "Domains and their records"}
	partApps      = part{"apps", "app:read", "Apps"}
	partSpaces    = part{"spaces", "project:read", "Spaces buckets"}
)

// attaches reports the claim that an app is attached to a managed database,
// which takes both listings to make.
func attaches(c atlas.Claim) bool {
	return c.Subject.Kind == atlas.KindService && c.Facet.Kind == atlas.FacetRelation &&
		c.Facet.Relation == atlas.RelationDependsOn &&
		strings.HasPrefix(c.Facet.Target.Entry.Key, databaseKeyPrefix)
}

// holds reports whether a claim the voice holds came from p's listing, read
// off the claim alone. An app's attachment to a database came from both the
// apps and the databases, so a read refused either carries it.
func (p part) holds(c atlas.Claim) bool {
	s := c.Subject
	under := func(k atlas.Kind, prefix string) bool { return s.Kind == k && strings.HasPrefix(s.Key, prefix) }
	switch p {
	case partDroplets:
		return under(atlas.KindHost, dropletKeyPrefix)
	case partClusters:
		return under(atlas.KindHost, clusterKeyPrefix)
	case partBalancers:
		return under(atlas.KindHost, balancerKeyPrefix)
	case partDatabases:
		return under(atlas.KindDatastore, databaseKeyPrefix) || under(atlas.KindQueue, databaseKeyPrefix) ||
			attaches(c)
	case partDomains:
		return s.Kind == atlas.KindDomain || s.Kind == atlas.KindHostname ||
			under(atlas.KindHost, atlas.HostAddrPrefix+"/")
	case partApps:
		return s.Kind == atlas.KindService || s.Kind == atlas.KindEndpoint
	case partSpaces:
		return under(atlas.KindBucket, spaceKeyPrefix)
	}
	return false
}

// unread records p as a listing the token may not make.
func (p part) unread(b *collect.Builder) {
	b.Unread(collect.Unread{Part: p.name, Holds: p.holds,
		Detail: p.what + " were not read: the atlas's token lacks " + p.scope + ". DigitalOcean cannot " +
			"change a token's scopes, so create the token again with " + p.scope + " ticked and paste it."})
}

// atlasToken is the atlas's token and the team it reads, from the company's
// block, or [integration.ErrNotConfigured] where the company does not connect
// DigitalOcean or gave the atlas no token: the token is optional, and
// without it nothing at DigitalOcean is read.
func atlasToken(in collect.Input) (token, team string, err error) {
	if in.Config == nil {
		return "", "", fmt.Errorf("digitalocean: %w: no company document", integration.ErrNotConfigured)
	}
	cfg := in.Config.Integrations.DigitalOcean
	if cfg == nil || !cfg.Enabled || cfg.Provisioning == nil || cfg.Atlas == nil || in.Env == nil {
		return "", "", fmt.Errorf("digitalocean: %w: integrations.digitalocean is off or holds no atlas token",
			integration.ErrNotConfigured)
	}
	team = strings.TrimSpace(in.Env.Value(cfg.Provisioning.Team))
	token = strings.TrimSpace(in.Env.Value(cfg.Atlas.Token))
	if team == "" || token == "" {
		return "", "", fmt.Errorf("digitalocean: %w: the team and integrations.digitalocean.atlas.token did "+
			"not both resolve", integration.ErrNotConfigured)
	}
	return token, team, nil
}

// agentHolding is the agent whose sealed token is token, "" where none is.
// The atlas never reads as an agent: DigitalOcean's own audit log would then
// say the agent's token made the atlas's reads.
func agentHolding(in collect.Input, token string) string {
	for role := range in.Config.EachRole() {
		if role.Kind == org.KindHuman {
			continue
		}
		handle := role.Seat().Handle()
		sealed := strings.TrimSpace(in.Env.Value(
			"${" + seataccess.SinkName(integration.KindDigitalOcean, TokenField, handle) + "}"))
		if sealed != "" && subtle.ConstantTimeCompare([]byte(sealed), []byte(token)) == 1 {
			return handle
		}
	}
	return ""
}

// Collect proves the atlas's token changes nothing, then reads the team's
// resources and reports them as claims in the integration's voice. Every call
// it makes is a GET, or a check that cannot have an effect.
func (atlasSource) Collect(ctx context.Context, in collect.Input) (collect.Read, error) {
	token, team, err := atlasToken(in)
	if err != nil {
		return collect.Read{}, err
	}
	if agent := agentHolding(in, token); agent != "" {
		return collect.Read{}, fmt.Errorf("digitalocean: integrations.digitalocean.atlas.token is %s's own "+
			"token; create a token for the atlas alone and paste it", agent)
	}
	base := in.HTTP
	if base == nil {
		base = httpx.Client(ClientTimeout)
	}
	counted, calls := collect.Counted(base)
	client, err := NewClient(ClientOptions{Token: token, HTTP: counted})
	if err != nil {
		return collect.Read{}, err
	}
	checked, err := CheckAtlas(ctx, client, AtlasInput{Token: token, Team: team})
	if refusal, ok := IsRefusal(err); ok {
		// A TOKEN THE CHECK TURNS DOWN READS NOTHING, a token that can write
		// above all: the read fails, so nothing is recorded or retired.
		said := fmt.Errorf("digitalocean: the atlas's token is refused: %s %s", refusal.Detail, refusal.Hint)
		if refusal.dead {
			return collect.Read{}, fmt.Errorf("%w: %w", integration.ErrCredentialRejected, said)
		}
		return collect.Read{}, said
	}
	if err != nil {
		return collect.Read{}, fmt.Errorf("digitalocean: check the atlas's token: %w", reject(err))
	}
	b := collect.NewBuilder(integration.KindDigitalOcean, in.Now)
	// THE KEY THE SETTINGS GIVE THE ACCOUNT, so the two name one entry.
	account, ok := b.Ref("account.team.name", atlas.KindAccount, keyPrefix+config.DigitalOceanTeamRef(team))
	if !ok {
		return collect.Read{}, fmt.Errorf("digitalocean: the team %q is not an entry the atlas can name", team)
	}
	b.Says("account.team.name", account, atlas.FactAccountName, collect.PlainLabel(checked.Team.Name))
	r := &reader{client: client, b: b, account: account, lacks: checked.Lacks}
	steps := []func(context.Context) error{
		r.droplets, r.clusters, r.balancers, r.databases, r.domains, r.apps, r.spaces,
	}
	for _, step := range steps {
		if err := step(ctx); err != nil {
			return collect.Read{}, err
		}
	}
	b.Calls(calls())
	return b.Read(), nil
}

// reader is one read in flight.
type reader struct {
	client  *Client
	b       *collect.Builder
	account atlas.Ref
	lacks   []string

	// stores is each managed database by its name, for the apps attached
	// to one; empty where the listing was not made.
	stores map[string]atlas.Ref

	// answers is the Droplet or the load balancer answering on each public
	// address, for the names that point at one.
	answers map[netip.Addr]atlas.Ref
}

// answersOn says raw and, where it is a public address, remembers that host
// answers on it. Two entries of one voice sharing an address merge nothing
// in the fold, so a name is pointed at the resource itself.
func (r *reader) answersOn(where string, host atlas.Ref, raw string) {
	r.b.Says(where, host, atlas.FactAddress, raw)
	ip, err := netip.ParseAddr(strings.TrimSpace(raw))
	if err != nil || ip.IsPrivate() || ip.IsLoopback() || ip.IsLinkLocalUnicast() {
		return
	}
	if r.answers == nil {
		r.answers = map[netip.Addr]atlas.Ref{}
	}
	if _, taken := r.answers[ip.Unmap()]; !taken {
		r.answers[ip.Unmap()] = host
	}
}

// listed runs one part's listing. A scope the token lacks, proven before the
// read or answered 403 by the listing, is a finding and reads false; any other
// failure fails the read whole, since a listing cut short would retire what
// it did not reach.
func listed[T any](
	ctx context.Context, r *reader, p part, list func(context.Context) ([]T, error),
) ([]T, bool, error) {
	if slices.Contains(r.lacks, p.scope) {
		p.unread(r.b)
		return nil, false, nil
	}
	rows, err := list(ctx)
	switch {
	case err == nil:
		return rows, true, nil
	case Status(err) == http.StatusForbidden:
		p.unread(r.b)
		return nil, false, nil
	}
	return nil, false, fmt.Errorf("digitalocean: read the team's %s: %w", p.name, reject(err))
}

// held claims one resource of the team: its entry, that it belongs to the
// team's account, and its name.
func (r *reader) held(where string, k atlas.Kind, key, name string) (atlas.Ref, bool) {
	ref, ok := r.b.Ref(where+".id", k, key)
	if !ok {
		return atlas.Ref{}, false
	}
	r.b.Relates(where, ref, atlas.RelationBelongsTo, r.account)
	r.b.Says(where+".name", ref, atlas.FactName, collect.PlainLabel(name))
	return ref, true
}

func (r *reader) droplets(ctx context.Context) error {
	rows, ok, err := listed(ctx, r, partDroplets, r.client.Droplets)
	if !ok {
		return err
	}
	slices.SortFunc(rows, func(a, z Droplet) int { return cmp.Compare(a.ID, z.ID) })
	for _, d := range rows {
		id := strconv.FormatInt(d.ID, 10)
		where := "droplets[" + id + "]"
		host, ok := r.held(where, atlas.KindHost, dropletKeyPrefix+id, d.Name)
		if !ok {
			continue
		}
		r.b.Says(where+".region", host, atlas.FactRegion, string(d.Region))
		for _, n := range slices.Concat(d.Networks.V4, d.Networks.V6) {
			r.answersOn(where+".networks", host, n.Address)
		}
	}
	return nil
}

func (r *reader) clusters(ctx context.Context) error {
	rows, ok, err := listed(ctx, r, partClusters, r.client.Clusters)
	if !ok {
		return err
	}
	slices.SortFunc(rows, func(a, z Cluster) int { return cmp.Compare(a.ID, z.ID) })
	for _, c := range rows {
		where := "kubernetes_clusters[" + c.ID + "]"
		if host, ok := r.held(where, atlas.KindHost, clusterKeyPrefix+c.ID, c.Name); ok {
			r.b.Says(where+".region", host, atlas.FactRegion, string(c.Region))
		}
	}
	return nil
}

func (r *reader) balancers(ctx context.Context) error {
	rows, ok, err := listed(ctx, r, partBalancers, r.client.Balancers)
	if !ok {
		return err
	}
	slices.SortFunc(rows, func(a, z Balancer) int { return cmp.Compare(a.ID, z.ID) })
	for _, lb := range rows {
		where := "load_balancers[" + lb.ID + "]"
		host, ok := r.held(where, atlas.KindHost, balancerKeyPrefix+lb.ID, lb.Name)
		if !ok {
			continue
		}
		r.b.Says(where+".region", host, atlas.FactRegion, string(lb.Region))
		r.answersOn(where+".ip", host, lb.IP)
		r.answersOn(where+".ipv6", host, lb.IPv6)
	}
	return nil
}

// engineWord is the atlas's word for an engine as DigitalOcean names it.
func engineWord(engine string) string {
	switch engine {
	case "pg", "advanced_pg":
		return "postgres"
	case "mysql", "advanced_mysql":
		return "mysql"
	case "redis", "valkey", "mongodb", "kafka", "opensearch":
		return engine
	}
	return "other"
}

func (r *reader) databases(ctx context.Context) error {
	rows, ok, err := listed(ctx, r, partDatabases, r.client.Databases)
	if !ok {
		return err
	}
	r.stores = map[string]atlas.Ref{}
	slices.SortFunc(rows, func(a, z Database) int { return cmp.Compare(a.ID, z.ID) })
	for _, d := range rows {
		where := "databases[" + d.ID + "]"
		kind := atlas.KindDatastore
		if d.Engine == "kafka" {
			kind = atlas.KindQueue
		}
		store, ok := r.held(where, kind, databaseKeyPrefix+d.ID, d.Name)
		if !ok {
			continue
		}
		if _, dup := r.stores[d.Name]; !dup && d.Name != "" {
			r.stores[d.Name] = store
		}
		r.b.Relates(where, store, atlas.RelationRunsOn, r.account)
		r.b.Says(where+".engine", store, atlas.FactEngine, engineWord(d.Engine))
		r.b.Says(where+".version", store, atlas.FactEngineVersion, d.Version)
		r.b.Says(where+".region", store, atlas.FactRegion, string(d.Region))
	}
	return nil
}

func (r *reader) domains(ctx context.Context) error {
	rows, ok, err := listed(ctx, r, partDomains, r.client.Domains)
	if !ok {
		return err
	}
	slices.SortFunc(rows, func(a, z Domain) int { return cmp.Compare(a.Name, z.Name) })
	// EVERY DOMAIN FIRST, so a CNAME into another of the team's domains finds
	// its zone whatever order the domains were listed in.
	var zones []atlas.Ref
	for _, d := range rows {
		where := "domains[" + d.Name + "]"
		zone, ok := r.b.Ref(where+".name", atlas.KindDomain, d.Name)
		if !ok {
			continue
		}
		zones = append(zones, zone)
		r.b.Relates(where, zone, atlas.RelationServedBy, r.account)
		r.b.Relates(where, zone, atlas.RelationBelongsTo, r.account)
	}
	for _, zone := range zones {
		for _, recordType := range []string{recordA, recordAAAA, recordCNAME} {
			records, err := r.client.Records(ctx, zone.Key, recordType)
			if err != nil {
				return fmt.Errorf("digitalocean: read the %s records of a domain: %w", recordType, reject(err))
			}
			r.records(zone, zones, recordType, records)
		}
	}
	return nil
}

// absolute is a record's name or target as a whole name: DigitalOcean writes
// the zone itself as "@" and a name inside it without the zone.
func absolute(name, zone string) string {
	name = strings.TrimSpace(name)
	switch {
	case name == "@" || name == "":
		return zone
	case strings.HasSuffix(name, "."):
		return name
	case name == zone || strings.HasSuffix(name, "."+zone):
		return name
	}
	return name + "." + zone
}

// records claims each name a domain's records of one type give and what it
// points at: another name for a CNAME, a server by its address otherwise.
func (r *reader) records(zone atlas.Ref, zones []atlas.Ref, recordType string, records []Record) {
	slices.SortFunc(records, func(a, z Record) int { return cmp.Compare(a.ID, z.ID) })
	for _, rec := range records {
		if rec.Type != recordType {
			continue
		}
		at := "domains[" + zone.Key + "].records[" + strconv.FormatInt(rec.ID, 10) + "]"
		host, ok := r.b.Ref(at+".name", atlas.KindHostname, absolute(rec.Name, zone.Key))
		if !ok {
			continue
		}
		r.b.Relates(at+".name", host, atlas.RelationInZone, zone)
		if recordType == recordCNAME {
			// A TARGET WITH NO DOT is a name inside the zone; any other is
			// whole as DigitalOcean wrote it.
			data := strings.TrimSpace(rec.Data)
			if data == "@" || !strings.Contains(data, ".") {
				data = absolute(data, zone.Key)
			}
			target, ok := r.b.Ref(at+".data", atlas.KindHostname, data)
			if !ok {
				continue
			}
			r.b.Relates(at+".data", host, atlas.RelationResolvesTo, target)
			if owner, found := collect.ZoneOf(target, zones); found {
				r.b.Relates(at+".data", target, atlas.RelationInZone, owner)
			}
			continue
		}
		// AN ADDRESS ONE OF THE TEAM'S OWN RESOURCES ANSWERS ON is that
		// resource; any other is a server known by its address alone.
		if ip, err := netip.ParseAddr(strings.TrimSpace(rec.Data)); err == nil {
			if origin, ours := r.answers[ip.Unmap()]; ours {
				r.b.Relates(at+".data", host, atlas.RelationResolvesTo, origin)
				continue
			}
		}
		key, err := atlas.AddressHostKey(rec.Data)
		if err != nil {
			r.b.Withhold(at+".data", err)
			continue
		}
		origin, ok := r.b.Ref(at+".data", atlas.KindHost, key)
		if !ok {
			continue
		}
		r.b.Says(at+".data", origin, atlas.FactAddress, rec.Data)
		r.b.Relates(at+".data", host, atlas.RelationResolvesTo, origin)
	}
}

// The web addresses of the forges an app's component names a repository on.
const (
	githubWeb = "https://github.com"
	gitlabWeb = "https://gitlab.com"
)

func (r *reader) apps(ctx context.Context) error {
	rows, ok, err := listed(ctx, r, partApps, r.client.Apps)
	if !ok {
		return err
	}
	slices.SortFunc(rows, func(a, z App) int { return cmp.Compare(a.ID, z.ID) })
	for _, a := range rows {
		where := "apps[" + a.ID + "]"
		// AN APP IS THE SERVICE ITS NAME NAMES, so what a monitor or a
		// repository says of a service of that name is said of this one.
		app, ok := r.b.Ref(where+".spec.name", atlas.KindService, a.Spec.Name)
		if !ok {
			continue
		}
		r.b.Says(where+".spec.name", app, atlas.FactName, a.Spec.Name)
		r.b.Relates(where, app, atlas.RelationBelongsTo, r.account)
		r.b.Relates(where, app, atlas.RelationRunsOn, r.account)
		r.exposes(where+".live_url", app, a.LiveURL)
		for i, d := range a.Spec.Domains {
			if name := strings.TrimSpace(d.Domain); name != "" {
				r.exposes(where+".spec.domains["+strconv.Itoa(i)+"]", app, "https://"+name)
			}
		}
		for _, c := range a.components() {
			if c.GitHub != nil {
				r.builtFrom(where+".spec.github.repo", app, githubWeb, c.GitHub.Repo)
			}
			if c.GitLab != nil {
				r.builtFrom(where+".spec.gitlab.repo", app, gitlabWeb, c.GitLab.Repo)
			}
		}
		for _, d := range a.Spec.Databases {
			// A DEV DATABASE lives inside the app and is no cluster of the team.
			if store, found := r.stores[d.ClusterName]; found && d.Production {
				r.b.Relates(where+".spec.databases", app, atlas.RelationDependsOn, store)
			}
		}
	}
	return nil
}

// exposes claims the address raw and that app answers on it.
func (r *reader) exposes(where string, app atlas.Ref, raw string) {
	if strings.TrimSpace(raw) == "" {
		return
	}
	address, _, err := collect.Address(raw)
	if err != nil {
		r.b.Withhold(where, err)
		return
	}
	if endpoint, ok := r.b.Ref(where, atlas.KindEndpoint, address); ok {
		r.b.Relates(where, app, atlas.RelationExposes, endpoint)
	}
}

// builtFrom relates app to the repository repo names on the forge at web. The
// repository is named and never claimed: it lives at the forge.
func (r *reader) builtFrom(where string, app atlas.Ref, web, repo string) {
	if strings.TrimSpace(repo) == "" {
		return
	}
	key, err := atlas.ForgeRepositoryKey(web, repo)
	if err != nil {
		r.b.Withhold(where, err)
		return
	}
	if repository, ok := r.b.Names(where, atlas.KindRepository, key); ok {
		r.b.Relates(where, app, atlas.RelationBuiltFrom, repository)
	}
}

func (r *reader) spaces(ctx context.Context) error {
	rows, ok, err := listed(ctx, r, partSpaces, r.client.Spaces)
	if !ok {
		return err
	}
	slices.Sort(rows)
	for i, name := range slices.Compact(rows) {
		where := "spaces[" + strconv.Itoa(i) + "]"
		// A NAME SHAPED LIKE A CREDENTIAL IS REFUSED BEFORE IT IS A KEY
		// SEGMENT, whose escapes would reshape it past every rule.
		if redact.Contains(name) {
			r.b.Withhold(where+".name", fmt.Errorf("%s key: %w", atlas.KindBucket, atlas.ErrCredential))
			continue
		}
		r.held(where, atlas.KindBucket, spaceKeyPrefix+collect.KeySegment(name), name)
	}
	return nil
}
