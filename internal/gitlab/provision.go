package gitlab

import (
	"fmt"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/provision"
)

// Naming shared by the provisioning and the engine's reads.
//
// A provisioned seat's token is the engine's: it is sealed under the name
// the engine derives for the seat (GITLAB_TOKEN_<HANDLE>) by per-seat access
// ([Vendor]), and no document names it. A company that does not provision
// GitLab (D4b) still gives each seat its own token, an external credential,
// under one of [CredentialKeys] in its `mcp_env.gitlab` block, and the engine
// reads it from there for the seat's tools and its identity.

// CredentialKeys are the spellings an external seat token arrives under in
// `mcp_env.gitlab`, in the order they are tried: the glab CLI reads
// GITLAB_TOKEN, an HTTP MCP server a Private-Token header. Exported so every
// reader tries the same list.
var CredentialKeys = []string{
	"GITLAB_TOKEN",
	"GITLAB_PERSONAL_ACCESS_TOKEN",
	"Private-Token",
	"Authorization",
}

// SeatEnv is the mcp_env server whose credentials belong to the code host.
//
// FROM THE ORG MODEL'S OWN TABLE: see [org.EngineReadMCPEnv].
const SeatEnv = org.MCPEnvGitLab

// PlanFor is the plan the engine's current call sites read a seat's
// mcp_env.gitlab variable from: the tool-credential declaration and the
// setup pass, which hands it to [Reconcile] and [Teardown], both of which
// ignore it now that seats are per-seat access. It goes when those call sites
// are retired; nothing in this package provisions from it.
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
				handle, key, provision.Shape(stripScheme(value)))
			continue
		}
		// NO EMAIL, and that omission is the whole of the fix below.
		entry := provision.Seat{
			Handle:   handle,
			Role:     seat.Name,
			TokenVar: name,
		}
		// THE OVERRIDE THAT NAMES THIS SEAT — see
		// [config.GitLabProvisioning.OverrideFor].
		if level, found := cfg.Provisioning.OverrideFor(seat); found {
			entry.AccessLevel = string(level)
		}
		plan.Add(entry)
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

// GITLAB NAMES THE ADDRESS, AND A SEAT PLAN MUST NOT.
//
// A service account was created with a derived `crewlet-<handle>@
// noreply.crewlet.invalid`, on the reasoning that GitLab requires an address,
// that it must be unique per account, and that `noreply.` says "this mailbox
// does not exist" — which is right for a robot. Two of those three are false
// and the third is what broke it.
//
// GitLab does NOT require one: `email` is optional on both service-account
// routes, and omitting it has GitLab generate an address under its own noreply
// domain. What its documentation adds is the sentence that matters — "custom
// email addresses require confirmation before the account is active, unless
// the group has a matching verified domain". `crewlet.invalid` is a reserved
// TLD (RFC 2606), so no confirmation mail can ever be delivered and no domain
// can ever be verified: every account this created was permanently inactive.
//
// GitLab then refuses every token such an account holds, with
// `403 Your primary email address is not confirmed`. Measured: a seat's token
// was minted, sealed, refused on the next pass's check, and minted again —
// 144 live tokens with `api` scope over one connect, and 164 in total. That
// loop is fixed at three points (see [Client.Tokens] and the fresh-token probe
// in the vendor's mint); THIS is the point at which it stops starting.
//
// A GENERATED ADDRESS IS STILL A ROBOT'S. GitLab's own is
// `service_account_group_…@noreply.<instance>`, which is the same promise the
// derived one was making and is one the instance can actually keep.

// Username is the service-account name for a seat.
//
// FROM THE HANDLE, which is immutable (ADR-0013): GitLab keeps no field of
// this engine's own, so this name is how the next pass recognises the account
// it made.
func Username(p *config.GitLabProvisioning, handle string) string {
	prefix := strings.TrimSpace(p.UsernamePrefix)
	if prefix == "" {
		prefix = "crewlet"
	}
	return prefix + "-" + handle
}
