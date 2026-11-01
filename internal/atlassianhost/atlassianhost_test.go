package atlassianhost_test

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/atlassianhost"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/confluence"
	"github.com/crewlet/crewlet/internal/jira"
)

// kind is what an address names, as the rule answers it.
type kind int

const (
	// other is a written host no rule names: Data Center unless the company
	// declared otherwise.
	other kind = iota
	site
	gateway
	// unwritten is an address whose host is a ${VAR}: what the company
	// declared, or unknown.
	unwritten
)

// addresses is the rule, case by case. Every host here that the three copies
// this package replaced answered differently is marked with the copy that got
// it wrong, because those are the cases a re-divergence would reopen.
var addresses = []struct {
	addr string
	want kind
}{
	{"https://acme.atlassian.net", site},
	{"https://acme.atlassian.net/", site},
	{"https://acme.atlassian.net/wiki", site},
	{"https://acme.atlassian.net/jira", site},
	{"https://ACME.Atlassian.NET/browse/ENG-1", site},
	{"https://acme.atlassian.net:443", site},
	{"https://acme.atlassian.net./wiki", site},
	{"https://acme.jira.com", site},
	// Government Cloud: every copy called it Data Center, so Confluence
	// went to the site root (Jira's side) and Jira to its v2 surface.
	{"https://acme.atlassian-us-gov-mod.net", site},
	{"https://acme.atlassian-us-gov-mod.net/wiki", site},

	{"https://api.atlassian.com/ex/jira/0f3a", gateway},
	{"https://api.atlassian.com/ex/confluence/0f3a", gateway},
	// Confluence matched the gateway's path case-sensitively, and only its
	// own product's: both were Data Center to it.
	{"https://API.ATLASSIAN.COM/ex/confluence/0f3a", gateway},
	{"https://api.atlassian.com/ex/jira/0f3a/rest/api/3", gateway},
	// The gateway's own /jira is the product it addresses, never the
	// console's suffix a site's address is trimmed of.
	{"https://api.atlassian.com/ex/jira", gateway},
	// No /ex/ path: config and Confluence read it as Data Center and asked
	// for a signing secret; it is the gateway's host all the same.
	{"https://api.atlassian.com", gateway},
	// Government Cloud's own gateway, which every copy — and the first
	// version of this one — called Data Center.
	{"https://api.atlassian-us-gov-mod.com/ex/jira/0f3a", gateway},
	{"https://api.atlassian-us-gov-mod.com/ex/confluence/0f3a", gateway},

	// atlassian.com hosts that are not the gateway: Jira read every one as
	// Cloud. jira.atlassian.com is a self-hosted Jira ("deploymentType":
	// "Server", no /rest/api/3), and no customer site lives under the domain.
	{"https://jira.atlassian.com", other},
	{"https://confluence.atlassian.com", other},
	{"https://acme.atlassian.com", other},
	// Government Cloud's services domain is its atlassian.com: only the
	// gateway on it addresses a product.
	{"https://acme.atlassian-us-gov-mod.com", other},
	// The gateway's path behind somebody else's host: config matched the
	// substring and called it Cloud.
	{"https://proxy.corp.example.com/api.atlassian.com/ex/jira/0f3a", other},
	// An Isolated Cloud site, at a base domain of its own: no list names
	// it, so the company's declaration does.
	{"https://acme.example-org.atlassian-isolated.net", other},

	{"https://atlassian.net", other},
	{"https://evilatlassian.net", other},
	{"https://acme.atlassian.net.corp.example.com", other},
	{"https://wiki.corp.example.com", other},
	{"https://jira.internal:8443/jira", other},
	{"http://localhost:8090", other},
	{"acme.atlassian.net", other},
	{"not a url at all", other},
	{"", other},

	// A reference where the host is: nothing here can know it.
	{"${JIRA_URL}", unwritten},
	{"${JIRA_URL}/wiki", unwritten},
	{"https://${SITE}.atlassian.net", unwritten},
	// A reference anywhere else leaves the host written: the branch that
	// first read every reference as unknown let a Data Center instance
	// with its context path referenced skip its signing secret.
	{"https://acme.atlassian.net/${CONTEXT}", site},
	{"https://jira.corp.example.com/${CONTEXT}", other},
	{"https://jira.corp.example.com:${PORT}/jira", other},
	{"https://${USER}@jira.corp.example.com", other},
}

// declarations is every value integrations.atlassian.deployment can carry,
// the absence of one included.
var declarations = []atlassianhost.Deployment{
	atlassianhost.Unknown, atlassianhost.Cloud, atlassianhost.DataCenter,
}

// want is the rule's answer for an address of kind k in a company that
// declared declared, stated independently of [atlassianhost.Of].
func want(k kind, declared atlassianhost.Deployment) atlassianhost.Deployment {
	switch {
	case k == site || k == gateway:
		return atlassianhost.Cloud
	case declared != atlassianhost.Unknown:
		return declared
	case k == other:
		return atlassianhost.DataCenter
	default:
		return atlassianhost.Unknown
	}
}

