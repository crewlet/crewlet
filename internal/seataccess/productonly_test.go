package seataccess_test

import (
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/seataccess/seataccesstest"
)

// productOnlyLevel is the empty level a vendor offers as a level of its own.
var productOnlyLevel = seataccess.Level{ID: "", Name: "Product access only"}

// THE EMPTY LEVEL IS A LEVEL ONLY WHERE IT IS OFFERED, on a single-level lane
// and on a set lane alike.
func TestTheEmptyLevelIsOfferedOnlyWhereTheVendorListsIt(t *testing.T) {
	t.Parallel()
	without := seataccess.Options{Levels: []seataccess.Level{{ID: "a"}}}
	if _, ok := without.LevelsOf(""); ok {
		t.Fatal("an empty level read as offered where the vendor lists none")
	}
	for _, multiple := range []bool{false, true} {
		with := seataccess.Options{Multiple: multiple, Levels: []seataccess.Level{productOnlyLevel, {ID: "a"}}}
		levels, ok := with.LevelsOf("")
		if !ok || len(levels) != 1 || levels[0].Name != "Product access only" {
			t.Fatalf("multiple %v: LevelsOf(\"\") = %+v, %v", multiple, levels, ok)
		}
	}
}

// atlassianSync syncs one agent seat's Jira block against a fake Atlassian
// vendor that offers product access only.
func atlassianSync(t *testing.T, block *config.RoleJira) (*seataccesstest.Vendor, seataccess.SyncReport) {
	t.Helper()
	v := seataccesstest.NewVendor(integration.KindAtlassian, "TOKEN", "EMAIL")
	v.Levels[seataccess.LaneJira] = append([]seataccess.Level{productOnlyLevel}, v.Levels[seataccess.LaneJira]...)
	c := &config.Company{Name: "Acme", Roles: []config.Role{{
		Name: "SRE", Handle: "sre", Integrations: config.RoleIntegrations{Jira: block},
	}}}
	report, err := seataccess.Sync(t.Context(), seataccess.SyncInput{
		Vendor: v, Open: seataccess.OpenInput{Config: c}, Ledger: seataccesstest.NewLedger(),
		Sink: seataccesstest.NewSink(), Pass: "pass-1", By: "reconcile loop",
		Now: func() time.Time { return time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	return v, report
}

// PRODUCT ACCESS ONLY IS A SETTING THE SYNC APPLIES, not an unset level: the
// account is created with no role and nothing is reported.
func TestTheSyncAppliesProductAccessOnly(t *testing.T) {
	t.Parallel()
	v, report := atlassianSync(t, &config.RoleJira{Enabled: yes()})
	if len(report.Findings) != 0 {
		t.Fatalf("findings = %+v, want none", report.Findings)
	}
	if len(v.Applied) == 0 || !strings.HasPrefix(v.Applied[0], "sre/jira/create/") {
		t.Fatalf("applied = %v, want the account created", v.Applied)
	}
}

// AND IT HOLDS NO LIST: a stored document naming projects beside it is a
// finding, and nothing is asked of the vendor for that lane.
func TestProductAccessOnlyWithAListIsAFinding(t *testing.T) {
	t.Parallel()
	v, report := atlassianSync(t, &config.RoleJira{Enabled: yes(), Projects: []string{"x"}})
	if len(v.Applied) != 0 {
		t.Fatalf("applied = %v, want nothing", v.Applied)
	}
	if len(report.Findings) != 1 || !strings.Contains(report.Findings[0].Detail, "product access only") {
		t.Fatalf("findings = %+v, want the list refused", report.Findings)
	}
}
