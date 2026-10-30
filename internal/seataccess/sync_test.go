package seataccess_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/seataccess/seataccesstest"
)

var at = time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)

func yes() *bool { b := true; return &b }
func no() *bool  { b := false; return &b }

// company is one agent seat, sre, with its Datadog block, plus a human seat.
func company(block *config.RoleDatadog) *config.Company {
	return &config.Company{
		Name: "Acme",
		Roles: []config.Role{
			{Name: "SRE", Handle: "sre", Integrations: config.RoleIntegrations{Datadog: block}},
			{Name: "Founder", Handle: "founder", Kind: org.KindHuman},
		},
	}
}

type harness struct {
	v      *seataccesstest.Vendor
	ledger *seataccesstest.Ledger
	sink   *seataccesstest.Sink
	drifts []seataccess.DriftRestored
}

func newHarness() *harness {
	return &harness{
		v:      seataccesstest.NewVendor(integration.KindDatadog, "APP_KEY"),
		ledger: seataccesstest.NewLedger(),
		sink:   seataccesstest.NewSink(),
	}
}

func (h *harness) sync(t *testing.T, c *config.Company, asks map[string]seataccess.Ask) seataccess.SyncReport {
	t.Helper()
	report, err := seataccess.Sync(t.Context(), seataccess.SyncInput{
		Vendor: h.v, Open: seataccess.OpenInput{Config: c}, Ledger: h.ledger, Sink: h.sink,
		Pass: "pass-1", By: "reconcile loop", Asks: asks, Now: func() time.Time { return at },
		OnDrift: func(_ context.Context, d seataccess.DriftRestored) { h.drifts = append(h.drifts, d) },
	})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	return report
}

func (h *harness) record(t *testing.T) seataccess.Record {
	t.Helper()
	rec, ok := h.ledger.Record(integration.KindDatadog, "sre")
	if !ok {
		t.Fatal("the ledger holds no record for sre")
	}
	return rec
}

// ON CREATES THE ACCOUNT, SEALS ITS KEY UNDER THE ENGINE'S NAME, AND RECORDS
// IT; the human seat is never touched.
func TestSyncCreatesSealsAndRecords(t *testing.T) {
	t.Parallel()
	h := newHarness()
	report := h.sync(t, company(&config.RoleDatadog{Enabled: yes(), Role: "a"}), nil)
	if len(report.Findings) != 0 {
		t.Fatalf("findings = %+v, want none", report.Findings)
	}
	if _, ok := h.sink.Held("DATADOG_APP_KEY_SRE"); !ok {
		t.Fatal("nothing sealed under DATADOG_APP_KEY_SRE")
	}
	rec := h.record(t)
	if rec.Account == nil || rec.Account.State != seataccess.AccountActive || rec.Account.CreatedByPass != "pass-1" {
		t.Fatalf("account = %+v, want an active account created by pass-1", rec.Account)
	}
	if rec.Credential == nil || rec.Credential.Variable != "DATADOG_APP_KEY_SRE" || rec.Credential.By != "reconcile loop" {
		t.Fatalf("credential = %+v", rec.Credential)
	}
	lane := rec.Lanes[dd]
	if lane.Sync == nil || lane.Sync.Outcome != seataccess.OutcomeOK || lane.Applied == nil || lane.Applied.Level != "a" {
		t.Fatalf("lane = %+v", lane)
	}
	if _, ok := h.ledger.Record(integration.KindDatadog, "founder"); ok {
		t.Fatal("a human seat was recorded")
	}
	// A SECOND SYNC CHANGES NOTHING.
	before := len(h.v.Applied)
	h.sync(t, company(&config.RoleDatadog{Enabled: yes(), Role: "a"}), nil)
	if len(h.v.Applied) != before {
		t.Fatalf("a converged seat took steps: %v", h.v.Applied[before:])
	}
}

