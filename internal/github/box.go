package github

import (
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// An agent's own app token in its executor box, for git and gh beside the
// seat's GitHub tool server.
//
// THE FILE IS WHAT A LONG RUN READS. An installation token lasts an hour and a
// process's environment cannot change under it, so the variable is good for
// [RunTokenLife] from launch and the file, which the completion poll rewrites
// whenever the token renews, is good for as long as the run lasts. The git-auth
// recipe reads the file first (docs/concepts/code-sandbox.md).
const (
	// BoxTokenEnv is the variable git's credential helper and gh read the
	// token from at launch.
	BoxTokenEnv = "GITHUB_TOKEN"
	// BoxTokenFile is the token file's path, relative to the box's home.
	BoxTokenFile = ".config/crewlet/github-token"
)

// BoxDefaults is what an agent on at GitHub finds in its box while the
// company runs GitHub, which is when the engine issues its app's token: the
// token as [BoxTokenEnv], and the file the poll keeps current.
func (vendor) BoxDefaults(c *config.Company, role *config.Role) seataccess.BoxSet {
	handle := strings.TrimSpace(role.IdentityKey())
	if c == nil || c.Integrations.GitHub == nil || !c.Integrations.GitHub.Enabled || handle == "" {
		return seataccess.BoxSet{}
	}
	token := "${" + SeatVar(handle, FieldAppToken) + "}"
	return seataccess.BoxSet{
		Credentials: map[string]string{BoxTokenEnv: token},
		Files:       []seataccess.BoxFile{{Path: BoxTokenFile, Ref: token, Rewritten: true}},
	}
}
