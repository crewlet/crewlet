package scaleway_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/redact/redacttest"
	"github.com/crewlet/crewlet/internal/scaleway"
)

// The fixture's resources. Obviously fake values throughout.
const (
	serverID    = "dddddddd-0000-4000-8000-000000000001"
	clusterID   = "dddddddd-0000-4000-8000-000000000002"
	databaseID  = "dddddddd-0000-4000-8000-000000000003"
	cacheID     = "dddddddd-0000-4000-8000-000000000004"
	balancerID  = "dddddddd-0000-4000-8000-000000000005"
	containerID = "dddddddd-0000-4000-8000-000000000006"
	functionID  = "dddddddd-0000-4000-8000-000000000007"
	namespaceID = "dddddddd-0000-4000-8000-000000000008"
	// canarySecret is planted in every field the read must never decode.
	canarySecret = "atlas-canary-never-decoded"
)

// undecoded is what a row carries in the fields the read never maps: each
// holds value, so a decoder that starts mapping one puts value in the read.
func undecoded(value string) map[string]any {
	return map[string]any{
		"environment_variables":                map[string]any{"DATABASE_URL": value},
		"secret_environment_variables":         []any{map[string]any{"key": "TOKEN", "hashed_value": value}},
		"admin_password_encrypted_value":       value,
		"admin_password_encryption_ssh_key_id": value,
		"user_data":                            map[string]any{"cloud-init": value},
		"endpoint":                             map[string]any{"ip": "198.51.100.7", "port": 5432, "hostname": value},
		"endpoints":                            []any{map[string]any{"ip": "198.51.100.7", "port": 5432, "name": value}},
		"init_settings":                        []any{map[string]any{"name": "password", "value": value}},
		"settings":                             []any{map[string]any{"name": "requirepass", "value": value}},
		"user_name":                            value,
		"acl_rules":                            []any{map[string]any{"description": value}},
		"cluster_url":                          "https://" + value + ".api.k8s.fr-par.scw.cloud:6443",
		"open_id_connect_config":               map[string]any{"client_id": value},
		"registry_image":                       "rg.fr-par.scw.cloud/acme/" + value,
		"registry_endpoint":                    value,
		"domain_name":                          value,
		"command":                              []any{value},
		"args":                                 []any{value},
		"description":                          value,
		"tags":                                 []any{value},
		"hostname":                             value,
		"comment":                              value,
		"created_by":                           value,
		"ns":                                   []any{value},
	}
}

// with is row with the undecoded fields holding value beside it.
func with(value string, row map[string]any) map[string]any {
	out := undecoded(value)
	maps.Copy(out, row)
	return out
}

// plant fills the fake with one of everything the atlas reads, every row
// carrying canary in the fields it never decodes.
func (f *fakeScaleway) plant(canary string) {
	f.rows["instances/fr-par-1"] = []map[string]any{with(canary, map[string]any{
		"id": serverID, "name": "web-1", "project": projectWeb, "zone": "fr-par-1",
		"public_ips": []any{map[string]any{"address": "203.0.113.10"}}, "private_ip": "10.0.0.5",
	})}
	f.rows["kubernetes/fr-par"] = []map[string]any{with(canary, map[string]any{
		"id": clusterID, "name": "prod", "project_id": projectWeb, "region": "fr-par", "version": "1.31.2",
	})}
	f.rows["databases/fr-par"] = []map[string]any{with(canary, map[string]any{
		"id": databaseID, "name": "orders", "project_id": projectData, "region": "fr-par", "engine": "PostgreSQL-15",
	})}
	f.rows["redis/fr-par-1"] = []map[string]any{with(canary, map[string]any{
		"id": cacheID, "name": "sessions", "project_id": projectData, "zone": "fr-par-1", "version": "7.2.5",
	})}
	f.rows["load_balancers/nl-ams-1"] = []map[string]any{with(canary, map[string]any{
		"id": balancerID, "name": "edge", "project_id": projectWeb, "zone": "nl-ams-1",
		"ip": []any{map[string]any{"ip_address": "203.0.113.20", "reverse": canary}},
	})}
	f.rows["dns"] = []map[string]any{
		with(canary, map[string]any{"domain": "acme.example", "subdomain": "", "project_id": projectWeb}),
		with(canary, map[string]any{"domain": "acme.example", "subdomain": "eu", "project_id": projectWeb}),
	}
	f.rows["dns records/acme.example"] = []map[string]any{
		with(canary, map[string]any{"id": "rec-apex", "type": "A", "name": "", "data": "203.0.113.20"}),
		with(canary, map[string]any{"id": "rec-www", "type": "CNAME", "name": "www", "data": "api.eu.acme.example."}),
		with(canary, map[string]any{"id": "rec-txt", "type": "TXT", "name": "verify", "data": canary}),
		with(canary, map[string]any{"id": "rec-mx", "type": "MX", "name": "", "data": "10 " + canary}),
	}
	f.rows["containers namespaces/fr-par"] = []map[string]any{with(canary, map[string]any{
		"id": namespaceID, "name": "apps", "project_id": projectWeb})}
	f.rows["containers/fr-par"] = []map[string]any{with(canary, map[string]any{
		"id": containerID, "name": "api", "namespace_id": namespaceID, "region": "fr-par"})}
	f.rows["functions namespaces/fr-par"] = []map[string]any{with(canary, map[string]any{
		"id": namespaceID, "name": "jobs", "project_id": projectData})}
	f.rows["functions/fr-par"] = []map[string]any{with(canary, map[string]any{
		"id": functionID, "name": "nightly", "namespace_id": namespaceID, "region": "fr-par"})}
	f.buckets[projectWeb+"/fr-par"] = []string{"acme-assets"}
	f.buckets[projectData+"/nl-ams"] = []string{"acme-backups"}
}