func TestTheRuleNamesSitesAndGateways(t *testing.T) {
	t.Parallel()
	for _, tc := range addresses {
		if tc.want == unwritten {
			continue
		}
		if got := atlassianhost.IsSite(tc.addr); got != (tc.want == site) {
			t.Errorf("IsSite(%q) = %v, want %v", tc.addr, got, tc.want == site)
		}
		if got := atlassianhost.IsGateway(tc.addr); got != (tc.want == gateway) {
			t.Errorf("IsGateway(%q) = %v, want %v", tc.addr, got, tc.want == gateway)
		}
	}
}

// A RECOGNISED HOST IS NEVER OVERRULED, AND EVERY OTHER IS THE COMPANY'S TO
// DECLARE. Atlassian's own hosts cannot be anybody's Data Center; an
// unrecognised one is Data Center for a company that said nothing, because
// that is what a self-hosted instance looks like, and Cloud for one that
// declared it, because that is what an Isolated Cloud site looks like.
func TestTheDeclarationDecidesOnlyWhatTheHostCannot(t *testing.T) {
	t.Parallel()
	for _, tc := range addresses {
		for _, declared := range declarations {
			if got, w := atlassianhost.Of(tc.addr, declared), want(tc.want, declared); got != w {
				t.Errorf("Of(%q, %q) = %q, want %q", tc.addr, declared, got, w)
			}
		}
	}
}

// EVERY CONSUMER ANSWERS ONE COMPANY ONE WAY. The validator, the Jira client
// and the Confluence client each decide Cloud or Data Center from the same
// url and the same declaration, and the decision has a consequence in each —
// a demanded signing secret, a REST version, a /wiki prefix — so an address
// they disagree on is a company admitted as one deployment and served as the
// other.
func TestEveryConsumerAgreesWithTheRule(t *testing.T) {
	t.Parallel()
	for _, tc := range addresses {
		for _, declared := range declarations {
			deploy := want(tc.want, declared)
			cloud := deploy == atlassianhost.Cloud
			// A client is handed resolved addresses, and answers one of
			// the two for any it is handed.
			if got := jira.DeploymentOf(tc.addr, declared) == jira.Cloud; got != cloud {
				t.Errorf("jira.DeploymentOf(%q, %q) Cloud = %v, want %v",
					tc.addr, declared, got, cloud)
			}
			if got := confluence.DeploymentOf(tc.addr, declared) == confluence.Cloud; got != cloud {
				t.Errorf("confluence.DeploymentOf(%q, %q) Cloud = %v, want %v",
					tc.addr, declared, got, cloud)
			}
			// Validation reads only an address with a scheme, or one
			// held as a reference, as a deployment: it refuses anything
			// else for the scheme, which is a different problem.
			if !strings.HasPrefix(tc.addr, "http") && !strings.HasPrefix(tc.addr, "${") {
				continue
			}
			for _, block := range []string{"jira", "confluence"} {
				err := validateURL(t, block, tc.addr, declared)
				asked := err != nil && strings.Contains(err.Error(), "webhook_secret")
				if wantAsked := deploy == atlassianhost.DataCenter; asked != wantAsked {
					t.Errorf("%s.url %q declared %q: webhook_secret demanded = %v, want %v (%v)",
						block, tc.addr, declared, asked, wantAsked, err)
				}
				// The one combination no answer serves: a host only
				// Atlassian's Cloud answers on, in a company that says
				// it runs Data Center.
				conflict := err != nil && strings.Contains(err.Error(), "is an Atlassian Cloud address")
				wantConflict := declared == atlassianhost.DataCenter &&
					(tc.want == site || tc.want == gateway)
				if conflict != wantConflict {
					t.Errorf("%s.url %q declared %q: conflict reported = %v, want %v (%v)",
						block, tc.addr, declared, conflict, wantConflict, err)
				}
			}
		}
	}
}

// A Cloud site serves Confluence under /wiki and the gateway already names
// the product, so the prefix follows IsSite exactly; a site's /jira suffix,
// which Atlassian's console prints after the site address, is trimmed for
// Jira and only there. An address no rule names is used as written, which is
// why an Isolated Cloud company writes its Confluence url out whole.
func TestTheRESTBasesFollowTheSiteRule(t *testing.T) {
	t.Parallel()
	for _, tc := range addresses {
		if !strings.HasPrefix(tc.addr, "http") || strings.Contains(tc.addr, "${") {
			continue
		}
		base := strings.TrimRight(tc.addr, "/")
		wantWiki, wantJira := base, base
		if tc.want == site {
			if !strings.HasSuffix(base, "/wiki") {
				wantWiki = base + "/wiki"
			}
			wantJira = strings.TrimSuffix(base, "/jira")
		}
		if got := confluence.RESTBase(tc.addr); got != wantWiki {
			t.Errorf("confluence.RESTBase(%q) = %q, want %q", tc.addr, got, wantWiki)
		}
		if got := jira.RESTBase(tc.addr); got != wantJira {
			t.Errorf("jira.RESTBase(%q) = %q, want %q", tc.addr, got, wantJira)
		}
	}
}

