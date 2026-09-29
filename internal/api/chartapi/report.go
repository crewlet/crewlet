package chartapi

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/notify"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/schedule"
)

// THE CONTINUOUS REPORT: one evaluation over the chart this node is running
// and the settings epoch it has applied.
//
// # The gap it closes, which the split created
//
// A company used to be one document, and one validation read all of it. Now
// it is two things with two lifetimes: a chart that changes when somebody
// hires or moves, and a settings revision that changes when somebody edits
// the company's configuration. Each is valid on its own terms — the chart
// domain refuses a record that breaks its rules, and `crewlet validate`
// refuses a document that breaks the settings' — and NEITHER validates the
// PAIR.
//
// That pair is where the expensive failures live. A revision that removes a
// provider key is a perfectly valid revision; every seat whose model chain
// names it now has no model at all, and nothing says so until one of them
// takes a turn and fails. A seat pointed at a worker template somebody
// deleted narrows its delegate grant to nothing. A seat with a sandbox gate
// on a company with no sandbox backend cannot run code and reports it as a
// tool error.
//
// # Why it is CONTINUOUS and not a validation
//
// Nothing here can be refused at a write, and that is the point rather than a
// limitation: the two halves are written by different people at different
// times, so every one of these findings is reachable through two writes that
// were each correct when they were made. Refusing the second would refuse an
// operator's edit over a seat they have never heard of.
//
// So it is a REPORT, evaluated over what is running, and one evaluation feeds
// every surface that renders it — [Evaluate] here, `/chart/check` in this
// package and `/health` in internal/api. A gauge, a screen and a probe that
// each ran their own would eventually disagree about whether something is
// wrong, which is the failure mode the state log's alarm table exists to
// prevent and this borrows wholesale.

// Held answers which seats somebody in the identity directory is bound to,
// each named by its IDENTITY — the handle it was created under
// ([org.Role.Origin], ADR-0020) — and read in ONE snapshot.
//
// # One read for the whole answer
//
// It used to be asked per seat, a transaction each: an evaluation over forty
// human seats was forty reads at forty instants, so a report could combine
// bindings that never coexisted, and every `/health` a load balancer polled
// paid for all of them. One reading is what every surface renders from.
//
// # Consumer-defined, and THREE-VALUED in two places
//
// An ERROR is this node failing to read a directory it holds, and a NIL Held
// is a node with no directory to ask at all — one that started with no active
// company, and so opened no native runtime, or a surface stood up with no
// directory behind it. Neither is "nobody holds this seat", and collapsing
// either into it is how every human seat in the company gets reported unheld
// at once: the report then leaves [KindSeatUnheld] UNDECIDED rather than
// answering it — the arm off under a nil, every human seat counted in
// [Report.Unchecked] under an error — and the seat listing's `unheld` filter
// refuses rather than filtering.
//
// IT TAKES THE CALLER'S CONTEXT, because it is a read of the identity
// estate's rows: a request that is gone, or a node shutting down, stops it.
type Held func(ctx context.Context) (map[string]bool, error)

// Severity orders a finding by what it costs.
type Severity string

const (
	// SeverityError is something that does not work: a seat with no model,
	// a gate with no backend behind it. It is running and it is broken.
	SeverityError Severity = "error"

	// SeverityWarning is something that works and is not what anybody
	// meant — a reference that resolves to nothing, a narrowing that
	// narrows to the empty set.
	SeverityWarning Severity = "warning"
)

// severityRank orders the two for [Report.Worst]. Higher is worse.
var severityRank = map[Severity]int{SeverityWarning: 1, SeverityError: 2}

// FindingKind is what class of thing is wrong.
//
// A CLOSED SET WITH A COUNT PER KIND on the report, because "three findings"
// tells an operator watching a gauge nothing and "three seats reference a
// provider that is gone" tells them what changed.
type FindingKind string

