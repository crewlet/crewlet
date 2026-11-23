package googleauth_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/googleauth"
	"github.com/crewlet/crewlet/internal/googlecloud"
	"github.com/crewlet/crewlet/internal/seatname"
)

// EVERY ID THE ENGINE MAKES FITS GOOGLE'S RULE AND NAMES WHAT IT IS FOR: 6 to
// 30 lowercase letters, digits and dashes, a seat's growth identity distinct
// from another company's seat of the same handle, from another handle that
// slugs alike and from the seat's Google Cloud account, and the reader
// distinct per company.
func TestEveryIdentityIDFitsGooglesRuleAndNeverCollides(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("growth-marketing-lead-", 4)
	ids := map[string]string{
		"a seat":                   googleauth.GrowthAccountID("Acme", "growth-lead"),
		"the same seat elsewhere":  googleauth.GrowthAccountID("Other", "growth-lead"),
		"a long handle":            googleauth.GrowthAccountID("Acme", long),
		"a long handle, one more":  googleauth.GrowthAccountID("Acme", long+"x"),
		"a handle with no letter":  googleauth.GrowthAccountID("Acme", "!!"),
		"the seat's cloud account": googlecloud.AccountID("Acme", "growth-lead"),
		"the reader":               googleauth.ReaderAccountID("Acme"),
		"another company's reader": googleauth.ReaderAccountID("Other"),
		"a long company's reader":  googleauth.ReaderAccountID(strings.Repeat("Acme Holdings ", 5)),
	}
	seen := map[string]string{}
	for what, id := range ids {
		if len(id) < 6 || len(id) > 30 || strings.Trim(id, "abcdefghijklmnopqrstuvwxyz0123456789-") != "" ||
			strings.HasSuffix(id, "-") {
			t.Errorf("%s: %q is not a service account ID Google accepts", what, id)
		}
		if prior, dup := seen[id]; dup {
			t.Errorf("%s and %s share %q", what, prior, id)
		}
		seen[id] = what
	}
	if want := seatname.For(seatname.Google, "Acme", "growth-lead"); ids["a seat"] != want {
		t.Errorf("a seat's growth identity is %q, not the name every seat's identity takes, %q", ids["a seat"], want)
	}
	if !strings.HasPrefix(ids["the reader"], "crewlet-gr-") {
		t.Errorf("the reader %q does not say what it is", ids["the reader"])
	}
}

// A PRODUCT REGISTERS WHOLE AND ONCE, and EnabledProducts is what a company
// turns on.
func TestAProductRegistersWholeAndOnce(t *testing.T) {
	t.Parallel()
	whole := googleauth.Product{Kind: "test_registry", Name: "Test", APIs: []string{"test.googleapis.com"},
		Scopes: map[string][]string{"read": {analytics}}, ReaderScopes: []string{analytics},
		On: func(c *config.Company) bool { return c.Name == "On Corp" }}
	panics := func(p googleauth.Product) (did bool) {
		defer func() { did = recover() != nil }()
		googleauth.Register(p)
		return false
	}
	partial := whole
	partial.Kind, partial.ReaderScopes = "test_partial", nil
	if !panics(partial) {
		t.Error("a product without reader scopes registered")
	}
	if panics(whole) {
		t.Fatal("a whole product did not register")
	}
	if !panics(whole) {
		t.Error("a product registered twice")
	}
	if got := googleauth.EnabledProducts(&config.Company{Name: "On Corp"}); len(got) != 1 || got[0].Kind != whole.Kind {
		t.Errorf("enabled products %+v", got)
	}
	if got := googleauth.EnabledProducts(&config.Company{Name: "Off Corp"}); len(got) != 0 {
		t.Errorf("a company with the product off has %+v", got)
	}
}
