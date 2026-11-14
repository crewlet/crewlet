package aws

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/integration"
)

// AWS's half of the atlas (ADR-0148): what the account runs, read whole as
// the atlas's own role, which the company adds to its stack by choice
// (`integrations.aws.provisioning.atlas_read`). Never as the provisioner,
// which reads only IAM, and never as an agent, whose calls CloudTrail names.
//
// EACH LISTING OF EACH REGION IS REFUSED ON ITS OWN: an organization's policy
// may close a Region or a service to every role in the account. What AWS
// refuses is a finding on the source's standing, and what an earlier read
// recorded there stands.

func init() { collect.Register(atlasSource{}) }

// atlasSource is AWS's [collect.Source].
type atlasSource struct{}

func (atlasSource) Kind() integration.Kind { return integration.KindAWS }

// On reports whether c adds the atlas's role to its stack: the reader is the
// company's choice, so it is a source only once asked for ([collect.Optional]).
func (atlasSource) On(c *config.Company) bool {
	return c != nil && c.Integrations.AWS != nil && c.Integrations.AWS.Provisioning != nil &&
		c.Integrations.AWS.Provisioning.AtlasRead
}

// The parts of AWS a read can be refused.
const (
	partReader        = "reader"
	partAccount       = "account"
	partInstances     = "instances"
	partDatabases     = "databases"
	partLoadBalancers = "load_balancers"
	partQueues        = "queues"
	partBuckets       = "buckets"
	partDNS           = "dns"
)

// regionalParts is every listing made once a Region, in the order read.
var regionalParts = []string{partInstances, partDatabases, partLoadBalancers, partQueues}

// accountKeyPrefix starts the key of the account and of everything in it.
const accountKeyPrefix = string(atlas.VendorAWS) + "/"

// Collect reads the account's Regions, instances, databases, load balancers,
// queues, buckets and DNS, and reports them as claims in the integration's
// voice. It writes nothing at AWS.
func (atlasSource) Collect(ctx context.Context, in collect.Input) (collect.Read, error) {
	if !(atlasSource{}).On(in.Config) {
		return collect.Read{}, fmt.Errorf("aws: %w: integrations.aws.provisioning.atlas_read is off, so the "+
			"account is not read for the atlas", integration.ErrNotConfigured)
	}
	base := in.HTTP
	if base == nil {
		base = httpx.Client(ClientTimeout)
	}
	counted, calls := collect.Counted(base)
	client, err := ClientFor(in.Config, in.Env, counted)
	if err != nil {
		return collect.Read{}, err
	}
	b := collect.NewBuilder(integration.KindAWS, in.Now)
	reader, err := client.NewAtlasClient()
	if errors.Is(err, ErrNoAtlasReader) {
		// ASKED FOR AND NOT IN THE STACK YET: a step somebody has to take,
		// said on the source's standing, with everything recorded standing.
		b.Unread(collect.Unread{Part: partReader, Holds: func(atlas.Claim) bool { return true },
			Detail: "The stack output does not carry Atlas's role yet. Run the CloudShell block on the AWS " +
				"connection again and paste its last line."})
		return b.Read(), nil
	}
	if err != nil {
		return collect.Read{}, err
	}
	r := &atlasRead{client: reader, b: b, refused: map[string][]string{}, inUse: map[string]bool{}}
	if err = r.read(ctx); err != nil {
		return collect.Read{}, err
	}
	b.Calls(calls())
	return b.Read(), nil
}

// atlasRead is one read in flight.
type atlasRead struct {
	client  *AtlasClient
	b       *collect.Builder
	account atlas.Ref
	// refused is the Regions each regional part was refused in.
	refused map[string][]string
	// inUse is every Region the read found something in.
	inUse map[string]bool
}

// refused reports AWS refusing the atlas's role a listing, as opposed to
// failing to answer it or refusing the session itself: a policy that does
// not allow the action there. BY ITS CODE, never by a 403 alone, which AWS
// also answers for a signature or a session it does not accept.
func refused(err error) bool {
	var api *APIError
	if !errors.As(err, &api) {
		return false
	}
	return strings.Contains(api.Code, "AccessDenied") || api.Code == "UnauthorizedOperation"
}

// reject marks err as the credential's refusal where AWS answered 401 or 403.
func reject(err error) error { return integration.Reject(err, Status(err)) }

