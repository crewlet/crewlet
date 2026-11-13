package digitalocean

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/crewlet/crewlet/internal/setup"
)

// atlasNow is the instant every atlas read here is made at.
var atlasNow = time.Date(2026, 11, 13, 9, 0, 0, 0, time.UTC)

// Values the fake carries in fields the read never decodes.
const (
	canaryText   = "atlas-canary-undecoded"
	canarySecret = "atlas-canary-p4ssw0rd"
	canaryEmail  = "owner@example.com"
)

// The fixture's ids and the keys of its entries.
const (
	clusterID  = "aaaaaaaa-0000-4000-8000-000000000001"
	postgresID = "bbbbbbbb-0000-4000-8000-000000000001"
	kafkaID    = "bbbbbbbb-0000-4000-8000-000000000002"
	balancerID = "cccccccc-0000-4000-8000-000000000001"
	appID      = "dddddddd-0000-4000-8000-000000000001"
	teamKey    = "digitalocean/example-team"
)

// atlasToken is the token made for the atlas, and agentToken a seat's own.
var (
	atlasTok = fakeToken(0xa7)
	agentTok = fakeToken(0x51)
)

// atlasInventory is a team with one of everything the atlas reads, each row
// carrying what DigitalOcean sends beside the fields the read decodes.
func atlasInventory() *inventory {
	return &inventory{
		droplets: []map[string]any{{
			"id": 1001, "name": "web-1", "status": "active", "user_data": canarySecret,
			"region": map[string]any{"slug": "fra1", "name": canaryText},
			"networks": map[string]any{
				"v4": []any{
					map[string]any{"ip_address": "203.0.113.10", "type": "public", "gateway": "203.0.113.1"},
					map[string]any{"ip_address": "10.10.0.2", "type": "private"},
				},
				"v6": []any{map[string]any{"ip_address": "2001:db8::10", "type": "public"}},
			},
			"tags": []any{canaryText}, "image": map[string]any{"description": canaryText},
		}},
		clusters: []map[string]any{{
			"id": clusterID, "name": "prod", "region": "fra1", "version": "1.33.1-do.0",
			"endpoint": "https://" + canaryText + ".k8s.ondigitalocean.com", "tags": []any{canaryText},
			"node_pools": []any{map[string]any{"name": canaryText, "nodes": []any{map[string]any{"droplet_id": "1001"}}}},
		}},
		databases: []map[string]any{
			{
				"id": postgresID, "name": "main-pg", "engine": "pg", "version": "16", "region": "fra1",
				"connection": map[string]any{
					"uri":  "postgresql://doadmin:" + canarySecret + "@" + canaryText + ".db.ondigitalocean.com:25060/defaultdb",
					"host": canaryText + ".db.ondigitalocean.com", "user": "doadmin", "password": canarySecret,
				},
				"private_connection": map[string]any{"password": canarySecret},
				"users":              []any{map[string]any{"name": "doadmin", "password": canarySecret}},
			},
			{"id": kafkaID, "name": "events", "engine": "kafka", "version": "3.8", "region": "fra1",
				"users": []any{map[string]any{"access_key": canarySecret}}},
		},
		balancers: []map[string]any{{
			"id": balancerID, "name": "edge", "ip": "203.0.113.20", "ipv6": "", "status": "active",
			"region":           map[string]any{"slug": "fra1"},
			"forwarding_rules": []any{map[string]any{"certificate_id": canaryText}},
		}},
		domains: []map[string]any{{"name": "acme.example", "zone_file": "@ IN TXT " + canarySecret}},
		records: map[string][]map[string]any{"acme.example": {
			{"id": 1, "type": "A", "name": "@", "data": "203.0.113.20"},
			{"id": 2, "type": "A", "name": "www", "data": "203.0.113.10"},
			{"id": 3, "type": "AAAA", "name": "www", "data": "2001:db8::10"},
			{"id": 4, "type": "CNAME", "name": "api", "data": "@"},
			{"id": 5, "type": "CNAME", "name": "docs", "data": "pages.vendor.example."},
			{"id": 6, "type": "TXT", "name": "@", "data": canarySecret},
		}},
		apps: []map[string]any{{
			"id": appID, "live_url": "https://shop-abc12.ondigitalocean.app", "owner_uuid": canaryText,
			"spec": map[string]any{
				"name":    "shop",
				"domains": []any{map[string]any{"domain": "shop.acme.example", "type": "PRIMARY"}},
				"envs":    []any{map[string]any{"key": "API_KEY", "value": canarySecret, "type": "GENERAL"}},
				"services": []any{map[string]any{
					"name":   "web",
					"github": map[string]any{"repo": "acme/shop", "branch": "main"},
					"envs":   []any{map[string]any{"key": "DATABASE_URL", "value": canarySecret}},
				}},
				"workers": []any{map[string]any{"name": "jobs", "gitlab": map[string]any{"repo": "acme/jobs"},
					"run_command": canaryText}},
				"databases": []any{
					map[string]any{"name": "db", "cluster_name": "main-pg", "production": true, "db_user": canaryText},
					map[string]any{"name": "dev", "production": false},
				},
			},
		}},
		projects: map[string][]string{
			"p1": {"do:droplet:1001", "do:space:acme-assets", "do:space:acme-backups"},
			"p2": {"do:space:acme-assets", "do:dbaas:" + postgresID},
		},
	}
}

