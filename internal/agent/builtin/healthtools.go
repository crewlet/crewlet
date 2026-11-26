package builtin

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/host"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tools/argval"
)

// The health tools: fleet_health, each live node as its heartbeat states it
// with the alarms its own table has firing, and code_status, the Gitlet nodes'
// caches and hottest repositories or one repository on each: ONE list
// ([HealthTools]) a seat's registry and the operator catalogue both take.
//
// An administrator reads them, and a seat only with its role's health grant
// (`role.health_tools`, authz.SeatGrantHealth); [Withheld] hides them
// elsewhere. They set no thresholds: what is wrong is the alarm table's to say
// (ADR-0015), and they report the nodes' own marks unjudged. The grant never
// widens a seat's repository reads (ADR-0088, [codeStatus.hottest]), and in a
// turn somebody woke both read only for somebody of the company who may act
// ([fleetHealth.forRequester]), never relaying the view to a stranger.

// The health tools' names: each is its authority row's action.
const (
	FleetHealthTool = string(authz.ActionFleetHealth)
	CodeStatusTool  = string(authz.ActionCodeStatus)
)

// HealthToolNames are the two, in the order a catalogue lists them.
func HealthToolNames() []string { return []string{FleetHealthTool, CodeStatusTool} }

// LeaseLister is the coordination store's live leases (internal/coord's
// ListLive), which fleet_health reads the nodes' presence from, and its clock
// (Now), which it reads how long each has left on ([coord.TimeLeft]).
type LeaseLister interface {
	ListLive(ctx context.Context, class coord.Class) ([]coord.Lease, error)
	Now(ctx context.Context, class coord.Class) (time.Time, error)
}

// StatusAsker is the fleet's Gitlet nodes and what Gitlet on each says of
// itself: internal/engine's roster off the presence leases and its scatter on
// crewlet.code.status.
type StatusAsker interface {
	// GitletNodes is every live Gitlet node by id, in no order promised: the keys
	// code_status pages by, once it has sorted them ([codeStatus.ask]).
	GitletNodes(ctx context.Context) ([]string, error)
	// CodeStatus asks the Gitlet nodes ask names, and no other, and waits for
	// those alone.
	CodeStatus(ctx context.Context, ask CodeStatusAsk) (CodeStatusReport, error)
}

// CodeStatusAsk is one status question: which Gitlet nodes answer it, and what
// each answer carries beside the node's cache.
type CodeStatusAsk struct {
	// Nodes is the Gitlet nodes asked, by id; the report covers these alone. A
	// reply from another node (an older build answers whatever the question
	// names) is dropped, and the ask ends once every one of them has answered.
	Nodes []string
	// Every says Nodes is every Gitlet node GitletNodes listed, so the question
	// names none and stays a few bytes however large the fleet, where naming
	// the roster would send it to every node.
	Every bool
	// Repo is the id of a repository each node reports beside itself; ""
	// asks about none.
	Repo string
	// Hot asks each node for its hot list (host.StatusAsk.Hot).
	Hot bool
}

// CodeStatusReport is the Gitlet nodes' answers to one status question as the
// asking node heard them, with which of them answered: the coverage every
// fanned answer carries (internal/eventfan).
type CodeStatusReport struct {
	Statuses []host.CodeStatus
	Coverage eventfan.Coverage
}

// HealthDeps are what the health tools read through. The zero value serves
// neither.
type HealthDeps struct {
	// Leases is the fleet's live leases. Nil omits fleet_health.
	Leases LeaseLister

	// Requesters composes who a seat's turn acts for, and Chart is the chart
	// the authority table decides them by ([fleetHealth.forRequester]).
	// REQUIRED beside Leases (nil omits fleet_health), so a surface that left
	// the ceiling out serves no view of the deployment to whoever messages a
	// granted seat. code_status takes its own from Code.
	Requesters Requesters
	Chart      authz.Chart

	// Self is this node's id, which fleet_health's answer marks.
	Self string

	// Status asks the Gitlet nodes' status. Nil omits code_status, as do Code
	// deps that serve no code tool ([CodeDeps.serves]).
	Status StatusAsker

	// Code is the code tools' own deps: code_status resolves a repository name
	// and judges a seat's grant exactly as they do, for whoever woke the turn;
	// Code.Actor names the caller.
	Code CodeDeps
}

// HealthTools are the health tools a seat and an operator share: ONE list, so
// a schema and a refusal's words are written once. Every one reads. Each is
// served only with the requester ceiling beside it, fleet_health with
// Requesters and Chart and code_status on the code tools' terms
// ([CodeDeps.serves]), never as a tool reading the deployment for whoever
// messaged a seat.
func HealthTools(deps HealthDeps) []tools.Callable {
	var out []tools.Callable
	if deps.Leases != nil && deps.Requesters != nil && deps.Chart != nil {
		out = append(out, &fleetHealth{deps: deps})
	}
	if deps.Status != nil && deps.Code.serves() {
		out = append(out, &codeStatus{deps: deps})
	}
	return out
}

// healthCandidates are [HealthTools] as a registry's candidates.
func healthCandidates(deps HealthDeps) []candidate {
	var out []candidate
	for _, tool := range HealthTools(deps) {
		out = append(out, candidate{tool, true})
	}
	return out
}

// healthAnnotations classifies one health tool; false for a name that is not
// one. Proven idempotent reads: a turn that only looked at the fleet delivered
// nothing (the delivery check), a worker may be handed them (the guard), and
// the operator catalogue audits no call to one.
func healthAnnotations(name string) (tools.Annotations, bool) {
	if slices.Contains(HealthToolNames(), name) {
		return tools.Annotations{ReadOnly: mcp.Yes, Idempotent: mcp.Yes}, true
	}
	return tools.Annotations{}, false
}

