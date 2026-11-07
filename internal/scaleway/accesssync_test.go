package scaleway_test

import (
	"context"
	"errors"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/scaleway"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/seataccess/seataccesstest"
)

// fakeVendor is the registered Scaleway vendor, opened against the fake with
// the world's clock.
type fakeVendor struct {
	seataccess.Vendor
	hc  *http.Client
	now func() time.Time
}

func (v fakeVendor) Open(ctx context.Context, in seataccess.OpenInput) (seataccess.Session, error) {
	return scaleway.OpenAccess(ctx, v.Vendor, in, v.hc, v.now)
}

// sinkSource resolves the sealed store's values, as the engine's resolver does.
type sinkSource struct{ s *seataccesstest.Sink }

func (s sinkSource) Lookup(name string) (string, bool) { return s.s.Held(name) }

// syncWorld is one company, its Scaleway organization, the ledger and the
// sealed store, run through the shared sync exactly as the loop runs it.
type syncWorld struct {
	t       *testing.T
	f       *fakeScaleway
	vendor  seataccess.Vendor
	company *config.Company
	ledger  *seataccesstest.Ledger
	sink    *seataccesstest.Sink
	clock   time.Time
	drift   []seataccess.DriftRestored
}

var (
	sealedSecret = seataccess.SinkName(integration.KindScaleway, scaleway.SecretKeyField, "sre")
	sealedAccess = seataccess.SinkName(integration.KindScaleway, scaleway.AccessKeyField, "sre")
)

func newSyncWorld(t *testing.T, level string, projects ...string) *syncWorld {
	t.Helper()
	f := newFakeScaleway()
	registered, ok := seataccess.Lookup(integration.KindScaleway)
	if !ok {
		t.Fatal("no Scaleway vendor is registered")
	}
	on := true
	company := &config.Company{Name: "Acme"}
	company.Integrations.Scaleway = &config.Scaleway{Enabled: true, Provisioning: &config.ScalewayProvisioning{
		OrganizationID: "${SCW_ORG}", AccessKey: provAccess, SecretKey: "${SCW_SECRET}",
	}}
	company.Roles = []config.Role{{
		Name: "SRE", Handle: "sre",
		Integrations: config.RoleIntegrations{Scaleway: &config.RoleScaleway{
			Enabled: &on, Level: level, Projects: projects,
		}},
	}}
	w := &syncWorld{
		t: t, f: f, company: company, clock: f.now,
		ledger: seataccesstest.NewLedger(), sink: seataccesstest.NewSink(),
	}
	w.vendor = fakeVendor{Vendor: registered, hc: f.server(t), now: func() time.Time { return w.clock }}
	return w
}

func (w *syncWorld) seat() *config.RoleScaleway { return w.company.Roles[0].Integrations.Scaleway }

// advance moves the world's clock, and the fake's with it.
func (w *syncWorld) advance(d time.Duration) {
	w.clock = w.clock.Add(d)
	now := w.clock
	w.f.edit(func(f *fakeScaleway) { f.now = now })
}

func (w *syncWorld) open() seataccess.OpenInput {
	return seataccess.OpenInput{Config: w.company, Env: config.NewResolver(
		config.MapSource{"SCW_ORG": testOrg, "SCW_SECRET": provSecret}, sinkSource{w.sink})}
}

func (w *syncWorld) syncWith(asks map[string]seataccess.Ask, disconnect bool) seataccess.SyncReport {
	w.t.Helper()
	report, err := seataccess.Sync(w.t.Context(), seataccess.SyncInput{
		Vendor: w.vendor, Ledger: w.ledger, Sink: w.sink, Pass: "test", By: "test",
		Asks: asks, Disconnect: disconnect, Now: func() time.Time { return w.clock },
		Open:    w.open(),
		OnDrift: func(_ context.Context, d seataccess.DriftRestored) { w.drift = append(w.drift, d) },
	})
	if err != nil {
		w.t.Fatalf("sync: %v", err)
	}
	return report
}

func (w *syncWorld) sync() seataccess.SyncReport { return w.syncWith(nil, false) }

