package seataccess_test

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/seataccess/seataccesstest"
)

// A KEY IS WITHIN REACH IN ANY CASE ITS VENDOR IGNORES: a lower-case Jira or
// Confluence key is the upper-case one the connection lists, a Confluence
// personal space's key is matched exactly, and an open reach takes anything.
func TestAKeyIsWithinReachAsItsVendorMatchesIt(t *testing.T) {
	t.Parallel()
	jira := seataccess.Options{Lane: seataccess.LaneJira, Scope: []string{"ENG", "OPS"}}
	conf := seataccess.Options{Lane: seataccess.LaneConfluence, Scope: []string{"ENG", "~alice"}}
	for _, tc := range []struct {
		opts  seataccess.Options
		entry string
		want  bool
	}{
		{jira, "eng", true},
		{jira, " Ops ", true},
		{jira, "web", false},
		{jira, "", false},
		{conf, "eng", true},
		{conf, "~alice", true},
		{conf, "~Alice", false},
		{seataccess.Options{Lane: seataccess.LaneGitHub, ScopeOpen: true}, "acme/anything", true},
	} {
		if got := tc.opts.Reaches(tc.entry); got != tc.want {
			t.Errorf("%s: Reaches(%q) = %v, want %v", tc.opts.Lane, tc.entry, got, tc.want)
		}
	}
	d := seataccess.Desired{Scope: []string{"eng", "ENG", " ~Bob "}}.NormalizedFor(seataccess.LaneConfluence)
	if !slices.Equal(d.Scope, []string{"ENG", "~Bob"}) {
		t.Errorf("NormalizedFor = %v", d.Scope)
	}
	if got := seataccess.ScopeKey(seataccess.LaneMattermost, " Town-Square "); got != "Town-Square" {
		t.Errorf("another lane's entry = %q, want it trimmed and otherwise as written", got)
	}
}