const (
	// KindProviderUnknown is a seat whose model chain names a provider key
	// the applied settings do not declare. The seat has NO MODEL: provider
	// resolution falls through the chain and finds nothing.
	KindProviderUnknown FindingKind = "provider_unknown"

	// KindWorkerUnknown is a seat whose `workers:` narrowing names a
	// template the settings do not declare. The grant is a security
	// boundary — a template widens nothing — so a name that resolves to
	// nothing narrows this seat to fewer workers than anybody intended.
	KindWorkerUnknown FindingKind = "worker_unknown"

	// KindSandboxUnconfigured is a seat with its code gate open on a
	// company with no sandbox backend. Nothing it is asked to build will
	// run, and the failure arrives as a tool error inside a turn.
	KindSandboxUnconfigured FindingKind = "sandbox_unconfigured"

	// KindReferenceDangling is a reference inside the chart that resolves
	// to nothing — a `manages:` entry, a unit's lead, a seat's unit. The
	// chart applies it (an applier that refused would stop that object's
	// every later change on every node), so this is where it surfaces.
	KindReferenceDangling FindingKind = "reference_dangling"

	// KindSeatUnheld is a human seat NOBODY IN THE DIRECTORY IS BOUND TO.
	// The seat exists in the chart, a turn can route work to it, and no
	// person can sign in and act as it — so every authority rule asking
	// "do you lead this" falls through, and the work waits for somebody
	// who cannot arrive.
	//
	// IT READS THE DIRECTORY, which is what it could not do until one
	// existed: this used to read a seat's declared CONTACT block, so a
	// company that manages its people elsewhere saw every human seat
	// reported here. That was honest and useless in the same breath, and
	// it is a different question — see [KindSeatUnreachable].
	KindSeatUnheld FindingKind = "seat_unheld"

	// KindSeatUnreachable is a human seat with no contact identity, so
	// nothing addressed to it reaches anybody on the chat surface this
	// company runs.
	//
	// ITS OWN KIND rather than an arm of [KindSeatUnheld], because the two
	// are independent facts with different remedies and a seat can have
	// either without the other: somebody who signs in but gets no
	// notifications, and somebody who is messaged constantly and cannot
	// open the dashboard as themselves. Folded together, whichever remedy
	// a person tried first would appear not to work.
	//
	// THIS IS THE ONLY PLACE IT IS SAID. Validation admits the seat — a
	// person who works only through the dashboard has no chat account to
	// declare, and refusing the seat refused them — so a warning here is
	// the one signal an operator gets that nobody can be @-mentioned there,
	// and a warning rather than an error because the state is legitimate.
	KindSeatUnreachable FindingKind = "seat_unreachable"

	// KindIdentityShared is a seat whose ADDRESS or CONTACT IDENTITY another
	// seat resolves to as well. An address and a contact identity are what
	// an inbound payload is routed by, and each routes to ONE seat: the
	// party registry keeps the first seat declaring an address and the
	// last declaring a contact id, so every other seat claiming it stops
	// receiving what is addressed there — and an action by that account is
	// attributed to a colleague — with nothing anywhere saying so.
	//
	// HERE BECAUSE NOTHING ELSE CAN SAY IT. A chart write arbitrates on one
	// object's subject and cannot refuse a rule across two of them, and
	// the address is SEALED: two rows carry two different references, so
	// only a node that resolves both can see they are one mailbox. An
	// error, because one of the seats is unreachable today.
	KindIdentityShared FindingKind = "identity_shared"

	// KindScheduleUnrunnable is an ENABLED schedule nothing can run: a
	// unit's `each` with no direct agent member, a unit's `lead` whose
	// effective lead is a person or nobody, or a schedule on a human seat.
	// The scheduler skips it on every tick, so it never fires.
	//
	// HERE BECAUSE NOTHING ELSE CAN SAY IT. A company file is refused for
	// one, but a chart write cannot be: the schedule is its object's own
	// content, while what makes it runnable — a member's kind, a lead
	// inherited from an ancestor — is written on other objects' subjects,
	// so two writes each correct when they were made strand it with nobody
	// to refuse. Read through the scheduler's own runner resolution
	// ([schedule.StrandedIn]), so what this names is exactly what the tick
	// skips. An error, because the work it describes is not happening.
	KindScheduleUnrunnable FindingKind = "schedule_unrunnable"
)

