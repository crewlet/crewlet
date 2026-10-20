package engine

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/atlassian"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/datadog"
	"github.com/crewlet/crewlet/internal/gitlab"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/mattermost"
	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/setup"
)

// A seat's tool server is started only with the identity it is configured to
// act as.
//
// A per-role child exists to BE one seat at a vendor, and what makes it that
// seat is the seat's own `mcp_env` block for the server — the configuration
// calls it the seat's tool credentials, and a unit's block is layered under
// it. So every `${VAR}` that block references must resolve to a value before
// a child is started from it, on this node or in a coding agent's box. One
// that resolves to nothing has three possible stories, and they are told
// apart rather than reported alike:
//
//   - A PROVISIONING PASS WILL MINT IT. A company that provisions its seats'
//     identities — an integration whose pass creates each agent's account and
//     seals its credential under the variable the seat points at — is claimed
//     before that pass has run: on every first boot, and for every seat a
//     revision adds. The child is NOT STARTED, and that is not a failure:
//     the heartbeat counts it as waiting, with a sentence naming the variable
//     and the integration that mints it, and the log says so once per pass at
//     info. It starts once the seal reaches this node (below).
//   - A PROVISIONING PASS WOULD MINT IT, AND CANNOT UNTIL A PERSON ACTS. Its
//     own credential does not resolve or is refused, an administrator has an
//     app or a setting to switch on, the seat's own account could not be made
//     or was switched off at the vendor by somebody else. Waiting would wait
//     for ever behind a screen saying nothing is wrong, so this is a FAILURE,
//     whose sentence is what that integration's last pass recorded — read
//     from the fleet's record of it, as the integration card reads it, and
//     never re-derived here ([mintStopped]).
//   - NOTHING WILL. The operator's own secret is missing, or the seat points
//     at a variable nobody holds. That is a failure, named with the variable
//     and the remedy.
//
// None is retried on a timer. Nothing about a child held back changes until
// this node's secret snapshot does, and every change to it is followed by a
// pass over every seat this node holds: an apply re-reads the store before it
// converges the seats, and a provisioning pass that seals asks for that pass
// itself the moment it has refreshed this node's snapshot
// ([refreshingSink.Flush]) — before, and whatever becomes of, the
// re-activation it requests so every OTHER node does the same
// ([Engine.rebuildForSealedSecrets]). `crewlet secrets set` writes the store
// and tells the operator to reload, which is an apply. A timer would retry
// nothing that could start. What a person does at an integration is the one
// thing no pass here can see, which is why a child held back on one is judged
// again on every heartbeat against the integration's last report rather than
// on its own pass alone ([Engine.mcpStatus]).
//
// # What it replaced
//
// The child was spawned with the variable empty. A vendor's server refuses
// that, so every first boot under provisioning logged each seat's server as
// failed, retried it on the restart schedule ([mcp.RestartWait]), and offered
// the seat's turns tools that refused `unavailable`, for a credential the
// engine itself knew it had not minted yet. A server that does not refuse an
// empty token is worse: it starts, and acts as nobody, or as whatever default
// identity it falls back to, under the seat's name.
//
// # Why the seat's block, and not every reference a server has
//
// The configuration marks a server's own `env` and `headers` as credential
// positions too, and a shared server's key lives there. They are left out of
// this rule anyway, for one reason: they are also where a host value is
// passed through by name, and nothing in the reference grammar says which of
// a block's references may resolve to nothing. The tools guide's own example
// passes `NPM_CONFIG_REGISTRY: "${NPM_CONFIG_REGISTRY}"`, unset on most hosts,
// and npm reads an empty `npm_config_*` as unset — so refusing every reference
// that resolves to nothing would refuse that server on every such host. A
// grammar that could say "optional" is not something this rule may invent on
// its own: the shell's `${VAR:-}` would reach the setup scripts the resolver
// deliberately leaves alone (internal/envref). So a reference there that
// resolves to nothing is passed on empty, as a shell would — internal/mcp
// warns with the key (`empty_env_vars`) — and a shared server, which carries
// no seat's identity at all, is governed by that half alone: whose key is
// unset starts, fails to authenticate, and is retried by its bridge on the
// restart schedule, as before. The seat's block carries no such passthrough —
// a unit's `JIRA_URL` there is one of the values the seat's server cannot
// work without — which is why it is the half the rule can hold.
//
// # Why here, and not in internal/mcp
//
// internal/mcp is handed values with their references already resolved, and
// it never reads the secret store, which is what keeps a tool server's
// credentials on one path ([serverSpec]). By the time a spec reaches it an
// unset token and a value deliberately written empty are the same empty
// string, and neither the variable nor whether anything will mint it is
// there to name. This is the last place both are in hand.
//
// # Why the seat's mailbox does not wait for it
//
// A claim returns once the seat's children have converged, because its
// contract is that what the first turn needs is up before the mailbox opens.
// A child held back for its credential is not something the first turn can
// need: the identity it would act as does not exist at the vendor yet, or
// waits on a person, so nothing there can be addressed to it, and holding the
// mailbox for a vendor pass — which may itself be waiting on an
// administrator's credential — would hold every message the seat could
// answer with the tools it has.

