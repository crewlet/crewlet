package advisor_test

import (
	"errors"
	"reflect"
	"regexp"
	"testing"

	"github.com/crewlet/crewlet/internal/advisor"
	"github.com/crewlet/crewlet/internal/clientsource"
	"github.com/crewlet/crewlet/internal/config"
)

const accessDoc = "name: Acme\nroles:\n" +
	"  - name: Founder\n    kind: human\n    contact:\n      slack_user_id: U0FOUNDER\n" +
	"  - name: PM\n    handle: pm\n    integrations:\n      github:\n        enabled: true\n" +
	"        permissions:\n          contents: write\n          pull_requests: read\n" +
	"  - name: SRE\n    handle: sre\n    health_tools: true\n"

// grants applies a fix's one change as the dashboard sends it, then reports
// whether the seat now holds the access.
func grants(t *testing.T, c *config.Company, handle string, k advisor.AccessKey, fix advisor.Fix) bool {
	t.Helper()
	if len(fix.Options) != 1 {
		t.Fatalf("fix offers %d options, want the one grant", len(fix.Options))
	}
	apply(t, c, fix.Options[0].Change)
	if err := c.ValidateAdmission(); err != nil {
		t.Fatalf("the grant breaks an admission rule: %v", err)
	}
	for role := range c.EachRole() {
		if role.Seat().Handle() == handle {
			return errors.Is(advisor.CanAsk(role.Seat(), k), advisor.ErrAccessHeld)
		}
	}
	return false
}

// EVERY GRANT IS BUILT FOR THE SEAT THAT ASKED, AND DOES WHAT IT SAYS: applied,
// the seat holds the access and the company still admits; nothing else moves.
func TestEveryGrantChangesOnlyWhatItNames(t *testing.T) {
	t.Parallel()
	c := company(t, accessDoc)
	fix, err := advisor.AccessFix(c, "pm", advisor.AccessGitHubPulls)
	if err != nil {
		t.Fatal(err)
	}
	if ch := fix.Options[0].Change; ch.Entity != advisor.EntityRoles || ch.ID != "pm" || ch.Summary == "" {
		t.Errorf("change = %+v, want a write of pm's own entity with a summary", ch)
	}
	// THE ONE FIELD, merged into the entity as the preview reads it: a
	// snapshot of the seat's integrations would revert any change made since.
	want := map[string]any{"integrations": map[string]any{"github": map[string]any{
		"permissions": map[string]any{"pull_requests": "write"}}}}
	if ch := fix.Options[0].Change; len(ch.Set) != 0 || !reflect.DeepEqual(ch.Merge, want) {
		t.Errorf("change sets %v and merges %v, want only %v merged", ch.Set, ch.Merge, want)
	}
	if !grants(t, c, "pm", advisor.AccessGitHubPulls, fix) {
		t.Error("after the change pm still cannot merge")
	}
	for role := range c.EachRole() {
		if role.Seat().Handle() == "pm" &&
			role.Integrations.GitHub.Permissions["contents"] != config.GitHubWrite {
			t.Errorf("the grant changed another permission: %v", role.Integrations.GitHub.Permissions)
		}
	}
	health, err := advisor.AccessFix(company(t, accessDoc), "pm", advisor.AccessHealth)
	if err != nil || !grants(t, company(t, accessDoc), "pm", advisor.AccessHealth, health) {
		t.Errorf("the health grant = %+v, %v, want pm watching the engine's health", health, err)
	}
}

// A GRANT NOBODY COULD MAKE IS REFUSED, each in its own words.
func TestAGrantThatCannotBeMadeIsRefused(t *testing.T) {
	t.Parallel()
	c := company(t, accessDoc)
	for _, tc := range []struct {
		seat string
		key  advisor.AccessKey
		want error
	}{
		{"sre", advisor.AccessHealth, advisor.ErrAccessHeld},
		{"sre", advisor.AccessGitHubPulls, advisor.ErrAccessNotTakable},
		{"pm", "grant-everything", advisor.ErrAccessUnknown},
	} {
		if _, err := advisor.AccessFix(c, tc.seat, tc.key); !errors.Is(err, tc.want) {
			t.Errorf("AccessFix(%s, %s) = %v, want %v", tc.seat, tc.key, err, tc.want)
		}
	}
	if _, err := advisor.AccessFix(c, "founder", advisor.AccessHealth); err == nil {
		t.Error("a person's seat was offered an agent's grant")
	}
}

// EVERY KEY IS AN OPTION ID A DECISION TAKES, and names itself as a grant.
func TestEveryAccessKeyIsAnOptionID(t *testing.T) {
	t.Parallel()
	option := regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)
	for _, k := range advisor.AccessKeys() {
		if !option.MatchString(string(k)) || !advisor.IsAccessOption(string(k)) || k.Does() == "" {
			t.Errorf("%q is no option id a decision takes, or says nothing", k)
		}
	}
	if advisor.IsAccessOption("decline") || advisor.IsAccessOption("grant-everything") {
		t.Error("an id outside the catalogue reads as a grant")
	}
}

// THE DASHBOARD TELLS A GRANT OPTION BY THE ENGINE'S OWN PREFIX: a choice it
// misread would be answered without the grant being previewed and saved.
func TestTheDashboardKnowsAGrantOptionByTheEnginesPrefix(t *testing.T) {
	t.Parallel()
	got, err := clientsource.Scalar(clientsource.Tree(t), "ACCESS_OPTION_PREFIX")
	if err != nil {
		t.Fatal(err)
	}
	if got != advisor.AccessPrefix {
		t.Errorf("the dashboard's prefix is %q, the engine's %q", got, advisor.AccessPrefix)
	}
}
