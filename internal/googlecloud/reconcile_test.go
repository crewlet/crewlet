package googlecloud_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googlecloud"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/integration/integrationtest"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// googleWorld is a Google Cloud pass as the engine runs it: the company half,
// then every agent's account through the access sync.
type googleWorld struct{ sync *syncWorld }

func (*googleWorld) Kind() integration.Kind { return integration.KindGoogleCloud }

func (w *googleWorld) Reconcile(ctx context.Context) ([]integration.Finding, error) {
	res, err := w.company(ctx)
	if err != nil {
		return nil, err
	}
	report, err := seataccess.Sync(ctx, seataccess.SyncInput{
		Vendor: w.sync.vendor, Ledger: w.sync.ledger, Sink: w.sync.sink, Pass: "test", By: "test",
		Now: w.sync.clock.now, Open: w.sync.open(),
	})
	if err != nil {
		return nil, err
	}
	return append(res.Findings(), report.Findings...), nil
}

// company is the company half over the ledger's accounts.
func (w *googleWorld) company(ctx context.Context) (*googlecloud.Result, error) {
	client, err := googlecloud.ClientFor(w.sync.company, w.sync.open().Env, w.sync.vendor.(fakeVendor).hc)
	if err != nil {
		return nil, err
	}
	googlecloud.SetClock(client, w.sync.clock.now, w.sync.clock.sleep)
	records, err := w.sync.ledger.Records(ctx, integration.KindGoogleCloud)
	if err != nil {
		return nil, err
	}
	seats := map[string]string{}
	for handle, rec := range records {
		if rec.Held() {
			seats[handle] = rec.Account.ID
		}
	}
	return googlecloud.Reconcile(ctx, googlecloud.Options{
		Client: client, Company: w.sync.company.Name, Seats: seats,
		InUse: seataccess.LevelsInUse(w.sync.company, seataccess.LaneGoogleCloud),
	})
}

// convergedGoogle is two agents at different levels, made and settled.
func convergedGoogle(t *testing.T) *googleWorld {
	t.Helper()
	s := newSyncWorld(t, "", projectWeb)
	on := true
	s.company.Roles = append(s.company.Roles, config.Role{Name: "QA", Handle: "qa",
		Integrations: config.RoleIntegrations{GoogleCloud: &config.RoleGoogleCloud{
			Enabled: &on, Level: config.GoogleCloudLevelOperator, Projects: []string{projectWeb, projectData},
		}}})
	w := &googleWorld{sync: s}
	if findings, err := w.Reconcile(t.Context()); err != nil || len(findings) != 0 {
		t.Fatalf("seeding pass = %+v, %v", findings, err)
	}
	s.settle()
	return w
}

// outstandingGoogle is a converged world where a person has to act twice: an
// agent names a project nobody finished preparing, and somebody disabled the
// other's account at Google.
func outstandingGoogle(t *testing.T) *googleWorld {
	t.Helper()
	w := convergedGoogle(t)
	w.sync.company.Roles[1].Integrations.GoogleCloud.Projects = []string{projectHalf}
	w.sync.f.edit(func(f *fakeGoogle) { f.byAccount(w.sync.email()).Disabled = true })
	return w
}

// THE CONTRACT IS CERTIFIED AGAINST THE REAL SYNC, OVER BOTH WORLDS.
func TestTheGoogleCloudReconcilerMeetsTheContract(t *testing.T) {
	t.Parallel()
	var converged *googleWorld
	integrationtest.Run(t, integrationtest.Reconciler{
		Converged: func(integrationtest.TB) integration.Reconciler {
			converged = convergedGoogle(t)
			return converged
		},
		Outstanding: func(integrationtest.TB) integration.Reconciler { return outstandingGoogle(t) },
		Mutations:   func() int { return len(converged.sync.f.wrote()) },
	})
}

