package integration

import (
	"slices"
	"time"
)

// ExpiryWarning is how far ahead of a credential's published expiry a pass
// reports it as [FindingCredentialExpiring].
//
// TWO WEEKLY REVIEWS. A token is rotated by a person — a new one minted at the
// vendor and sealed into this deployment's secret store — and the person who
// reads the Integrations screen does it on their own schedule, usually once a
// week. Fourteen days means the warning is on screen for at least two of
// those looks before the credential lapses, so one missed week is not an
// outage. Shorter and a holiday is one; longer and the note sits on a working
// integration for so long that people stop reading it. A constant rather than
// a knob because no company has a reason to be warned later than it can act,
// and a settled surface is re-read on the settled interval, so the finding
// appears within one interval of the window opening whatever the value.
const ExpiryWarning = 14 * 24 * time.Hour

// ExpiresSoon reports whether a credential expiring at `at` is inside
// [ExpiryWarning] of now. A zero `at` is a credential with no published expiry
// and is never reported.
//
// An expiry already in the past is inside the window too. A pass that got far
// enough to read the date authenticated with the credential, so a vendor still
// accepting it past its date is a vendor about to refuse it — the same thing to
// tell a person, and the one moment staying silent would be worst.
func ExpiresSoon(at, now time.Time) bool {
	return !at.IsZero() && at.Sub(now) <= ExpiryWarning
}

// ToolSurface is one engine surface behind a tool, and the name a person
// knows it by.
type ToolSurface struct {
	// Key is the surface's key on the `integrations` answer — a [Kind], or
	// `forge` for the relay, which is a delivery path rather than a surface
	// anything reconciles.
	Key string
	// Name is how a sentence about this surface names it.
	Name string
}

// Tool is one thing a company connects, and the surfaces the engine reaches it
// over.
//
// # Why the engine groups surfaces at all
//
// A company thinks in tools: it connected Atlassian, not "the atlassian,
// confluence, jira and forge surfaces". The dashboard drew one card per tool
// and derived that card's state itself, with its own copy of the phase order
// and its own rules for which surface's word wins — a second classifier, in a
// second language, over the same rows [Classify] had already judged. It
// drifted the way the vendor classifiers [Classify] replaced had drifted: a
// disconnect drew neutral on the card and amber on the surface it named.
// So the grouping is declared here, once, and the dashboard's copy is held
// against it (`INTEGRATION_TOOLS`, `TestTheDashboardGroupsSurfacesAsTheEngineDoes`).
type Tool struct {
	Key      string
	Surfaces []ToolSurface
}

// Tools is every tool this build serves, in the catalogue's order.
//
// Every surface in [Kinds] belongs to exactly one tool, and so does the Forge
// relay; `TestEverySurfaceBelongsToOneTool` holds both.
var Tools = []Tool{
	{Key: "slack", Surfaces: []ToolSurface{{string(KindSlack), "Slack"}}},
	{Key: "mattermost", Surfaces: []ToolSurface{{string(KindMattermost), "Mattermost"}}},
	{Key: "atlassian", Surfaces: []ToolSurface{
		// THE ORGANIZATION FIRST: it is where an agent's account is
		// created, and the two products are what that account works in.
		{string(KindAtlassian), "Organization"},
		{string(KindConfluence), "Confluence"},
		{string(KindJira), "Jira"},
		{"forge", "Forge relay"},
	}},
	{Key: "github", Surfaces: []ToolSurface{{string(KindGitHub), "GitHub"}}},
	{Key: "gitlab", Surfaces: []ToolSurface{{string(KindGitLab), "GitLab"}}},
	{Key: "datadog", Surfaces: []ToolSurface{{string(KindDatadog), "Datadog"}}},
}

// ToolState is a tool's standing in the one question a person asks of it: does
// it work, and if not, is anybody waiting on me?
type ToolState string

const (
	// ToolConnected is a tool whose every configured surface works.
	ToolConnected ToolState = "connected"

	// ToolAttention is a tool a PERSON has to act on: a phase whose actor
	// waits on a person, a ready surface nothing can deliver to, or a
	// credential about to expire. The Settings sidebar counts these, so
	// it is only ever something a person can do.
	ToolAttention ToolState = "attention"

	// ToolNotConnected is a configured tool that does not work YET and that
	// nobody has to act on: the engine or the vendor is mid-flight (setting
	// up agents, applying a grant, the window before the first report, a
	// disconnect being carried out), or this node cannot read the status at
	// all. A card in this state offers nothing, because nothing a person
	// does moves it.
	ToolNotConnected ToolState = "not_connected"

	// ToolNotInUse is a tool this company does not use: no block at all, or
	// every block it has switched off. The label tells the two apart.
	ToolNotInUse ToolState = "not_in_use"
)

// ToolStates is every state, from "somebody has to act" to "nothing is
// configured".
var ToolStates = []ToolState{ToolAttention, ToolNotConnected, ToolConnected, ToolNotInUse}