// atlasFake is the team as the atlas reads it, with a token made for the
// atlas that holds the atlas's scopes and extra, and nothing else.
func atlasFake(extra ...string) *fakeDO {
	f := newFakeDO()
	f.inv = atlasInventory()
	f.add(atlasTok, append(AtlasScopes(), extra...))
	return f
}

func atlasCompany() *config.Company {
	c := &config.Company{Name: "Acme"}
	c.Integrations.DigitalOcean = &config.DigitalOcean{Enabled: true,
		Provisioning: &config.DigitalOceanProvisioning{Team: "Example Team"},
		Atlas:        &config.DigitalOceanAtlas{Token: "${DIGITALOCEAN_ATLAS_TOKEN}"}}
	on := true
	c.Roles = []config.Role{{Name: "Nova", Handle: "nova",
		Integrations: config.RoleIntegrations{DigitalOcean: &config.RoleDigitalOcean{Enabled: &on}}}}
	return c
}

func (f *fakeDO) atlasInput(t *testing.T, edit ...func(*config.Company, config.MapSource)) collect.Input {
	t.Helper()
	c := atlasCompany()
	env := config.MapSource{"DIGITALOCEAN_ATLAS_TOKEN": atlasTok, "DIGITALOCEAN_TOKEN_NOVA": agentTok}
	for _, e := range edit {
		e(c, env)
	}
	return collect.Input{Config: c, Now: atlasNow, Env: config.NewResolver(env), HTTP: f.server(t)}
}

func (f *fakeDO) atlasRead(t *testing.T) collect.Read {
	t.Helper()
	read, err := atlasSource{}.Collect(t.Context(), f.atlasInput(t))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return read
}

// atlasView is claims as the atlas folds them.
func atlasView(claims []atlas.Claim) atlas.View {
	return atlas.Join(atlas.Fold(claims, atlasNow), atlas.Joined{}, atlasNow)
}

func atlasEntry(t *testing.T, v atlas.View, k atlas.Kind, key string) atlas.Entry {
	t.Helper()
	e, ok := v.Find(atlas.MustRef(k, key).ID())
	if !ok {
		t.Fatalf("%s %s is not in the read", k, key)
	}
	return e
}

// zoneHost is the fixture's zone as a name of its own.
func zoneHost(t *testing.T, v atlas.View) atlas.Entry {
	t.Helper()
	return atlasEntry(t, v, atlas.KindHostname, "acme.example")
}

// atlasFacts is every value an entry holds for a fact, sorted.
func atlasFacts(e atlas.Entry, f atlas.Fact) string {
	var out []string
	for _, fv := range e.Facts {
		if fv.Fact == f {
			out = append(out, fv.Value.Text)
		}
	}
	slices.Sort(out)
	return strings.Join(out, " ")
}

// relates reports whether e relies through rel on the entry k and key name.
func relates(e atlas.Entry, rel atlas.Relation, v atlas.View, k atlas.Kind, key string) bool {
	to, ok := v.Find(atlas.MustRef(k, key).ID())
	return ok && slices.ContainsFunc(e.Relations, func(ed atlas.Edge) bool {
		return ed.Relation == rel && ed.To == to.ID
	})
}

