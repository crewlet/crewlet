package googlecloud_test

import (
	"context"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googlecloud"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/seataccess/seataccesstest"
)

// fakeVendor is the registered Google Cloud vendor, opened against the fake on
// the world's clock.
type fakeVendor struct {
	seataccess.Vendor
	hc    *http.Client
	clock *testClock
}

func (v fakeVendor) Open(_ context.Context, in seataccess.OpenInput) (seataccess.Session, error) {
	return googlecloud.OpenAccess(v.Vendor, in, v.hc, v.clock.now, v.clock.sleep)
}

// sinkSource resolves the sealed store's values, as the engine's resolver does.
type sinkSource struct{ s *seataccesstest.Sink }

func (s sinkSource) Lookup(name string) (string, bool) { return s.s.Held(name) }

// syncWorld is one company, its Google Cloud projects, the ledger and the
// sealed store, run through the shared sync exactly as the loop runs it.
type syncWorld struct {
	t       *testing.T
	f       *fakeGoogle
	clock   *testClock
	vendor  seataccess.Vendor
	company *config.Company
	ledger  *seataccesstest.Ledger
	sink    *seataccesstest.Sink
	key     string
	drift   []seataccess.DriftRestored
}

func newSyncWorld(t *testing.T, level string, projects ...string) *syncWorld {
	t.Helper()
	key, pemText := fakeKey(t)
	clock := newTestClock()
	f := newFakeGoogle(t, &key.PublicKey, clock)
	registered, ok := seataccess.Lookup(integration.KindGoogleCloud)
	if !ok {
		t.Fatal("no Google Cloud vendor is registered")
	}
	on := true
	company := &config.Company{Name: testCompany}
	company.Integrations.GoogleCloud = &config.GoogleCloud{Enabled: true, Provisioning: &config.GoogleCloudProvisioning{
		Provisioner: provisioner, Provider: "${GCP_PROVIDER}", FederationKey: "${" + googlecloud.FederationKeySecret + "}",
	}}
	company.Roles = []config.Role{{
		Name: "SRE", Handle: "sre",
		Integrations: config.RoleIntegrations{GoogleCloud: &config.RoleGoogleCloud{
			Enabled: &on, Level: level, Projects: projects,
		}},
	}}
	return &syncWorld{
		t: t, f: f, clock: clock, company: company, key: pemText,
		ledger: seataccesstest.NewLedger(), sink: seataccesstest.NewSink(),
		vendor: fakeVendor{Vendor: registered, hc: f.server(t), clock: clock},
	}
}

func (w *syncWorld) seat() *config.RoleGoogleCloud {
	return w.company.Roles[0].Integrations.GoogleCloud
}

func (w *syncWorld) open() seataccess.OpenInput {
	return seataccess.OpenInput{Config: w.company, Env: config.NewResolver(config.MapSource{
		"GCP_PROVIDER": testProvider, googlecloud.FederationKeySecret: w.key,
	}, sinkSource{w.sink})}
}

func (w *syncWorld) syncWith(asks map[string]seataccess.Ask) seataccess.SyncReport {
	w.t.Helper()
	report, err := seataccess.Sync(w.t.Context(), seataccess.SyncInput{
		Vendor: w.vendor, Ledger: w.ledger, Sink: w.sink, Pass: "test", By: "test",
		Asks: asks, Now: w.clock.now, Open: w.open(),
		OnDrift: func(_ context.Context, d seataccess.DriftRestored) { w.drift = append(w.drift, d) },
	})
	if err != nil {
		w.t.Fatalf("sync: %v", err)
	}
	return report
}

func (w *syncWorld) sync() seataccess.SyncReport { return w.syncWith(nil) }

// settle moves the world past the read-back bound of every write so far.
func (w *syncWorld) settle() { w.clock.advance(googlecloud.ObserveBudget + time.Second) }

func (w *syncWorld) record() seataccess.Record {
	w.t.Helper()
	rec, ok := w.ledger.Record(integration.KindGoogleCloud, "sre")
	if !ok {
		w.t.Fatal("the ledger has no record for sre")
	}
	return rec
}

// email is the seat's account address, as Google made it.
func (w *syncWorld) email() string {
	return googlecloud.AccountEmail(googlecloud.AccountID(testCompany, "sre"), identity)
}