// seatMinter is a provisioning pass that mints seats' OWN credentials: it
// creates an account per agent at its vendor and seals each one's credential
// under the variable the seat's configuration points at.
//
// THE PLAN IS THE DECLARATION, and seatPlan is the only place it is made. The
// pass's own Run mints by it, the engine reads what a child waits for off it
// ([Engine.mintedFor]), and so does anything else that asks which seats this
// engine provisions at the vendor ([Engine.atlassianCredentialSealed]) — so a
// variable a pass would mint and the variable a child waits for cannot be two
// spellings of one fact. Two tests hold that: every pass this build runs
// declares this or is listed as minting no seat credential
// (TestEveryPassThatMintsASeatsCredentialDeclaresItsPlan), and no code in this
// package reads a vendor's plan anywhere but in a seatPlan or a teardown
// (TestAVendorPlanIsReadOnlyThroughSeatPlan) — a teardown, because what it
// removes is what may have been created, whether or not the company still
// asks for more.
type seatMinter interface {
	setup.Pass

	// seatPlan is the plan this pass mints seats' credentials by under c,
	// or an error where c does not ask it to mint any: its block absent,
	// switched off, or with no provisioning in it, which is
	// [integration.ErrNotConfigured]. A company that made its accounts by
	// hand is the last of those, and its variables are the operator's to
	// supply, not this pass's.
	seatPlan(c *Company) (*provision.Plan, error)
}

// seatPlan is every agent's Atlassian account. The block exists for nothing
// else: the organization key in it is what creates them.
func (*atlassianPass) seatPlan(c *Company) (*provision.Plan, error) {
	if c.Config.Integrations.Atlassian == nil {
		return nil, integration.ErrNotConfigured
	}
	return atlassian.PlanFor(c.Org)
}

// seatPlan is every agent's GitLab service account. gitlab.PlanFor itself
// refuses a block that is off or names no provisioning.
func (*gitlabPass) seatPlan(c *Company) (*provision.Plan, error) {
	return gitlab.PlanFor(c.Org, c.Config.Integrations.GitLab)
}

// seatPlan is every agent's Mattermost bot, where the company provisions
// them. ONLY THERE: mattermost.PlanFor plans for any enabled block, and a
// company whose bots were made by hand holds no provisioning block and no
// administrator token, so this engine's pass mints none of their tokens. A
// person may still run `crewlet mattermost provision` for it, which is their
// own gesture and seals into the store like `crewlet secrets set`.
func (*mattermostPass) seatPlan(c *Company) (*provision.Plan, error) {
	cfg := c.Config.Integrations.Mattermost
	if cfg == nil || !cfg.Enabled || cfg.Provisioning == nil {
		return nil, integration.ErrNotConfigured
	}
	return mattermost.PlanFor(c.Org, cfg)
}

// seatPlan is every agent's Datadog service account. The pass converges
// nothing for a block that is off, which datadog.PlanFor does not check.
func (*datadogPass) seatPlan(c *Company) (*provision.Plan, error) {
	cfg := c.Config.Integrations.Datadog
	if cfg == nil || !cfg.Enabled {
		return nil, integration.ErrNotConfigured
	}
	return datadog.PlanFor(c.Org, cfg)
}

