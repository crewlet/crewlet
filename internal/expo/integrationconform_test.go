package expo_test

import (
	"context"
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

// expoWorld is the agents' robots run through the access sync, which is all of
// an Expo pass once the account name resolves (the engine checks that part).
type expoWorld struct {
	sync *syncWorld
	sink *countingSink
}

func (*expoWorld) Kind() integration.Kind { return integration.KindExpo }

func (w *expoWorld) Reconcile(ctx context.Context) ([]integration.Finding, error) {
	report, err := seataccess.Sync(ctx, seataccess.SyncInput{
		Vendor: w.sync.vendor, Ledger: w.sync.ledger, Sink: w.sink, Pass: "test", By: "test",
		Open: w.sync.open(),
	})
	if err != nil {
		return nil, err
	}
	return report.Findings, nil
}

// mutations is every write at Expo and into the sealed store.
func (w *expoWorld) mutations() int { return len(w.sync.expo.writes()) + int(w.sink.writes.Load()) }

// convergedExpo sets two agents up at different levels the way a person does
// (a robot at Expo, its token pasted), so the question left is whether the
// next pass writes.
func convergedExpo(t *testing.T) *expoWorld {
	t.Helper()
	s := newSyncWorld(t, config.ExpoLevelDeveloper)
	on := true
	s.company.Roles = append(s.company.Roles, config.Role{Name: "SRE", Handle: "sre",
		Integrations: config.RoleIntegrations{Expo: &config.RoleExpo{Enabled: &on}}})
	s.sync()
	_, qa := s.expo.robot("crewlet-acme-qa", expo.RoleDeveloper)
	_, sre := s.expo.robot("crewlet-acme-sre", expo.RoleViewOnly)
	for handle, tok := range map[string]string{"qa": qa.value, "sre": sre.value} {
		if _, err := s.pasteFor(handle, tok); err != nil {
			t.Fatalf("paste for %s: %v", handle, err)
		}
	}
	w := &expoWorld{sync: s, sink: &countingSink{Sink: s.sink}}
	if findings, err := w.Reconcile(context.Background()); err != nil || len(findings) != 0 {
		t.Fatalf("seeding pass = %+v, %v", findings, err)
	}
	return w
}

// outstandingExpo is a converged world where a person has to act three times:
// qa's token was revoked, sre's robot was given another role at Expo, and a
// new agent has no robot yet.
func outstandingExpo(t *testing.T) *expoWorld {
	t.Helper()
	w := convergedExpo(t)
	on := true
	w.sync.company.Roles = append(w.sync.company.Roles, config.Role{Name: "Release", Handle: "release",
		Integrations: config.RoleIntegrations{Expo: &config.RoleExpo{Enabled: &on}}})
	sealed, _ := w.sync.sink.Held(sealedQA)
	w.sync.expo.mu.Lock()
	tok := w.sync.expo.tokens[sealed]
	w.sync.expo.mu.Unlock()
	w.sync.expo.revoke(tok)
	sre, _ := w.sync.ledger.Record(integration.KindExpo, "sre")
	w.sync.expo.setRole(sre.Account.ID, expo.RoleRunner)
	return w
}

// THE CONTRACT IS CERTIFIED AGAINST THE REAL SYNC, OVER BOTH WORLDS.
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

// AND THE OUTSTANDING WORLD REPORTS EVERY FAULT IT HOLDS, each on the seat a
// person acts for, pointing at the account's access tokens page.
func TestTheOutstandingExpoWorldReportsEveryFault(t *testing.T) {
	t.Parallel()
	findings, err := outstandingExpo(t).Reconcile(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"qa": false, "sre": false, "release": false}
	for _, f := range findings {
		if _, ok := want[f.Subject]; !ok || f.Kind != integration.FindingApprovalRequired ||
			f.ActionURL != expo.AccessTokensURL(testAccountName) {
			t.Errorf("unexpected finding %+v", f)
			continue
		}
		want[f.Subject] = true
	}
	for handle, seen := range want {
		if !seen {
			t.Errorf("the outstanding world did not report seat %s", handle)
		}
	}
}

// AND THE COUNTER SEES THE WRITES IT IS THERE TO SEE: turning an agent off
// clears its token from the sealed store.
func TestTheExpoWriteCounterCountsWrites(t *testing.T) {
	t.Parallel()
	w := convergedExpo(t)
	before := w.mutations()
	off := false
	w.sync.seat().Enabled = &off
	if _, err := w.Reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if w.mutations() <= before {
		t.Fatal("turning an agent off made no write the counter saw")
	}
	if _, ok := w.sync.sink.Held(sealedQA); ok {
		t.Errorf("%s is still sealed after the agent was turned off", sealedQA)
	}
}