// roles is every role the seat holds on project.
func (w *syncWorld) roles(project string) []string {
	plain, conditional := googlecloud.HeldBy(w.f.policy(project), "serviceAccount:"+w.email())
	return slices.Concat(plain, conditional)
}

func (w *syncWorld) wantRoles(project, level string) {
	w.t.Helper()
	want, _ := googlecloud.LevelRoles(level, project)
	if level == "" {
		want = nil
	}
	if got := w.roles(project); !slices.Equal(got, want) {
		w.t.Fatalf("on %s the seat holds %v, want %s's %v", project, got, level, want)
	}
}

func noFindings(t *testing.T, label string, report seataccess.SyncReport) {
	t.Helper()
	if len(report.Findings) != 0 {
		t.Fatalf("%s reported %+v", label, report.Findings)
	}
}

// A SEAT'S WHOLE LIFE THROUGH THE SHARED SYNC: on makes its account, lets the
// provisioner alone issue its token, and grants its level one list per write;
// a converged pass writes nothing; a raise, a new project and a lowering each
// touch one list per write on the same unique ID; a role removed at Google is
// restored and reported; off disables the account first and then removes its
// roles, keeping it; on again turns the same account back on; remove deletes
// it. Every write the fake allowed is inside one of the provisioner's
// conditions, which is how Google holds them.
func TestAGoogleCloudSeatThroughTheSync(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, "", projectWeb)

	noFindings(t, "turning on", w.sync())
	rec := w.record()
	sa, ok := w.f.account(w.email())
	if !ok || !rec.Held() || rec.Account.ID != sa.UniqueID || rec.Account.Name != sa.Email {
		t.Fatalf("record %+v, account %+v: the ledger does not record the account by its unique ID", rec.Account, sa)
	}
	if sa.DisplayName != "Crewlet Acme sre" || sa.Description != googlecloud.Description(testCompany, "sre") {
		t.Errorf("the account reads %q, %q", sa.DisplayName, sa.Description)
	}
	plain, _ := googlecloud.HeldBy(sa.Policy, "serviceAccount:"+provisioner)
	if !slices.Equal(plain, []string{"roles/iam.serviceAccountTokenCreator"}) || len(sa.Policy.Bindings) != 1 {
		t.Errorf("the account's policy is %+v, want Token Creator for the provisioner alone", sa.Policy.Bindings)
	}
	w.wantRoles(projectWeb, config.GoogleCloudLevelViewer)
	w.wantRoles(projectData, "")
	for _, project := range []string{projectWeb, projectData, identity} {
		for _, b := range w.f.policy(project).Bindings {
			if b.Role == "roles/iam.serviceAccountTokenCreator" {
				t.Errorf("Token Creator was granted on the project %s", project)
			}
		}
	}

	w.settle()
	writes := len(w.f.wrote())
	noFindings(t, "a converged pass", w.sync())
	if got := w.f.wrote(); len(got) != writes {
		t.Fatalf("a converged pass wrote %q", got[writes:])
	}

	// A RAISE AND A NEW PROJECT, each one list per write.
	w.seat().Level = config.GoogleCloudLevelAdminister
	w.seat().Projects = []string{projectWeb, projectData}
	noFindings(t, "raising to administer on two projects", w.sync())
	w.wantRoles(projectWeb, config.GoogleCloudLevelAdminister)
	w.wantRoles(projectData, config.GoogleCloudLevelAdminister)
	if got := w.record().Account.ID; got != sa.UniqueID {
		t.Errorf("a level change made account %s, want the same %s", got, sa.UniqueID)
	}

	// DRIFT: somebody takes a Viewer role off the seat at Google.
	w.settle()
	w.f.edit(func(f *fakeGoogle) {
		googlecloud.Revoke(&f.projects[projectWeb].Policy, "serviceAccount:"+w.email(), []string{"roles/dns.reader"})
	})
	noFindings(t, "restoring drift", w.sync())
	w.wantRoles(projectWeb, config.GoogleCloudLevelAdminister)
	if len(w.drift) != 1 || w.drift[0].After.Level != config.GoogleCloudLevelAdminister {
		t.Errorf("drift events = %+v, want one restoring administer", w.drift)
	}

	// A LOWERING: Administer's list goes, then Operator's.
	w.settle()
	before := len(w.f.wrote())
	w.seat().Level = config.GoogleCloudLevelViewer
	w.seat().Projects = []string{projectWeb}
	noFindings(t, "lowering to viewer on one project", w.sync())
	w.wantRoles(projectWeb, config.GoogleCloudLevelViewer)
	w.wantRoles(projectData, "")
	var webWrites []string
	for _, write := range w.f.wrote()[before:] {
		if strings.HasPrefix(write, "set the policy of "+projectWeb) {
			webWrites = append(webWrites, write)
		}
	}
	if len(webWrites) != 2 || !strings.Contains(webWrites[0], "roles/storage.admin") ||
		!strings.Contains(webWrites[1], "roles/storage.objectUser") {
		t.Errorf("the lowering on %s wrote %q, want Administer's list then Operator's", projectWeb, webWrites)
	}

	// OFF: disabled first, then its roles; the account stays.
	w.settle()
	before = len(w.f.wrote())
	off := false
	w.seat().Enabled = &off
	noFindings(t, "turning off", w.sync())
	took := w.f.wrote()[before:]
	if len(took) < 2 || took[0] != "disable "+w.email() {
		t.Fatalf("off wrote %q, want the disable first", took)
	}
	if sa, ok := w.f.account(w.email()); !ok || !sa.Disabled {
		t.Fatalf("after off the account is %+v, %v; want it kept and disabled", sa, ok)
	}
	w.wantRoles(projectWeb, "")
	if rec := w.record(); rec.Account.State != seataccess.AccountDisabled {
		t.Errorf("the ledger records %q, want disabled by Crewlet", rec.Account.State)
	}

	// ON AGAIN: the same account, turned back on.
	w.settle()
	on := true
	w.seat().Enabled = &on
	noFindings(t, "turning back on", w.sync())
	if sa2, _ := w.f.account(w.email()); sa2.Disabled || sa2.UniqueID != sa.UniqueID {
		t.Fatalf("on again: %+v, want the same account enabled", sa2)
	}
	w.wantRoles(projectWeb, config.GoogleCloudLevelViewer)

	// REMOVE, once off: deleted, and the ledger closes it.
	w.settle()
	w.seat().Enabled = &off
	w.sync()
	w.settle()
	w.syncWith(map[string]seataccess.Ask{"sre": {Remove: true}})
	if _, ok := w.f.account(w.email()); ok {
		t.Fatal("remove left the account at Google")
	}
	if rec, ok := w.ledger.Record(integration.KindGoogleCloud, "sre"); ok && rec.Held() {
		t.Errorf("the ledger still holds %+v", rec.Account)
	}
	w.f.onlyGoogle(t)
}

