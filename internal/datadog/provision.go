package datadog

import (
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provision"
)

// SeatEnv is the mcp_env server whose credentials belong to Datadog.
const SeatEnv = "datadog"

// CredentialKeys are the spellings a seat's application key arrives under.
//
// A LIST rather than one name, for the reason GitLab's is: a company writes
// its own mcp_env, and refusing to find a credential because it was spelled
// the other obvious way is a silent no-op an operator cannot debug.
//
//nolint:gochecknoglobals // an immutable list, not state
var CredentialKeys = []string{"DD_APP_KEY", "DATADOG_APP_KEY", "app_key"}

// DefaultEmailDomain is what a service account's address is built under when
// the company names none.
//
// Datadog requires an address and never delivers to it, so this is a domain
// that cannot receive mail by construction rather than one somebody owns:
// `.invalid` is reserved by RFC 2606 for exactly this. A real domain would
// mean a bounce, or worse a delivery, for an account that is not a person.
const DefaultEmailDomain = "agents.crewlet.invalid"

// DefaultRole is the Datadog role an agent account is created holding.
//
// READ ONLY, deliberately, and it is the honest default for accounts nothing
// yet authenticates with: an agent that only needs to be woken by an alert
// needs no write at all. A company whose agents act in Datadog names a wider
// role explicitly, which is a decision somebody makes rather than one this
// engine makes for them.
const DefaultRole = "Datadog Read Only Role"

// PlanFor is the seats this company wants a Datadog identity for.
//
// A seat opts IN by naming a ${VAR} in its mcp_env datadog block, exactly as
// every other provisioned app works: the variable is where the minted key is
// written, and a seat with nowhere to put one is a seat this pass leaves
// alone rather than an error.
func PlanFor(o *org.Organization, cfg *config.Datadog) (*provision.Plan, error) {
	if cfg == nil {
		return nil, fmt.Errorf("datadog: the company config has no datadog block")
	}
	if cfg.Provisioning == nil {
		return nil, fmt.Errorf(
			"datadog: integrations.datadog.provisioning is unset, so there is " +
				"no organization to create identities in")
	}
	plan := &provision.Plan{}
	if o == nil {
		return plan, nil
	}
	for seat := range o.AllRoles() {
		if !seat.IsAgent() {
			// A human seat is a person with their own Datadog login;
			// minting one an account would create a second identity for
			// somebody who already has one.
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
		name, ok := provision.SoleVar(value)
		if !ok {
			// THE NOTE NAMES THE SHAPE, NEVER THE VALUE: it is printed in
			// a report an operator pastes into a ticket, and the value is
			// either a credential or a string containing one.
			plan.Note("%s: mcp_env.datadog.%s is not a whole ${VAR} reference, "+
				"so there is nowhere to write a minted key — point it at a "+
				"variable, or manage this seat's key by hand", handle, key)
			continue
		}
		plan.Add(provision.Seat{
			Handle:   handle,
			Role:     seat.Name,
			TokenVar: name,
			Email:    AccountEmail(cfg.Provisioning, handle),
		})
	}
	return plan, nil
}

// firstCredential finds the key a seat's block carries its app key under.
func firstCredential(block map[string]string) (string, string) {
	for _, key := range CredentialKeys {
		if value := strings.TrimSpace(block[key]); value != "" {
			return key, value
		}
	}
	return "", ""
}

// AccountEmail is the address one seat's service account is created with.
//
// DERIVED, so the pass that creates an account and the teardown that disables
// it agree about which account is whose without storing a mapping. The handle
// is the only part that varies, which is what makes the address a stable name
// for a seat rather than a value somebody has to look up.
func AccountEmail(p *config.DatadogProvisioning, handle string) string {
	domain := DefaultEmailDomain
	if p != nil && strings.TrimSpace(p.EmailDomain) != "" {
		domain = strings.TrimSpace(p.EmailDomain)
	}
	// `crewlet-`, the same prefix GitLab's service accounts carry, so a
	// person reading a user list at either app can see at a glance which
	// accounts this engine made. NOT `agent-`: that string is a queue
	// marker (topics.AgentGroupPrefix), and a literal of it here reads to
	// the hand-built-subject guard exactly like somebody assembling a
	// consumer group by hand.
	return fmt.Sprintf("crewlet-%s@%s", handle, domain)
}

// AccountName is the display name one seat's account carries, so a person
// reading Datadog's user list can tell which agent it is.
func AccountName(role, handle string) string {
	if strings.TrimSpace(role) == "" {
		return handle + " (Crewlet)"
	}
	return role + " (Crewlet)"
}

// RoleName is the Datadog role agent accounts are created holding.
func RoleName(p *config.DatadogProvisioning) string {
	if p != nil && strings.TrimSpace(p.Role) != "" {
		return strings.TrimSpace(p.Role)
	}
	return DefaultRole
}