// mintedFor is every variable a provisioning pass this build runs writes one
// seat's credentials into under c — its token and, where the vendor assigns
// one, its address — each with the integration that mints it.
//
// A pass whose plan cannot be derived mints nothing: what it would have to
// say about that is its own finding, on its integration's status.
func (e *Engine) mintedFor(c *Company, handle string) map[string]integration.Kind {
	var out map[string]integration.Kind
	for _, pass := range e.setupPasses() {
		minter, ok := pass.(seatMinter)
		if !ok {
			continue
		}
		plan, err := minter.seatPlan(c)
		if err != nil || plan == nil {
			continue
		}
		for _, seat := range plan.Seats {
			if seat.Handle != handle {
				continue
			}
			for _, name := range []string{seat.TokenVar, seat.EmailVar} {
				if name == "" {
					continue
				}
				if out == nil {
					out = map[string]integration.Kind{}
				}
				out[name] = pass.Kind()
			}
		}
	}
	return out
}

// seatCredentialMissing is why a seat's child was not started, or a coding
// agent not given a server: references the seat's own block for the server
// makes resolved to nothing. See the file comment.
type seatCredentialMissing struct {
	// seat is the seat's handle, which is what an integration's findings
	// name a seat by; server is the configuration's name for the server,
	// and instance the bridge's name for the seat's child of it.
	seat, server, instance string

	// unset are the variables nothing sets and nothing this engine runs
	// mints, and empty the ones that are set, to nothing: the operator's to
	// supply, with different remedies.
	unset, empty []string

	// minting are the variables a provisioning pass mints for this seat,
	// and by the integration that mints each.
	minting map[string]integration.Kind

	// stopped is, for each integration that mints one of those and cannot
	// until a person acts, what its last pass recorded about it
	// ([mintStopped]) — the rest of the sentence after "will not mint".
	// Empty where nothing has been judged, or nothing stops it.
	stopped map[integration.Kind]string
}

// seatCredentialsMissing reports what of a seat's block for server resolves to
// nothing, or nil when every reference in it resolves. minted is what the
// seat's provisioning passes mint ([Engine.mintedFor]).
//
// The block's keys are read in order, so a report and the log line it is
// written to name the same variables the same way at every pass.
func seatCredentialsMissing(
	seat, server, instance string, block map[string]string,
	env *config.Resolver, minted map[string]integration.Kind,
) *seatCredentialMissing {
	var names []string
	for _, key := range slices.Sorted(maps.Keys(block)) {
		names = append(names, env.Missing(block[key])...)
	}
	m := credentialGap(seat, names, env, minted)
	if m != nil {
		m.server, m.instance = server, instance
	}
	return m
}

// credentialGap sorts variables that resolved to nothing into the three
// stories — minted for this seat, set to nothing, set nowhere — each named
// once, in the order given. Nil when there are none.
func credentialGap(
	seat string, names []string, env *config.Resolver, minted map[string]integration.Kind,
) *seatCredentialMissing {
	var m *seatCredentialMissing
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		if m == nil {
			m = &seatCredentialMissing{seat: seat}
		}
		if kind, ok := minted[name]; ok {
			if m.minting == nil {
				m.minting = map[string]integration.Kind{}
			}
			m.minting[name] = kind
			continue
		}
		// SET TO NOTHING IS NOT UNSET, and the two are told apart because
		// their remedies are: a variable exported empty is exported, so
		// "export it" sends the operator to do what they already did.
		if _, set := env.LookupOK(name); set {
			m.empty = append(m.empty, name)
			continue
		}
		m.unset = append(m.unset, name)
	}
	return m
}

// waiting reports a child held back for provisioning alone, which is not a
// failure: everything it lacks, a pass will mint, and nothing that pass has
// recorded says a person must act first.
func (m *seatCredentialMissing) waiting() bool {
	return len(m.unset) == 0 && len(m.empty) == 0 && len(m.stopped) == 0
}

// mintingKinds are the integrations that mint what m waits for, in name order.
func (m *seatCredentialMissing) mintingKinds() []integration.Kind {
	kinds := make(map[integration.Kind]struct{}, len(m.minting))
	for _, kind := range m.minting {
		kinds[kind] = struct{}{}
	}
	return slices.Sorted(maps.Keys(kinds))
}