// THE READ CLAIMS WHAT THE TEAM RUNS ON, AND NOTHING THAT OPENS IT: each
// Droplet, cluster, load balancer, database, domain, name, app and bucket,
// what belongs to the team's account and what relies on what. A database's
// connection and users, an app's environment, a Droplet's user data and a
// record that is no address are never decoded, and the account's user is
// never read.
//
// Mutation: decode a database's connection or an app's environment into a
// claim, or read a domain's TXT records, and a canary reaches the read.
func TestTheAtlasReadsTheTeamsResources(t *testing.T) {
	t.Parallel()
	f := atlasFake()
	read := f.atlasRead(t)
	f.onlyDigitalOcean(t)
	if len(read.Withheld) != 0 || len(read.Unread) != 0 || read.Calls != len(f.requests) {
		t.Fatalf("withheld %+v, unread %+v, %d calls counted of %d made", read.Withheld, read.Unread,
			read.Calls, len(f.requests))
	}
	if len(f.revoked) != 0 {
		t.Fatalf("the read revoked %v", f.revoked)
	}
	v := atlasView(read.Claims)
	team := atlasEntry(t, v, atlas.KindAccount, teamKey)
	if atlasFacts(team, atlas.FactAccountName) != "Example Team" {
		t.Errorf("the team: %+v", team.Facts)
	}
	owned := func(e atlas.Entry) bool { return relates(e, atlas.RelationBelongsTo, v, atlas.KindAccount, teamKey) }

	web := atlasEntry(t, v, atlas.KindHost, "digitalocean/droplet/1001")
	if web.Name != "web-1" || atlasFacts(web, atlas.FactRegion) != "fra1" || !owned(web) ||
		atlasFacts(web, atlas.FactAddress) != "10.10.0.2 2001:db8::10 203.0.113.10" {
		t.Errorf("the Droplet: %q %+v %+v", web.Name, web.Facts, web.Relations)
	}
	// A NAME POINTING AT ONE OF THE TEAM'S OWN ADDRESSES points at the
	// resource, so no second server is claimed for the address.
	for _, c := range read.Claims {
		if c.Subject.Kind == atlas.KindHost && strings.HasPrefix(c.Subject.Key, atlas.HostAddrPrefix+"/") {
			t.Errorf("a server is claimed by its address alone: %s", c.Subject)
		}
	}
	prod := atlasEntry(t, v, atlas.KindHost, "digitalocean/kubernetes/"+clusterID)
	edge := atlasEntry(t, v, atlas.KindHost, "digitalocean/load-balancer/"+balancerID)
	if prod.Name != "prod" || !owned(prod) || edge.Name != "edge" || !owned(edge) ||
		atlasFacts(edge, atlas.FactAddress) != "203.0.113.20" {
		t.Errorf("the cluster %q %+v, the load balancer %q %+v", prod.Name, prod.Facts, edge.Name, edge.Facts)
	}
	pg := atlasEntry(t, v, atlas.KindDatastore, "digitalocean/database/"+postgresID)
	if pg.Name != "main-pg" || atlasFacts(pg, atlas.FactEngine) != "postgres" ||
		atlasFacts(pg, atlas.FactEngineVersion) != "16" || !owned(pg) ||
		!relates(pg, atlas.RelationRunsOn, v, atlas.KindAccount, teamKey) {
		t.Errorf("the database: %q %+v %+v", pg.Name, pg.Facts, pg.Relations)
	}
	if events := atlasEntry(t, v, atlas.KindQueue, "digitalocean/database/"+kafkaID); events.Name != "events" ||
		atlasFacts(events, atlas.FactEngine) != "kafka" {
		t.Errorf("the Kafka cluster: %q %+v", events.Name, events.Facts)
	}
	zone := atlasEntry(t, v, atlas.KindDomain, "acme.example")
	if !relates(zone, atlas.RelationServedBy, v, atlas.KindAccount, teamKey) || !owned(zone) {
		t.Errorf("the domain: %+v", zone.Relations)
	}
	www := atlasEntry(t, v, atlas.KindHostname, "www.acme.example")
	api := atlasEntry(t, v, atlas.KindHostname, "api.acme.example")
	docs := atlasEntry(t, v, atlas.KindHostname, "docs.acme.example")
	if !relates(www, atlas.RelationInZone, v, atlas.KindDomain, "acme.example") ||
		!relates(www, atlas.RelationResolvesTo, v, atlas.KindHost, "digitalocean/droplet/1001") ||
		len(www.Relations) != 2 ||
		!relates(zoneHost(t, v), atlas.RelationResolvesTo, v, atlas.KindHost, "digitalocean/load-balancer/"+balancerID) ||
		!relates(api, atlas.RelationResolvesTo, v, atlas.KindHostname, "acme.example") ||
		!relates(docs, atlas.RelationResolvesTo, v, atlas.KindHostname, "pages.vendor.example") {
		t.Errorf("the names: www %+v, api %+v, docs %+v", www.Relations, api.Relations, docs.Relations)
	}
	shop := atlasEntry(t, v, atlas.KindService, "shop")
	for _, want := range []struct {
		rel  atlas.Relation
		kind atlas.Kind
		key  string
	}{
		{atlas.RelationBelongsTo, atlas.KindAccount, teamKey},
		{atlas.RelationRunsOn, atlas.KindAccount, teamKey},
		{atlas.RelationExposes, atlas.KindEndpoint, "https://shop-abc12.ondigitalocean.app"},
		{atlas.RelationExposes, atlas.KindEndpoint, "https://shop.acme.example"},
		{atlas.RelationDependsOn, atlas.KindDatastore, "digitalocean/database/" + postgresID},
	} {
		if !relates(shop, want.rel, v, want.kind, want.key) {
			t.Errorf("the app does not %s %s: %+v", want.rel, want.key, shop.Relations)
		}
	}
	// A repository is named, never claimed: it lives at its forge.
	var built []string
	for _, c := range read.Claims {
		if c.Subject == shop.Ref && c.Facet.Relation == atlas.RelationBuiltFrom {
			built = append(built, c.Facet.Target.Entry.Key)
		}
		if c.Subject.Kind == atlas.KindRepository {
			t.Errorf("the read claims a repository: %+v", c)
		}
	}
	if slices.Sort(built); !slices.Equal(built, []string{"github.com/acme/shop", "gitlab.com/acme/jobs"}) {
		t.Errorf("the app is built from %v", built)
	}
	// A bucket two projects list is one bucket.
	buckets := 0
	for _, e := range v.Entries {
		if e.Ref.Kind == atlas.KindBucket {
			buckets++
		}
	}
	if assets := atlasEntry(t, v, atlas.KindBucket, "digitalocean/space/acme-assets"); buckets != 2 ||
		assets.Name != "acme-assets" || !owned(assets) {
		t.Errorf("%d buckets, the assets bucket %q %+v", buckets, assets.Name, assets.Relations)
	}
	out, err := json.Marshal(read)
	if err != nil {
		t.Fatal(err)
	}
	for _, canary := range []string{canaryText, canarySecret, canaryEmail, "doadmin", atlasTok} {
		if strings.Contains(string(out), canary) {
			t.Errorf("%q reached the read", canary)
		}
	}
}

