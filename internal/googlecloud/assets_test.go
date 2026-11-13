package googlecloud_test

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googlecloud"
	"github.com/crewlet/crewlet/internal/redact/redacttest"
)

// asset is one search result as Google answers it, with the fields the read
// maps: its full resource name, type, display name, location and state.
func asset(name, assetType, display, location, state string) map[string]any {
	return map[string]any{"name": name, "assetType": assetType, "displayName": display, "location": location,
		"state": state}
}

// assetWorld is atlasWorld with the atlas's account made and acme-web running
// an instance, a database, a Cloud Run service, a function, a bucket and a
// topic; acme-data's search is refused, as a project the role was never
// granted on.
func assetWorld(t *testing.T) *atlasWorld {
	t.Helper()
	w := newAtlasWorld(t)
	w.f.addAtlas()
	vm := asset("//compute.googleapis.com/projects/"+projectWeb+"/zones/europe-west1-b/instances/web-1",
		googlecloud.AssetInstance, "web-1", "europe-west1-b", "RUNNING")
	vm["additionalAttributes"] = map[string]any{"internalIPs": []string{"10.0.0.5"}, "externalIPs": []string{"203.0.113.9"},
		"machineType": "atlas-canary-machine", "networkInterfaceNetworks": []string{"atlas-canary-net"}}
	vm["versionedResources"] = []any{map[string]any{"resource": map[string]any{
		"metadata": map[string]any{"items": []any{map[string]any{"key": "startup-script", "value": "atlas-canary-secret"}}}}}}
	fn := asset("//cloudfunctions.googleapis.com/projects/"+projectWeb+"/locations/europe-west1/functions/resize",
		"cloudfunctions.googleapis.com/Function", "resize", "europe-west1", "ACTIVE")
	fn["versionedResources"] = []any{map[string]any{"resource": map[string]any{
		"environmentVariables": map[string]any{"API_KEY": "atlas-canary-secret"}}}}
	w.f.assets = map[string][]map[string]any{
		projectWeb: {
			vm,
			asset("//compute.googleapis.com/projects/"+projectWeb+"/zones/europe-west1-c/instances/batch",
				googlecloud.AssetInstance, "", "europe-west1-c", "TERMINATED"),
			asset("//sqladmin.googleapis.com/projects/"+projectWeb+"/instances/orders",
				"sqladmin.googleapis.com/Instance", "orders", "europe-west1", "RUNNABLE"),
			asset("//redis.googleapis.com/projects/"+projectWeb+"/locations/europe-west1/instances/cache",
				"redis.googleapis.com/Instance", "cache", "europe-west1", "READY"),
			asset("//run.googleapis.com/projects/"+projectWeb+"/locations/europe-west1/services/api",
				"run.googleapis.com/Service", "api", "europe-west1", ""),
			fn,
			asset("//storage.googleapis.com/acme-web-assets", "storage.googleapis.com/Bucket", "acme-web-assets", "EU", ""),
			asset("//pubsub.googleapis.com/projects/"+projectWeb+"/topics/orders", "pubsub.googleapis.com/Topic",
				"orders", "global", ""),
			asset("//compute.googleapis.com/projects/"+projectWeb+"/zones/europe-west1-b/instances/going",
				googlecloud.AssetInstance, "going", "europe-west1-b", "DELETING"),
			asset("//container.googleapis.com/projects/"+projectWeb+"/locations/europe-west1/clusters/gke",
				"container.googleapis.com/Cluster", "gke", "europe-west1", "RUNNING"),
		},
	}
	return w
}

// atlasOn is the company with the atlas's reader asked for.
func atlasOn(c *config.Company) { c.Integrations.GoogleCloud.Provisioning.AtlasRead = true }

func (w *atlasWorld) readWith(t *testing.T, edit ...func(*config.Company)) collect.Read {
	t.Helper()
	read, err := googleSource(t).Collect(t.Context(), w.input(t, edit...))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return read
}