// FindingKinds is every kind, for the walks and for a surface rendering a
// legend.
var FindingKinds = []FindingKind{
	KindProviderUnknown, KindWorkerUnknown, KindSandboxUnconfigured,
	KindReferenceDangling, KindSeatUnheld, KindSeatUnreachable,
	KindIdentityShared, KindScheduleUnrunnable,
}

// Resolve answers a `${VAR}` name through this node's own resolution chain —
// the secret store in front of the environment — reporting whether anything
// answered.
//
// NIL SKIPS THE SHARED-IDENTITY ARM rather than answering it, for [Held]'s
// reason: an address the chart sealed is a reference on every row, so a report
// that could not resolve them would compare references, which never collide,
// and call a company clean that is routing two people's mail to one of them.
type Resolve = org.EnvLookup

// Finding is one thing wrong with the company as it is actually running.
type Finding struct {
	Kind     FindingKind `json:"kind"`
	Severity Severity    `json:"severity"`

	// Object is the seat handle or unit key this is about.
	Object string `json:"object"`

	// Names is what that object referenced and did not find. Empty where
	// the finding is about the object itself rather than a reference.
	Names string `json:"names,omitempty"`

	// Detail says what is wrong and Remedy what to do, in that order and
	// separately — internal/integration's Finding states why, and this
	// borrows the split rather than restating the reasoning.
	Detail string `json:"detail"`
	Remedy string `json:"remedy,omitempty"`
}

// Report is one evaluation of the running company.
type Report struct {
	// Findings are every one, ordered: worst first, then by kind, then by
	// object. STABLE, because two consecutive evaluations of an unchanged
	// company that rendered in different orders would read as a company
	// that keeps changing.
	Findings []Finding `json:"findings"`

	// Counts is how many of each kind, so a caller watching one number can
	// say which class grew.
	Counts map[FindingKind]int `json:"counts,omitempty"`

	// Seats and Units are what was evaluated, which is what makes an empty
	// report readable: no findings over 40 seats is a statement, and no
	// findings over 0 seats is a node that has not applied a chart yet.
	Seats int `json:"seats"`
	Units int `json:"units"`

	// Unchecked is how many human seats this evaluation could not ask the
	// identity directory about, so [KindSeatUnheld] was left UNDECIDED for
	// them rather than answered — see [Held]. Absent at zero. It is the
	// same rule as [Report.Evaluated] one arm down: a seat whose holder
	// could not be read is neither a finding nor a clean bill, and a count
	// of findings that silently excluded it would read as the second.
	Unchecked int `json:"unchecked,omitempty"`

	// Evaluated is false when this node could not evaluate at all — it
	// holds no chart view, or no settings epoch. ABSENT EVIDENCE IS NOT A
	// CLEAN BILL: a report of zero findings from a node that read nothing
	// is the single most misleading answer this surface could give.
	Evaluated bool `json:"evaluated"`
}

// Worst is the highest severity present, or empty when there is nothing
// wrong.
func (r Report) Worst() Severity {
	var worst Severity
	for _, f := range r.Findings {
		if severityRank[f.Severity] > severityRank[worst] {
			worst = f.Severity
		}
	}
	return worst
}

