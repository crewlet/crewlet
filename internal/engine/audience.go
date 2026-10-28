package engine

import (
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/agent/colleague"
	"github.com/crewlet/crewlet/internal/agent/runner"
	"github.com/crewlet/crewlet/internal/org"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/codingagent"
)

// Who a parked coding run's question is put to.
//
// The coding agent names its audience in its own words — `requester`,
// `manager`, `team`, or a colleague's name — and until this existed nothing
// resolved that label: every parked question in the company was visible to
// everybody and none of them said whose it was. So the park resolves it once,
// against the chart, into the seats the question waits on, and records
// whether those seats are what the run asked for or a FALLBACK because the
// label named nobody this chart has.
//
// THE FALLBACK IS THE SEAT'S LEAD CHAIN — its managers, nearest first, or,
// for a seat nobody manages, the leads of the units above it. A question is
// never parked waiting on nobody while somebody in the chart is answerable
// for the seat that asked; a question that reached a person only because
// nobody else could be named says so, and a reader can tell the two apart.

// audienceResolver resolves a run's audience against the live chart.
//
// THE LIVE EPOCH, read per park: the park happens when the job finishes,
// which may be hours after the launch, and the people a question is put to
// are the ones the company has when it is asked.
type audienceResolver struct{ engine *Engine }

var _ sandbox.AudienceResolver = audienceResolver{}

// ResolveAudience answers one park.
func (r audienceResolver) ResolveAudience(run sandbox.PendingRun, label string) sandbox.Audience {
	company := r.engine.Company()
	if company == nil || company.Org == nil {
		return sandbox.Audience{Fallback: true}
	}
	return resolveAudience(company.Org, r.engine.LookupSecret, r.engine.WithheldContacts(),
		run, label)
}

// The labels the coding agent's ask shim documents, beside a free name. See
// internal/sandbox/codingagent/ask.go, which is where a coding agent is told
// them.
const (
	audienceRequester = "requester"
	audienceManager   = "manager"
	audienceTeam      = "team"
)

// exactTiers are the colleague match methods a NAMED audience may resolve
// through. Only these, never a substring or a fuzzy match: a question is put
// to a person on a coding agent's say-so, and a guessed person is somebody
// else's question in their inbox. A name that resolves only approximately is
// a name the chart does not have, and the question falls back.
var exactTiers = []colleague.Method{
	colleague.MethodExactHandle, colleague.MethodExternalID,
	colleague.MethodExactRole, colleague.MethodCaseInsensitive,
}

// resolveAudience is the rule itself, over a chart value so it is exercised
// without an engine.
//
// lookup resolves the chart's `${VAR}` contact ids, which a name may be
// ([builtin.Corpus]): the node's own chain, from the engine.
//
// withheld is the party registry's withholding ([Engine.WithheldContacts]): a
// person a free name matches whose seat the identity directory withholds — a
// suspended or departed holder — is somebody an inbound message would not be
// attributed to, so a question is not put to them by name either, the same
// rule a seat's own colleague lookup takes. Nil withholds nobody.
func resolveAudience(o *org.Organization, lookup org.EnvLookup, withheld func(handle string) bool,
	run sandbox.PendingRun, label string) sandbox.Audience {
	seat := o.SeatByHandle(run.AgentHandle)
	named := namedAudience(o, lookup, withheld, seat, run, label)
	if len(named) > 0 {
		return sandbox.Audience{Handles: named}
	}
	return sandbox.Audience{Handles: leadChain(o, seat), Fallback: true}
}

// namedAudience is the seats a label names, or nothing when it names nobody
// this chart has.
func namedAudience(o *org.Organization, lookup org.EnvLookup, withheld func(handle string) bool,
	seat *org.Role, run sandbox.PendingRun, label string) []string {
	label = strings.TrimPrefix(strings.TrimSpace(label), "@")
	switch strings.ToLower(label) {
	case "":
		return nil
	case audienceRequester:
		// WHO WOKE THE TURN, recorded at the launch — the one frame that
		// saw the trigger — by its seat's handle. A seat the org no
		// longer has is nobody.
		if found := o.Role(run.Requester); found != nil {
			return []string{found.Handle()}
		}
		return nil
	case audienceManager:
		if seat == nil {
			return nil
		}
		if manager := o.Manager(seat); manager != nil {
			return []string{manager.Handle()}
		}
		return nil
	case audienceTeam:
		return teamOf(o, seat)
	}
	found := colleague.Resolve(label, builtin.Corpus(o, lookup, withheld))
	if len(found) != 1 || !slices.Contains(exactTiers, found[0].Method) {
		return nil
	}
	return []string{found[0].Seat.Handle}
}