// Valid reports whether s is a state this build knows.
func (s ToolState) Valid() bool { return slices.Contains(ToolStates, s) }

// rank orders the states for picking a tool's surface: what a person owes
// first, then what is not working yet, then what works, then what is off.
func (s ToolState) rank() int {
	switch s {
	case ToolAttention:
		return 3
	case ToolNotConnected:
		return 2
	case ToolConnected:
		return 1
	default:
		return 0
	}
}

// The labels a roll-up adds for situations that are not phases. A phase's
// own label comes from [Phase.Label].
const (
	labelActionNeeded      = "Action needed"
	labelExpiring          = "Credential expiring"
	labelPaused            = "Paused"
	labelConnecting        = "Connecting"
	labelStatusUnavailable = "Status unavailable"
	labelNotInUse          = "Not in use"
)

// SurfaceStatus is everything the roll-up reads about one CONFIGURED surface —
// one row of the `integrations` answer.
//
// THE THREE INGRESS FACTS ARE THREE-VALUED, like the row's own: nil is "this
// node cannot say" and is never read as a fault.
type SurfaceStatus struct {
	// Key is the surface's row key.
	Key string
	// Enabled is the block's own switch, true where the block has none.
	Enabled bool
	// Known is whether this node could read the fleet's status rows at
	// all. False is a coordination store that did not answer — not a
	// surface nobody has checked.
	Known bool
	// State is the fleet's row for this surface, nil where there is none.
	State *State
	// Converges is whether a reconcile pass converges this surface in this
	// build; nil when this node cannot say.
	Converges *bool
	// SecretUsable, Routes and EndpointCurrent are the row's ingress facts.
	SecretUsable    *bool
	Routes          *bool
	EndpointCurrent *bool
}

// ToolRollup is one tool's answer: its state, the words for it, and why.
type ToolRollup struct {
	Key string `json:"key"`
	// Surfaces are the tool's surface keys, whether or not configured, so
	// a reader groups rows exactly as the roll-up did.
	Surfaces []string  `json:"surfaces"`
	State    ToolState `json:"state"`
	// Label is the state in a reader's words: a phase's own label, or one
	// of the roll-up's for a situation that is not a phase.
	Label string `json:"label"`
	// Reason is one sentence saying why, about [ToolRollup.Surface]. Empty
	// for a tool that works or is not in use.
	Reason string `json:"reason"`
	// Surface is the key of the surface the state was taken from, empty for
	// a tool with nothing configured.
	Surface string `json:"surface,omitempty"`
}

// verdict is one surface's contribution to its tool.
type verdict struct {
	state         ToolState
	label, reason string
	surface       string
	// phase orders two verdicts of the same state; empty for a verdict
	// that is not a phase, which ranks as ready.
	phase   Phase
	tearing bool
}

// Rollup is one tool's state, from the rows of its configured surfaces.
//
// # The rules, in the order they apply
//
//  1. NOTHING CONFIGURED is not in use. A tool nobody set up has nothing to
//     report, and a fault list of every tool a company does not use is not a
//     catalogue.
//  2. A TEARDOWN WINS. A tool being taken away is not one anybody should be
//     sent to fix, and showing it connected invites a second Disconnect.
//  3. Otherwise each configured surface is judged on its own, and the tool
//     takes the surface whose state comes first in [ToolStates] — so a
//     surface a person owes something on outranks one the engine is still
//     bringing up, which is the question the state answers. Within one
//     state the least ready phase ([Phases]) wins, then the catalogue order.
//
// One surface is judged by its reported phase, through [Report.Outcome] — who
// has to act, not what failed: settled is connected, blocked is attention,
// waiting is not connected. Over a READY phase two more facts are read,
// because the loop says nothing about them: a delivery path that cannot work
// (a secret that did not resolve, no parser routing it, a registration at an
// address that moved) needs action, and so does a credential about to expire.
// A surface with no report is paused where its block is off, judged on those
// same ingress facts where no pass converges it (Slack), unavailable where
// this node could not read the status, and connecting otherwise.
func Rollup(tool Tool, present []SurfaceStatus) ToolRollup {
	out := ToolRollup{Key: tool.Key, Surfaces: make([]string, 0, len(tool.Surfaces))}
	names := map[string]string{}
	order := map[string]int{}
	for i, s := range tool.Surfaces {
		out.Surfaces = append(out.Surfaces, s.Key)
		names[s.Key] = s.Name
		order[s.Key] = i
	}
	verdicts := make([]verdict, 0, len(present))
	for _, s := range present {
		name := names[s.Key]
		if name == "" {
			name = s.Key
		}
		verdicts = append(verdicts, judge(s, name))
	}
	if len(verdicts) == 0 {
		out.State, out.Label = ToolNotInUse, labelNotInUse
		return out
	}
	// The catalogue's order first, so every later tie is broken the same
	// way on every read.
	slices.SortStableFunc(verdicts, func(a, b verdict) int {
		return order[a.surface] - order[b.surface]
	})
	best := verdicts[0]
	for _, v := range verdicts[1:] {
		if worse(v, best) {
			best = v
		}
	}
	out.State, out.Label, out.Reason, out.Surface = best.state, best.label, best.reason, best.surface
	return out
}

