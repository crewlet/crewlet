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

// A tool server is started only with what its configuration references.
//
// Every `${VAR}` a server's spec resolves — its own `env` or `headers`, its
// `url`, and, for a seat's child of a per-role template, the seat's own
// `mcp_env` block for it, a unit's block layered under that — must resolve to
// a value before the server is started: on this node, as a shared server or a
// seat's child, or in a coding agent's box. A credential that arrives empty
// either fails the server, which is then retried for a value that is not
// coming, or — worse — is accepted, and the server acts as nobody, or as
// whatever default identity it falls back to, under the seat's name. One that
// resolves to nothing has three possible stories, and they are told apart
// rather than reported alike:
//
//   - A PROVISIONING PASS WILL MINT IT. A company that provisions its seats'
//     identities — an integration whose pass creates each agent's account and
//     seals its credential under the variable the seat points at — is claimed
//     before that pass has run: on every first boot, and for every seat a
//     revision adds. The child is NOT STARTED, and that is not a failure:
//     the heartbeat counts it as waiting, with a sentence naming the variable
//     and the integration that mints it, and the log says so once per pass at
//     info. It starts once the seal reaches this node (below). Only a seat's
//     values can be this: a pass mints per seat, so a shared server has
//     nothing waiting for it.
//   - A PROVISIONING PASS WOULD MINT IT, AND CANNOT UNTIL A PERSON ACTS. Its
//     own credential does not resolve or is refused, an administrator has an
//     app or a setting to switch on, the seat's own account could not be made
//     or was switched off at the vendor by somebody else. Waiting would wait
//     for ever behind a screen saying nothing is wrong, so this is a FAILURE,
//     whose sentence is what that integration's last pass recorded — read
//     from the fleet's record of it, as the integration card reads it, and
//     never re-derived here ([mintStopped]).
//   - NOTHING WILL. The operator's own secret is missing, or a value points
//     at a variable nobody holds. That is a failure, named with the variable
//     and the remedies: supply it — with the integration's own provision
//     command too, for a seat's bot or account — or, where one of the
//     server's own keys may go without it, declare that key optional.
//
// None is retried on a timer. Nothing about a server held back changes until
// this node's secret snapshot does, and every change to it is followed by a
// pass over every seat this node holds and by an apply's reconcile of the
// shared servers: an apply re-reads the store before it converges either, and
// a provisioning pass that seals asks for the seats' pass itself the moment it
// has refreshed this node's snapshot ([refreshingSink.Flush]) — before, and
// whatever becomes of, the re-activation it requests so every OTHER node does
// the same ([Engine.rebuildForSealedSecrets]). `crewlet secrets set` writes the
// store and tells the operator to reload, which is an apply. A timer would
// retry nothing that could start. What a person does at an integration is the
// one thing no pass here can see, which is why a child held back on one is
// judged again on every heartbeat against the integration's last report rather
// than on its own pass alone ([Engine.mcpStatus]).
//
// # A key that may go without its value is declared, and left out
//
// A host setting passed through by name — the tools guide's
// `NPM_CONFIG_REGISTRY: "${NPM_CONFIG_REGISTRY}"`, unset on most hosts — is a
// reference that may legitimately resolve to nothing, and nothing in a value
// can say so. So the server declares it: a key listed in its
// [config.MCPServer.OptionalKeys] whose value does not resolve is LEFT OUT,
// whole, and holds nothing back — the child is started without the variable,
// the request sent without the header. Never half a value ("Bearer " is a
// credential that looks set) and never an empty one. The list is the server's
// and governs every value it is started with, a seat's block included,
// because a seat's block lands in the same environment or the same headers as
// the server's own; and it is read after the layers are merged, so a seat's
// value that resolves to nothing leaves the key out rather than handing back
// the template's — which, for a key carrying an identity, would start the
// seat's child as whoever the template names. The url is never optional: a
// server is dialled at it.
//
// Two things the declaration does NOT do. It does not stop the wait for a
// variable a pass mints for the seat: that value is coming, and started
// without it the child would act as nobody until the seal — the first story
// above, reached again by a declaration. And the sentence never OFFERS it for
// a seat's own block, which is the identity the seat acts as, or for a key
// whose variable the url needs too; it offers it for the server's own
// settings, where a host's passthrough is written.
//
// THE SHELL'S `${VAR:-}` IS NOT HOW, although it reads like the obvious
// spelling: the reference grammar deliberately parses no shell expansion, so a
// sandbox setup step's script survives config resolution untouched
// (internal/envref), and teaching it `:-` would substitute inside operators'
// scripts.
//
// # What it replaced
//
// A seat's block was held to this alone, and a server's own `env` and
// `headers` were passed on empty: a shared server whose key was unset started,
// failed to authenticate, and was retried by its bridge on the restart
// schedule for a value nothing would supply, its tools listed and refusing,
// with an `empty_env_vars` warning naming the key. They were left out of the
// rule because nothing said which of their references may resolve to nothing;
// the declaration is what says. And before the seat's half, a seat's child was
// spawned with its credential empty: every first boot under provisioning
// logged each seat's server as failed, retried it, and offered the seat's
// turns tools that refused `unavailable`, for a credential the engine itself
// knew it had not minted yet.
//
// # Why here, and not in internal/mcp
//
// internal/mcp is handed values with their references already resolved, and
// it never reads the secret store, which is what keeps a tool server's
// credentials on one path ([serverSpec]). By the time a spec reaches it an
// unset token and a value deliberately written empty are the same empty
// string, and neither the variable nor whether anything will mint it is
// there to name. This is the last place both are in hand — and the ONE place
// a server's values are resolved at all: [resolveServer] is what [serverSpec]
// and a coding agent's box are built from, so no path starts a server around
// it.
//
// # Why the seat's mailbox does not wait for it
//
// A claim returns once the seat's children have converged, because its
// contract is that what the first turn needs is up before the mailbox opens.
// A child held back for its credential is not something the first turn can
// need: the identity it would act as does not exist at the vendor yet, or
// waits on a person, or on an operator's secret, so nothing there can be
// addressed to it, and holding the mailbox for it — which may itself be
// waiting on an administrator's credential — would hold every message the
// seat could answer with the tools it has.

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

