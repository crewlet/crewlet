package seataccess_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/seataccess/seataccesstest"
	"github.com/crewlet/crewlet/internal/seatname"
	"github.com/crewlet/crewlet/internal/vendortext"
)

const standingLane = seataccess.LaneGooglePlay

var standingSpec = seataccess.IdentitySpec{Ways: []seataccess.IdentityWay{seataccess.WayInvited, seataccess.WayMade},
	Default: seataccess.IdentityOwn, Name: seatname.Google, Standing: true}

// standingVendor is a lane whose identity Crewlet made for every seat, which
// a person adds at the product (added) or Crewlet adds when asked.
type standingVendor struct {
	added   bool
	checks  int
	dropped int
	// droppedScope is the scope the last drop was asked to take it off.
	droppedScope []string
}

// IdentityGuide names the identity by the rule, which a company rename can
// leave behind the identity that exists.
func (*standingVendor) IdentityGuide(*config.Company, seataccess.SeatRef, seataccess.Lane,
	seataccess.Desired) seataccess.IdentityGuide {
	return seataccess.IdentityGuide{Vendor: "Google Play", Noun: "Google identity", Name: "crewlet-growth-ffffff",
		Steps: []seataccess.GuideStep{{Text: vendortext.Plain("Email address:"), Copy: &seataccess.GuideCopy{Label: "email",
			Value: "crewlet-growth-ffffff"}}},
		Entries: []seataccess.IdentityEntry{{ID: "com.acme.app"}},
		Wider:   &seataccess.GuideWider{Label: "Add it once to the developer account"}}
}

func (*standingVendor) Kind() integration.Kind { return integration.KindGooglePlay }

func (*standingVendor) Catalog() seataccess.Catalog {
	return seataccess.Catalog{Kind: integration.KindGooglePlay, Lanes: []seataccess.LaneSpec{{
		Lane: standingLane, Levels: true, CreatedBy: seataccess.ByCrewlet, EnabledBy: seataccess.ByCrewlet,
		EngineHeld: true, Identity: standingSpec,
	}}}
}

func (*standingVendor) Steps(seataccess.SeatRef, seataccess.Lane, seataccess.Op, seataccess.Desired,
	seataccess.Observed) []seataccess.Step {
	return nil
}

func (v *standingVendor) Open(context.Context, seataccess.OpenInput) (seataccess.Session, error) {
	return v, nil
}

func (*standingVendor) Options(_ context.Context, lane seataccess.Lane) (seataccess.Options, error) {
	return seataccess.Options{Lane: lane, Levels: []seataccess.Level{{ID: "read", Name: "Read"}}}, nil
}

func (*standingVendor) Observe(_ context.Context, in seataccess.ObserveInput) (seataccess.Observation, error) {
	out := seataccess.Observation{}
	for _, h := range in.Handles() {
		out[h] = map[seataccess.Lane]seataccess.Observed{standingLane: {Exists: true, Enabled: true, Level: "read",
			Scope: []string{"com.acme.app"}}}
	}
	return out, nil
}

func (*standingVendor) Apply(context.Context, seataccess.ApplyInput) (seataccess.StepResult, error) {
	return seataccess.StepResult{}, errors.New("no step")
}

const standingEmail = "crewlet-growth-0a1b2c@id.iam.gserviceaccount.com"

func (v *standingVendor) CheckIdentity(context.Context, seataccess.IdentityInput) (seataccess.IdentityCheck, error) {
	v.checks++
	if !v.added {
		return seataccess.IdentityCheck{Status: seataccess.IdentityNone, ID: standingEmail, Name: standingEmail,
			Said: standingEmail + " is not added to com.acme.app yet."}, nil
	}
	return seataccess.IdentityCheck{Status: seataccess.IdentityReady, ID: standingEmail, Name: standingEmail}, nil
}

func (v *standingVendor) KeepIdentity(ctx context.Context, in seataccess.IdentityInput) (seataccess.IdentityKept, error) {
	if in.Way == seataccess.WayMade {
		v.added = true
	}
	check, err := v.CheckIdentity(ctx, in)
	return seataccess.IdentityKept{Check: check, Way: in.Way, ID: check.ID, Name: check.Name}, err
}

func (v *standingVendor) DropIdentity(_ context.Context, in seataccess.IdentityInput) (seataccess.IdentityDropped, error) {
	v.dropped++
	v.droppedScope = in.Want.Scope
	return seataccess.IdentityDropped{}, nil
}

// playCompany is one agent seat on Google Play, on or off, naming apps.
func playCompany(on bool, apps ...string) *config.Company {
	if len(apps) == 0 && on {
		apps = []string{"com.acme.app"}
	}
	return &config.Company{Name: "Acme", Integrations: config.Integrations{
		GooglePlay: &config.GooglePlay{Enabled: true}},
		Roles: []config.Role{{Name: "Growth", Handle: "growth", Integrations: config.RoleIntegrations{
			GooglePlay: &config.RoleGooglePlay{Enabled: &on, Level: "read", Apps: apps}}}}}
}

