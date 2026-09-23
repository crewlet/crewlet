package notify_test

import (
	"context"
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
)

// A SEAT WHOSE HOLDER MAY NOT BE REACHED REGISTERS NO CONTACT IDENTITY — and
// stays a party.
//
// The control is the registry every node built before the directory existed:
// the org view alone, which went on attributing a suspended founder's Slack
// messages to the founder's seat. The same org with the directory's reading
// applied resolves that account to nobody, while the seat itself still answers
// by handle, because a mention of the founder and a colleague line in a prompt
// are about the SEAT and are not what a suspension withdraws.
func TestAWithheldSeatRegistersNoContactIdentity(t *testing.T) {
	t.Parallel()
	o := company()
	o.Normalize()

	// THE CONTROL: the org view alone routes to the suspended founder.
	chartOnly := notify.NewRegistry(o)
	chartOnly.ReconcileHumanContacts(o, env(nil), notify.Standing{})
	if _, ok := chartOnly.ByExternalID("slack", "U0FOUNDER"); !ok {
		t.Fatal("the org-view-only registry does not route the founder at all, " +
			"so nothing below can show a withdrawal")
	}

	suspended := notify.StandingOf([]notify.Holder{
		{Seat: "dana-founder", Stage: iam.StageSuspended},
	})
	r := notify.NewRegistry(o)
	rec := r.ReconcileHumanContacts(o, env(nil), suspended)

	for ns, id := range map[string]string{"slack": "U0FOUNDER", "github": "danaf"} {
		if p, ok := r.ByExternalID(ns, id); ok {
			t.Errorf("a suspended holder's %s account still resolves to %q", ns, p.Handle)
		}
	}
	if rec.Withheld != 2 || rec.Registered != 0 {
		t.Errorf("reconciled %+v, want both of the founder's identities withheld "+
			"and none registered", rec)
	}
	if why, ok := r.Withholding("dana-founder"); !ok || why != notify.WithheldSuspended {
		t.Errorf("the registry reports the founder's seat as %q/%v, want suspended",
			why, ok)
	}
	if p, ok := r.ByHandle("dana-founder"); !ok || !p.Human {
		t.Errorf("the seat itself stopped resolving (%+v): a suspension withdraws "+
			"the identities, not the colleague", p)
	}
	if !r.Standing().Equal(suspended) {
		t.Error("the registry does not remember the reading it was built from")
	}
}

// REINSTATING SOMEBODY RESTORES THEIR IDENTITIES, and reconciling the SAME
// registry against a moved reading withdraws and restores exactly as a contact
// edit does — so the rule does not depend on the engine always building fresh.
func TestAMovedStandingWithdrawsAndRestoresOnOneRegistry(t *testing.T) {
	t.Parallel()
	o := company()
	o.Normalize()
	r := notify.NewRegistry(o)

	r.ReconcileHumanContacts(o, env(nil), notify.StandingOf([]notify.Holder{
		{Seat: "dana-founder", Stage: iam.StageActive},
	}))
	if _, ok := r.ByExternalID("slack", "U0FOUNDER"); !ok {
		t.Fatal("an active holder's identity did not register")
	}

	rec := r.ReconcileHumanContacts(o, env(nil), notify.StandingOf([]notify.Holder{
		{Seat: "dana-founder", Stage: iam.StageSuspended},
	}))
	if rec.Withdrawn != 2 {
		t.Errorf("a suspension withdrew %d identities, want 2", rec.Withdrawn)
	}
	if _, ok := r.ByExternalID("slack", "U0FOUNDER"); ok {
		t.Error("the suspended holder still resolves")
	}

	rec = r.ReconcileHumanContacts(o, env(nil), notify.StandingOf([]notify.Holder{
		{Seat: "dana-founder", Stage: iam.StageActive},
	}))
	if rec.Registered != 2 {
		t.Errorf("reinstating registered %d identities, want 2", rec.Registered)
	}
	if p, ok := r.ByExternalID("slack", "U0FOUNDER"); !ok || p.Handle != "dana-founder" {
		t.Errorf("reinstating did not restore the founder's account: %+v", p)
	}
	if _, ok := r.Withholding("dana-founder"); ok {
		t.Error("a reinstated seat still reports itself withheld")
	}
}