// lines is every claim of a read as one line, sorted.
func lines(read collect.Read) []string {
	out := make([]string, 0, len(read.Claims))
	for _, c := range read.Claims {
		line := c.Subject.String()
		switch c.Facet.Kind {
		case atlas.FacetFact:
			line += " " + string(c.Facet.Fact) + "=" + c.Value.Text
		case atlas.FacetRelation:
			line += " " + string(c.Facet.Relation) + " " + c.Facet.Target.Entry.String()
		}
		out = append(out, line)
	}
	slices.Sort(out)
	return out
}

// WHAT EACH PROJECT RUNS IS READ AS THE ATLAS'S OWN ACCOUNT, BY NAME AND TYPE:
// an instance as a server with its addresses, a database with the engine its
// type names, a Cloud Run service and a function as workloads, a bucket by
// its global name, a topic as a queue. A type outside the closed list and a
// resource being deleted are left out; a stopped instance is still a server.
func TestTheAtlasReadsWhatEachProjectRunsAsItsOwnAccount(t *testing.T) {
	t.Parallel()
	w := assetWorld(t)
	read := w.readWith(t, atlasOn)
	got := lines(read)
	const (
		vm = "host:google_cloud/" + projectWeb + "/europe-west1-b/web-1"
		db = "datastore:google_cloud/" + projectWeb + "/europe-west1/orders"
	)
	for _, want := range []string{
		vm, vm + " name=web-1", vm + " address=10.0.0.5", vm + " address=203.0.113.9", vm + " region=europe-west1",
		vm + " belongs_to vendor_account:" + webKey,
		"host:google_cloud/" + projectWeb + "/europe-west1-c/batch name=batch",
		db + " name=orders", db + " runs_on vendor_account:" + webKey, db + " region=europe-west1",
		"datastore:google_cloud/" + projectWeb + "/europe-west1/cache engine=redis",
		"workload:google_cloud/" + projectWeb + "/europe-west1/api runs_on vendor_account:" + webKey,
		"workload:google_cloud/" + projectWeb + "/europe-west1/resize name=resize",
		"bucket:gcs/acme-web-assets region=eu", "bucket:gcs/acme-web-assets belongs_to vendor_account:" + webKey,
		"vendor_account:" + webKey + " capability=object_storage",
		"queue:google_cloud/" + projectWeb + "/global/orders runs_on vendor_account:" + webKey,
	} {
		if !slices.Contains(got, want) {
			t.Errorf("the read does not say %q", want)
		}
	}
	for _, line := range got {
		if strings.Contains(line, "/going") || strings.Contains(line, "/gke") {
			t.Errorf("the read says %q: a resource being deleted and a type outside the list are not recorded", line)
		}
		if strings.Contains(line, "orders engine=") {
			t.Errorf("the read says %q: a Cloud SQL instance's engine is in its configuration, which is not read", line)
		}
	}
	if w.sent("GET /v1/projects/"+projectWeb+":searchAllResources") != 1 {
		t.Error("acme-web's resources were not searched once")
	}
}

// THE SEARCH ASKS FOR THE NAMED FIELDS AND TYPES ALONE: an explicit read mask
// without versionedResources, labels, tags or descriptions, and the closed
// list of types. Nothing a result carries beyond them reaches the read, a
// function's environment and an instance's startup script least of all.
func TestTheSearchAsksOnlyForNamesAndTypes(t *testing.T) {
	t.Parallel()
	w := assetWorld(t)
	read := w.readWith(t, atlasOn)
	var queries []map[string][]string
	w.f.edit(func(f *fakeGoogle) {
		for _, q := range f.searched {
			queries = append(queries, q)
		}
	})
	if len(queries) == 0 {
		t.Fatal("no search was made")
	}
	for _, q := range queries {
		mask := strings.Split(strings.Join(q["readMask"], ","), ",")
		slices.Sort(mask)
		if !slices.Equal(mask, []string{"additionalAttributes", "assetType", "displayName", "location", "name", "state"}) {
			t.Errorf("a search asked for %v, want the six named fields alone", mask)
		}
		if len(q["assetTypes"]) == 0 || slices.ContainsFunc(q["assetTypes"], func(s string) bool {
			return strings.ContainsAny(s, "*.?|") && !strings.Contains(s, "googleapis.com/")
		}) {
			t.Errorf("a search asked for the types %v, want the closed list", q["assetTypes"])
		}
	}
	out, err := json.Marshal(read)
	if err != nil {
		t.Fatal(err)
	}
	for _, absent := range []string{"atlas-canary", canaryText, canaryEmail} {
		if strings.Contains(string(out), absent) {
			t.Errorf("the read carries %q, which sits only in fields the read does not map", absent)
		}
	}
}