// productsFake is the organization with the atlas application made and one
// of everything planted, and the sink holding what the pass sealed.
func productsFake(t *testing.T) (*fakeScaleway, *memSink) {
	t.Helper()
	f := atlasFake()
	sink := newMemSink()
	f.atlasPass(t, sink, true)
	f.plant(canarySecret)
	f.edit(func(f *fakeScaleway) { f.requests, f.writes = nil, nil })
	return f, sink
}

// productsInput is a read's input with the switch on and the sealed names
// resolving to what the sink holds.
func (f *fakeScaleway) productsInput(t *testing.T, sink *memSink) collect.Input {
	t.Helper()
	in := f.atlasInput(t, func(c *config.Company) { c.Integrations.Scaleway.Atlas = true })
	source := config.MapSource{"SCW_ORG": strings.ToUpper(testOrg), "SCW_SECRET": provSecret}
	maps.Copy(source, sink.values)
	in.Env = config.NewResolver(source)
	return in
}

func (f *fakeScaleway) productsRead(t *testing.T, sink *memSink) collect.Read {
	t.Helper()
	read, err := scalewaySource(t).Collect(t.Context(), f.productsInput(t, sink))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return read
}

func ref(kind atlas.Kind, key string) atlas.Ref { return atlas.MustRef(kind, key) }

// exists reports whether claims hold r's exists claim.
func exists(claims []atlas.Claim, r atlas.Ref) bool {
	return slices.ContainsFunc(claims, func(c atlas.Claim) bool {
		return c.Subject == r && c.Facet.Kind == atlas.FacetExists && !c.Denied
	})
}

// factsOf is every value claims hold for r's fact, sorted.
func factsOf(claims []atlas.Claim, r atlas.Ref, f atlas.Fact) []string {
	var out []string
	for _, c := range claims {
		if c.Subject == r && c.Facet.Kind == atlas.FacetFact && c.Facet.Fact == f {
			out = append(out, c.Value.Text)
		}
	}
	slices.Sort(out)
	return out
}

// related reports whether claims hold from's relation rel to to.
func related(claims []atlas.Claim, from atlas.Ref, rel atlas.Relation, to atlas.Ref) bool {
	return slices.ContainsFunc(claims, func(c atlas.Claim) bool {
		return c.Subject == from && c.Facet.Kind == atlas.FacetRelation && c.Facet.Relation == rel &&
			c.Facet.Target.Entry == to
	})
}