// entry is the entry of kind k that id names in region, keyed by the
// account, the Region and the resource's own identifier there, and its
// exists claim; false where it is withheld.
func (r *atlasRead) entry(where string, k atlas.Kind, region, id string) (atlas.Ref, bool) {
	segment, ok := r.b.Segment(where, k, id)
	if !ok {
		return atlas.Ref{}, false
	}
	return r.b.Ref(where, k, accountKeyPrefix+r.client.Account()+"/"+region+"/"+segment)
}

// read is the read itself: the Regions, then each Region's listings, then
// the account's buckets and DNS.
func (r *atlasRead) read(ctx context.Context) error {
	b := r.b
	// THE REGIONS FIRST, AND THEY ARE NOT OPTIONAL: the listing proves the
	// sign-in and the role, so a later refusal is a policy's.
	regions, err := r.client.Regions(ctx)
	if err != nil {
		return fmt.Errorf("aws: list the account's Regions as %s: %w", r.client.conn.resolve(atlasName), reject(err))
	}
	// THE KEY THE SETTINGS GIVE THE ACCOUNT, so the two name one entry.
	account, ok := b.Ref("account", atlas.KindAccount, accountKeyPrefix+r.client.Account())
	if !ok {
		return fmt.Errorf("aws: the account %q is not an entry the atlas can name", r.client.Account())
	}
	r.account = account
	alias, err := r.client.AccountAlias(ctx)
	switch {
	case err == nil:
		b.Says("account.alias", account, atlas.FactAccountName, collect.PlainLabel(alias))
	case refused(err):
		b.Unread(collect.Unread{Part: partAccount, Detail: "The account's alias was not read: AWS refused " +
			"Atlas's role. " + rerunForAtlas,
			Holds: func(c atlas.Claim) bool { return c.Subject == account && isFact(c, atlas.FactAccountName) }})
	default:
		return fmt.Errorf("aws: read the account's alias: %w", err)
	}
	slices.Sort(regions)
	for i, region := range slices.Compact(regions) {
		// A REGION IS A REGION CODE OR IT IS NOT ASKED: its name becomes a
		// host name and a part of every key, and an error would repeat it.
		if !config.AWSRegion(region) {
			b.Withhold("regions["+strconv.Itoa(i)+"]", errNotARegion)
			continue
		}
		for _, part := range regionalParts {
			err = r.regional(ctx, part, region)
			switch {
			case err == nil:
			case refused(err):
				r.refused[part] = append(r.refused[part], region)
			default:
				// ANYTHING ELSE IS A READ THAT DID NOT FINISH, a rate limit
				// included, and fails whole: a listing cut short would
				// withdraw what it did not reach.
				return fmt.Errorf("aws: read the %s of %s: %w", strings.ReplaceAll(part, "_", " "), region, err)
			}
		}
	}
	if err = r.buckets(ctx); err != nil {
		return err
	}
	if err = r.dns(ctx); err != nil {
		return err
	}
	for _, region := range slices.Sorted(mapKeys(r.inUse)) {
		b.Says("regions", account, atlas.FactRegion, region)
	}
	r.unreadRegions()
	return nil
}

// errNotARegion is a Region name that is not a Region code.
var errNotARegion = errors.New("not an AWS Region code")

// rerunForAtlas is the fix for a listing AWS refuses the atlas's role.
const rerunForAtlas = "Run the CloudShell block on the AWS connection again, which holds the role to what " +
	"this engine reads; a refusal that stays is a policy of the organization's."

// isFact reports a claim of the fact f.
func isFact(c atlas.Claim, f atlas.Fact) bool {
	return c.Facet.Kind == atlas.FacetFact && c.Facet.Fact == f
}

// mapKeys is the keys of m, in no order.
func mapKeys(m map[string]bool) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// partShape is the kind a regional part's entries are, and what its first
// resource says the account is used for.
func partShape(part string) (atlas.Kind, atlas.Capability) {
	switch part {
	case partInstances:
		return atlas.KindHost, atlas.CapabilityCompute
	case partDatabases:
		return atlas.KindDatastore, atlas.CapabilityDatabase
	case partQueues:
		return atlas.KindQueue, ""
	}
	return atlas.KindHostname, ""
}

