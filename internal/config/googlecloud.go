package config

import (
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/words"
)

// GoogleCloud gives each agent seat its own Google Cloud service account, made
// in a project kept for Crewlet's accounts and granted the seat's level on the
// projects it names, through a provisioner service account the engine reaches
// by Workload Identity Federation with a key it holds itself.
//
// It ingests nothing: the block is provisioning alone, so [GoogleCloud.validate]
// requires it whenever the block is enabled. It names no project list: the
// projects an agent may be given are discovered from the grants the owner made
// to the provisioner on each one.
type GoogleCloud struct {
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Turn the integration on."`

	// Provisioning is the provisioner the agents' accounts are made by, the
	// federation provider it is reached through, and the key that signs for it.
	Provisioning *GoogleCloudProvisioning `yaml:"provisioning,omitempty" json:"provisioning,omitempty" desc:"The provisioner service account, the Workload Identity Federation provider and the engine's federation key the agents' service accounts are made with. Required when enabled."`
}

// GoogleCloudProvisioning is what the engine needs to make, grant, disable and
// delete one service account per agent seat.
type GoogleCloudProvisioning struct {
	// Provisioner is the provisioner service account's email. The project in
	// it is the identity project every agent's account is made in.
	Provisioner string `yaml:"provisioner,omitempty" json:"provisioner,omitempty" desc:"Email of the provisioner service account, <id>@<identity project>.iam.gserviceaccount.com. The agents' service accounts are created in its project."`

	// Provider is the federation provider's full resource name, which is also
	// the audience of every token the engine signs.
	Provider string `yaml:"provider,omitempty" json:"provider,omitempty" desc:"Full resource name of the Workload Identity Federation provider holding the engine's public key: projects/<number>/locations/global/workloadIdentityPools/<pool>/providers/<provider>. Until it is set, no agent's account can be made."`

	// AtlasRead lets the atlas read what each of the provisioner's projects
	// runs, as a service account of its own the identity block makes.
	AtlasRead bool `yaml:"atlas_read,omitempty" json:"atlas_read,omitempty" desc:"Let the atlas list what each project the provisioner reads runs (instances, databases, Cloud Run services, functions, buckets and topics) through Cloud Asset Inventory, as the service account crewlet-atlas in the identity project, which may only search resources by name and type. Off by default. Turning it on changes the Cloud Shell block: run it again."`

	// FederationKey is the engine's own ES256 private key (PKCS#8 PEM). Google
	// holds only its public half, and it signs nothing but a ten-minute token
	// for the one provider.
	FederationKey string `secret:"true" yaml:"federation_key,omitempty" json:"federation_key,omitempty" desc:"The engine's ES256 private key (PKCS#8 PEM) whose public half the federation provider holds. Minted by the connect form and never shown; it can act as the provisioner, which can create service accounts in the identity project and grant the agent levels."`
}

// The levels a seat's service account is granted on its projects, least access
// first. internal/googlecloud maps each to the roles it grants.
const (
	GoogleCloudLevelViewer     = "viewer"
	GoogleCloudLevelOperator   = "operator"
	GoogleCloudLevelAdminister = "administer"
)

// GoogleCloudLevels is every level a seat may name, least access first.
func GoogleCloudLevels() []string {
	return []string{GoogleCloudLevelViewer, GoogleCloudLevelOperator, GoogleCloudLevelAdminister}
}

// RoleGoogleCloud is a seat's own Google Cloud service account.
type RoleGoogleCloud struct {
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Whether this agent has its own Google Cloud service account. Off disables it and removes its roles; the account and its audit history stay."`

	// Level is what the account is granted on each project; empty is viewer.
	Level string `yaml:"level,omitempty" json:"level,omitempty" js:"enum=viewer|operator|administer" desc:"viewer (the default): read the project, Compute, GKE, Cloud Run, Cloud SQL, DNS, buckets, metrics and logs, and call Google's MCP tools. operator: viewer plus restarting and scaling VMs, pods and Cloud SQL, running Cloud Run jobs, editing alerting and reading and writing Cloud Storage objects. administer: operator plus managing VMs, networks, firewalls, clusters, Cloud SQL, DNS and buckets, which can leave access behind; granting it needs a fresh sign-in."`

	// Projects are the Google Cloud project IDs the level is granted on.
	Projects []string `yaml:"projects,omitempty" json:"projects,omitempty" desc:"Google Cloud project IDs this agent's level is granted on, each prepared for Crewlet. Required when enabled: an agent in no project can reach nothing."`
}

// LevelOrDefault is the seat's level, viewer where it names none.
func (r *RoleGoogleCloud) LevelOrDefault() string {
	if r == nil {
		return GoogleCloudLevelViewer
	}
	if level := strings.TrimSpace(r.Level); level != "" {
		return level
	}
	return GoogleCloudLevelViewer
}