// THE ATLAS READS WHAT THE PROJECTS HOLD, UNDER ITS OWN KEY: each server,
// cluster and load balancer is a host, each database and cache a datastore,
// each bucket a bucket by name, each DNS zone a domain with the names its A,
// AAAA and CNAME records give, each container and function a workload by
// name; each belongs to, or runs on, its project's account, and the project
// is said to use what was found in it. Every call is a GET, the provisioner
// key reads the projects alone, and nothing is addressed but Scaleway's API
// and Object Storage.
//
// Mutation: read the products with the provisioner key, and the fake, which
// grants it no product set, refuses every listing.
func TestTheAtlasReadsWhatTheProjectsHold(t *testing.T) {
	t.Parallel()
	f, sink := productsFake(t)
	read := f.productsRead(t, sink)
	if len(read.Unread) != 0 || len(read.Withheld) != 0 {
		t.Fatalf("the read left %+v unread and withheld %+v", read.Unread, read.Withheld)
	}
	if len(f.wrote()) != 0 {
		t.Fatalf("the read wrote at Scaleway: %v", f.wrote())
	}
	for _, host := range f.rw.Hosts() {
		if host != "https://api.scaleway.com" && !strings.HasPrefix(host, "https://s3.") {
			t.Errorf("the read addressed %s", host)
		}
	}
	if read.Calls != len(f.requests) {
		t.Errorf("the read counted %d calls and made %d", read.Calls, len(f.requests))
	}
	web, data := ref(atlas.KindAccount, webKey), ref(atlas.KindAccount, dataKey)
	c := read.Claims

	server := ref(atlas.KindHost, "scaleway/instances/"+serverID)
	if !exists(c, server) || !slices.Equal(factsOf(c, server, atlas.FactName), []string{"web-1"}) ||
		!slices.Equal(factsOf(c, server, atlas.FactAddress), []string{"10.0.0.5", "203.0.113.10"}) ||
		!slices.Equal(factsOf(c, server, atlas.FactRegion), []string{"fr-par-1"}) ||
		!related(c, server, atlas.RelationBelongsTo, web) {
		t.Errorf("the server: %v %v", factsOf(c, server, atlas.FactName), factsOf(c, server, atlas.FactAddress))
	}
	cluster := ref(atlas.KindHost, "scaleway/kubernetes/"+clusterID)
	if !exists(c, cluster) || !slices.Equal(factsOf(c, cluster, atlas.FactName), []string{"prod"}) ||
		!related(c, cluster, atlas.RelationBelongsTo, web) {
		t.Errorf("the cluster: %v", factsOf(c, cluster, atlas.FactName))
	}
	balancer := ref(atlas.KindHost, "scaleway/load-balancers/"+balancerID)
	if !exists(c, balancer) || !slices.Equal(factsOf(c, balancer, atlas.FactAddress), []string{"203.0.113.20"}) {
		t.Errorf("the load balancer: %v", factsOf(c, balancer, atlas.FactAddress))
	}
	database := ref(atlas.KindDatastore, "scaleway/databases/"+databaseID)
	if !exists(c, database) || !slices.Equal(factsOf(c, database, atlas.FactEngine), []string{"postgres"}) ||
		!slices.Equal(factsOf(c, database, atlas.FactEngineVersion), []string{"15"}) ||
		!related(c, database, atlas.RelationRunsOn, data) || !related(c, database, atlas.RelationBelongsTo, data) {
		t.Errorf("the database: %v %v", factsOf(c, database, atlas.FactEngine), factsOf(c, database, atlas.FactEngineVersion))
	}
	if len(factsOf(c, database, atlas.FactPort)) != 0 {
		t.Error("the database's endpoint was read")
	}
	cache := ref(atlas.KindDatastore, "scaleway/redis/"+cacheID)
	if !exists(c, cache) || !slices.Equal(factsOf(c, cache, atlas.FactEngine), []string{"redis"}) ||
		!slices.Equal(factsOf(c, cache, atlas.FactEngineVersion), []string{"7.2.5"}) {
		t.Errorf("the cache: %v", factsOf(c, cache, atlas.FactEngineVersion))
	}
	assets := ref(atlas.KindBucket, "scaleway/buckets/fr-par/acme-assets")
	backups := ref(atlas.KindBucket, "scaleway/buckets/nl-ams/acme-backups")
	if !exists(c, assets) || !related(c, assets, atlas.RelationBelongsTo, web) ||
		!exists(c, backups) || !related(c, backups, atlas.RelationBelongsTo, data) ||
		!slices.Equal(factsOf(c, backups, atlas.FactRegion), []string{"nl-ams"}) {
		t.Errorf("the buckets were not read into their projects")
	}
	zone, sub := ref(atlas.KindDomain, "acme.example"), ref(atlas.KindDomain, "eu.acme.example")
	apex, www := ref(atlas.KindHostname, "acme.example"), ref(atlas.KindHostname, "www.acme.example")
	target := ref(atlas.KindHostname, "api.eu.acme.example")
	if !exists(c, zone) || !exists(c, sub) || !related(c, zone, atlas.RelationServedBy, web) ||
		!related(c, apex, atlas.RelationInZone, zone) || !related(c, apex, atlas.RelationResolvesTo, balancer) ||
		!related(c, www, atlas.RelationResolvesTo, target) || !related(c, target, atlas.RelationInZone, sub) {
		t.Errorf("the DNS zones and their names were not read as records say")
	}
	for _, unread := range []string{"verify.acme.example"} {
		if exists(c, ref(atlas.KindHostname, unread)) {
			t.Errorf("a record that is no A, AAAA or CNAME gave the name %s", unread)
		}
	}
	// The apex points at the load balancer's address, so it resolves to the
	// load balancer itself, and no second server is known by that address.
	if exists(c, ref(atlas.KindHost, "addr/203.0.113.20")) {
		t.Error("the load balancer's address was claimed as a second server")
	}
	container := ref(atlas.KindWorkload, "scaleway/containers/"+containerID)
	function := ref(atlas.KindWorkload, "scaleway/functions/"+functionID)
	if !exists(c, container) || !slices.Equal(factsOf(c, container, atlas.FactName), []string{"api"}) ||
		!related(c, container, atlas.RelationRunsOn, web) ||
		!exists(c, function) || !slices.Equal(factsOf(c, function, atlas.FactName), []string{"nightly"}) ||
		!related(c, function, atlas.RelationRunsOn, data) {
		t.Errorf("the container and the function were not read by name into their projects")
	}
	if len(factsOf(c, container, atlas.FactImage)) != 0 {
		t.Error("a container's image was read")
	}
	if got := factsOf(c, web, atlas.FactCapability); !slices.Equal(got, []string{"compute", "dns", "object_storage"}) {
		t.Errorf("the web project uses %v", got)
	}
	if got := factsOf(c, data, atlas.FactCapability); !slices.Equal(got, []string{"compute", "database", "object_storage"}) {
		t.Errorf("the data project uses %v", got)
	}
	// An unchanged read records nothing.
	if plan := collect.Decide(c, read, collect.Status{Kind: integration.KindScaleway}, atlasNow.Add(time.Hour)); len(plan.Records) != 0 {
		t.Errorf("an unchanged read recorded %+v", plan.Records)
	}
}

