package scaleway_test

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
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/redact/redacttest"
	"github.com/crewlet/crewlet/internal/scaleway"
)

// atlasNow is the instant every atlas read here is made at.
var atlasNow = time.Date(2026, 11, 13, 7, 0, 0, 0, time.UTC)

// Values the fake carries in fields the read never decodes.
const (
	canaryEmail = "atlas-canary-person@acme.example"
	canaryText  = "atlas-canary-description"
)

// The keys of the fixture's entries.
const (
	orgKey  = "scaleway/" + testOrg
	webKey  = "scaleway/" + projectWeb
	dataKey = "scaleway/" + projectData
)

// atlasFake is the organization as the atlas reads it: the default project,
// which carries the Organization's own ID, beside the fixture's three.
func atlasFake() *fakeScaleway {
	f := newFakeScaleway()
	f.projects = append(f.projects, scaleway.Project{ID: testOrg, Name: "default"})
	f.projectFields = map[string]any{"description": canaryText, "created_by": canaryEmail,
		"organization_id": testOrg}
	return f
}

func atlasCompany() *config.Company {
	c := &config.Company{Name: "Acme"}
	c.Integrations.Scaleway = &config.Scaleway{Enabled: true, Provisioning: &config.ScalewayProvisioning{
		OrganizationID: "${SCW_ORG}", SecretKey: "${SCW_SECRET}"}}
	return c
}

func (f *fakeScaleway) atlasInput(t *testing.T, edit ...func(*config.Company)) collect.Input {
	t.Helper()
	c := atlasCompany()
	for _, e := range edit {
		e(c)
	}
	env := config.NewResolver(config.MapSource{"SCW_ORG": strings.ToUpper(testOrg), "SCW_SECRET": provSecret})
	return collect.Input{Config: c, Now: atlasNow, Env: env, HTTP: f.server(t)}
}

func scalewaySource(t *testing.T) collect.Source {
	t.Helper()
	src, ok := collect.Lookup(integration.KindScaleway)
	if !ok {
		t.Fatal("no Scaleway source is registered")
	}
	return src
}

