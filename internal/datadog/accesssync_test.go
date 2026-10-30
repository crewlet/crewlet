package datadog_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/datadog"
	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/seataccess/seataccesstest"
)

// regionVendor is the registered Datadog vendor, opened against a fake region.
type regionVendor struct {
	seataccess.Vendor
	hc *http.Client
}

func (v regionVendor) Open(ctx context.Context, in seataccess.OpenInput) (seataccess.Session, error) {
	return datadog.OpenAccess(ctx, v.Vendor, in, v.hc)
}

// syncWorld is one company, its Datadog organization, the ledger and the
// sealed store, run through the shared sync exactly as the loop runs it.
type syncWorld struct {
	t       *testing.T
	org     *accessOrg
	vendor  seataccess.Vendor
	company *config.Company
	ledger  *seataccesstest.Ledger
	sink    *seataccesstest.Sink
	drift   []seataccess.DriftRestored
}

func newSyncWorld(t *testing.T, role string) *syncWorld {
	t.Helper()
	org := newAccessOrg()
	server := httptest.NewServer(http.HandlerFunc(org.serve))
	t.Cleanup(server.Close)
	registered, _ := seataccess.Lookup(integration.KindDatadog)
	on := true
	company := &config.Company{}
	company.Integrations.Datadog = &config.Datadog{
		Enabled: true,
		Provisioning: &config.DatadogProvisioning{
			Site: "datadoghq.com", APIKey: "${DD_API}", AppKey: "${DD_APP}",
		},
	}
	company.Roles = []config.Role{{
		Name: "SRE", Handle: "sre",
		Integrations: config.RoleIntegrations{
			Datadog: &config.RoleDatadog{Enabled: &on, Role: role},
		},
	}}
	return &syncWorld{
		t: t, org: org, company: company,
		vendor: regionVendor{Vendor: registered, hc: &http.Client{
			Transport: httpxtest.Rewrite(t, server),
		}},
		ledger: seataccesstest.NewLedger(), sink: seataccesstest.NewSink(),
	}
}

func (w *syncWorld) seat() *config.RoleDatadog { return w.company.Roles[0].Integrations.Datadog }

func (w *syncWorld) sync() seataccess.SyncReport {
	w.t.Helper()
	report, err := seataccess.Sync(context.Background(), seataccess.SyncInput{
		Vendor: w.vendor, Ledger: w.ledger, Sink: w.sink, Pass: "test", By: "test",
		Open: seataccess.OpenInput{
			Config: w.company,
			Env:    config.NewResolver(config.MapSource{"DD_API": "fake-api", "DD_APP": "fake-app"}),
		},
		OnDrift: func(_ context.Context, d seataccess.DriftRestored) { w.drift = append(w.drift, d) },
	})
	if err != nil {
		w.t.Fatalf("sync: %v", err)
	}
	return report
}

func (w *syncWorld) record() seataccess.Record {
	w.t.Helper()
	rec, ok := w.ledger.Record(integration.KindDatadog, "sre")
	if !ok {
		w.t.Fatal("the ledger has no record for sre")
	}
	return rec
}