// GOOGLE ANSWERING LATE IS NEVER DRIFT AND NEVER A FAULT: a new account
// unknown to the project for a while, and every read-back stale twice, are
// waited out within the bound, and the next pass inside the bound reads the
// same staleness as settling rather than as somebody's change.
func TestAStaleReadBackInsideTheBoundIsNeverDriftOrAFault(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, config.GoogleCloudLevelOperator, projectWeb)
	w.f.edit(func(f *fakeGoogle) { f.lag, f.notYetFirst = 2, 3 })
	noFindings(t, "turning on with Google answering late", w.sync())
	w.wantRoles(projectWeb, config.GoogleCloudLevelOperator)

	// THE POLICY AS IT STOOD BEFORE THE GRANT, which is what a stale read
	// answers: the provisioner's conditions, and none of the seat's roles.
	w.f.edit(func(f *fakeGoogle) {
		past := clonePolicy(f.projects[projectWeb].Policy)
		roles, _ := googlecloud.LevelRoles(config.GoogleCloudLevelAdminister, projectWeb)
		googlecloud.Revoke(&past, "serviceAccount:"+w.email(), roles)
		f.policyPast[projectWeb], f.policyLag[projectWeb] = past, 2
	})
	writes := len(w.f.wrote())
	noFindings(t, "a pass inside the bound reading a stale policy", w.sync())
	if len(w.drift) != 0 || len(w.f.wrote()) != writes {
		t.Fatalf("a stale read inside the bound was drift %+v, writes %q", w.drift, w.f.wrote()[writes:])
	}
}

