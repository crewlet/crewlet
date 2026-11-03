package cloudflare_test

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/cloudflare"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/integration/integrationtest"
)

// cloudflareWorld is the company's half of Cloudflare as the loop reconciles
// it: one account at the fake and the company's block.
type cloudflareWorld struct {
	f   *fakeCloudflare
	opt cloudflare.Options
}

func (w *cloudflareWorld) Kind() integration.Kind { return integration.KindCloudflare }

func (w *cloudflareWorld) Reconcile(ctx context.Context) ([]integration.Finding, error) {
	res, err := cloudflare.Reconcile(ctx, w.opt)
	if err != nil {
		return nil, err
	}
	return res.Findings(), nil
}

func (w *cloudflareWorld) mutations() int { return len(w.f.wrote()) }

var reconcileNow = time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)

// convergedCloudflare is a world with nothing to report: an active
// provisioner holding every group, with no expiry, over zones that exist.
func convergedCloudflare(t *testing.T, tb integrationtest.TB) *cloudflareWorld {
	t.Helper()
	f := newFakeCloudflare()
	c, err := cloudflare.NewClient(cloudflare.ClientOptions{AccountID: testAccount, Token: provisioner, HTTP: f.server(t)})
	if err != nil {
		tb.Fatalf("NewClient: %v", err)
	}
	return &cloudflareWorld{f: f, opt: cloudflare.Options{
		Client: c, Now: func() time.Time { return reconcileNow }, Company: "Acme",
		Config: &config.Cloudflare{Enabled: true, Provisioning: &config.CloudflareProvisioning{
			AccountID: testAccount, APIToken: "${CLOUDFLARE_API_TOKEN}", Zones: []string{zoneExampleName},
		}},
	}}
}

// outstandingCloudflare holds two faults a person settles: a zone the account
// does not have (the document's), and a group the provisioner does not hold
// which the level an agent is on grants (Cloudflare's, by an admin).
func outstandingCloudflare(t *testing.T, tb integrationtest.TB) *cloudflareWorld {
	t.Helper()
	w := convergedCloudflare(t, tb)
	w.opt.Config.Provisioning.Zones = []string{zoneExampleName, "missing.example"}
	w.opt.InUse = []string{config.CloudflareLevelOperate}
	w.f.set(func(f *fakeCloudflare) {
		own := f.provisionerOwn[0]
		own.PermissionGroups = slices.DeleteFunc(slices.Clone(own.PermissionGroups),
			func(g cloudflare.GroupRef) bool { return g.ID == groupID("Cache Settings Write") })
		f.provisionerOwn = []cloudflare.Policy{own}
		// AND A TOKEN NAMED FOR AN AGENT THAT NOTHING RECORDS, an advisory
		// owed by an admin and never touched.
		f.tokens["tok-stray"] = &fakeToken{ID: "tok-stray", Name: "crewlet-acme-old-sre",
			Status: cloudflare.StatusActive}
	})
	return w
}

// THE CONTRACT IS CERTIFIED AGAINST THE REAL RECONCILER, OVER BOTH WORLDS.
func TestTheCloudflareReconcilerMeetsTheContract(t *testing.T) {
	t.Parallel()
	var converged *cloudflareWorld
	integrationtest.Run(t, integrationtest.Reconciler{
		Converged: func(tb integrationtest.TB) integration.Reconciler {
			converged = convergedCloudflare(t, tb)
			return converged
		},
		Outstanding: func(tb integrationtest.TB) integration.Reconciler {
			return outstandingCloudflare(t, tb)
		},
		Mutations: func() int { return converged.mutations() },
	})
}