// Evaluate reports every way the chart and the settings disagree.
//
// PURE OVER VALUES, for the reason internal/textindex gives for the same
// shape: a rule that can only be exercised through a running fleet is a rule
// nobody re-measures. It takes the two halves a node already holds and
// returns what is wrong with the pair.
//
// A NIL HALF IS "COULD NOT EVALUATE", never "nothing is wrong". See
// [Report.Evaluated]. The context is handed to held and to nothing else: the
// directory is the one input that is not a value. Resolve is how this node
// resolves a `${VAR}`, and nil skips the one arm that needs it ([Resolve]).
func Evaluate(ctx context.Context, o *org.Organization, settings *config.Company,
	held Held, resolve Resolve) Report {
	if o == nil || settings == nil {
		return Report{Findings: []Finding{}}
	}
	out := Report{Evaluated: true, Counts: map[FindingKind]int{}}
	for range o.AllUnits() {
		out.Units++
	}
	holding := holdingOf(ctx, o, held)
	for role := range o.AllRoles() {
		out.Seats++
		out.Findings = append(out.Findings, seatFindings(role, settings)...)
		finding, unchecked := holding.finding(role)
		if finding != nil {
			out.Findings = append(out.Findings, *finding)
		}
		if unchecked {
			out.Unchecked++
		}
	}
	for _, ref := range o.DanglingRefs() {
		out.Findings = append(out.Findings, danglingFinding(ref))
	}
	for _, stranded := range schedule.StrandedIn(o) {
		out.Findings = append(out.Findings, strandedFinding(stranded))
	}
	if resolve != nil {
		out.Findings = append(out.Findings, sharedIdentityFindings(o, resolve)...)
	}
	for _, f := range out.Findings {
		out.Counts[f.Kind]++
	}
	slices.SortFunc(out.Findings, func(a, b Finding) int {
		if a.Severity != b.Severity {
			return severityRank[b.Severity] - severityRank[a.Severity]
		}
		if a.Kind != b.Kind {
			return strings.Compare(string(a.Kind), string(b.Kind))
		}
		if a.Object != b.Object {
			return strings.Compare(a.Object, b.Object)
		}
		return strings.Compare(a.Names, b.Names)
	})
	if out.Findings == nil {
		out.Findings = []Finding{}
	}
	return out
}

// seatFindings is everything wrong with one seat against these settings.
func seatFindings(role *org.Role, settings *config.Company) []Finding {
	handle := role.Handle()
	var out []Finding
	for _, key := range missingProviders(role, settings) {
		out = append(out, Finding{
			Kind: KindProviderUnknown, Severity: SeverityError,
			Object: handle, Names: key,
			Detail: fmt.Sprintf("%s runs on provider %q and the applied "+
				"settings declare no such provider, so this seat resolves to "+
				"no model at all and every turn it takes fails", handle, key),
			Remedy: "declare the provider under providers.llm, or point the " +
				"seat's chain at one that exists",
		})
	}
	for _, name := range missingWorkers(role, settings) {
		out = append(out, Finding{
			Kind: KindWorkerUnknown, Severity: SeverityWarning,
			Object: handle, Names: name,
			Detail: fmt.Sprintf("%s may delegate to worker %q and the applied "+
				"settings declare no such template. The grant is a filter "+
				"rather than a definition, so this narrows the seat to fewer "+
				"workers than the list suggests", handle, name),
			Remedy: "declare the template under workers, or drop the name " +
				"from this seat's workers list",
		})
	}
	if sandboxUnbacked(role, settings) {
		out = append(out, Finding{
			Kind: KindSandboxUnconfigured, Severity: SeverityError,
			Object: handle,
			Detail: fmt.Sprintf("%s has its code gate open and this company "+
				"configures no sandbox backend, so a coding run cannot start "+
				"and the seat learns that as a tool error inside a turn",
				handle),
			Remedy: "configure providers.sandbox, or close the seat's " +
				"sandbox gate",
		})
	}
	if unreachable(role) {
		out = append(out, Finding{
			Kind: KindSeatUnreachable, Severity: SeverityWarning,
			Object: handle,
			Detail: fmt.Sprintf("%s is a human seat with no contact identity, "+
				"so nothing addressed to it reaches anybody", handle),
			Remedy: "give the seat a contact identity on the chat surface " +
				"this company runs — or leave it, for a person who works " +
				"only through the dashboard: agents then hand them work by " +
				"assigning it in the tracker rather than mentioning them",
		})
	}
	return out
}