// THE RULE, stage by stage.
//
// An ALLOWLIST of the stages that route, for internal/iam's own reason turned
// towards delivery: a stage this build cannot name is one a newer peer wrote,
// and of the two ways to be wrong about it the one that reaches nobody is the
// one a rolling upgrade repairs in minutes.
func TestTheStandingRuleRoutesOnlyWhatMayBeReached(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		holder notify.Holder
		want   notify.Withholding // "" routes
	}{
		{"active", notify.Holder{Stage: iam.StageActive}, ""},
		{"invited", notify.Holder{Stage: iam.StageInvited}, ""},
		{"enrolling", notify.Holder{Stage: iam.StageEnrolling}, ""},
		{"a reservation mid-enrolment", notify.Holder{}, ""},
		{"suspended", notify.Holder{Stage: iam.StageSuspended}, notify.WithheldSuspended},
		{"retired", notify.Holder{Stage: iam.StageRetired}, notify.WithheldRetired},
		{"removed", notify.Holder{Removed: true}, notify.WithheldRemoved},
		{"a stage from a newer build", notify.Holder{Stage: "on_sabbatical"},
			notify.WithheldUnrecognised},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			tc.holder.Seat = "dana-founder"
			o := company()
			o.Normalize()
			why, withheld := notify.StandingOf([]notify.Holder{tc.holder}).
				Seats(o)["dana-founder"]
			if tc.want == "" {
				if withheld {
					t.Errorf("withheld as %q, want routed", why)
				}
				return
			}
			if !withheld || why != tc.want {
				t.Errorf("got %q/%v, want %q", why, withheld, tc.want)
			}
			if !why.Valid() {
				t.Errorf("%q is not a Withholding this package names", why)
			}
		})
	}
}

// A SEAT WITH TWO HOLDERS IS WITHHELD IF EITHER MAY NOT BE REACHED, and says
// the reason that will still be true after the other is fixed.
//
// Two holders is a duplicate binding only a restore produces, which a duty
// reports. The contact map is one and there is no way to say whose it is, so
// routing it because ONE of them is active would be routing to the other half
// of the time.
func TestADuplicateBindingIsWithheldIfEitherHolderMayNotBeReached(t *testing.T) {
	t.Parallel()
	s := notify.StandingOf([]notify.Holder{
		{Seat: "dana-founder", Stage: iam.StageActive},
		{Seat: "dana-founder", Stage: iam.StageSuspended},
		{Seat: "dana-founder", Stage: iam.StageRetired},
	})
	o := company()
	o.Normalize()
	if why, ok := s.Seats(o)["dana-founder"]; !ok || why != notify.WithheldRetired {
		t.Errorf("got %q/%v, want retired — the harder one to undo", why, ok)
	}
}

// READING THE DIRECTORY IS THREE-VALUED.
//
// No directory is CHART-ONLY and no error — a node that does not run the
// identity domain. A directory that could not be read is an ERROR, never an
// empty reading: an empty reading withholds nothing, and folding an outage
// into it would hand every suspended person's seat back to the chart.
func TestReadingTheDirectoryIsThreeValued(t *testing.T) {
	t.Parallel()
	none, err := notify.ReadStanding(t.Context(), nil)
	if err != nil || none.Consulted() {
		t.Errorf("no directory answered %+v, %v; want chart-only and no error", none, err)
	}

	boom := errors.New("the replicated estate is not open")
	_, err = notify.ReadStanding(t.Context(), directoryFunc(
		func(context.Context) ([]notify.Holder, error) { return nil, boom }))
	if !errors.Is(err, boom) {
		t.Errorf("an unreadable directory answered %v, want its error", err)
	}

	read, err := notify.ReadStanding(t.Context(), directoryFunc(
		func(context.Context) ([]notify.Holder, error) { return nil, nil }))
	if err != nil || !read.Consulted() || len(read.Withheld()) != 0 {
		t.Errorf("an empty directory answered %+v, %v; want a consulted reading "+
			"withholding nothing", read, err)
	}
	// AND THE TWO EMPTY ANSWERS ARE DIFFERENT READINGS, so a node that
	// gains the directory rebuilds rather than deciding nothing moved.
	if read.Equal(none) {
		t.Error("a consulted empty reading equals the chart-only one")
	}
}

