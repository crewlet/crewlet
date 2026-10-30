package config

import (
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/org"
)

// WHERE A SEAT'S CREDENTIAL MAY BE WRITTEN, AND WHO ELSE MAY READ IT.
//
// A seat's credential at a vendor is its identity there, and the incident that
// made per-seat access a stored setting was one variable read by two parties:
// a seat's provisioning sink pointed at the variable the company's own block
// read, and the pass sealed the seat's minted key over the organization's. So
// every variable a seat's credential lives in has exactly one reader, and who
// writes it is decided per integration ([Company.SeatCredentialMode], D4b):
//
//   - ENGINE MODE, where the engine provisions the integration: the engine
//     mints the seat's credential and seals it under the seat's own name
//     ([envref.SealedName]). The document names none: a `${VAR}` under a
//     seat's or unit's `mcp_env.<kind>` is refused, and a role credential
//     position may name only the seat's own sealed name, which the engine
//     itself writes there (a GitHub App's key from the manifest flow).
//   - EXTERNAL MODE, where somebody outside Crewlet holds the account: the
//     seat names its credential's variable in `mcp_env.<kind>` or the role
//     credential position, and the engine never writes, rotates or forgets it
//     (D4a). That variable must be exclusive to the seat and may not be
//     anybody's sealed name.
//
// In both modes, anywhere in a seat's role entry, a reference to ANOTHER
// seat's sealed name or to a variable a company `integrations` block reads is
// refused: that is one seat reading another's identity, or the company's. A
// seat's own sealed names stay readable everywhere in its own entry, which is
// how its executor box reaches its own GitLab token for git.
//
// ADMISSION RULES like the rest of roleaccessrules.go; the access sync holds
// the same line again at the vendor (internal/seataccess).

// SeatSealedFields is every value the engine seals per seat for kind, by
// field, the credential first, as the vendors' catalogs name them; GitHub's
// APP_TOKEN is the installation token the engine issues at use. RESTATED for
// the reason [CodeAccessTiers] is: internal/seataccess holds the vendors'
// catalogs to it.
func SeatSealedFields(kind string) []string {
	switch kind {
	case "datadog":
		return []string{"APP_KEY"}
	case "gitlab":
		return []string{"TOKEN"}
	case "atlassian":
		return []string{"TOKEN", "EMAIL"}
	case "mattermost":
		return []string{"BOT_TOKEN"}
	case "slack":
		return []string{"BOT_TOKEN", "SIGNING_SECRET", "CLIENT_SECRET", "CLIENT_ID"}
	case "github":
		return []string{"APP_KEY", "APP_WEBHOOK_SECRET", "APP_TOKEN"}
	default:
		return nil
	}
}

// documentMode is [Company.SeatCredentialMode] as the document alone can tell
// it. Whether the deployment is managed (`api.auth.company_writers`) is a
// bootstrap fact the document does not carry, and it turns only GitHub's mode:
// GitHub's agents' apps are the engine's only where the engine writes the
// document. So the document's rules read GitHub as external, the stricter
// engine rules being a subset no managed deployment may be held to, and an
// agent's app key still reaches the seat only through its own exclusive
// variable.
func (c *Company) documentMode(kind string) SeatCredentialMode {
	return c.SeatCredentialMode(kind, true)
}

// credentialKindOf is the integration whose seat credential an `mcp_env`
// block named server carries, by the server's conventional name (Jira's and
// Confluence's are Atlassian's), and whether it carries one.
func credentialKindOf(server string) (string, bool) {
	kind := SeatCredentialKind(server)
	return kind, slices.Contains(SeatCredentialKinds(), kind)
}

// sealedOwners is every agent seat's sealed name, mapped to the seat it
// belongs to.
func (c *Company) sealedOwners() map[string]string {
	out := map[string]string{}
	for role := range c.EachRole() {
		handle := role.IdentityKey()
		if role.Kind == org.KindHuman || handle == "" {
			continue
		}
		for _, kind := range SeatCredentialKinds() {
			for _, field := range SeatSealedFields(kind) {
				out[envref.SealedName(kind, field, handle)] = handle
			}
		}
	}
	return out
}

// credentialPosition is one role field that names a seat's credential.
type credentialPosition struct {
	kind, field, key, value string
}