// PAST THE BOUND IT IS A FAULT THE NEXT PASS RETRIES, never a refusal: Google
// still not knowing the new account after three minutes fails the lane with
// the sentence a seat waits on, and the next pass grants it once it does.
func TestGoogleStillSettlingPastTheBoundIsRetried(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, "", projectWeb)
	w.f.edit(func(f *fakeGoogle) { f.notYetFirst = 1000 })
	report := w.sync()
	if len(report.Findings) != 1 || !strings.Contains(report.Findings[0].Detail, googlecloud.ReasonCreating) {
		t.Fatalf("a bound passed reported %+v, want one fault naming %q", report.Findings, googlecloud.ReasonCreating)
	}
	lane := w.record().Lanes[seataccess.LaneGoogleCloud]
	if lane.Sync == nil || lane.Sync.Outcome != seataccess.OutcomeFailed || lane.Sync.Error.Actor != seataccess.ByCrewlet {
		t.Fatalf("the lane ended %+v, want a failure Crewlet retries", lane.Sync)
	}
	w.f.edit(func(f *fakeGoogle) { f.notYet = map[string]int{} })
	w.settle()
	noFindings(t, "the retry once Google knows the account", w.sync())
	w.wantRoles(projectWeb, config.GoogleCloudLevelViewer)
}

// AN ACCOUNT SOMEBODY DISABLED AT GOOGLE IS NEVER TURNED BACK ON UNASKED (D3),
// and is when a person asks.
func TestAnAccountDisabledAtGoogleWaitsForAPerson(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, "", projectWeb)
	w.sync()
	w.settle()
	w.f.edit(func(f *fakeGoogle) { f.byAccount(w.email()).Disabled = true })
	report := w.sync()
	if sa, _ := w.f.account(w.email()); !sa.Disabled {
		t.Fatal("a pass turned back on an account somebody disabled at Google")
	}
	if len(report.Findings) != 1 || report.Findings[0].Kind != integration.FindingIdentityFailed {
		t.Fatalf("findings = %+v, want the seat's disable reported for a person", report.Findings)
	}
	w.settle()
	noFindings(t, "turning back on when asked", w.syncWith(map[string]seataccess.Ask{"sre": {Reenable: true}}))
	if sa, _ := w.f.account(w.email()); sa.Disabled {
		t.Fatal("a person's ask did not turn the account back on")
	}
}

// AN ACCOUNT UNDER THE SEAT'S NAME THAT THE LEDGER DOES NOT RECORD IS NEVER
// ADOPTED: the create is refused with what to do, and nothing is written to it.
func TestAnAccountUnderTheSeatsNameIsNeverAdopted(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, "", projectWeb)
	w.f.edit(func(f *fakeGoogle) {
		f.accounts["199"] = &fakeAccount{ID: googlecloud.AccountID(testCompany, "sre"), Email: w.email(),
			UniqueID: "199", Policy: googlecloud.Policy{Etag: "a0"}}
	})
	report := w.sync()
	if len(report.Findings) != 1 || !strings.Contains(report.Findings[0].Detail, "no record of making it") {
		t.Fatalf("findings = %+v, want the stranger account refused", report.Findings)
	}
	if rec, ok := w.ledger.Record(integration.KindGoogleCloud, "sre"); ok && rec.Held() {
		t.Fatalf("the ledger adopted %+v", rec.Account)
	}
	w.wantRoles(projectWeb, "")
}

// A ROLE NO LEVEL GRANTS IS NAMED AND LEFT: the provisioner's conditions refuse
// revoking it, so the seat is refused with the remedy, its level roles kept.
func TestARoleNoLevelGrantsIsNamedAndLeft(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, "", projectWeb)
	w.sync()
	w.settle()
	w.f.edit(func(f *fakeGoogle) {
		googlecloud.Grant(&f.projects[projectWeb].Policy, "serviceAccount:"+w.email(), []string{"roles/editor"})
	})
	report := w.sync()
	if len(report.Findings) != 1 || !strings.Contains(report.Findings[0].Detail, "roles/editor") {
		t.Fatalf("findings = %+v, want roles/editor named", report.Findings)
	}
	if !slices.Contains(w.roles(projectWeb), "roles/editor") {
		t.Fatal("a role outside the conditions was taken")
	}
}