// A SUSPENSION FINDS A SEAT THE CHART HAS RENAMED SINCE THE BIND.
//
// A binding names its seat by the handle the seat was CREATED under (ADR-0020),
// which a rename never moves. The control is the reading applied to that name
// as a live handle — what the registry did before the binding was an identity —
// which names a handle no seat answers to, so the suspended founder's Slack
// account went on resolving to their renamed seat.
func TestASuspensionAfterARenameStillWithholdsTheSeat(t *testing.T) {
	t.Parallel()
	o := renamedCompany()
	suspended := notify.StandingOf([]notify.Holder{
		{Seat: "dana-founder", Stage: iam.StageSuspended},
	})

	// THE CONTROL: no seat answers to the bound identity as its own handle.
	for role := range o.AllRoles() {
		if role.Handle() == "dana-founder" {
			t.Fatal("the fixture's seat still answers to its old handle, so " +
				"nothing below is about a rename")
		}
	}

	r := notify.NewRegistry(o)
	rec := r.ReconcileHumanContacts(o, env(nil), suspended)
	if p, ok := r.ByExternalID("slack", "U0FOUNDER"); ok {
		t.Errorf("a suspended holder's account still resolves to %q after the "+
			"seat was renamed", p.Handle)
	}
	if why, ok := r.Withholding("dana"); !ok || why != notify.WithheldSuspended {
		t.Errorf("the renamed seat reports %q/%v, want suspended", why, ok)
	}
	if rec.Withheld != 2 {
		t.Errorf("withheld %d identities, want both of the founder's", rec.Withheld)
	}
	if got := r.Withheld(); len(got) != 1 || got[0] != "dana" {
		t.Errorf("the registry withholds %v, want the seat by its current handle", got)
	}
}

// A BINDING FOLLOWS ITS SEAT, AND NEVER THE ADDRESS IT WAS MADE WITH.
//
// The founder's seat was created as `founder`, renamed to `dana-founder` and
// then to `dana`, and a new seat for somebody else has since taken the retired
// alias `dana-founder` — which the chart allows, because an alias is not an
// identity. The founder is suspended. Their binding names `founder`, so it is
// THEIR seat that is withheld, under its current handle, and the stranger's
// seat routes: resolved as an address the other way round, the stranger was
// silenced for somebody else's suspension while the suspended person's own
// Slack messages went on being attributed to their seat.
func TestABindingFollowsItsSeatAndNeverTheAddressItWasMadeWith(t *testing.T) {
	t.Parallel()
	o := company()
	for _, role := range o.Roles {
		if role.Name == "Dana Founder" {
			role.DeclaredHandle = "dana"
			role.OriginHandle = "founder"
			role.FormerHandles = []string{"dana-founder", "founder"}
		}
	}
	o.Roles = append(o.Roles, &org.Role{
		Name: "Dana Founder Two", Kind: org.KindHuman, DeclaredHandle: "dana-founder",
		Contact: &org.HumanContact{SlackUserID: "U0SECOND"},
	})
	o.Normalize()

	r := notify.NewRegistry(o)
	r.ReconcileHumanContacts(o, env(nil), notify.StandingOf([]notify.Holder{
		{Seat: "founder", Stage: iam.StageSuspended},
		{Seat: "dana-founder", Stage: iam.StageActive},
	}))
	if p, ok := r.ByExternalID("slack", "U0FOUNDER"); ok {
		t.Errorf("the suspended founder's account still resolves to %q", p.Handle)
	}
	if why, ok := r.Withholding("dana"); !ok || why != notify.WithheldSuspended {
		t.Errorf("the founder's renamed seat reports %q/%v, want suspended", why, ok)
	}
	if p, ok := r.ByExternalID("slack", "U0SECOND"); !ok || p.Handle != "dana-founder" {
		t.Errorf("the seat that took the retired alias was withheld for "+
			"somebody else's suspension (%+v, %v)", p, ok)
	}
}

