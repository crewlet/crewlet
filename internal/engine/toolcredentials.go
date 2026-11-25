package engine

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/provision"
	"github.com/crewlet/crewlet/internal/setup"
	"github.com/crewlet/crewlet/internal/words"
)

// A tool server is started only with what its configuration references: every
// `${VAR}` its values name resolves, or it is held back, as waiting for a pass
// that will mint it, as stopped on a person, or as failed for a value nobody
// holds; a key goes without its value only where declared optional, and
// [resolveServer] is the one place that is decided (package doc, "Tool server
// credentials").

// seatMinter is a provisioning pass that mints seats' OWN credentials: it
// creates an account per agent at its vendor and seals each one's credential
// under the variable the seat's configuration points at.
//
// seatPlan is the only declaration of that plan, each one
// [Engine.seatAccessPlan] under the names the access sync seals into
// ([seataccess.SinkName]); [Engine.mintedFor] and
// [Engine.atlassianCredentialSealed] read it, so what the sync seals and what
// a child waits for cannot drift. Held by
// TestEveryPassThatMintsASeatsCredentialDeclaresItsPlan and
// TestAVendorPlanIsReadOnlyThroughSeatPlan (which lets a teardown read the
// plan too: it removes what may have been created).
type seatMinter interface {
	setup.Pass

	// seatPlan is the plan this pass mints seats' credentials by under c, or
	// [integration.ErrNotConfigured] where c asks it to mint none: its block
	// absent, switched off, or without provisioning (accounts made by hand,
	// whose variables are the operator's to supply).
	seatPlan(c *Company) (*provision.Plan, error)
}

// seatPlan is every agent that is on at Jira or Confluence, whose Atlassian
// account the access sync creates.
func (p *atlassianPass) seatPlan(c *Company) (*provision.Plan, error) {
	return p.engine.seatAccessPlan(c, integration.KindAtlassian)
}

// seatPlan is every agent that is on at GitLab.
func (p *gitlabPass) seatPlan(c *Company) (*provision.Plan, error) {
	return p.engine.seatAccessPlan(c, integration.KindGitLab)
}

// seatPlan is every agent that is on at Mattermost.
func (p *mattermostPass) seatPlan(c *Company) (*provision.Plan, error) {
	return p.engine.seatAccessPlan(c, integration.KindMattermost)
}

// seatPlan is every agent that is on at Datadog.
func (p *datadogPass) seatPlan(c *Company) (*provision.Plan, error) {
	return p.engine.seatAccessPlan(c, integration.KindDatadog)
}