// unreadRegions records each regional part AWS refused somewhere, with the
// Regions counted, and carries what an earlier read recorded there.
func (r *atlasRead) unreadRegions() {
	account := r.account
	for _, part := range regionalParts {
		regions := r.refused[part]
		if len(regions) == 0 {
			continue
		}
		kind, capability := partShape(part)
		prefixes := make([]string, len(regions))
		for i, region := range regions {
			prefixes[i] = accountKeyPrefix + r.client.Account() + "/" + region + "/"
		}
		r.b.Unread(collect.Unread{Part: part,
			Detail: "The " + strings.ReplaceAll(part, "_", " ") + " of " + integration.Count(len(regions), "Region") +
				" were not read: AWS refused Atlas's role there. " + rerunForAtlas,
			Holds: func(c atlas.Claim) bool {
				if c.Subject == account {
					// WHAT THE PART SAYS OF THE ACCOUNT: that it is used for
					// this, and that these Regions are in use.
					return (capability != "" && isFact(c, atlas.FactCapability) && c.Value.Text == string(capability)) ||
						(isFact(c, atlas.FactRegion) && slices.Contains(regions, c.Value.Text))
				}
				if c.Subject.Kind != kind {
					return false
				}
				if kind == atlas.KindHostname {
					return slices.ContainsFunc(regions, func(region string) bool { return balancerIn(c.Subject.Key, region) })
				}
				return slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(c.Subject.Key, p) })
			}})
	}
}

// balancerIn reports a load balancer's name that is one of region's.
func balancerIn(name, region string) bool {
	return strings.Contains(name, "."+region+".elb.") || strings.Contains(name, ".elb."+region+".")
}

// regional reads one part of one Region.
func (r *atlasRead) regional(ctx context.Context, part, region string) error {
	switch part {
	case partInstances:
		return r.instances(ctx, region)
	case partDatabases:
		return r.databases(ctx, region)
	case partLoadBalancers:
		return r.loadBalancers(ctx, region)
	}
	return r.queues(ctx, region)
}

// uses records that the account runs something in region, and what for.
func (r *atlasRead) uses(where, region string, capability atlas.Capability) {
	r.inUse[region] = true
	if capability != "" {
		r.b.Says(where, r.account, atlas.FactCapability, string(capability))
	}
}

// holds claims that the entry e is the account's, in region.
func (r *atlasRead) holds(where string, e atlas.Ref, region string) {
	r.b.Relates(where, e, atlas.RelationBelongsTo, r.account)
	if region != "" {
		r.b.Says(where, e, atlas.FactRegion, region)
	}
}

// instances claims each instance of region as a server: its name and its
// addresses. One that is terminated, or on its way there, is not listed, so
// it leaves as any that is gone.
func (r *atlasRead) instances(ctx context.Context, region string) error {
	found, err := r.client.Instances(ctx, region)
	if err != nil {
		return err
	}
	b := r.b
	for _, it := range found {
		if it.State == "terminated" || it.State == "shutting-down" {
			continue
		}
		where := "regions[" + region + "].instances[" + it.ID + "]"
		host, ok := r.entry(where+".instanceId", atlas.KindHost, region, it.ID)
		if !ok {
			continue
		}
		r.uses(where, region, atlas.CapabilityCompute)
		r.holds(where, host, region)
		name := collect.PlainLabel(it.Name)
		if strings.TrimSpace(name) == "" {
			name = it.ID
		}
		b.Says(where+".tags.Name", host, atlas.FactName, name)
		b.Says(where+".privateIpAddress", host, atlas.FactAddress, it.PrivateIP)
		b.Says(where+".ipAddress", host, atlas.FactAddress, it.PublicIP)
	}
	return nil
}

// databases claims each database instance of region: its engine, its
// version and the port it listens on. An instance being deleted is not
// listed.
func (r *atlasRead) databases(ctx context.Context, region string) error {
	found, err := r.client.Databases(ctx, region)
	if err != nil {
		return err
	}
	b := r.b
	for _, db := range found {
		if db.Status == "deleting" {
			continue
		}
		where := "regions[" + region + "].databases[" + db.ID + "]"
		store, ok := r.entry(where+".DBInstanceIdentifier", atlas.KindDatastore, region, db.ID)
		if !ok {
			continue
		}
		r.uses(where, region, atlas.CapabilityDatabase)
		r.holds(where, store, region)
		b.Relates(where, store, atlas.RelationRunsOn, r.account)
		b.Says(where+".DBInstanceIdentifier", store, atlas.FactName, collect.PlainLabel(db.ID))
		b.Says(where+".Engine", store, atlas.FactEngine, engineOf(db.Engine))
		b.Says(where+".EngineVersion", store, atlas.FactEngineVersion, versionOf(db.EngineVersion))
		if db.Port > 0 {
			b.Says(where+".Endpoint.Port", store, atlas.FactPort, strconv.Itoa(db.Port))
		}
	}
	return nil
}