// A CLOUD ID IS READ THROUGH THE GATEWAY OF ITS SITE'S CLOUD. The id names
// no environment, and the commercial gateway does not serve a Government
// Cloud site, so the site beside the id places it; with none, or a custom
// domain, it is the commercial gateway. Whatever is built is a gateway the
// rule recognises, so a cloud id is never read as Data Center.
func TestACloudIDIsReadThroughItsSitesGateway(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		site, wantHost string
	}{
		{"", "api.atlassian.com"},
		{"https://acme.atlassian.net", "api.atlassian.com"},
		{"https://acme.jira.com", "api.atlassian.com"},
		{"https://wiki.acme.example", "api.atlassian.com"},
		{"https://acme.atlassian-us-gov-mod.net", "api.atlassian-us-gov-mod.com"},
		{"https://ACME.atlassian-us-gov-mod.net./wiki", "api.atlassian-us-gov-mod.com"},
	} {
		if got := atlassianhost.Gateway(tc.site); got != tc.wantHost {
			t.Errorf("Gateway(%q) = %q, want %q", tc.site, got, tc.wantHost)
		}
		j := config.Jira{CloudID: "0f3a", SiteURL: tc.site}
		c := config.Confluence{CloudID: "0f3a", SiteURL: tc.site}
		for product, base := range map[string]string{
			"jira": j.BaseURL(), "confluence": c.BaseURL(),
		} {
			if w := "https://" + tc.wantHost + "/ex/" + product + "/0f3a"; base != w {
				t.Errorf("%s with site %q reads through %q, want %q", product, tc.site, base, w)
			}
			if !atlassianhost.IsGateway(base) ||
				atlassianhost.Of(base, atlassianhost.Unknown) != atlassianhost.Cloud {
				t.Errorf("%s: the gateway built for a cloud id, %q, is not one the rule "+
					"reads as Cloud", product, base)
			}
		}
	}
	// AND THE SITE IS RESOLVED WITH THE ID: a referenced site would
	// otherwise place every Government Cloud id on the commercial gateway.
	env := map[string]string{"SITE": "https://acme.atlassian-us-gov-mod.net", "ID": "0f3a"}
	value := func(v string) string {
		if name, ok := strings.CutPrefix(v, "${"); ok {
			return env[strings.TrimSuffix(name, "}")]
		}
		return v
	}
	j := config.Jira{CloudID: "${ID}", SiteURL: "${SITE}"}
	if got, w := j.ResolvedBaseURL(value), "https://api.atlassian-us-gov-mod.com/ex/jira/0f3a"; got != w {
		t.Errorf("jira resolved a referenced Government site to %q, want %q", got, w)
	}
	c := config.Confluence{CloudID: "${ID}", SiteURL: "${SITE}"}
	if got, w := c.ResolvedBaseURL(value), "https://api.atlassian-us-gov-mod.com/ex/confluence/0f3a"; got != w {
		t.Errorf("confluence resolved a referenced Government site to %q, want %q", got, w)
	}
}

// A cloud id beside a declaration of Data Center is the conflict a url on a
// Cloud host is: it is read through a gateway, which no Data Center has.
func TestACloudIDInADataCenterCompanyIsRefused(t *testing.T) {
	t.Parallel()
	for _, block := range []string{"jira", "confluence"} {
		err := validateDoc(t, block, `    cloud_id: "0f3a"`, atlassianhost.DataCenter)
		if err == nil || !strings.Contains(err.Error(), "cloud id is read through Atlassian Cloud's gateway") {
			t.Errorf("%s: a cloud id in a company declaring data_center was not refused: %v",
				block, err)
		}
		if err := validateDoc(t, block, `    cloud_id: "0f3a"`, atlassianhost.Cloud); err != nil {
			t.Errorf("%s: a cloud id in a company declaring cloud was refused: %v", block, err)
		}
	}
}

// validateURL validates a company whose block names only url and a token.
func validateURL(t *testing.T, block, url string, declared atlassianhost.Deployment) error {
	t.Helper()
	return validateDoc(t, block, `    url: "`+url+`"`, declared)
}

// validateDoc validates a company whose block carries fields and a token,
// with integrations.atlassian.deployment set to declared when it is one.
func validateDoc(t *testing.T, block, fields string, declared atlassianhost.Deployment) error {
	t.Helper()
	atlassian := ""
	if declared != atlassianhost.Unknown {
		atlassian = "  atlassian:\n    deployment: " + string(declared) + "\n"
	}
	c, err := config.ParseCompanyDocument([]byte(`
name: Acme
providers:
  sandbox: {fake: true}
  llm:
    fast:
      model: claude-golden
integrations:
` + atlassian + `  ` + block + `:
` + fields + `
    token: t
roles:
  - name: SWE
    llm: fast
`))
	if err != nil {
		t.Fatalf("%s %q: %v", block, fields, err)
	}
	return c.Validate()
}