// THE READ MEETS THE SETTINGS ON ONE ENTRY, WHATEVER THE TEAM IS CALLED: a
// team's name may hold a space and an account's key may not, so both key the
// team by its name's slug.
//
// Mutation: key the account by the name as typed, and a team with a space in
// its name is no entry at all.
func TestTheDigitalOceanReadAndTheSettingsNameOneTeam(t *testing.T) {
	t.Parallel()
	read := atlasFake().atlasRead(t)
	settings := atlas.SettingsSource("r1")
	ref := atlas.MustRef(atlas.KindAccount, "digitalocean/"+config.DigitalOceanTeamRef("Example Team"))
	exists, err := atlas.Exists(ref, settings, atlasNow)
	if err != nil {
		t.Fatal(err)
	}
	named, err := atlas.Says(ref, atlas.FactName, "DigitalOcean", settings, atlasNow)
	if err != nil {
		t.Fatal(err)
	}
	v := atlasView(append([]atlas.Claim{exists, named}, read.Claims...))
	if team := atlasEntry(t, v, atlas.KindAccount, teamKey); team.Name != "DigitalOcean" || team.Review ||
		len(team.Sources) != 2 {
		t.Errorf("the team: %q, review %v, sources %+v", team.Name, team.Review, team.Sources)
	}
}