// NOTHING THE READ DOES NOT MAP REACHES IT, AND NOTHING THAT RETURNS A SECRET
// IS ASKED FOR: a value planted in every field Scaleway sends beside the ones
// the atlas keeps (a container's and a function's environment, a server's
// encrypted password, a database's endpoints and settings, a TXT record's
// content, every tag and description) is in no claim, and no call names a
// server's user data, a kubeconfig, a database's users or a bucket's objects.
//
// Mutation: decode `environment_variables`, `endpoints` or a TXT record into
// a claim, and the canary reaches the read.
func TestNothingScalewaySendsBesideTheMappedFieldsReachesTheAtlas(t *testing.T) {
	t.Parallel()
	f, sink := productsFake(t)
	read := f.productsRead(t, sink)
	out, err := json.Marshal(read)
	if err != nil {
		t.Fatal(err)
	}
	for _, canary := range []string{canarySecret, canaryEmail, canaryText, provSecret,
		sink.values[scaleway.AtlasSecretName], sink.values[scaleway.AtlasAccessName]} {
		if strings.Contains(string(out), canary) {
			at := strings.Index(string(out), canary)
			t.Errorf("%q reached the read near %q", canary, string(out)[max(0, at-100):at])
		}
	}
	for _, request := range f.requests {
		if !strings.HasPrefix(request, "GET ") {
			t.Errorf("the read made %s", request)
		}
		for _, never := range []string{"user_data", "kubeconfig", "/users", "/backups", "/certificate", "/logs", "/tokens", "/secrets"} {
			if strings.Contains(request, never) {
				t.Errorf("the read asked for %s", request)
			}
		}
	}
}