// Withheld is the builtins of snapshot the authority table refuses the party on
// ctx before any object is named ([Admits]), sorted: the tools that party's
// surfaces must not offer it, so what a seat is offered and what it is
// admitted to cannot differ (a model shown a refused tool reaches for it round
// after round). ONLY A REFUSAL WITHHOLDS: on [ErrUndecidable] the tool is
// offered and the call decides again, so silence never hides a tool a seat
// holds. An MCP or agent-to-agent tool has no row to ask. Asked outside any
// turn, as [Admits] says: whoever wakes a turn bounds its calls, never the offer.
func Withheld(ctx context.Context, authorize Authorizer, snapshot tools.Snapshot) []string {
	var out []string
	for _, e := range snapshot.Entries() {
		if e.Origin != tools.OriginBuiltin {
			continue
		}
		if errors.Is(Admits(ctx, authorize, e.Name()), ErrRefused) {
			out = append(out, e.Name())
		}
	}
	slices.Sort(out)
	return out
}

// ---- fleet_health ------------------------------------------------------- //

// The fleet_health pages, keyset by node id. A node's row is some 300 bytes as
// sent, a few dozen more per alarm firing, so a couple of hundred nodes would
// pass ToolAnswerBytes whole, and spend every alarm-woken turn's context on
// nodes nobody asked about: the answer is paged, narrowed by `node` or
// `firing`, and each page cut whole-row as sent ([foreignRead.fit]). 25 by
// default, the code tools' listing page and most fleets whole; 100 at most,
// near half of one answer.
const (
	defaultFleetHealthPage = 25
	maxFleetHealthPage     = 100
)

type fleetHealth struct{ deps HealthDeps }

var _ tools.SeatCallable = (*fleetHealth)(nil)

func (t *fleetHealth) Name() string { return FleetHealthTool }

func (t *fleetHealth) Description() string {
	return "The fleet as each live node's own heartbeat states it: its roles, how its broker takes " +
		"part, the turns it is running, whether it is draining, what it concluded about its config, " +
		"the seats it holds — and every alarm its own table has firing, with when it was raised. " +
		"What is wrong is the alarms' to say: this reports them as the nodes did and judges nothing " +
		"else. A node with `alarms_evaluated: false` has no table it has evaluated (a node holding " +
		"no data, or one still booting), which is not the same as nothing firing. Nodes by id, a page " +
		"at a time: pass `next` back as `cursor` until no `next` comes back. `firing` lists only the " +
		"nodes with an alarm firing — what an alarm's wake asks — and `node` reads one node."
}

func (t *fleetHealth) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"firing": map[string]any{
				"type": "boolean",
				"description": "Only the nodes whose own table has an alarm firing. A node with no table " +
					"it evaluated cannot say, so it is counted (`unevaluated`) rather than listed.",
			},
			"node": map[string]any{
				"type":        "string",
				"description": "One node's id, to read that node alone.",
			},
			"cursor": map[string]any{
				"type": "string",
				"description": "The `next` of the previous page, handed back unchanged; leave it out " +
					"for the first page.",
			},
			"limit": map[string]any{
				"type": "integer",
				"description": fmt.Sprintf("How many nodes a page lists, 1 to %d; %d by default. A page "+
					"that would outgrow one answer holds fewer, and says where the next begins.",
					maxFleetHealthPage, defaultFleetHealthPage),
			},
		},
	}
}

// fleetHealthAnswer is fleet_health's answer: one page of the live nodes, by
// id.
type fleetHealthAnswer struct {
	ThisNode string       `json:"this_node"`
	Nodes    []nodeHealth `json:"nodes"`
	// Next is the id the next page begins after, handed back as `cursor`;
	// absent on the last page.
	Next string `json:"next,omitempty"`
	// Unevaluated is, where the call asked for `firing`, how many live nodes
	// have no table they evaluated and so were left out: counted, so silence
	// never reads as health. Absent otherwise, where every node is listed.
	Unevaluated *int `json:"unevaluated,omitempty"`
}

// fleetHealthQuery is what one fleet_health call asks for.
type fleetHealthQuery struct {
	node   string
	firing bool
	cursor string
	limit  int
}

// query is the call's arguments, or the refusal of ones it cannot read —
// refused before anything is read.
func (t *fleetHealth) query(args map[string]any) (fleetHealthQuery, *tools.Result) {
	q := fleetHealthQuery{node: strings.TrimSpace(argString(args, "node")),
		cursor: strings.TrimSpace(argString(args, "cursor")),
		limit:  argInt(args, "limit", defaultFleetHealthPage)}
	if raw, present := args["firing"]; present && raw != nil {
		// By [argval.Bool], the surface check's own reader.
		firing, ok := argval.Bool(raw)
		if !ok {
			// A FILTER READ AS FALSE would list every node to a caller that
			// asked for the ones firing.
			r := failed(fmt.Sprintf("%s: `firing` is true or false.", t.Name()))
			return q, &r
		}
		q.firing = firing
	}
	switch {
	case q.limit < 1 || q.limit > maxFleetHealthPage:
		r := failed(fmt.Sprintf("%s: `limit` is 1 to %d.", t.Name(), maxFleetHealthPage))
		return q, &r
	case q.node != "" && (q.firing || q.cursor != "" || args["limit"] != nil):
		r := failed(fmt.Sprintf("%s: `node` reads that one node, so it takes no `firing`, `cursor` or "+
			"`limit`.", t.Name()))
		return q, &r
	}
	return q, nil
}

// nodeHealth is one live node as its presence lease and heartbeat state it.
type nodeHealth struct {
	ID     string            `json:"id"`
	Roles  []string          `json:"roles"`
	Broker string            `json:"broker"`
	Labels map[string]string `json:"labels,omitempty"`
	// ExpiresIn is the seconds its presence lease has left on the store's
	// clock ([coord.TimeLeft]), never this node's: a node that stopped
	// renewing reads as running out. Absent where the store's clock could not
	// be read: not saying, never running out.
	ExpiresIn *int `json:"expires_in,omitempty"`
	// Seats is how many seats it says it holds, as of its last renewal.
	Seats int `json:"seats"`
	// Status is what its heartbeat carried, absent where it carried none:
	// a node that is not saying, never an idle one.
	Status *nodeStatus `json:"status,omitempty"`
}

