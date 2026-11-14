package scaleway

import (
	"cmp"
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlas/collect"
	"github.com/crewlet/crewlet/internal/httpx"
	"github.com/crewlet/crewlet/internal/integration"
)

// Scaleway's half of the atlas (ADR-0137): the Organization and each of its
// projects, as accounts, read whole under the provisioner key the company
// already gave the engine. One listing, with GET alone.
//
// NOTHING INSIDE A PROJECT IS READ UNDER THAT KEY: the provisioner's policy
// holds project read and IAM management and nothing of any product. What a
// project holds is read under the atlas application's own key, where the
// company switched it on (ADR-0144, atlasproducts.go).

func init() { collect.Register(atlasSource{}) }

// atlasSource is Scaleway's [collect.Source].
type atlasSource struct{}

func (atlasSource) Kind() integration.Kind { return integration.KindScaleway }

// partProjects is the one part of Scaleway a read can be refused.
const partProjects = "projects"

// accountKeyPrefix starts the key of every account Scaleway holds.
const accountKeyPrefix = string(atlas.VendorScaleway) + "/"

// atlasClient is the client a read makes and the counted transport under it,
// from the company's block, or [integration.ErrNotConfigured] where the company does not connect
// Scaleway or its key does not resolve.
func atlasClient(in collect.Input) (*Client, *http.Client, func() int, error) {
	if in.Config == nil {
		return nil, nil, nil, fmt.Errorf("scaleway: %w: no company document", integration.ErrNotConfigured)
	}
	cfg := in.Config.Integrations.Scaleway
	if cfg == nil || !cfg.Enabled || cfg.Provisioning == nil {
		return nil, nil, nil, fmt.Errorf("scaleway: %w: integrations.scaleway is off or has no provisioning block",
			integration.ErrNotConfigured)
	}
	var org, secret string
	if in.Env != nil {
		org = strings.TrimSpace(in.Env.Value(cfg.Provisioning.OrganizationID))
		secret = strings.TrimSpace(in.Env.Value(cfg.Provisioning.SecretKey))
	}
	if org == "" || secret == "" {
		return nil, nil, nil, fmt.Errorf("scaleway: %w: the Organization ID and the provisioner secret key did not "+
			"both resolve", integration.ErrNotConfigured)
	}
	base := in.HTTP
	if base == nil {
		base = httpx.Client(ClientTimeout)
	}
	counted, calls := collect.Counted(base)
	client, err := NewClient(ClientOptions{OrganizationID: org, SecretKey: secret, HTTP: counted})
	if err != nil {
		return nil, nil, nil, fmt.Errorf("scaleway: integrations.scaleway.provisioning.organization_id: %w", err)
	}
	return client, counted, calls, nil
}

// Collect reads the Organization's projects and reports them as claims in
// the integration's voice. It writes nothing at Scaleway.
func (atlasSource) Collect(ctx context.Context, in collect.Input) (collect.Read, error) {
	client, counted, calls, err := atlasClient(in)
	if err != nil {
		return collect.Read{}, err
	}
	b := collect.NewBuilder(integration.KindScaleway, in.Now)
	// THE KEY THE SETTINGS GIVE THE ACCOUNT, so the two name one entry.
	org, ok := b.Ref("organization", atlas.KindAccount, accountKeyPrefix+client.OrganizationID())
	if !ok {
		return collect.Read{}, fmt.Errorf("scaleway: the Organization %q is not an entry the atlas can name",
			client.OrganizationID())
	}
	projects, err := client.Projects(ctx)
	switch {
	case err == nil:
		readProjects(b, org, projects)
		// THE PRODUCTS ONLY BESIDE A WHOLE LISTING OF PROJECTS: a refused one
		// carries every claim below the Organization as it stands.
		if perr := readProducts(ctx, b, in, counted, org, projects); perr != nil {
			return collect.Read{}, perr
		}
	case Status(err) == http.StatusForbidden:
		// A KEY SCALEWAY KNOWS AND REFUSES THE LISTING lacks the permission
		// set, which is a finding: a key it does not know answers 401.
		b.Unread(collect.Unread{Part: partProjects,
			Detail: "The Organization's projects were not read: Scaleway refused the provisioner key. Give " +
				"its policy ProjectReadOnly on the Organization.",
			Holds: func(c atlas.Claim) bool { return c.Subject != org }})
	default:
		// ANYTHING ELSE IS A READ THAT DID NOT FINISH, and fails whole: a
		// listing cut short would retire what it did not reach.
		return collect.Read{}, fmt.Errorf("scaleway: list the Organization's projects: %w", reject(err))
	}
	b.Calls(calls())
	return b.Read(), nil
}

// readProjects claims each project as an account, by the name Scaleway
// holds for it. THAT IT SITS UNDER THE ORGANIZATION IS NOT SAID: the atlas has
// no relation from an account to an account, and its vendor places it.
func readProjects(b *collect.Builder, org atlas.Ref, projects []Project) {
	slices.SortFunc(projects, func(a, z Project) int { return cmp.Compare(a.ID, z.ID) })
	for _, p := range projects {
		// THE ID AS SCALEWAY WROTE IT, for the builder to judge: a shape a
		// credential keeps in its case is known only before it is lowered.
		where := "projects[" + p.ID + "]"
		account, ok := b.Ref(where+".id", atlas.KindAccount, accountKeyPrefix+p.ID)
		// THE DEFAULT PROJECT CARRIES THE ORGANIZATION'S OWN ID, so it is the
		// entry the settings already name and describe.
		if !ok || account == org {
			continue
		}
		b.Says(where+".id", account, atlas.FactVendor, string(atlas.VendorScaleway))
		id := strings.TrimPrefix(account.Key, accountKeyPrefix)
		b.Says(where+".id", account, atlas.FactAccountRef, id)
		name := collect.PlainLabel(p.Name)
		b.Says(where+".name", account, atlas.FactAccountName, name)
		if strings.TrimSpace(name) == "" {
			name = id
		}
		b.Says(where+".name", account, atlas.FactName, name)
	}
}