// NO CREDENTIAL SHAPE REACHES THE ATLAS FROM A SCALEWAY PRODUCT: every shape
// the redaction pass knows, planted in every field the read maps (each name,
// each ID, each address, each version, each record) and in every field it
// never decodes, leaves no piece of itself in the read; what the mapped
// fields refused is counted, and what sat beside a credential is still read.
//
// Mutation: skip the builder's grammar on a product's key or name, and a
// shape reaches the read.
func TestNoCredentialShapeReachesTheAtlasFromAScalewayProduct(t *testing.T) {
	t.Parallel()
	for _, shape := range redacttest.Shapes() {
		s := shape.Sample
		f, sink := productsFake(t)
		f.edit(func(f *fakeScaleway) {
			f.plant(s)
			// One row per listing with the shape in every mapped field, beside
			// the clean row plant left.
			add := func(key string, row map[string]any) { f.rows[key] = append(f.rows[key], with(s, row)) }
			add("instances/fr-par-1", map[string]any{"id": s, "name": s, "project": s, "zone": s,
				"public_ips": []any{map[string]any{"address": s}}, "private_ip": s})
			add("instances/fr-par-1", map[string]any{"id": "dddddddd-0000-4000-8000-0000000000a1", "name": s,
				"project": projectWeb, "zone": "fr-par-1", "public_ip": map[string]any{"address": s}})
			add("kubernetes/fr-par", map[string]any{"id": s, "name": s, "project_id": s, "region": s, "version": s})
			add("databases/fr-par", map[string]any{"id": "dddddddd-0000-4000-8000-0000000000a2", "name": s,
				"project_id": projectData, "region": s, "engine": "PostgreSQL-" + s})
			add("databases/fr-par", map[string]any{"id": s, "name": "x", "project_id": s, "engine": s})
			add("redis/fr-par-1", map[string]any{"id": "dddddddd-0000-4000-8000-0000000000a3", "name": s,
				"project_id": projectData, "zone": s, "version": s})
			add("load_balancers/nl-ams-1", map[string]any{"id": s, "name": s, "project_id": s, "zone": s,
				"ip": []any{map[string]any{"ip_address": s}}})
			add("dns", map[string]any{"domain": s, "subdomain": s, "project_id": s})
			add("dns records/acme.example", map[string]any{"id": s, "type": "A", "name": s, "data": s})
			add("dns records/acme.example", map[string]any{"id": "rec-c", "type": "CNAME", "name": "shop", "data": s})
			add("containers namespaces/fr-par", map[string]any{"id": s, "name": s, "project_id": s})
			add("containers/fr-par", map[string]any{"id": s, "name": s, "namespace_id": s, "region": s})
			add("containers/fr-par", map[string]any{"id": "dddddddd-0000-4000-8000-0000000000a4", "name": s,
				"namespace_id": namespaceID, "region": "fr-par"})
			add("functions/fr-par", map[string]any{"id": s, "name": s, "namespace_id": namespaceID, "region": s})
			f.buckets[projectWeb+"/fr-par"] = append(f.buckets[projectWeb+"/fr-par"], s)
		})
		read, err := scalewaySource(t).Collect(t.Context(), f.productsInput(t, sink))
		if err != nil {
			t.Fatalf("%s: Collect: %v", shape.Name, err)
		}
		out, err := json.Marshal(read)
		if err != nil {
			t.Fatal(err)
		}
		for _, piece := range []string{s[len(s)/2:], s[len(s)/4 : len(s)/4+12]} {
			if strings.Contains(string(out), piece) {
				at := strings.Index(string(out), piece)
				t.Errorf("%s: a piece of the shape reached the read near %q", shape.Name,
					string(out)[max(0, at-120):at])
			}
		}
		if len(read.Withheld) < 15 {
			t.Errorf("%s: %d values withheld, want every planted mapped field counted", shape.Name, len(read.Withheld))
		}
		// What sat beside a credential is still read, and an entry whose
		// name was refused is still an entry, by its ID.
		c := read.Claims
		if !exists(c, ref(atlas.KindHost, "scaleway/instances/"+serverID)) ||
			!exists(c, ref(atlas.KindBucket, "scaleway/buckets/fr-par/acme-assets")) ||
			!exists(c, ref(atlas.KindHostname, "www.acme.example")) {
			t.Errorf("%s: what sat beside the credential was not read", shape.Name)
		}
		renamed := ref(atlas.KindWorkload, "scaleway/containers/dddddddd-0000-4000-8000-0000000000a4")
		if !exists(c, renamed) || len(factsOf(c, renamed, atlas.FactName)) != 0 {
			t.Errorf("%s: the container whose name was refused: %v", shape.Name, factsOf(c, renamed, atlas.FactName))
		}
	}
}