// THE DOCUMENT IS READ AND WRITTEN IN THE VENDOR'S SPELLING, so the access a
// save wrote and the access a pass holds the vendor to are one setting.
func TestTheDocumentIsReadAndWrittenInTheVendorsSpelling(t *testing.T) {
	t.Parallel()
	role := &config.Role{Integrations: config.RoleIntegrations{
		Jira: &config.RoleJira{Enabled: yes(), ProjectRole: "a", Projects: []string{"eng"}},
	}}
	if d, _ := seataccess.DesiredOf(role, seataccess.LaneJira); !slices.Equal(d.Scope, []string{"ENG"}) {
		t.Fatalf("read = %v", d.Scope)
	}
	err := seataccess.SetDesired(role, seataccess.LaneConfluence,
		seataccess.Desired{Enabled: true, Level: "a", Scope: []string{"eng", "~Bob"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := role.Integrations.Confluence.Spaces; !slices.Equal(got, []string{"ENG", "~Bob"}) {
		t.Fatalf("written = %v", got)
	}
}

// keysSync syncs a seat's Jira block against v and ledger, as one pass.
func keysSync(t *testing.T, v seataccess.Vendor, ledger seataccess.Ledger, role config.Role) seataccess.SyncReport {
	t.Helper()
	role.Name, role.Handle = "SRE", "sre"
	report, err := seataccess.Sync(t.Context(), seataccess.SyncInput{
		Vendor: v, Open: seataccess.OpenInput{Config: &config.Company{Name: "Acme", Roles: []config.Role{role}}},
		Ledger: ledger, Sink: seataccesstest.NewSink(), Pass: "pass-1", By: "reconcile loop",
		Now: func() time.Time { return time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC) },
	})
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	return report
}

// A LOWER-CASE KEY IS WITHIN REACH AND IS NEVER DRIFT: the first pass grants
// it, and the next finds the vendor's upper-case key the same and does
// nothing.
func TestALowerCaseKeyIsWithinReachAndIsNeverDrift(t *testing.T) {
	t.Parallel()
	v := seataccesstest.NewVendor(integration.KindAtlassian, "TOKEN", "EMAIL")
	v.Scope[seataccess.LaneJira] = []string{"ENG", "OPS"}
	ledger := seataccesstest.NewLedger()
	role := config.Role{Integrations: config.RoleIntegrations{
		Jira: &config.RoleJira{Enabled: yes(), ProjectRole: "a", Projects: []string{"eng"}},
	}}
	if report := keysSync(t, v, ledger, role); len(report.Findings) != 0 {
		t.Fatalf("first pass findings = %+v", report.Findings)
	}
	if len(v.Applied) == 0 || !strings.HasPrefix(v.Applied[0], "sre/jira/create/") {
		t.Fatalf("applied = %v, want the account created", v.Applied)
	}
	before := len(v.Applied)
	if report := keysSync(t, v, ledger, role); len(report.Findings) != 0 {
		t.Fatalf("second pass findings = %+v", report.Findings)
	}
	if again := v.Applied[before:]; len(again) != 0 {
		t.Fatalf("the second pass applied %v, want nothing", again)
	}
}

// A DRAFT OR A RECORD IN ANOTHER CASE IS NOT DRIFT: the preview of a draft
// typed in lower case, against a record written before keys were spelled as
// the vendor matches them, plans nothing on an account holding the key.
func TestAKeysCaseAloneIsNeverPlannedAsAChange(t *testing.T) {
	t.Parallel()
	old := seataccess.Desired{Enabled: true, Level: "a", Scope: []string{"eng"}}
	plan, err := seataccess.Plan(seataccesstest.NewVendor(integration.KindAtlassian, "TOKEN", "EMAIL"),
		seataccess.SeatInput{
			Seat: seataccess.SeatRef{Handle: "sre"},
			Want: map[seataccess.Lane]seataccess.Desired{seataccess.LaneJira: old},
			Have: map[seataccess.Lane]seataccess.Observed{seataccess.LaneJira: {
				Exists: true, Enabled: true, Level: "a", Scope: []string{"ENG"},
				Credential: &seataccess.Credential{Variable: "ATLASSIAN_TOKEN_SRE"},
			}},
			Record: &seataccess.Record{
				Account: &seataccess.Account{ID: "acct-1", State: seataccess.AccountActive},
				Lanes:   map[seataccess.Lane]seataccess.LaneRecord{seataccess.LaneJira: {Applied: &old}},
			},
		})
	if err != nil {
		t.Fatal(err)
	}
	if steps := plan[seataccess.LaneJira]; len(steps) != 0 {
		t.Fatalf("steps = %+v, want none", steps)
	}
}

// requiringVendor is the fake vendor whose Confluence lane takes a set of
// levels, every one of which needs "a".
type requiringVendor struct{ *seataccesstest.Vendor }

func (v requiringVendor) Open(ctx context.Context, in seataccess.OpenInput) (seataccess.Session, error) {
	s, err := v.Vendor.Open(ctx, in)
	return requiringSession{s}, err
}

type requiringSession struct{ seataccess.Session }

func (s requiringSession) Options(ctx context.Context, lane seataccess.Lane) (seataccess.Options, error) {
	o, err := s.Session.Options(ctx, lane)
	if lane == seataccess.LaneConfluence {
		o.Multiple, o.RequiredLevel = true, "a"
	}
	return o, err
}

// A SET WITHOUT THE LEVEL EVERY OTHER NEEDS IS A FINDING naming that level by
// the vendor's name, and nothing is asked of the vendor; with it, the set is
// granted.
func TestASetWithoutTheRequiredLevelIsAFinding(t *testing.T) {
	t.Parallel()
	opts := seataccess.Options{Multiple: true, RequiredLevel: "a",
		Levels: []seataccess.Level{productOnlyLevel, {ID: "a", Name: "View"}, {ID: "b"}}}
	if l, missing := opts.MissingRequired("b"); !missing || l.Name != "View" {
		t.Fatalf("MissingRequired(b) = %+v, %v", l, missing)
	}
	for _, level := range []string{"a b", "a", ""} {
		if _, missing := opts.MissingRequired(level); missing {
			t.Fatalf("MissingRequired(%q) reported a set that needs nothing more", level)
		}
	}
	if _, missing := (seataccess.Options{RequiredLevel: "a"}).MissingRequired("b"); missing {
		t.Fatal("a single-level lane reported a missing set member")
	}

	for _, tc := range []struct {
		ops     []string
		finding bool
	}{{[]string{"b"}, true}, {[]string{"a", "b"}, false}} {
		v := requiringVendor{seataccesstest.NewVendor(integration.KindAtlassian, "TOKEN", "EMAIL")}
		role := config.Role{Integrations: config.RoleIntegrations{
			Confluence: &config.RoleConfluence{Enabled: yes(), Operations: tc.ops, Spaces: []string{"x"}},
		}}
		report := keysSync(t, v, seataccesstest.NewLedger(), role)
		if !tc.finding {
			if len(report.Findings) != 0 || len(v.Applied) == 0 {
				t.Fatalf("%v: findings %+v, applied %v", tc.ops, report.Findings, v.Applied)
			}
			continue
		}
		if len(report.Findings) != 1 || !strings.Contains(report.Findings[0].Detail, "without A") ||
			len(v.Applied) != 0 {
			t.Fatalf("%v: findings %+v, applied %v", tc.ops, report.Findings, v.Applied)
		}
	}
}