// nodeStatus is the live half of a node's heartbeat (coord.NodeStatus).
type nodeStatus struct {
	InFlight  int       `json:"in_flight"`
	Draining  bool      `json:"draining"`
	StartedAt time.Time `json:"started_at,omitzero"`
	Posture   string    `json:"posture,omitempty"`
	// AlarmsEvaluated is whether the node has an alarm table it evaluated,
	// and Alarms the kinds it found firing, the longest-standing first —
	// present, empty or not, exactly where it did.
	AlarmsEvaluated bool         `json:"alarms_evaluated"`
	Alarms          *[]alarmMark `json:"alarms,omitempty"`
}

// alarmMark is one alarm firing on a node, as its heartbeat marks it.
type alarmMark struct {
	Kind  string    `json:"kind"`
	Since time.Time `json:"since,omitzero"`
}

// evaluated reports whether the node's heartbeat says it evaluated an alarm
// table: false for a heartbeat that carried no status, as for one that said
// it had none.
func (n nodeHealth) evaluated() bool { return n.Status != nil && n.Status.AlarmsEvaluated }

// firing reports whether the node's own table has an alarm firing — the
// table's verdict, as the node marked it.
func (n nodeHealth) firing() bool {
	return n.evaluated() && n.Status.Alarms != nil && len(*n.Status.Alarms) > 0
}

func (t *fleetHealth) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

func (t *fleetHealth) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	if refusal := t.forRequester(ctx, turn); refusal != nil {
		return *refusal, nil
	}
	q, refusal := t.query(args)
	if refusal != nil {
		return *refusal, nil
	}
	leases, err := t.deps.Leases.ListLive(ctx, coord.ClassNode)
	if err != nil {
		return refusedBy(tools.RefusalUnavailable, err, FleetHealthTool+" could not read the fleet's "+
			"presence from the coordination store just now, so it says nothing rather than show a "+
			"fleet with nodes missing. Try again shortly."), nil
	}
	// DEGRADES: an unreadable store clock costs the rows their countdown,
	// never the rows, whose presence the listing already established.
	left, err := coord.TimeLeft(ctx, t.deps.Leases, coord.ClassNode)
	if err != nil {
		log.WarnContext(ctx, "fleet_health_store_clock_unavailable", "error", err)
	}
	nodes := make([]nodeHealth, 0, len(leases))
	for _, lease := range leases {
		profile, ok := placement.FromLease(lease)
		if !ok {
			continue
		}
		nodes = append(nodes, nodeHealthOf(profile, lease, left))
	}
	slices.SortFunc(nodes, func(a, b nodeHealth) int { return cmp.Compare(a.ID, b.ID) })
	answer := fleetHealthAnswer{ThisNode: t.deps.Self}
	read := newForeignRead(turn, "")
	if q.node != "" {
		i := slices.IndexFunc(nodes, func(n nodeHealth) bool { return n.ID == q.node })
		if i < 0 {
			return refused(tools.RefusalNotFound, fmt.Sprintf("%s: no live node is named %q — a node "+
				"that has stopped renewing its presence is not live. Leave `node` out to list the live "+
				"nodes.", t.Name(), clip(q.node))), nil
		}
		answer.Nodes = nodes[i : i+1]
		return read.page(ctx, t.Name(), answer, "That node's own row is past one answer.")
	}
	if q.firing {
		unevaluated := 0
		nodes = slices.DeleteFunc(nodes, func(n nodeHealth) bool {
			if !n.evaluated() {
				unevaluated++
				return true
			}
			return !n.firing()
		})
		answer.Unevaluated = &unevaluated
	}
	// Keyset by id: the next page begins after the last row kept, whether
	// `limit` or the answer's size cut it.
	rest := nodes[sortedAfter(nodes, func(n nodeHealth) string { return n.ID }, q.cursor):]
	page := func(k int) any {
		a := answer
		a.Nodes = rest[:k]
		if k > 0 && k < len(rest) {
			a.Next = rest[k-1].ID
		}
		return a
	}
	k, rendered, err := read.fit(min(q.limit, len(rest)), page)
	switch {
	case err != nil:
		return labelFault(ctx, t.Name(), err), nil
	case k == 0 && len(rest) > 0:
		return failed(fmt.Sprintf("%s: node %q alone is past one answer — its labels are that long — so "+
			"no page holds it whole. Pass %q as `cursor` to read the nodes after it.", t.Name(),
			clip(rest[0].ID), rest[0].ID)), nil
	}
	return compactAnswer(rendered, "Ask for fewer with `limit`, for only the nodes with an alarm "+
		"firing with `firing`, or for one node with `node`.")
}

// sortedAfter is where in rows, sorted by the id idOf reads, the first row
// after cursor stands: 0 for no cursor.
func sortedAfter[T any](rows []T, idOf func(T) string, cursor string) int {
	if cursor == "" {
		return 0
	}
	i, found := slices.BinarySearchFunc(rows, cursor, func(r T, id string) int {
		return cmp.Compare(idOf(r), id)
	})
	if found {
		i++
	}
	return i
}

// forRequester is nil where this call may read the deployment's health for
// whoever its turn acts for, and otherwise its refusal, before anything is read.
// The grant opens an administrator's view, which a seat must not read out to
// anybody on a chat surface it listens on, so the requester ([ceilingOf]) is
// decided again ([companyRequester]) on [authz.ActionFleetHealthRead]. That
// row, never the health row: everybody of the company who may act holds it,
// where a requester holds no admin and a colleague seat no grant of the
// asker's, so the health row would refuse every turn anybody woke. code_status
// decides on Gitlet's read verb for the same reason.
func (t *fleetHealth) forRequester(ctx context.Context, turn *turnctx.Turn) *tools.Result {
	return companyRequester(ctx, turn, t.deps.Requesters, t.deps.Chart, t.Name(), requesterTerms{
		action: authz.ActionFleetHealthRead, done: "the deployment's health is read", nothing: "Nothing was read.",
	})
}

