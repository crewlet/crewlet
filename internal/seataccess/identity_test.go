package seataccess_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
	"github.com/crewlet/crewlet/internal/seataccess/seataccesstest"
	"github.com/crewlet/crewlet/internal/seatname"
)

var ownSpec = seataccess.IdentitySpec{Ways: []seataccess.IdentityWay{seataccess.WayHandedIn},
	Default: seataccess.IdentityCompany, Company: true, Name: seatname.AppStoreConnect, Credential: "KEY",
	Retiring: "RETIRING_KEY"}

// THE RESOLVER NEVER MIXES THE TWO SILENTLY: own where the seat keeps one that
// works, the company's where it keeps none or is set to it, and a refusal,
// never the company's, where the vendor refused the seat's own.
func TestTheResolverNeverFallsBackSilently(t *testing.T) {
	t.Parallel()
	own := seataccess.Desired{Enabled: true, Identity: seataccess.IdentityOwn}
	ready := &seataccess.IdentityRecord{ID: "K1", Check: &seataccess.IdentityCheck{Status: seataccess.IdentityReady}}
	refused := &seataccess.IdentityRecord{ID: "K1", Check: &seataccess.IdentityCheck{
		Status: seataccess.IdentityRefused, Said: "Apple answers 401"}}
	tried := &seataccess.IdentityRecord{Attempt: &seataccess.IdentityCheck{Status: seataccess.IdentityWrongRole}}
	for _, c := range []struct {
		name    string
		spec    seataccess.IdentitySpec
		want    seataccess.Desired
		rec     *seataccess.IdentityRecord
		own     bool
		refused bool
	}{
		{"company-only vendor", seataccess.IdentitySpec{}, own, ready, false, false},
		{"set to the company's", ownSpec, seataccess.Desired{Enabled: true, Identity: seataccess.IdentityCompany},
			ready, false, false},
		{"unset reads as the default", ownSpec, seataccess.Desired{Enabled: true}, ready, false, false},
		{"own, nothing kept yet", ownSpec, own, nil, false, false},
		{"own, an attempt not kept", ownSpec, own, tried, false, false},
		{"own and ready", ownSpec, own, ready, true, false},
		{"own and refused by the vendor", ownSpec, own, refused, false, true},
		{"own by construction", seataccess.IdentitySpec{Ways: []seataccess.IdentityWay{seataccess.WayInvited}},
			seataccess.Desired{Enabled: true}, nil, true, false},
	} {
		got, err := seataccess.ResolveIdentity(integration.KindMetaAds, c.spec, "growth", c.want, c.rec)
		if _, isRefusal := errors.AsType[*seataccess.IdentityRefusal](err); isRefusal != c.refused {
			t.Errorf("%s: error %v, want a refusal %v", c.name, err, c.refused)
		}
		if c.refused && got.Own {
			t.Errorf("%s: a refused identity still signs", c.name)
		}
		if got.Own != c.own {
			t.Errorf("%s: own = %v, want %v", c.name, got.Own, c.own)
		}
	}
}