// judged is m weighed against what each integration minting one of its
// variables last recorded, keyed by integration, as the fleet holds it.
//
// A FRESH VALUE, never m changed in place: the heartbeat judges the outcome a
// pass filed, on every beat, while the next pass may be filing another. Nil
// states is "this node could not read the record", and m is then returned
// with the judgement its pass stored rather than cleared — the record not
// answering for a moment is no evidence that a person has acted. That is the
// pass's judgement and not the previous beat's, so for the one beat that
// cannot read a wait the last beat judged stopped can read as waiting again,
// and the reverse, until the next beat reads the record.
func (m *seatCredentialMissing) judged(states map[integration.Kind]integration.State) *seatCredentialMissing {
	if m == nil || states == nil || len(m.minting) == 0 {
		return m
	}
	out := *m
	out.stopped = nil
	for _, kind := range m.mintingKinds() {
		state, found := states[kind]
		if !found {
			// NO PASS HAS RUN, which is the first boot this rule exists
			// for: the claim always comes first.
			continue
		}
		if why, stopped := mintStopped(state, m.seat); stopped {
			if out.stopped == nil {
				out.stopped = map[integration.Kind]string{}
			}
			out.stopped[kind] = why
		}
	}
	return &out
}

// mintStopped reports whether an integration's last recorded pass says it will
// not mint seat's credentials until a person acts, and what it found if so —
// the end of a sentence that begins "the <kind> integration will not mint
// ${…}".
//
// READ OFF THE PASS'S OWN CONCLUSION, by the vocabulary that pass reported
// in, and never re-derived here: whether its credential resolves, whether the
// vendor accepted it, whether the seat's account exists are each a vendor's
// question, and a second answer beside the pass's would be the copy that
// drifts. Three things stop it:
//
//   - A disconnect, which mints nothing for anybody.
//   - A finding ABOUT THIS SEAT that a person owes ([integration.Actor]) —
//     its account could not be made, or somebody switched it off at the
//     vendor. Failing that, a finding about this seat that the engine or the
//     vendor owes — the account is being made, its grant is landing — is the
//     opposite answer: work on it is in hand, whatever else the integration
//     reports.
//   - A report a person owes at a station BEFORE IDENTITIES — the pass's own
//     credential, an app or an approval ([integration.Phase]): a pass held
//     there reached no seat. A report at a later station — a webhook that
//     cannot be registered, a grant too small — is about something the pass
//     did after the accounts, which it made.
//
// One case reads as stopped that is not: a pass that minted this seat's
// credential and reported, about something else, a finding before
// identities — Datadog's webhook token is one. It lasts only until the seal
// reaches this node, which its re-activation does in seconds, and the node
// that sealed asked for its own pass before the report was written.
func mintStopped(state integration.State, seat string) (string, bool) {
	if state.TearingDown() {
		return "it is being disconnected", true
	}
	inHand := false
	for _, f := range state.Findings {
		if f.Subject != seat && !slices.Contains(f.Subjects, seat) {
			continue
		}
		if _, actor := f.Kind.Verdict(); !actor.WaitsOnAPerson() {
			inHand = true
			continue
		}
		// The finding's own sentence, or the vocabulary's for its kind
		// where it carried none.
		return "until a person acts; its last pass found: " +
			integration.Classify([]integration.Finding{f}).Detail, true
	}
	if inHand {
		return "", false
	}
	report := state.Reported()
	if report.Actor.WaitsOnAPerson() && beforeIdentities(report.Phase) {
		return "until a person acts; its last pass found: " + report.Detail, true
	}
	return "", false
}

// beforeIdentities reports a phase at a station an integration passes before
// its pass reaches any seat's identity — in [integration.Phases]' order, the
// ones ahead of [integration.PhaseProvisioning]. A phase this build does not
// know is at no station it can place.
func beforeIdentities(p integration.Phase) bool {
	at := slices.Index(integration.Phases, p)
	return at >= 0 && at < slices.Index(integration.Phases, integration.PhaseProvisioning)
}