// requesterTerms are what [companyRequester] decides and says: the row a
// requester must hold, what the tool does only for somebody of the company,
// and what was not done.
type requesterTerms struct {
	action        authz.Action
	done, nothing string
}

// companyRequester is nil where a seat's call may act for whoever its turn acts
// for ([ceilingOf]), decided by the authority table on terms.action, and
// otherwise its refusal. A stranger or a person who may not act in the company
// now is refused; a colleague seat, a person of the company and a turn nobody
// woke are not. A requester this node could not compose or decide is
// undecidable, never a turn nobody woke. A person's or a token's own call is
// no deputy. A credential acting as no seat is refused too
// ([worksForACredential]): it is of the company only for the apps its grants
// name (ADR-0109), and what these tools read is no app, so neither a
// `[worklet]` nor an `[admin]` token's turn reads them.
func companyRequester(ctx context.Context, turn *turnctx.Turn, requesters Requesters, chart authz.Chart,
	tool string, terms requesterTerms) *tools.Result {
	// Who calls, as the gate decided: the context's principal, else the
	// turn's seat (turnctx.WithPrincipal).
	p, _ := iam.From(turnctx.WithPrincipal(ctx, turn))
	if p.Kind != iam.KindSeat {
		return nil
	}
	ceiling, err := ceilingOf(ctx, turn, p.Seat, requesters)
	if err != nil {
		r := refusedBy(tools.RefusalUnavailable, fmt.Errorf("%w: %w", ErrUndecidable, err),
			fmt.Sprintf("%s could not establish who this turn's work is for, and %s only for somebody "+
				"of the company who may act. %s Try again shortly.", tool, terms.done, terms.nothing))
		return &r
	}
	if ceiling.Kind == authz.CeilingNone {
		return nil
	}
	if worksForACredential(ceiling) {
		r := credentialRefused(tool, terms)
		return &r
	}
	dec := authz.DecideWithin(ctx, p, ceiling, terms.action, authz.Object{}, chart, time.Now().UTC())
	switch {
	case dec.Unknown():
		r := refusedBy(tools.RefusalUnavailable, fmt.Errorf("%w: %w", ErrUndecidable, dec.Err),
			fmt.Sprintf("%s could not decide right now whether whoever this turn is working for may act in "+
				"the company. %s Try again shortly.", tool, terms.nothing))
		return &r
	case !dec.Allowed && dec.Reason == authz.ReasonCeiling:
		r := authorityRefusal(tool, DecisionError(terms.action, dec))
		r.Output = companyCeilingRefused(tool, ceiling.Kind, terms.done)
		return &r
	case !dec.Allowed:
		r := authorityRefusal(tool, DecisionError(terms.action, dec))
		return &r
	}
	return nil
}

// worksForACredential reports a ceiling composed for a credential acting as no
// seat ([credentialCeiling]): a machine requester, whose write woke the turn.
func worksForACredential(c authz.Ceiling) bool {
	return c.Kind == authz.CeilingRequester && c.Principal.Kind == iam.KindMachine
}

// credentialRefused is a tool that acts only for a person or a seat of the
// company, refused in a turn a credential acting as no seat woke
// ([companyRequester]): the requester ceiling's reason, for a machine.
func credentialRefused(tool string, terms requesterTerms) tools.Result {
	r := authorityRefusal(tool, DecisionError(terms.action, authz.Decision{Reason: authz.ReasonCeiling,
		Ceiling: authz.CeilingRequester, RequesterKind: iam.KindMachine}))
	r.Output = fmt.Sprintf("%s didn't run: this turn is working for a credential acting as no seat, which "+
		"reaches only the apps its grants name, and %s only for a person or a seat of the company. %s",
		tool, terms.done, terms.nothing)
	return r
}

// companyCeilingRefused is a tool's refusal for whoever its turn acts for: a
// stranger, or a requester who may not act in the company.
func companyCeilingRefused(tool string, kind authz.CeilingKind, done string) string {
	if kind == authz.CeilingStranger {
		return fmt.Sprintf("%s didn't run: this turn was woken by somebody the company does not know, and "+
			"%s for nobody outside the company. A person or a seat of the company has to ask for it.", tool, done)
	}
	return fmt.Sprintf("%s didn't run: whoever this turn is working for may not act in the company now, "+
		"and %s only for somebody who may.", tool, done)
}

// nodeHealthOf is one presence lease as fleet_health reports it, its time left
// read by left, the store's clock ([coord.TimeLeft]), or unsaid where left is
// nil.
func nodeHealthOf(profile placement.NodeProfile, lease coord.Lease,
	left func(coord.Lease) time.Duration) nodeHealth {

	n := nodeHealth{
		ID: profile.ID, Roles: profile.Roles.Names(), Broker: profile.Broker.String(),
		Labels: profile.Labels, Seats: profile.Held,
	}
	if left != nil {
		seconds := int(left(lease) / time.Second)
		n.ExpiresIn = &seconds
	}
	live, ok := coord.StatusFromMeta(lease.Meta)
	if !ok {
		return n
	}
	s := &nodeStatus{
		InFlight: live.InFlight, Draining: live.Draining, StartedAt: live.StartedAt.UTC(),
		Posture: live.Posture, AlarmsEvaluated: live.AlarmsEvaluated,
	}
	if live.AlarmsEvaluated {
		// The marks as the node gave them, longest-standing first, nothing
		// added: present and empty where it looked and found nothing.
		marks := make([]alarmMark, 0, len(live.Alarms))
		for _, m := range live.Alarms {
			marks = append(marks, alarmMark{Kind: m.Kind, Since: m.Since.UTC()})
		}
		s.Alarms = &marks
	}
	n.Status = s
	return n
}

// ---- code_status -------------------------------------------------------- //

