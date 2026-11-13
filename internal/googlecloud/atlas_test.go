package googlecloud_test

import (
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
	"github.com/crewlet/crewlet/internal/googlecloud"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/redact/redacttest"
)

// atlasNow is the instant every atlas read here is made at.
var atlasNow = time.Date(2026, 11, 13, 7, 0, 0, 0, time.UTC)

// Values the fake carries in fields the read never decodes.
const (
	canaryEmail = "atlas-canary-person@acme.example"
	canaryText  = "atlas-canary-label"
)

// The fixture's organization, and the keys of its entries.
const (
	testOrganization = "organizations/123456789012"
	identityKey      = "google_cloud/" + identity
	webKey           = "google_cloud/" + projectWeb
	dataKey          = "google_cloud/" + projectData
	servicesOfWeb    = "GET /v1/projects/" + projectWeb + "/services"
	searchProjects   = "GET /v3/projects:search"
)

// atlasWorld is a company's Google Cloud as the atlas reads it: the web
// project runs compute and a database under the organization, the data
// project sits in a folder, and a project on its way out is not active.
type atlasWorld struct {
	f   *fakeGoogle
	key string
}

func newAtlasWorld(t *testing.T) *atlasWorld {
	t.Helper()
	key, pemText := fakeKey(t)
	// THE WALL CLOCK, since the source builds its own client: a token the
	// fake issues is then live for the hour the client takes it to be.
	f := newFakeGoogle(t, &key.PublicKey, &testClock{t: time.Now().UTC()})
	f.projects[projectWeb].Parent = testOrganization
	f.projects[projectWeb].Services = services("compute", "sqladmin", "run", "storage", "bigquery", "unheardof")
	f.projects[projectData].Parent = "folders/77"
	f.projects[projectData].Services = services("aiplatform", "dns")
	f.projects["acme-leaving"] = &fakeProject{ID: "acme-leaving", Name: "Leaving", State: "DELETE_REQUESTED",
		Parent: testOrganization}
	f.unmapped = map[string]any{"labels": map[string]string{"owner": canaryEmail, "note": canaryText},
		"title": canaryText, "createTime": "2026-01-01T00:00:00Z", "etag": canaryText}
	return &atlasWorld{f: f, key: pemText}
}

func (w *atlasWorld) input(t *testing.T, edit ...func(*config.Company)) collect.Input {
	t.Helper()
	c := &config.Company{Name: testCompany}
	c.Integrations.GoogleCloud = &config.GoogleCloud{Enabled: true, Provisioning: &config.GoogleCloudProvisioning{
		Provisioner: provisioner, Provider: "${GCP_PROVIDER}", FederationKey: "${GCP_KEY}",
	}}
	for _, e := range edit {
		e(c)
	}
	env := config.NewResolver(config.MapSource{"GCP_PROVIDER": testProvider, "GCP_KEY": w.key})
	return collect.Input{Config: c, Now: atlasNow, Env: env, HTTP: w.f.server(t)}
}

func googleSource(t *testing.T) collect.Source {
	t.Helper()
	src, ok := collect.Lookup(integration.KindGoogleCloud)
	if !ok {
		t.Fatal("no Google Cloud source is registered")
	}
	return src
}

func (w *atlasWorld) read(t *testing.T) collect.Read {
	t.Helper()
	read, err := googleSource(t).Collect(t.Context(), w.input(t))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	return read
}