func (w *syncWorld) record() seataccess.Record {
	w.t.Helper()
	rec, ok := w.ledger.Record(integration.KindScaleway, "sre")
	if !ok {
		w.t.Fatal("the ledger has no record for sre")
	}
	return rec
}

// policy is the one policy the fake holds on the application, failing the
// test where it holds another number.
func (w *syncWorld) policy(appID string) fakePolicy {
	w.t.Helper()
	held := w.f.policiesOf(appID)
	if len(held) != 1 {
		w.t.Fatalf("the application holds %d policies, want 1: %+v", len(held), held)
	}
	return held[0]
}

// A SEAT'S WHOLE LIFE THROUGH THE SHARED SYNC: on makes its application with
// one policy at its level on its projects and mints its key, sealing the
// secret and the access key under the engine's names; a converged pass writes
// nothing; a widening at Scaleway is narrowed back and reported; a level and
// a project change rewrite the policy in place and move the key's default
// project; a key near its expiry is replaced; off deletes the keys, the policy
// and the application and clears what was sealed; on again makes new ones.
func TestAScalewaySeatThroughTheSync(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, "", projectWeb)

	if report := w.sync(); len(report.Findings) != 0 {
		t.Fatalf("turning on reported %+v", report.Findings)
	}
	rec := w.record()
	if !rec.Held() || rec.Credential == nil || rec.Credential.Variable != sealedSecret ||
		!slices.Equal(rec.Credential.Others, []string{sealedAccess}) {
		t.Fatalf("record = %+v", rec)
	}
	app, ok := w.f.appNamed("crewlet-acme-sre")
	if !ok || rec.Account.ID != app.ID || app.Org != testOrg {
		t.Fatalf("application = %+v, want crewlet-acme-sre in the organization, as the ledger records", app)
	}
	pol := w.policy(app.ID)
	if level, projects := scaleway.LevelOf(pol.Rules); pol.Name != app.Name || level != config.ScalewayLevelRead ||
		!slices.Equal(projects, []string{projectWeb}) {
		t.Fatalf("policy %q holds %q on %v, want read on the web project under the application's name",
			pol.Name, level, projects)
	}
	keys := w.f.keysOf(app.ID)
	if len(keys) != 1 || keys[0].Description != scaleway.KeyDescription("sre") || keys[0].DefaultProject != projectWeb {
		t.Fatalf("keys = %+v, want one engine key in the web project", keys)
	}
	if want := w.clock.Add(scaleway.KeyLifetime).Truncate(time.Second); !keys[0].ExpiresAt.Equal(want) {
		t.Errorf("the key expires %s, want %s", keys[0].ExpiresAt, want)
	}
	if secret, _ := w.sink.Held(sealedSecret); secret != keys[0].Secret {
		t.Error("the sealed secret is not the key's")
	}
	if access, _ := w.sink.Held(sealedAccess); access != keys[0].Access {
		t.Error("the sealed access key is not the key's")
	}

	writes := len(w.f.wrote())
	if report := w.sync(); len(report.Findings) != 0 {
		t.Errorf("a converged pass reported %+v", report.Findings)
	}
	if got := w.f.wrote(); len(got) != writes {
		t.Fatalf("a converged pass wrote %q", got[writes:])
	}

	// DRIFT: somebody widens the policy at Scaleway to Administer.
	wide, _ := scaleway.RulesFor(config.ScalewayLevelAdminister, testOrg, []string{projectWeb})
	w.f.edit(func(f *fakeScaleway) { f.policies[pol.ID].Rules = rulesOf(wide) })
	w.sync()
	if level, _ := scaleway.LevelOf(w.policy(app.ID).Rules); level != config.ScalewayLevelRead {
		t.Errorf("after drift the policy holds %q, want read restored", level)
	}
	if len(w.drift) != 1 || w.drift[0].After.Level != config.ScalewayLevelRead {
		t.Errorf("drift events = %+v, want one restoring read", w.drift)
	}

	// AN EDIT: the document moves the seat to operate, which is no drift,
	// and in place: the same policy.
	w.seat().Level = config.ScalewayLevelOperate
	w.sync()
	if now := w.policy(app.ID); now.ID != pol.ID {
		t.Errorf("a level change replaced the policy %s with %s", pol.ID, now.ID)
	} else if level, _ := scaleway.LevelOf(now.Rules); level != config.ScalewayLevelOperate {
		t.Errorf("after the edit the policy holds %q, want operate", level)
	}
	if len(w.drift) != 1 {
		t.Errorf("an edit was reported as drift: %+v", w.drift)
	}

	// A PROJECT CHANGE moves the policy and the key's default project.
	w.seat().Projects = []string{projectData}
	w.sync()
	if _, projects := scaleway.LevelOf(w.policy(app.ID).Rules); !slices.Equal(projects, []string{projectData}) {
		t.Errorf("after the project change the policy names %v", projects)
	}
	if keys := w.f.keysOf(app.ID); len(keys) != 1 || keys[0].DefaultProject != projectData {
		t.Errorf("after the project change the keys are %+v, want the data project as default", keys)
	}

	// RENEWAL: 61 days on, fewer than 30 remain. A new key, sealed, and the
	// old one deleted, since a key's expiry cannot change.
	old := w.f.keysOf(app.ID)[0]
	w.advance(61 * 24 * time.Hour)
	if report := w.sync(); len(report.Findings) != 0 {
		t.Fatalf("the renewal reported %+v", report.Findings)
	}
	keys = w.f.keysOf(app.ID)
	if len(keys) != 1 || keys[0].Access == old.Access {
		t.Fatalf("after renewal the keys are %+v, want one new key", keys)
	}
	if access, _ := w.sink.Held(sealedAccess); access != keys[0].Access {
		t.Error("the renewal sealed another key's access key")
	}
	if secret, _ := w.sink.Held(sealedSecret); secret != keys[0].Secret {
		t.Error("the renewal sealed another key's secret")
	}

	// A ROTATION A PERSON ASKED FOR mints anew and deletes the old.
	before := keys[0]
	w.syncWith(map[string]seataccess.Ask{"sre": {Rotate: true}}, false)
	if keys := w.f.keysOf(app.ID); len(keys) != 1 || keys[0].Access == before.Access {
		t.Errorf("after a rotation the keys are %+v", keys)
	}

	// OFF deletes the keys first, so the agent stops at once, then the policy
	// and the application, and clears what was sealed.
	off := false
	w.seat().Enabled = &off
	writes = len(w.f.wrote())
	w.sync()
	got := w.f.wrote()[writes:]
	if len(got) != 3 || !strings.HasPrefix(got[0], "delete key ") || !strings.HasPrefix(got[1], "delete policy ") ||
		got[2] != "delete app "+app.ID {
		t.Errorf("turning off wrote %q, want the key, the policy and the application deleted in that order", got)
	}
	if _, ok := w.f.appNamed("crewlet-acme-sre"); ok {
		t.Error("turning the agent off left its application at Scaleway")
	}
	for _, name := range []string{sealedSecret, sealedAccess} {
		if _, ok := w.sink.Held(name); ok {
			t.Errorf("turning the agent off left %s sealed", name)
		}
	}
	if w.record().Held() {
		t.Error("the ledger still holds a deleted application")
	}

	// BACK ON makes a new application.
	on := true
	w.seat().Enabled = &on
	w.sync()
	if again := w.record(); !again.Held() || again.Account.ID == app.ID {
		t.Fatalf("back on = %+v, want a new application", again.Account)
	}
	if _, ok := w.sink.Held(sealedSecret); !ok {
		t.Error("back on sealed nothing")
	}
	w.f.onlyScaleway(t)
}