func (f *fakeScaleway) atlasRead(t *testing.T) collect.Read {
	t.Helper()
	read, err := scalewaySource(t).Collect(t.Context(), f.atlasInput(t))
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

func atlasFact(e atlas.Entry, f atlas.Fact) string {
	for _, fv := range e.Facts {
		if fv.Fact == f {
			return fv.Value.Text
		}
	}
	return ""
}

// THE READ CLAIMS THE ORGANIZATION AND EVERY PROJECT AS AN ACCOUNT, by
// the key the settings give the Organization, with one listing and GET alone.
// The default project carries the Organization's ID, so it is the
// Organization's entry and is described no further. Nothing a row carries
// beside its ID and name reaches the read.
//
// Mutation: decode a project's description or its creator into a claim, and
// a canary reaches the read; name the default project, and the settings'
// name for the Organization is disputed.
func TestTheAtlasReadsTheOrganizationsProjects(t *testing.T) {
	t.Parallel()
	f := atlasFake()
	read := f.atlasRead(t)
	f.onlyScaleway(t)
	if got := f.requests; !slices.Equal(got, []string{"GET /account/v3/projects"}) || read.Calls != 1 {
		t.Fatalf("the read made %v and counted %d calls, want the one listing", got, read.Calls)
	}
	if len(f.wrote()) != 0 {
		t.Fatalf("the read wrote at Scaleway: %v", f.wrote())
	}
	v := atlasView(read.Claims)
	if len(v.Entries) != 4 {
		t.Fatalf("the read holds %d entries, want the Organization and three projects", len(v.Entries))
	}
	org := atlasEntry(t, v, orgKey)
	if len(org.Facts) != 0 || len(org.Relations) != 0 {
		t.Errorf("the Organization, which is also the default project, was described: %+v %+v",
			org.Facts, org.Relations)
	}
	web := atlasEntry(t, v, webKey)
	if web.Name != "web" || atlasFact(web, atlas.FactAccountName) != "web" ||
		atlasFact(web, atlas.FactVendor) != "scaleway" || atlasFact(web, atlas.FactAccountRef) != projectWeb ||
		len(web.Relations) != 0 || len(read.Withheld) != 0 {
		t.Errorf("the web project: %q %+v %+v, withheld %+v", web.Name, web.Facts, web.Relations, read.Withheld)
	}
	// A project nobody named is known by its ID.
	if unnamed := atlasEntry(t, v, "scaleway/"+projectUnnamed); unnamed.Name != projectUnnamed ||
		atlasFact(unnamed, atlas.FactAccountName) != "" {
		t.Errorf("the unnamed project: %q %+v", unnamed.Name, unnamed.Facts)
	}
	out, err := json.Marshal(read)
	if err != nil {
		t.Fatal(err)
	}
	for _, canary := range []string{canaryEmail, canaryText, provSecret, "@"} {
		if strings.Contains(string(out), canary) {
			t.Errorf("%q reached the read", canary)
		}
	}
}

// THE READ MEETS THE SETTINGS ON ONE ENTRY: the Organization is keyed as the
// settings key it, so the account a company connected and the one its
// projects belong to are the same, and the two voices dispute nothing.
//
// Mutation: key the Organization by the ID as it was typed, uppercase
// included, and the projects belong to a second account nobody connected.
func TestTheScalewayReadAndTheSettingsNameOneOrganization(t *testing.T) {
	t.Parallel()
	read := atlasFake().atlasRead(t)
	settings := atlas.SettingsSource("r1")
	ref := atlas.MustRef(atlas.KindAccount, "scaleway/"+config.ScalewayID(strings.ToUpper(testOrg)))
	var claims []atlas.Claim
	exists, err := atlas.Exists(ref, settings, atlasNow)
	if err != nil {
		t.Fatal(err)
	}
	named, err := atlas.Says(ref, atlas.FactName, "Scaleway", settings, atlasNow)
	if err != nil {
		t.Fatal(err)
	}
	claims = append(append(claims, exists, named), read.Claims...)
	v := atlasView(claims)
	org := atlasEntry(t, v, orgKey)
	if org.Name != "Scaleway" || org.Review || len(org.Sources) != 2 {
		t.Errorf("the Organization: %q, review %v, sources %+v", org.Name, org.Review, org.Sources)
	}
	if len(v.Entries) != 4 {
		t.Errorf("%d entries, want the four the read holds", len(v.Entries))
	}
}

// A LISTING SCALEWAY REFUSES THE KEY IS A FINDING AND RETIRES NOTHING: a key
// whose policy lost ProjectReadOnly leaves the projects unread, the standing
// says what to grant in the engine's own words, and no project an earlier
// read recorded is missed or retired, however long the refusal lasts.
//
// Mutation: read the refused listing as an empty one, and every project is
// retired a day after somebody edits the provisioner's policy.
func TestAListingScalewayRefusesIsAFindingAndRetiresNothing(t *testing.T) {
	t.Parallel()
	whole := atlasFake().atlasRead(t)
	f := atlasFake()
	f.refuse["projects"] = http.StatusForbidden
	read := f.atlasRead(t)
	if len(read.Unread) != 1 || read.Unread[0].Part != "projects" ||
		!strings.Contains(read.Unread[0].Detail, "ProjectReadOnly") ||
		strings.Contains(read.Unread[0].Detail, "insufficient permissions") {
		t.Fatalf("the refused listing: %+v", read.Unread)
	}
	if len(read.Claims) != 1 || read.Claims[0].Subject.Key != orgKey {
		t.Fatalf("the refused read claims %+v, want the Organization alone", read.Claims)
	}
	status := collect.Status{Kind: integration.KindScaleway}
	for _, at := range []time.Time{atlasNow.Add(time.Hour), atlasNow.Add(30 * time.Hour), atlasNow.Add(60 * time.Hour)} {
		plan := collect.Decide(whole.Claims, read, status, at)
		if len(plan.Records) != 0 || len(plan.Status.Missing) != 0 || len(plan.Status.Unread) != 1 {
			t.Fatalf("at %s the refusal recorded %+v, missed %+v and found %+v", at, plan.Records,
				plan.Status.Missing, plan.Status.Unread)
		}
		status = plan.Status
	}
	// The same read, were the listing whole and empty, retires every project.
	empty := read
	empty.Unread = nil
	first := collect.Decide(whole.Claims, empty, collect.Status{Kind: integration.KindScaleway}, atlasNow.Add(time.Hour))
	if plan := collect.Decide(whole.Claims, empty, first.Status, atlasNow.Add(26*time.Hour)); plan.Status.Retired != 3 {
		t.Errorf("the control retired %d projects, want 3", plan.Status.Retired)
	}
}

// A PROJECT THAT IS GONE IS RETIRED AFTER TWO COMPLETE READS A DAY APART,
// and an unchanged read records nothing.
//
// Mutation: retire on the first read that misses a project, and one page
// Scaleway served short removes a project from the atlas.
func TestAScalewayProjectTwoReadsMissIsRetired(t *testing.T) {
	t.Parallel()
	whole := atlasFake().atlasRead(t)
	status := collect.Status{Kind: integration.KindScaleway}
	if plan := collect.Decide(whole.Claims, whole, status, atlasNow.Add(time.Hour)); len(plan.Records) != 0 {
		t.Fatalf("an unchanged read recorded %+v", plan.Records)
	}
	f := atlasFake()
	f.projects = slices.DeleteFunc(f.projects, func(p scaleway.Project) bool { return p.ID == projectData })
	read := f.atlasRead(t)
	first := collect.Decide(whole.Claims, read, status, atlasNow.Add(time.Hour))
	if len(first.Records) != 0 || first.Status.Retired != 0 || len(first.Status.Missing) != 1 {
		t.Fatalf("the first miss recorded %+v and retired %d", first.Records, first.Status.Retired)
	}
	second := collect.Decide(whole.Claims, read, first.Status, atlasNow.Add(26*time.Hour))
	if second.Status.Retired != 1 || len(second.Records) != 1 ||
		second.Records[0].Entry != atlas.MustRef(atlas.KindAccount, dataKey) {
		t.Fatalf("the second miss retired %d with %+v", second.Status.Retired, second.Records)
	}
}

// NO CREDENTIAL SHAPE REACHES THE ATLAS FROM SCALEWAY: every shape the
// redaction pass knows, planted in a project's name, in its ID and in the
// fields the read never decodes, leaves no piece of itself in the read, and
// what the mapped fields refused is counted.
//
// Mutation: cut a name before the builder judges it, or skip the builder's
// grammar on a key or a value, and a shape reaches the read.
func TestNoCredentialShapeReachesTheAtlasFromScaleway(t *testing.T) {
	t.Parallel()
	for _, shape := range redacttest.Shapes() {
		s := shape.Sample
		// A name long enough that cutting it to a label's length would cut
		// the shape short of its end, where no rule knows it any more.
		long := strings.Repeat("m", max(1, atlas.MaxLabelRunes-len(s)+len(s)/4)) + " " + s
		f := atlasFake()
		f.projects = []scaleway.Project{
			{ID: projectWeb, Name: s}, {ID: projectData, Name: long}, {ID: s, Name: "billing"},
			{ID: projectUnnamed, Name: "api"},
		}
		f.projectFields = map[string]any{"description": s, "created_by": s, "secret_key": s, "tags": []string{s}}
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
		// Two names, each refused as a name and as an account's name, and
		// one ID are refused where they sat.
		if len(read.Withheld) < 5 {
			t.Errorf("%s: %d values withheld, want every planted mapped field: %+v", shape.Name,
				len(read.Withheld), read.Withheld)
		}
		// What sat beside a credential is still read.
		v := atlasView(read.Claims)
		if api := atlasEntry(t, v, "scaleway/"+projectUnnamed); api.Name != "api" {
			t.Errorf("%s: the project beside the credential was not read: %+v", shape.Name, api)
		}
		// A project whose name was refused is still an entry, by its ID.
		if web := atlasEntry(t, v, webKey); web.Name != webKey {
			t.Errorf("%s: the project whose name was refused is named %q", shape.Name, web.Name)
		}
	}
}

// A LISTING THAT DID NOT FINISH IS NO READ AT ALL: a server fault fails the
// read whole, so nothing is retired on a listing cut short; a key Scaleway
// does not know is a rejected credential, never a missing permission; and a
// short rate limit is waited out inside the call.
//
// Mutation: read a dead key as a refusal, and a rotated key is reported as a
// missing permission set.
func TestAListingThatFailsFailsTheScalewayRead(t *testing.T) {
	t.Parallel()
	for name, fault := range map[string]func(*fakeScaleway){
		"a server fault": func(f *fakeScaleway) { f.refuse["projects"] = http.StatusBadGateway },
		"a dead key":     func(f *fakeScaleway) { delete(f.keys, provAccess) },
	} {
		f := atlasFake()
		fault(f)
		read, err := scalewaySource(t).Collect(t.Context(), f.atlasInput(t))
		if err == nil || errors.Is(err, integration.ErrNotConfigured) || len(read.Claims) != 0 {
			t.Errorf("%s: the read answered %d claims and %v", name, len(read.Claims), err)
		}
		if rejected := name == "a dead key"; errors.Is(err, integration.ErrCredentialRejected) != rejected {
			t.Errorf("%s: a rejected credential is %v, want %v: %v", name, !rejected, rejected, err)
		}
	}
	limited := atlasFake()
	limited.limited["projects"] = 2
	for i := range 150 {
		limited.projects = append(limited.projects, scaleway.Project{
			ID: "cccccccc-0000-4000-8000-" + strconv.Itoa(100000000000+i), Name: "p" + strconv.Itoa(i)})
	}
	read := limited.atlasRead(t)
	// Two waits, then two pages of a hundred: every project past the first
	// page is read, and every request is counted.
	if v := atlasView(read.Claims); len(v.Entries) != 154 || read.Calls != 4 {
		t.Errorf("a limited, paged listing read %d entries in %d calls, want 154 in 4", len(v.Entries), read.Calls)
	}
}

// A COMPANY THAT DOES NOT CONNECT SCALEWAY, OR WHOSE KEY DOES NOT RESOLVE, IS
// NOT CONFIGURED, which the duty reads as nothing to read rather than a fault.
func TestAnUnconnectedScalewayIsNotConfigured(t *testing.T) {
	t.Parallel()
	for name, edit := range map[string]func(*config.Company){
		"no block":     func(c *config.Company) { c.Integrations.Scaleway = nil },
		"switched off": func(c *config.Company) { c.Integrations.Scaleway.Enabled = false },
		"no provision": func(c *config.Company) { c.Integrations.Scaleway.Provisioning = nil },
		"no key":       func(c *config.Company) { c.Integrations.Scaleway.Provisioning.SecretKey = "${SCW_UNSET}" },
	} {
		f := atlasFake()
		if _, err := scalewaySource(t).Collect(t.Context(), f.atlasInput(t, edit)); !errors.Is(err, integration.ErrNotConfigured) {
			t.Errorf("%s: %v, want not configured", name, err)
		}
		if len(f.requests) != 0 {
			t.Errorf("%s: the read called Scaleway: %v", name, f.requests)
		}
	}
	if _, err := scalewaySource(t).Collect(t.Context(), collect.Input{}); !errors.Is(err, integration.ErrNotConfigured) {
		t.Errorf("no document: %v, want not configured", err)
	}
}