// seatPlan is every agent that is on at Cloudflare.
func (p *cloudflarePass) seatPlan(c *Company) (*provision.Plan, error) {
	return p.engine.seatAccessPlan(c, integration.KindCloudflare)
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
// A pass whose plan cannot be derived mints nothing; why is its own finding,
// on its integration's status.
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
			for _, name := range append([]string{seat.TokenVar}, seat.AlsoVars...) {
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
	// Credentials issued at use wait on an integration's step as a seal does
	// (a person installing the agent's GitHub App), so a value naming one is
	// told as waiting, never as a secret the operator forgot (package doc,
	// "Seat credentials issued at use").
	for _, issuer := range e.seatIssuers() {
		if name := issuer.variable(c, handle); name != "" {
			if out == nil {
				out = map[string]integration.Kind{}
			}
			out[name] = issuer.kind()
		}
	}
	return out
}

// seatMint is what one seat's values may be waiting for: the variables an
// integration mints or issues for the seat ([Engine.mintedFor]), and the
// first of the seat's issued tokens as this read asked for them
// ([seatToken]), which say why one that could not be issued went without.
// Nil, a shared server's, mints nothing: it acts as no seat.
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

// unissued is why the engine could not issue name this time and whether a
// wait clears it, or the zero [issueFailure] where it did not try or did.
func (m *seatMint) unissued(name string) issueFailure {
	if m == nil {
		return issueFailure{}
	}
	return m.token.failure(name)
}

// atUse is the words of the integration that issues name at use, and false
// for a variable a pass seals, whose value arrives with the seal.
func (m *seatMint) atUse(name string) (issuedWords, bool) {
	if m == nil {
		return issuedWords{}, false
	}
	t := m.token.link(name)
	if t == nil {
		return issuedWords{}, false
	}
	return t.issuer.words(), true
}

// issueFailure is a credential the engine went to issue a seat at use and
// could not: the issuer's reason, as the log and a tool server's sentence
// carry it, whether a wait clears it ([seatIssuer.refusal]) or a person's
// step does, and the issuer's words for each.
type issueFailure struct {
	said   string
	passes bool
	words  issuedWords
}

// credentialMissing is why a server was not started (a seat's child, a shared
// server, or one a harness's box was not given) or what a launch's own
// environment lacks ([Engine.runEnvironment]): references that resolved to
// nothing under keys not declared optional (package doc, "Tool server
// credentials").
type credentialMissing struct {
	// seat is the seat's handle, which an integration's findings name it by,
	// empty for a shared server; server is the configuration's name and
	// instance the bridge's name for this start, both empty for a launch.
	seat, server, instance string

	// reader is what goes without a value its sentence offers to declare
	// optional: "the server", or "the run".
	reader string

	// unset are the variables nothing sets and nothing this engine mints, and
	// empty the ones set to nothing: the operator's, with different remedies.
	unset, empty []string

	// issued are those of unset that a seat's own block holds (its bot's or
	// account's credential), which the integration's provision command can
	// seal too; nothing seals a server's own setting that way.
	issued []string

	// optional names, by the keys holding one of unset or empty, where each
	// could be declared optional, for the remedy. Only the server's own keys:
	// never a seat's block, the identity it acts as, and never a key holding a
	// variable the url needs, which would still hold the server back.
	optional map[string]string

	// minting are the variables a provisioning pass mints for this seat, by
	// the integration that mints each.
	minting map[string]integration.Kind

	// atUse are the integrations among minting's that issue their variable
	// at use rather than seal it ([seatIssuer]), with their words.
	atUse map[integration.Kind]issuedWords

	// unissued are the variables the engine could not issue this seat at use,
	// each with why ([seatToken.unissued]): a failure retried on the restart
	// schedule, which a wait clears where the vendor did not answer and only a
	// person's step where it refused. Never a token.
	unissued map[string]issueFailure

	// stopped is, for each minting integration that cannot until a person
	// acts, what its last pass recorded ([mintStopped]): the rest of the
	// sentence after "will not mint". Empty where nothing judged or stops it.
	stopped map[integration.Kind]string

	// unsure is, for each minting integration whose last pass's findings
	// cannot say whether it holds this seat up (unread, or cut where they
	// may have named it, [unsaid]), why: held, never read as waiting.
	unsure map[integration.Kind]unsaidOf
}

// unsaidOf is why an integration cannot say ([unsaid]), and whether only
// because this node is still reading its findings, which is no failure.
type unsaidOf struct {
	why     string
	reading bool
}

// serverValues are what one start of a tool server reads, every reference
// resolved: the environment of a stdio server or the headers of an http one,
// and its url.
type serverValues struct {
	values map[string]string
	url    string
}

// resolveServer is the rule every start of a tool server passes through
// (package doc, "Tool server credentials"): its own env (stdio) or headers
// (http) with block, a seat's mcp_env, over them, every reference resolved, or
// why it may not start. seat and mint ([seatMint]) are empty for a shared
// server; env is the seat's resolution ([Engine.seatResolver]).
//
// MERGED BEFORE RESOLVING, by the key the server reads
// ([config.MCPServer.ValueKey], so `authorization` replaces `Authorization`):
// a seat's unresolved value never falls back to the template's, which would
// start the seat's child as whoever the template names. Each layer is read in
// key order, so a report and its log line name the same variables at every
// pass, two spellings of one header in one layer included.
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
	// The company's OAuth client secret is read at every renewal of the
	// server's sign-in, so one that resolves to nothing holds the server back
	// as its url would. Never optional, and never sent to the server.
	if server.OAuth != nil {
		gap.pin(env.Missing(server.OAuth.ClientSecret))
	}
	where := "mcp_servers." + server.Name + ".optional_keys"
	// The integration's own values ride the seat's block but are no seat's
	// credential, so its provision command seals none of them.
	org := map[string]bool{}
	for _, k := range server.OrgBindings() {
		org[server.ValueKey(k.Key)] = true
	}
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
		gap.hold(l.key, missing, server.OptionalKey(l.key), offer, l.seat && !org[id])
	}
	m := gap.missing(seat, "the server")
	if m == nil {
		return out, nil
	}
	m.server, m.instance = server.Name, instance
	return serverValues{}, m
}

// refGap gathers what a set of values lacks, key by key: the references that
// resolve to nothing, for [credentialGap] to sort into its stories, and what
// each remedy may offer for them. Every set of values the engine starts
// something with goes through it, so whether a key may be left out, and which
// remedy names it, is one rule.
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
// the key holds anything back. optional is whether the key is declared
// optional; where is where it could be, or "" where the remedy never offers
// it; issued is whether a seat's own block wrote the value.
//
// A DECLARED KEY STILL HOLDS BACK WHAT A PASS MINTS FOR THE SEAT: that value
// is coming, and left out the child would act as nobody until the seal.
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
		// TRIED AND REFUSED IS NOT WAITING: a token the vendor would not
		// issue is a failure with its reason, retried, never "waiting for
		// the integration", which reads as a step somebody has yet to take.
		if failure := mint.unissued(name); failure.said != "" {
			if m.unissued == nil {
				m.unissued = map[string]issueFailure{}
			}
			m.unissued[name] = failure
			continue
		}
		if kind, ok := mint.kind(name); ok {
			if m.minting == nil {
				m.minting = map[string]integration.Kind{}
			}
			m.minting[name] = kind
			if said, atUse := mint.atUse(name); atUse {
				if m.atUse == nil {
					m.atUse = map[integration.Kind]issuedWords{}
				}
				m.atUse[kind] = said
			}
			continue
		}
		// SET TO NOTHING IS NOT UNSET: "export it" would send the operator
		// to do what they already did.
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
	return len(m.unset) == 0 && len(m.empty) == 0 && len(m.stopped) == 0 && len(m.unissued) == 0 &&
		len(m.unsure) == 0
}