// A TOKEN THAT CAN CHANGE ANYTHING, OR READ A SECRET, READS NOTHING: the
// atlas proves its token before every read with requests that cannot have an
// effect, and a token holding any scope the proof can see fails the read
// before one listing is made. So does a token DigitalOcean no longer knows,
// one of another team, and one of a kind that expires.
//
// Mutation: read with whatever token is sealed, and a Full Access token
// pasted for the atlas reads the team under the atlas's name.
func TestATokenThatCanWriteReadsNothing(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		extra []string
		edit  func(*fakeDO, config.MapSource)
		says  string
		dead  bool
	}{
		"a write scope":        {extra: []string{"droplet:create"}, says: "droplet:create"},
		"a delete scope":       {extra: []string{"load_balancer:delete"}, says: "load_balancer:delete"},
		"a read-only token":    {extra: []string{"billing:read"}, says: "billing:read"},
		"stored secrets":       {extra: []string{"security:read"}, says: "security:read"},
		"database passwords":   {extra: []string{"database:view_credentials"}, says: "database:view_credentials"},
		"cluster credentials":  {extra: []string{"kubernetes:access_cluster"}, says: "kubernetes:access_cluster"},
		"another team's token": {edit: func(f *fakeDO, _ config.MapSource) { f.tokens[atlasTok].team = "Other" }, says: "team Other"},
		"a locked account":     {edit: func(f *fakeDO, _ config.MapSource) { f.tokens[atlasTok].status = "locked" }, says: "locked"},
		"a deleted token": {edit: func(f *fakeDO, _ config.MapSource) { f.tokens[atlasTok].revoked = true },
			says: "did not accept", dead: true},
		"an OAuth token": {edit: func(_ *fakeDO, env config.MapSource) {
			env["DIGITALOCEAN_ATLAS_TOKEN"] = "doo_v1_" + strings.Repeat("ab", 32)
		}, says: "OAuth token"},
	} {
		f := atlasFake(tc.extra...)
		in := f.atlasInput(t, func(_ *config.Company, env config.MapSource) {
			if tc.edit != nil {
				tc.edit(f, env)
			}
		})
		read, err := atlasSource{}.Collect(t.Context(), in)
		if err == nil || errors.Is(err, integration.ErrNotConfigured) || len(read.Claims) != 0 ||
			!strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s: the read answered %d claims and %v", name, len(read.Claims), err)
		}
		if errors.Is(err, integration.ErrCredentialRejected) != tc.dead {
			t.Errorf("%s: a rejected credential is %v, want %v: %v", name, !tc.dead, tc.dead, err)
		}
		for path, asked := range f.inv.pages {
			// The proof asks a listing for one row, once; the read never starts.
			if asked > 1 || strings.Contains(path, "/records") || strings.Contains(path, "/resources") {
				t.Errorf("%s: the refused token read %s %d times", name, path, asked)
			}
		}
		if err != nil && strings.Contains(err.Error(), atlasTok) {
			t.Errorf("%s: the fault carries the token", name)
		}
	}
}

// THE ATLAS NEVER READS AS AN AGENT: a token sealed for a seat is refused as
// the atlas's before DigitalOcean is asked anything, so DigitalOcean's own
// log never shows an agent's token making the atlas's reads.
//
// Mutation: drop the comparison, and one token pasted in both places reads
// the team hourly as the agent.
func TestAnAgentsTokenIsNeverTheAtlass(t *testing.T) {
	t.Parallel()
	f := atlasFake()
	f.add(agentTok, AtlasScopes())
	in := f.atlasInput(t, func(_ *config.Company, env config.MapSource) {
		env["DIGITALOCEAN_ATLAS_TOKEN"] = agentTok
	})
	_, err := atlasSource{}.Collect(t.Context(), in)
	if err == nil || !strings.Contains(err.Error(), "nova's own token") || len(f.requests) != 0 {
		t.Fatalf("an agent's token read as the atlas: %v, after %v", err, f.requests)
	}
}