// A PROJECT WHOSE SEARCH GOOGLE REFUSES IS A FINDING, and what an earlier read
// recorded of it stands; every other project is read. An atlas account that
// cannot be reached at all is one finding naming the block, which keeps every
// resource recorded.
func TestARefusedSearchIsAFindingAndWithdrawsNothing(t *testing.T) {
	t.Parallel()
	whole := assetWorld(t)
	whole.f.assets[projectData] = []map[string]any{asset("//compute.googleapis.com/projects/"+projectData+
		"/zones/us-central1-a/instances/etl", googlecloud.AssetInstance, "etl", "us-central1-a", "RUNNING")}
	before := whole.readWith(t, atlasOn)

	w := assetWorld(t)
	read := w.readWith(t, atlasOn)
	var part *collect.Unread
	for i := range read.Unread {
		if read.Unread[i].Part == "assets" {
			part = &read.Unread[i]
		}
	}
	if part == nil || !strings.Contains(part.Detail, "Prepare a project") {
		t.Fatalf("unread %+v, want the refused projects' resources named with their fix", read.Unread)
	}
	for _, c := range before.Claims {
		if slices.ContainsFunc(read.Claims, func(o atlas.Claim) bool { return o.Subject == c.Subject && o.Slot() == c.Slot() }) {
			continue
		}
		if !slices.ContainsFunc(read.Unread, func(u collect.Unread) bool { return u.Holds != nil && u.Holds(c) }) {
			t.Errorf("the claim %s %s would be withdrawn by a search that was only refused", c.Subject, c.Slot())
		}
	}
	if part.Holds(atlas.Claim{Subject: atlas.MustRef(atlas.KindHost, "google_cloud/"+projectWeb+"/europe-west1-b/web-1")}) {
		t.Error("a refused project's finding holds a server of a project that was read")
	}

	for name, breakIt := range map[string]func(*fakeGoogle){
		"no account":             func(f *fakeGoogle) { delete(f.accounts, "100000000000000000099") },
		"no Workload Identity":   func(f *fakeGoogle) { f.atlasTrusted = false },
		"the API off everywhere": nil,
	} {
		w := assetWorld(t)
		if breakIt != nil {
			w.f.edit(breakIt)
		} else {
			w.f.edit(func(f *fakeGoogle) {
				f.refuse["GET /v1/projects/"+projectWeb+":searchAllResources"] = http.StatusForbidden
			})
		}
		read := w.readWith(t, atlasOn)
		held := slices.ContainsFunc(read.Unread, func(u collect.Unread) bool {
			return (u.Part == "atlas_reader" || u.Part == "assets") &&
				u.Holds(atlas.Claim{Subject: atlas.MustRef(atlas.KindBucket, "gcs/acme-web-assets")})
		})
		if !held || slices.ContainsFunc(read.Claims, func(c atlas.Claim) bool { return c.Subject.Kind == atlas.KindHost }) {
			t.Errorf("%s: unread %+v, want every recorded resource kept and none read", name, read.Unread)
		}
		if !slices.ContainsFunc(read.Claims, func(c atlas.Claim) bool { return c.Subject.Key == webKey }) {
			t.Errorf("%s: the projects themselves were not read", name)
		}
	}
}