// seatPlan is every agent that is on at Slack, whose bot token the access sync
// seals once a person approves its app's install.
func (p *slackPass) seatPlan(c *Company) (*provision.Plan, error) {
	return p.engine.seatAccessPlan(c, integration.KindSlack)
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
	// AND THE TOKEN THE ENGINE ISSUES THE SEAT AT USE, which no pass seals
	// and which waits on the same thing a seal does: an integration's step,
	// here a person creating and installing the agent's app. Declared beside
	// the plans so a value naming it is held back and told as waiting on
	// that integration, never reported as a secret the operator forgot.
	// See githubtoken.go.
	if name := githubIssued(c, handle); name != "" {
		if out == nil {
			out = map[string]integration.Kind{}
		}
		out[name] = integration.KindGitHub
	}
	return out
}

// seatMint is what one seat's values may be waiting for: the variables an
// integration mints or issues for the seat, by integration
// ([Engine.mintedFor]), and the seat's own issued token as this read asked
// for it ([seatToken]), which says why one it could not issue went without.
//
// NIL IS A VALUE NOTHING MINTS, which is a shared server's: it acts as no
// seat, so nothing is minted or issued for it.
type seatMint struct {
	kinds map[string]integration.Kind
	token *seatToken
}

// kind is the integration that mints or issues name for the seat, if any.
func (m *seatMint) kind(name string) (integration.Kind, bool) {
	if m == nil {
		return "", false
	}
	kind, ok := m.kinds[name]
	return kind, ok
}

