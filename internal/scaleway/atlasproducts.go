package scaleway

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/config"
)

// What the projects hold, read under the atlas application's own key
// (ADR-0144): servers, Kubernetes clusters and load balancers as hosts,
// databases and caches as datastores, buckets by name, DNS zones and what
// their A, AAAA and CNAME records point at, and Serverless Containers and
// Functions by name. Each product is one part of the read, behind its own
// permission set, and a part Scaleway refuses is a finding that carries what
// an earlier read of it recorded.
//
// READ ONLY WHERE THE COMPANY ASKED: with `integrations.scaleway.atlas` off
// nothing here is called, and what an earlier read recorded is retired like
// anything else two complete reads miss.

// The parts of the product read, each one listing's.
const (
	partInstances     = "instances"
	partKubernetes    = "kubernetes"
	partLoadBalancers = "load_balancers"
	partDatabases     = "databases"
	partRedis         = "redis"
	partBuckets       = "buckets"
	partDNS           = "dns"
	partContainers    = "containers"
	partFunctions     = "functions"
)

// The key every entry of a part starts with, after its kind: a part is told
// from its neighbours of the same kind by it.
const (
	instanceKeyPrefix     = accountKeyPrefix + "instances/"
	kubernetesKeyPrefix   = accountKeyPrefix + "kubernetes/"
	loadBalancerKeyPrefix = accountKeyPrefix + "load-balancers/"
	databaseKeyPrefix     = accountKeyPrefix + "databases/"
	redisKeyPrefix        = accountKeyPrefix + "redis/"
	bucketKeyPrefix       = accountKeyPrefix + "buckets/"
	containerKeyPrefix    = accountKeyPrefix + "containers/"
	functionKeyPrefix     = accountKeyPrefix + "functions/"
)

// atlasPart is one product of the read: what a person calls it, what its
// listing shows a project uses, which of the voice's claims are its own, and
// the read itself, which lists everything before it claims anything so a
// listing cut short claims nothing.
type atlasPart struct {
	name, noun string
	uses       atlas.Capability
	holds      func(atlas.Claim) bool
	read       func(r *productRead, ctx context.Context) (func(), error)
}

// entriesOf is the claims on entries of kind whose key starts with prefix.
func entriesOf(kind atlas.Kind, prefix string) func(atlas.Claim) bool {
	return func(c atlas.Claim) bool { return c.Subject.Kind == kind && strings.HasPrefix(c.Subject.Key, prefix) }
}

// ofDNS is the claims the DNS part makes: on zones, on names, and on the
// servers its address records point at.
func ofDNS(c atlas.Claim) bool {
	switch c.Subject.Kind {
	case atlas.KindDomain, atlas.KindHostname:
		return true
	case atlas.KindHost:
		return strings.HasPrefix(c.Subject.Key, atlas.HostAddrPrefix+"/")
	}
	return false
}

// atlasParts is every part, in the order a read makes them.
//
//nolint:gochecknoglobals // an immutable table, not state
var atlasParts = []atlasPart{
	{partInstances, "servers", atlas.CapabilityCompute, entriesOf(atlas.KindHost, instanceKeyPrefix), (*productRead).servers},
	{partKubernetes, "Kubernetes clusters", atlas.CapabilityCompute, entriesOf(atlas.KindHost, kubernetesKeyPrefix), (*productRead).clusters},
	{partLoadBalancers, "load balancers", atlas.CapabilityCompute, entriesOf(atlas.KindHost, loadBalancerKeyPrefix), (*productRead).loadBalancers},
	{partDatabases, "databases", atlas.CapabilityDatabase, entriesOf(atlas.KindDatastore, databaseKeyPrefix), (*productRead).databases},
	{partRedis, "Redis clusters", atlas.CapabilityDatabase, entriesOf(atlas.KindDatastore, redisKeyPrefix), (*productRead).caches},
	{partBuckets, "buckets", atlas.CapabilityObjectStorage, entriesOf(atlas.KindBucket, bucketKeyPrefix), (*productRead).buckets},
	{partDNS, "DNS zones", atlas.CapabilityDNS, ofDNS, (*productRead).dns},
	{partContainers, "Serverless Containers", atlas.CapabilityCompute, entriesOf(atlas.KindWorkload, containerKeyPrefix), (*productRead).containers},
	{partFunctions, "Serverless Functions", atlas.CapabilityCompute, entriesOf(atlas.KindWorkload, functionKeyPrefix), (*productRead).functions},
}

// held reports whether a claim the voice holds came from the part: one on
// the part's own entries, or the capability its listing gives a project.
func (p atlasPart) held(c atlas.Claim) bool {
	if p.holds(c) {
		return true
	}
	return c.Subject.Kind == atlas.KindAccount && c.Facet.Kind == atlas.FacetFact &&
		c.Facet.Fact == atlas.FactCapability && c.Value.Text == string(p.uses)
}