// OFF DISABLES AND KEEPS THE ACCOUNT, REVOKES ITS KEY AND CLEARS THE SEALED
// VALUE (D1).
func TestSyncOffDisablesKeepsAndClears(t *testing.T) {
	t.Parallel()
	h := newHarness()
	h.sync(t, company(&config.RoleDatadog{Enabled: yes(), Role: "a"}), nil)
	h.sync(t, company(&config.RoleDatadog{Enabled: no(), Role: "a"}), nil)
	if h.v.Account("sre") == nil {
		t.Fatal("off deleted the account")
	}
	if _, ok := h.sink.Held("DATADOG_APP_KEY_SRE"); ok {
		t.Fatal("off left the key sealed")
	}
	rec := h.record(t)
	if rec.Account.State != seataccess.AccountDisabled || rec.Credential != nil {
		t.Fatalf("record = %+v, want a disabled account with nothing sealed", rec)
	}
	// AND BACK ON needs no person: this engine disabled it.
	h.sync(t, company(&config.RoleDatadog{Enabled: yes(), Role: "a"}), nil)
	if rec := h.record(t); rec.Account.State != seataccess.AccountActive || rec.Credential == nil {
		t.Fatalf("back on: record = %+v", rec)
	}
}

// DRIFT IS RESTORED AND REPORTED (D2).
func TestSyncRestoresDriftAndReportsIt(t *testing.T) {
	t.Parallel()
	h := newHarness()
	c := company(&config.RoleDatadog{Enabled: yes(), Role: "a"})
	h.sync(t, c, nil)
	if err := h.v.Mutate("sre", func(a *seataccesstest.Account) { a.Level[dd] = "c" }); err != nil {
		t.Fatal(err)
	}
	h.sync(t, c, nil)
	if got := h.v.Account("sre").Level[dd]; got != "a" {
		t.Fatalf("level = %q, want the document's a restored", got)
	}
	if len(h.drifts) != 1 || h.drifts[0].Before.Level != "c" || h.drifts[0].After.Level != "a" {
		t.Fatalf("drifts = %+v, want one from c to a", h.drifts)
	}
}

// AN ACCOUNT DISABLED BY HAND IS NEVER TURNED BACK ON UNASKED (D3).
func TestSyncLeavesAHandDisabledAccountOff(t *testing.T) {
	t.Parallel()
	h := newHarness()
	c := company(&config.RoleDatadog{Enabled: yes(), Role: "a"})
	h.sync(t, c, nil)
	if err := h.v.Mutate("sre", func(a *seataccesstest.Account) { a.Enabled[dd] = false }); err != nil {
		t.Fatal(err)
	}
	report := h.sync(t, c, nil)
	if h.v.Account("sre").Enabled[dd] {
		t.Fatal("the sync turned a hand-disabled account back on")
	}
	if len(report.Findings) != 1 || report.Findings[0].Subject != "sre" {
		t.Fatalf("findings = %+v, want one for sre", report.Findings)
	}
	if got := h.record(t).Lanes[dd].Sync.Outcome; got != seataccess.OutcomeWaiting {
		t.Fatalf("outcome = %q, want waiting on a person", got)
	}
	h.sync(t, c, map[string]seataccess.Ask{"sre": {Reenable: true}})
	if !h.v.Account("sre").Enabled[dd] {
		t.Fatal("Turn back on did not turn it back on")
	}
}