// worse reports whether a outranks b as the verdict a tool reports.
func worse(a, b verdict) bool {
	switch {
	case a.tearing != b.tearing:
		return a.tearing
	case a.state.rank() != b.state.rank():
		return a.state.rank() > b.state.rank()
	default:
		return distance(a.phase) < distance(b.phase)
	}
}

// distance is how far from working a phase is, lowest first.
//
// DOUBLED so a phase this build does not know sits BETWEEN the worst phase it
// knows and ready: never presented as the ready one, and never masking a phase
// this build knows is broken. The empty phase is a verdict that is not a phase
// at all, and ranks as ready.
func distance(p Phase) int {
	if p == "" {
		p = PhaseReady
	}
	if at := slices.Index(Phases, p); at >= 0 {
		return at * 2
	}
	return len(Phases)*2 - 3
}

// judge is one configured surface's verdict.
func judge(s SurfaceStatus, name string) verdict {
	v := verdict{surface: s.Key}
	if s.Known && s.State != nil && s.State.TearingDown() {
		r := s.State.Reported()
		v.state, v.label, v.reason, v.phase, v.tearing =
			ToolNotConnected, r.Phase.Label(), r.Detail, r.Phase, true
		return v
	}
	if s.Known && s.State != nil && s.State.Observed() {
		r := s.State.Reported()
		v.phase, v.label, v.reason = r.Phase, r.Phase.Label(), r.Detail
		switch r.Outcome() {
		case OutcomeSettled:
			v.state = ToolConnected
		case OutcomeBlocked:
			v.state = ToolAttention
		default:
			v.state = ToolNotConnected
		}
		if r.Phase == PhaseReady {
			// CONNECTED IS A CLAIM THE LOOP CANNOT MAKE ALONE. It says
			// nothing about deliveries, so a ready surface whose every
			// delivery is refused needs a person, in the words this
			// engine uses for exactly that.
			if fault := ingressFault(s, name); fault != "" {
				v.state, v.label, v.reason = ToolAttention, labelActionNeeded, fault
				return v
			}
		}
		if v.state != ToolAttention {
			// AN EXPIRING CREDENTIAL IS A PERSON'S DEADLINE whatever the
			// engine is doing meanwhile: it lapses on its date whether or
			// not a pass is setting up agents, and only a person can
			// replace it.
			if f, ok := expiring(s.State.Findings); ok {
				v.state, v.label = ToolAttention, labelExpiring
				v.reason = f.Detail
				if v.reason == "" {
					v.reason = f.Kind.sentence(f.Subject)
				}
			}
		}
		return v
	}
	switch {
	case !s.Enabled:
		v.state, v.label = ToolNotInUse, labelPaused
		v.reason = name + " is switched off in the company configuration"
	case s.Converges != nil && !*s.Converges:
		// A SURFACE NO PASS CONVERGES NEVER REPORTS, so "the loop has not
		// got to it yet" would be a permanent claim about it. Judged from
		// what CAN be seen instead: the same ingress facts as above.
		if fault := ingressFault(s, name); fault != "" {
			v.state, v.label, v.reason = ToolAttention, labelActionNeeded, fault
		} else {
			v.state, v.label = ToolConnected, PhaseReady.Label()
		}
	case !s.Known:
		// NOT "connecting": nothing here knows whether the loop has
		// reported, and a coordination store that did not answer is not
		// evidence about anybody's integration.
		v.state, v.label = ToolNotConnected, labelStatusUnavailable
		v.reason = "this node could not read the fleet's integration status, so it " +
			"cannot say how " + name + " is doing"
	default:
		v.state, v.label = ToolNotConnected, labelConnecting
		v.reason = name + " is configured and the reconcile loop has not reported on it yet"
	}
	return v
}

// ingressFault is the sentence for a delivery path that cannot work, or "".
func ingressFault(s SurfaceStatus, name string) string {
	switch {
	case s.SecretUsable != nil && !*s.SecretUsable:
		return name + "'s delivery secret did not resolve on this node, so every " +
			"delivery is refused"
	case s.Routes != nil && !*s.Routes:
		return name + " deliveries are verified and stored, and no parser turns " +
			"them into work for a seat"
	case s.EndpointCurrent != nil && !*s.EndpointCurrent:
		return name + " is registered at an address this deployment no longer " +
			"answers on, so its deliveries go nowhere"
	}
	return ""
}

// expiring is the first credential-expiry finding in a pass's list.
func expiring(findings []Finding) (Finding, bool) {
	for _, f := range findings {
		if f.Kind == FindingCredentialExpiring {
			return f, true
		}
	}
	return Finding{}, false
}