// sent is how many times the fake served call.
func (w *atlasWorld) sent(call string) int {
	n := 0
	w.f.edit(func(f *fakeGoogle) {
		for _, c := range f.requests {
			if c == call {
				n++
			}
		}
	})
	return n
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

// THE READ CLAIMS EVERY ACTIVE PROJECT AS AN ACCOUNT, AND WHAT EACH PROJECT'S
// ENABLED SERVICES SAY IT IS USED FOR, as closed words: an API a new project already has enabled, and
// one this build does not know, say nothing. It is made as the provisioner,
// with GET alone beside the two calls that mint its token. Nothing a row
// carries beside the mapped fields reaches the read: no label, no title.
//
// Mutation: decode a project's labels or a service's title into a claim, and
// a canary reaches the read; claim a project that is being deleted, and it
// stays in the atlas for the thirty days Google keeps it.
func TestTheAtlasReadsProjectsAndWhatTheirServicesSay(t *testing.T) {
	t.Parallel()
	w := newAtlasWorld(t)
	read := w.read(t)
	w.f.onlyGoogle(t)
	if wrote := w.f.wrote(); len(wrote) != 0 {
		t.Fatalf("the read wrote at Google: %v", wrote)
	}
	// One token, one listing, and one listing of services for each of the
	// five active projects.
	if read.Calls != 8 || w.sent(searchProjects) != 1 || w.sent(servicesOfWeb) != 1 {
		t.Errorf("the read counted %d calls: %v", read.Calls, w.f.requests)
	}
	for _, call := range w.f.requests {
		if method, path, _ := strings.Cut(call, " "); method != http.MethodGet && path != "/v1/token" &&
			!strings.HasSuffix(path, ":generateAccessToken") {
			t.Errorf("the read sent %s", call)
		}
	}
	v := atlasView(read.Claims)
	if len(v.Entries) != 5 || len(read.Withheld) != 0 {
		t.Fatalf("the read holds %d entries and withheld %+v, want the five active projects", len(v.Entries),
			read.Withheld)
	}
	web := atlasEntry(t, v, webKey)
	if web.Name != "Acme Web" || !slices.Equal(atlasFacts(web, atlas.FactAccountName), []string{"Acme Web"}) ||
		!slices.Equal(atlasFacts(web, atlas.FactVendor), []string{"google_cloud"}) ||
		!slices.Equal(atlasFacts(web, atlas.FactAccountRef), []string{projectWeb}) ||
		!slices.Equal(atlasFacts(web, atlas.FactCapability), []string{"compute", "database"}) ||
		len(web.Relations) != 0 {
		t.Errorf("the web project: %q %+v %+v", web.Name, web.Facts, web.Relations)
	}
	if data := atlasEntry(t, v, dataKey); !slices.Equal(atlasFacts(data, atlas.FactCapability),
		[]string{"dns", "model_api"}) {
		t.Errorf("the data project: %+v", data.Facts)
	}
	if _, ok := v.Find(atlas.MustRef(atlas.KindAccount, "google_cloud/acme-leaving").ID()); ok {
		t.Error("a project that is being deleted was claimed")
	}
	out, err := json.Marshal(read)
	if err != nil {
		t.Fatal(err)
	}
	for _, canary := range []string{canaryEmail, canaryText, "ya29.", "googleapis", "organizations/", "@"} {
		if strings.Contains(string(out), canary) {
			t.Errorf("%q reached the read", canary)
		}
	}
}

// THE READ MEETS THE SETTINGS ON THE IDENTITY PROJECT: it is keyed as the
// settings key it and is named by them alone, so the account a company
// connected is one entry and the two voices dispute nothing.
//
// Mutation: name the identity project from Google's display name, and every
// company's Google Cloud account sits in Review for a name nobody chose.
func TestTheGoogleCloudReadAndTheSettingsNameOneIdentityProject(t *testing.T) {
	t.Parallel()
	read := newAtlasWorld(t).read(t)
	settings := atlas.SettingsSource("r1")
	ref := atlas.MustRef(atlas.KindAccount, identityKey)
	exists, err := atlas.Exists(ref, settings, atlasNow)
	if err != nil {
		t.Fatal(err)
	}
	named, err := atlas.Says(ref, atlas.FactName, "Google Cloud", settings, atlasNow)
	if err != nil {
		t.Fatal(err)
	}
	v := atlasView(append([]atlas.Claim{exists, named}, read.Claims...))
	account := atlasEntry(t, v, identityKey)
	if account.Name != "Google Cloud" || account.Review || len(account.Sources) != 2 ||
		!slices.Equal(atlasFacts(account, atlas.FactAccountName), []string{"Acme Crewlet identities"}) {
		t.Errorf("the identity project: %q, review %v, %+v", account.Name, account.Review, account.Facts)
	}
	if len(v.Entries) != 5 {
		t.Errorf("%d entries, want the five the read holds", len(v.Entries))
	}
}

// A PROJECT WHOSE SERVICES GOOGLE REFUSES IS A FINDING AND WITHDRAWS NOTHING:
// the project is still claimed, the standing says what to run in the engine's
// own words, and what an earlier read said the project is used for stands,
// however long the refusal lasts.
//
// Mutation: read a refused listing as an empty one, and a project loses what
// it is used for the hour somebody removes Service Usage Viewer.
func TestAProjectsServicesGoogleRefusesAreAFindingAndWithdrawNothing(t *testing.T) {
	t.Parallel()
	whole := newAtlasWorld(t).read(t)
	w := newAtlasWorld(t)
	w.f.refuse[servicesOfWeb] = http.StatusForbidden
	w.f.refuse["GET /v1/projects/"+projectData+"/services"] = http.StatusNotFound
	read := w.read(t)
	if len(read.Unread) != 1 || read.Unread[0].Part != "services" ||
		!strings.HasPrefix(read.Unread[0].Detail, "The enabled services of 2 projects") ||
		!strings.Contains(read.Unread[0].Detail, "Service Usage Viewer") ||
		strings.Contains(read.Unread[0].Detail, "refused by the test") {
		t.Fatalf("the refused part: %+v", read.Unread)
	}
	v := atlasView(read.Claims)
	if web := atlasEntry(t, v, webKey); web.Name != "Acme Web" || len(atlasFacts(web, atlas.FactCapability)) != 0 {
		t.Errorf("the project beside its refused services: %q %+v", web.Name, web.Facts)
	}
	status := collect.Status{Kind: integration.KindGoogleCloud}
	for _, at := range []time.Time{atlasNow.Add(time.Hour), atlasNow.Add(30 * time.Hour), atlasNow.Add(60 * time.Hour)} {
		plan := collect.Decide(whole.Claims, read, status, at)
		if len(plan.Records) != 0 || len(plan.Status.Missing) != 0 || len(plan.Status.Unread) != 1 {
			t.Fatalf("at %s the refusal recorded %+v, missed %+v and found %+v", at, plan.Records,
				plan.Status.Missing, plan.Status.Unread)
		}
		status = plan.Status
	}
	// The same read, were the listings whole and empty, withdraws what the
	// two projects are used for.
	empty := read
	empty.Unread = nil
	plan := collect.Decide(whole.Claims, empty, collect.Status{Kind: integration.KindGoogleCloud}, atlasNow.Add(time.Hour))
	withdrawn := 0
	for _, r := range plan.Records {
		withdrawn += len(r.Withdrawn)
	}
	if len(plan.Records) != 2 || withdrawn != 4 {
		t.Errorf("the control withdrew %d capabilities of %d projects, want 4 of 2", withdrawn, len(plan.Records))
	}
}

// A PROJECT THAT IS GONE IS RETIRED AFTER TWO COMPLETE READS A DAY APART, an
// unchanged read records nothing, and a service somebody switches off is
// withdrawn at the next read.
//
// Mutation: retire on the first read that misses a project, and one page
// Google served short removes a project from the atlas.
func TestAGoogleCloudProjectTwoReadsMissIsRetired(t *testing.T) {
	t.Parallel()
	whole := newAtlasWorld(t).read(t)
	status := collect.Status{Kind: integration.KindGoogleCloud}
	if plan := collect.Decide(whole.Claims, whole, status, atlasNow.Add(time.Hour)); len(plan.Records) != 0 {
		t.Fatalf("an unchanged read recorded %+v", plan.Records)
	}
	w := newAtlasWorld(t)
	w.f.projects[projectData].State = "DELETE_REQUESTED"
	delete(w.f.projects[projectWeb].Services, "sqladmin.googleapis.com")
	read := w.read(t)
	first := collect.Decide(whole.Claims, read, status, atlasNow.Add(time.Hour))
	if len(first.Records) != 1 || first.Records[0].Entry.Key != webKey || len(first.Records[0].Withdrawn) != 1 ||
		first.Status.Retired != 0 || len(first.Status.Missing) != 1 {
		t.Fatalf("the first read recorded %+v and retired %d", first.Records, first.Status.Retired)
	}
	second := collect.Decide(whole.Claims, read, first.Status, atlasNow.Add(26*time.Hour))
	retired := slices.IndexFunc(second.Records, func(r collect.EntryRead) bool { return r.Retires })
	if second.Status.Retired != 1 || retired < 0 || second.Records[retired].Entry.Key != dataKey {
		t.Fatalf("the second miss retired %d with %+v", second.Status.Retired, second.Records)
	}
}

// NO CREDENTIAL SHAPE REACHES THE ATLAS FROM GOOGLE CLOUD: every shape the
// redaction pass knows, planted in a project's ID, its display name, a
// service's name and the fields the read never decodes, leaves no
// piece of itself in the read, and what the mapped fields refused is counted.
//
// Mutation: say a service's name as it came, cut a name before the builder
// judges it, or skip the builder's grammar on a key, and a shape reaches the
// read.
func TestNoCredentialShapeReachesTheAtlasFromGoogleCloud(t *testing.T) {
	t.Parallel()
	for _, shape := range redacttest.Shapes() {
		s := shape.Sample
		// A name long enough that cutting it to a label's length would cut
		// the shape short of its end, where no rule knows it any more.
		long := strings.Repeat("m", max(1, atlas.MaxLabelRunes-len(s)+len(s)/4)) + " " + s
		w := newAtlasWorld(t)
		w.f.projects[projectWeb].Name = s
		w.f.projects[projectWeb].Services = map[string]bool{s: true, s + ".googleapis.com": true,
			"compute.googleapis.com": true}
		w.f.projects[projectData].Name = long
		w.f.projects[projectData].Parent = "organizations/" + s
		w.f.projects[projectHalf].Parent = s
		w.f.projects[s] = &fakeProject{ID: s, Name: "Billing", State: "ACTIVE", Parent: testOrganization}
		w.f.unmapped = map[string]any{"labels": map[string]string{"token": s}, "title": s, "etag": s,
			"name": "projects/" + s, "tags": map[string]string{s: s}}
		read := w.read(t)
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
		// Two display names, each refused as a name and as an account's
		// name, and one project's ID are refused where they sat.
		if len(read.Withheld) < 5 {
			t.Errorf("%s: %d values withheld, want every planted mapped field: %+v", shape.Name,
				len(read.Withheld), read.Withheld)
		}
		// What sat beside a credential is still read.
		v := atlasView(read.Claims)
		if web := atlasEntry(t, v, webKey); !slices.Equal(atlasFacts(web, atlas.FactCapability), []string{"compute"}) ||
			web.Name != webKey {
			t.Errorf("%s: the project beside the credential: %q %+v", shape.Name, web.Name, web.Facts)
		}
	}
}

// A LISTING THAT DID NOT FINISH IS NO READ AT ALL: a listing that fails for
// any reason but a refusal of one project's services, Google's rate limit
// past what a call waits and a provisioner Google stops trusting included,
// fails the whole read, so nothing is withdrawn or retired on a listing cut
// short. A provisioner Google refuses the projects is a rejected credential,
// never a missing role.
//
// Mutation: read a rate-limited listing as a refusal, and a busy minute is
// recorded as a missing role.
func TestAListingThatFailsOrIsRateLimitedFailsTheGoogleCloudRead(t *testing.T) {
	t.Parallel()
	for name, fault := range map[string]func(*fakeGoogle){
		"a server fault": func(f *fakeGoogle) { f.refuse[servicesOfWeb] = http.StatusBadGateway },
		// A wait longer than one call takes, so the limit is handed back.
		"the rate limit": func(f *fakeGoogle) {
			f.limited, f.retryAfter = map[string]int{servicesOfWeb: 1}, 60
		},
		"the projects":       func(f *fakeGoogle) { f.refuse[searchProjects] = http.StatusForbidden },
		"a refused exchange": func(f *fakeGoogle) { f.refuse["POST /v1/token"] = http.StatusBadRequest },
	} {
		w := newAtlasWorld(t)
		fault(w.f)
		read, err := googleSource(t).Collect(t.Context(), w.input(t))
		if err == nil || errors.Is(err, integration.ErrNotConfigured) || len(read.Claims) != 0 {
			t.Errorf("%s: the read answered %d claims and %v", name, len(read.Claims), err)
		}
		rejected := name == "the projects" || name == "a refused exchange"
		if errors.Is(err, integration.ErrCredentialRejected) != rejected {
			t.Errorf("%s: a rejected credential is %v, want %v: %v", name, !rejected, rejected, err)
		}
		if name == "the rate limit" && (googlecloud.Status(err) != http.StatusTooManyRequests ||
			!strings.Contains(err.Error(), "asked to wait 1m0s")) {
			t.Errorf("the limit handed back does not say what Google asked: %v", err)
		}
	}
}

// THE PROJECTS ARE WALKED TO THEIR BOUNDS: a listing past its first page is
// read to its end, and a company past the ceiling is a finding that reads and
// retires nothing.
//
// Mutation: read the first thousand projects of a larger company, and the
// rest are retired a day later.
func TestTheGoogleCloudProjectsAreWalkedToTheirBounds(t *testing.T) {
	t.Parallel()
	w := newAtlasWorld(t)
	w.f.page = 2
	read := w.read(t)
	if v := atlasView(read.Claims); len(v.Entries) != 5 || w.sent(searchProjects) != 3 {
		t.Errorf("a paged listing read %d entries in %d pages, want 5 in 3", len(atlasView(read.Claims).Entries),
			w.sent(searchProjects))
	}
	whole := newAtlasWorld(t).read(t)
	large := newAtlasWorld(t)
	for i := range 1000 {
		id := "acme-p" + strconv.Itoa(i)
		large.f.projects[id] = &fakeProject{ID: id, Name: id, State: "ACTIVE"}
	}
	past := large.read(t)
	if len(past.Unread) != 1 || past.Unread[0].Part != "projects" || len(past.Claims) != 0 ||
		!strings.Contains(past.Unread[0].Detail, "1000") {
		t.Fatalf("a company past the ceiling: %d claims, %+v", len(past.Claims), past.Unread)
	}
	if plan := collect.Decide(whole.Claims, past, collect.Status{Kind: integration.KindGoogleCloud},
		atlasNow.Add(48*time.Hour)); len(plan.Records) != 0 || len(plan.Status.Missing) != 0 {
		t.Errorf("a company past the ceiling recorded %+v and missed %+v", plan.Records, plan.Status.Missing)
	}
}

// A COMPANY THAT DOES NOT CONNECT GOOGLE CLOUD, OR WHOSE PROVIDER OR KEY DOES
// NOT RESOLVE, IS NOT CONFIGURED, which the duty reads as nothing to read
// rather than a fault.
func TestAnUnconnectedGoogleCloudIsNotConfigured(t *testing.T) {
	t.Parallel()
	for name, edit := range map[string]func(*config.Company){
		"no block":     func(c *config.Company) { c.Integrations.GoogleCloud = nil },
		"switched off": func(c *config.Company) { c.Integrations.GoogleCloud.Enabled = false },
		"no provision": func(c *config.Company) { c.Integrations.GoogleCloud.Provisioning = nil },
		"no provider":  func(c *config.Company) { c.Integrations.GoogleCloud.Provisioning.Provider = "" },
		"no key": func(c *config.Company) {
			c.Integrations.GoogleCloud.Provisioning.FederationKey = "${GCP_UNSET}"
		},
	} {
		w := newAtlasWorld(t)
		if _, err := googleSource(t).Collect(t.Context(), w.input(t, edit)); !errors.Is(err, integration.ErrNotConfigured) {
			t.Errorf("%s: %v, want not configured", name, err)
		}
		if len(w.f.requests) != 0 {
			t.Errorf("%s: the read called Google: %v", name, w.f.requests)
		}
	}
	if _, err := googleSource(t).Collect(t.Context(), collect.Input{}); !errors.Is(err, integration.ErrNotConfigured) {
		t.Errorf("no document: %v, want not configured", err)
	}
}