// codeStatusJudges is how many of a seat's judgements of the repositories a
// code_status answer names run (and exist) at once ([codeStatus.hottest]):
// each is one code-tool read at the seat's level, and the code log's barrier
// is single-flighted per node (internal/statelog), so a wave shares one round.
// Eight judges the forty repositories a hot list keeps at most (twenty per
// window) in five waves; more in flight would only queue behind the estate
// answerer's slots every other caller shares.
const codeStatusJudges = 8

// The code_status pages, keyset by Gitlet node id over the live roster
// (StatusAsker.GitletNodes). A node's row is some 230 bytes as sent, one
// repository's state on it some 350, so a couple of hundred nodes pass
// ToolAnswerBytes whole: each page is cut whole-row as sent ([foreignRead.fit])
// and asks only its own nodes, save the fleet's first page and its hot list
// ([codeStatus.ask]), so it costs and waits on those alone. 25 by default, as
// fleet_health's; 100 at most: some forty KB of rows with their coverage,
// beside the hot list's fifteen, fit one answer.
const (
	defaultCodeStatusPage = 25
	maxCodeStatusPage     = 100
)

type codeStatus struct{ deps HealthDeps }

var _ tools.SeatCallable = (*codeStatus)(nil)

func (t *codeStatus) Name() string { return CodeStatusTool }

func (t *codeStatus) Description() string {
	return fmt.Sprintf("The company's Gitlet nodes as each states itself: its cache's bytes against "+
		"its budget, how many repositories it holds a copy of, the copies it evicted and repaired "+
		"over the last hour and day, and the fleet's hottest repositories — each node's own count "+
		"of the fetches, clones, pushes and bytes it served of one, and its copy there, the %d "+
		"hottest of the hour and of the day. With `repo`, that one repository on every Gitlet node "+
		"instead. Gitlet nodes by id, a page at a time: pass `next` back as `cursor` until no `next` "+
		"comes back; the hot list rides the first page. `node` reads one Gitlet node, with its own hot "+
		"list. A page asks only its own Gitlet nodes. It reports what the nodes counted and judges "+
		"nothing; `coverage` names a node of the page that did not answer, and its `complete` says "+
		"whether every node the page lists answered — on the fleet's first page, whose hot list is "+
		"every Gitlet node's, whether every Gitlet node did.", cache.HotRepos)
}

func (t *codeStatus) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo": map[string]any{
				"type": "string",
				"description": "One repository's name, as " + CodeListReposTool + " lists it, " +
					"to read on every Gitlet node; the fleet's hottest when left out.",
			},
			"node": map[string]any{
				"type":        "string",
				"description": "One Gitlet node's id, to read that node alone.",
			},
			"cursor": map[string]any{
				"type": "string",
				"description": "The `next` of the previous page, handed back unchanged; leave it out " +
					"for the first page.",
			},
			"limit": map[string]any{
				"type": "integer",
				"description": fmt.Sprintf("How many Gitlet nodes a page lists, 1 to %d; %d by default. A "+
					"page that would outgrow one answer holds fewer, and says where the next begins.",
					maxCodeStatusPage, defaultCodeStatusPage),
			},
		},
	}
}

func (t *codeStatus) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

// forCompany is nil where code_status may read the deployment's view for
// whoever its turn acts for, and the refusal for a credential acting as no seat
// ([worksForACredential]): composed once and bound to the call, so the code
// read decided after it on the same composition reads nothing again.
func (t *codeStatus) forCompany(ctx context.Context, turn *turnctx.Turn) *tools.Result {
	p, _ := iam.From(turnctx.WithPrincipal(ctx, turn))
	if p.Kind != iam.KindSeat {
		return nil
	}
	terms := requesterTerms{action: authz.ActionCodeRead,
		done: "the deployment's view is read", nothing: "Nothing was read."}
	ceiling, err := ceilingOf(ctx, turn, p.Seat, t.deps.Code.Requesters)
	switch {
	case err != nil:
		r := refusedBy(tools.RefusalUnavailable, fmt.Errorf("%w: %w", ErrUndecidable, err),
			fmt.Sprintf("%s could not establish who this turn's work is for, and %s only for a person or a "+
				"seat of the company. %s Try again shortly.", t.Name(), terms.done, terms.nothing))
		return &r
	case worksForACredential(ceiling):
		r := credentialRefused(t.Name(), terms)
		return &r
	}
	return nil
}

// codeStatusFleet is code_status's answer about the fleet's Gitlet nodes: one
// page of them.
type codeStatusFleet struct {
	Nodes []gitletNodeStatus `json:"nodes"`
	// Hot is the hot list, on the first page alone — present there, empty or
	// not — and absent on every later one.
	Hot *[]hotOnNode `json:"hot,omitempty"`
	// Withheld is how many repositories above the list's cut the answer
	// leaves out because the caller's grant does not read them
	// ([codeStatus.hottest]): a seat's only, on the page carrying the list.
	Withheld int `json:"withheld,omitempty"`
	// Next is the id the next page begins after, handed back as `cursor`;
	// absent on the last page.
	Next     string            `json:"next,omitempty"`
	Coverage eventfan.Coverage `json:"coverage"`
}

// gitletNodeStatus is one Gitlet node's account of its cache.
type gitletNodeStatus struct {
	Node  string       `json:"node"`
	At    time.Time    `json:"at"`
	Cache cache.Status `json:"cache"`
}

// hotOnNode is one repository as one Gitlet node served it.
type hotOnNode struct {
	Node string `json:"node"`
	host.RepoStatus
}

// codeStatusRepo is code_status's answer about one repository: one page of
// the Gitlet nodes.
type codeStatusRepo struct {
	Repo  string       `json:"repo"`
	ID    string       `json:"id"`
	Nodes []repoOnNode `json:"nodes"`
	// Next is as codeStatusFleet's.
	Next     string            `json:"next,omitempty"`
	Coverage eventfan.Coverage `json:"coverage"`
}

// repoOnNode is one repository on one Gitlet node: what it served of it in each
// window and its copy there.
type repoOnNode struct {
	Node string    `json:"node"`
	At   time.Time `json:"at"`
	host.RepoStatus
}