// AND THE OUTSTANDING WORLD REPORTS EACH FAULT IT HOLDS, on the field that
// clears it.
func TestTheOutstandingWorldReportsEveryFaultItHolds(t *testing.T) {
	t.Parallel()
	findings, err := outstandingCloudflare(t, t).Reconcile(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := map[integration.FindingKind]string{
		integration.FindingUnknownTier:          "integrations.cloudflare.provisioning.zones",
		integration.FindingGrantShort:           "cloudflare level operate",
		integration.FindingLevelUnavailable:     "cloudflare level configure",
		integration.FindingRegistrationOrphaned: "cloudflare agent tokens",
	}
	for _, f := range findings {
		subject, ok := want[f.Kind]
		if !ok || f.Subject != subject {
			t.Errorf("unexpected finding %s on %q: %s", f.Kind, f.Subject, f.Detail)
			continue
		}
		delete(want, f.Kind)
	}
	for kind, subject := range want {
		t.Errorf("the world holds %s on %s and it was not reported", kind, subject)
	}
}

// AN EXPIRING PROVISIONER IS AN ADVISORY WITH ITS DATE, weeks early.
func TestAnExpiringProvisionerIsReported(t *testing.T) {
	t.Parallel()
	w := convergedCloudflare(t, t)
	expires := reconcileNow.Add(10 * 24 * time.Hour)
	w.f.set(func(f *fakeCloudflare) { f.provisionerExpires = rfc3339(expires) })
	findings, err := w.Reconcile(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].Kind != integration.FindingCredentialExpiring ||
		!findings[0].ExpiresAt.Equal(expires) {
		t.Fatalf("findings = %+v, want one expiring credential on %v", findings, expires)
	}
	w.f.set(func(f *fakeCloudflare) { f.provisionerExpires = rfc3339(reconcileNow.Add(60 * 24 * time.Hour)) })
	if findings, _ := w.Reconcile(t.Context()); len(findings) != 0 {
		t.Errorf("a token expiring in 60 days reported %+v", findings)
	}
}

// A LEVEL CLOUDFLARE CANNOT RESOLVE IS REPORTED BY NAME, and so is every
// level above it, since each grants what the levels before it do: a fault
// for the level an agent is on, an advisory for one nobody holds.
func TestAnUnresolvableLevelIsReported(t *testing.T) {
	t.Parallel()
	w := convergedCloudflare(t, t)
	w.opt.InUse = []string{config.CloudflareLevelRead, config.CloudflareLevelOperate}
	w.f.set(func(f *fakeCloudflare) {
		f.groups = slices.DeleteFunc(f.groups, func(g fakeGroup) bool { return g.Name == "Cache Purge" && g.Selectable == nil })
	})
	findings, err := w.Reconcile(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 2 {
		t.Fatalf("findings = %+v, want the operate and configure levels' missing group", findings)
	}
	for i, want := range []struct {
		level string
		kind  integration.FindingKind
	}{{"operate", integration.FindingUnknownTier}, {"configure", integration.FindingLevelUnavailable}} {
		if findings[i].Kind != want.kind || findings[i].Subject != "cloudflare level "+want.level ||
			!slices.Equal(findings[i].Subjects, []string{"Cache Purge"}) {
			t.Errorf("finding %d = %+v, want the %s level missing Cache Purge as %s", i, findings[i], want.level, want.kind)
		}
	}
}

// A LEVEL THE PROVISIONER CANNOT GRANT NEEDS ATTENTION ONLY WHILE AN AGENT IS
// ON IT. A company whose agents never take configure reads ready, with a note
// naming what the level waits on; an agent on it makes the same gap a fault.
// Mutation: report a gap whatever the agents hold, and the first half is red.
func TestALevelNoAgentHoldsIsANoteNotAFault(t *testing.T) {
	t.Parallel()
	w := convergedCloudflare(t, t)
	w.opt.InUse = []string{config.CloudflareLevelOperate}
	w.f.set(func(f *fakeCloudflare) {
		own := f.provisionerOwn[0]
		own.PermissionGroups = slices.DeleteFunc(slices.Clone(own.PermissionGroups), func(g cloudflare.GroupRef) bool {
			return g.ID == groupID("Zone Settings Write") || g.ID == groupID("Response Compression Write")
		})
		f.provisionerOwn = []cloudflare.Policy{own}
	})
	findings, err := w.Reconcile(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].Kind != integration.FindingLevelUnavailable ||
		findings[0].Detail != "The Configure level is unavailable until the provisioner token holds "+
			"Zone Settings: Edit and Response Compression: Edit" ||
		!slices.Equal(findings[0].Subjects, []string{"Zone Settings: Edit", "Response Compression: Edit"}) {
		t.Fatalf("findings = %+v, want the one note on the Configure level", findings)
	}
	if report := integration.Classify(findings); report.Phase != integration.PhaseReady {
		t.Errorf("a level nobody holds classified as %s, want ready", report.Phase)
	}

	w.opt.InUse = append(w.opt.InUse, config.CloudflareLevelConfigure)
	findings, err = w.Reconcile(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].Kind != integration.FindingGrantShort ||
		findings[0].Subject != "cloudflare level configure" {
		t.Fatalf("findings = %+v, want the Configure level's gap as a fault", findings)
	}
	if report := integration.Classify(findings); report.Phase != integration.PhaseDegraded {
		t.Errorf("an agent's level the token cannot grant classified as %s, want degraded", report.Phase)
	}
}

// ONLY AN ACTIVE TOKEN NAMED FOR THIS COMPANY'S AGENTS AND RECORDED NOWHERE IS
// AN ORPHAN: the ledger's tokens, the provisioner, an inert token and another
// company's are not, and the pass deletes nothing.
func TestAnUnrecordedAgentTokenIsReportedAndLeftAlone(t *testing.T) {
	t.Parallel()
	w := convergedCloudflare(t, t)
	w.opt.Managed = []string{"tok-managed"}
	w.f.set(func(f *fakeCloudflare) {
		for _, tok := range []*fakeToken{
			{ID: "tok-managed", Name: "crewlet-acme-sre", Status: cloudflare.StatusActive},
			{ID: "tok-stray", Name: "crewlet-acme-sre", Status: cloudflare.StatusActive},
			{ID: "tok-off", Name: "crewlet-acme-ops", Status: cloudflare.StatusDisabled},
			{ID: "tok-other", Name: "crewlet-globex-sre", Status: cloudflare.StatusActive},
			{ID: "tok-human", Name: "deploy", Status: cloudflare.StatusActive},
		} {
			f.tokens[tok.ID] = tok
		}
	})
	findings, err := w.Reconcile(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(findings) != 1 || findings[0].Kind != integration.FindingRegistrationOrphaned ||
		!slices.Equal(findings[0].Subjects, []string{"crewlet-acme-sre (tok-stray)"}) {
		t.Fatalf("findings = %+v, want the one unrecorded active agent token", findings)
	}
	if got := w.f.wrote(); len(got) != 0 {
		t.Errorf("the advisory wrote %v", got)
	}
}

// A REFUSED OR INACTIVE PROVISIONER IS THE CREDENTIAL'S, and the loop records
// it as refused rather than waiting on it; an outage is a fault it retries.
func TestARefusedProvisionerIsARejectedCredential(t *testing.T) {
	t.Parallel()
	for name, arrange := range map[string]func(*fakeCloudflare){
		"refused":        func(f *fakeCloudflare) { f.refuse["verify"] = http.StatusUnauthorized },
		"expired":        func(f *fakeCloudflare) { f.provisionerStatus = cloudflare.StatusExpired },
		"no token right": func(f *fakeCloudflare) { f.refuse["groups"] = http.StatusForbidden },
	} {
		w := convergedCloudflare(t, t)
		w.f.set(arrange)
		_, err := w.Reconcile(t.Context())
		if !errors.Is(err, integration.ErrCredentialRejected) {
			t.Errorf("%s: %v, want a rejected credential", name, err)
		}
	}
	w := convergedCloudflare(t, t)
	w.f.set(func(f *fakeCloudflare) { f.refuse["zones"] = http.StatusBadGateway })
	if _, err := w.Reconcile(t.Context()); err == nil || errors.Is(err, integration.ErrCredentialRejected) {
		t.Errorf("an outage = %v, want a fault that is not a refusal", err)
	}
	w.opt.Config = nil
	if _, err := w.Reconcile(t.Context()); !errors.Is(err, integration.ErrNotConfigured) {
		t.Errorf("no block = %v, want not configured", err)
	}
}