// productRead is one read of the products.
type productRead struct {
	b        *collect.Builder
	client   *Client
	access   string
	org      atlas.Ref
	projects []Project

	// byAddress is the servers and load balancers this read claimed, by the
	// key of each public address; one two of them share is in shared.
	byAddress map[string]atlas.Ref
	shared    map[string]bool
}

// at records that host answers at a public address, for the DNS part to
// point a name at. An address two hosts share names neither.
func (r *productRead) at(host atlas.Ref, address string) {
	key, err := atlas.AddressHostKey(address)
	if err != nil || r.shared[key] {
		return
	}
	if prior, ok := r.byAddress[key]; ok && prior != host {
		delete(r.byAddress, key)
		r.shared[key] = true
		return
	}
	r.byAddress[key] = host
}

// readProducts reads every part under the atlas application's key, where the
// company switched it on. A part Scaleway refuses is unread and the rest are
// still read; a key Scaleway does not accept leaves every part unread, since
// the pass that keeps the key replaces it; any other failure fails the read.
func readProducts(ctx context.Context, b *collect.Builder, in collect.Input, hc *http.Client, org atlas.Ref,
	projects []Project) error {

	cfg := in.Config.Integrations.Scaleway
	if !cfg.Atlas {
		return nil
	}
	secret := strings.TrimSpace(in.Env.Value("${" + AtlasSecretName + "}"))
	access := strings.TrimSpace(in.Env.Value("${" + AtlasAccessName + "}"))
	unread := func(parts []atlasPart, detail func(atlasPart) string) {
		for _, p := range parts {
			b.Unread(collect.Unread{Part: p.name, Detail: detail(p), Holds: p.held})
		}
	}
	if secret == "" || !accessKeyPattern().MatchString(access) {
		unread(atlasParts, func(p atlasPart) string {
			return "The " + p.noun + " were not read: the Atlas application has no API key yet. The next " +
				"check of the Scaleway connection makes it."
		})
		return nil
	}
	client, err := NewClient(ClientOptions{
		OrganizationID: strings.TrimPrefix(org.Key, accountKeyPrefix), SecretKey: secret, HTTP: hc,
	})
	if err != nil {
		return fmt.Errorf("scaleway: the atlas application's client: %w", err)
	}
	r := &productRead{b: b, client: client, access: access, org: org, projects: projects,
		byAddress: map[string]atlas.Ref{}, shared: map[string]bool{}}
	for i, p := range atlasParts {
		claim, err := p.read(r, ctx)
		switch Status(err) {
		case 0:
			if err != nil {
				return fmt.Errorf("scaleway: read the %s: %w", p.noun, err)
			}
			claim()
		case http.StatusForbidden:
			unread([]atlasPart{p}, func(p atlasPart) string {
				return "The " + p.noun + " were not read: Scaleway refused the Atlas application's key. " +
					"Its policy grants " + atlasSetOf(p.name) + " on every project: check nobody changed it at Scaleway."
			})
		case http.StatusUnauthorized:
			unread(atlasParts[i:], func(p atlasPart) string {
				return "The " + p.noun + " were not read: Scaleway does not accept the Atlas application's " +
					"key. The next check of the Scaleway connection replaces it."
			})
			return nil
		default:
			return fmt.Errorf("scaleway: read the %s: %w", p.noun, reject(err))
		}
	}
	return nil
}

// account is the account a resource's project is: the Organization's own
// entry for its default project, which carries the Organization's ID. It is
// named, never claimed: the listing of projects claims the accounts.
func (r *productRead) account(where, projectID string) (atlas.Ref, bool) {
	id := config.ScalewayID(projectID)
	if id == "" {
		// NO PROJECT SAID: the bare prefix would name Scaleway itself.
		return atlas.Ref{}, false
	}
	return r.b.Names(where, atlas.KindAccount, accountKeyPrefix+id)
}

// resource claims one entry of a part: that it exists under key, its name,
// that it belongs to its project, and that the project uses what the part
// stands for. False where its key is withheld.
func (r *productRead) resource(where string, kind atlas.Kind, key, name, projectID string,
	uses atlas.Capability) (atlas.Ref, bool) {

	entry, ok := r.b.Ref(where+".id", kind, key)
	if !ok {
		return atlas.Ref{}, false
	}
	r.b.Says(where+".name", entry, atlas.FactName, collect.PlainLabel(name))
	if account, ok := r.account(where+".project_id", projectID); ok {
		switch kind {
		case atlas.KindWorkload:
			// A WORKLOAD BELONGS TO NO ACCOUNT in the vocabulary: it runs on one.
			r.b.Relates(where+".project_id", entry, atlas.RelationRunsOn, account)
		case atlas.KindDatastore:
			r.b.Relates(where+".project_id", entry, atlas.RelationRunsOn, account)
			r.b.Relates(where+".project_id", entry, atlas.RelationBelongsTo, account)
		default:
			r.b.Relates(where+".project_id", entry, atlas.RelationBelongsTo, account)
		}
		r.b.Says(where+".project_id", account, atlas.FactCapability, string(uses))
	}
	return entry, true
}

