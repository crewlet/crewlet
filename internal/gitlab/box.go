package gitlab

import (
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// BoxTokenEnv is the variable git's credential helper and glab read an
// agent's own GitLab token from in its executor box, beside the seat's glab
// tool server.
const BoxTokenEnv = "GITLAB_TOKEN"

// BoxDefaults is what an agent on at GitLab finds in its box where the engine
// provisions GitLab and so seals the agent's token: the token as
// [BoxTokenEnv]. Where the token is handed in from outside Crewlet, the seat's
// own settings name the variable that holds it.
func (Vendor) BoxDefaults(c *config.Company, role *config.Role) seataccess.BoxSet {
	handle := strings.TrimSpace(role.IdentityKey())
	if handle == "" || c.SeatCredentialMode(string(integration.KindGitLab), false) != config.SeatCredentialEngine {
		return seataccess.BoxSet{}
	}
	return seataccess.BoxSet{Credentials: map[string]string{
		BoxTokenEnv: "${" + seataccess.SinkName(integration.KindGitLab, FieldToken, handle) + "}",
	}}
}
