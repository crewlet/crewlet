package config

import (
	"regexp"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/words"
)

// Scaleway gives each agent seat its own Scaleway IAM application, with one
// policy granting the seat's level on its projects and its own API key, made
// and kept by the engine through the company's provisioner key.
//
// It ingests nothing: the block is provisioning alone, so [Scaleway.validate]
// requires it whenever the block is enabled. Atlas is the one thing beside it:
// a read-only application of the engine's own, off unless a company asks.
type Scaleway struct {
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Turn the integration on."`

	// Provisioning is the organization the agents' applications are made in
	// and the provisioner key they are made with.
	Provisioning *ScalewayProvisioning `yaml:"provisioning,omitempty" json:"provisioning,omitempty" desc:"The Scaleway Organization and the provisioner key the engine makes each agent's IAM application, policy and API key with. Required when enabled."`

	// Atlas has the engine make one more IAM application, named for the
	// atlas, holding read-only permission sets on every project and nothing
	// else, and read the company's products under it (ADR-0144).
	Atlas bool `yaml:"atlas,omitempty" json:"atlas,omitempty" desc:"See more in Atlas: the engine makes a read-only IAM application named for Atlas, with its own policy and API key, and reads the Organization's servers, clusters, databases, load balancers, bucket names, DNS zones, containers and functions under it. Off deletes the application, its policy and its key."`
}

// ScalewayProvisioning is what the engine needs to make, narrow, rotate and
// delete one IAM application, policy and API key per agent seat.
type ScalewayProvisioning struct {
	// OrganizationID is the organization every agent application is made in.
	OrganizationID string `yaml:"organization_id,omitempty" json:"organization_id,omitempty" desc:"Scaleway Organization ID (a UUID) the agents' IAM applications are created in."`

	// AccessKey is the provisioner API key's access key, an identifier rather
	// than a secret.
	AccessKey string `yaml:"access_key,omitempty" json:"access_key,omitempty" desc:"Access key of the provisioner application's API key: SCW followed by 17 capital letters or digits."`

	// SecretKey is that key's secret. Its application holds IAMApplicationManager,
	// IAMPolicyManager and ProjectReadOnly, which can mint a key for, and grant
	// anything to, any application in the organization.
	SecretKey string `secret:"true" yaml:"secret_key,omitempty" json:"secret_key,omitempty" desc:"Secret key of the provisioner API key, whose application holds IAMApplicationManager, IAMPolicyManager and ProjectReadOnly on the Organization. It can mint keys for, and grant anything to, any application in the Organization: give it a short expiry and rotate it."`
}

// The levels a seat's Scaleway policy grants, least access first.
// internal/scaleway maps each to the permission sets it grants.
const (
	ScalewayLevelRead       = "read"
	ScalewayLevelOperate    = "operate"
	ScalewayLevelAdminister = "administer"
)

// ScalewayLevels is every level a seat may name, least access first.
func ScalewayLevels() []string {
	return []string{ScalewayLevelRead, ScalewayLevelOperate, ScalewayLevelAdminister}
}

// RoleScaleway is a seat's own Scaleway IAM application.
type RoleScaleway struct {
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Whether this agent has its own Scaleway IAM application, policy and API key. Off deletes all three."`

	// Level is what the seat's policy grants; empty is read.
	Level string `yaml:"level,omitempty" json:"level,omitempty" js:"enum=read|operate|administer" desc:"read (the default): servers, storage, networks, DNS and databases in its projects, plus the audit trail. operate: read plus restarting servers and changing firewalls, IPs, load balancers and private networks. administer: operate plus creating and changing servers, disks, databases, Kubernetes and the registry, which can leave access behind; granting it needs a fresh sign-in."`

	// Projects are the Scaleway project IDs the policy grants the level on.
	Projects []string `yaml:"projects,omitempty" json:"projects,omitempty" desc:"Scaleway project IDs this agent's policy grants its level on. Required when enabled: an agent in no project can reach nothing."`
}