// engineOf folds an RDS engine's name into the atlas's word for it: Aurora's
// two editions as the engines they speak, and every engine the atlas has no
// word for as other. A switch, so the table costs nothing at init.
func engineOf(vendor string) string {
	switch {
	case vendor == "":
		return ""
	case vendor == "postgres" || vendor == "aurora-postgresql":
		return "postgres"
	case vendor == "mysql" || vendor == "aurora-mysql":
		return "mysql"
	case vendor == "mariadb":
		return "mariadb"
	}
	return "other"
}

// versionOf is the leading run of digits and dots of an engine's version,
// which is the engine's own: RDS appends its build to some (a MySQL-speaking
// Aurora is 8.0.mysql_aurora.3.05.2), in letters a version does not take.
func versionOf(vendor string) string {
	end := strings.IndexFunc(vendor, func(c rune) bool { return (c < '0' || c > '9') && c != '.' })
	if end >= 0 {
		vendor = vendor[:end]
	}
	return strings.TrimRight(vendor, ".")
}

// loadBalancers claims each load balancer of region as the name it answers
// on, which is what a DNS record points at.
func (r *atlasRead) loadBalancers(ctx context.Context, region string) error {
	found, err := r.client.LoadBalancers(ctx, region)
	if err != nil {
		return err
	}
	for i, lb := range found {
		if lb.DNSName == "" || lb.State == "failed" {
			continue
		}
		where := "regions[" + region + "].load_balancers[" + strconv.Itoa(i) + "]"
		name, ok := r.b.Ref(where+".DNSName", atlas.KindHostname, lb.DNSName)
		if !ok {
			continue
		}
		r.uses(where, region, "")
		r.b.Relates(where, name, atlas.RelationBelongsTo, r.account)
	}
	return nil
}

// queues claims each queue of region by its name.
func (r *atlasRead) queues(ctx context.Context, region string) error {
	found, err := r.client.Queues(ctx, region)
	if err != nil {
		return err
	}
	for _, name := range found {
		if name == "" {
			continue
		}
		where := "regions[" + region + "].queues[" + name + "]"
		queue, ok := r.entry(where, atlas.KindQueue, region, name)
		if !ok {
			continue
		}
		r.uses(where, region, "")
		r.holds(where, queue, region)
		r.b.Relates(where, queue, atlas.RelationRunsOn, r.account)
		r.b.Says(where, queue, atlas.FactName, collect.PlainLabel(name))
	}
	return nil
}

// buckets claims each bucket by its name, which S3 holds one of worldwide,
// under the key the engine's own object store names its bucket by.
func (r *atlasRead) buckets(ctx context.Context) error {
	account := r.account
	found, err := r.client.Buckets(ctx)
	switch {
	case err == nil:
	case refused(err):
		r.b.Unread(collect.Unread{Part: partBuckets,
			Detail: "The buckets were not read: AWS refused Atlas's role. " + rerunForAtlas,
			Holds: func(c atlas.Claim) bool {
				return c.Subject.Kind == atlas.KindBucket ||
					(c.Subject == account && isFact(c, atlas.FactCapability) &&
						c.Value.Text == string(atlas.CapabilityObjectStorage))
			}})
		return nil
	default:
		return fmt.Errorf("aws: read the buckets: %w", err)
	}
	for _, bucket := range found {
		where := "buckets[" + bucket.Name + "]"
		ref, ok := r.b.Ref(where, atlas.KindBucket, atlas.ObjectsS3Prefix+"/"+bucket.Name)
		if !ok {
			continue
		}
		r.b.Says(where, account, atlas.FactCapability, string(atlas.CapabilityObjectStorage))
		r.holds(where, ref, bucket.Region)
		r.b.Says(where, ref, atlas.FactName, bucket.Name)
		if bucket.Region != "" {
			r.inUse[bucket.Region] = true
		}
	}
	return nil
}

