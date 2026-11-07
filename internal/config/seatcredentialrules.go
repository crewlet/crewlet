package config

import (
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
	"github.com/crewlet/crewlet/internal/words"
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
// AND IN ITS BOX, NEVER UNDER A NAME ITS EXECUTOR SIGNS IN BY. A box CLI reads
// the seat's credential from a variable the seat's box sets (doctl's
// DIGITALOCEAN_ACCESS_TOKEN, glab's GITLAB_TOKEN), and the seat's executor
// harness runs in the same environment. A harness that reads its model
// provider's key from its environment reads those very names (OpenCode keys
// its DigitalOcean and GitLab providers by them), so a seat whose executor
// entry signs in by one would make every model call with the seat's tool
// credential: the second reader this file exists to refuse
// ([Company.validateSeatSignIns]).
//
// ADMISSION RULES like the rest of roleaccessrules.go; the access sync holds
// the same line again at the vendor (internal/seataccess).

// seatField is one value the engine holds for an agent seat at an
// integration, by the field name the vendor's catalog gives it.
type seatField struct {
	name string
	// tool is a value a seat's tool server may read; the rest only the
	// engine reads (a Slack app's signing secret, a GitHub App's key).
	tool bool
}

// seatFields is each integration's per-seat values, the credential first,
// as the vendors' catalogs name them: THE ONE RESTATEMENT of them in config,
// which every per-kind list here derives from. Config is the leaf every
// vendor package depends on, so it restates rather than imports, and
// internal/seataccess holds every registered vendor's catalog to this list.
func seatFields(kind string) []seatField {
	switch kind {
	case "datadog":
		return []seatField{{"APP_KEY", true}}
	case "gitlab":
		return []seatField{{"TOKEN", true}}
	case "atlassian":
		return []seatField{{"TOKEN", true}, {"EMAIL", true}}
	case "mattermost":
		return []seatField{{"BOT_TOKEN", true}}
	case "slack":
		return []seatField{{"BOT_TOKEN", true}, {"SIGNING_SECRET", false},
			{"CLIENT_SECRET", false}, {"CLIENT_ID", false}}
	case "github":
		return []seatField{{"APP_KEY", false}, {"APP_WEBHOOK_SECRET", false}, {"APP_TOKEN", true}}
	case "cloudflare":
		return []seatField{{"API_TOKEN", true}}
	case "expo":
		// RETIRED_TOKEN is a turned-off seat's token, kept only to see its
		// robot deleted at Expo, and read by no tool.
		return []seatField{{"TOKEN", true}, {"RETIRED_TOKEN", false}}
	case "digitalocean":
		// RETIRED_TOKEN is a replaced or turned-off seat's token, kept only
		// until Crewlet's revoke of it at DigitalOcean succeeds, and read by
		// no tool.
		return []seatField{{"TOKEN", true}, {"RETIRED_TOKEN", false}}
	case "scaleway":
		// The secret key first, as the credential; the access key names it.
		return []seatField{{"SECRET_KEY", true}, {"ACCESS_KEY", true}}
	case "google_cloud":
		// The access token the engine issues at use for an hour, never sealed.
		return []seatField{{"ACCESS_TOKEN", true}}
	default:
		return nil
	}
}

// SeatSealedFields is every value the engine holds per seat for kind, by
// field, the credential first ([seatFields]). GitHub's APP_TOKEN is the
// installation token the engine issues at use.
func SeatSealedFields(kind string) []string {
	var out []string
	for _, f := range seatFields(kind) {
		out = append(out, f.name)
	}
	return out
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
// Confluence's are Atlassian's, google-cloud is google_cloud's), and whether it
// carries one.
func credentialKindOf(server string) (string, bool) {
	kind := SeatServerKind(server)
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

// engineOnly is handle's sealed names that no tool reads ([seatField.tool]):
// values the engine reads itself, from the role's credential position where
// it has one.
func engineOnly(handle string) map[string]bool {
	out := map[string]bool{}
	for _, kind := range SeatCredentialKinds() {
		for _, f := range seatFields(kind) {
			if !f.tool {
				out[envref.SealedName(kind, f.name, handle)] = true
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
				seats := "seat"
				if len(others) > 1 {
					seats = "seats"
				}
				p.add(path, ErrConflict,
					"${%s} holds this agent's credential and is also named by %s %s: an "+
						"agent's credential has one reader. Give each agent its own "+
						"variable", name, seats, words.And(others))
			}
		}

		// A VALUE ONLY THE ENGINE READS (a turned-off Expo robot's token) is
		// named nowhere but its own credential position: never a box, a
		// tool or a setting.
		mine, atPosition := engineOnly(own), map[string]int{}
		for _, pos := range role.credentialPositions() {
			for _, name := range envref.Names(pos.value) {
				atPosition[name]++
			}
		}
		for _, name := range sortedKeys(refs.Seats[own]) {
			if mine[name] && refs.Seats[own][name] > atPosition[name] {
				p.add(path, ErrConflict,
					"${%s} is a value the engine holds for this agent and reads only itself, "+
						"which no box, tool or setting may be handed: remove the reference", name)
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

// SeatCredentialVariables is every variable the agent seat with handle keeps
// one of its own vendor credentials in, mapped to the integration it is a
// credential at, by the rules above: its sealed names, which the engine writes
// in engine mode, and every variable its own credential positions name (its
// `mcp_env` block for an integration, its role credential fields), where
// somebody outside Crewlet holds the account. Empty for a human seat and a
// handle no seat holds.
//
// A value of the seat's box that references one is the seat's credential for a
// tool, which never signs its executor's harness in: config refuses it under a
// name the executor signs in by ([Company.validateSeatSignIns]), and a launch
// judges a run's sign-in without it (internal/engine).
func (c *Company) SeatCredentialVariables(handle string) map[string]string {
	for role := range c.EachRole() {
		if role.Kind != org.KindHuman && handle != "" && role.IdentityKey() == handle {
			return role.credentialVariables()
		}
	}
	return map[string]string{}
}

// credentialVariables is [Company.SeatCredentialVariables] for the agent seat
// r.
func (r *Role) credentialVariables() map[string]string {
	handle := r.IdentityKey()
	out := map[string]string{}
	for _, kind := range SeatCredentialKinds() {
		for _, field := range SeatSealedFields(kind) {
			out[envref.SealedName(kind, field, handle)] = kind
		}
	}
	for server, block := range r.MCPEnv {
		kind, managed := credentialKindOf(server)
		if !managed {
			continue
		}
		for _, value := range block {
			for _, name := range envref.Names(value) {
				out[name] = kind
			}
		}
	}
	for _, pos := range r.credentialPositions() {
		for _, name := range envref.Names(pos.value) {
			out[name] = pos.kind
		}
	}
	return out
}

// seatServerWords names the MCP server that reads a seat's credential at
// kind, as a refusal offers it in place of a box line: Atlassian's are the
// jira and confluence servers, and every other kind's is named for it.
func seatServerWords(kind string) string {
	if kind == "atlassian" {
		return "jira or confluence MCP server"
	}
	return kind + " MCP server"
}

// ExecutorSignIns is every name a member of the executor chain of the agent
// seat with handle signs its harness in by
// ([harnessprofile.Profile.CredentialEnvNames]), to the first such member's
// providers.llm key. Empty for a handle no agent seat with a chain holds.
func (c *Company) ExecutorSignIns(handle string) map[string]string {
	for seat := range c.executorSeats() {
		if seat.role.IdentityKey() == handle {
			return c.signIns(seat)
		}
	}
	return map[string]string{}
}

// signIns is [Company.ExecutorSignIns] for seat.
func (c *Company) signIns(seat executorSeat) map[string]string {
	signs := map[string]string{}
	for _, key := range c.runners(seat.chain) {
		provider := c.Providers.LLM[key]
		profile, err := harnessprofile.Load(provider.HarnessName(), provider.Overrides)
		if err != nil {
			continue
		}
		for _, name := range profile.CredentialEnvNames(provider.Env) {
			if _, seen := signs[name]; !seen {
				signs[name] = key
			}
		}
	}
	return signs
}

// validateSeatSignIns refuses a value of an agent seat's own box settings that
// carries one of the seat's credentials ([Company.SeatCredentialVariables])
// under a name a member of its executor chain signs its harness in by
// ([harnessprofile.Profile.CredentialEnvNames]).
//
// EVERY MEMBER THAT CAN RUN THE EXECUTOR, not the head's alone: a launch falls
// back down the chain ([Company.runners]), and the member it lands on runs in
// the box this seat's settings fill. A member whose profile does not load is
// skipped here, because that entry is refused where it is written
// ([LLMProvider.validateCredentials]) and names no sign-in to compare.
func (c *Company) validateSeatSignIns() error {
	var p problems
	for seat := range c.executorSeats() {
		box := seat.role.Sandbox
		if box == nil {
			continue
		}
		signs := c.signIns(seat)
		mine := seat.role.credentialVariables()
		check := func(here Path, name, value string) {
			key, signsIn := signs[name]
			if !signsIn {
				return
			}
			refs := envref.Names(value)
			i := slices.IndexFunc(refs, func(ref string) bool { return mine[ref] != "" })
			if i < 0 {
				return
			}
			kind := mine[refs[i]]
			provider := c.Providers.LLM[key]
			p.add(here, ErrConflict,
				"%s is also how providers.llm.%s, the %s harness that runs this agent's executor, signs in "+
					"to its model, so this line would sign the agent's model calls in with its own %s "+
					"credential ${%s}. Give this agent the %s instead and remove the line, or run its "+
					"executor on an entry that does not sign in by %s (another harness, or another model "+
					"provider)",
				name, key, provider.HarnessName(), kind, refs[i], seatServerWords(kind), name)
		}
		for _, name := range sortedKeys(box.Env) {
			check(entry(at(seat.path, "sandbox.env"), name), name, box.Env[name])
		}
		for i, step := range box.Setup {
			for _, name := range sortedKeys(step.Env) {
				check(entry(at(idx(at(seat.path, "sandbox.setup"), i), "env"), name), name, step.Env[name])
			}
		}
	}
	return p.err()
}