// WITH THE SWITCH OFF NOTHING INSIDE A PROJECT IS ASKED FOR, even where a key
// is still sealed; and what an earlier read recorded is retired like anything
// two complete reads miss, never kept by a switch nobody reads.
//
// Mutation: read whenever a key resolves, and switching the atlas off at the
// connection stops nothing until the pass deletes the key.
func TestWithTheSwitchOffNoProductIsRead(t *testing.T) {
	t.Parallel()
	f, sink := productsFake(t)
	whole := f.productsRead(t, sink)
	f.edit(func(f *fakeScaleway) { f.requests = nil })
	in := f.productsInput(t, sink)
	in.Config.Integrations.Scaleway.Atlas = false
	read, err := scalewaySource(t).Collect(t.Context(), in)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.requests, []string{"GET /account/v3/projects"}) || len(read.Unread) != 0 {
		t.Fatalf("with the switch off the read made %v and left %+v unread", f.requests, read.Unread)
	}
	first := collect.Decide(whole.Claims, read, collect.Status{Kind: integration.KindScaleway}, atlasNow.Add(time.Hour))
	second := collect.Decide(whole.Claims, read, first.Status, atlasNow.Add(26*time.Hour))
	if first.Status.Retired != 0 || second.Status.Retired == 0 {
		t.Errorf("switched off, the first read retired %d and the second %d", first.Status.Retired, second.Status.Retired)
	}
}

// A PRODUCT SCALEWAY REFUSES THE ATLAS KEY IS A FINDING, AND THE REST ARE
// READ: the standing names the part and the permission set in the engine's
// own words, nothing an earlier read of that part recorded is missed or
// retired however long the refusal lasts, the capability it gave a project
// stands, and every other product is read as usual.
//
// Mutation: read the refused listing as an empty one, and every database is
// retired a day after somebody edits the atlas application's policy.
func TestAProductScalewayRefusesIsAFindingAndRetiresNothing(t *testing.T) {
	t.Parallel()
	f, sink := productsFake(t)
	whole := f.productsRead(t, sink)
	for _, part := range []string{"databases", "buckets", "dns"} {
		f.edit(func(f *fakeScaleway) { f.refuse = map[string]int{part: http.StatusForbidden} })
		read := f.productsRead(t, sink)
		if len(read.Unread) != 1 || read.Unread[0].Part != part ||
			strings.Contains(read.Unread[0].Detail, "insufficient permissions") ||
			!strings.Contains(read.Unread[0].Detail, productSets[part]) {
			t.Fatalf("%s refused: %+v", part, read.Unread)
		}
		if len(read.Claims) == 0 || len(read.Claims) >= len(whole.Claims) {
			t.Fatalf("%s refused: the read holds %d claims of %d", part, len(read.Claims), len(whole.Claims))
		}
		status := collect.Status{Kind: integration.KindScaleway}
		for _, at := range []time.Time{atlasNow.Add(time.Hour), atlasNow.Add(30 * time.Hour), atlasNow.Add(60 * time.Hour)} {
			plan := collect.Decide(whole.Claims, read, status, at)
			if len(plan.Records) != 0 || len(plan.Status.Missing) != 0 || len(plan.Status.Unread) != 1 {
				t.Fatalf("%s refused: at %s the read recorded %+v and missed %+v", part, at, plan.Records, plan.Status.Missing)
			}
			status = plan.Status
		}
		// The control: the same read, were the listing whole and empty, retires.
		empty := read
		empty.Unread = nil
		first := collect.Decide(whole.Claims, empty, collect.Status{Kind: integration.KindScaleway}, atlasNow.Add(time.Hour))
		if plan := collect.Decide(whole.Claims, empty, first.Status, atlasNow.Add(26*time.Hour)); plan.Status.Retired == 0 {
			t.Errorf("%s: the control retired nothing", part)
		}
	}
}

