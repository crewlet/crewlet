package seataccess_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/seataccess/seataccesstest"
)

// manyProjects is n project ids of size bytes each.
func manyProjects(n, size int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("p%0*d", size-1, i)
	}
	return out
}

// A LANE IS SIZED IN ITS VENDOR'S WORDS: 300 projects with six-byte ids, each
// of which the vendor names in a kilobyte, fit the ledger record by their ids
// and not by their names, which the record keeps. The lane is held before any
// vendor step, since a step whose record cannot be kept is one the next pass
// cannot tell from a change made by hand.
//
// Mutation: size the lane without its vendor's words (fits' words as nil),
// and the account is created.
func TestALaneIsSizedInItsVendorsWords(t *testing.T) {
	t.Parallel()
	projects := manyProjects(300, 6)
	names := map[string]string{}
	for _, p := range projects {
		names[p] = strings.Repeat("n", 1000) + p
	}
	v := seataccesstest.NewVendor(integration.KindGitLab, "TOKEN")
	v.Levels[seataccess.LaneGitLab] = []seataccess.Level{{ID: "30", Name: "Developer"}}
	v.Scope[seataccess.LaneGitLab] = projects
	v.ScopeNames = map[seataccess.Lane]map[string]string{seataccess.LaneGitLab: names}
	ledger := seataccesstest.NewLedger()
	c := &config.Company{Name: "Acme", Roles: []config.Role{{Name: "SRE", Handle: "sre",
		Integrations: config.RoleIntegrations{GitLab: &config.RoleGitLab{Enabled: yes(), AccessLevel: 30,
			Projects: projects}}}}}
	if _, err := seataccess.Sync(t.Context(), seataccess.SyncInput{
		Vendor: v, Open: seataccess.OpenInput{Config: c}, Ledger: ledger, Sink: seataccesstest.NewSink(),
		Pass: "pass-1", By: "reconcile loop", Now: func() time.Time { return at },
	}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if len(v.Applied) != 0 {
		t.Fatalf("the vendor was asked %v for a lane whose record cannot be kept", v.Applied)
	}
	rec, _ := ledger.Record(integration.KindGitLab, "sre")
	if got := rec.Lanes[seataccess.LaneGitLab].Sync; got == nil || got.Error == nil ||
		got.Error.Step != seataccess.VerbScope {
		t.Fatalf("the lane's sync = %+v, want a scope failure", got)
	}
}

// A HELD LANE HOLDS ONLY ITSELF: an Atlassian seat whose Jira scope outgrows
// its record while Confluence is turned off still has its Confluence access
// taken away, since taking access away is what a person asked for, and only
// Jira carries the failure.
//
// Mutation: hold the whole seat when one lane does not fit, and Confluence
// stays on at the vendor.
func TestAHeldLaneHoldsOnlyItself(t *testing.T) {
	t.Parallel()
	projects := manyProjects(5000, 60)
	v := seataccesstest.NewVendor(integration.KindAtlassian, "TOKEN", "EMAIL")
	v.Scope[seataccess.LaneJira] = projects
	ledger := seataccesstest.NewLedger()
	sync := func(jira *config.RoleJira, confluence *bool) {
		t.Helper()
		c := &config.Company{Name: "Acme", Roles: []config.Role{{Name: "SRE", Handle: "sre",
			Integrations: config.RoleIntegrations{Jira: jira,
				Confluence: &config.RoleConfluence{Enabled: confluence, SpaceRole: "a"}}}}}
		if _, err := seataccess.Sync(t.Context(), seataccess.SyncInput{
			Vendor: v, Open: seataccess.OpenInput{Config: c}, Ledger: ledger, Sink: seataccesstest.NewSink(),
			Pass: "pass-1", By: "reconcile loop", Now: func() time.Time { return at },
		}); err != nil {
			t.Fatalf("Sync: %v", err)
		}
	}
	sync(&config.RoleJira{Enabled: yes(), ProjectRole: "a", Projects: projects[:1]}, yes())
	if acct := v.Account("sre"); acct == nil || !acct.Enabled[seataccess.LaneConfluence] {
		t.Fatalf("premise: the first sync left Confluence off: %+v", acct)
	}
	before := len(v.Applied)
	sync(&config.RoleJira{Enabled: yes(), ProjectRole: "a", Projects: projects}, no())
	took := v.Applied[before:]
	for _, step := range took {
		if strings.HasPrefix(step, "sre/jira/") {
			t.Errorf("Jira, whose record cannot be kept, took %s", step)
		}
	}
	if v.Account("sre").Enabled[seataccess.LaneConfluence] {
		t.Fatalf("Confluence is still on at the vendor after it was turned off (took %v)", took)
	}
	rec, _ := ledger.Record(integration.KindAtlassian, "sre")
	if got := rec.Lanes[seataccess.LaneJira].Sync; got == nil || got.Error == nil || got.Error.Step != seataccess.VerbScope {
		t.Errorf("Jira's sync = %+v, want its scope failure", got)
	}
	if got := rec.Lanes[seataccess.LaneConfluence].Sync; got == nil || got.Outcome != seataccess.OutcomeOK {
		t.Errorf("Confluence's sync = %+v, want it taken", got)
	}
}

// A VENDOR'S LONG REFUSAL IS KEPT AT ITS HEAD: a refusal of 100,000 bytes,
// each one JSON writes as six, is recorded cut and marked within
// MaxSeatAccessTextBytes as encoded, so the seat's record stays within its
// class and the store keeps it.
//
// Mutation: put the record without holding its words to their bounds, and the
// store refuses it, so the seat's row reads that its record outgrew the ledger
// rather than what the vendor said.
func TestAVendorsLongRefusalIsKeptAtItsHead(t *testing.T) {
	t.Parallel()
	ledger, err := seataccess.NewCoordLedger(memory.NewFleet())
	if err != nil {
		t.Fatal(err)
	}
	v := seataccesstest.NewVendor(integration.KindDatadog, "APP_KEY")
	long := strings.Repeat("<", 100_000)
	v.Refuse[seataccess.OpCreate] = &seataccess.Refusal{Said: long, Remedy: long, Actor: seataccess.ByYou}
	if _, err := seataccess.Sync(t.Context(), seataccess.SyncInput{
		Vendor: v, Open: seataccess.OpenInput{Config: company(&config.RoleDatadog{Enabled: yes(), Role: "a"})},
		Ledger: ledger, Sink: seataccesstest.NewSink(), Pass: "pass-1", By: "reconcile loop",
		Now: func() time.Time { return at },
	}); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	rec, found, err := ledger.SeatRecord(t.Context(), integration.KindDatadog, "sre")
	if err != nil || !found {
		t.Fatalf("the seat's record: %v, %v", found, err)
	}
	f := rec.Lanes[dd].Sync.Error
	if f == nil || !strings.HasPrefix(f.Said, "<<<") || !strings.HasPrefix(f.Remedy, "<<<") {
		t.Fatalf("the recorded failure = %.120q, want the vendor's sentence at its head", f)
	}
	for field, text := range map[string]string{"said": f.Said, "remedy": f.Remedy} {
		encoded, err := json.Marshal(text)
		if err != nil {
			t.Fatal(err)
		}
		if len(encoded)-2 > seataccess.MaxSeatAccessTextBytes || len(text) == len(long) {
			t.Errorf("its %s is kept as %d bytes encoded, want it cut within %d", field, len(encoded)-2,
				seataccess.MaxSeatAccessTextBytes)
		}
	}
}

// refusingFirst is a ledger whose first write of one seat's record is refused
// for its size.
type refusingFirst struct {
	*seataccesstest.Ledger
	handle  string
	mu      sync.Mutex
	refused bool
}

func (l *refusingFirst) Put(ctx context.Context, rec seataccess.Record) error {
	l.mu.Lock()
	refuse := rec.Handle == l.handle && !l.refused
	l.refused = l.refused || refuse
	l.mu.Unlock()
	if refuse {
		return fmt.Errorf("%w: %s's record", coord.ErrValueTooLarge, rec.Handle)
	}
	return l.Ledger.Put(ctx, rec)
}

// A RECORD PAST ITS CLASS FAILS ITS SEAT, NEVER THE PASS: the store refusing
// one seat's record for its size keeps the record the pass read with that
// failure on it, and the kind's next seat is still synced.
//
// Mutation: return the refusal as the pass's error, and the next seat is never
// reached.
func TestARecordPastItsClassFailsItsSeatNotThePass(t *testing.T) {
	t.Parallel()
	ledger := &refusingFirst{Ledger: seataccesstest.NewLedger(), handle: "aaa"}
	v := seataccesstest.NewVendor(integration.KindDatadog, "APP_KEY")
	block := &config.RoleDatadog{Enabled: yes(), Role: "a"}
	c := &config.Company{Name: "Acme", Roles: []config.Role{
		{Name: "A", Handle: "aaa", Integrations: config.RoleIntegrations{Datadog: block}},
		{Name: "B", Handle: "bbb", Integrations: config.RoleIntegrations{Datadog: block}},
	}}
	report, err := seataccess.Sync(t.Context(), seataccess.SyncInput{
		Vendor: v, Open: seataccess.OpenInput{Config: c}, Ledger: ledger, Sink: seataccesstest.NewSink(),
		Pass: "pass-1", By: "reconcile loop", Now: func() time.Time { return at },
	})
	if err != nil {
		t.Fatalf("Sync: %v, want the refusal kept to its seat", err)
	}
	next, ok := ledger.Record(integration.KindDatadog, "bbb")
	if !ok || next.Lanes[dd].Sync == nil || next.Lanes[dd].Sync.Outcome != seataccess.OutcomeOK {
		t.Fatalf("the next seat's record = %+v, %v, want it synced", next, ok)
	}
	refused, ok := ledger.Record(integration.KindDatadog, "aaa")
	if f := refused.Lanes[dd].Sync; !ok || f == nil || f.Error == nil || !strings.Contains(f.Error.Said, "outgrew") {
		t.Fatalf("the refused seat's record = %+v, want its failure", refused)
	}
	if !strings.Contains(fmt.Sprint(report.Findings), "outgrew") {
		t.Errorf("findings = %+v, want the refused seat's", report.Findings)
	}
}