// THE COMPANY'S IDENTITY IS NEVER IN SYNC WHERE THE VENDOR OFFERS THE SEAT ITS
// OWN: the row reads Needs you until the seat's own works, Refused where the
// vendor refused it; a company-only lane reads In sync with nothing to set up.
func TestTheRowNeedsYouUntilTheSeatsOwnIdentityWorks(t *testing.T) {
	t.Parallel()
	on := seataccess.Desired{Enabled: true}
	for _, c := range []struct {
		name string
		id   *seataccess.IdentityAnswer
		want seataccess.LaneStatus
	}{
		{"company-only", &seataccess.IdentityAnswer{Offered: false, Status: seataccess.IdentityCompanys,
			Holds: seataccess.HoldsCompany}, seataccess.StatusInSync},
		{"offered, on the company's", &seataccess.IdentityAnswer{Offered: true,
			Status: seataccess.IdentityCompanys, Holds: seataccess.HoldsCompany}, seataccess.StatusNeedsYou},
		{"offered, not set up", &seataccess.IdentityAnswer{Offered: true, Status: seataccess.IdentityNone,
			Holds: seataccess.HoldsCompany}, seataccess.StatusNeedsYou},
		{"offered, wrong role", &seataccess.IdentityAnswer{Offered: true, Status: seataccess.IdentityWrongRole,
			Holds: seataccess.HoldsCompany}, seataccess.StatusNeedsYou},
		{"offered, ready", &seataccess.IdentityAnswer{Offered: true, Status: seataccess.IdentityReady,
			Holds: seataccess.HoldsOwn}, seataccess.StatusInSync},
		{"offered, replacing", &seataccess.IdentityAnswer{Offered: true, Status: seataccess.IdentityReplacing,
			Holds: seataccess.HoldsOwn}, seataccess.StatusInSync},
		{"offered, refused", &seataccess.IdentityAnswer{Offered: true, Status: seataccess.IdentityRefused,
			Holds: seataccess.HoldsNone}, seataccess.StatusRefused},
	} {
		la := seataccess.LaneAnswer{Desired: on, Plan: []seataccess.Step{}, Identity: c.id}
		if got := seataccess.StatusOf(la, false, false); got != c.want {
			t.Errorf("%s: status %s, want %s", c.name, got, c.want)
		}
	}
	off := seataccess.LaneAnswer{Desired: seataccess.Desired{}, Identity: &seataccess.IdentityAnswer{Offered: true,
		Status: seataccess.IdentityCompanys}}
	if got := seataccess.StatusOf(off, false, false); got != seataccess.StatusOff {
		t.Errorf("an off seat reads %s, want off: it needs no identity", got)
	}
}

// identityVendor is an engine-held lane offering a seat its own identity: a
// key a person hands in, which its check reads back by the key's value.
type identityVendor struct {
	// roles is each key's role at the vendor, by value; a key absent is
	// revoked there.
	roles map[string]string
	// dropped counts the identities given up.
	dropped int
}

const identityLane = seataccess.LaneMetaAds

func (*identityVendor) Kind() integration.Kind { return integration.KindMetaAds }

func (*identityVendor) Catalog() seataccess.Catalog {
	return seataccess.Catalog{Kind: integration.KindMetaAds, Lanes: []seataccess.LaneSpec{{
		Lane: identityLane, Levels: true, CreatedBy: seataccess.ByCrewlet, EnabledBy: seataccess.ByCrewlet,
		EngineHeld: true, Identity: ownSpec,
	}}}
}

func (*identityVendor) Steps(seataccess.SeatRef, seataccess.Lane, seataccess.Op, seataccess.Desired,
	seataccess.Observed) []seataccess.Step {
	return nil
}

func (v *identityVendor) Open(context.Context, seataccess.OpenInput) (seataccess.Session, error) {
	return v, nil
}

func (*identityVendor) Options(_ context.Context, lane seataccess.Lane) (seataccess.Options, error) {
	return seataccess.Options{Lane: lane, Levels: []seataccess.Level{{ID: "read", Name: "Read"}}}, nil
}

func (*identityVendor) Observe(_ context.Context, in seataccess.ObserveInput) (seataccess.Observation, error) {
	out := seataccess.Observation{}
	for _, h := range in.Handles() {
		out[h] = map[seataccess.Lane]seataccess.Observed{identityLane: {Exists: true, Enabled: true, Level: "read"}}
	}
	return out, nil
}

func (*identityVendor) Apply(context.Context, seataccess.ApplyInput) (seataccess.StepResult, error) {
	return seataccess.StepResult{}, errors.New("no step")
}

func (v *identityVendor) verdict(value string) seataccess.IdentityCheck {
	switch role, ok := v.roles[value]; {
	case !ok:
		return seataccess.IdentityCheck{Status: seataccess.IdentityRefused, Said: "the vendor refuses the key"}
	case role != "Marketing":
		return seataccess.IdentityCheck{Status: seataccess.IdentityWrongRole, Role: role, Needs: "Marketing",
			Said: "the key holds " + role}
	}
	return seataccess.IdentityCheck{Status: seataccess.IdentityReady, Role: "Marketing", Needs: "Marketing"}
}