// AN ATLAS APPLICATION WITH NO KEY THAT WORKS LEAVES EVERY PRODUCT UNREAD
// AND THE PROJECTS READ: a key not sealed yet, and one Scaleway no longer
// accepts, are the pass's to replace, so the read says so for each part,
// keeps what it recorded, and never fails over it.
//
// Mutation: fail the read on the atlas key's 401, and a key somebody deleted
// at Scaleway stops the projects being read as well.
func TestAnAtlasKeyThatDoesNotWorkLeavesTheProductsUnread(t *testing.T) {
	t.Parallel()
	f, sink := productsFake(t)
	whole := f.productsRead(t, sink)
	for name, fault := range map[string]func(*fakeScaleway, *memSink){
		"no key sealed": func(_ *fakeScaleway, s *memSink) { delete(s.values, scaleway.AtlasSecretName) },
		"a dead key": func(f *fakeScaleway, s *memSink) {
			f.edit(func(f *fakeScaleway) { delete(f.keys, s.values[scaleway.AtlasAccessName]) })
		},
	} {
		f, sink := productsFake(t)
		fault(f, sink)
		read := f.productsRead(t, sink)
		if len(read.Unread) != 9 || !exists(read.Claims, ref(atlas.KindAccount, webKey)) {
			t.Fatalf("%s: %d parts unread: %+v", name, len(read.Unread), read.Unread)
		}
		plan := collect.Decide(whole.Claims, read, collect.Status{Kind: integration.KindScaleway}, atlasNow.Add(time.Hour))
		if len(plan.Records) != 0 || len(plan.Status.Missing) != 0 {
			t.Errorf("%s: the read recorded %+v and missed %+v", name, plan.Records, plan.Status.Missing)
		}
	}
}

// A PRODUCT LISTING THAT DID NOT FINISH FAILS THE READ WHOLE, a locality
// Scaleway does not serve a product in is nothing there, and a listing longer
// than a page is read to its end, in the Instance API's paging and the
// others' alike.
//
// Mutation: stop the Instance listing at its first page, and the hundred and
// first server is retired a day later.
func TestAProductListingIsReadWholeOrNotAtAll(t *testing.T) {
	t.Parallel()
	f, sink := productsFake(t)
	f.edit(func(f *fakeScaleway) { f.refuse = map[string]int{"kubernetes": http.StatusBadGateway} })
	if read, err := scalewaySource(t).Collect(t.Context(), f.productsInput(t, sink)); err == nil ||
		errors.Is(err, integration.ErrNotConfigured) || len(read.Claims) != 0 {
		t.Errorf("a server fault answered %d claims and %v", len(read.Claims), err)
	}
	f, sink = productsFake(t)
	f.edit(func(f *fakeScaleway) {
		f.unserved = map[string]bool{"it-mil-1": true, "pl-waw": true}
		for i := range 150 {
			f.rows["instances/nl-ams-2"] = append(f.rows["instances/nl-ams-2"], map[string]any{
				"id": fmt.Sprintf("eeeeeeee-0000-4000-8000-%012d", i), "name": "node-" + strconv.Itoa(i),
				"project": projectWeb, "zone": "nl-ams-2"})
			f.rows["redis/nl-ams-1"] = append(f.rows["redis/nl-ams-1"], map[string]any{
				"id": fmt.Sprintf("ffffffff-0000-4000-8000-%012d", i), "name": "cache-" + strconv.Itoa(i),
				"project_id": projectData, "zone": "nl-ams-1", "version": "7.2.5"})
		}
	})
	read := f.productsRead(t, sink)
	hosts, stores := 0, 0
	for _, c := range read.Claims {
		if c.Facet.Kind != atlas.FacetExists {
			continue
		}
		switch {
		case strings.HasPrefix(c.Subject.Key, "scaleway/instances/"):
			hosts++
		case strings.HasPrefix(c.Subject.Key, "scaleway/redis/"):
			stores++
		}
	}
	if hosts != 151 || stores != 151 || len(read.Unread) != 0 {
		t.Errorf("the paged listings read %d servers and %d caches, want 151 of each; unread %+v", hosts, stores, read.Unread)
	}
}