// A CLEAN IDENTITY PROJECT REPORTS NOTHING: the provisioner and the ledger's
// accounts are all it holds, and every level can be granted.
func TestACleanIdentityProjectReportsNothing(t *testing.T) {
	t.Parallel()
	w := convergedGoogle(t)
	res, err := w.company(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Findings(); len(got) != 0 {
		t.Fatalf("findings = %+v", got)
	}
}

// AN ACCOUNT THE PROVISIONER COULD ACT AS, AND ONE NAMED FOR AN AGENT WITH NO
// SEAT, ARE REPORTED, never touched; another company's agent sharing the
// identity project is neither.
func TestStrangersInTheIdentityProjectAreReported(t *testing.T) {
	t.Parallel()
	w := convergedGoogle(t)
	writes := len(w.sync.f.wrote())
	w.sync.f.edit(func(f *fakeGoogle) {
		f.accounts["401"] = &fakeAccount{Email: "deploy@" + identity + ".iam.gserviceaccount.com", UniqueID: "401"}
		f.accounts["402"] = &fakeAccount{Email: googlecloud.AccountEmail(googlecloud.AccountID(testCompany, "ops"), identity),
			UniqueID: "402", DisplayName: googlecloud.DisplayName(testCompany, "ops")}
		f.accounts["403"] = &fakeAccount{Email: googlecloud.AccountEmail(googlecloud.AccountID("Other", "sre"), identity),
			UniqueID: "403", DisplayName: googlecloud.DisplayName("Other", "sre")}
	})
	res, err := w.company(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[integration.FindingKind][]string{}
	for _, f := range res.Findings() {
		kinds[f.Kind] = f.Subjects
	}
	if got := kinds[integration.FindingGrantExcess]; !slices.Equal(got, []string{"deploy@" + identity + ".iam.gserviceaccount.com"}) {
		t.Errorf("foreign accounts = %v", got)
	}
	if got := kinds[integration.FindingRegistrationOrphaned]; len(got) != 1 || !strings.Contains(got[0], "crewlet-ops-") {
		t.Errorf("orphaned accounts = %v, want this company's ops agent alone", got)
	}
	if len(w.sync.f.wrote()) != writes {
		t.Error("the company pass wrote at Google")
	}
}

// A LEVEL A SEAT HOLDS THAT GOOGLE'S ROLES CANNOT BE GRANTED AT BLOCKS, and an
// unused one is an advisory.
func TestAGapOnALevelInUseBlocks(t *testing.T) {
	t.Parallel()
	w := convergedGoogle(t)
	w.sync.f.edit(func(f *fakeGoogle) {
		r := f.roles["roles/storage.admin"]
		r.Permissions = append(r.Permissions, "storage.hmacKeys.create")
		f.roles["roles/storage.admin"] = r
	})
	res, err := w.company(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	findings := res.Findings()
	if len(findings) != 1 || findings[0].Kind != integration.FindingLevelUnavailable {
		t.Fatalf("an unused gap = %+v, want an advisory", findings)
	}
	w.sync.seat().Level = config.GoogleCloudLevelAdminister
	if res, err = w.company(t.Context()); err != nil {
		t.Fatal(err)
	}
	if findings := res.Findings(); len(findings) != 1 || findings[0].Kind != integration.FindingUnknownTier {
		t.Fatalf("a gap in use = %+v, want the blocking finding", findings)
	}
}

// A PROVISIONER CREWLET CANNOT REACH IS THE CREDENTIAL'S ERROR, which nothing
// else a pass reports could be trusted over.
func TestAnUnreachableProvisionerIsTheCredentialsError(t *testing.T) {
	t.Parallel()
	w := convergedGoogle(t)
	other, _ := fakeKey(t)
	w.sync.f.edit(func(f *fakeGoogle) { f.pub = &other.PublicKey; f.tokens = map[string]fakeToken{} })
	_, err := w.company(t.Context())
	if !errors.Is(err, integration.ErrCredentialRejected) || !strings.Contains(err.Error(), "federation") {
		t.Fatalf("err = %v, want the provisioner refused at federation", err)
	}
}