// credentialPositions is every role credential position the seat fills.
func (r *Role) credentialPositions() []credentialPosition {
	var out []credentialPosition
	add := func(kind, field, key, value string) {
		if strings.TrimSpace(value) != "" {
			out = append(out, credentialPosition{kind, field, key, value})
		}
	}
	in := r.Integrations
	if s := in.Slack; s != nil {
		add("slack", "BOT_TOKEN", "integrations.slack.bot_token", s.BotToken)
		add("slack", "SIGNING_SECRET", "integrations.slack.signing_secret", s.SigningSecret)
	}
	if m := in.Mattermost; m != nil {
		add("mattermost", "BOT_TOKEN", "integrations.mattermost.bot_token", m.BotToken)
	}
	if g := in.GitHub; g != nil {
		add("github", "APP_KEY", "integrations.github.private_key", g.PrivateKey)
		add("github", "APP_WEBHOOK_SECRET", "integrations.github.webhook_secret", g.WebhookSecret)
	}
	return out
}

// validateSeatCredentials is the rules above, over the whole document.
func (c *Company) validateSeatCredentials(refs VariableReferences) error {
	var p problems
	owners := c.sealedOwners()
	for role, path := range c.EachRole() {
		own := role.IdentityKey()
		if role.Kind == org.KindHuman || own == "" {
			continue
		}

		// ENGINE MODE: the document names no credential for the kind.
		for _, server := range sortedKeys(role.MCPEnv) {
			kind, isManaged := credentialKindOf(server)
			if !isManaged || c.documentMode(kind) != SeatCredentialEngine {
				continue
			}
			block := role.MCPEnv[server]
			for _, key := range sortedKeys(block) {
				if names := envref.Names(block[key]); len(names) > 0 && !allOwn(names, own, owners) {
					p.add(at(at(at(path, "mcp_env"), server), key), ErrConflict,
						"the engine provisions %s for this company, so it seals this agent's "+
							"credential itself and binds it to the agent's tool: remove this "+
							"key (a setting every agent shares goes in the mcp_servers entry)",
						kind)
				}
			}
		}

		// THE ROLE CREDENTIAL POSITIONS.
		for _, pos := range role.credentialPositions() {
			here := at(path, pos.key)
			name, whole := envref.Whole(strings.TrimSpace(pos.value))
			mine := envref.SealedName(pos.kind, pos.field, own)
			switch {
			case c.documentMode(pos.kind) == SeatCredentialEngine:
				if !whole || name != mine {
					p.add(here, ErrConflict,
						"the engine provisions %s for this company and seals this agent's "+
							"credential under ${%s} itself: remove this field, or name exactly "+
							"that variable", pos.kind, mine)
				}
			case whole && owners[name] != "" && name != mine:
				p.add(here, ErrConflict,
					"${%s} is the engine's own name for a credential it seals for %s, "+
						"so it cannot hold one provided from outside: name a variable "+
						"of this agent's own", name, owners[name])
			}
		}

		// EXCLUSIVE: no other seat's credential positions name this seat's.
		for _, name := range sortedKeys(refs.SeatCredentials[own]) {
			var others []string
			for _, handle := range sortedKeys(refs.SeatCredentials) {
				if handle != own && refs.SeatCredentials[handle][name] > 0 {
					others = append(others, handle)
				}
			}
			if len(others) > 0 {
				p.add(path, ErrConflict,
					"${%s} holds this agent's credential and is also named by seat %s: an "+
						"agent's credential has one reader. Give each agent its own "+
						"variable", name, strings.Join(others, " and "))
			}
		}

		// ANYWHERE IN THE SEAT: another seat's sealed name, or a company
		// credential.
		for _, name := range sortedKeys(refs.Seats[own]) {
			switch owner := owners[name]; {
			case owner != "" && owner != own:
				p.add(path, ErrConflict,
					"${%s} is seat %s's own credential, which no other seat may read: "+
						"remove the reference", name, owner)
			case owner == "" && refs.Integrations[name] > 0:
				p.add(path, ErrConflict,
					"${%s} is a company integration's credential, which no seat may "+
						"read: remove the reference (a value every agent's tool needs goes "+
						"in the mcp_servers entry)", name)
			}
		}
	}

	// A UNIT'S BLOCK IS EVERY MEMBER'S, so in engine mode it names nothing.
	for unit, path := range c.EachUnit() {
		for _, server := range sortedKeys(unit.MCPEnv) {
			kind, isManaged := credentialKindOf(server)
			if !isManaged || c.documentMode(kind) != SeatCredentialEngine {
				continue
			}
			block := unit.MCPEnv[server]
			for _, key := range sortedKeys(block) {
				if envref.Has(block[key]) {
					p.add(at(at(at(path, "mcp_env"), server), key), ErrConflict,
						"the engine provisions %s for this company and binds each agent's own "+
							"credential to its tool, so a unit names none: remove this key",
						kind)
				}
			}
		}
	}
	return p.err()
}

// allOwn reports whether every name is one of handle's own sealed names.
func allOwn(names []string, handle string, owners map[string]string) bool {
	return !slices.ContainsFunc(names, func(n string) bool { return owners[n] != handle })
}