// A SEAT'S WHOLE LIFE THROUGH THE SHARED SYNC: on creates the account with its
// role and seals its key under the engine's name; a converged pass writes
// nothing; a role added by hand is restored and reported; off disables the
// account, deletes its keys and clears the sealed value; back on enables it
// and seals a fresh key.
func TestADatadogSeatThroughTheSync(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, stdRole)

	if report := w.sync(); len(report.Findings) != 0 {
		t.Fatalf("turning on reported %+v", report.Findings)
	}
	rec := w.record()
	if !rec.Held() || rec.Credential == nil || rec.Credential.Variable != "DATADOG_APP_KEY_SRE" {
		t.Fatalf("record = %+v", rec)
	}
	account := w.org.user(rec.Account.ID)
	if !slices.Equal(account.roles, []string{stdRole}) || account.email != "crewlet-sre@agents.crewlet.invalid" {
		t.Errorf("account = %+v", account)
	}
	sealed, ok := w.sink.Held("DATADOG_APP_KEY_SRE")
	keys := w.org.keyIDs(rec.Account.ID)
	if !ok || sealed == "" || len(keys) != 1 {
		t.Fatalf("sealed=%v keys=%q, want one key sealed under the engine's name", ok, keys)
	}

	writes := len(w.org.wrote())
	if report := w.sync(); len(report.Findings) != 0 {
		t.Errorf("a converged pass reported %+v", report.Findings)
	}
	if len(w.org.wrote()) != writes {
		t.Errorf("a converged pass wrote %q", w.org.wrote()[writes:])
	}

	// DRIFT: somebody adds the account to Datadog Admin Role by hand.
	w.org.mu.Lock()
	w.org.users[rec.Account.ID].roles = append(w.org.users[rec.Account.ID].roles, adminRole)
	w.org.mu.Unlock()
	w.sync()
	if got := w.org.user(rec.Account.ID).roles; !slices.Equal(got, []string{stdRole}) {
		t.Errorf("roles after drift = %q, want the seat's role restored", got)
	}
	if len(w.drift) != 1 || w.drift[0].After.Level != stdRole {
		t.Errorf("drift events = %+v, want one restoring %s", w.drift, stdRole)
	}

	// OFF.
	off := false
	w.seat().Enabled = &off
	w.sync()
	account = w.org.user(rec.Account.ID)
	if !account.disabled || account.title != datadog.DisconnectedTitle {
		t.Errorf("account after off = %+v, want disabled and marked", account)
	}
	if keys := w.org.keyIDs(rec.Account.ID); len(keys) != 0 {
		t.Errorf("keys after off = %q", keys)
	}
	if _, ok := w.sink.Held("DATADOG_APP_KEY_SRE"); ok {
		t.Error("the sealed key outlived the seat being turned off")
	}
	if rec := w.record(); rec.Account.State != seataccess.AccountDisabled {
		t.Errorf("ledger state = %q, want disabled by this engine", rec.Account.State)
	}

	// BACK ON.
	on := true
	w.seat().Enabled = &on
	w.sync()
	account = w.org.user(rec.Account.ID)
	if account.disabled || account.title != "" || len(w.org.keyIDs(rec.Account.ID)) != 1 {
		t.Errorf("account after back on = %+v", account)
	}
	if _, ok := w.sink.Held("DATADOG_APP_KEY_SRE"); !ok {
		t.Error("no key sealed after turning the seat back on")
	}
}

// A KEY THE SYNC CANNOT SEAL IS DELETED AT DATADOG (design 5.2), and the seat
// reports the failure rather than holding a live key nobody has.
func TestADatadogKeyThatCannotBeSealedIsRevoked(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, roRole)
	w.sink.FailRecord = map[string]error{"DATADOG_APP_KEY_SRE": errors.New("the store refused")}
	report := w.sync()
	rec := w.record()
	if !rec.Held() {
		t.Fatalf("record = %+v, want the created account recorded", rec)
	}
	if keys := w.org.keyIDs(rec.Account.ID); len(keys) != 0 {
		t.Errorf("keys = %q, want the unsealed key revoked", keys)
	}
	if len(report.Findings) != 1 || report.Findings[0].Kind != integration.FindingIdentityFailed {
		t.Errorf("findings = %+v, want the seat's failure", report.Findings)
	}
}

// AN ACCOUNT SOMEBODY ELSE DISABLED IS NEVER TURNED BACK ON BY A PASS (D3): it
// waits for a person, and nothing is written at Datadog.
func TestADatadogAccountDisabledByHandWaits(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, roRole)
	w.sync()
	rec := w.record()
	w.org.mu.Lock()
	w.org.users[rec.Account.ID].disabled = true
	w.org.mu.Unlock()
	writes := len(w.org.wrote())
	w.sync()
	if account := w.org.user(rec.Account.ID); !account.disabled {
		t.Error("an account disabled by hand was turned back on")
	}
	if got := w.org.wrote(); len(got) != writes {
		t.Errorf("writes = %q, want none", got[writes:])
	}
	if sync := w.record().Lanes[seataccess.LaneDatadog].Sync; sync == nil ||
		sync.Outcome != seataccess.OutcomeWaiting {
		t.Errorf("sync = %+v, want waiting for a person", sync)
	}
}

// A KEY DELETED AT DATADOG IS REPLACED. The ledger still records the sealed
// key, and the account holds none, so the sealed value answers 403 for ever:
// the next sync mints and seals a fresh one rather than reporting the seat in
// sync.
func TestADatadogKeyDeletedAtDatadogIsReMinted(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, roRole)
	w.sync()
	rec := w.record()
	before, _ := w.sink.Held("DATADOG_APP_KEY_SRE")
	w.org.mu.Lock()
	w.org.keys[rec.Account.ID] = nil
	w.org.mu.Unlock()

	if report := w.sync(); len(report.Findings) != 0 {
		t.Fatalf("findings = %+v", report.Findings)
	}
	after, ok := w.sink.Held("DATADOG_APP_KEY_SRE")
	if !ok || after == before || len(w.org.keyIDs(rec.Account.ID)) != 1 {
		t.Errorf("sealed=%v changed=%v keys=%q, want a fresh key sealed",
			ok, after != before, w.org.keyIDs(rec.Account.ID))
	}
}