// TWO SEATS CLAIMING ONE IDENTITY ARE BOTH WITHHELD.
//
// The chart never issues an identity twice, so this is an organization built by
// hand or a restore's residue. Picking one of the two could route a suspended
// person's accounts through the seat not picked; withholding both makes one
// person briefly unreachable, which is the direction to be wrong in.
func TestTwoSeatsClaimingOneIdentityAreBothWithheld(t *testing.T) {
	t.Parallel()
	o := renamedCompany()
	o.Roles = append(o.Roles, &org.Role{
		Name: "Dana Founder Two", Kind: org.KindHuman, DeclaredHandle: "dana-founder",
		Contact: &org.HumanContact{SlackUserID: "U0SECOND"},
	})
	o.Normalize()

	seats := notify.StandingOf([]notify.Holder{
		{Seat: "dana-founder", Stage: iam.StageSuspended},
	}).Seats(o)
	for _, handle := range []string{"dana", "dana-founder"} {
		if why := seats[handle]; why != notify.WithheldSuspended {
			t.Errorf("%s reads %q, want suspended — both seats carry the "+
				"suspended binding's identity", handle, why)
		}
	}
}

// AN UNREAD DIRECTORY WITHHOLDS EVERY HUMAN SEAT, and is a reading of its own.
//
// It is what a node builds from before it has ever read its directory, so it
// must differ from both readings it could be confused with: chart-only, which
// routes everybody, and an empty directory, which routes everybody the chart
// declares — either of which, standing in for it, would compare Equal to the
// net's first real reading and skip the rebuild that opens routing.
func TestAnUnreadDirectoryWithholdsEveryHumanSeat(t *testing.T) {
	t.Parallel()
	o := company()
	o.Normalize()
	unread := notify.Unread()
	seats := unread.Seats(o)
	if why, ok := seats["dana-founder"]; !ok || why != notify.WithheldUnread {
		t.Errorf("the human seat reads %q/%v, want %q", why, ok, notify.WithheldUnread)
	}
	if _, ok := seats["engineering-lead"]; ok {
		t.Error("an unread directory withholds an agent seat, which declares no contact")
	}
	if !notify.WithheldUnread.Valid() {
		t.Errorf("%q is not a Withholding this package names", notify.WithheldUnread)
	}
	if unread.Equal(notify.Standing{}) || unread.Equal(notify.StandingOf(nil)) {
		t.Error("an unread reading equals one that routes everybody")
	}
}

// renamedCompany is the fixture company after the founder's seat was renamed
// from `dana-founder` to `dana` — the shape the chart gives a renamed seat: the
// handle it was created under frozen as its origin, and retired as an alias.
func renamedCompany() *org.Organization {
	o := company()
	for _, role := range o.Roles {
		if role.Name == "Dana Founder" {
			role.DeclaredHandle = "dana"
			role.OriginHandle = "dana-founder"
			role.FormerHandles = []string{"dana-founder"}
		}
	}
	o.Normalize()
	return o
}

// directoryFunc adapts a function to the seam.
type directoryFunc func(context.Context) ([]notify.Holder, error)

func (f directoryFunc) SeatHolders(ctx context.Context) ([]notify.Holder, error) {
	return f(ctx)
}