// A SCOPE THE TOKEN LACKS IS A FINDING AND RETIRES NOTHING: its listing is
// left unread, the standing says which scope to tick, every other part is
// read, and what an earlier read recorded of that part stands, an app's
// attachment to a database included.
//
// Mutation: read the refused listing as an empty one, and every database is
// retired a day after somebody pastes a narrower token.
func TestAScopeTheTokenLacksIsAFindingAndRetiresNothing(t *testing.T) {
	t.Parallel()
	whole := atlasFake().atlasRead(t)
	f := atlasFake()
	p := f.tokens[atlasTok]
	p.scopes = slices.DeleteFunc(p.scopes, func(s string) bool { return s == "database:read" })
	read := f.atlasRead(t)
	if len(read.Unread) != 1 || read.Unread[0].Part != "databases" ||
		!strings.Contains(read.Unread[0].Detail, "database:read") {
		t.Fatalf("the refused listing: %+v", read.Unread)
	}
	v := atlasView(read.Claims)
	if _, ok := v.Find(atlas.MustRef(atlas.KindDatastore, "digitalocean/database/"+postgresID).ID()); ok {
		t.Error("a database was read without database:read")
	}
	atlasEntry(t, v, atlas.KindHost, "digitalocean/droplet/1001")
	atlasEntry(t, v, atlas.KindService, "shop")
	status := collect.Status{Kind: integration.KindDigitalOcean}
	for _, at := range []time.Time{atlasNow.Add(time.Hour), atlasNow.Add(30 * time.Hour), atlasNow.Add(60 * time.Hour)} {
		plan := collect.Decide(whole.Claims, read, status, at)
		if len(plan.Records) != 0 || len(plan.Status.Missing) != 0 || len(plan.Status.Unread) != 1 {
			t.Fatalf("at %s the refusal recorded %+v, missed %+v and found %+v", at, plan.Records,
				plan.Status.Missing, plan.Status.Unread)
		}
		status = plan.Status
	}
	// The same read, were the listing whole and empty, retires both clusters.
	empty := read
	empty.Unread = nil
	first := collect.Decide(whole.Claims, empty, collect.Status{Kind: integration.KindDigitalOcean}, atlasNow.Add(time.Hour))
	if plan := collect.Decide(whole.Claims, empty, first.Status, atlasNow.Add(26*time.Hour)); plan.Status.Retired != 2 {
		t.Errorf("the control retired %d databases, want 2", plan.Status.Retired)
	}
	// EVERY PART HOLDS ITS OWN CLAIMS AND NO OTHER'S, the team's own aside.
	parts := []part{partDroplets, partClusters, partBalancers, partDatabases, partDomains, partApps, partSpaces}
	for _, c := range whole.Claims {
		var held []string
		for _, p := range parts {
			if p.holds(c) {
				held = append(held, p.name)
			}
		}
		switch {
		case c.Subject.Key == teamKey && len(held) == 0:
		case attaches(c) && slices.Equal(held, []string{"databases", "apps"}):
		case !attaches(c) && c.Subject.Key != teamKey && len(held) == 1:
		default:
			t.Errorf("%s %s is held by %v", c.Subject, c.Slot(), held)
		}
	}
}

// NO CREDENTIAL'S SHAPE REACHES THE ATLAS FROM DIGITALOCEAN, whichever field
// a person typed it into: a name, an id, an address, a record, a repository
// or a bucket shaped like one is withheld where it sat, and what sat beside
// it is still read. The fields the read never decodes carry one too.
//
// Mutation: skip the builder's grammar on a key or a value, or write a
// bucket's name into its key before judging it, and a shape reaches the read.
func TestNoCredentialShapeReachesTheAtlasFromDigitalOcean(t *testing.T) {
	t.Parallel()
	for _, shape := range redacttest.Shapes() {
		s := shape.Sample
		f := atlasFake()
		inv := f.inv
		inv.droplets = append(inv.droplets,
			map[string]any{"id": 1002, "name": s, "region": map[string]any{"slug": s}, "user_data": s,
				"networks": map[string]any{"v4": []any{map[string]any{"ip_address": s}}}},
			map[string]any{"id": 1003, "name": "beside", "region": map[string]any{"slug": "fra1"}})
		inv.clusters = append(inv.clusters, map[string]any{"id": s, "name": s, "region": s, "endpoint": s})
		inv.databases = append(inv.databases, map[string]any{"id": s, "name": s, "engine": s, "version": s,
			"region": s, "connection": map[string]any{"uri": s, "password": s}})
		inv.balancers = append(inv.balancers, map[string]any{"id": s, "name": s, "ip": s, "ipv6": s,
			"region": map[string]any{"slug": s}})
		inv.domains = append(inv.domains, map[string]any{"name": s, "zone_file": s})
		inv.records["acme.example"] = append(inv.records["acme.example"],
			map[string]any{"id": 7, "type": "A", "name": s, "data": s},
			map[string]any{"id": 8, "type": "CNAME", "name": "alias", "data": s},
			map[string]any{"id": 9, "type": "TXT", "name": "@", "data": s})
		inv.apps = append(inv.apps, map[string]any{"id": s, "live_url": "https://" + s + ".example/?key=" + s,
			"spec": map[string]any{"name": s, "domains": []any{map[string]any{"domain": s}},
				"envs":      []any{map[string]any{"key": s, "value": s}},
				"services":  []any{map[string]any{"github": map[string]any{"repo": s}, "envs": []any{map[string]any{"value": s}}}},
				"databases": []any{map[string]any{"cluster_name": s, "production": true}}}},
			map[string]any{"id": "eeeeeeee-0000-4000-8000-000000000002",
				"live_url": "https://deploy:" + canarySecret + "@pay.acme.example/?token=" + canarySecret,
				"spec":     map[string]any{"name": "pay", "services": []any{map[string]any{"github": map[string]any{"repo": "acme/" + s}}}}})
		inv.projects["p3"] = []string{"do:space:" + s, "do:space:beside"}
		read := f.atlasRead(t)
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
		if len(read.Withheld) < 12 {
			t.Errorf("%s: %d values withheld, want every planted mapped field: %+v", shape.Name,
				len(read.Withheld), read.Withheld)
		}
		// What sat beside a credential is still read.
		v := atlasView(read.Claims)
		atlasEntry(t, v, atlas.KindHost, "digitalocean/droplet/1003")
		atlasEntry(t, v, atlas.KindBucket, "digitalocean/space/beside")
		atlasEntry(t, v, atlas.KindHostname, "alias.acme.example")
		// An address keeps its place and loses the credential it carried.
		pay := atlasEntry(t, v, atlas.KindService, "pay")
		if !relates(pay, atlas.RelationExposes, v, atlas.KindEndpoint, "https://pay.acme.example/") {
			t.Errorf("%s: the app beside the credential lost its address: %+v", shape.Name, pay.Relations)
		}
		if strings.Contains(string(out), canarySecret) {
			t.Errorf("%s: an address kept the credential it carried", shape.Name)
		}
	}
}

