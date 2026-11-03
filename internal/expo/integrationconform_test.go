package expo_test

import (
	"context"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/expo"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/integration/integrationtest"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/seataccess/seataccesstest"
)

// countingSink is the test sink, counting every seal and every clear: a write
// into this deployment's own store is a write the suite has to see.
type countingSink struct {
	*seataccesstest.Sink
	writes atomic.Int64
}

func (s *countingSink) Record(ctx context.Context, name, value string) error {
	s.writes.Add(1)
	return s.Sink.Record(ctx, name, value)
}

func (s *countingSink) Forget(ctx context.Context, names ...string) error {
	s.writes.Add(1)
	return s.Sink.Forget(ctx, names...)
}

// expoWorld is the company half and the agents' robots, run as the engine's
// pass runs them: the company reconcile, then the access sync.
type expoWorld struct {
	sync   *syncWorld
	sink   *countingSink
	client *expo.Client
}

func (*expoWorld) Kind() integration.Kind { return integration.KindExpo }

func (w *expoWorld) Reconcile(ctx context.Context) ([]integration.Finding, error) {
	res, err := expo.Reconcile(ctx, expo.Options{Client: w.client, Account: testAccountName})
	if err != nil {
		return nil, err
	}
	report, err := seataccess.Sync(ctx, seataccess.SyncInput{
		Vendor: w.sync.vendor, Ledger: w.sync.ledger, Sink: w.sink, Pass: "test", By: "test",
		Open: seataccess.OpenInput{
			Config: w.sync.company,
			Env: config.NewResolver(config.MapSource{
				"EXPO_ACCOUNT": testAccountName, "EXPO_ACCESS_TOKEN": provisioner,
			}),
		},
	})
	if err != nil {
		return nil, err
	}
	return append(res.Findings(), report.Findings...), nil
}

// mutations is every write at Expo and into the sealed store.
func (w *expoWorld) mutations() int { return len(w.sync.expo.writes()) + int(w.sink.writes.Load()) }

// convergedExpo stands two agents up at different levels and converges them
// BY RUNNING THE PASS, so the world is converged by definition and the
// question left is whether the next pass writes.
func convergedExpo(t *testing.T) *expoWorld {
	t.Helper()
	s := newSyncWorld(t, config.ExpoLevelDeveloper)
	on := true
	s.company.Roles = append(s.company.Roles, config.Role{Name: "SRE", Handle: "sre",
		Integrations: config.RoleIntegrations{Expo: &config.RoleExpo{Enabled: &on}}})
	client, err := expo.NewClient(expo.ClientOptions{Token: provisioner, HTTP: &http.Client{Transport: s.expo.rw}})
	if err != nil {
		t.Fatal(err)
	}
	w := &expoWorld{sync: s, sink: &countingSink{Sink: s.sink}, client: client}
	findings, err := w.Reconcile(context.Background())
	if err != nil || len(findings) != 0 {
		t.Fatalf("seeding pass = %+v, %v", findings, err)
	}
	return w
}

// outstandingExpo is a converged world where a person has to act twice: the
// provisioner lost the Admin role, and a new agent's robot cannot be created.
func outstandingExpo(t *testing.T) *expoWorld {
	t.Helper()
	w := convergedExpo(t)
	on := true
	w.sync.company.Roles = append(w.sync.company.Roles, config.Role{Name: "Release", Handle: "release",
		Integrations: config.RoleIntegrations{Expo: &config.RoleExpo{Enabled: &on}}})
	w.sync.expo.mu.Lock()
	defer w.sync.expo.mu.Unlock()
	w.sync.expo.viewerPerms = []expo.Permission{expo.PermissionView, expo.PermissionPublish}
	w.sync.expo.refuse["CrewletCreateRobot"] = fakeRefusal{code: "UNAUTHORIZED_ERROR", message: "not permitted"}
	return w
}

// THE CONTRACT IS CERTIFIED AGAINST THE REAL PASS, OVER BOTH WORLDS.
func TestTheExpoReconcilerMeetsTheContract(t *testing.T) {
	t.Parallel()
	var converged *expoWorld
	integrationtest.Run(t, integrationtest.Reconciler{
		Converged: func(integrationtest.TB) integration.Reconciler {
			converged = convergedExpo(t)
			return converged
		},
		Outstanding: func(integrationtest.TB) integration.Reconciler { return outstandingExpo(t) },
		Mutations:   func() int { return converged.mutations() },
	})
}

// AND THE OUTSTANDING WORLD REPORTS EVERY FAULT IT HOLDS, each on the subject
// a person acts on.
func TestTheOutstandingExpoWorldReportsEveryFault(t *testing.T) {
	t.Parallel()
	findings, err := outstandingExpo(t).Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[integration.FindingKind]bool{
		integration.FindingGrantShort: false, integration.FindingIdentityFailed: false,
	}
	for _, f := range findings {
		if _, ok := want[f.Kind]; !ok {
			t.Errorf("unexpected finding %+v", f)
			continue
		}
		want[f.Kind] = true
		if f.Kind == integration.FindingGrantShort &&
			(f.Subject != "integrations.expo.provisioning.access_token" || f.ActionURL != expo.MembersURL(testAccountName)) {
			t.Errorf("grant_short = %+v", f)
		}
	}
	for kind, seen := range want {
		if !seen {
			t.Errorf("the outstanding world did not report %s", kind)
		}
	}
}

// AND THE COUNTER SEES THE WRITES IT IS THERE TO SEE: a level change reaches
// Expo, and a rotation reaches the sealed store.
func TestTheExpoWriteCounterCountsWrites(t *testing.T) {
	t.Parallel()
	w := convergedExpo(t)
	before := w.mutations()
	w.sync.seat().Level = config.ExpoLevelViewer
	if _, err := w.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.mutations() <= before {
		t.Fatal("a level change made no write the counter saw")
	}
	if !slices.Contains(w.sync.expo.writes(), "CrewletRevoke") {
		t.Errorf("writes = %v, want the revoke of the permissions above viewer", w.sync.expo.writes())
	}
}