// holding is ONE reading of the directory, for one evaluation.
type holding struct {
	// asked is false where there was no directory to ask — a nil [Held] —
	// or no human seat to ask about, and the arm is then off.
	asked bool
	seats map[string]bool
	err   error
}

// holdingOf reads the directory once for an evaluation, and only when the
// chart holds a human seat to ask about: an all-agent company pays nothing.
//
// A NIL held asks nothing — the whole arm is off on a node with no directory,
// which is one fact about the node rather than a count of seats. An ERROR is
// logged once and every human seat is then UNCHECKED: a directory this node
// holds and cannot read is a fault with a cause, and the count is what keeps
// the report from reading as clean while it lasts.
func holdingOf(ctx context.Context, o *org.Organization, held Held) holding {
	if held == nil {
		return holding{}
	}
	for role := range o.AllRoles() {
		if !role.IsHuman() {
			continue
		}
		seats, err := held(ctx)
		if err != nil {
			log.WarnContext(ctx, "chart_report_holding_unreadable",
				"error", err)
		}
		return holding{asked: true, seats: seats, err: err}
	}
	return holding{}
}

// finding is the one arm of a seat's findings that reads the identity
// directory: a [KindSeatUnheld] finding, or nil, and whether the seat was left
// UNCHECKED because the read failed.
func (h holding) finding(role *org.Role) (*Finding, bool) {
	if !h.asked || !role.IsHuman() {
		return nil, false
	}
	if h.err != nil {
		return nil, true
	}
	// ASKED BY THE SEAT'S IDENTITY — the handle it was created under — which
	// is what a binding names (ADR-0020): asked by the handle it answers to
	// now, a renamed seat whose holder is bound read as unheld.
	if h.seats[role.Origin()] {
		return nil, false
	}
	handle := role.Handle()
	return &Finding{
		Kind: KindSeatUnheld, Severity: SeverityWarning,
		Object: handle,
		Detail: fmt.Sprintf("%s is a human seat nobody in the directory "+
			"is bound to, so no person can sign in and act as it — work "+
			"routed here waits for somebody who cannot arrive", handle),
		Remedy: "invite the person who holds this seat, or bind an " +
			"existing person to it",
	}, false
}

// missingProviders is every provider key this seat names that the settings do
// not declare, deduplicated and in a stable order.
//
// EVERY CHAIN, not just the default one. A per-phase chain is what a seat
// falls back FROM, so a review phase pointed at a provider that is gone fails
// only when a turn reaches review — which is later, rarer and harder to
// connect to the edit that caused it.
func missingProviders(role *org.Role, settings *config.Company) []string {
	chains := []org.ProviderKeys{
		role.LLM, role.LLMReview, role.LLMSubagent, role.LLMAuxiliary,
		role.LLMJudge, role.LLMSandbox,
	}
	var out []string
	for _, chain := range chains {
		for _, key := range chain {
			if key == "" {
				continue
			}
			if _, declared := settings.Providers.LLM[key]; declared {
				continue
			}
			if !slices.Contains(out, key) {
				out = append(out, key)
			}
		}
	}
	slices.Sort(out)
	return out
}

// missingWorkers is every worker template this seat names that the settings do
// not declare.
//
// AN EMPTY LIST NAMES NOTHING AND IS NOT A FINDING: empty means every
// template, which is the default and cannot dangle.
func missingWorkers(role *org.Role, settings *config.Company) []string {
	var out []string
	for _, name := range role.Workers {
		if name == "" {
			continue
		}
		if _, declared := settings.Workers[name]; declared {
			continue
		}
		if !slices.Contains(out, name) {
			out = append(out, name)
		}
	}
	slices.Sort(out)
	return out
}