// dns claims each public hosted zone as a domain the account serves, and
// what its A, AAAA and CNAME records and its aliases point at. A PRIVATE
// ZONE IS NOT READ: its names answer inside a network only, and one that
// shadows a public name would be recorded as that name.
func (r *atlasRead) dns(ctx context.Context) error {
	b, account := r.b, r.account
	unread := func() {
		b.Unread(collect.Unread{Part: partDNS,
			Detail: "The hosted zones and their records were not read: AWS refused Atlas's role. " + rerunForAtlas,
			Holds: func(c atlas.Claim) bool {
				switch c.Subject.Kind {
				case atlas.KindDomain, atlas.KindHostname:
					return true
				case atlas.KindHost:
					return strings.HasPrefix(c.Subject.Key, atlas.HostAddrPrefix+"/")
				}
				return c.Subject == account && isFact(c, atlas.FactCapability) &&
					c.Value.Text == string(atlas.CapabilityDNS)
			}})
	}
	found, err := r.client.HostedZones(ctx)
	switch {
	case err == nil:
	case refused(err):
		unread()
		return nil
	default:
		return fmt.Errorf("aws: read the hosted zones: %w", err)
	}
	found = slices.DeleteFunc(found, func(z HostedZone) bool { return z.Private })
	// EVERY ZONE FIRST, so an alias into another of the account's zones
	// finds its zone whatever order the zones were listed in.
	refs := map[string]atlas.Ref{}
	var zones []atlas.Ref
	for _, z := range found {
		where := "hosted_zones[" + z.ID + "]"
		zone, ok := b.Ref(where+".Name", atlas.KindDomain, z.Name)
		if !ok {
			continue
		}
		refs[z.ID] = zone
		zones = append(zones, zone)
		b.Says(where, account, atlas.FactCapability, string(atlas.CapabilityDNS))
		b.Relates(where, zone, atlas.RelationServedBy, account)
		b.Relates(where, zone, atlas.RelationBelongsTo, account)
	}
	for _, z := range found {
		zone, ok := refs[z.ID]
		if !ok {
			continue
		}
		var sets []RecordSet
		sets, err = r.client.RecordSets(ctx, z.ID)
		if refused(err) {
			// ONE PERMISSION COVERS EVERY ZONE'S RECORDS, so a refusal of
			// one is a refusal of all, and what was recorded stands.
			unread()
			return nil
		}
		if err != nil {
			return fmt.Errorf("aws: read the records of a hosted zone: %w", err)
		}
		r.records(zone, zones, "hosted_zones["+z.ID+"]", sets)
	}
	return nil
}

// records claims each name a zone's A, AAAA and CNAME record sets give and
// what it points at.
func (r *atlasRead) records(zone atlas.Ref, zones []atlas.Ref, where string, sets []RecordSet) {
	b := r.b
	for i, set := range sets {
		if set.Type != recordA && set.Type != recordAAAA && set.Type != recordCNAME {
			continue
		}
		at := where + ".record_sets[" + strconv.Itoa(i) + "]"
		host, ok := b.Ref(at+".Name", atlas.KindHostname, recordName(set.Name))
		if !ok {
			continue
		}
		b.Relates(at+".Name", host, atlas.RelationInZone, zone)
		if set.Alias != "" {
			b.PointsAtName(at+".AliasTarget", host, aliasTarget(set.Alias), zones)
			continue
		}
		for _, value := range set.Values {
			if set.Type == recordCNAME {
				b.PointsAtName(at+".ResourceRecords", host, value, zones)
			} else {
				b.PointsAtAddress(at+".ResourceRecords", host, value)
			}
		}
	}
}

// recordName is a record's name as DNS writes it: Route 53 answers a
// wildcard's asterisk as the escape \052.
func recordName(vendor string) string {
	return strings.Replace(vendor, `\052`, "*", 1)
}

// aliasTarget is the name an alias points at. The console writes an alias to
// a load balancer with a dualstack. label before the balancer's own name,
// and both answer alike, so the label is dropped and the two meet on one key.
func aliasTarget(vendor string) string {
	name := strings.TrimSuffix(vendor, ".")
	if rest, ok := strings.CutPrefix(name, "dualstack."); ok && strings.Contains(rest, ".elb.") {
		return rest
	}
	return name
}