func standingSync(t *testing.T, v *standingVendor, ledger *seataccesstest.Ledger, c *config.Company,
	ask *seataccess.IdentityAsk) *seataccess.IdentityRecord {
	t.Helper()
	return syncStanding(t, v, ledger, c, ask, seataccesstest.NewSink())
}

// syncStanding is one pass, a dry run where sink is nil.
func syncStanding(t *testing.T, v *standingVendor, ledger *seataccesstest.Ledger, c *config.Company,
	ask *seataccess.IdentityAsk, sink *seataccesstest.Sink) *seataccess.IdentityRecord {
	t.Helper()
	in := seataccess.SyncInput{Vendor: v, Open: seataccess.OpenInput{Config: c}, Ledger: ledger,
		Pass: "pass-1", By: "test", Now: func() time.Time { return at }}
	if sink != nil {
		in.Sink = sink
	}
	if ask != nil {
		in.Asks = map[string]seataccess.Ask{"growth": {Lane: standingLane, Identity: ask}}
	}
	if _, err := seataccess.Sync(t.Context(), in); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	rec, _ := ledger.Record(integration.KindGooglePlay, "growth")
	return rec.Lanes[standingLane].Identity
}

// A STANDING IDENTITY IS CHECKED ON EVERY PASS AND KEPT EXACTLY WHILE IT
// PASSES: nobody has to come back and press anything once they added it, and
// one removed at the product reads Needs you again, never Refused, since it is
// still the seat's own. Add it for me is a keep that adds first; turning the
// seat off gives it up.
//
// Mutation: check only kept identities (the handed-in lanes' rule), and the
// identity a person added is never kept.
func TestAStandingIdentityIsKeptExactlyWhileItPasses(t *testing.T) {
	t.Parallel()
	v := &standingVendor{}
	ledger := seataccesstest.NewLedger()
	c := playCompany(true)

	got := standingSync(t, v, ledger, c, nil)
	if got.Kept() || got.Status() != seataccess.IdentityNone || got.Attempt == nil {
		t.Fatalf("not added: %+v", got)
	}
	want, _ := seataccess.DesiredOf(&c.Roles[0], standingLane)
	answer := seataccess.IdentityOf(v, c, seataccess.SeatRef{Handle: "growth", Name: "Growth"}, standingLane, want,
		seataccess.Observed{}, got, time.Now())
	if answer.Holds != seataccess.HoldsOwn || !strings.Contains(answer.Said, "Google Play refuses its calls") ||
		answer.Problem == nil || !strings.Contains(answer.Problem.Body, "not added") || answer.Name != standingEmail {
		t.Fatalf("the box before it is added: %+v", answer)
	}

	v.added = true
	if got = standingSync(t, v, ledger, c, nil); !got.Kept() || got.Status() != seataccess.IdentityReady ||
		got.ID != standingEmail {
		t.Fatalf("added at the product, a pass did not keep it: %+v", got)
	}

	v.added = false
	if got = standingSync(t, v, ledger, c, nil); got.Kept() || got.Status() != seataccess.IdentityNone {
		t.Fatalf("removed at the product: %+v", got)
	}
	if _, err := seataccess.ResolveIdentity(integration.KindGooglePlay, standingSpec, "growth", want, got); err != nil {
		t.Fatalf("a standing identity stopped signing: %v", err)
	}

	got = standingSync(t, v, ledger, c, &seataccess.IdentityAsk{Keep: true, Way: seataccess.WayMade})
	if !got.Kept() || got.Way != seataccess.WayMade {
		t.Fatalf("Add it for me: %+v", got)
	}

	if got = standingSync(t, v, ledger, playCompany(false), nil); got != nil || v.dropped != 1 {
		t.Fatalf("off: %+v, dropped %d", got, v.dropped)
	}
	checks := v.checks
	if got = standingSync(t, v, ledger, playCompany(false), nil); got != nil || v.dropped != 1 || v.checks != checks {
		t.Errorf("an off seat is given up again or checked: %+v, dropped %d", got, v.dropped)
	}
}