// A LISTING THAT DID NOT FINISH IS NO READ AT ALL: a server fault on any
// listing fails the read whole, so nothing is retired on a listing cut
// short. A listing longer than a page is read to its end.
//
// Mutation: stop at the first page, and a team's hundred-and-first Droplet is
// retired a day later.
func TestAListingThatFailsFailsTheDigitalOceanRead(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"/v2/droplets", "/v2/databases", "/v2/domains/acme.example/records",
		"/v2/apps", "/v2/projects/p2/resources"} {
		f := atlasFake()
		f.inv.fail = map[string]int{path: http.StatusBadGateway}
		// The proof asks the listing for one row, so the fault starts after it.
		read, err := readAfterProof(t, f)
		if err == nil || errors.Is(err, integration.ErrNotConfigured) || len(read.Claims) != 0 {
			t.Errorf("%s: the read answered %d claims and %v", path, len(read.Claims), err)
		}
	}
	paged := atlasFake()
	paged.inv.pageCap = 2
	for i := range 5 {
		paged.inv.droplets = append(paged.inv.droplets, map[string]any{"id": 2000 + i, "name": "node-" + strconv.Itoa(i)})
	}
	read := paged.atlasRead(t)
	v := atlasView(read.Claims)
	for i := range 5 {
		atlasEntry(t, v, atlas.KindHost, "digitalocean/droplet/"+strconv.Itoa(2000+i))
	}
	// Six Droplets two a page are three pages, beside the proof's own row.
	if paged.inv.pages["/v2/droplets"] != 4 {
		t.Errorf("the Droplets were read in %d requests, want 4", paged.inv.pages["/v2/droplets"])
	}
}

// readAfterProof reads with the listing faults held off until the proof of
// the token, which asks each listing for a row, has passed.
func readAfterProof(t *testing.T, f *fakeDO) (collect.Read, error) {
	t.Helper()
	fail := f.inv.fail
	f.inv.fail = nil
	in := f.atlasInput(t)
	client, err := NewClient(ClientOptions{Token: atlasTok, HTTP: in.HTTP})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := CheckAtlas(t.Context(), client, AtlasInput{Token: atlasTok, Team: "Example Team"}); err != nil {
		t.Fatalf("the proof: %v", err)
	}
	f.mu.Lock()
	f.inv.fail = fail
	f.mu.Unlock()
	return atlasSource{}.Collect(t.Context(), in)
}