// REMOVE IS A SEPARATE ACTION, ONLY WHEN OFF: it removes the account, clears
// the key and drops the record.
func TestSyncRemovesOnlyWhenOff(t *testing.T) {
	t.Parallel()
	h := newHarness()
	h.sync(t, company(&config.RoleDatadog{Enabled: yes(), Role: "a"}), nil)
	_, err := seataccess.Sync(t.Context(), seataccess.SyncInput{
		Vendor: h.v, Open: seataccess.OpenInput{Config: company(&config.RoleDatadog{Enabled: yes(), Role: "a"})},
		Ledger: h.ledger, Sink: h.sink, Asks: map[string]seataccess.Ask{"sre": {Remove: true}},
	})
	if !errors.Is(err, seataccess.ErrStillOn) {
		t.Fatalf("a remove while on = %v, want ErrStillOn", err)
	}
	off := company(&config.RoleDatadog{Enabled: no(), Role: "a"})
	h.sync(t, off, nil)
	h.sync(t, off, map[string]seataccess.Ask{"sre": {Remove: true}})
	if h.v.Account("sre") != nil {
		t.Fatal("remove left the account")
	}
	if _, ok := h.ledger.Record(integration.KindDatadog, "sre"); ok {
		t.Fatal("remove left the ledger record")
	}
}

// A SEAT THAT LEFT THE DOCUMENT IS OFF: the ledger reaches its account.
func TestSyncTurnsOffASeatThatLeftTheDocument(t *testing.T) {
	t.Parallel()
	h := newHarness()
	h.sync(t, company(&config.RoleDatadog{Enabled: yes(), Role: "a"}), nil)
	h.sync(t, &config.Company{Name: "Acme"}, nil)
	if acct := h.v.Account("sre"); acct == nil || acct.Enabled[dd] {
		t.Fatalf("account = %+v, want it kept and disabled", acct)
	}
	if rec := h.record(t); rec.Account.State != seataccess.AccountDisabled {
		t.Fatalf("record = %+v, want the departed seat's account recorded disabled", rec)
	}
}

// AN ACCOUNT DELETED AT THE VENDOR closes its record and is created afresh.
func TestSyncRecreatesAnAccountDeletedAtTheVendor(t *testing.T) {
	t.Parallel()
	h := newHarness()
	c := company(&config.RoleDatadog{Enabled: yes(), Role: "a"})
	h.sync(t, c, nil)
	first := h.record(t).Account.ID
	h.v.Delete("sre")
	h.sync(t, c, nil)
	rec := h.record(t)
	if rec.Account == nil || rec.Account.ID == first || rec.Account.State != seataccess.AccountActive {
		t.Fatalf("account = %+v, want a fresh one replacing %s", rec.Account, first)
	}
}

// A FAILED SEAL REVOKES WHAT WAS MINTED, for every vendor.
func TestSyncRevokesAMintItCannotSeal(t *testing.T) {
	t.Parallel()
	h := newHarness()
	h.sink.FailRecord = map[string]error{"DATADOG_APP_KEY_SRE": errors.New("store unavailable")}
	report := h.sync(t, company(&config.RoleDatadog{Enabled: yes(), Role: "a"}), nil)
	if len(h.v.Revoked) != 1 {
		t.Fatalf("revoked = %v, want the minted key revoked", h.v.Revoked)
	}
	if len(report.Findings) != 1 || !strings.Contains(report.Findings[0].Detail, "store unavailable") {
		t.Fatalf("findings = %+v, want the failed seal reported", report.Findings)
	}
	if got := h.record(t).Lanes[dd].Sync.Outcome; got != seataccess.OutcomeFailed {
		t.Fatalf("outcome = %q, want failed", got)
	}
}

// A SINK SOMETHING ELSE NAMES IS NEVER SEALED INTO, the incident's class.
func TestSyncRefusesASinkTheCompanyNames(t *testing.T) {
	t.Parallel()
	h := newHarness()
	c := company(&config.RoleDatadog{Enabled: yes(), Role: "a"})
	c.Integrations.Datadog = &config.Datadog{Enabled: true, Provisioning: &config.DatadogProvisioning{
		AppKey: "${DATADOG_APP_KEY_SRE}",
	}}
	report := h.sync(t, c, nil)
	if len(h.v.Applied) != 0 {
		t.Fatalf("applied %v, want nothing", h.v.Applied)
	}
	if len(report.Findings) != 1 || !strings.Contains(report.Findings[0].Detail, "DATADOG_APP_KEY_SRE") {
		t.Fatalf("findings = %+v, want the shared name reported", report.Findings)
	}
}

