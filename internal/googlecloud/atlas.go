package googlecloud

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/integration"
)

// Google Cloud's half of the atlas (ADR-0137): every active project the
// provisioner can read, as an account, and what each project's enabled APIs
// say it is used for, read whole as the provisioner. Every call is a GET.
//
// A PROJECT'S SERVICES ARE REFUSED ON THEIR OWN: listing them takes Service
// Usage Viewer on that project, which the provisioner holds only where the
// Prepare a project block ran. A project Google refuses is a finding on the
// source's standing, and what an earlier read recorded of it stands.

func init() { collect.Register(atlasSource{}) }

// atlasSource is Google Cloud's [collect.Source].
type atlasSource struct{}

func (atlasSource) Kind() integration.Kind { return integration.KindGoogleCloud }

// The parts of Google Cloud a read can be refused.
const (
	partProjects = "projects"
	partServices = "services"
)

// atlasProjectCeiling is the most projects one read walks: each costs a
// request for its services, and a company past this is not read at all,
// since half a listing would retire the other half.
const atlasProjectCeiling = 1000

// accountKeyPrefix starts the key of every account Google Cloud holds.
const accountKeyPrefix = string(atlas.VendorGoogleCloud) + "/"

// Collect reads the projects and each one's enabled services, and reports
// them as claims in the integration's voice. It writes nothing at Google.
func (atlasSource) Collect(ctx context.Context, in collect.Input) (collect.Read, error) {
	base := in.HTTP
	if base == nil {
		base = httpx.Client(ClientTimeout)
	}
	counted, calls := collect.Counted(base)
	client, err := ClientFor(in.Config, in.Env, counted)
	if err != nil {
		return collect.Read{}, err
	}
	r := &atlasRead{client: client, b: collect.NewBuilder(integration.KindGoogleCloud, in.Now),
		unread: map[string]bool{}}
	if err = r.read(ctx); err != nil {
		return collect.Read{}, err
	}
	r.b.Calls(calls())
	return r.b.Read(), nil
}

// atlasRead is one read in flight.
type atlasRead struct {
	client *Client
	b      *collect.Builder
	// unread is the accounts whose services Google refused, by key.
	unread map[string]bool
}

// refused reports Google refusing the provisioner a project's listing, as
// opposed to failing to answer it: a role it lacks there, or the 404 Google
// answers for what a caller cannot see. Never a 401 and never a rate limit.
func refused(err error) bool {
	status := Status(err)
	return status == http.StatusForbidden || status == http.StatusNotFound
}

// read is the read itself: every active project, then each one's services.
func (r *atlasRead) read(ctx context.Context) error {
	b := r.b
	// THE LISTING FIRST, AND IT IS NOT OPTIONAL: it proves the federation and
	// the provisioner, so a later refusal is a role's and never a dead key's.
	found, err := r.client.SearchProjects(ctx)
	if err != nil {
		return fmt.Errorf("googlecloud: list the projects the provisioner can read: %w", reject(err))
	}
	// A PROJECT ON ITS WAY OUT IS NOT LISTED, so it leaves as any that is gone.
	projects := slices.DeleteFunc(found, func(p Project) bool { return p.State != "ACTIVE" })
	if len(projects) > atlasProjectCeiling {
		b.Unread(collect.Unread{Part: partProjects, Holds: func(atlas.Claim) bool { return true },
			Detail: "The provisioner can read more projects than one read walks (" +
				strconv.Itoa(atlasProjectCeiling) + "), so none is read."})
		return nil
	}
	slices.SortFunc(projects, func(a, z Project) int { return cmp.Compare(a.ProjectID, z.ProjectID) })
	for _, p := range projects {
		if err = r.project(ctx, p); err != nil {
			return err
		}
	}
	if len(r.unread) > 0 {
		unread := r.unread
		b.Unread(collect.Unread{Part: partServices,
			Detail: "The enabled services of " + strconv.Itoa(len(unread)) + " projects were not read: Google " +
				"refused the provisioner. Run the Prepare a project block on each, which grants it Service " +
				"Usage Viewer there.",
			Holds: func(c atlas.Claim) bool {
				return c.Facet.Kind == atlas.FacetFact && c.Facet.Fact == atlas.FactCapability &&
					unread[c.Subject.Key]
			}})
	}
	return nil
}

// project claims one project as an account and reads what its enabled
// services say it does. WHAT A PROJECT SITS UNDER IS NOT SAID: the atlas has
// no relation from an account to an account, and a folder is neither.
func (r *atlasRead) project(ctx context.Context, p Project) error {
	b := r.b
	where := "projects[" + p.ProjectID + "]"
	// THE KEY THE SETTINGS GIVE THE IDENTITY PROJECT, so the two name one entry.
	account, ok := b.Ref(where+".projectId", atlas.KindAccount, accountKeyPrefix+p.ProjectID)
	if !ok {
		return nil
	}
	b.Says(where+".projectId", account, atlas.FactVendor, string(atlas.VendorGoogleCloud))
	b.Says(where+".projectId", account, atlas.FactAccountRef, p.ProjectID)
	name := collect.PlainLabel(p.DisplayName)
	b.Says(where+".displayName", account, atlas.FactAccountName, name)
	// THE SETTINGS NAME THE IDENTITY PROJECT, and two names would be a
	// disagreement for a person to settle; every other project is named here.
	if p.ProjectID != r.client.IdentityProject() {
		if strings.TrimSpace(name) == "" {
			name = p.ProjectID
		}
		b.Says(where+".displayName", account, atlas.FactName, name)
	}
	enabled, err := r.client.EnabledServices(ctx, p.ProjectID)
	switch {
	case err == nil:
	case refused(err):
		r.unread[account.Key] = true
		return nil
	default:
		// ANYTHING ELSE IS A READ THAT DID NOT FINISH, Google's rate limit
		// included, and fails whole: a listing cut short would withdraw
		// what it did not reach.
		return fmt.Errorf("googlecloud: read the enabled services of a project: %w", err)
	}
	for _, service := range slices.Sorted(mapKeys(enabled)) {
		// A CLOSED WORD, NEVER THE SERVICE'S NAME: an API this build does
		// not know says nothing.
		b.Says(where+".services", account, atlas.FactCapability, string(capabilityOf(service)))
	}
	return nil
}

// mapKeys is the keys of m, in no order.
func mapKeys(m map[string]bool) func(func(string) bool) {
	return func(yield func(string) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}

// capabilityOf is what an enabled API says its project is used for, "" for
// one that says nothing. ONLY AN API SOMEBODY SWITCHED ON: a new project has
// Cloud Storage, BigQuery, Datastore, logging and monitoring enabled before
// anybody uses it, so those name no capability. A switch, so the table costs
// nothing at init.
func capabilityOf(service string) atlas.Capability {
	switch strings.TrimSuffix(service, ".googleapis.com") {
	case "compute", "run", "container", "cloudfunctions", "appengine", "batch":
		return atlas.CapabilityCompute
	case "sqladmin", "spanner", "firestore", "redis", "memcache", "alloydb", "bigtableadmin":
		return atlas.CapabilityDatabase
	case "dns":
		return atlas.CapabilityDNS
	case "aiplatform", "generativelanguage":
		return atlas.CapabilityModelAPI
	case "cloudbuild", "clouddeploy":
		return atlas.CapabilityCI
	case "sourcerepo", "securesourcemanager":
		return atlas.CapabilityCodeHosting
	case "identitytoolkit", "iap":
		return atlas.CapabilityIdentity
	}
	return ""
}