// sandboxUnbacked reports a seat whose code gate is open on a company with no
// backend behind it.
//
// `self` IS EXEMPT and is the one cell with no providers.sandbox behind it:
// it is the executor's own agent-mode run, which happens in the engine's
// process rather than in a box somebody has to configure.
func sandboxUnbacked(role *org.Role, settings *config.Company) bool {
	if role.Sandbox == nil || !role.Sandbox.Enabled {
		return false
	}
	if role.Sandbox.RunIn == "self" {
		return false
	}
	return settings.Providers.Sandbox == nil
}

// unreachable reports a human seat nobody can be reached at.
func unreachable(role *org.Role) bool {
	return role.IsHuman() && (role.Contact == nil || role.Contact.IsEmpty())
}

// sharedIdentityFindings is every seat an address or a contact identity it
// declares does not route to, because another seat resolves to it too.
//
// ASKED OF THE ROUTING ITSELF rather than of a rule written beside it. The
// address arm builds the party registry every node routes an address through
// ([notify.NewRegistry]) and asks it where each seat's own resolved address
// goes — which is exactly the question, plus tags and the fold included: two
// seats sharing a mailbox through `notif+<handle>@` each get their own, and a
// tag spelling somebody else's handle does not. The contact arm follows
// [notify.Registry.ReconcileHumanContacts]' own rule, the last human seat in
// chart order keeping an identity, and names every other one — once per
// identity, however many transports read it (Jira and Confluence share one
// account id).
func sharedIdentityFindings(o *org.Organization, resolve Resolve) []Finding {
	var out []Finding
	registry := notify.NewRegistry(o, resolve)
	for role := range o.AllRoles() {
		handle := role.Handle()
		address := role.ResolvedEmail(resolve)
		if handle == "" || address == "" {
			continue
		}
		party, routed := registry.ByEmail(address)
		if !routed || party.Handle == handle {
			continue
		}
		out = append(out, Finding{
			Kind: KindIdentityShared, Severity: SeverityError,
			Object: handle, Names: "email",
			Detail: fmt.Sprintf("%s's address resolves to the one %s declares, "+
				"and an address routes to one seat: a vendor payload from it — a "+
				"comment, a push, a review — is attributed to %s, and nothing "+
				"addressed there reaches %s", handle, party.Handle, party.Handle,
				handle),
			Remedy: "give the seat an address of its own — or, for seats that " +
				"share one mailbox, plus-address it with the seat's own handle " +
				"(notif+<handle>@…), which routes by the tag",
		})
	}

	type contact struct {
		transport org.Transport
		id        string
	}
	holders := map[contact][]string{}
	for role := range o.AllRoles() {
		if !role.IsHuman() || role.Contact == nil || role.Handle() == "" {
			continue
		}
		for _, id := range role.Contact.ResolvedIdentities(resolve) {
			key := contact{id.Transport, id.ExternalID}
			if !slices.Contains(holders[key], role.Handle()) {
				holders[key] = append(holders[key], role.Handle())
			}
		}
	}
	type loss struct{ seat, keeper, id string }
	lost := map[loss][]string{}
	for key, seats := range holders {
		if len(seats) < 2 {
			continue
		}
		keeper := seats[len(seats)-1]
		for _, seat := range seats[:len(seats)-1] {
			at := loss{seat, keeper, key.id}
			lost[at] = append(lost[at], string(key.transport))
		}
	}
	for at, transports := range lost {
		slices.Sort(transports)
		surfaces := strings.Join(transports, ", ")
		out = append(out, Finding{
			Kind: KindIdentityShared, Severity: SeverityError,
			Object: at.seat, Names: surfaces,
			Detail: fmt.Sprintf("%s shares its %s identity with %s, and a contact "+
				"identity routes to one seat: a message or a mention from that "+
				"account is attributed to %s, and %s is not reachable there",
				at.seat, surfaces, at.keeper, at.keeper, at.seat),
			Remedy: "give each seat its own account on that surface",
		})
	}
	return out
}