// unissued is why the engine could not issue name this time, or "".
func (m *seatMint) unissued(name string) string {
	if m == nil {
		return ""
	}
	return m.token.unissued(name)
}

// credentialMissing is why a server was not started — a seat's child, a
// shared server, or one a coding agent's box was not given — or what a coding
// run's own environment lacks ([Engine.runEnvironment]): references among the
// values it is started with resolved to nothing, and the keys holding them are
// not declared optional. See the file comment.
type credentialMissing struct {
	// seat is the seat's handle, which is what an integration's findings
	// name a seat by — empty for a shared server; server is the
	// configuration's name for the server, and instance the bridge's name
	// for this start of it, both empty for a coding run.
	seat, server, instance string

	// reader is what goes without a value its sentence offers to declare
	// optional: "the server", or "the run".
	reader string

	// unset are the variables nothing sets and nothing this engine runs
	// mints, and empty the ones that are set, to nothing: the operator's to
	// supply, with different remedies.
	unset, empty []string

	// issued are those of unset that a seat's own block holds — the
	// credential of its bot or its account, which the integration's own
	// provision command seals as `crewlet secrets set` does. Nothing seals a
	// server's own setting that way, so its remedy never offers the command.
	issued []string

	// optional names, by the keys holding one of unset or empty, where each
	// key could be declared optional instead — for the sentence's remedy.
	// Only the server's own keys: a seat's block is the identity it acts as,
	// which the engine never offers to leave out; and never a key holding a
	// variable the url needs, which would leave the server held back by its
	// url once declared.
	optional map[string]string

	// minting are the variables a provisioning pass mints for this seat,
	// and by the integration that mints each.
	minting map[string]integration.Kind

	// unissued are the variables the engine went to issue this seat at use
	// and could not, each with why ([seatToken.unissued]): a failure, but a
	// passing one, retried at the next attempt. Never a token.
	unissued map[string]string

	// stopped is, for each integration that mints one of those and cannot
	// until a person acts, what its last pass recorded about it
	// ([mintStopped]) — the rest of the sentence after "will not mint".
	// Empty where nothing has been judged, or nothing stops it.
	stopped map[integration.Kind]string
}

// serverValues are what one start of a tool server reads, every reference
// resolved: the environment of a stdio server or the headers of an http one,
// and its url.
type serverValues struct {
	values map[string]string
	url    string
}

// resolveServer is THE RULE every start of a tool server passes through: the
// values server is started with — its own env (stdio) or headers (http), and
// block, a seat's mcp_env for it, over them key by key — with every reference
// resolved, or why it may not start. See the file comment.
//
// seat is the seat's handle and mint what is minted or issued for it
// ([seatMint]), both empty for a shared server; env is the seat's own
// resolution ([Engine.seatResolver]); instance is what the start is called.
//
// MERGED BEFORE ANYTHING IS RESOLVED, so a seat's key replaces the server's
// whole: a seat's value that resolves to nothing never hands the key back to
// the template's default, which for a key carrying an identity would start
// the seat's child as whoever the template names. And merged by the key the
// server READS ([config.MCPServer.ValueKey]): an http header is one header in
// any case, so a seat's `authorization` replaces its template's
// `Authorization` rather than going out beside it. Each layer is read in key
// order, and the keys after it, so a report and the log line it is written to
// name the same variables the same way at every pass — and two spellings of
// one header in one layer, which a written document may not hold, resolve the
// same way at every pass too.
func resolveServer(
	server *config.MCPServer, seat, instance string, block map[string]string,
	env *config.Resolver, mint *seatMint,
) (serverValues, *credentialMissing) {
	own := server.Env
	if server.Kind() == config.TransportHTTP {
		own = server.Headers
	}
	type layered struct {
		key, value string
		// seat is whether the seat's block wrote the value.
		seat bool
	}
	merged := make(map[string]layered, len(own)+len(block))
	for _, key := range slices.Sorted(maps.Keys(own)) {
		merged[server.ValueKey(key)] = layered{key: key, value: own[key]}
	}
	for _, key := range slices.Sorted(maps.Keys(block)) {
		merged[server.ValueKey(key)] = layered{key: key, value: block[key], seat: true}
	}

	out := serverValues{url: env.Value(server.URL)}
	gap := refGap{env: env, mint: mint}
	gap.pin(env.Missing(server.URL))
	where := "mcp_servers." + server.Name + ".optional_keys"
	for _, id := range slices.Sorted(maps.Keys(merged)) {
		l := merged[id]
		missing := env.Missing(l.value)
		if len(missing) == 0 {
			if out.values == nil {
				out.values = make(map[string]string, len(merged))
			}
			out.values[l.key] = env.Value(l.value)
			continue
		}
		offer := where
		if l.seat {
			offer = ""
		}
		gap.hold(l.key, missing, server.OptionalKey(l.key), offer, l.seat)
	}
	m := gap.missing(seat, "the server")
	if m == nil {
		return out, nil
	}
	m.server, m.instance = server.Name, instance
	return serverValues{}, m
}