func (v *identityVendor) KeepIdentity(_ context.Context, in seataccess.IdentityInput) (seataccess.IdentityKept, error) {
	if in.Value == "" {
		return seataccess.IdentityKept{}, &seataccess.Refusal{Said: "no key was handed in"}
	}
	if in.Value == "unsigned" {
		return seataccess.IdentityKept{}, fmt.Errorf("check the key: %w", vendorAnswer{})
	}
	return seataccess.IdentityKept{Check: v.verdict(in.Value), Way: in.Way, ID: in.FileName,
		Name:   seatname.For(seatname.AppStoreConnect, "Acme", in.Seat.Handle),
		Minted: []seataccess.Minted{{Field: "KEY", Value: in.Value}}}, nil
}

func (v *identityVendor) CheckIdentity(_ context.Context, in seataccess.IdentityInput) (seataccess.IdentityCheck, error) {
	for value, role := range v.roles {
		if role != "" && in.Record.ID == "id-"+value {
			return v.verdict(value), nil
		}
	}
	return seataccess.IdentityCheck{Status: seataccess.IdentityRefused, Said: "the vendor refuses the key"}, nil
}

func (v *identityVendor) DropIdentity(context.Context, seataccess.IdentityInput) (seataccess.IdentityDropped, error) {
	v.dropped++
	return seataccess.IdentityDropped{Waiting: &seataccess.Waiting{Detail: "Revoke the key at Apple"}}, nil
}

// vendorAnswer is a vendor client's error for its definite 4xx answer.
type vendorAnswer struct{}

func (vendorAnswer) Error() string { return "the vendor answered 403" }
func (vendorAnswer) VendorRefusal() *seataccess.Refusal {
	return &seataccess.Refusal{Said: "Accept the latest agreement.", Remedy: "Sign in as the Account Holder."}
}

// A VENDOR'S DEFINITE ANSWER IS THE ATTEMPT'S REFUSAL, never a fault: the
// sync answers, the record says the vendor's reason and remedy, and nothing
// is kept or sealed.
//
// Mutation: classify only a *Refusal, and the vendor's 4xx fails the sync
// the sheet then calls a check that did not finish.
func TestAVendorsRefusalIsTheAttemptNotAFault(t *testing.T) {
	t.Parallel()
	v := &identityVendor{roles: map[string]string{}}
	ledger, sink := seataccesstest.NewLedger(), seataccesstest.NewSink()
	got := identitySync(t, v, ledger, sink, growthCompany("own", true), &seataccess.IdentityAsk{Keep: true,
		Way: seataccess.WayHandedIn, Value: "unsigned", FileName: "id-unsigned"})
	if got.Kept() || got.Attempt == nil || got.Attempt.Status != seataccess.IdentityRefused ||
		got.Attempt.Said != "Accept the latest agreement." || got.Attempt.Remedy != "Sign in as the Account Holder." {
		t.Fatalf("the vendor's refusal is not the attempt: %+v", got)
	}
	if _, held := sink.Held("META_ADS_KEY_GROWTH"); held {
		t.Fatal("a refused key was sealed")
	}
}

// growthCompany is one agent seat on the identity lane at mode.
func growthCompany(mode string, on bool) *config.Company {
	enabled := on
	return &config.Company{Name: "Acme", Integrations: config.Integrations{
		MetaAds: &config.MetaAds{Enabled: true}},
		Roles: []config.Role{{Name: "Growth", Handle: "growth", Integrations: config.RoleIntegrations{
			MetaAds: &config.RoleMetaAds{Enabled: &enabled, Level: "read", AdAccounts: []string{"act_1"},
				Identity: mode}}}}}
}