// WHAT SOMEBODY ELSE PUT ON AN AGENT'S APPLICATION IS NEVER TOUCHED BY A GRANT
// OR A RENEWAL: a policy under another name and a key without the engine's
// description stay, and the engine's own policy is still the one it rewrites.
// Mutation: retire every key but the new one, and the person's key goes.
func TestAStrangersPolicyAndKeyAreLeftAlone(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.ScalewayLevelRead, projectWeb)
	w.sync()
	appID := w.record().Account.ID
	org := testOrg
	w.f.edit(func(f *fakeScaleway) {
		f.policies["pol-stranger"] = &fakePolicy{ID: "pol-stranger", Name: "billing-for-sre", AppID: appID,
			CreatedAt: f.now, Rules: []scaleway.Rule{{PermissionSetNames: []string{"BillingManager"}, OrganizationID: &org}}}
		f.keys["SCWPERSONSKEY0000001"] = &fakeKey{Access: "SCWPERSONSKEY0000001", Secret: "00000000-0000-4000-8000-0000000000aa",
			AppID: appID, Description: "debugging", CreatedAt: f.now, ExpiresAt: f.now.Add(24 * time.Hour)}
	})
	w.seat().Level = config.ScalewayLevelOperate
	w.advance(61 * 24 * time.Hour)
	w.f.edit(func(f *fakeScaleway) { f.keys["SCWPERSONSKEY0000001"].ExpiresAt = f.now.Add(24 * time.Hour) })
	if report := w.sync(); len(report.Findings) != 0 {
		t.Fatalf("the pass reported %+v", report.Findings)
	}
	held := w.f.policiesOf(appID)
	if len(held) != 2 {
		t.Fatalf("the application holds %+v, want the engine's policy and the stranger's", held)
	}
	for _, p := range held {
		level, _ := scaleway.LevelOf(p.Rules)
		switch p.Name {
		case "crewlet-acme-sre":
			if level != config.ScalewayLevelOperate {
				t.Errorf("the engine's policy holds %q, want operate", level)
			}
		case "billing-for-sre":
			if !slices.Equal(p.Rules[0].PermissionSetNames, []string{"BillingManager"}) {
				t.Errorf("the stranger's policy was rewritten to %+v", p.Rules)
			}
		}
	}
	var engine, person int
	for _, k := range w.f.keysOf(appID) {
		if k.Access == "SCWPERSONSKEY0000001" {
			person++
		} else {
			engine++
		}
	}
	if person != 1 || engine != 1 {
		t.Errorf("after the renewal the application holds %d of the person's keys and %d engine keys, want 1 and 1",
			person, engine)
	}
}