// refGap gathers what a set of values lacks, key by key: the references among
// them that resolve to nothing, for [credentialGap] to sort into its stories,
// and what each of the sentence's remedies may offer for them.
//
// ONE GATHERING for every set of values the engine starts something with, so
// whether a key may be left out, and which remedy names it, is decided by one
// rule wherever a value is read.
type refGap struct {
	env  *config.Resolver
	mint *seatMint

	// names are the variables the gap holds, in the order met.
	names []string
	// declarable is, for each variable, the keys holding it and where each
	// could be declared optional.
	declarable map[string]map[string]string
	// pinned are variables something with no key needs — a server's url —
	// so declaring a key holding one optional would start nothing.
	pinned map[string]bool
	// issued are variables a seat's own block holds.
	issued map[string]bool
}

// hold records missing, the references key's value lacks, and reports whether
// the key holds anything back.
//
// optional is whether the key is declared optional; where is where it could
// be, for the remedy, or "" for a key the remedy never offers to leave out;
// issued is whether a seat's own block wrote the value.
//
// A DECLARED KEY HOLDS BACK ONLY WHAT A PASS MINTS FOR THE SEAT, which is
// waited for whatever the declaration says: the declaration is for a value
// nothing will supply, and that one is coming. Left out instead, the seat's
// child would start without its identity and act as nobody until the seal —
// the failure this rule exists to stop, reached again by a declaration the
// sentence itself had offered.
func (g *refGap) hold(key string, missing []string, optional bool, where string, issued bool) bool {
	holds := false
	for _, name := range missing {
		if _, minted := g.mint.kind(name); optional && !minted {
			continue
		}
		holds = true
		g.names = append(g.names, name)
		if issued {
			if g.issued == nil {
				g.issued = map[string]bool{}
			}
			g.issued[name] = true
		}
		if where == "" || optional {
			continue
		}
		if g.declarable == nil {
			g.declarable = map[string]map[string]string{}
		}
		if g.declarable[name] == nil {
			g.declarable[name] = map[string]string{}
		}
		g.declarable[name][key] = where
	}
	return holds
}

// pin records missing, the references something with no key needs — a
// server's url, at which it is dialled.
func (g *refGap) pin(missing []string) {
	for _, name := range missing {
		g.names = append(g.names, name)
		if g.pinned == nil {
			g.pinned = map[string]bool{}
		}
		g.pinned[name] = true
	}
}