func identitySync(t *testing.T, v *identityVendor, ledger *seataccesstest.Ledger, sink *seataccesstest.Sink,
	c *config.Company, ask *seataccess.IdentityAsk) *seataccess.IdentityRecord {
	t.Helper()
	in := seataccess.SyncInput{Vendor: v, Open: seataccess.OpenInput{Config: c}, Ledger: ledger,
		Pass: "pass-1", By: "test", Now: func() time.Time { return at }}
	if sink != nil {
		in.Sink = sink
	}
	if ask != nil {
		in.Asks = map[string]seataccess.Ask{"growth": {Lane: identityLane, Identity: ask}}
	}
	if _, err := seataccess.Sync(t.Context(), in); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	rec, _ := ledger.Record(integration.KindMetaAds, "growth")
	return rec.Lanes[identityLane].Identity
}

// AN IDENTITY IS KEPT ONLY ONCE ITS CHECK FINDS IT READY, sealed at the seat's
// engine-owned name; one at the wrong role is kept nowhere and the box says
// why; a replacement leaves the old one for a person to revoke; a pass that
// finds it refused stops it; and back to the company's gives it up and clears
// the seal.
func TestASeatIdentityIsCheckedBeforeItIsKeptAndOnEveryPass(t *testing.T) {
	t.Parallel()
	v := &identityVendor{roles: map[string]string{"wide": "Admin", "good": "Marketing", "next": "Marketing"}}
	ledger, sink := seataccesstest.NewLedger(), seataccesstest.NewSink()
	c := growthCompany("own", true)
	const sealed = "META_ADS_KEY_GROWTH"

	got := identitySync(t, v, ledger, sink, c, &seataccess.IdentityAsk{Keep: true, Way: seataccess.WayHandedIn,
		Value: "wide", FileName: "id-wide"})
	if got.Kept() || got.Status() != seataccess.IdentityWrongRole {
		t.Fatalf("an Admin key was kept, or not said: %+v", got)
	}
	if _, held := sink.Held(sealed); held {
		t.Fatal("a key that failed its check was sealed")
	}

	got = identitySync(t, v, ledger, sink, c, &seataccess.IdentityAsk{Keep: true, Way: seataccess.WayHandedIn,
		Value: "good", FileName: "id-good"})
	if !got.Kept() || got.Status() != seataccess.IdentityReady || got.Attempt != nil {
		t.Fatalf("a ready key was not kept: %+v", got)
	}
	if value, held := sink.Held(sealed); !held || value != "good" {
		t.Fatalf("the kept key is sealed as %q, %v", value, held)
	}
	if got.Name != "crewlet-growth" {
		t.Fatalf("kept name %q, want the shared slug", got.Name)
	}

	got = identitySync(t, v, ledger, sink, c, &seataccess.IdentityAsk{Keep: true, Way: seataccess.WayHandedIn,
		Value: "next", FileName: "id-next"})
	if got.Status() != seataccess.IdentityReplacing || len(got.Retiring) != 1 || got.Retiring[0].ID != "id-good" {
		t.Fatalf("a replacement does not ask for the old key's revoke: %+v", got)
	}

	delete(v.roles, "next")
	got = identitySync(t, v, ledger, sink, c, nil)
	if got.Status() != seataccess.IdentityRefused {
		t.Fatalf("a pass did not find the revoked key refused: %+v", got)
	}
	if _, err := seataccess.ResolveIdentity(integration.KindMetaAds, ownSpec, "growth",
		seataccess.Desired{Enabled: true, Identity: seataccess.IdentityOwn}, got); err == nil {
		t.Fatal("a refused key still resolves to a signer")
	}

	// A DRY RUN READS ONLY: it gives nothing up, even once the seat is back on
	// the company's.
	back := growthCompany("company", true)
	if got = identitySync(t, v, ledger, nil, back, nil); !got.Kept() || v.dropped != 0 {
		t.Fatalf("a dry run gave the identity up: %+v, dropped %d", got, v.dropped)
	}
	got = identitySync(t, v, ledger, sink, back, nil)
	if got.Kept() || v.dropped != 1 || len(got.Retiring) != 2 {
		t.Fatalf("back to the company's did not give it up and ask for the revoke: %+v", got)
	}
	if _, held := sink.Held(sealed); held {
		t.Fatal("the given-up key is still sealed")
	}
}

