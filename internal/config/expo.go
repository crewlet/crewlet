package config

import (
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/words"
)

// Expo gives each agent seat its own Expo robot user and access token on the
// company's account, kept at the seat's level through the provisioner token.
// It ingests nothing, so [Expo.validate] requires provisioning when enabled.
type Expo struct {
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Turn the integration on."`

	// Provisioning is the account the agents' robots are created on and the
	// token they are created with.
	Provisioning *ExpoProvisioning `yaml:"provisioning,omitempty" json:"provisioning,omitempty" desc:"The Expo account and the provisioner token the engine creates each agent's robot user with. Required when enabled."`
}

// ExpoProvisioning is what the engine needs to create, re-level and delete one
// Expo robot user per agent seat, and to mint and revoke its token.
type ExpoProvisioning struct {
	// AccountName is the Expo account or organization the robots act for.
	AccountName string `yaml:"account_name,omitempty" json:"account_name,omitempty" desc:"Expo account or organization name, as it appears in expo.dev/accounts/<name>, that the agents' robot users are created on."`

	// AccessToken is a robot's or a person's access token holding the Admin
	// role on the account, which is what manages robots and their tokens.
	AccessToken string `secret:"true" yaml:"access_token,omitempty" json:"access_token,omitempty" desc:"Expo access token whose user or robot holds the Admin role on the account, so it can manage robot users and their tokens."`
}

// The levels a seat's Expo robot is created at, least access first.
// internal/expo maps each to the Expo permissions it grants.
const (
	ExpoLevelViewer         = "viewer"
	ExpoLevelRunner         = "runner"
	ExpoLevelDeveloper      = "developer"
	ExpoLevelReleaseManager = "release_manager"
)

// ExpoLevels is every level a seat may name, least access first.
func ExpoLevels() []string {
	return []string{ExpoLevelViewer, ExpoLevelRunner, ExpoLevelDeveloper, ExpoLevelReleaseManager}
}

// RoleExpo is a seat's own Expo robot user.
type RoleExpo struct {
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Whether this agent has its own Expo robot user. Off deletes the robot and its token."`

	// Level is what the robot may do on the account; empty is viewer.
	Level string `yaml:"level,omitempty" json:"level,omitempty" js:"enum=viewer|runner|developer|release_manager" desc:"viewer (the default): reads projects, builds, submissions and updates. runner: viewer plus low-risk actions such as starting builds. developer: runner plus publishing updates. release_manager: developer plus publishing to protected channels."`
}

// LevelOrDefault is the seat's level, viewer where it names none.
func (r *RoleExpo) LevelOrDefault() string {
	if r == nil {
		return ExpoLevelViewer
	}
	if level := strings.TrimSpace(r.Level); level != "" {
		return level
	}
	return ExpoLevelViewer
}

func (e *Expo) validate(path Path) error {
	var p problems
	if !e.Enabled {
		return p.err()
	}
	if e.Provisioning == nil {
		p.add(at(path, "provisioning"), ErrMissing,
			"required when expo is enabled: it names the account the agents' robot "+
				"users are created on and the token they are created with")
		return p.err()
	}
	prov := e.Provisioning
	name := strings.TrimSpace(prov.AccountName)
	switch {
	case name == "":
		p.add(at(path, "provisioning.account_name"), ErrMissing,
			"required when expo is enabled: the account or organization name shown "+
				"in its expo.dev/accounts/<name> address")
	case envref.Has(name):
		// Resolved where the client is built.
	case strings.HasPrefix(name, "@") || strings.ContainsAny(name, "/ \t\n"):
		p.add(at(path, "provisioning.account_name"), ErrUnknownValue,
			"%q is not an Expo account name: give the name alone, as it appears in "+
				"expo.dev/accounts/<name>, with no @, slash or space", prov.AccountName)
	}
	if strings.TrimSpace(prov.AccessToken) == "" {
		p.add(at(path, "provisioning.access_token"), ErrMissing,
			"required when expo is enabled: the access token, holding the Admin "+
				"role on the account, the engine manages the agents' robot users with")
	}
	return p.err()
}

// checkExpo holds a seat's level to the closed set.
func (c *Company) checkExpo(p *problems, path Path, b *RoleExpo) {
	if b == nil {
		return
	}
	if level := strings.TrimSpace(b.Level); level != "" && !slices.Contains(ExpoLevels(), level) {
		p.add(at(path, "level"), ErrUnknownValue,
			"%q is not an Expo level this engine offers; give %s",
			b.Level, words.Or(ExpoLevels()))
	}
}