// The shapes Google gives a project ID (6 to 30 lowercase letters, digits and
// hyphens, from a letter, or a legacy domain-scoped `example.com:id`), a
// service account's email, and a workload identity provider's resource name.
//
// Compiled at first use, not at init: Gitlet's push hook is this
// binary, and pays every package's init (cmd/crewlet's hookinit_test.go).
// Each bounded repeat compiles to a program holding every count.
var (
	googleProjectID = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^([a-z0-9][a-z0-9.-]*[a-z0-9]:)?[a-z][a-z0-9-]{4,28}[a-z0-9]$`)
	})
	googleServiceAccount = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(`^[a-z][a-z0-9-]{4,28}[a-z0-9]@([a-z][a-z0-9-]{4,28}[a-z0-9])\.iam\.gserviceaccount\.com$`)
	})
	googleProvider = sync.OnceValue(func() *regexp.Regexp {
		return regexp.MustCompile(
			`^projects/[0-9]{1,20}/locations/global/workloadIdentityPools/[a-z0-9-]{4,32}/providers/[a-z0-9-]{4,32}$`)
	})
)

// GoogleCloudProjectID reports whether id is a Google Cloud project ID.
func GoogleCloudProjectID(id string) bool { return googleProjectID().MatchString(id) }

// GoogleCloudServiceAccount is the project a service account's email names,
// and false for a value that is no service account's email.
func GoogleCloudServiceAccount(email string) (string, bool) {
	m := googleServiceAccount().FindStringSubmatch(strings.TrimSpace(email))
	if m == nil {
		return "", false
	}
	return m[1], true
}

// GoogleCloudProvider reports whether name is a workload identity provider's
// full resource name.
func GoogleCloudProvider(name string) bool {
	return googleProvider().MatchString(strings.TrimSpace(name))
}

func (g *GoogleCloud) validate(path Path) error {
	var p problems
	if !g.Enabled {
		return p.err()
	}
	if g.Provisioning == nil {
		p.add(at(path, "provisioning"), ErrMissing,
			"required when google_cloud is enabled: it names the provisioner service "+
				"account the agents' accounts are made by and the key the engine reaches it with")
		return p.err()
	}
	prov := g.Provisioning
	email := strings.TrimSpace(prov.Provisioner)
	switch {
	case email == "":
		p.add(at(path, "provisioning.provisioner"), ErrMissing,
			"required when google_cloud is enabled: the provisioner service account's email, "+
				"crewlet-provisioner@<identity project>.iam.gserviceaccount.com")
	case envref.Has(email):
		// Resolved and checked where the client is built.
	default:
		if _, ok := GoogleCloudServiceAccount(email); !ok {
			p.add(at(path, "provisioning.provisioner"), ErrUnknownValue,
				"%q is not a service account email, which reads <id>@<project>.iam.gserviceaccount.com",
				prov.Provisioner)
		}
	}
	// THE PROVIDER MAY FOLLOW THE KEY: the connect form mints the key first,
	// and the provider exists only once its public half is uploaded. Absent,
	// the pass reports the step that creates it.
	if provider := strings.TrimSpace(prov.Provider); provider != "" && !envref.Has(provider) && !GoogleCloudProvider(provider) {
		p.add(at(path, "provisioning.provider"), ErrUnknownValue,
			"%q is not a workload identity provider's full resource name, which reads "+
				"projects/<number>/locations/global/workloadIdentityPools/<pool>/providers/<provider>",
			prov.Provider)
	}
	if strings.TrimSpace(prov.FederationKey) == "" {
		p.add(at(path, "provisioning.federation_key"), ErrMissing,
			"required when google_cloud is enabled: the engine's federation key, which the "+
				"connect form mints and seals")
	}
	return p.err()
}

// checkGoogleCloud holds a seat's level to the closed set and its projects to
// project IDs, each once. That an enabled seat names one is the shared rule
// ([SeatScopeRequired]).
func (c *Company) checkGoogleCloud(p *problems, path Path, b *RoleGoogleCloud) {
	if b == nil {
		return
	}
	if level := strings.TrimSpace(b.Level); level != "" && !slices.Contains(GoogleCloudLevels(), level) {
		p.add(at(path, "level"), ErrUnknownValue,
			"%q is not a Google Cloud level this engine offers; give %s",
			b.Level, words.Or(GoogleCloudLevels()))
	}
	seen := map[string]bool{}
	for i, project := range b.Projects {
		id := strings.TrimSpace(project)
		switch {
		case id == "":
			p.add(idx(at(path, "projects"), i), ErrMissing,
				"a project ID is empty; remove the entry or give the project's ID")
		case !GoogleCloudProjectID(id):
			p.add(idx(at(path, "projects"), i), ErrUnknownValue,
				"%q is not a Google Cloud project ID: 6 to 30 lowercase letters, digits and "+
					"hyphens, starting with a letter, as the console shows it beside the name", project)
		case seen[id]:
			p.add(idx(at(path, "projects"), i), ErrConflict, "%q is listed twice", project)
		}
		seen[id] = true
	}
}