// A POLICY DELETED AT SCALEWAY IS A PERSON TURNING THE AGENT OFF THERE (D3):
// no pass writes it back unasked, and Turn back on writes it again and keeps
// the key the seat still holds.
func TestAPolicyDeletedAtScalewayWaitsForAPerson(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.ScalewayLevelRead, projectWeb)
	w.sync()
	appID := w.record().Account.ID
	pol := w.policy(appID)
	w.f.edit(func(f *fakeScaleway) { delete(f.policies, pol.ID) })
	writes := len(w.f.wrote())
	report := w.sync()
	if len(report.Findings) != 1 || report.Findings[0].Kind != integration.FindingIdentityFailed {
		t.Fatalf("findings = %+v, want the seat waiting on a person", report.Findings)
	}
	if got := w.f.wrote(); len(got) != writes {
		t.Fatalf("a policy deleted by hand was written back unasked: %q", got[writes:])
	}
	secret, _ := w.sink.Held(sealedSecret)
	w.syncWith(map[string]seataccess.Ask{"sre": {Reenable: true}}, false)
	if level, _ := scaleway.LevelOf(w.policy(appID).Rules); level != config.ScalewayLevelRead {
		t.Errorf("after Turn back on the policy holds %q", level)
	}
	if after, _ := w.sink.Held(sealedSecret); after != secret {
		t.Error("turning the agent back on replaced a key it still held")
	}
}

// A POLICY SCALEWAY REFUSES TAKES ITS APPLICATION BACK, so no application is
// left holding nothing for the ledger never to record.
func TestARefusedPolicyTakesItsApplicationBack(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.ScalewayLevelRead, projectWeb)
	w.f.refuse["create policy"] = http.StatusForbidden
	report := w.sync()
	if len(report.Findings) != 1 || report.Findings[0].Subject != "sre" ||
		!strings.Contains(report.Findings[0].Remedy, "IAMPolicyManager") {
		t.Fatalf("findings = %+v, want the seat refused naming what the provisioner needs", report.Findings)
	}
	if _, ok := w.f.appNamed("crewlet-acme-sre"); ok {
		t.Error("a refused policy left its application at Scaleway")
	}
	if rec, ok := w.ledger.Record(integration.KindScaleway, "sre"); ok && rec.Held() {
		t.Errorf("the ledger holds %+v after a refused create", rec.Account)
	}
}