// ONLY A FULLY SET-UP PROJECT IS OFFERED, by ID with Google's display name: a
// half set-up one, one with a widened crewletOperator, one whose condition
// grants more than a list, one where the provisioner holds an unconditional
// admin role, and one nobody set up are not.
func TestOnlyAFullySetUpProjectIsOffered(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, "", projectWeb)
	w.f.edit(func(f *fakeGoogle) {
		f.addProject("acme-wide", "Wide", 3, true)
		r := f.roles[googlecloud.CustomRole("acme-wide")]
		r.Permissions = append(r.Permissions, "iam.serviceAccounts.actAs")
		f.roles[googlecloud.CustomRole("acme-wide")] = r
		f.addProject("acme-ortrue", "Or true", 3, true)
		b := &f.projects["acme-ortrue"].Policy.Bindings[1]
		b.Condition = &googlecloud.Condition{Expression: b.Condition.Expression + " || true"}
		f.addProject("acme-plain", "Plain", 3, true)
		f.projects["acme-plain"].Policy.Bindings = append(f.projects["acme-plain"].Policy.Bindings,
			googlecloud.Binding{Role: "roles/resourcemanager.projectIamAdmin", Members: []string{"serviceAccount:" + provisioner}})
		f.addProject("acme-nocustom", "No custom", 3, false)
	})
	session, err := w.vendor.Open(t.Context(), w.open())
	if err != nil {
		t.Fatal(err)
	}
	opts, err := session.Options(t.Context(), seataccess.LaneGoogleCloud)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(opts.Scope, []string{projectData, projectWeb}) || opts.ScopeNames[projectWeb] != "Acme Web" ||
		!opts.ScopeLive || !opts.ScopeRequired || opts.Rotatable {
		t.Fatalf("offer = %+v, want the two set-up projects with their names", opts)
	}
	var ids []string
	for _, l := range opts.Levels {
		ids = append(ids, l.ID)
		if l.Unavailable != "" {
			t.Errorf("%s is offered unavailable: %s", l.ID, l.Unavailable)
		}
	}
	if !slices.Equal(ids, googlecloud.Levels()) || opts.Levels[0].Administers || !opts.Levels[2].Administers {
		t.Errorf("levels %+v", opts.Levels)
	}
}

// A PROJECT NO LONGER SET UP IS REFUSED FOR THE SEAT that names it, before
// anything is asked of Google.
func TestAProjectNotSetUpIsRefusedForTheSeat(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, "", projectHalf)
	report := w.sync()
	if len(report.Findings) != 1 || !strings.Contains(report.Findings[0].Detail, projectHalf) {
		t.Fatalf("findings = %+v, want %s refused", report.Findings, projectHalf)
	}
	if _, ok := w.f.account(w.email()); ok {
		t.Error("an account was made for a seat in no set-up project")
	}
}

// A LEVEL WHOSE ROLE GOOGLE CHANGED IS UNAVAILABLE: a Viewer role that now
// holds a credential permission takes Viewer and Operator off the offer and
// refuses a seat at either, while Administer, which may hold it, stays.
func TestALevelWhoseRoleGoogleChangedIsUnavailable(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, "", projectWeb)
	w.f.edit(func(f *fakeGoogle) {
		r := f.roles["roles/compute.viewer"]
		r.Permissions = append(r.Permissions, "compute.instances.setMetadata")
		f.roles["roles/compute.viewer"] = r
	})
	session, err := w.vendor.Open(t.Context(), w.open())
	if err != nil {
		t.Fatal(err)
	}
	opts, err := session.Options(t.Context(), seataccess.LaneGoogleCloud)
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range opts.Levels {
		if want := l.ID != config.GoogleCloudLevelAdminister; (l.Unavailable != "") != want {
			t.Errorf("%s unavailable = %q, want %v", l.ID, l.Unavailable, want)
		}
	}
	report := w.sync()
	if len(report.Findings) != 1 || !strings.Contains(report.Findings[0].Detail, "compute.instances.setMetadata") {
		t.Fatalf("findings = %+v, want the viewer seat refused naming the permission", report.Findings)
	}
	w.wantRoles(projectWeb, "")
}