// A COMPANY THAT DOES NOT CONNECT DIGITALOCEAN, OR GAVE THE ATLAS NO TOKEN,
// IS NOT CONFIGURED, which the duty reads as nothing to read rather than a
// fault: the token is optional, and nothing is asked of DigitalOcean without it.
func TestADigitalOceanWithNoAtlasTokenIsNotConfigured(t *testing.T) {
	t.Parallel()
	for name, edit := range map[string]func(*config.Company, config.MapSource){
		"no block":     func(c *config.Company, _ config.MapSource) { c.Integrations.DigitalOcean = nil },
		"switched off": func(c *config.Company, _ config.MapSource) { c.Integrations.DigitalOcean.Enabled = false },
		"no team":      func(c *config.Company, _ config.MapSource) { c.Integrations.DigitalOcean.Provisioning = nil },
		"no atlas":     func(c *config.Company, _ config.MapSource) { c.Integrations.DigitalOcean.Atlas = nil },
		"no token":     func(_ *config.Company, env config.MapSource) { delete(env, "DIGITALOCEAN_ATLAS_TOKEN") },
	} {
		f := atlasFake()
		if _, err := (atlasSource{}).Collect(t.Context(), f.atlasInput(t, edit)); !errors.Is(err, integration.ErrNotConfigured) {
			t.Errorf("%s: %v, want not configured", name, err)
		}
		if len(f.requests) != 0 {
			t.Errorf("%s: the read called DigitalOcean: %v", name, f.requests)
		}
	}
	if _, err := (atlasSource{}).Collect(t.Context(), collect.Input{}); !errors.Is(err, integration.ErrNotConfigured) {
		t.Errorf("no document: %v, want not configured", err)
	}
	if src, ok := collect.Lookup(integration.KindDigitalOcean); !ok || src.Kind() != integration.KindDigitalOcean {
		t.Error("no DigitalOcean source is registered")
	}
}

// THE CONNECT FORM OFFERS THE ATLAS'S TOKEN AS AN OPTION AND TESTS IT AS THE
// READ PROVES IT: a token that reads and changes nothing works, one that
// lacks a read works and says what is left unread, and one that can write is
// refused with what to do. Nothing else of the form is a credential.
func TestTheConnectFormTestsTheAtlasToken(t *testing.T) {
	t.Parallel()
	none := func(string) (string, bool) { return "", false }
	var field *setup.Requirement
	for _, r := range Requirements(nil, none) {
		if r.Kind == setup.KindSecret || r.Checkable {
			if r.Field != AtlasField || r.Required || r.Connect || !strings.HasPrefix(r.Hint, "See more in Atlas") {
				t.Errorf("%s = %+v", r.Field, r)
			}
			field = &r
		}
	}
	if field == nil || field.ConfigPath != "integrations.digitalocean.atlas.token" {
		t.Fatalf("the form offers no atlas token: %+v", field)
	}
	for _, scope := range AtlasScopes() {
		if !strings.Contains(field.More, scope) || !strings.HasSuffix(scope, ":read") {
			t.Errorf("the field's step leaves out %s, or it is no read", scope)
		}
	}
	check := func(f *fakeDO, token string) setup.Verdict {
		values := map[string]string{AtlasField: token, "team": "Example Team"}
		v, err := Probe(f.server(t))(context.Background(), setup.Check{Field: AtlasField,
			Value: func(field string) string { return values[field] }})
		if err != nil {
			t.Fatalf("the probe: %v", err)
		}
		return v
	}
	if v := check(atlasFake(), atlasTok); v.Outcome != setup.VerifyWorks || len(v.Warnings) != 0 {
		t.Errorf("a token made for the atlas: %+v", v)
	}
	narrow := atlasFake()
	narrow.tokens[atlasTok].scopes = []string{"account:read", "droplet:read"}
	if v := check(narrow, atlasTok); v.Outcome != setup.VerifyWorks || len(v.Warnings) != 6 {
		t.Errorf("a token with one read: %+v", v)
	}
	if v := check(atlasFake("domain:update"), atlasTok); v.Outcome != setup.VerifyRefused ||
		!strings.Contains(v.Summary, "domain:update") || strings.Contains(v.Summary, atlasTok) {
		t.Errorf("a token that can write: %+v", v)
	}
	if _, err := Probe(nil)(context.Background(), setup.Check{Field: "team",
		Value: func(string) string { return "Example Team" }}); !errors.Is(err, setup.ErrNotCheckable) {
		t.Errorf("the probe checked the team: %v", err)
	}
}