func (r *productRead) servers(ctx context.Context) (func(), error) {
	rows, err := r.client.Servers(ctx)
	return func() {
		slices.SortFunc(rows, func(a, z Server) int { return cmp.Compare(a.ID, z.ID) })
		for _, s := range rows {
			where := "servers[" + s.ID + "]"
			host, ok := r.resource(where, atlas.KindHost, instanceKeyPrefix+s.ID, s.Name, s.Project, atlas.CapabilityCompute)
			if !ok {
				continue
			}
			r.b.Says(where+".zone", host, atlas.FactRegion, s.Zone)
			addrs := s.PublicIPs
			if s.PublicIP != nil {
				addrs = append(addrs, *s.PublicIP)
			}
			for _, ip := range addrs {
				r.b.Says(where+".public_ips", host, atlas.FactAddress, ip.Address)
				r.at(host, ip.Address)
			}
			if s.PrivateIP != nil {
				r.b.Says(where+".private_ip", host, atlas.FactAddress, *s.PrivateIP)
			}
		}
	}, err
}

func (r *productRead) clusters(ctx context.Context) (func(), error) {
	rows, err := r.client.Clusters(ctx)
	return func() {
		slices.SortFunc(rows, func(a, z Cluster) int { return cmp.Compare(a.ID, z.ID) })
		for _, c := range rows {
			where := "clusters[" + c.ID + "]"
			if host, ok := r.resource(where, atlas.KindHost, kubernetesKeyPrefix+c.ID, c.Name, c.ProjectID,
				atlas.CapabilityCompute); ok {
				r.b.Says(where+".region", host, atlas.FactRegion, c.Region)
			}
		}
	}, err
}

func (r *productRead) loadBalancers(ctx context.Context) (func(), error) {
	rows, err := r.client.LoadBalancers(ctx)
	return func() {
		slices.SortFunc(rows, func(a, z LoadBalancer) int { return cmp.Compare(a.ID, z.ID) })
		for _, lb := range rows {
			where := "lbs[" + lb.ID + "]"
			host, ok := r.resource(where, atlas.KindHost, loadBalancerKeyPrefix+lb.ID, lb.Name, lb.ProjectID,
				atlas.CapabilityCompute)
			if !ok {
				continue
			}
			r.b.Says(where+".zone", host, atlas.FactRegion, lb.Zone)
			for _, ip := range lb.IPs {
				r.b.Says(where+".ip", host, atlas.FactAddress, ip.Address)
				r.at(host, ip.Address)
			}
		}
	}, err
}

func (r *productRead) databases(ctx context.Context) (func(), error) {
	rows, err := r.client.Databases(ctx)
	return func() {
		slices.SortFunc(rows, func(a, z Database) int { return cmp.Compare(a.ID, z.ID) })
		for _, d := range rows {
			where := "instances[" + d.ID + "]"
			store, ok := r.resource(where, atlas.KindDatastore, databaseKeyPrefix+d.ID, d.Name, d.ProjectID,
				atlas.CapabilityDatabase)
			if !ok {
				continue
			}
			r.b.Says(where+".region", store, atlas.FactRegion, d.Region)
			engine, version := databaseEngine(d.Engine)
			r.b.Says(where+".engine", store, atlas.FactEngine, engine)
			r.b.Says(where+".engine", store, atlas.FactEngineVersion, version)
		}
	}, err
}

// databaseEngine reads Scaleway's engine word ("PostgreSQL-15", "MySQL-8")
// as the atlas's engine and its version; an engine the atlas has no word for
// is other, and says no version.
func databaseEngine(vendor string) (engine, version string) {
	name, version, _ := strings.Cut(strings.TrimSpace(vendor), "-")
	switch strings.ToLower(name) {
	case "":
		return "", ""
	case "postgresql":
		return "postgres", version
	case "mysql":
		return "mysql", version
	}
	return "other", ""
}

func (r *productRead) caches(ctx context.Context) (func(), error) {
	rows, err := r.client.Caches(ctx)
	return func() {
		slices.SortFunc(rows, func(a, z Cache) int { return cmp.Compare(a.ID, z.ID) })
		for _, c := range rows {
			where := "clusters[" + c.ID + "]"
			store, ok := r.resource(where, atlas.KindDatastore, redisKeyPrefix+c.ID, c.Name, c.ProjectID,
				atlas.CapabilityDatabase)
			if !ok {
				continue
			}
			r.b.Says(where+".zone", store, atlas.FactRegion, c.Zone)
			r.b.Says(where+".engine", store, atlas.FactEngine, "redis")
			r.b.Says(where+".version", store, atlas.FactEngineVersion, c.Version)
		}
	}, err
}