// AN ORGANIZATION CAPPING KEY LIFETIMES BELOW 90 DAYS is the seat's finding
// naming the cap and where it is set, and the next pass mints once it is
// raised. Nothing is minted shorter.
func TestAKeyLifetimeCapIsTheSeatsFinding(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.ScalewayLevelRead, projectWeb)
	w.f.maxLifetime = 30 * 24 * time.Hour
	report := w.sync()
	if len(report.Findings) != 1 {
		t.Fatalf("findings = %+v", report.Findings)
	}
	f := report.Findings[0]
	if f.ActionURL != scaleway.SecurityURL || !strings.Contains(f.Remedy, "90 days") {
		t.Errorf("finding = %+v, want the cap named with the security settings", f)
	}
	if _, ok := w.sink.Held(sealedSecret); ok {
		t.Error("a refused key sealed a value")
	}
	w.f.edit(func(f *fakeScaleway) { f.maxLifetime = 0 })
	if report := w.sync(); len(report.Findings) != 0 {
		t.Fatalf("after the cap was raised the pass reported %+v", report.Findings)
	}
	if _, ok := w.sink.Held(sealedSecret); !ok {
		t.Error("after the cap was raised nothing was sealed")
	}
}

// A FULL QUOTA IS THE SEAT'S FINDING, pointing at the quotas page.
func TestAFullQuotaIsTheSeatsFinding(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.ScalewayLevelRead, projectWeb)
	w.f.quotaApps = 1
	report := w.sync()
	if len(report.Findings) != 1 || report.Findings[0].ActionURL != scaleway.QuotasURL ||
		!strings.Contains(report.Findings[0].Remedy, "quota") {
		t.Fatalf("findings = %+v, want the quota named", report.Findings)
	}
}

// A KEY THAT CANNOT BE SEALED IS DELETED, so no live key is held by nobody,
// and the next pass mints a fresh one.
func TestAnUnsealableKeyIsDeleted(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.ScalewayLevelRead, projectWeb)
	w.sink.FailRecord = map[string]error{sealedAccess: errors.New("the store is read-only")}
	report := w.sync()
	if len(report.Findings) != 1 || report.Findings[0].Kind != integration.FindingIdentityFailed {
		t.Fatalf("findings = %+v, want the seat's failure", report.Findings)
	}
	appID := w.record().Account.ID
	if keys := w.f.keysOf(appID); len(keys) != 0 {
		t.Errorf("%d keys are live with their secret sealed nowhere", len(keys))
	}
	if _, ok := w.sink.Held(sealedSecret); ok {
		t.Error("the secret stayed sealed beside an access key that could not be")
	}
	w.sink.FailRecord = nil
	w.sync()
	if _, ok := w.sink.Held(sealedAccess); !ok {
		t.Error("the next pass did not mint a fresh key")
	}
}

// A KEY CREATED WITHOUT ITS SECRET IS DELETED AT ONCE, never left live and
// held by nobody.
func TestAKeyWithoutItsSecretIsDeleted(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.ScalewayLevelRead, projectWeb)
	w.f.noSecret = true
	if report := w.sync(); len(report.Findings) != 1 {
		t.Fatalf("findings = %+v, want the seat's failure", report.Findings)
	}
	if keys := w.f.keysOf(w.record().Account.ID); len(keys) != 0 {
		t.Errorf("a key without its secret was left live: %+v", keys)
	}
}

// A SEAT IN NO PROJECT, OR IN ONE THE ORGANIZATION DOES NOT HAVE, IS REFUSED
// BEFORE SCALEWAY IS ASKED ANYTHING THAT WRITES.
func TestASeatsProjectsAreHeldToTheOrganization(t *testing.T) {
	t.Parallel()
	for name, projects := range map[string][]string{
		"no project":     nil,
		"a stranger's":   {"aaaaaaaa-0000-4000-8000-000000000999"},
		"one and a typo": {projectWeb, "aaaaaaaa-0000-4000-8000-000000000998"},
	} {
		w := newSyncWorld(t, config.ScalewayLevelRead, projects...)
		report := w.sync()
		if len(report.Findings) != 1 || report.Findings[0].Subject != "sre" {
			t.Errorf("%s: findings = %+v, want the seat refused", name, report.Findings)
		}
		if got := w.f.wrote(); len(got) != 0 {
			t.Errorf("%s: wrote %q", name, got)
		}
	}
}

