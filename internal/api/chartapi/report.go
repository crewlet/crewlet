package chartapi

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/org"
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

	// KindSeatUnheld is a human seat nobody holds — no contact identity, so
	// nothing can reach the person the seat is for and every notification
	// addressed to it goes nowhere.
	//
	// UNTIL THERE IS AN IDENTITY DIRECTORY this reads a seat's declared
	// contact block, which is the only evidence this build has. A company
	// that manages its people elsewhere will see every human seat here
	// until that directory exists, and that is honest rather than useful:
	// the engine genuinely cannot reach them.
	KindSeatUnheld FindingKind = "seat_unheld"
)

// FindingKinds is every kind, for the walks and for a surface rendering a
// legend.
var FindingKinds = []FindingKind{
	KindProviderUnknown, KindWorkerUnknown, KindSandboxUnconfigured,
	KindReferenceDangling, KindSeatUnheld,
}

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
// [Report.Evaluated].
func Evaluate(o *org.Organization, settings *config.Company) Report {
	if o == nil || settings == nil {
		return Report{Findings: []Finding{}}
	}
	out := Report{Evaluated: true, Counts: map[FindingKind]int{}}
	for range o.AllUnits() {
		out.Units++
	}
	for role := range o.AllRoles() {
		out.Seats++
		out.Findings = append(out.Findings, seatFindings(role, settings)...)
	}
	for _, ref := range o.DanglingRefs() {
		out.Findings = append(out.Findings, danglingFinding(ref))
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
	if unheld(role) {
		out = append(out, Finding{
			Kind: KindSeatUnheld, Severity: SeverityWarning,
			Object: handle,
			Detail: fmt.Sprintf("%s is a human seat with no contact identity, "+
				"so nothing addressed to it reaches anybody", handle),
			Remedy: "give the seat a contact identity on the chat surface " +
				"this company runs",
		})
	}
	return out
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

// unheld reports a human seat nobody can be reached at.
func unheld(role *org.Role) bool {
	return role.IsHuman() && (role.Contact == nil || role.Contact.IsEmpty())
}

// danglingFinding renders one reference that resolves to nothing.
func danglingFinding(ref org.DanglingRef) Finding {
	object, detail := ref.From, ""
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
		Object: object, Names: ref.To, Detail: detail,
		Remedy: "correct the reference, or create what it names — a retired " +
			"address goes on resolving, so this one names nothing at all",
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
	got := s.report()
	httpjson.Write(w, http.StatusOK, map[string]any{
		"report": got, "worst": string(got.Worst()),
	})
}