func (t *codeStatus) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	// The deployment's view is the health grant's, never read for a credential
	// acting as no seat ([companyRequester]); the repository reads alone would
	// not bound it, since such a credential reads every repository itself.
	// FIRST, so the refusal names no Gitlet grant an administrator would make
	// to no effect.
	if refusal := t.forCompany(ctx, turn); refusal != nil {
		return *refusal, nil
	}
	// The code tools' one composition, the requester decided on Gitlet's read
	// verb: the repository reads below are the requester ceiling's (in a turn
	// a colleague seat woke, read for it too), the rest of the answer the
	// health grant's ([CodeDeps.callerOn]).
	c, refusal := t.deps.Code.callerOn(ctx, turn, t.Name(), authz.ActionCodeRead)
	if refusal != nil {
		return *refusal, nil
	}
	q, refusal := t.query(args)
	if refusal != nil {
		return *refusal, nil
	}
	if strings.TrimSpace(argString(args, "repo")) != "" {
		return t.oneRepo(ctx, turn, c, args, q)
	}
	first := q.cursor == ""
	page, more, report, refusal := t.ask(ctx, q, "", first)
	if refusal != nil {
		return *refusal, nil
	}
	statusOf := make(map[string]host.CodeStatus, len(report.Statuses))
	for _, s := range report.Statuses {
		statusOf[s.Node] = s
	}
	var (
		hot      *[]hotOnNode
		withheld int
	)
	if first {
		// The first page alone carries the hot list (the fleet's, or with
		// `node` that node's own), judged for the caller here, so a later page
		// costs no read of Gitlet's rows.
		var entries []cache.RepoHeat
		var pairs []hotOnNode
		for _, s := range report.Statuses {
			for _, r := range s.Hot {
				entries = append(entries, r.RepoHeat)
				pairs = append(pairs, hotOnNode{Node: s.Node, RepoStatus: r})
			}
		}
		kept, w, failure := t.hottest(ctx, turn, c, entries)
		if failure != nil {
			return *failure, nil
		}
		list := make([]hotOnNode, 0, len(kept))
		for _, i := range kept {
			list = append(list, pairs[i])
		}
		hot, withheld = &list, w
	}
	read := newForeignRead(turn, c.asker.Seat)
	return t.cut(ctx, read, page, more, func(nodes []string, next string) any {
		a := codeStatusFleet{Nodes: make([]gitletNodeStatus, 0, len(nodes)), Hot: hot, Withheld: withheld,
			Next: next, Coverage: coverageOf(report.Coverage, nodes, first && q.node == "")}
		for _, id := range nodes {
			if s, ok := statusOf[id]; ok {
				a.Nodes = append(a.Nodes, gitletNodeStatus{Node: s.Node, At: s.At, Cache: s.Cache})
			}
		}
		return a
	})
}

// codeStatusQuery is the page one code_status call asks for.
type codeStatusQuery struct {
	node   string
	cursor string
	limit  int
}

// query is the call's paging arguments, or the refusal of ones it cannot read
// — refused before anything is read.
func (t *codeStatus) query(args map[string]any) (codeStatusQuery, *tools.Result) {
	q := codeStatusQuery{node: strings.TrimSpace(argString(args, "node")),
		cursor: strings.TrimSpace(argString(args, "cursor")),
		limit:  argInt(args, "limit", defaultCodeStatusPage)}
	switch {
	case q.limit < 1 || q.limit > maxCodeStatusPage:
		r := failed(fmt.Sprintf("%s: `limit` is 1 to %d.", t.Name(), maxCodeStatusPage))
		return q, &r
	case q.node != "" && (q.cursor != "" || args["limit"] != nil):
		r := failed(fmt.Sprintf("%s: `node` reads that one Gitlet node, so it takes no `cursor` or "+
			"`limit`.", t.Name()))
		return q, &r
	}
	return q, nil
}

// ask is the page q asks for and the Gitlet nodes' answers to it: the q.limit
// live Gitlet nodes after q.cursor (or q.node alone), whether more follow, and
// the report of a status question about repository repoID ("" none) asked of
// the page's nodes alone, or, where hot is set without q.node, of every Gitlet
// node with its hot list (bounded by cache.HotRepos, and the bulk of a status):
// the fleet's first page. A roster that could not be read is refused
// unavailable, never paged over whoever answered: those keys move between
// pages, so a node silent on one ask could fall behind a cursor already past
// it. A q.node that is no live Gitlet node is refused not found at once.
func (t *codeStatus) ask(ctx context.Context, q codeStatusQuery, repoID string, hot bool) (
	page []string, more bool, report CodeStatusReport, refusal *tools.Result) {

	roster, err := t.deps.Status.GitletNodes(ctx)
	if err != nil {
		r := refusedBy(tools.RefusalUnavailable, err, CodeStatusTool+" could not read the fleet's Gitlet "+
			"nodes from the coordination store just now, so it says nothing rather than show a fleet "+
			"with nodes missing. Try again shortly.")
		return nil, false, CodeStatusReport{}, &r
	}
	// Sorted and deduplicated: a page begins after its cursor by binary
	// search, so lease order would skip and repeat nodes between pages.
	slices.Sort(roster)
	roster = slices.Compact(roster)
	if q.node != "" {
		if _, found := slices.BinarySearch(roster, q.node); !found {
			r := refused(tools.RefusalNotFound, fmt.Sprintf("%s: no Gitlet node is named %q — a Gitlet node is "+
				"a live node with the gitlet role. Leave `node` out to list them.", t.Name(), clip(q.node)))
			return nil, false, CodeStatusReport{}, &r
		}
		page = []string{q.node}
	} else {
		rest := roster[sortedAfter(roster, func(id string) string { return id }, q.cursor):]
		page, more = rest[:min(q.limit, len(rest))], len(rest) > q.limit
	}
	question := CodeStatusAsk{Nodes: page, Repo: repoID, Hot: hot}
	if hot && q.node == "" {
		// The fleet's hot list is every Gitlet node's, so its page asks them all.
		question.Nodes, question.Every = roster, true
	}
	report, err = t.deps.Status.CodeStatus(ctx, question)
	if err != nil {
		r := refusedBy(tools.RefusalUnavailable, err, CodeStatusTool+" could not ask the Gitlet nodes "+
			"just now. Try again shortly.")
		return nil, false, CodeStatusReport{}, &r
	}
	return page, more, report, nil
}