// THE BOX WORDS EVERY STATE ONE WAY: who the vendor sees, the one action, and
// what needs a person.
func TestTheBoxSaysWhoTheVendorSees(t *testing.T) {
	t.Parallel()
	v := &identityVendor{}
	seat := seataccess.SeatRef{Handle: "growth", Name: "Growth Marketer"}
	on := seataccess.Desired{Enabled: true}
	company := seataccess.IdentityOf(v, nil, seat, identityLane, on, seataccess.Observed{}, nil, time.Now())
	if !company.Offered || company.Status != seataccess.IdentityCompanys || !company.SetUp || company.Replace {
		t.Fatalf("on the company's: %+v", company)
	}
	own := on
	own.Identity = seataccess.IdentityOwn
	ready := seataccess.IdentityOf(v, nil, seat, identityLane, own, seataccess.Observed{}, &seataccess.IdentityRecord{ID: "K",
		Name: "crewlet-growth", Check: &seataccess.IdentityCheck{Status: seataccess.IdentityReady}}, time.Now())
	if ready.Holds != seataccess.HoldsOwn || !ready.Replace || ready.SetUp || ready.StatusLabel != "Ready" {
		t.Fatalf("ready: %+v", ready)
	}
	refused := seataccess.IdentityOf(v, nil, seat, identityLane, own, seataccess.Observed{}, &seataccess.IdentityRecord{ID: "K",
		Check: &seataccess.IdentityCheck{Status: seataccess.IdentityRefused, Said: "Apple answers 401."}}, time.Now())
	if refused.Holds != seataccess.HoldsNone || !refused.SetUp || !refused.UseCompany || refused.Problem == nil {
		t.Fatalf("refused: %+v", refused)
	}
}

// accountVendor is a lane where each seat holds an account of its own, whose
// robot is its identity.
type accountVendor struct{ *seataccesstest.Vendor }

func (v accountVendor) Catalog() seataccess.Catalog {
	cat := v.Vendor.Catalog()
	for i := range cat.Lanes {
		cat.Lanes[i].Identity = seataccess.IdentitySpec{Ways: []seataccess.IdentityWay{seataccess.WayHandedIn},
			Default: seataccess.IdentityOwn, Name: seatname.Expo}
	}
	return cat
}

// AN ACCOUNT LANE'S IDENTITY IS ITS ACCOUNT: Needs you until its credential is
// sealed, Ready while the vendor holds it, Refused once the vendor shows it
// gone, and never the company's, which such a vendor does not share.
func TestAnAccountLanesIdentityIsItsAccount(t *testing.T) {
	t.Parallel()
	v := accountVendor{seataccesstest.NewVendor(integration.KindExpo, "TOKEN")}
	lane := seataccess.LaneExpo
	seat := seataccess.SeatRef{Handle: "qa", Name: "QA"}
	on := seataccess.Desired{Enabled: true}
	sealed := &seataccess.Credential{Variable: "EXPO_TOKEN_QA"}
	for _, c := range []struct {
		name string
		have seataccess.Observed
		want seataccess.IdentityStatus
		row  seataccess.LaneStatus
	}{
		{"no robot yet", seataccess.Observed{}, seataccess.IdentityNone, seataccess.StatusNeedsYou},
		{"a robot with no token", seataccess.Observed{Exists: true, Enabled: true}, seataccess.IdentityNone,
			seataccess.StatusNeedsYou},
		{"a robot with its token", seataccess.Observed{Exists: true, Enabled: true, Account: "crewlet-qa",
			Credential: sealed}, seataccess.IdentityReady, seataccess.StatusInSync},
		{"its token gone", seataccess.Observed{Exists: true, Enabled: true, Credential: sealed,
			CredentialGone: true}, seataccess.IdentityRefused, seataccess.StatusRefused},
	} {
		id := seataccess.IdentityOf(v, nil, seat, lane, on, c.have, nil, time.Now())
		if id == nil || id.Status != c.want || id.Holds == seataccess.HoldsCompany || id.UseCompany {
			t.Errorf("%s: %+v, want %s and never the company's", c.name, id, c.want)
			continue
		}
		la := seataccess.LaneAnswer{Desired: on, Observed: c.have, Plan: []seataccess.Step{}, Identity: id}
		if got := seataccess.StatusOf(la, false, false); got != c.row {
			t.Errorf("%s: row %s, want %s", c.name, got, c.row)
		}
	}
}