// missing is the gap, sorted into its stories ([credentialGap]) with what each
// remedy names, or nil where every value resolved or was left out. reader is
// what goes without a value the sentence offers to declare optional.
func (g *refGap) missing(seat, reader string) *credentialMissing {
	m := credentialGap(seat, g.names, g.env, g.mint)
	if m == nil {
		return nil
	}
	m.reader = reader
	for _, name := range m.unset {
		if g.issued[name] {
			m.issued = append(m.issued, name)
		}
	}
	for _, name := range append(slices.Clone(m.unset), m.empty...) {
		if g.pinned[name] {
			continue
		}
		for key, where := range g.declarable[name] {
			if m.optional == nil {
				m.optional = map[string]string{}
			}
			m.optional[key] = where
		}
	}
	return m
}

// credentialGap sorts variables that resolved to nothing into the stories
// (issued for this seat and refused this time, minted or issued for this seat
// and not yet there, set to nothing, set nowhere), each named once, in the
// order given. Nil when there are none.
func credentialGap(
	seat string, names []string, env *config.Resolver, mint *seatMint,
) *credentialMissing {
	var m *credentialMissing
	seen := map[string]bool{}
	for _, name := range names {
		if seen[name] {
			continue
		}
		seen[name] = true
		if m == nil {
			m = &credentialMissing{seat: seat}
		}
		// TRIED AND REFUSED IS NOT WAITING. A token the engine went to
		// issue and GitHub would not give is a failure with GitHub's
		// reason, retried, never "waiting for the integration", which
		// would read as a step somebody has yet to take.
		if why := mint.unissued(name); why != "" {
			if m.unissued == nil {
				m.unissued = map[string]string{}
			}
			m.unissued[name] = why
			continue
		}
		if kind, ok := mint.kind(name); ok {
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
func (m *credentialMissing) waiting() bool {
	return len(m.unset) == 0 && len(m.empty) == 0 && len(m.stopped) == 0 && len(m.unissued) == 0
}

// arrival is what a value that is only waiting arrives with: the seal, for
// a credential a pass seals into the store, or the agent's app being
// installed, for one the engine issues at use ([issuedAtUse]).
func (m *credentialMissing) arrival() string {
	for _, kind := range m.mintingKinds() {
		if !issuedAtUse(kind) {
			return "the seal reaches this node"
		}
	}
	return "the agent's GitHub App is installed"
}

// mintingKinds are the integrations that mint what m waits for, in name order.
func (m *credentialMissing) mintingKinds() []integration.Kind {
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
func (m *credentialMissing) judged(states map[integration.Kind]integration.State) *credentialMissing {
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
// value, only variables and keys. Failures first, because the heartbeat bounds
// the sentence ([coord.MaxMCPErrorBytes]) and what somebody has to act on is
// the part that must survive the cut.
func (m *credentialMissing) reason() string {
	var parts []string
	if len(m.unset) > 0 {
		// A seat's bot or account made by hand is sealed by its
		// integration's own command as well; a server's own setting is
		// nobody's account, and no command seals it.
		how := "crewlet secrets set"
		switch {
		case len(m.issued) == len(m.unset):
			how += ", or the integration's provision command"
		case len(m.issued) > 0:
			how += ", or for " + varRefs(m.issued) + " the integration's provision command"
		}
		parts = append(parts, fmt.Sprintf("%s %s and nothing this engine runs mints %s: "+
			"seal %s (%s), which a running node re-activates the company for as it "+
			"stores it (POST /config/reload for one stored with the engine stopped), "+
			"or set %s in the engine's own environment and restart it",
			varRefs(m.unset), plural(m.unset, "is not set", "are not set"),
			plural(m.unset, "it", "them"), plural(m.unset, "it", "them"), how,
			plural(m.unset, "it", "them")))
	}
	if len(m.empty) > 0 {
		parts = append(parts, fmt.Sprintf("%s %s: seal a value (crewlet secrets set), which "+
			"a running node re-activates the company for as it stores it",
			varRefs(m.empty), plural(m.empty, "is set to an empty value", "are set to empty values")))
	}
	for _, name := range slices.Sorted(maps.Keys(m.unissued)) {
		// NOT "at the next attempt": the issuer holds a refusal for the
		// restart schedule's wait, and an attempt inside it is handed the
		// same answer without a request ([github.Issuer.RetryWait]).
		parts = append(parts, fmt.Sprintf("${%s} could not be issued: %s; GitHub is asked "+
			"for it again on the restart schedule", name, m.unissued[name]))
	}
	if remedy := m.optionalRemedy(); remedy != "" {
		parts = append(parts, remedy)
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
		verb := "mint"
		if issuedAtUse(kind) {
			verb = "issue"
		}
		if why, stopped := m.stopped[kind]; stopped {
			parts = append(parts, fmt.Sprintf("the %s integration will not %s %s %s",
				kind, verb, varRefs(names), why))
			continue
		}
		waits = append(waits, fmt.Sprintf("the %s integration to %s %s", kind, verb, varRefs(names)))
	}
	if len(waits) > 0 {
		parts = append(parts, "waiting for "+strings.Join(waits, " and "))
	}
	return strings.Join(parts, "; ")
}

// optionalRemedy is the last remedy for what the operator has to supply: the
// keys holding it, and where each could be declared optional instead, for a
// value the server or the run can go without — or "" where no key holding one
// is one the sentence offers to leave out.
func (m *credentialMissing) optionalRemedy() string {
	if len(m.optional) == 0 {
		return ""
	}
	byWhere := map[string][]string{}
	for key, where := range m.optional {
		byWhere[where] = append(byWhere[where], key)
	}
	var lists []string
	for _, where := range slices.Sorted(maps.Keys(byWhere)) {
		keys := byWhere[where]
		slices.Sort(keys)
		lists = append(lists, joined(keys)+" under "+where)
	}
	keys := slices.Collect(maps.Keys(m.optional))
	return fmt.Sprintf("or, if %s can go without %s, list %s",
		m.reader, plural(keys, "it", "them"), strings.Join(lists, " and "))
}

// Error is the sentence the heartbeat carries and the log writes: why the
// child is not started, and — for a child that is only waiting — when it
// starts.
func (m *credentialMissing) Error() string {
	when := ""
	if m.waiting() {
		when = "; it starts once " + m.arrival()
	}
	return "not started: " + m.reason() + when
}

// mintedVariables lists what a child waits for, sorted, for a log line.
func (m *credentialMissing) mintedVariables() []string {
	return slices.Sorted(maps.Keys(m.minting))
}

// logAttrs are a log line's fields naming what is missing, in its three kinds
// and the integrations judged as stopped, each only where there is one: an
// empty list in a line reads as a value somebody computed.
func (m *credentialMissing) logAttrs() []any {
	var attrs []any
	if len(m.unset) > 0 {
		attrs = append(attrs, "unset", m.unset)
	}
	if len(m.empty) > 0 {
		attrs = append(attrs, "empty", m.empty)
	}
	if len(m.unissued) > 0 {
		attrs = append(attrs, "unissued", slices.Sorted(maps.Keys(m.unissued)))
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
	return joined(refs)
}

// joined renders a list for a sentence: "A", "A and B", "A, B and C".
func joined(items []string) string {
	if len(items) == 1 {
		return items[0]
	}
	return strings.Join(items[:len(items)-1], ", ") + " and " + items[len(items)-1]
}

// mintingStates reads the fleet's record of every integration that mints a
// variable one of missing waits for, keyed by integration — nil when nothing
// waits on one, so nothing is read, or when the record could not be read,
// which [credentialMissing.judged] takes as "cannot say".
//
// ONE READ PER INTEGRATION, and only for a child held back on one, which is a
// first boot, a seat a revision added, or a company whose provisioning a
// person has to unblock: on every other beat nothing waits and nothing is
// read. The record is the fleet's rather than this node's because the pass
// that writes it runs on whichever node holds the integration duty.
func (e *Engine) mintingStates(
	ctx context.Context, missing []*credentialMissing,
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