// unsureAlone reports whether all that holds the child is integrations that
// cannot say ([credentialMissing.unsure]): no fault of the operator's, and no
// wait either, so a person sees it as a warning.
func (m *credentialMissing) unsureAlone() bool {
	return len(m.unsure) > 0 && len(m.unset) == 0 && len(m.empty) == 0 && len(m.stopped) == 0 &&
		len(m.unissued) == 0
}

// reading reports whether all that holds the child is findings this node is
// still reading ([unsaidOf]): no failure, so said at info, and the next
// reading judges it.
func (m *credentialMissing) reading() bool {
	if !m.unsureAlone() {
		return false
	}
	for _, u := range m.unsure {
		if !u.reading {
			return false
		}
	}
	return true
}

// arrival is what a value that is only waiting arrives with: the seal, for
// a credential a pass seals into the store, or each issuer's own step, for
// one the engine issues at use ([issuedWords.arrival]).
func (m *credentialMissing) arrival() string {
	const sealed = "the seal reaches this node"
	var steps []words.Said
	for _, kind := range m.mintingKinds() {
		said, atUse := m.atUse[kind]
		if !atUse {
			return sealed
		}
		steps = append(steps, words.Said(said.arrival))
	}
	if len(steps) == 0 {
		return sealed
	}
	return words.And(steps)
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
// A FRESH VALUE, never m changed in place: the status record's writer judges a
// pass's outcome on every reading while the next pass may be filing another.
// Nil states is "the record could not be read", and m keeps its pass's
// judgement rather than clearing it: a record not answering is no evidence a
// person acted. For that one reading, a wait the last reading judged stopped
// may read as waiting again, or the reverse.
func (m *credentialMissing) judged(states map[integration.Kind]integration.State) *credentialMissing {
	if m == nil || states == nil || len(m.minting) == 0 {
		return m
	}
	out := *m
	out.stopped, out.unsure = nil, nil
	for _, kind := range m.mintingKinds() {
		state, found := states[kind]
		if !found {
			// No pass has run: the first boot, where the claim comes first.
			continue
		}
		why, stopped := mintStopped(state, m.seat)
		if !stopped && unsaid(state) {
			// CANNOT SAY for this one: findings a person owes may name the
			// seat and are not in hand. A stop m judged stands; otherwise
			// the child is held on it, saying why, never read as waiting.
			if why, stopped = m.stopped[kind]; !stopped {
				if out.unsure == nil {
					out.unsure = map[integration.Kind]unsaidOf{}
				}
				out.unsure[kind] = unsaidWhy(state)
				continue
			}
		}
		if stopped {
			if out.stopped == nil {
				out.stopped = map[integration.Kind]string{}
			}
			out.stopped[kind] = why
		}
	}
	return &out
}

// unsaid reports whether what state's findings say of a seat is not all in
// hand: a person owes findings naming something, and they were unread or the
// record cut what they name ([integration.FindingsRecord.NamesOmitted]). Only
// a stop such findings show is then certain.
func unsaid(state integration.State) bool {
	rec := state.FindingsRecord
	return rec.NeedsFindingsFor() && (state.FindingsUnread || rec.NamesOmitted > 0)
}

// unsaidWhy is why an [unsaid] state cannot say, the end of "whether the
// <kind> integration will mint ${…} cannot be read: ". A cut first, which no
// read of the record undoes; then a read in flight, which is no failure.
func unsaidWhy(state integration.State) unsaidOf {
	switch n := state.FindingsRecord.NamesOmitted; {
	case n > 0:
		noun := "findings"
		if n == 1 {
			noun = "finding"
		}
		return unsaidOf{why: fmt.Sprintf("its last pass found more than its findings record keeps, and the "+
			"record cut %d %s that may name this agent", n, noun)}
	case state.FindingsReading:
		return unsaidOf{why: "its last pass's findings are not read yet; the next reading has them", reading: true}
	}
	return unsaidOf{why: "its last pass's findings could not be read beside its status"}
}

// mintStopped reports whether an integration's last recorded pass says it will
// not mint seat's credentials until a person acts, and if so the end of "the
// <kind> integration will not mint ${…}". Read off the pass's own conclusion,
// never re-derived: a second answer beside the pass's would drift.
//
// Stopped by a disconnect; by a finding about this seat a person owes
// ([integration.Actor]), where one the engine or vendor owes means work on it
// is in hand whatever else is reported; or by a report a person owes at a
// phase before identities ([integration.Phase]), which reached no seat. A pass
// that minted this seat and reported such a finding about something else
// (Datadog's webhook token) reads as stopped until the seal reaches this node,
// seconds later.
func mintStopped(state integration.State, seat string) (string, bool) {
	if state.TearingDown() {
		return "it is being disconnected", true
	}
	inHand := false
	for _, f := range state.Findings {
		if f.Subject != seat && !slices.Contains(f.Subjects, seat) {
			continue
		}
		if phase, _ := f.Kind.Verdict(); phase == integration.PhaseReady {
			// AN ADVISORY holds no seat up: the integration works, with a note.
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
// value, only variables and keys. Failures first, because the status record
// bounds the sentence ([coord.MaxMCPErrorBytes]) and what somebody has to act on is
// the part that must survive the cut.
func (m *credentialMissing) reason() string {
	var parts []string
	if len(m.unset) > 0 {
		// Only a seat's bot or account can also be sealed by its
		// integration's provision command.
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
		failure := m.unissued[name]
		// Refused is a person's to fix, which asking again does not change.
		then := failure.words.retried
		if !failure.passes {
			then = failure.words.refused
		}
		parts = append(parts, fmt.Sprintf("${%s} could not be issued: %s; %s", name, failure.said, then))
	}
	if remedy := m.optionalRemedy(); remedy != "" {
		parts = append(parts, remedy)
	}
	var waits []words.Said
	for _, kind := range m.mintingKinds() {
		var names []string
		for name, k := range m.minting {
			if k == kind {
				names = append(names, name)
			}
		}
		slices.Sort(names)
		verb := "mint"
		if said, atUse := m.atUse[kind]; atUse {
			verb = said.verb
		}
		if why, stopped := m.stopped[kind]; stopped {
			parts = append(parts, fmt.Sprintf("the %s integration will not %s %s %s",
				kind, verb, varRefs(names), why))
			continue
		}
		if u, unsure := m.unsure[kind]; unsure {
			parts = append(parts, fmt.Sprintf("whether the %s integration will %s %s cannot be read: %s",
				kind, verb, varRefs(names), u.why))
			continue
		}
		waits = append(waits, words.Said(fmt.Sprintf("the %s integration to %s %s", kind, verb, varRefs(names))))
	}
	if len(waits) > 0 {
		parts = append(parts, "waiting for "+words.And(waits))
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
	var lists []words.Said
	for _, where := range slices.Sorted(maps.Keys(byWhere)) {
		keys := byWhere[where]
		slices.Sort(keys)
		lists = append(lists, words.Said(asWritten(keys)+" under "+where))
	}
	keys := slices.Collect(maps.Keys(m.optional))
	return fmt.Sprintf("or, if %s can go without %s, list %s",
		m.reader, plural(keys, "it", "them"), words.And(lists))
}

// Error is the sentence the status record carries and the log writes: why the
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
	if len(m.unsure) > 0 {
		attrs = append(attrs, "unsure", slices.Sorted(maps.Keys(m.unsure)))
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
func varRefs(vars []string) string {
	refs := make([]words.Said, len(vars))
	for i, name := range vars {
		refs[i] = words.Said("${" + name + "}")
	}
	return words.And(refs)
}

// asWritten renders keys and paths as an operator writes them in the file, unquoted
// beside the references [varRefs] writes: "A", "A and B", "A, B and C".
func asWritten(items []string) string {
	said := make([]words.Said, len(items))
	for i, item := range items {
		said[i] = words.Said(item)
	}
	return words.And(said)
}

// mintingStates reads the fleet's record of every integration that mints a
// variable one of missing waits for, keyed by integration: nil when nothing
// waits on one, or when the record could not be read, which
// [credentialMissing.judged] takes as "cannot say". One status read by its key
// at the term per integration, only while a child is held back on one, and
// findings only where a person owes one, from [mintFindings], never read on
// this path. The record is the fleet's because the pass that writes it runs
// on whichever node holds the integration duty.
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
		if !found {
			continue
		}
		// NO PERSON OWES A FINDING NAMING ANYTHING: none can stop or hold
		// this seat's mint, so the status alone judges it ([mintStopped]).
		if state.FindingsRecord.NeedsFindingsFor() {
			state = e.mintFindings.attach(state, store.LoadFindings)
		}
		states[kind] = state
	}
	return states
}