// reason says what is missing and how to supply it, or what is awaited — no
// value, only variables. Failures first, because the heartbeat bounds the
// sentence ([coord.MaxMCPErrorBytes]) and what somebody has to act on is the
// part that must survive the cut.
func (m *seatCredentialMissing) reason() string {
	var parts []string
	if len(m.unset) > 0 {
		parts = append(parts, fmt.Sprintf("%s %s and nothing this engine runs mints %s: "+
			"seal %s (crewlet secrets set, or the integration's provision command) and "+
			"POST /config/reload, or set %s in the engine's own environment and restart it",
			varRefs(m.unset), plural(m.unset, "is not set", "are not set"),
			plural(m.unset, "it", "them"), plural(m.unset, "it", "them"),
			plural(m.unset, "it", "them")))
	}
	if len(m.empty) > 0 {
		parts = append(parts, fmt.Sprintf("%s %s, which is no credential: seal a value "+
			"(crewlet secrets set) and POST /config/reload, or take the key out of the "+
			"seat's mcp_env if the server needs none",
			varRefs(m.empty), plural(m.empty, "is set to an empty value", "are set to empty values")))
	}
	var waits []string
	for _, kind := range m.mintingKinds() {
		var names []string
		for name, k := range m.minting {
			if k == kind {
				names = append(names, name)
			}
		}
		slices.Sort(names)
		if why, stopped := m.stopped[kind]; stopped {
			parts = append(parts, fmt.Sprintf("the %s integration will not mint %s %s",
				kind, varRefs(names), why))
			continue
		}
		waits = append(waits, fmt.Sprintf("the %s integration to mint %s", kind, varRefs(names)))
	}
	if len(waits) > 0 {
		parts = append(parts, "waiting for "+strings.Join(waits, " and "))
	}
	return strings.Join(parts, "; ")
}

// Error is the sentence the heartbeat carries and the log writes: why the
// child is not started, and — for a child that is only waiting — when it
// starts.
func (m *seatCredentialMissing) Error() string {
	when := ""
	if m.waiting() {
		when = "; it starts once the seal reaches this node"
	}
	return "not started: " + m.reason() + when
}

// mintedVariables lists what a child waits for, sorted, for a log line.
func (m *seatCredentialMissing) mintedVariables() []string {
	return slices.Sorted(maps.Keys(m.minting))
}

// logAttrs are a log line's fields naming what is missing, in its three kinds
// and the integrations judged as stopped, each only where there is one: an
// empty list in a line reads as a value somebody computed.
func (m *seatCredentialMissing) logAttrs() []any {
	var attrs []any
	if len(m.unset) > 0 {
		attrs = append(attrs, "unset", m.unset)
	}
	if len(m.empty) > 0 {
		attrs = append(attrs, "empty", m.empty)
	}
	if len(m.minting) > 0 {
		attrs = append(attrs, "awaiting", m.mintedVariables())
	}
	if len(m.stopped) > 0 {
		attrs = append(attrs, "stopped", slices.Sorted(maps.Keys(m.stopped)))
	}
	return attrs
}

// plural picks the singular or the plural phrase by how many names there are.
func plural(names []string, one, many string) string {
	if len(names) == 1 {
		return one
	}
	return many
}

// varRefs renders variable names as the references an operator wrote:
// "${A}", "${A} and ${B}", "${A}, ${B} and ${C}".
func varRefs(names []string) string {
	refs := make([]string, len(names))
	for i, name := range names {
		refs[i] = "${" + name + "}"
	}
	if len(refs) == 1 {
		return refs[0]
	}
	return strings.Join(refs[:len(refs)-1], ", ") + " and " + refs[len(refs)-1]
}

// mintingStates reads the fleet's record of every integration that mints a
// variable one of missing waits for, keyed by integration — nil when nothing
// waits on one, so nothing is read, or when the record could not be read,
// which [seatCredentialMissing.judged] takes as "cannot say".
//
// ONE READ PER INTEGRATION, and only for a child held back on one, which is a
// first boot, a seat a revision added, or a company whose provisioning a
// person has to unblock: on every other beat nothing waits and nothing is
// read. The record is the fleet's rather than this node's because the pass
// that writes it runs on whichever node holds the integration duty.
func (e *Engine) mintingStates(
	ctx context.Context, missing []*seatCredentialMissing,
) map[integration.Kind]integration.State {
	kinds := map[integration.Kind]struct{}{}
	for _, m := range missing {
		for _, kind := range m.mintingKinds() {
			kinds[kind] = struct{}{}
		}
	}
	if len(kinds) == 0 || e.backends == nil || e.backends.Fleet == nil {
		return nil
	}
	store, err := e.IntegrationStore()
	if err != nil {
		return nil
	}
	states := make(map[integration.Kind]integration.State, len(kinds))
	for _, kind := range slices.Sorted(maps.Keys(kinds)) {
		state, found, err := store.LoadIntegration(ctx, kind)
		if err != nil {
			return nil
		}
		if found {
			states[kind] = state
		}
	}
	return states
}
