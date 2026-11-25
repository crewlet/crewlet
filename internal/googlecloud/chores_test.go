package googlecloud_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/googlecloud"
	"github.com/crewlet/crewlet/internal/integration"
)

// testAnalytics is a growth product as its package registers it.
var testAnalytics = googleauth.Product{Kind: "test_analytics", Name: "Test Analytics",
	APIs: []string{"analyticsdata.googleapis.com", "analyticsadmin.googleapis.com"}}

// chores runs the products' chores on w, failing the test on an error.
func (w *clientWorld) chores(t *testing.T, products ...googleauth.Product) []integration.Finding {
	t.Helper()
	findings, err := googlecloud.ConvergeGrowthProducts(t.Context(), w.c, testCompany, products)
	if err != nil {
		t.Fatalf("chores: %v", err)
	}
	return findings
}

// TURNING A PRODUCT ON NEEDS NO BLOCK: Crewlet turns on its APIs and makes the
// company reader, trusted by the company's federated subject alone, and a
// second pass writes nothing.
//
// Mutation: skip the enable or the reader's trust, and a check fails.
func TestCrewletTurnsOnAProductsAPIsAndMakesTheReader(t *testing.T) {
	t.Parallel()
	w := newClientWorld(t)
	if f := w.chores(t); len(f) != 0 || len(w.f.wrote()) != 0 {
		t.Fatalf("with no product on: findings %+v, writes %q; want none", f, w.f.wrote())
	}
	if f := w.chores(t, testAnalytics); len(f) != 0 {
		t.Fatalf("findings %+v", f)
	}
	w.f.edit(func(f *fakeGoogle) {
		for _, api := range testAnalytics.APIs {
			if !f.projects[identity].Services[api] {
				t.Errorf("%s is not on in the identity project", api)
			}
		}
	})
	email := googleauth.AccountEmail(googleauth.ReaderAccountID(testCompany), identity)
	reader, found := w.f.account(email)
	if !found || !strings.Contains(reader.Description, "for Crewlet") {
		t.Fatalf("the reader is %+v (found %v)", reader, found)
	}
	principal := googleauth.Principal(testProvider, testCompany)
	if !slices.ContainsFunc(reader.Policy.Bindings, func(b googlecloud.Binding) bool {
		return b.Role == "roles/iam.workloadIdentityUser" && slices.Equal(b.Members, []string{principal})
	}) {
		t.Errorf("the reader trusts %+v, want the company's principal alone", reader.Policy.Bindings)
	}
	before := len(w.f.wrote())
	if f := w.chores(t, testAnalytics); len(f) != 0 || len(w.f.wrote()) != before {
		t.Errorf("a second pass: findings %+v, writes %q", f, w.f.wrote()[before:])
	}
}

// A COMPANY CONNECTED BY AN EARLIER BLOCK IS TOLD ONCE TO RUN IT AGAIN: one
// finding whether or not a product waits, nothing turned on, and the product
// pointed at the same fix.
//
// Mutation: try the enable anyway, or raise a finding per product, and this
// fails.
func TestAnOldBlockIsToldOnceToRunTheBlockAgain(t *testing.T) {
	t.Parallel()
	w := newClientWorld(t)
	w.f.edit(func(f *fakeGoogle) { f.oldBlock = true })
	for _, products := range [][]googleauth.Product{nil, {testAnalytics, {Kind: "test_play", Name: "Test Play",
		APIs: []string{"androidpublisher.googleapis.com"}}}} {
		f := w.chores(t, products...)
		again := slices.IndexFunc(f, func(x integration.Finding) bool { return x.Title == "Run the Cloud Shell block again, once" })
		if again < 0 || slices.ContainsFunc(f[again+1:], func(x integration.Finding) bool { return x.Title == f[again].Title }) ||
			f[again].Kind != integration.FindingApprovalRequired {
			t.Fatalf("%d products: findings %+v, want one run-again finding", len(products), f)
		}
	}
	for _, write := range w.f.wrote() {
		if strings.HasPrefix(write, "enable ") {
			t.Errorf("an old block's provisioner turned APIs on: %q", write)
		}
	}
	may, err := googlecloud.MayEnableAPIs(t.Context(), w.c)
	if err != nil || may {
		t.Errorf("an old block's provisioner may enable %v (%v)", may, err)
	}
}

// A READER CREWLET DID NOT MAKE IS NEVER ADOPTED: an account by the reader's
// name that does not say Crewlet made it is a finding, and nobody is let act
// as it.
func TestAReaderCrewletDidNotMakeIsNeverAdopted(t *testing.T) {
	t.Parallel()
	w := newClientWorld(t)
	email := googleauth.AccountEmail(googleauth.ReaderAccountID(testCompany), identity)
	w.f.edit(func(f *fakeGoogle) {
		f.accounts["100000000000000000009"] = &fakeAccount{ID: googleauth.ReaderAccountID(testCompany), Email: email,
			UniqueID: "100000000000000000009", Description: "Somebody's own reports bot"}
	})
	f := w.chores(t, testAnalytics)
	if len(f) != 1 || f[0].Kind != integration.FindingIdentityFailed || !strings.Contains(f[0].Detail, email) {
		t.Fatalf("findings %+v, want the reader named as not Crewlet's", f)
	}
	if slices.Contains(w.f.wrote(), "set the policy of "+email) {
		t.Error("Crewlet let itself act as an account it did not make")
	}
}

// THE READER AN EARLIER BLOCK MADE IS CREWLET'S, so a company connected before
// keeps it: its trust is checked and nothing is made again.
func TestTheReaderAnEarlierBlockMadeIsKept(t *testing.T) {
	t.Parallel()
	w := newClientWorld(t)
	email := googleauth.AccountEmail(googleauth.ReaderAccountID(testCompany), identity)
	w.f.edit(func(f *fakeGoogle) {
		f.accounts["100000000000000000009"] = &fakeAccount{ID: googleauth.ReaderAccountID(testCompany), Email: email,
			UniqueID: "100000000000000000009", Description: "Reads growth figures for the whole company, for Crewlet",
			Policy: googlecloud.Policy{Etag: "a0", Bindings: []googlecloud.Binding{{Role: "roles/iam.workloadIdentityUser",
				Members: []string{googleauth.Principal(testProvider, testCompany)}}}}}
	})
	if f := w.chores(t, testAnalytics); len(f) != 0 {
		t.Fatalf("findings %+v", f)
	}
	for _, write := range w.f.wrote() {
		if strings.Contains(write, email) {
			t.Errorf("the kept reader was written: %q", write)
		}
	}
}