// cut is the answer of the longest prefix of page that fits one tool answer as
// sent: build makes the answer listing nodes, with next the id the following
// page begins after, "" where nothing is left of page and more says no live
// Gitlet node follows it.
func (t *codeStatus) cut(ctx context.Context, read *foreignRead, page []string, more bool,
	build func(nodes []string, next string) any) (tools.Result, error) {

	k, rendered, err := read.fit(len(page), func(k int) any {
		next := ""
		if k > 0 && (k < len(page) || more) {
			next = page[k-1]
		}
		return build(page[:k], next)
	})
	switch {
	case err != nil:
		return labelFault(ctx, t.Name(), err), nil
	case k == 0 && len(page) > 0:
		return failed(fmt.Sprintf("%s: Gitlet node %q alone is past one answer — its id is that long — so "+
			"no page holds it whole. Pass %q as `cursor` to read the nodes after it.", t.Name(),
			clip(page[0]), page[0])), nil
	}
	return compactAnswer(rendered, "Ask for fewer Gitlet nodes with `limit`, or for one with `node`.")
}

// coverageOf is cover's part for nodes, in nodes' order, a node cover does not
// name left out. Where whole, Complete is cover's own (on the fleet's first
// page, every Gitlet node its hot list was drawn from); otherwise whether
// every node listed answered, since the question asked nodes past what the
// answer had room for, and those are the next page's to report.
func coverageOf(cover eventfan.Coverage, nodes []string, whole bool) eventfan.Coverage {
	part := make(map[string]eventfan.NodeCoverage, len(cover.Nodes))
	for _, n := range cover.Nodes {
		part[n.ID] = n
	}
	out := eventfan.Coverage{Nodes: make([]eventfan.NodeCoverage, 0, len(nodes)), Complete: true}
	for _, id := range nodes {
		n, ok := part[id]
		if ok {
			out.Nodes = append(out.Nodes, n)
		}
		out.Complete = out.Complete && ok && n.Answered
	}
	if whole {
		out.Complete = cover.Complete
	}
	return out
}

// oneRepo is code_status about the repository args names, on the page of Gitlet
// nodes q asks for.
func (t *codeStatus) oneRepo(ctx context.Context, turn *turnctx.Turn, c codeCaller,
	args map[string]any, q codeStatusQuery) (tools.Result, error) {

	name, refusal := repoArg(t.Name(), args)
	if refusal != nil {
		return *refusal, nil
	}
	repo, refusal := t.deps.Code.repo(ctx, turn, t.Name(), c, name)
	if refusal != nil {
		return *refusal, nil
	}
	page, more, report, refusal := t.ask(ctx, q, repo.ID, false)
	if refusal != nil {
		return *refusal, nil
	}
	onNode := make(map[string]repoOnNode, len(report.Statuses))
	for _, s := range report.Statuses {
		on := repoOnNode{Node: s.Node, At: s.At, RepoStatus: host.RepoStatus{RepoHeat: cache.RepoHeat{ID: repo.ID}}}
		if s.Repo != nil && s.Repo.ID == repo.ID {
			on.RepoStatus = *s.Repo
		}
		onNode[s.Node] = on
	}
	read := newForeignRead(turn, c.asker.Seat)
	return t.cut(ctx, read, page, more, func(nodes []string, next string) any {
		a := codeStatusRepo{Repo: repo.Name, ID: repo.ID, Nodes: make([]repoOnNode, 0, len(nodes)),
			Next: next, Coverage: coverageOf(report.Coverage, nodes, false)}
		for _, id := range nodes {
			if on, ok := onNode[id]; ok {
				a.Nodes = append(a.Nodes, on)
			}
		}
		return a
	})
}

// hottest is the hot list as the caller is answered it: the indices into
// entries, in the list's order, that [cache.Hottest] keeps once every
// repository the caller may not read is left out, and how many ranked above
// the cut it left out. A person reads every repository (ADR-0088) and is no
// deputy, so needs no read. A seat is held to its grant and, in a turn a
// colleague seat woke, to that seat's ([codeStatus.readable]), never on a
// guess; a read that fails fails the call ([unjudged]). The list is cut as
// though every unjudged repository were readable, the kept unjudged ones are
// judged a wave at a time ([codeStatus.judgeWave]), and it is cut again, until
// all it keeps are judged readable: exactly what judging all would keep (one
// the cut passes trails HotRepos readable ones per window), with the reads
// bounded by the answer, never by the fleet's some 2 × HotRepos per node.
func (t *codeStatus) hottest(ctx context.Context, turn *turnctx.Turn, c codeCaller,
	entries []cache.RepoHeat) ([]int, int, *tools.Result) {

	if c.asker.Kind != gitlet.PrincipalAgent {
		return cache.Hottest(entries, cache.HotRepos), 0, nil
	}
	names := map[string]string{} // id -> the name a node counted it by
	for _, e := range entries {
		if names[e.ID] == "" {
			names[e.ID] = e.Name
		}
	}
	readable := map[string]bool{} // id -> the verdict, for every repository judged
	for {
		var open []int // the entries not yet found unreadable
		for i, e := range entries {
			if ok, judged := readable[e.ID]; ok || !judged {
				open = append(open, i)
			}
		}
		candidates := make([]cache.RepoHeat, len(open))
		for k, i := range open {
			candidates[k] = entries[i]
		}
		kept := cache.Hottest(candidates, cache.HotRepos)
		var wave []string
		leftOut := false // a repository was left out unread this pass
		for _, k := range kept {
			id := candidates[k].ID
			if _, judged := readable[id]; judged || slices.Contains(wave, id) {
				continue
			}
			if name := names[id]; name == "" || !gitlet.ValidName(name) {
				// NO NODE NAMED IT BY A NAME GITLET RESOLVES, so
				// nothing could judge it readable: left out unread.
				readable[id], leftOut = false, true
				continue
			}
			if len(wave) < codeStatusJudges {
				wave = append(wave, id)
			}
		}
		switch {
		case len(wave) > 0:
			verdicts, failure := t.judgeWave(ctx, turn, c, wave, names)
			if failure != nil {
				return nil, 0, failure
			}
			maps.Copy(readable, verdicts)
		case !leftOut:
			// EVERY REPOSITORY THE LIST KEEPS IS JUDGED READABLE: it is the
			// answer.
			out := make([]int, len(kept))
			for j, k := range kept {
				out[j] = open[k]
			}
			withheld := 0
			for _, ok := range readable {
				if !ok {
					withheld++
				}
			}
			return out, withheld, nil
		}
	}
}