// A DELETE THAT FAILS LEAVES THE AGENT STOPPED, and the next pass deletes the
// rest.
func TestAFailedApplicationDeleteIsRetried(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.ScalewayLevelRead, projectWeb)
	w.sync()
	appID := w.record().Account.ID
	off := false
	w.seat().Enabled = &off
	w.f.refuse["delete app"] = http.StatusBadGateway
	if report := w.sync(); len(report.Findings) != 1 {
		t.Fatalf("a failed delete reported %+v, want the seat's failure", report.Findings)
	}
	if keys, policies := w.f.keysOf(appID), w.f.policiesOf(appID); len(keys)+len(policies) != 0 {
		t.Fatalf("after a failed delete the application holds %+v and %+v, want nothing", keys, policies)
	}
	w.f.edit(func(f *fakeScaleway) { delete(f.refuse, "delete app") })
	w.sync()
	if _, ok := w.f.appNamed("crewlet-acme-sre"); ok {
		t.Error("the next pass left the application at Scaleway")
	}
	if _, ok := w.sink.Held(sealedSecret); ok {
		t.Error("the next pass left the secret sealed")
	}
}

// A DISCONNECT DELETES EVERY APPLICATION, and a seat that left the document
// has its application deleted and its record dropped.
func TestADisconnectAndARemovedSeatDeleteTheirApplications(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.ScalewayLevelRead, projectWeb)
	w.sync()
	w.syncWith(nil, true)
	if _, ok := w.f.appNamed("crewlet-acme-sre"); ok {
		t.Error("a disconnect left the application")
	}

	w = newSyncWorld(t, config.ScalewayLevelRead, projectWeb)
	w.sync()
	w.company.Roles = nil
	w.sync()
	if _, ok := w.f.appNamed("crewlet-acme-sre"); ok {
		t.Error("a seat that left the document kept its application")
	}
	if _, ok := w.ledger.Record(integration.KindScaleway, "sre"); ok {
		t.Error("the record of a seat that left the document was kept")
	}
}

// THE OFFER IS THE THREE LEVELS AND THE ORGANIZATION'S PROJECTS: Administer
// marked as an administration grant, every level's note said, the projects by
// ID with their names, and a level the catalogue cannot grant offered with
// why.
func TestTheOfferIsTheLevelsAndTheProjects(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.ScalewayLevelRead, projectWeb)
	w.f.edit(func(f *fakeScaleway) { delete(f.sets, "KubernetesFullAccess") })
	session, err := w.vendor.Open(t.Context(), w.open())
	if err != nil {
		t.Fatal(err)
	}
	offer, err := session.Options(t.Context(), seataccess.LaneScaleway)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, l := range offer.Levels {
		ids = append(ids, l.ID)
		if l.Note == "" || l.Administers != (l.ID == config.ScalewayLevelAdminister) {
			t.Errorf("level %+v: every level says what it grants, and Administer alone administers", l)
		}
		if unavailable := l.ID == config.ScalewayLevelAdminister; (l.Unavailable != "") != unavailable ||
			unavailable && !strings.Contains(l.Unavailable, "KubernetesFullAccess") {
			t.Errorf("level %s is offered as unavailable %q", l.ID, l.Unavailable)
		}
	}
	if !slices.Equal(ids, scaleway.Levels()) {
		t.Errorf("levels %v, want %v", ids, scaleway.Levels())
	}
	if !slices.Equal(offer.Scope, []string{projectWeb, projectData, projectUnnamed}) || offer.ScopeOpen ||
		!offer.ScopeLive || !offer.ScopeRequired || !offer.Scoped || !offer.Rotatable {
		t.Errorf("offer = %+v", offer)
	}
	if offer.ScopeNames[projectWeb] != "web" || offer.ScopeNames[projectData] != "data" {
		t.Errorf("project names = %v", offer.ScopeNames)
	}
	if _, named := offer.ScopeNames[projectUnnamed]; named {
		t.Error("a project with no name was given one")
	}
}