// LevelOrDefault is the seat's level, read where it names none.
func (r *RoleScaleway) LevelOrDefault() string {
	if r == nil {
		return ScalewayLevelRead
	}
	if level := strings.TrimSpace(r.Level); level != "" {
		return level
	}
	return ScalewayLevelRead
}

// scalewayUUID is the shape Scaleway gives an organization and a project ID.
// Compiled at first use, not at init: Gitlet's push hook is this
// binary, and pays every package's init (cmd/crewlet's hookinit_test.go);
// its bounded repeats compile to a program holding every count, about 18 KB.
var scalewayUUID = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
})

// scalewayAccessKey is the shape of an API key's access key, compiled at
// first use for the same reason.
var scalewayAccessKey = sync.OnceValue(func() *regexp.Regexp {
	return regexp.MustCompile(`^SCW[A-Z0-9]{17}$`)
})

// ScalewayID is an organization or project ID as Scaleway matches it: trimmed
// and lowercased.
func ScalewayID(id string) string { return strings.ToLower(strings.TrimSpace(id)) }

func (s *Scaleway) validate(path Path) error {
	var p problems
	if !s.Enabled {
		return p.err()
	}
	if s.Provisioning == nil {
		p.add(at(path, "provisioning"), ErrMissing,
			"required when scaleway is enabled: it names the Organization the agents' "+
				"applications are created in and the key they are created with")
		return p.err()
	}
	prov := s.Provisioning
	org := strings.TrimSpace(prov.OrganizationID)
	switch {
	case org == "":
		p.add(at(path, "provisioning.organization_id"), ErrMissing,
			"required when scaleway is enabled: the Organization ID shown in the Scaleway "+
				"console's Organization settings")
	case envref.Has(org):
		// Resolved and checked where the client is built.
	case !scalewayUUID().MatchString(ScalewayID(org)):
		p.add(at(path, "provisioning.organization_id"), ErrUnknownValue,
			"%q is not a Scaleway Organization ID, which is a UUID", prov.OrganizationID)
	}
	access := strings.TrimSpace(prov.AccessKey)
	switch {
	case access == "":
		p.add(at(path, "provisioning.access_key"), ErrMissing,
			"required when scaleway is enabled: the access key of the provisioner "+
				"application's API key")
	case envref.Has(access):
		// Resolved and checked where the client is built.
	case !scalewayAccessKey().MatchString(access):
		p.add(at(path, "provisioning.access_key"), ErrUnknownValue,
			"%q is not a Scaleway access key, which is SCW followed by 17 capital letters "+
				"or digits", prov.AccessKey)
	}
	if strings.TrimSpace(prov.SecretKey) == "" {
		p.add(at(path, "provisioning.secret_key"), ErrMissing,
			"required when scaleway is enabled: the secret key of the provisioner API key")
	}
	return p.err()
}

// checkScaleway holds a seat's level to the closed set and its projects to
// project IDs, each once. That an enabled seat names one is the shared rule
// ([SeatScopeRequired]).
func (c *Company) checkScaleway(p *problems, path Path, b *RoleScaleway) {
	if b == nil {
		return
	}
	if level := strings.TrimSpace(b.Level); level != "" && !slices.Contains(ScalewayLevels(), level) {
		p.add(at(path, "level"), ErrUnknownValue,
			"%q is not a Scaleway level this engine offers; give %s",
			b.Level, words.Or(ScalewayLevels()))
	}
	seen := map[string]bool{}
	for i, project := range b.Projects {
		id := ScalewayID(project)
		switch {
		case id == "":
			p.add(idx(at(path, "projects"), i), ErrMissing,
				"a project ID is empty; remove the entry or give the project's ID")
		case !scalewayUUID().MatchString(id):
			p.add(idx(at(path, "projects"), i), ErrUnknownValue,
				"%q is not a Scaleway project ID, which is a UUID", project)
		case seen[id]:
			p.add(idx(at(path, "projects"), i), ErrConflict, "%q is listed twice", project)
		}
		seen[id] = true
	}
}