// buckets lists every project's buckets: Object Storage knows no
// Organization, so each project is asked in turn.
func (r *productRead) buckets(ctx context.Context) (func(), error) {
	var rows []Bucket
	for _, p := range r.projects {
		id := config.ScalewayID(p.ID)
		if !uuidPattern().MatchString(id) {
			// AN ID THAT IS NO UUID never reaches the key that signs the call.
			continue
		}
		found, err := r.client.Buckets(ctx, r.access, id)
		if err != nil {
			return nil, err
		}
		rows = append(rows, found...)
	}
	return func() {
		slices.SortFunc(rows, func(a, z Bucket) int {
			return cmp.Or(cmp.Compare(a.Region, z.Region), cmp.Compare(a.Name, z.Name))
		})
		for _, bk := range rows {
			where := "buckets[" + bk.Region + "/" + bk.Name + "]"
			// A BUCKET'S NAME IS ITS IDENTITY, unique in its region, so the
			// key is the name and the builder judges it as it was written.
			bucket, ok := r.resource(where, atlas.KindBucket, bucketKeyPrefix+bk.Region+"/"+bk.Name, bk.Name,
				bk.ProjectID, atlas.CapabilityObjectStorage)
			if ok {
				r.b.Says(where+".region", bucket, atlas.FactRegion, bk.Region)
			}
		}
	}, nil
}

// dns lists every zone and its records, and claims each zone and what its
// A, AAAA and CNAME records point at. No other record type is claimed.
func (r *productRead) dns(ctx context.Context) (func(), error) {
	zones, err := r.client.DNSZones(ctx)
	if err != nil {
		return nil, err
	}
	slices.SortFunc(zones, func(a, z DNSZone) int { return cmp.Compare(a.Name(), z.Name()) })
	records := make([][]DNSRecord, len(zones))
	for i, z := range zones {
		if records[i], err = r.client.DNSRecords(ctx, z.Name()); err != nil {
			return nil, err
		}
	}
	return func() {
		// EVERY ZONE FIRST, so a CNAME into another zone finds its zone.
		refs := make([]atlas.Ref, len(zones))
		named := make([]bool, len(zones))
		var all []atlas.Ref
		for i, z := range zones {
			// A ZONE HAS NO ID, so its place is its position: a locator
			// built from its name would carry whatever the name carries.
			where := "dns_zones[" + strconv.Itoa(i) + "]"
			if refs[i], named[i] = r.b.Ref(where+".domain", atlas.KindDomain, z.Name()); !named[i] {
				continue
			}
			all = append(all, refs[i])
			if account, ok := r.account(where+".project_id", z.ProjectID); ok {
				r.b.Relates(where, refs[i], atlas.RelationServedBy, account)
				r.b.Relates(where, refs[i], atlas.RelationBelongsTo, account)
				r.b.Says(where+".project_id", account, atlas.FactCapability, string(atlas.CapabilityDNS))
			}
		}
		for i := range zones {
			if !named[i] {
				continue
			}
			for _, rec := range records[i] {
				name := refs[i].Key
				if rel := strings.TrimSuffix(strings.TrimSpace(rec.Name), "."); rel != "" {
					name = rel + "." + name
				}
				r.b.Name("dns_zones["+strconv.Itoa(i)+"].records["+rec.ID+"]", refs[i], all, collect.Record{
					Type: rec.Type, Name: name, Content: strings.TrimSuffix(strings.TrimSpace(rec.Data), "."),
					Hosts: r.byAddress,
				})
			}
		}
	}, nil
}

func (r *productRead) containers(ctx context.Context) (func(), error) {
	namespaces, rows, err := r.client.Containers(ctx)
	return r.serverless("containers", containerKeyPrefix, namespaces, rows), err
}

func (r *productRead) functions(ctx context.Context) (func(), error) {
	namespaces, rows, err := r.client.Functions(ctx)
	return r.serverless("functions", functionKeyPrefix, namespaces, rows), err
}

// serverless claims each container or function as a workload, by its name,
// running on the project its namespace is in.
func (r *productRead) serverless(field, prefix string, namespaces []Namespace, rows []Serverless) func() {
	return func() {
		project := map[string]string{}
		for _, n := range namespaces {
			project[n.ID] = n.ProjectID
		}
		slices.SortFunc(rows, func(a, z Serverless) int { return cmp.Compare(a.ID, z.ID) })
		for _, s := range rows {
			r.resource(field+"["+s.ID+"]", atlas.KindWorkload, prefix+s.ID, s.Name, project[s.NamespaceID],
				atlas.CapabilityCompute)
		}
	}
}