// A PREVIEW DRAWS SCALEWAY'S OWN STEPS: a seat turned on makes its application
// with its policy, then mints its key sealed under both names; a renewal mints
// before it deletes; taking Administer away, by a level change or by off,
// tells the person to rotate what it reached.
func TestThePlanDrawsScalewaysSteps(t *testing.T) {
	t.Parallel()
	vendor, _ := seataccess.Lookup(integration.KindScaleway)
	seat := seataccess.SeatRef{Handle: "sre", Name: "SRE"}
	want := seataccess.Desired{Enabled: true, Level: config.ScalewayLevelRead, Scope: []string{projectWeb}}
	plan, err := seataccess.Plan(vendor, seataccess.SeatInput{
		Seat: seat, Want: map[seataccess.Lane]seataccess.Desired{seataccess.LaneScaleway: want},
	})
	if err != nil {
		t.Fatal(err)
	}
	steps := plan[seataccess.LaneScaleway]
	if len(steps) != 2 || steps[0].Verb != seataccess.VerbCreate || steps[1].Verb != seataccess.VerbMint ||
		!strings.Contains(steps[1].Text, sealedSecret) || !strings.Contains(steps[1].Text, sealedAccess) {
		t.Fatalf("a seat turned on plans %+v", steps)
	}
	renew := vendor.Steps(seat, seataccess.LaneScaleway, seataccess.OpMint, want, seataccess.Observed{})
	if len(renew) != 2 || renew[0].Verb != seataccess.VerbMint || renew[1].Verb != seataccess.VerbRevoke || renew[1].Index != 1 {
		t.Errorf("a mint plans %+v, want the new key then the deletion of the old", renew)
	}
	admin := seataccess.Observed{Exists: true, Enabled: true, Level: config.ScalewayLevelAdminister}
	for _, op := range []seataccess.Op{seataccess.OpLevel, seataccess.OpDisable} {
		got := vendor.Steps(seat, seataccess.LaneScaleway, op, want, admin)
		if len(got) == 0 || !strings.Contains(got[0].Note, "Rotate") {
			t.Errorf("%s away from Administer plans %+v, want the rotation caveat", op, got)
		}
	}
	if got := vendor.Steps(seat, seataccess.LaneScaleway, seataccess.OpLevel, want,
		seataccess.Observed{Exists: true, Enabled: true, Level: config.ScalewayLevelOperate}); got[0].Note != "" {
		t.Errorf("a change between levels that never held Administer carries %q", got[0].Note)
	}
}

// THE BOX GETS THE SEAT'S OWN KEY, THE ORGANIZATION AS THE DOCUMENT WRITES IT,
// THE SEAT'S DEFAULT PROJECT AND NO UPDATE CHECK, only while its Scaleway lane
// is on: the key as the credentials scw reads, with no line in the document,
// and the rest as settings, none of them a secret.
func TestTheBoxDefaultsFollowTheSeat(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.ScalewayLevelRead, projectData, projectWeb)
	settings, credentials := map[string]string{}, map[string]string{}
	for _, d := range seataccess.BoxDefaults(w.company, "sre") {
		if d.Kind != integration.KindScaleway {
			t.Errorf("default %+v names another integration", d)
		}
		if d.Credential {
			credentials[d.Env] = d.Value
			continue
		}
		settings[d.Env] = d.Value
	}
	want := map[string]string{
		scaleway.OrganizationEnv: "${SCW_ORG}", scaleway.ProjectEnv: projectWeb, scaleway.CheckVersionEnv: "true",
	}
	if !maps.Equal(settings, want) {
		t.Errorf("box settings = %v, want %v", settings, want)
	}
	keys := map[string]string{
		scaleway.SecretKeyEnv: "${SCALEWAY_SECRET_KEY_SRE}", scaleway.AccessKeyEnv: "${SCALEWAY_ACCESS_KEY_SRE}",
	}
	if !maps.Equal(credentials, keys) {
		t.Errorf("box credentials = %v, want %v", credentials, keys)
	}
	off := false
	w.seat().Enabled = &off
	if d := seataccess.BoxDefaults(w.company, "sre"); len(d) != 0 {
		t.Errorf("a seat that is off gets %+v", d)
	}
}
