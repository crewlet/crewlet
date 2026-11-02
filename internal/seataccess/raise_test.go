package seataccess_test

import (
	"errors"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// datadogOffer is Datadog's roles as read: the Admin Role administers.
var datadogOffer = seataccess.Options{Lane: seataccess.LaneDatadog, Live: true,
	Levels: []seataccess.Level{
		{ID: "role-ro", Name: "Datadog Read Only Role"},
		{ID: "role-std", Name: "Datadog Standard Role"},
		{ID: "role-admin", Name: "Datadog Admin Role", Administers: true},
	}}

// A RAISE IS ASKED A STEP-UP WHEREVER IT CANNOT BE TOLD NOT TO BE ONE (D10).
//
// On an offer that was read, the vendor's own facts decide: a raise to the
// Admin Role asks, a move between ordinary roles does not. On an offer nobody
// could read, a turn-on or a change of level asks, since which level
// administers is unknown; turning off and changing nothing never ask. A lane
// managed outside Crewlet never asks, since the engine grants nothing there.
// Mutation: judge an unread offer as the vendor's facts, or ask of an external
// lane, and a case here fails.
func TestAStepUpFailsClosedOnAnUnreadOffer(t *testing.T) {
	t.Parallel()
	unread := seataccess.UnreadOffers(integration.KindDatadog, false)[seataccess.LaneDatadog]
	external := seataccess.UnreadOffers(integration.KindDatadog, true)[seataccess.LaneDatadog]
	on := func(level string) seataccess.Desired {
		return seataccess.Desired{Enabled: true, Level: level}
	}
	off := seataccess.Desired{Level: "role-std"}
	for name, tc := range map[string]struct {
		opts          seataccess.Options
		before, after seataccess.Desired
		want          bool
	}{
		"read: a raise to admin":         {datadogOffer, on("role-std"), on("role-admin"), true},
		"read: an ordinary move":         {datadogOffer, on("role-ro"), on("role-std"), false},
		"unread: a turn-on":              {unread, off, on("role-std"), true},
		"unread: a change of level":      {unread, on("role-ro"), on("role-std"), true},
		"unread: a turn-off":             {unread, on("role-admin"), off, false},
		"unread: the same level, again":  {unread, on("role-std"), on("role-std"), false},
		"external: a turn-on at a level": {external, off, on("role-admin"), false},
	} {
		if got := seataccess.AsksStepUp(tc.opts, tc.before, tc.after); got != tc.want {
			t.Errorf("%s: asks step-up = %v, want %v", name, got, tc.want)
		}
	}
}

// datadogSeats is a company with Datadog provisioned and the SRE on it at
// level.
func datadogSeats(level string, enabled bool) *config.Company {
	return &config.Company{
		Integrations: config.Integrations{Datadog: &config.Datadog{Enabled: true,
			Provisioning: &config.DatadogProvisioning{}}},
		Roles: []config.Role{{Name: "SRE", Handle: "sre",
			Integrations: config.RoleIntegrations{Datadog: &config.RoleDatadog{
				Enabled: &enabled, Role: level}}}},
	}
}

// A WRITE OF THE DOCUMENT IS A RAISE WHERE ITS SEATS MOVE UP, AND ONLY THERE.
//
// The decider compares the revision replaced with the one proposed, seat by
// seat and lane by lane: the SRE moved to the Admin Role is a raise, a seat
// new to the document already at it is too, and a write that leaves every
// seat's setting as it was is never one and reads no vendor at all.
// Mutation: compare the proposal alone, or read the vendor for a write that
// moves no seat, and a case here fails.
func TestAWriteRaisesOnlyWhereASeatMovesUp(t *testing.T) {
	t.Parallel()
	reads := 0
	offers := func(kind integration.Kind) map[seataccess.Lane]seataccess.Options {
		reads++
		if kind != integration.KindDatadog {
			return nil
		}
		return map[seataccess.Lane]seataccess.Options{seataccess.LaneDatadog: datadogOffer}
	}
	std, admin := datadogSeats("role-std", true), datadogSeats("role-admin", true)
	if got := seataccess.RaisesBetween(std, admin, false, offers); len(got) != 1 ||
		got[0] != (seataccess.Raise{Kind: integration.KindDatadog, Handle: "sre",
			Lane: seataccess.LaneDatadog}) {
		t.Errorf("std to admin = %+v, want the SRE's Datadog", got)
	}
	if got := seataccess.RaisesBetween(nil, admin, false, offers); len(got) != 1 {
		t.Errorf("a first document with the SRE at admin = %+v, want one raise", got)
	}
	if got := seataccess.RaisesBetween(admin, std, false, offers); len(got) != 0 {
		t.Errorf("a lowering = %+v, want none", got)
	}
	reads = 0
	if got := seataccess.RaisesBetween(admin, datadogSeats("role-admin", true), false, offers); len(got) != 0 ||
		reads != 0 {
		t.Errorf("an unchanged seat = %+v after %d vendor reads, want none and none", got, reads)
	}
}

// KEEPING THE VENDOR'S SETTING WRITES EACH VENDOR'S OWN TERMS (D2, D3).
//
// An account turned off at the vendor is kept off with the role the document
// names, so turning it back on restores that; an account at another role
// keeps that role; a Datadog account holding several roles is refused, since
// keeping one would take the others away; Slack keeps on or off alone.
// Mutation: keep the observed state wholesale, and a case here fails.
func TestKeepingTheVendorsSettingWritesItsOwnTerms(t *testing.T) {
	t.Parallel()
	want := seataccess.Desired{Enabled: true, Level: "role-std"}
	got, err := seataccess.KeptAccess(seataccess.LaneDatadog, want,
		seataccess.Observed{Exists: true, Enabled: false})
	if err != nil || got.Enabled || got.Level != "role-std" {
		t.Errorf("kept off = %+v, %v; want off at the document's role", got, err)
	}
	got, err = seataccess.KeptAccess(seataccess.LaneDatadog, want,
		seataccess.Observed{Exists: true, Enabled: true, Level: "role-ro"})
	if err != nil || !got.Enabled || got.Level != "role-ro" {
		t.Errorf("kept the vendor's role = %+v, %v; want on at role-ro", got, err)
	}
	if _, err = seataccess.KeptAccess(seataccess.LaneDatadog, want, seataccess.Observed{
		Exists: true, Enabled: true, Level: "role-ro role-admin"}); !errors.Is(err,
		seataccess.ErrSeveralRoles) {
		t.Errorf("several roles = %v, want ErrSeveralRoles", err)
	}
	got, err = seataccess.KeptAccess(seataccess.LaneSlack, seataccess.Desired{Enabled: true},
		seataccess.Observed{Exists: true, Enabled: true, Level: "bot", Scope: []string{"x"}})
	if err != nil || !got.Enabled || got.Level != "" || len(got.Scope) != 0 {
		t.Errorf("Slack kept = %+v, %v; want on and nothing else", got, err)
	}
}

// A KIND THE ENGINE STARTS GRANTING IS A TURN-ON, in two writes as in one.
//
// The SRE is set to Datadog's Admin Role while Datadog is handed in from
// outside Crewlet, which grants nothing and asks nobody. A later write adds the
// provisioning block and moves no seat setting at all, yet from it on the
// engine creates the account at that role, so that write is the raise.
// Mutation: compare the seat with its prior setting whatever the kind's mode,
// and the second write passes with no person asked.
func TestAKindTheEngineStartsGrantingIsATurnOn(t *testing.T) {
	t.Parallel()
	offers := func(kind integration.Kind) map[seataccess.Lane]seataccess.Options {
		if kind != integration.KindDatadog {
			return nil
		}
		return map[seataccess.Lane]seataccess.Options{seataccess.LaneDatadog: datadogOffer}
	}
	external := datadogSeats("role-admin", true)
	external.Integrations = config.Integrations{}
	provisioned := datadogSeats("role-admin", true)
	got := seataccess.RaisesBetween(external, provisioned, false, offers)
	if len(got) != 1 || got[0] != (seataccess.Raise{Kind: integration.KindDatadog,
		Handle: "sre", Lane: seataccess.LaneDatadog}) {
		t.Errorf("provisioning a kind under a seat already at admin = %+v, want the SRE's Datadog", got)
	}
	if got := seataccess.RaisesBetween(provisioned, datadogSeats("role-admin", true), false, offers); len(got) != 0 {
		t.Errorf("a provisioned kind left as it was = %+v, want none", got)
	}
	if got := seataccess.RaisesBetween(external, datadogSeats("role-std", true), false, offers); len(got) != 0 {
		t.Errorf("provisioning a kind under a seat at an ordinary role = %+v, want none", got)
	}
}
