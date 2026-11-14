package config

import (
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/words"
)

// Expo gives each agent seat its own Expo robot user on the company's account.
// A person creates each robot at Expo and pastes its token; the engine checks
// it, seals it and keeps checking it with the seat's own token.
type Expo struct {
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Turn the integration on."`

	// Provisioning names the account the agents' robots act for.
	Provisioning *ExpoProvisioning `yaml:"provisioning,omitempty" json:"provisioning,omitempty" desc:"The Expo account the agents' robot users act for. Required when enabled."`

	// Atlas is the robot the atlas reads the account as; nil reads nothing.
	Atlas *ExpoAtlas `yaml:"atlas,omitempty" json:"atlas,omitempty" desc:"Optional. The view-only robot the atlas reads the account's projects and builds as. Without it the atlas shows the Expo account alone."`
}

// ExpoAtlas is the atlas's own identity at Expo: a robot user a person
// creates for it alone, with the Viewer role, so its reads are no agent's and
// it is removed apart from every agent's robot.
type ExpoAtlas struct {
	Token string `secret:"true" yaml:"token,omitempty" json:"token,omitempty" desc:"Reference to the sealed access token of a robot user made for the atlas, holding the Viewer role on the account. A robot with any other role is refused."`
}

// ExpoProvisioning is the account every seat's robot must hold its role on.
// It carries no credential: each robot's token is the seat's own.
type ExpoProvisioning struct {
	// AccountName is the Expo account or organization the robots act for.
	AccountName string `yaml:"account_name,omitempty" json:"account_name,omitempty" desc:"Expo account or organization name, as it appears in expo.dev/accounts/<name>, that the agents' robot users act for."`
}

// The levels a seat's Expo robot holds, least access first: each is one of
// Expo's own robot roles, which internal/expo compares by Expo's Role.
const (
	ExpoLevelViewer         = "viewer"
	ExpoLevelRunner         = "runner"
	ExpoLevelDeveloper      = "developer"
	ExpoLevelReleaseManager = "release_manager"
)

// ExpoLevelDefault is the level a seat that names none holds: the least access,
// until a live account shows whether Viewer can read EAS builds through eas-cli.
const ExpoLevelDefault = ExpoLevelViewer

// ExpoLevels is every level a seat may name, least access first.
func ExpoLevels() []string {
	return []string{ExpoLevelViewer, ExpoLevelRunner, ExpoLevelDeveloper, ExpoLevelReleaseManager}
}

// RoleExpo is a seat's own Expo robot user.
type RoleExpo struct {
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Whether this agent has its own Expo robot user, which a person creates at Expo and pastes the token of. Off clears its sealed token."`

	// Level is the robot's role on the account; empty is viewer.
	Level string `yaml:"level,omitempty" json:"level,omitempty" js:"enum=viewer|runner|developer|release_manager" desc:"The robot's Expo role. viewer (the default): views all projects. runner: views projects and performs low-risk actions on them. developer: creates builds, publishes updates to unprotected channels and manages credentials. release_manager: developer plus protected channels."`
}

// LevelOrDefault is the seat's level, [ExpoLevelDefault] where it names none.
func (r *RoleExpo) LevelOrDefault() string {
	if r == nil || strings.TrimSpace(r.Level) == "" {
		return ExpoLevelDefault
	}
	return strings.TrimSpace(r.Level)
}

func (e *Expo) validate(path Path) error {
	var p problems
	if !e.Enabled {
		return p.err()
	}
	if e.Provisioning == nil {
		p.add(at(path, "provisioning"), ErrMissing,
			"required when expo is enabled: it names the account the agents' robot "+
				"users act for")
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
