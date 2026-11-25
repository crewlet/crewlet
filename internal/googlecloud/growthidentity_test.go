package googlecloud_test

import (
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/googlecloud"
	"github.com/crewlet/crewlet/internal/integration"
)

// growth converges the identities of seats on w with store, failing the test
// on an error.
func (w *clientWorld) growth(t *testing.T, store *memory.Fleet, seats ...googlecloud.GrowthSeat) []integration.Finding {
	t.Helper()
	held, err := store.ListGoogleIdentities(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	findings, err := googlecloud.ConvergeGrowthIdentities(t.Context(), w.c, testCompany, seats, held, store)
	if err != nil {
		t.Fatalf("converge: %v", err)
	}
	return findings
}

// THE GROWTH IDENTITY IS NEVER THE CLOUD IDENTITY: a seat on a growth lane
// gets an account of its own, `crewlet-<handle>-<6 hex>`, recorded by Google's unique ID,
// that the provisioner may mint for, and the seat's Google Cloud account is
// neither made nor touched.
//
// Mutation: name it AccountID (the cloud one), and the two collide.
func TestASeatsGrowthIdentityIsItsOwnAccountRecordedByUniqueID(t *testing.T) {
	t.Parallel()
	w := newClientWorld(t)
	store := memory.NewFleet()
	if f := w.growth(t, store, googlecloud.GrowthSeat{Handle: "growth-lead", On: true}); len(f) != 0 {
		t.Fatalf("findings %+v", f)
	}
	rec, found, err := store.GoogleIdentity(t.Context(), "growth-lead")
	if err != nil || !found || !rec.Usable() {
		t.Fatalf("record %+v found=%v %v, want a usable identity", rec, found, err)
	}
	wantID := googleauth.GrowthAccountID(testCompany, "growth-lead")
	if !strings.HasPrefix(wantID, "crewlet-growth-lead-") || rec.Email != googleauth.AccountEmail(wantID, identity) {
		t.Errorf("identity %s, want %s in the identity project", rec.Email, wantID)
	}
	if wantID == googlecloud.AccountID(testCompany, "growth-lead") {
		t.Error("the growth identity's ID is the cloud identity's")
	}
	sa, found, err := w.c.ServiceAccount(t.Context(), rec.UniqueID)
	if err != nil || !found || sa.Email != rec.Email {
		t.Fatalf("the recorded unique ID reads %+v found=%v %v", sa, found, err)
	}
	if _, _, err := w.c.Connection().SeatToken(t.Context(), rec.UniqueID, cloudPlatform, time.Hour); err != nil {
		t.Errorf("the provisioner may not mint for the identity: %v", err)
	}
	if _, found, _ := w.c.ServiceAccount(t.Context(),
		googleauth.AccountEmail(googlecloud.AccountID(testCompany, "growth-lead"), identity)); found {
		t.Error("the seat's Google Cloud account was made")
	}
}

// OFF WHEN THE LAST LANE IS, ON AGAIN WITHOUT A NEW ACCOUNT, KEPT OFF WHEN THE
// SEAT GOES: the record's account is disabled and enabled, never replaced, and
// a removed seat's stays at Google, off, with the roles people gave it.
//
// Mutation: create where the record is disabled, and turning a lane on again
// makes a second account the person never added to any product.
func TestAGrowthIdentityFollowsTheOROfTheSeatsLanes(t *testing.T) {
	t.Parallel()
	w := newClientWorld(t)
	store := memory.NewFleet()
	w.growth(t, store, googlecloud.GrowthSeat{Handle: "growth-lead", On: true})
	first, _, _ := store.GoogleIdentity(t.Context(), "growth-lead")

	w.growth(t, store, googlecloud.GrowthSeat{Handle: "growth-lead", On: false})
	off, _, _ := store.GoogleIdentity(t.Context(), "growth-lead")
	if sa, _, _ := w.c.ServiceAccount(t.Context(), first.UniqueID); !sa.Disabled || !off.Disabled || off.Usable() {
		t.Fatalf("after the last lane went off: account disabled=%v, record %+v", sa.Disabled, off)
	}

	w.growth(t, store, googlecloud.GrowthSeat{Handle: "growth-lead", On: true})
	on, _, _ := store.GoogleIdentity(t.Context(), "growth-lead")
	if sa, _, _ := w.c.ServiceAccount(t.Context(), first.UniqueID); sa.Disabled || on.UniqueID != first.UniqueID || !on.Usable() {
		t.Fatalf("after a lane came back on: account disabled=%v, record %+v, want the first account on", sa.Disabled, on)
	}

	w.growth(t, store)
	removed, found, _ := store.GoogleIdentity(t.Context(), "growth-lead")
	if sa, atGoogle, _ := w.c.ServiceAccount(t.Context(), first.UniqueID); !found || !atGoogle || !sa.Disabled ||
		!removed.Disabled {
		t.Errorf("a removed seat's identity: recorded=%v at Google=%v disabled=%v, want kept and off",
			found, atGoogle, sa.Disabled)
	}
}

// NEVER ADOPTED BY NAME, NEVER TURNED BACK ON UNASKED: an account under the
// identity's ID that Crewlet did not record is a person's to delete, and one a
// person turned off at Google stays off with a finding.
func TestAGrowthIdentityIsNeverAdoptedNorReenabledUnasked(t *testing.T) {
	t.Parallel()
	w := newClientWorld(t)
	id := googleauth.GrowthAccountID(testCompany, "growth-lead")
	if _, err := w.c.CreateServiceAccount(t.Context(), id, "squatter", "made by hand"); err != nil {
		t.Fatal(err)
	}
	store := memory.NewFleet()
	f := w.growth(t, store, googlecloud.GrowthSeat{Handle: "growth-lead", On: true})
	if len(f) != 1 || f[0].Kind != integration.FindingIdentityFailed || !strings.Contains(f[0].Detail, "no record") {
		t.Fatalf("findings %+v, want the unrecorded account named", f)
	}
	if _, found, _ := store.GoogleIdentity(t.Context(), "growth-lead"); found {
		t.Fatal("an account Crewlet did not make was recorded")
	}

	w2 := newClientWorld(t)
	store2 := memory.NewFleet()
	w2.growth(t, store2, googlecloud.GrowthSeat{Handle: "growth-lead", On: true})
	rec, _, _ := store2.GoogleIdentity(t.Context(), "growth-lead")
	if err := w2.c.DisableServiceAccount(t.Context(), rec.UniqueID); err != nil {
		t.Fatal(err)
	}
	f = w2.growth(t, store2, googlecloud.GrowthSeat{Handle: "growth-lead", On: true})
	if len(f) != 1 || f[0].Kind != integration.FindingApprovalRequired {
		t.Fatalf("findings %+v, want a person's step for an account turned off at Google", f)
	}
	if sa, _, _ := w2.c.ServiceAccount(t.Context(), rec.UniqueID); !sa.Disabled {
		t.Error("an account a person turned off was turned back on")
	}
	if !slices.ContainsFunc(f, func(x integration.Finding) bool { return strings.Contains(x.Subject, "growth-lead") }) {
		t.Errorf("the finding %+v does not name the seat", f)
	}
}

// A RECORDED GROWTH IDENTITY IS CREWLET'S OWN ACCOUNT: the Google Cloud pass's
// orphan check spares it, and still names one Crewlet has no record of.
//
// Mutation: drop Options.Growth from the recorded set, and every seat's growth
// identity is reported as an orphan for a person to delete.
func TestARecordedGrowthIdentityIsNotAnOrphan(t *testing.T) {
	t.Parallel()
	w := newClientWorld(t)
	store := memory.NewFleet()
	w.growth(t, store, googlecloud.GrowthSeat{Handle: "growth-lead", On: true},
		googlecloud.GrowthSeat{Handle: "mobile-lead", On: true})
	held, err := store.ListGoogleIdentities(t.Context())
	if err != nil || len(held) != 2 {
		t.Fatalf("identities %+v, %v", held, err)
	}
	// mobile-lead's record is lost: its account is no longer Crewlet's to know.
	if _, err := store.DeleteGoogleIdentity(t.Context(), "mobile-lead", held[1].Version); err != nil {
		t.Fatal(err)
	}
	res, err := googlecloud.Reconcile(t.Context(), googlecloud.Options{
		Client: w.c, Company: testCompany, Growth: held[:1],
	})
	if err != nil {
		t.Fatal(err)
	}
	var orphaned []string
	for _, f := range res.Findings() {
		if f.Kind == integration.FindingRegistrationOrphaned {
			orphaned = f.Subjects
		}
	}
	if slices.Contains(orphaned, held[0].Email) || !slices.Contains(orphaned, held[1].Email) {
		t.Errorf("orphaned %v: want %s alone, never the recorded %s", orphaned, held[1].Email, held[0].Email)
	}
}

// THE BLOCK NEVER CARRIES A PRODUCT: no reader, no product API, so turning a
// Google product on never changes what a person ran. The reader is one per
// company, within Google's 30 characters.
//
// Mutation: name the reader by a fixed ID, and two companies sharing an
// identity project read each other's figures.
func TestTheCloudShellBlockNeverCarriesAGrowthProduct(t *testing.T) {
	t.Parallel()
	_, pemText := fakeKey(t)
	text, err := googlecloud.CloudShellBlock(testCompany, identity, pemText, true)
	if err != nil {
		t.Fatal(err)
	}
	reader := googleauth.ReaderAccountID(testCompany)
	for _, never := range []string{reader, "READER=", "analytics", "androidpublisher", "searchconsole", "googleads"} {
		if strings.Contains(text, never) {
			t.Errorf("the block names %q", never)
		}
	}
	if other := googleauth.ReaderAccountID("Other Corp"); other == reader || len(reader) > 30 {
		t.Errorf("readers %q and %q: want one per company, within Google's 30 characters", reader, other)
	}
}

// A PASS CUT SHORT LEAVES AN ACCOUNT CREWLET KNOWS IS ITS OWN: the intent is
// recorded by email before Google is asked, so an account Google made whose
// unique ID was never written is recorded on the next pass, never reported as
// a stranger's, and an off seat's is recorded and turned off.
//
// Mutation: create the account before recording the intent, and a pass cut
// short between the two leaves the seat blocked on its own account.
func TestAnAccountMadeByAPassCutShortIsRecordedNeverOrphaned(t *testing.T) {
	t.Parallel()
	for _, on := range []bool{true, false} {
		w := newClientWorld(t)
		store := memory.NewFleet()
		id := googleauth.GrowthAccountID(testCompany, "growth-lead")
		email := googleauth.AccountEmail(id, identity)
		if _, _, err := store.CreateGoogleIdentity(t.Context(), coord.GoogleIdentity{
			Handle: "growth-lead", Email: email, CreatedAt: w.clock.now(),
		}); err != nil {
			t.Fatal(err)
		}
		made, err := w.c.CreateServiceAccount(t.Context(), id, "Crewlet Acme growth-lead growth",
			googlecloud.GrowthDescription(testCompany, "growth-lead"))
		if err != nil {
			t.Fatal(err)
		}
		held, _ := store.ListGoogleIdentities(t.Context())
		res, err := googlecloud.Reconcile(t.Context(), googlecloud.Options{Client: w.c, Company: testCompany, Growth: held})
		if err != nil {
			t.Fatal(err)
		}
		for _, f := range res.Findings() {
			if f.Kind == integration.FindingRegistrationOrphaned {
				t.Errorf("on=%v: the account its own intent names is an orphan: %+v", on, f)
			}
		}
		if f := w.growth(t, store, googlecloud.GrowthSeat{Handle: "growth-lead", On: on}); len(f) != 0 {
			t.Fatalf("on=%v: findings %+v, want the account taken as Crewlet's", on, f)
		}
		rec, _, _ := store.GoogleIdentity(t.Context(), "growth-lead")
		sa, _, _ := w.c.ServiceAccount(t.Context(), made.UniqueID)
		if rec.UniqueID != made.UniqueID || rec.Usable() != on || sa.Disabled == on {
			t.Errorf("on=%v: record %+v, account disabled=%v", on, rec, sa.Disabled)
		}
	}
}

// THE ENGINE'S OWN DISABLE IS RECORDED FIRST: a record that says off over an
// account still on is a pass cut short, finished by turning it off, never a
// person's change.
func TestATurnOffCutShortIsFinishedNotReportedAsAPersons(t *testing.T) {
	t.Parallel()
	w := newClientWorld(t)
	store := memory.NewFleet()
	w.growth(t, store, googlecloud.GrowthSeat{Handle: "growth-lead", On: true})
	rec, _, _ := store.GoogleIdentity(t.Context(), "growth-lead")
	rec.Disabled = true
	if _, _, err := store.UpdateGoogleIdentity(t.Context(), rec); err != nil {
		t.Fatal(err)
	}
	if f := w.growth(t, store, googlecloud.GrowthSeat{Handle: "growth-lead", On: false}); len(f) != 0 {
		t.Fatalf("findings %+v", f)
	}
	if sa, _, _ := w.c.ServiceAccount(t.Context(), rec.UniqueID); !sa.Disabled {
		t.Error("an off seat's account the record says is off was left on")
	}
	if f := w.growth(t, store, googlecloud.GrowthSeat{Handle: "growth-lead", On: true}); len(f) != 0 {
		t.Fatalf("turning the lane back on: findings %+v, want the engine's own disable undone", f)
	}
}