// THE PLAN DRAWS GOOGLE CLOUD'S STEPS: three to turn on, two to turn off, one
// to remove, and none to mint, rotate or revoke, since the token is issued at
// use. Taking Administer away says what it may have left behind.
func TestThePlanDrawsGoogleCloudsSteps(t *testing.T) {
	t.Parallel()
	v, _ := seataccess.Lookup(integration.KindGoogleCloud)
	seat := seataccess.SeatRef{Handle: "sre", Name: "SRE"}
	want := seataccess.Desired{Enabled: true, Level: config.GoogleCloudLevelOperator, Scope: []string{projectWeb}}
	admin := seataccess.Observed{Exists: true, Enabled: true, Level: config.GoogleCloudLevelAdminister,
		Scope: []string{projectWeb}}
	for _, tc := range []struct {
		op    seataccess.Op
		have  seataccess.Observed
		steps int
		note  bool
	}{
		{seataccess.OpCreate, seataccess.Observed{}, 3, false},
		{seataccess.OpEnable, admin, 1, false},
		{seataccess.OpLevel, admin, 1, true},
		{seataccess.OpScope, admin, 1, false},
		{seataccess.OpDisable, admin, 2, true},
		{seataccess.OpRemove, admin, 1, false},
		{seataccess.OpMint, admin, 0, false},
		{seataccess.OpRotate, admin, 0, false},
		{seataccess.OpRevoke, admin, 0, false},
		{seataccess.OpRetire, admin, 0, false},
	} {
		steps := v.Steps(seat, seataccess.LaneGoogleCloud, tc.op, want, tc.have)
		if len(steps) != tc.steps {
			t.Errorf("%s: %d steps, want %d", tc.op, len(steps), tc.steps)
			continue
		}
		for i, s := range steps {
			if s.By != seataccess.ByCrewlet || s.Index != i || s.Op != tc.op || s.Text == "" {
				t.Errorf("%s step %d = %+v", tc.op, i, s)
			}
		}
		if tc.steps > 0 && (steps[0].Note != "") != tc.note {
			t.Errorf("%s: note %q, want one %v", tc.op, steps[0].Note, tc.note)
		}
	}
	if steps := v.Steps(seat, seataccess.LaneGoogleCloud, seataccess.OpDisable, want, admin); steps[0].Verb != seataccess.VerbDisable {
		t.Errorf("off's first step is %s, want the disable", steps[0].Verb)
	}
}

// AN ACCOUNT ID FITS GOOGLE AND TELLS COMPANIES AND HANDLES APART: at most 30
// lowercase letters, digits and dashes from a letter, the handle cut to 17.
func TestAnAccountIDFitsGoogleAndTellsSeatsApart(t *testing.T) {
	t.Parallel()
	valid := regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
	long := "site-reliability-engineering-lead"
	ids := map[string]bool{}
	for _, tc := range []struct{ company, handle string }{
		{testCompany, "sre"}, {testCompany, long}, {"Other Co", "sre"}, {testCompany, long + "-2"}, {testCompany, "___"},
	} {
		id := googlecloud.AccountID(tc.company, tc.handle)
		if !valid.MatchString(id) || !strings.HasPrefix(id, "crewlet-") {
			t.Errorf("%s/%s: %q is not an account ID Google takes", tc.company, tc.handle, id)
		}
		if ids[id] {
			t.Errorf("%s/%s: %q is another seat's", tc.company, tc.handle, id)
		}
		ids[id] = true
	}
	// CUT TO 17, "site-reliability-", and the dash it ends on dropped.
	if id := googlecloud.AccountID(testCompany, long); !regexp.MustCompile(`^crewlet-site-reliability-[0-9a-f]{4}$`).MatchString(id) {
		t.Errorf("a long handle reads %q, want it cut to 17", id)
	}
	if name := googlecloud.DisplayName(strings.Repeat("Acme ", 40), "sre"); len(name) > 100 {
		t.Errorf("a display name of %d bytes", len(name))
	}
}
