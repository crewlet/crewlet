package gitlab

import (
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provision"
)

// The GitLab half of provisioning: which seats need an account, and which
// variable each one's token goes into.
//
// # The engine names no variable of its own
//
// A seat's code-host credential lives in its `mcp_env.gitlab` block, under
// whichever key that seat's tool stack reads — the glab CLI wants
// GITLAB_TOKEN, an HTTP MCP server wants a Private-Token header. So the scan
// looks for a `${VAR}` reference under any of those keys and mints into the
// variable the CONFIG already points at. Inventing `CREWLET_GITLAB_TOKEN_<seat>`
// would be a variable the seat's own tools never read.

// CredentialKeys are the spellings a seat's token arrives under, in the
// order they are tried.
//
// The engine has the same list, and it must: this is the one place a config
// says where a seat's code-host credential lives, and a provisioner writing
// to a key the engine does not read would mint tokens nothing authenticates
// with. Exported so the engine's own lookup and this scan cannot drift.
var CredentialKeys = []string{
	"GITLAB_TOKEN",
	"GITLAB_PERSONAL_ACCESS_TOKEN",
	"Private-Token",
	"Authorization",
}

// SeatEnv is the mcp_env server whose credentials belong to the code host.
const SeatEnv = "gitlab"

// PlanFor walks the org for seats that need a GitLab account.
//
// # A literal is a note, not a failure
//
// A seat whose token is written out rather than referenced is one an
// operator manages by hand. That is a supported choice — it just cannot be
// provisioned, because there is no variable to mint into and rewriting the
// company config from a provisioning run is not this command's job. So it is
// reported and skipped, and the run continues for the seats that can be.
func PlanFor(o *org.Organization, cfg *config.GitLab) (*provision.Plan, error) {
	if cfg == nil || !cfg.Enabled {
		return nil, fmt.Errorf("gitlab: the company config does not enable gitlab")
	}
	if cfg.Provisioning == nil {
		return nil, fmt.Errorf(
			"gitlab: integrations.gitlab.provisioning is unset, so there is " +
				"nothing to reconcile — name at least a group")
	}
	plan := &provision.Plan{}
	for seat := range o.AllRoles() {
		if !seat.IsAgent() {
			// A human seat is addressable through its contact block and
			// holds no tool credential; minting one would create an
			// account nothing ever authenticates as.
			continue
		}
		handle := seat.Handle()
		block := seat.MCPEnv[SeatEnv]
		if len(block) == 0 {
			continue
		}
		key, value := firstCredential(block)
		if key == "" {
			continue
		}
		name, ok := provision.SoleVar(stripScheme(value))
		if !ok {
			// THE NOTE NAMES THE SHAPE, NEVER THE VALUE. It is printed in
			// a report an operator pastes into a ticket, and the value
			// here is either a literal credential or a string containing
			// one.
			plan.Note("%s: mcp_env.gitlab.%s is %s rather than a whole ${VAR} "+
				"reference, so there is nowhere to mint a token — point it "+
				"at a variable, or manage this seat's credential by hand",
				handle, key, describeShape(stripScheme(value)))
			continue
		}
		plan.Add(provision.Seat{
			Handle:   handle,
			Role:     seat.Name,
			TokenVar: name,
			Email:    accountEmail(cfg.Provisioning, handle),
		})
	}
	return plan, nil
}

// firstCredential finds the key a seat's block carries its token under.
func firstCredential(block map[string]string) (string, string) {
	for _, key := range CredentialKeys {
		if value := strings.TrimSpace(block[key]); value != "" {
			return key, value
		}
	}
	return "", ""
}

// stripScheme drops an Authorization scheme so the reference underneath is
// visible.
//
// `Authorization: Bearer ${VAR}` is a whole reference wearing a prefix. The
// engine strips the same prefix when it reads the value, so a scan that did
// not would report a config the engine is perfectly happy with as
// unprovisionable.
func stripScheme(value string) string {
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(value), "Bearer "))
}

// accountEmail is the address a seat's service account is created with.
//
// DERIVED rather than configured, because GitLab requires one and it must be
// unique per account — an operator supplying them by hand would be
// maintaining a second copy of the roster whose only job is to stay in step
// with the first.
func accountEmail(p *config.GitLabProvisioning, handle string) string {
	prefix := strings.TrimSpace(p.UsernamePrefix)
	if prefix == "" {
		prefix = "crewlet"
	}
	return fmt.Sprintf("%s-%s@%s", prefix, handle, serviceAccountDomain)
}

// serviceAccountDomain is the domain service-account addresses are formed
// under.
//
// `noreply.` is a convention every code host understands as "this mailbox
// does not exist", which is exactly right here: the account is a robot, and
// an address that looked deliverable would eventually have somebody's
// notification sent to it.
const serviceAccountDomain = "noreply.crewlet.invalid"

// Username is the service-account name for a seat.
func Username(p *config.GitLabProvisioning, handle string) string {
	prefix := strings.TrimSpace(p.UsernamePrefix)
	if prefix == "" {
		prefix = "crewlet"
	}
	return prefix + "-" + handle
}

// describeShape says what is wrong with a value without repeating it.
//
// The two cases have different fixes — a literal needs a variable, a
// composite needs the variable to be the whole value — so collapsing them
// into "not a reference" would leave an operator to work out which.
func describeShape(value string) string {
	if len(provision.ReferencedVars(value)) == 0 {
		return "a literal"
	}
	return "a reference embedded in other text"
}