// companyOnlyVendor is a lane whose vendor offers no identity per seat.
type companyOnlyVendor struct{ *seataccesstest.Vendor }

func (v companyOnlyVendor) Catalog() seataccess.Catalog {
	cat := v.Vendor.Catalog()
	for i := range cat.Lanes {
		cat.Lanes[i].Identity = seataccess.CompanyOnly
	}
	return cat
}

// WHERE THE VENDOR OFFERS NO IDENTITY PER SEAT, the company's is the normal
// state: In sync, with nothing to set up; and a lane that declares nothing
// answers no identity at all, so it is never worded as the company's by
// accident.
func TestACompanyOnlyLaneIsInSyncOnTheCompanys(t *testing.T) {
	t.Parallel()
	seat := seataccess.SeatRef{Handle: "growth"}
	on := seataccess.Desired{Enabled: true, Identity: seataccess.IdentityOwn}
	v := companyOnlyVendor{seataccesstest.NewVendor(integration.KindAppleAds)}
	id := seataccess.IdentityOf(v, nil, seat, seataccess.LaneAppleAds, on, seataccess.Observed{}, nil, time.Now())
	if id == nil || id.Offered || id.Status != seataccess.IdentityCompanys || id.SetUp || id.Replace || id.UseCompany {
		t.Fatalf("company-only: %+v", id)
	}
	la := seataccess.LaneAnswer{Desired: on, Plan: []seataccess.Step{}, Identity: id}
	if got := seataccess.StatusOf(la, false, false); got != seataccess.StatusInSync {
		t.Fatalf("company-only row reads %s, want in_sync", got)
	}
	plain := seataccesstest.NewVendor(integration.KindAppleAds)
	if got := seataccess.IdentityOf(plain, nil, seat, seataccess.LaneAppleAds, on, seataccess.Observed{}, nil, time.Now()); got != nil {
		t.Fatalf("a lane that declares nothing answers %+v", got)
	}
}

// A SEAT THAT LEAVES THE DOCUMENT KEEPS ITS RECORD while its identity waits
// for a revoke: the key is given up, its sealed value cleared, and the box's
// ask to revoke it outlives the seat until the vendor refuses the key.
func TestADepartedSeatsIdentityIsGivenUpNotForgotten(t *testing.T) {
	t.Parallel()
	v := &identityVendor{roles: map[string]string{"good": "Marketing"}}
	ledger, sink := seataccesstest.NewLedger(), seataccesstest.NewSink()
	identitySync(t, v, ledger, sink, growthCompany("own", true), &seataccess.IdentityAsk{Keep: true,
		Way: seataccess.WayHandedIn, Value: "good", FileName: "id-good"})
	gone := growthCompany("own", true)
	gone.Roles = nil
	in := seataccess.SyncInput{Vendor: v, Open: seataccess.OpenInput{Config: gone}, Ledger: ledger, Sink: sink,
		Pass: "pass-2", By: "test", Now: func() time.Time { return at }}
	if _, err := seataccess.Sync(t.Context(), in); err != nil {
		t.Fatalf("Sync: %v", err)
	}
	rec, held := ledger.Record(integration.KindMetaAds, "growth")
	if !held {
		t.Fatal("the departed seat's record was dropped while its key waits for a revoke")
	}
	id := rec.Lanes[identityLane].Identity
	if id.Kept() || len(id.Retiring) != 1 || id.Retiring[0].ID != "id-good" || v.dropped != 1 {
		t.Fatalf("departed identity: %+v, dropped %d", id, v.dropped)
	}
	if _, sealed := sink.Held("META_ADS_KEY_GROWTH"); sealed {
		t.Fatal("the departed seat's key is still sealed as its credential")
	}
}