// teamOf is the unit a seat sits in, as a question can be put to it: the
// unit's lead, and the PEOPLE who sit in it beside the seat. Agents are left
// out — a teammate agent that knew the answer is one the seat could have asked
// itself — and so is the seat that asked.
func teamOf(o *org.Organization, seat *org.Role) []string {
	if seat == nil {
		return nil
	}
	unit := o.UnitFor(seat)
	if unit == nil {
		return nil
	}
	var out []string
	add := func(r *org.Role) {
		if r == nil || r == seat || slices.Contains(out, r.Handle()) {
			return
		}
		out = append(out, r.Handle())
	}
	add(o.EffectiveLead(unit))
	for _, member := range unit.Roles {
		if member.IsHuman() {
			add(member)
		}
	}
	return out
}

// leadChain is the fallback: who is answerable for this seat, nearest first —
// its managers up to the top of the chart, or, for a seat nobody manages, the
// leads of the units that hold it from the innermost out.
func leadChain(o *org.Organization, seat *org.Role) []string {
	if seat == nil {
		return nil
	}
	var out []string
	add := func(r *org.Role) {
		if r == nil || r == seat || slices.Contains(out, r.Handle()) {
			return
		}
		out = append(out, r.Handle())
	}
	for _, manager := range o.Ancestors(seat) {
		add(manager)
	}
	if len(out) > 0 {
		return out
	}
	units := o.UnitChainFor(seat)
	for i := len(units) - 1; i >= 0; i-- {
		add(o.EffectiveLead(units[i]))
	}
	return out
}

// askBrief is the standing instruction that tells an executor run how to get
// a person's answer, and whom it can name when it asks.
//
// TWO WAYS, and which one turns on whether the run can go on without it. A
// question that only shapes what comes next is asked with the seat's own
// tools — a comment on the work item that puts it to somebody (its `ask`),
// or a message where the run was asked — and the run finishes its turn
// as usual: the answer wakes the seat's next turn like any other message, and
// nothing waits. A question the work cannot proceed without parks the run with
// the ask shim ([codingagent.AskInstruction]), and the answer resumes that same
// run. Told only of the shim, a run parked on every question, holding its box
// for one a teammate could have answered in the next turn; told of neither, it
// guessed.
//
// THE PARK HAD NO WAY IN. The box carries the ask shim and the collection
// reads what it writes, and this is the one text that tells a run the shim
// exists: a launch that did not carry it ([sandbox.LaunchRequest.Ask]) gave
// every run a park nothing could reach.
//
// THE NAMES ARE THE ONES THE PARK RESOLVES. The roster is the seat's unit
// by handle and the manager is the seat's manager by handle, so a `--to`
// that copies either resolves through the exact tiers [resolveAudience]
// accepts rather than falling back.
func askBrief(o *org.Organization, seat *org.Role) string {
	var roster []string
	manager := ""
	if o != nil && seat != nil {
		if unit := o.UnitFor(seat); unit != nil {
			for _, member := range unit.Roles {
				if member != seat {
					roster = append(roster, member.Handle())
				}
			}
		}
		if m := o.Manager(seat); m != nil {
			manager = m.Handle()
		}
	}
	return strings.Join([]string{
		"## When you need a person's answer",
		"### If you can go on without it",
		"Ask with your tools and keep working: `" + builtin.CommentOnWorkTool + "` with `ask` " +
			"naming whoever owes the answer puts the question on the work item, or send them a " +
			"message where you were asked. Then finish your turn as usual with `" +
			runner.SubmitWorkTool + "`, saying what is waiting on whom. Their answer wakes a " +
			"new turn of yours — do not wait for it here.",
		codingagent.AskInstruction(roster, manager),
	}, "\n")
}