// judgeWave is whether c may read each repository of wave (at most
// codeStatusJudges long), by the name a Gitlet node counted it by
// ([codeStatus.readable]), every read at once.
//
// THE FIRST READ THAT FAILS ENDS THE JUDGEMENT: a failure answers the whole
// call, so its error cancels every read in flight and starts no more. With the
// code log out of quorum each read fails only after its barrier's budget
// (statelog.ElectionBarrierBudget), so reads left running would hold the caller
// a budget per wave and append barriers to the failing log just as an alarm's
// wake sends every granted seat to look.
func (t *codeStatus) judgeWave(ctx context.Context, turn *turnctx.Turn, c codeCaller, wave []string,
	names map[string]string) (map[string]bool, *tools.Result) {

	verdicts := make(map[string]bool, len(wave))
	var mu sync.Mutex
	group, judging := errgroup.WithContext(ctx)
	group.SetLimit(codeStatusJudges)
	for _, id := range wave {
		if judging.Err() != nil {
			// A read failed, or the call was abandoned: nothing read now changes
			// the answer.
			break
		}
		group.Go(func() error {
			// A SLOT FREED BY THE READ THAT FAILED starts nothing either.
			if err := judging.Err(); err != nil {
				return err
			}
			ok, err := t.readable(judging, c, lookedUpRepo(turn, c.asker, names[id]), id, names[id])
			if err != nil {
				return err
			}
			mu.Lock()
			verdicts[id] = ok
			mu.Unlock()
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		r := unjudged(ctx, t.Name(), err)
		return nil, &r
	}
	return verdicts, nil
}

// unjudged is code_status's answer when a grant read its hot list needed could
// not be made: unavailable where Gitlet could not answer now (the conditions
// [codeFailure] answers so), a fault of this node otherwise. IT NAMES NO
// REPOSITORY: the caller may well not read the one the read was about, and
// naming it would leak the very name the answer withholds.
func unjudged(ctx context.Context, tool string, err error) tools.Result {
	var (
		unready  *gitlet.ErrGitUnready
		unserved *estate.ErrUnserved
	)
	why, isCondition := Condition(err)
	switch {
	case errors.As(err, new(*estate.ErrUnknownKeys)):
		// THE FLEET RUNS TWO BUILDS: the condition's own words, AHEAD of the
		// walk nobody served that it wraps, which would call it a wait.
	case errors.As(err, &unready), errors.Is(err, gitlet.ErrNotServing):
		why, isCondition = "no node holding Gitlet's rows could answer in time", true
	case errors.As(err, &unserved) && unserved.Git:
		why, isCondition = "none of the Gitlet nodes could answer in time", true
	}
	if isCondition {
		return refusedBy(tools.RefusalUnavailable, err, fmt.Sprintf("%s could not check which of the "+
			"repositories the Gitlet nodes named you may read (%s), so it answers nothing rather than name "+
			"one you may not. This is NOT an answer about the fleet%s", tool, why,
			conditionNext(err, ": try again shortly.", ". "+stuckRead)))
	}
	return faulted(ctx, tool, err, fmt.Sprintf("%s could not check which of the repositories the Gitlet "+
		"nodes named you may read: %s. This is NOT an answer about the fleet: say you could not read it.",
		tool, faultSaid))
}

// readable reports whether c may read the repository a Gitlet node counted as id
// by name: true where the code tools' resolution admits the asker (and whoever
// the call reads for) and names that id; false where it refuses either, or the
// name now names no repository or another; an error where it could not say.
// It is the code tools' one read ([CodeDeps.read]), so the ceiling seat rides
// it ([gitlet.ReadQuery.CeilingSeat]) and gitlet.ErrAboveCeiling is left out
// like the asker's own refusal. A resolution the turn holds (held) was made by
// that read for the same asker and ceiling, so it answers with no read. It
// keeps nothing it reads: the hot list names repositories no call chose, so it
// takes no hold against a scratch repository's expiry, and a kept resolution
// would be reused by a later code tool with no hold; only [CodeDeps.repoRead]
// keeps one, once its hold is taken ([HoldRepo]).
func (t *codeStatus) readable(ctx context.Context, c codeCaller, held *gitlet.RepoRef,
	id, name string) (bool, error) {

	if held != nil {
		return held.ID == id, nil
	}
	answer, err := t.deps.Code.read(ctx, c, gitlet.ReadQuery{Kind: gitlet.ReadRepo, Repo: name, Fresh: seatRead})
	switch {
	case errors.Is(err, gitlet.ErrNotPermitted), errors.Is(err, gitlet.ErrAboveCeiling),
		errors.Is(err, gitlet.ErrRepoNotFound):
		return false, nil
	case err != nil:
		return false, err
	case answer.Repo == nil:
		return false, fmt.Errorf("builtin: Gitlet answered repository %s's read with no detail", name)
	}
	return answer.Repo.ID == id, nil
}