// danglingFinding renders one reference that resolves to nothing.
func danglingFinding(ref org.DanglingRef) Finding {
	var detail string
	switch ref.Kind {
	case org.RefUnit:
		detail = fmt.Sprintf("%s says it sits in unit %q and this company has "+
			"no such unit, so the seat is placed at the org root instead — "+
			"above every team, which is rarely what anybody meant",
			ref.From, ref.To)
	case org.RefLead:
		detail = fmt.Sprintf("unit %s is led by %q and this company has no "+
			"such seat, so the unit's lead is inherited from an ancestor",
			ref.From, ref.To)
	default:
		detail = fmt.Sprintf("%s manages %q and this company has neither a "+
			"seat nor a unit by that name, so the entry manages nobody",
			ref.From, ref.To)
	}
	return Finding{
		Kind: KindReferenceDangling, Severity: SeverityWarning,
		Object: ref.From, Names: ref.To, Detail: detail,
		Remedy: "correct the reference, or create what it names — a retired " +
			"address goes on resolving, so this one names nothing at all",
	}
}

// strandedFinding renders one schedule nothing can run.
func strandedFinding(s schedule.Stranded) Finding {
	var detail, remedy string
	switch s.Reason {
	case schedule.StrandedHumanSeat:
		detail = fmt.Sprintf("%s is a human seat, and a person runs no turns, "+
			"so its schedule %q never fires", s.ScopeName, s.Schedule.Name)
		remedy = "move the schedule to an agent seat, or make this seat an " +
			"agent's"
	case schedule.StrandedNoAgentMember:
		detail = fmt.Sprintf("unit %s's schedule %q fans out to each direct "+
			"agent member, and the unit has none — a person and a child "+
			"unit's seats are never runners — so it never fires",
			s.ScopeName, s.Schedule.Name)
		remedy = "place an agent seat directly in the unit, or target the " +
			"schedule at the unit's lead"
	case schedule.StrandedLeadHuman:
		detail = fmt.Sprintf("unit %s's schedule %q is run by the unit's lead, "+
			"and its effective lead is a human seat, who runs no turns — so "+
			"it never fires", s.ScopeName, s.Schedule.Name)
		remedy = "have an agent seat lead the unit, or target the schedule " +
			"at each of its agent members"
	default:
		detail = fmt.Sprintf("unit %s's schedule %q is run by the unit's lead, "+
			"and nothing leads the unit, itself or through an ancestor — so "+
			"it never fires", s.ScopeName, s.Schedule.Name)
		remedy = "give the unit or an ancestor a lead, or target the " +
			"schedule at each of its agent members"
	}
	return Finding{
		Kind: KindScheduleUnrunnable, Severity: SeverityError,
		Object: s.ScopeName, Names: s.Schedule.Name,
		Detail: detail, Remedy: remedy + " — or disable it while it waits",
	}
}

// getCheck answers the report over this node's own view.
//
// THE SAME FUNCTION /health CALLS, which is the whole point: a probe, a gauge
// and a screen that each evaluated for themselves would eventually disagree
// about whether something is wrong, and the disagreement is discovered by
// somebody who trusted the wrong one.
func (s *Service) getCheck(w http.ResponseWriter, r *http.Request) {
	if key := refuseUnknownParams(r); key != "" {
		httpjson.FailWith(w, http.StatusBadRequest, httpjson.CodeBadParams,
			map[string]string{"detail": "this route does not read " + key})
		return
	}
	got := s.report(r.Context())
	httpjson.Write(w, http.StatusOK, map[string]any{
		"report": got, "worst": string(got.Worst()),
	})
}
