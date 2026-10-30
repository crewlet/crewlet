package datadog

import (
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
)

// SeatEnv is the mcp_env server whose credentials belong to Datadog.
//
// FROM THE ORG MODEL'S OWN TABLE: see [org.EngineReadMCPEnv].
const SeatEnv = org.MCPEnvDatadog

// CredentialKeys are the spellings a Datadog application key takes in a seat's
// `mcp_env.datadog` block.
//
// NOTHING READS A SEAT'S KEY FROM THERE ANY MORE: the engine seals each seat's
// key under the name it derives ([seataccess.SinkName]) and binds it to the
// seat's tool itself (D15). The list is what the document rules refuse in a
// seat's mcp_env for a managed integration (D4) and what the migration
// recognises when it moves a seat's key under the engine's name, so it names
// every spelling a company has written, DD_APPLICATION_KEY (the header
// Datadog's MCP server reads) among them.
//
//nolint:gochecknoglobals // an immutable list, not state
var CredentialKeys = []string{"DD_APP_KEY", "DATADOG_APP_KEY", "app_key", "DD_APPLICATION_KEY"}

// DefaultEmailDomain is what a service account's address is built under when
// the company names none.
//
// Datadog requires an address and never delivers to it, so this is a domain
// that cannot receive mail by construction rather than one somebody owns:
// `.invalid` is reserved by RFC 2606 for exactly this. A real domain would
// mean a bounce, or worse a delivery, for an account that is not a person.
const DefaultEmailDomain = "agents.crewlet.invalid"

// AccountEmail is the address one seat's service account is created with.
//
// DERIVED, so the pass that creates an account and the teardown that disables
// it agree about which account is whose without storing a mapping.
//
// FROM THE HANDLE, which is immutable (ADR-0013), so the address is stable for
// the life of the seat: Datadog will not change an account's address at all.
func AccountEmail(p *config.DatadogProvisioning, handle string) string {
	domain := emailDomain(p)
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
//
// The handle is the FALLBACK LABEL only, for a seat whose role is unnamed —
// the same string [AccountEmail] puts in the address, so the two columns of
// Datadog's user list agree rather than naming a seat two ways.
func AccountName(role, handle string) string {
	if strings.TrimSpace(role) == "" {
		return handle + " (Crewlet)"
	}
	return role + " (Crewlet)"
}

// emailDomain is the domain every agent account's address is built under:
// the company's, or [DefaultEmailDomain]. One function, so the address a
// pass creates an account under, the listing that finds it again and the
// orphan check that judges it cannot disagree about a stray space.
func emailDomain(p *config.DatadogProvisioning) string {
	if p != nil && strings.TrimSpace(p.EmailDomain) != "" {
		return strings.TrimSpace(p.EmailDomain)
	}
	return DefaultEmailDomain
}