// A STANDING IDENTITY READS THE SAME IN A DRY RUN, named as the vendor knows
// it, and is taken off what its seat last reached once the seat names
// nothing: a dry run keeps one a person just added, the guide names the
// identity that exists rather than the rule's, and an off seat with no apps
// left in the document still has its identity taken off the app it reached.
//
// Mutation: drop with the document's scope alone, and the off seat's identity
// is taken off nothing.
func TestAStandingIdentityIsReadAlikeNamedAsItIsAndDroppedWhereItReached(t *testing.T) {
	t.Parallel()
	v := &standingVendor{added: true}
	ledger := seataccesstest.NewLedger()
	c := playCompany(true)
	got := syncStanding(t, v, ledger, c, nil, nil)
	if !got.Kept() || got.Status() != seataccess.IdentityReady {
		t.Fatalf("a dry run did not read the added identity: %+v", got)
	}
	want, _ := seataccess.DesiredOf(&c.Roles[0], standingLane)
	answer := seataccess.IdentityOf(v, c, seataccess.SeatRef{Handle: "growth"}, standingLane, want,
		seataccess.Observed{}, got, time.Now())
	if answer.Guide == nil || answer.Guide.Name != standingEmail || answer.Guide.Steps[0].Copy.Value != standingEmail {
		t.Errorf("the guide names %+v, want the identity that exists", answer.Guide)
	}
	standingSync(t, v, ledger, c, nil)
	if got = standingSync(t, v, ledger, playCompany(false), nil); got != nil || v.dropped != 1 ||
		len(v.droppedScope) != 1 || v.droppedScope[0] != "com.acme.app" {
		t.Errorf("off with no apps: %+v, dropped %d off %v", got, v.dropped, v.droppedScope)
	}
}

// A NEW IDENTITY GETS READY ON THE VENDOR'S CLOCK, then is the person's: the
// box says it is getting ready, with no problem to fix, until Google can find
// it, and needs the person from then on, without waiting for the next check.
// The check's reading of each entry is laid over the guide's, and its
// account-level alternative links each holder of an entry still missing it.
//
// Mutation: read the stored status alone, and the row reads Getting ready
// until the next pass though the person could add it already.
func TestAGettingReadyIdentityIsThePersonsOnceGoogleCanFindIt(t *testing.T) {
	t.Parallel()
	v := &standingVendor{}
	c := playCompany(true)
	want, _ := seataccess.DesiredOf(&c.Roles[0], standingLane)
	made := time.Date(2026, 11, 25, 6, 0, 0, 0, time.UTC)
	rec := &seataccess.IdentityRecord{Attempt: &seataccess.IdentityCheck{Status: seataccess.IdentityPreparing,
		ReadyAt: made.Add(2 * time.Minute), Said: "Google Play can find it in about a minute.",
		Entries: []seataccess.IdentityEntry{{ID: "com.acme.app", Name: "Acme", Group: "Acme Studio",
			State: seataccess.EntryMissing, GroupURL: "https://play.example/acme"}}}}
	seat := seataccess.SeatRef{Handle: "growth", Name: "Growth"}
	early := seataccess.IdentityOf(v, c, seat, standingLane, want, seataccess.Observed{}, rec, made.Add(time.Minute))
	if early.Status != seataccess.IdentityPreparing || early.Problem != nil ||
		!strings.HasPrefix(early.Said, "Getting ready: ") || !early.Guide.ReadyAt.Equal(made.Add(2*time.Minute)) {
		t.Fatalf("a minute after it was made: %+v", early)
	}
	if g := early.Guide; len(g.Entries) != 1 || g.Entries[0].Name != "Acme" || g.Wider == nil ||
		len(g.Wider.Links) != 1 || g.Wider.Links[0].Label != "Acme Studio" {
		t.Errorf("the guide's entries %+v, wider %+v", g.Entries, g.Wider)
	}
	later := seataccess.IdentityOf(v, c, seat, standingLane, want, seataccess.Observed{}, rec, made.Add(3*time.Minute))
	if later.Status != seataccess.IdentityNone || !later.Guide.ReadyAt.IsZero() {
		t.Errorf("three minutes after it was made: %+v", later)
	}
}

// A LANE IS IN SYNC ONLY ON A CHECKED ROLE: an identity that works with its
// role unread reads Role not checked, and one the vendor cannot find yet
// reads Getting ready, never Needs you.
//
// Mutation: let an unchecked identity work as Ready does, and the row reads
// In sync.
func TestALaneIsInSyncOnlyOnACheckedRole(t *testing.T) {
	t.Parallel()
	on := seataccess.Desired{Enabled: true}
	for status, want := range map[seataccess.IdentityStatus]seataccess.LaneStatus{
		seataccess.IdentityReady:     seataccess.StatusInSync,
		seataccess.IdentityUnchecked: seataccess.StatusRoleUnchecked,
		seataccess.IdentityPreparing: seataccess.StatusPreparing,
		seataccess.IdentityNone:      seataccess.StatusNeedsYou,
	} {
		la := seataccess.LaneAnswer{Desired: on, Identity: &seataccess.IdentityAnswer{Offered: true,
			Holds: seataccess.HoldsOwn, Status: status}}
		if got := seataccess.StatusOf(la, false, false); got != want {
			t.Errorf("identity %s reads %s, want %s", status, got, want)
		}
	}
}