// A LEVEL THE VENDOR DOES NOT OFFER IS A FINDING, never a fallback.
func TestSyncReportsALevelTheVendorDoesNotOffer(t *testing.T) {
	t.Parallel()
	h := newHarness()
	report := h.sync(t, company(&config.RoleDatadog{Enabled: yes(), Role: "gone"}), nil)
	if len(h.v.Applied) != 0 {
		t.Fatalf("applied %v, want nothing", h.v.Applied)
	}
	if len(report.Findings) != 1 || report.Findings[0].Kind != integration.FindingUnknownTier {
		t.Fatalf("findings = %+v, want one unknown level", report.Findings)
	}
}

// A VENDOR'S REFUSAL NAMES THE STEP, ITS REASON, THE REMEDY AND WHO ACTS.
func TestSyncReportsAVendorRefusal(t *testing.T) {
	t.Parallel()
	h := newHarness()
	h.v.Refuse[seataccess.OpCreate] = &seataccess.Refusal{
		Said: "Forbidden: user_access_manage is required", Remedy: "Grant the key's user that permission",
		Actor: seataccess.ByYou, ActionURL: "https://app.datadoghq.com/organization-settings/users",
	}
	h.sync(t, company(&config.RoleDatadog{Enabled: yes(), Role: "a"}), nil)
	got := h.record(t).Lanes[dd].Sync
	if got.Outcome != seataccess.OutcomeFailed || got.Error == nil || got.Error.Step != seataccess.VerbCreate ||
		got.Error.Actor != seataccess.ByYou || got.Error.ActionURL == "" {
		t.Fatalf("sync = %+v", got)
	}
}

// A DRY RUN READS THE VENDOR AND CHANGES NOTHING.
func TestSyncWithNoSinkIsADryRun(t *testing.T) {
	t.Parallel()
	h := newHarness()
	_, err := seataccess.Sync(t.Context(), seataccess.SyncInput{
		Vendor: h.v, Open: seataccess.OpenInput{Config: company(&config.RoleDatadog{Enabled: yes(), Role: "a"})},
		Ledger: h.ledger,
	})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(h.v.Applied) != 0 {
		t.Fatalf("a dry run applied %v", h.v.Applied)
	}
}

// A DISCONNECT TURNS EVERY SEAT OFF, and removes where asked.
func TestSyncDisconnectTurnsEverySeatOff(t *testing.T) {
	t.Parallel()
	h := newHarness()
	c := company(&config.RoleDatadog{Enabled: yes(), Role: "a"})
	h.sync(t, c, nil)
	_, err := seataccess.Sync(t.Context(), seataccess.SyncInput{
		Vendor: h.v, Open: seataccess.OpenInput{Config: c}, Ledger: h.ledger, Sink: h.sink,
		Disconnect: true, Asks: map[string]seataccess.Ask{"sre": {Remove: true}},
	})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if h.v.Account("sre") != nil {
		t.Fatal("a disconnect asked to remove left the account")
	}
	if !slices.Contains(h.sink.Forgotten, "DATADOG_APP_KEY_SRE") {
		t.Fatalf("forgotten = %v, want the key cleared", h.sink.Forgotten)
	}
}

// A VENDOR THE COMPANY DOES NOT PROVISION is not configured, not a fault.
func TestSyncAnswersNotConfigured(t *testing.T) {
	t.Parallel()
	h := newHarness()
	h.v.NotConfigured = true
	_, err := seataccess.Sync(t.Context(), seataccess.SyncInput{
		Vendor: h.v, Open: seataccess.OpenInput{Config: company(nil)}, Ledger: h.ledger, Sink: h.sink,
	})
	if !errors.Is(err, integration.ErrNotConfigured) {
		t.Fatalf("Sync = %v, want ErrNotConfigured", err)
	}
}