// THE READER IS THE COMPANY'S CHOICE: off, no search is made and the atlas's
// account is never asked for a token.
func TestNoSearchIsMadeUntilTheCompanyAsks(t *testing.T) {
	t.Parallel()
	w := assetWorld(t)
	read := w.readWith(t)
	if slices.ContainsFunc(read.Claims, func(c atlas.Claim) bool { return c.Subject.Kind == atlas.KindHost }) ||
		w.sent("GET /v1/projects/"+projectWeb+":searchAllResources") != 0 ||
		w.sent("POST /v1/projects/-/serviceAccounts/"+googlecloud.AtlasAccountIn(identity)+":generateAccessToken") != 0 {
		t.Fatal("with the reader off, the atlas's account was used")
	}
}

// A SEARCH IS WALKED TO ITS LAST PAGE.
func TestEverySearchPageIsRead(t *testing.T) {
	t.Parallel()
	w := assetWorld(t)
	w.f.assetPage = 2
	got := lines(w.readWith(t, atlasOn))
	if !slices.Contains(got, "queue:google_cloud/"+projectWeb+"/global/orders name=orders") {
		t.Error("the last page of a paged search was not read")
	}
}

// NO CREDENTIAL'S SHAPE REACHES THE ATLAS FROM A SEARCH, whichever field
// carries it: a mapped one is refused by the vocabulary, an unmapped one is
// never decoded.
func TestNoCredentialShapeReachesTheAtlasFromASearch(t *testing.T) {
	t.Parallel()
	for _, shape := range redacttest.Shapes() {
		s := shape.Sample
		long := strings.Repeat("m", max(1, atlas.MaxLabelRunes-len(s)+len(s)/4)) + " " + s
		w := assetWorld(t)
		vm := asset("//compute.googleapis.com/projects/"+projectWeb+"/zones/europe-west1-b/instances/"+s,
			googlecloud.AssetInstance, long, s, s)
		vm["additionalAttributes"] = map[string]any{"internalIPs": []string{s}, "externalIPs": []string{s},
			"machineType": s}
		vm["versionedResources"] = []any{map[string]any{"resource": map[string]any{"env": s}}}
		w.f.assets[projectWeb] = append(w.f.assets[projectWeb], vm,
			asset("//storage.googleapis.com/"+s, "storage.googleapis.com/Bucket", s, s, s),
			asset("//run.googleapis.com/projects/"+projectWeb+"/locations/"+s+"/services/x", "run.googleapis.com/Service",
				s, s, ""))
		w.f.unmapped = map[string]any{"labels": map[string]string{"token": s}, "description": s, "tags": []string{s}}
		out, err := json.Marshal(w.readWith(t, atlasOn))
		if err != nil {
			t.Fatal(err)
		}
		for _, piece := range []string{s[len(s)/2:], s[len(s)/4 : len(s)/4+12]} {
			if at := strings.Index(string(out), piece); at >= 0 {
				t.Errorf("%s: a piece of the shape reached the read near %q", shape.Name, string(out)[max(0, at-120):at])
			}
		}
	}
}

