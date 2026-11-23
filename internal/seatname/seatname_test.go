package seatname_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/seatname"
)

var rules = map[string]seatname.Rule{
	"app_store_connect": seatname.AppStoreConnect,
	"meta_ads":          seatname.MetaAds,
	"expo":              seatname.Expo,
	"digitalocean":      seatname.DigitalOcean,
	"google":            seatname.Google,
	"gitlab":            seatname.GitLab,
	"cloudflare":        seatname.Cloudflare,
	"scaleway":          seatname.Scaleway,
	"mattermost":        seatname.Mattermost,
	"datadog":           seatname.Datadog,
	"underscores":       {What: "Name", Min: 3, Max: 30, Underscores: true, LetterFirst: true},
}

// Every name the function answers passes its vendor's rule, whatever the
// handle: capitals, spaces, dots, accents, a handle too long, or none at all.
func TestEveryNameKeepsItsVendorsRule(t *testing.T) {
	handles := []string{
		"growth", "Growth Marketer", "qa.lead", "a", "--", "", "Ünïcode",
		strings.Repeat("very-long-handle-", 6), "UPPER_snake", "x1", "9lives",
	}
	for vendor, rule := range rules {
		for _, h := range handles {
			name := seatname.For(rule, "Example Co", h)
			if err := rule.Check(name); err != nil {
				t.Errorf("%s, handle %q: %v", vendor, h, err)
			}
			if !strings.HasPrefix(name, seatname.Prefix) {
				t.Errorf("%s, handle %q: %q does not begin with %q", vendor, h, name, seatname.Prefix)
			}
		}
	}
}

// The name is crewlet-<handle> where the vendor's names are the company's
// own: no company, no hash, nothing to strip by hand.
func TestTheNameIsCrewletAndTheHandle(t *testing.T) {
	for _, c := range []struct {
		rule         seatname.Rule
		handle, want string
	}{
		{seatname.AppStoreConnect, "growth", "crewlet-growth"},
		{seatname.MetaAds, "Growth Marketer", "crewlet-growth-marketer"},
		{seatname.Expo, "qa.lead", "crewlet-qa-lead"},
		{rules["underscores"], "growth marketer", "crewlet_growth_marketer"},
		{seatname.AppStoreConnect, "!!", "crewlet-agent"},
	} {
		if got := seatname.For(c.rule, "Example Co", c.handle); got != c.want {
			t.Errorf("For(%s, %q) = %q, want %q", c.rule.What, c.handle, got, c.want)
		}
	}
}

// A shared namespace keeps two companies' seats apart: the same handle in two
// companies is two names, and one company's seat is one name every time.
func TestAHashedNameTellsCompaniesApart(t *testing.T) {
	a := seatname.For(seatname.Google, "Example Co", "growth")
	b := seatname.For(seatname.Google, "Other Co", "growth")
	if a == b {
		t.Fatalf("two companies' growth seats share %q", a)
	}
	if again := seatname.For(seatname.Google, "Example Co", "growth"); again != a {
		t.Fatalf("one seat named twice: %q then %q", a, again)
	}
	if !strings.HasPrefix(a, "crewlet-growth-") || len(a) != len("crewlet-growth-")+6 {
		t.Fatalf("Google name %q is not crewlet-growth-<6 hex>", a)
	}
	long := seatname.For(seatname.Google, "Example Co", strings.Repeat("handle", 10))
	if len(long) > 30 || strings.Contains(long, "--") {
		t.Fatalf("cut Google name %q breaks the cap or leaves a double hyphen", long)
	}
}

// Check refuses what each vendor refuses, so a test or a sheet can hold any
// name to it.
func TestCheckRefusesWhatTheVendorRefuses(t *testing.T) {
	for _, bad := range []string{"Crewlet-growth", "crewlet growth", "crewlet.growth", "crewlet-", "-crewlet",
		"ab", strings.Repeat("a", 31), "1crewlet"} {
		if seatname.Crewlet.Check(bad) == nil {
			t.Errorf("Crewlet's cap allowed %q", bad)
		}
	}
	if seatname.Google.Check("crewl") == nil {
		t.Error("Google allowed a five-character ID")
	}
}

// Each vendor's length is the one it publishes, or Crewlet's cap: a longer
// handle is cut to exactly that, and never past it.
func TestEachVendorCutsToItsOwnLength(t *testing.T) {
	long := strings.Repeat("handle", 50)
	for _, c := range []struct {
		rule seatname.Rule
		max  int
	}{
		{seatname.GitLab, 30}, {seatname.Expo, 30}, {seatname.DigitalOcean, 30},
		{seatname.Cloudflare, 120}, {seatname.Scaleway, 64},
		{seatname.Mattermost, 255}, {seatname.Datadog, 64},
	} {
		if got := seatname.For(c.rule, "Example Co", long); len(got) != c.max {
			t.Errorf("%s: a %d-character handle gave %d characters, want the vendor's %d",
				c.rule.What, len(long), len(got), c.max)
		}
	}
}

// Words after the handle survive a cut: a seat's tokens at two levels stay
// two names a person tells apart, the handle giving way rather than the level.
func TestWordsAfterTheHandleSurviveTheCut(t *testing.T) {
	if got := seatname.ForWith(seatname.DigitalOcean, "Example Co", "growth", "Operate"); got != "crewlet-growth-operate" {
		t.Fatalf("ForWith = %q, want crewlet-growth-operate", got)
	}
	long := strings.Repeat("handle", 10)
	read := seatname.ForWith(seatname.DigitalOcean, "Example Co", long, "read")
	admin := seatname.ForWith(seatname.DigitalOcean, "Example Co", long, "administer")
	if !strings.HasSuffix(read, "-read") || !strings.HasSuffix(admin, "-administer") || len(admin) != 30 {
		t.Fatalf("cut names %q and %q lost their level or the cap", read, admin)
	}
	if got := seatname.ForWith(seatname.Crewlet, "Example Co", "growth", strings.Repeat("word", 20)); seatname.Crewlet.Check(got) != nil ||
		!strings.HasPrefix(got, "crewlet-growth-word") {
		t.Fatalf("words longer than the cap gave %q", got)
	}
	if got := seatname.ForWith(seatname.Expo, "Example Co", "growth", "", "--"); got != "crewlet-growth" {
		t.Fatalf("empty words gave %q, want crewlet-growth", got)
	}
}
