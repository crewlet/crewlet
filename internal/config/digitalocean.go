package config

import (
	"slices"
	"strings"
	"unicode"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/words"
)

// DigitalOcean gives each agent seat its own DigitalOcean personal access
// token in the company's team. DigitalOcean lets no API create one, so a
// person creates each token in the control panel and pastes it; the engine
// checks its scopes, seals it and keeps checking it with the token itself.
type DigitalOcean struct {
	Enabled bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Turn the integration on."`

	// Provisioning names the team the agents' tokens act in.
	Provisioning *DigitalOceanProvisioning `yaml:"provisioning,omitempty" json:"provisioning,omitempty" desc:"The DigitalOcean team the agents' tokens act in. Required when enabled."`

	// Atlas is the token the atlas reads the team with; nil reads nothing.
	Atlas *DigitalOceanAtlas `yaml:"atlas,omitempty" json:"atlas,omitempty" desc:"Optional. The read-only token the atlas reads the team's resources with. Without it the atlas shows the DigitalOcean account alone."`
}

// DigitalOceanAtlas is the atlas's own identity at DigitalOcean: a personal
// access token a person creates for it alone, with read scopes only, so its
// reads are no agent's and it is revoked apart from every agent's token.
type DigitalOceanAtlas struct {
	Token string `secret:"true" yaml:"token,omitempty" json:"token,omitempty" desc:"Reference to the sealed personal access token made for the atlas, with these Custom Scopes and no other: account:read, droplet:read, kubernetes:read, database:read, load_balancer:read, domain:read, app:read and project:read. A token that can change anything is refused."`
}

// DigitalOceanProvisioning is the team every seat's token must belong to. It
// carries no credential: each token is the seat's own.
type DigitalOceanProvisioning struct {
	// Team is the team's name as the control panel's team menu shows it.
	Team string `yaml:"team,omitempty" json:"team,omitempty" desc:"The DigitalOcean team the agents' tokens act in, by its name as the control panel's team menu shows it. The first agent's token proves it."`
}

// DigitalOceanTeamRef is a team's name as the atlas keys the team's account:
// lowercase, every run of other characters one hyphen. A team's name may hold
// a space and an account's key may not, so the settings and the read of the
// team both key it through here.
func DigitalOceanTeamRef(team string) string { return org.Slugify(team) }

// The levels a seat's DigitalOcean token holds, least access first. Each is a
// fixed set of DigitalOcean's custom scopes, which internal/digitalocean holds.
const (
	DigitalOceanLevelRead       = "read"
	DigitalOceanLevelOperate    = "operate"
	DigitalOceanLevelAdminister = "administer"
)

// DigitalOceanLevelDefault is the level a seat that names none holds: the
// least access.
const DigitalOceanLevelDefault = DigitalOceanLevelRead

// DigitalOceanLevels is every level a seat may name, least access first.
func DigitalOceanLevels() []string {
	return []string{DigitalOceanLevelRead, DigitalOceanLevelOperate, DigitalOceanLevelAdminister}
}

// RoleDigitalOcean is a seat's own DigitalOcean token.
type RoleDigitalOcean struct {
	Enabled *bool `yaml:"enabled,omitempty" json:"enabled,omitempty" desc:"Whether this agent has its own DigitalOcean personal access token, which a person creates in the control panel and pastes. Off revokes the token at DigitalOcean and clears its sealed copy."`

	// Level is the token's scope set; empty is read.
	Level string `yaml:"level,omitempty" json:"level,omitempty" js:"enum=read|operate|administer" desc:"The token's DigitalOcean scopes. read (the default): reads Droplets, volumes, networking, DNS, databases, Kubernetes, apps and monitoring. operate: read plus creating, changing and deleting them. administer: operate plus cluster credentials, database passwords and consoles, which can outlive the token."`
}

// LevelOrDefault is the seat's level, [DigitalOceanLevelDefault] where it
// names none.
func (r *RoleDigitalOcean) LevelOrDefault() string {
	if r == nil || strings.TrimSpace(r.Level) == "" {
		return DigitalOceanLevelDefault
	}
	return strings.TrimSpace(r.Level)
}

func (d *DigitalOcean) validate(path Path) error {
	var p problems
	if !d.Enabled {
		return p.err()
	}
	if d.Provisioning == nil {
		p.add(at(path, "provisioning"), ErrMissing,
			"required when digitalocean is enabled: it names the team the agents' "+
				"tokens act in")
		return p.err()
	}
	team := d.Provisioning.Team
	switch trimmed := strings.TrimSpace(team); {
	case trimmed == "":
		p.add(at(path, "provisioning.team"), ErrMissing,
			"required when digitalocean is enabled: the team's name as the control "+
				"panel's team menu shows it")
	case envref.Has(trimmed):
		// Resolved where the session is opened.
	case trimmed != team || strings.ContainsFunc(team, unicode.IsControl):
		p.add(at(path, "provisioning.team"), ErrUnknownValue,
			"%q is not a team name as the control panel shows it: give it with no "+
				"leading or trailing space and on one line", team)
	}
	return p.err()
}

// checkDigitalOcean holds a seat's level to the closed set.
func (c *Company) checkDigitalOcean(p *problems, path Path, b *RoleDigitalOcean) {
	if b == nil {
		return
	}
	if level := strings.TrimSpace(b.Level); level != "" && !slices.Contains(DigitalOceanLevels(), level) {
		p.add(at(path, "level"), ErrUnknownValue,
			"%q is not a DigitalOcean level this engine offers; give %s",
			b.Level, words.Or(DigitalOceanLevels()))
	}
}