// THE IDENTITY BLOCK MAKES THE ATLAS'S ACCOUNT ONLY WHERE THE COMPANY ASKS,
// trusts it to the same principal as the provisioner, grants it the
// one-permission role on the provisioner's projects alone, and still prints
// the provider last.
func TestTheBlockMakesTheAtlasAccountOnlyWhenAsked(t *testing.T) {
	t.Parallel()
	_, pemText := fakeKey(t)
	off, err := googlecloud.CloudShellBlock(testCompany, provisioner, pemText, false)
	if err != nil {
		t.Fatal(err)
	}
	on, err := googlecloud.CloudShellBlock(testCompany, provisioner, pemText, true)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(off, googlecloud.AtlasAccountID) || strings.Contains(off, "cloudasset") {
		t.Fatal("the block makes the atlas's account with the reader off")
	}
	for _, want := range []string{
		"gcloud iam service-accounts create " + googlecloud.AtlasAccountID,
		"gcloud services enable cloudasset.googleapis.com",
		"--permissions=" + googlecloud.AtlasPermission + " ",
		`--filter="bindings.members:serviceAccount:$PROV"`,
		`--role="projects/$P/roles/` + googlecloud.AtlasRoleID + `"`,
	} {
		if !strings.Contains(on, want) {
			t.Errorf("the block with the reader does not say %s", want)
		}
	}
	if strings.Contains(on, "roles/cloudasset.viewer") || strings.Contains(on, "roles/cloudasset.owner") {
		t.Error("the block grants a predefined Cloud Asset role, which lists and exports whole assets")
	}
	if strings.Count(on, "--permissions=") != 2 || strings.Count(on, "--permissions="+googlecloud.AtlasPermission) != 2 {
		t.Error("the atlas's role holds a permission beside searchAllResources")
	}
	trusted := func(text string) []string {
		var out []string
		for _, s := range statements(text) {
			if strings.Contains(s, "roles/iam.workloadIdentityUser") {
				_, rest, _ := strings.Cut(s, `--member="`)
				member, _, _ := strings.Cut(rest, `"`)
				out = append(out, member)
			}
		}
		return out
	}
	if got := trusted(on); len(got) != 2 || got[0] != got[1] {
		t.Errorf("the provisioner and the atlas trust %v, want one principal", got)
	}
	tail := strings.Split(strings.TrimSpace(on), "\n")
	if tail[len(tail)-1] != ")" || !strings.HasPrefix(strings.TrimSpace(tail[len(tail)-3]),
		"gcloud iam workload-identity-pools providers describe "+googlecloud.ProviderID) {
		t.Errorf("the block with the reader does not print the provider last:\n%s", strings.Join(tail[len(tail)-4:], "\n"))
	}
}

// THE CONNECT FORM OFFERS THE READER AND NEVER REQUIRES IT, and the block it
// shows holds the atlas's account exactly when the switch is on.
func TestTheConnectFormOffersTheAtlasReader(t *testing.T) {
	t.Parallel()
	_, pemText := fakeKey(t)
	resolve := func(ref string) (string, bool) { return pemText, ref != "" }
	for _, on := range []bool{false, true} {
		in := &config.GoogleCloud{Enabled: true, Provisioning: &config.GoogleCloudProvisioning{Provisioner: provisioner,
			FederationKey: "${" + googlecloud.FederationKeySecret + "}", AtlasRead: on}}
		var block string
		found := false
		for _, req := range googlecloud.Requirements(testCompany, in, resolve) {
			switch req.Field {
			case "atlas_read":
				found = true
				if req.Required || req.Connect || req.Blocks != "" || req.Default != "false" || req.Present != on ||
					!strings.Contains(req.More, "To undo it") || !strings.Contains(req.More, "Cloud Shell block") {
					t.Errorf("the reader's switch is %+v: optional, off, with its step and its undo", req)
				}
			case "federation_key":
				if req.Public != nil {
					block = req.Public.Text
				}
			}
		}
		if !found || strings.Contains(block, "service-accounts create "+googlecloud.AtlasAccountID) != on {
			t.Errorf("with the reader %v the block holds the atlas's account: %v", on,
				strings.Contains(block, googlecloud.AtlasAccountID))
		}
	}
}

// THE CLOUD ASSET API OFF IN THE IDENTITY PROJECT IS ONE FINDING NAMING THE
// BLOCK, never a refusal of each project, which would send a person to every
// project for what one command in one project fixes.
func TestTheAssetAPIOffIsOneFindingNamingTheBlock(t *testing.T) {
	t.Parallel()
	w := assetWorld(t)
	w.f.assetsOff = true
	read := w.readWith(t, atlasOn)
	if len(read.Unread) != 1 || read.Unread[0].Part != "atlas_reader" ||
		!strings.Contains(read.Unread[0].Detail, "Cloud Asset API") {
		t.Fatalf("unread %+v, want one finding naming the Cloud Asset API and the block", read.Unread)
	}
}
