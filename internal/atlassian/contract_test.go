package atlassian_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/atlassian"
)

func TestAllowedAndForbiddenNeverOverlap(t *testing.T) {
	t.Parallel()
	// A permission on both lists would be reported as missing when the
	// agent lacks it AND as excess when it has it, so the container could
	// never be clean and the report would never converge.
	for _, product := range atlassian.Products {
		allowed := atlassian.Allowed(product)
		for _, name := range atlassian.Forbidden(product) {
			if slices.Contains(allowed, name) {
				t.Errorf("%s: %q is both granted and forbidden", product, name)
			}
		}
		if len(allowed) == 0 || len(atlassian.Forbidden(product)) == 0 {
			t.Errorf("%s: a contract with an empty half checks nothing", product)
		}
	}
}

func TestOneQueryAsksForBothHalves(t *testing.T) {
	t.Parallel()
	// Asked together on purpose. A second call for the forbidden half is
	// the call that gets dropped, and its absence then reads as "nothing
	// forbidden was granted" — which is the reassuring answer and the
	// wrong one.
	for _, product := range atlassian.Products {
		query := atlassian.PermissionQuery(product)
		for _, name := range append(atlassian.Allowed(product), atlassian.Forbidden(product)...) {
			if !slices.Contains(query, name) {
				t.Errorf("%s: the permission query omits %q", product, name)
			}
		}
	}
}

func TestClassifySeparatesWhatIsMissingFromWhatIsExcess(t *testing.T) {
	t.Parallel()
	// The two call for OPPOSITE responses — missing is the operator's to
	// grant, excess is only theirs to revoke — so a single "wrong" count
	// would send them to the same screen and be right about half the time.
	held := map[string]bool{}
	for _, name := range atlassian.Allowed(atlassian.ProductJira) {
		held[name] = true
	}
	held["BROWSE_PROJECTS"] = false
	held["DELETE_ISSUES"] = true

	missing, excess := atlassian.Classify(atlassian.ProductJira, held)
	if !slices.Equal(missing, []string{"BROWSE_PROJECTS"}) {
		t.Errorf("missing = %v", missing)
	}
	if !slices.Equal(excess, []string{"DELETE_ISSUES"}) {
		t.Errorf("excess = %v", excess)
	}

	// A container that holds every contract permission and none of the
	// forbidden ones reports nothing at all.
	clean := map[string]bool{}
	for _, name := range atlassian.Allowed(atlassian.ProductJira) {
		clean[name] = true
	}
	if missing, excess := atlassian.Classify(atlassian.ProductJira, clean); len(missing)+len(excess) != 0 {
		t.Errorf("a fully granted container reported missing=%v excess=%v", missing, excess)
	}
}

func TestScopesAreOrderStableWhateverOrderTheCallerAsksIn(t *testing.T) {
	t.Parallel()
	// Two scope lists are compared element by element, so an order that
	// followed whoever asked would make the same credential look different
	// to two readers — and a provisioner would re-mint on every run.
	forward := atlassian.Scopes([]atlassian.Product{
		atlassian.ProductJira, atlassian.ProductConfluence,
	})
	backward := atlassian.Scopes([]atlassian.Product{
		atlassian.ProductConfluence, atlassian.ProductJira,
	})
	if !slices.Equal(forward, backward) {
		t.Fatalf("scopes are caller-ordered:\n  %v\n  %v", forward, backward)
	}
}

func TestAJiraOnlySeatHoldsNoConfluenceScope(t *testing.T) {
	t.Parallel()
	// The whole reason scopes are per-product: a documentation agent must
	// hold no credential that can move a sprint, and a tracker agent must
	// hold none that can write the wiki.
	jira := atlassian.Scopes([]atlassian.Product{atlassian.ProductJira})
	for _, scope := range jira {
		if strings.Contains(scope, "confluence") {
			t.Errorf("a Jira-only seat was given %q", scope)
		}
	}
	wiki := atlassian.Scopes([]atlassian.Product{atlassian.ProductConfluence})
	for _, scope := range wiki {
		if strings.Contains(scope, "jira") {
			t.Errorf("a Confluence-only seat was given %q", scope)
		}
	}
	if len(atlassian.Scopes(nil)) != 0 {
		t.Error("a seat with no products was given scopes")
	}
}

func TestTheJiraLicenceUsesTheSoftwareARI(t *testing.T) {
	t.Parallel()
	// THE SILENT ONE. The plain `jira` ARI is ACCEPTED by the grant
	// endpoint and grants nothing — a run that reports success and agents
	// that can do nothing, with no error anywhere to explain it.
	if got := atlassian.MemberRoleARI(atlassian.ProductJira); got != "ari:cloud:jira-software::role/product/member" {
		t.Errorf("jira role ARI = %q", got)
	}
	if got := atlassian.SiteResourceARI(atlassian.ProductJira, "cloud-1"); got != "ari:cloud:jira-software::site/cloud-1" {
		t.Errorf("jira site ARI = %q", got)
	}
	if got := atlassian.MemberRoleARI(atlassian.ProductConfluence); got != "ari:cloud:confluence::role/product/member" {
		t.Errorf("confluence role ARI = %q", got)
	}
	if got := atlassian.SiteResourceARI(atlassian.ProductConfluence, "cloud-1"); got != "ari:cloud:confluence::site/cloud-1" {
		t.Errorf("confluence site ARI = %q", got)
	}
}

func TestConfluenceIsAddressedUnderItsOwnGatewayAndWiki(t *testing.T) {
	t.Parallel()
	// Under /ex/jira, Confluence answers 401 as though the token lacked
	// the scope rather than as though the address were wrong — which is a
	// misreading that costs an afternoon.
	got := atlassian.ProductBase("https://api.atlassian.com", atlassian.ProductConfluence, "cloud-1")
	if got != "https://api.atlassian.com/ex/confluence/cloud-1/wiki/rest/api" {
		t.Errorf("confluence base = %q", got)
	}
	got = atlassian.ProductBase("https://api.atlassian.com", atlassian.ProductJira, "cloud-1")
	if got != "https://api.atlassian.com/ex/jira/cloud-1/rest/api/3" {
		t.Errorf("jira base = %q", got)
	}
}

func TestAnUnknownProductIsAValueRatherThanAPanic(t *testing.T) {
	t.Parallel()
	if atlassian.Product("bitbucket").Valid() {
		t.Error("an unserved product was accepted")
	}
	if got := atlassian.Product("bitbucket").Label(); got != "bitbucket" {
		t.Errorf("label = %q, want the raw value", got)
	}
	if got := strings.Join(atlassian.ProductNames(), ","); got != "jira,confluence" {
		t.Errorf("product names = %q", got)
	}
}
