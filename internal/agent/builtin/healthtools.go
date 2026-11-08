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
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tools/argval"
)

// THE HEALTH TOOLS: fleet_health, every live node as its own heartbeat states
// it with the alarms its own table has firing, and code_status, the Gitlet nodes'
// caches and the repositories they served most, or one repository's state on
// each — the deployment as an administrator reads it, as ONE list
// ([HealthTools]) a seat's registry and the operator catalogue both take.
//
// # Who reads them
//
// A person as an administrator (authz.ActionFleetHealth, ActionCodeStatus:
// admin's, as the `fleet` question is), and a SEAT only where its role carries
// the health grant (`role.health_tools`, authz.SeatGrantHealth): the table
// decides the seat by its role's grant in place of the class, and the same
// decision is what withholds the two from the surfaces of every seat whose
// role does not carry it ([Withheld]), so a seat is offered them exactly where
// it is admitted.
//
// # They set no thresholds of their own
//
// What is WRONG is the alarm table's to say (ADR-0015): each node evaluates it
// and carries the kinds it found firing on its heartbeat (coord.NodeStatus),
// and fleet_health reports those marks as the node gave them — kind and since
// — beside the facts the heartbeat states, and nothing judged from them. A
// node that has no table it evaluated says so (`alarms_evaluated: false`)
// rather than reading as healthy. code_status reports each Gitlet node's own
// account (internal/code/host, Server.Status): its cache against its budget,
// what it spent on its own account, and what it served most. Neither tool
// ranks a node, flags a number or names a level; a seat that wants to know
// whether something is wrong reads the marks.
//
// # A repository is its grant's, the health grant or not
//
// The health grant gives a seat the deployment's view, never a repository's
// content or rows beyond its own repository grant (ADR-0088): code_status's
// one-repository form resolves the name as the code tools do, refused
// `not_permitted` where the seat may not read it, and the hottest repositories
// a Gitlet node names are held to the same grant — a seat's answer leaves out
// every repository its grant does not read and says how many of those it left
// out above the list's cut (`withheld`), judging them hottest first and only
// as far as the cut, so what it reads is bounded by the answer and not by the
// fleet ([codeStatus.hottest]). A person reads every repository, so a person's
// answer leaves nothing out.
//
// # And, in a turn somebody woke, read only for the company
//
// A granted seat reads the deployment's view FOR whoever woke its turn, and
// the requester ceiling ([ceilingOf], ceiling.go) holds both tools to it. A
// turn a stranger woke — a chat sender the company does not know — or a
// person who may not act in the company now is answered nothing by either: the
// view is an administrator's, and the grant lets a seat watch the deployment,
// not relay it to whoever can message the seat. fleet_health decides that
// somebody on [authz.ActionFleetHealthRead] ([fleetHealth.forRequester]) and
// code_status on Gitlet's read verb, both rows everybody of the company
// who may act holds, never on the health row: a requester holds no admin and a
// colleague seat no grant of the asker's, so the health row would refuse every
// turn anybody woke. A colleague or a person of the company is answered the
// seat's whole view.
//
// code_status's repository reads are code reads besides, which the requester
// ceiling holds further (ADR-0088, [CodeDeps.callerOn]): in a turn a colleague
// seat woke, each is read for that seat as well, so the hot list leaves out
// what the colleague may not read and counts it withheld, and the
// one-repository form is refused as the code tools refuse a read above the
// ceiling.

// The health tools' names: each is its authority row's action.
const (
	FleetHealthTool = string(authz.ActionFleetHealth)
	CodeStatusTool  = string(authz.ActionCodeStatus)
)

// HealthToolNames are the two, in the order a catalogue lists them.
func HealthToolNames() []string { return []string{FleetHealthTool, CodeStatusTool} }

// LeaseLister is the coordination store's live leases, as fleet_health reads
// the nodes' presence — internal/coord's ListLive, declared by its consumer.
type LeaseLister interface {
	ListLive(ctx context.Context, class coord.Class) ([]coord.Lease, error)
}

// StatusAsker is the fleet's Gitlet nodes and what Gitlet on each says of itself
// — internal/engine's roster off the presence leases and its scatter on
// crewlet.code.status, declared by their consumer.
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
	// Nodes is the Gitlet nodes asked, by id. The report covers these and no
	// other: a reply from a node it does not name is dropped — an older
	// build's, which answers whatever the question names — and the ask
	// ends once every one of them has answered.
	Nodes []string
	// Every says Nodes is every Gitlet node GitletNodes listed, so the question
	// names none and every Gitlet node answers it: the question stays the same
	// few bytes however large the fleet, where naming the whole roster would
	// send every node the roster.
	Every bool
	// Repo is the id of a repository each node reports beside itself; ""
	// asks about none.
	Repo string
	// Hot asks each node for its hot list (host.StatusAsk.Hot).
	Hot bool
}

// CodeStatusReport is the Gitlet nodes' answers to one status question, as the
// node that asked heard them: each node's own account, and which of the Gitlet
// nodes it asked answered — the one coverage shape every fanned answer carries
// (internal/eventfan), complete where every node asked answered.
type CodeStatusReport struct {
	Statuses []host.CodeStatus
	Coverage eventfan.Coverage
}

// HealthDeps are what the health tools read through. The zero value serves
// neither.
type HealthDeps struct {
	// Leases is the fleet's live leases. Nil omits fleet_health.
	Leases LeaseLister

	// Requesters composes who a seat's turn is acting for, and Chart is the
	// chart the authority table decides them by: fleet_health in a turn
	// somebody woke reads only for somebody of the company who may act
	// ([fleetHealth.forRequester]). REQUIRED beside Leases — nil omits
	// fleet_health — so a surface that left the ceiling out serves no view of
	// the deployment rather than one to whoever messages a granted seat.
	// code_status takes its own from Code, as the code reads do.
	Requesters Requesters
	Chart      authz.Chart

	// Self is this node's id, which fleet_health's answer marks.
	Self string

	// Now is the clock a lease's lifetime is read against. Nil is the wall
	// clock.
	Now func() time.Time

	// Status asks the Gitlet nodes' status. Nil omits code_status, as do Code
	// deps that serve no code tool ([CodeDeps.serves]): a company that hosts
	// no code has no Gitlet node to ask.
	Status StatusAsker

	// Code is the code tools' own deps: code_status names a repository by
	// name and judges a seat's grant over what it reports exactly as they
	// do, through the same resolution and for whoever woke the turn as they
	// read for them, and Code.Actor names the caller.
	Code CodeDeps
}

// HealthTools are the health tools a seat and an operator share: ONE list, so
// a schema and a refusal's words are written once. Every one reads.
//
// EACH ONLY WITH THE REQUESTER CEILING'S HALVES BESIDE IT: fleet_health over
// the leases with Requesters and Chart, and code_status ON THE CODE TOOLS'
// TERMS ([CodeDeps.serves]), its repository reads being theirs. Deps wired
// without them serve neither — never a tool that reads the deployment for
// whoever messaged a seat whatever the seat may.
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
// one.
//
// PROVEN READS, and idempotent: asking twice costs a round and changes
// nothing, so a turn that only looked at the fleet delivered nothing (the
// delivery check), a worker may be handed them (the guard), and the operator
// catalogue audits no call to one.
func healthAnnotations(name string) (tools.Annotations, bool) {
	if slices.Contains(HealthToolNames(), name) {
		return tools.Annotations{ReadOnly: mcp.Yes, Idempotent: mcp.Yes}, true
	}
	return tools.Annotations{}, false
}

// Withheld is the builtins of snapshot the authority table refuses the party on
// ctx before any object is named ([Admits]), sorted: the tools that party's
// surfaces must not offer it.
//
// THE ONE QUESTION A CALL IS DECIDED BY, asked once ahead of the calls, so
// what a seat is offered and what it is admitted to cannot differ: a tool the
// table refuses a seat outright — a verb whose row names a role grant its role
// does not carry — is one a model shown it would reach for and be refused,
// round after round. ONLY A REFUSAL WITHHOLDS: a node that cannot decide
// ([ErrUndecidable]) offers the tool and the call decides it again, as every
// call is; withholding on silence would hide a tool from a seat that holds it.
// An MCP or agent-to-agent tool is no builtin and has no row to ask.
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

// The fleet_health pages: nodes by id, a page at a time.
//
// A FLEET OUTGROWS ONE ANSWER. A node's row is some three hundred bytes as it
// is sent — its roles, broker, labels and heartbeat — and a data node's a few
// dozen more for every alarm it has firing, so a fleet of a couple of hundred
// nodes, fewer in an alarm storm, would read past ToolAnswerBytes whole; and
// long before that it is a turn's context spent on nodes nobody asked about,
// by every turn an alarm's wake starts. So the answer is PAGED, by node id,
// and NARROWED by what the reader is asking — `node` for one node, `firing`
// for the nodes whose own table has an alarm firing, the table's verdict and
// no threshold of this tool's — and every page is cut whole-row at the answer
// as it is sent ([foreignRead.fit]), so no page passes ToolAnswerBytes.
// TWENTY-FIVE a page by default, the code tools' listing's page: a screen of
// nodes, and the whole of most fleets. A HUNDRED at most, near half of one
// answer.
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
	// Unevaluated is, where the call asked for the nodes with an alarm
	// firing, how many live nodes have no table they evaluated — a node
	// holding no data, one still booting, or one whose heartbeat said
	// nothing — which `firing` cannot judge and so leaves out: counted,
	// so silence never reads as health. Absent otherwise, where every node
	// is listed with what it said.
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
		// BY THE CHECK'S OWN READER ([argval.Bool]): true or false and
		// nothing else, the rule the check at the surface holds it to.
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
	// ExpiresIn is the seconds its presence lease has left: a node that
	// stopped renewing reads as running out.
	ExpiresIn int `json:"expires_in"`
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
	now := time.Now()
	if t.deps.Now != nil {
		now = t.deps.Now()
	}
	nodes := make([]nodeHealth, 0, len(leases))
	for _, lease := range leases {
		profile, ok := placement.FromLease(lease)
		if !ok {
			continue
		}
		nodes = append(nodes, nodeHealthOf(profile, lease, now))
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
	// THE PAGE: the nodes after the cursor, at most `limit` of them, cut
	// whole-row wherever they would outgrow one answer as it is sent. The
	// listing is keyset by id, so the next page begins after the last row
	// kept.
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
// after cursor stands: 0 for no cursor. Both health tools page their nodes by
// it.
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
// whoever its turn is acting for, and otherwise the refusal it is answered
// with, before anything is read.
//
// A SEAT'S CALL IN A TURN SOMEBODY WOKE IS THEIR ERRAND: the seat's grant opens
// an administrator's view of the deployment — every node, its roles, labels and
// config posture, every alarm firing and since when — and a seat that read it
// out for whoever could message it would hand that view to anybody on a chat
// surface the seat listens on. So whoever the turn is acting for ([ceilingOf],
// the one composition every family's ceiling reads) is decided AGAIN, by the
// authority table ([authz.DecideWithin]), on [authz.ActionFleetHealthRead]: a
// stranger is answered nothing, and so is a person who may not act in the
// company now; a colleague seat or a person of the company is answered the
// seat's whole view, which the row admits them to — the product's decision,
// recorded on the row. A requester this node could not compose or decide is
// undecidable, never a turn nobody woke. A person's or a token's own call is
// no deputy, and a turn nobody woke — a schedule, an alarm's wake — reads for
// nobody but the seat.
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

// companyRequester is nil where a seat's call may act for whoever its turn is
// acting for ([ceilingOf]), decided by the authority table on terms.action,
// and otherwise the refusal it is answered with. A stranger, or a person who
// may not act in the company now, is refused; a colleague seat, a person of
// the company, and a turn nobody woke are not. A requester this node could not
// compose or decide is undecidable, never a turn nobody woke. A person's or a
// token's own call is no deputy.
func companyRequester(ctx context.Context, turn *turnctx.Turn, requesters Requesters, chart authz.Chart,
	tool string, terms requesterTerms) *tools.Result {
	// WHO CALLS, as the gate decided them: the context's answer, or the
	// turn's seat where nothing has answered (turnctx.WithPrincipal).
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

// companyCeilingRefused is what a tool refused for whoever its turn is acting for is
// answered with, by who that is: a stranger, or a requester who may not act in
// the company.
func companyCeilingRefused(tool string, kind authz.CeilingKind, done string) string {
	if kind == authz.CeilingStranger {
		return fmt.Sprintf("%s didn't run: this turn was woken by somebody the company does not know, and "+
			"%s for nobody outside the company. A person or a seat of the company has to ask for it.", tool, done)
	}
	return fmt.Sprintf("%s didn't run: whoever this turn is working for may not act in the company now, "+
		"and %s only for somebody who may.", tool, done)
}

// nodeHealthOf is one presence lease as fleet_health reports it.
func nodeHealthOf(profile placement.NodeProfile, lease coord.Lease, now time.Time) nodeHealth {
	n := nodeHealth{
		ID: profile.ID, Roles: profile.Roles.Names(), Broker: profile.Broker.String(),
		Labels: profile.Labels, Seats: profile.Held,
	}
	if !lease.ExpiresAt.IsZero() {
		n.ExpiresIn = max(int(lease.ExpiresAt.Sub(now)/time.Second), 0)
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
		// THE MARKS AS THE NODE GAVE THEM, in its order — the longest-standing
		// first — and nothing added: present and empty where it looked and
		// found nothing.
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
// code_status answer names run — and exist — at once: each is the code tools'
// one read of a repository at the seat's level, and the code log's barrier is
// single-flighted per node (internal/statelog), so a batch in flight shares one
// round, and the judgement goes a wave of them at a time ([codeStatus.hottest]).
// EIGHT, so the forty repositories at most a hot list keeps — twenty of each
// window — are judged in five waves where the seat reads them all, a wave more
// for every eight it may not read above the cut, while more in flight would
// only queue behind the estate answerer's slots every other caller shares.
const codeStatusJudges = 8

// The code_status pages: the Gitlet nodes by id, a page at a time.
//
// A FLEET'S GITLET NODES OUTGROW ONE ANSWER. A Gitlet node's row is some two hundred
// and thirty bytes as it is sent — its cache's figures — and one repository's
// state on a node some three hundred and fifty, each beside the node's part in
// the coverage, so a couple of hundred Gitlet nodes read past ToolAnswerBytes whole
// and the answer was refused, with nothing to narrow it by but the node list it
// could not read. So both answers are PAGED, by node id, over the live Gitlet
// nodes the presence leases list (StatusAsker.GitletNodes) — a node that did not
// answer is a page's row of coverage with no status beside it, so a page is
// bounded by its count of nodes whoever answered — and every page is cut
// whole-row at the answer as it is sent ([foreignRead.fit]), so no page passes
// ToolAnswerBytes. `node` reads one Gitlet node.
//
// AND THE PAGE IS WHAT IS ASKED, NOT ONLY WHAT IS SENT ([codeStatus.ask]): the
// roster is read first — a read of the coordination store this node holds, no
// Gitlet node asked — and the status question names the page's nodes alone, so a
// node it does not name answers nothing and the ask ends once the page's nodes
// have answered. A page costs its own nodes' answers, never the fleet's, and
// waits on its own slowest node, never on one silent anywhere in the fleet:
// asking the whole fleet for every page made walking N Gitlet nodes cost N²/limit
// answers, each with its node's hot list, and every page wait the whole
// two-second budget on one silent node. A page's `coverage` lists its own
// nodes' parts, and its `complete` says whether every one it lists answered —
// on the fleet's first page, whether every Gitlet node did, its hot list being
// drawn from all of them.
//
// A ROSTER THAT COULD NOT BE READ IS REFUSED, unavailable, as fleet_health
// refuses it, rather than paged over whoever answered a question asked of
// everybody: keys taken from each page's answerers move between pages, so a
// node silent on one page's ask and heard on the next could fall behind a
// cursor already past it, and a listing that skips a node it cannot see is
// the fleet with nodes missing that fleet_health declines to show.
//
// THE HOT LIST RIDES THE FIRST PAGE ALONE: it is the fleet's, already bounded by
// cache.HotRepos a window however many Gitlet nodes there are (some fifteen KB at
// most), and a seat's copy of it costs a read of Gitlet's rows for every
// repository judged ([codeStatus.hottest]). So the fleet's first page is the one
// question every Gitlet node answers, each with its hot list (host.StatusAsk.Hot);
// every other question — a later page, a repository's, a `node` read of the
// repository — asks no node for its list, which is the bulk of a status. With
// `node` the first page's list is that node's own, asked of it alone.
//
// TWENTY-FIVE a page by default, fleet_health's and the code tools' listing's
// page: a screen of nodes, and the whole of most fleets. A HUNDRED at most, as
// fleet_health's: a hundred rows of the larger kind with their coverage, some
// forty KB, beside the hot list's fifteen, are inside one answer, and a page
// that would not be — long node ids — is cut.
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

// codeStatusFleet is code_status's answer about the fleet's Gitlet nodes: one
// page of them.
type codeStatusFleet struct {
	Nodes []gitletNodeStatus `json:"nodes"`
	// Hot is the hot list, on the first page alone — present there, empty or
	// not — and absent on every later one.
	Hot *[]hotOnNode `json:"hot,omitempty"`
	// Withheld is how many repositories hotter than the list's cut the answer
	// leaves out because the caller's grant does not read them — a seat's
	// only, and never one below the cut, which is not in the list whoever
	// reads it ([codeStatus.hottest]). On the page carrying the list.
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

	// WHO IT READS AS, AND FOR: the code tools' one composition, with
	// whoever woke the turn decided on Gitlet's read verb — the
	// repository reads below are the requester ceiling's, the rest of the
	// answer the health grant's ([CodeDeps.callerOn]).
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
		// THE FIRST PAGE CARRIES THE HOT LIST: the fleet's, or with `node`
		// that node's own — each the nodes asked, and no other ([codeStatus.ask])
		// — judged for the caller here alone, so a later page costs no read of
		// Gitlet's rows.
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

// ask is the page q asks for and the Gitlet nodes' answers to it: the page's Gitlet
// nodes by id — the q.limit live Gitlet nodes after q.cursor, or q.node alone —
// whether live Gitlet nodes follow it, and the report of a status question about
// the repository whose id is repoID ("" none) asked of the page's nodes alone,
// or, where hot is set without q.node, of every Gitlet node with its hot list:
// the fleet's first page. Refused unavailable where the roster or the nodes
// could not be read, and not found where q.node is no live Gitlet node — said at
// once, never waited out as a node that does not answer.
func (t *codeStatus) ask(ctx context.Context, q codeStatusQuery, repoID string, hot bool) (
	page []string, more bool, report CodeStatusReport, refusal *tools.Result) {

	roster, err := t.deps.Status.GitletNodes(ctx)
	if err != nil {
		r := refusedBy(tools.RefusalUnavailable, err, CodeStatusTool+" could not read the fleet's Gitlet "+
			"nodes from the coordination store just now, so it says nothing rather than show a fleet "+
			"with nodes missing. Try again shortly.")
		return nil, false, CodeStatusReport{}, &r
	}
	// SORTED AND EACH ONCE HERE, by the reader whose keyset needs it: a page
	// begins after its cursor by binary search, so a roster in lease order
	// would skip and repeat nodes between pages.
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
		// THE FLEET'S HOT LIST IS EVERY GITLET NODE'S, so its page alone asks
		// them all.
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
// it is sent: build makes the answer listing nodes, with next the id the
// following page begins after — "" on the last, where nothing is left of page
// and more says no live Gitlet node follows it. The listing is keyset by id, so
// the next page begins after the last node kept, whether limit or the
// answer's size cut it.
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

// coverageOf is cover's part for nodes — each node's own, in nodes' order, a
// node cover does not name left out. Complete is, where whole, cover's own —
// every node the question was asked of, which on the fleet's first page is
// every Gitlet node its hot list was drawn from — and otherwise whether every node
// listed answered: a page's question asked a few nodes past what the answer
// had room for, and those are the next page's to report.
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

// hottest is the fleet's hot list as the caller is answered it — which of
// entries [cache.Hottest] keeps once every repository the caller may not read
// is left out, as indices into entries in the list's order — and how many
// repositories ranked above the list's cut it left out for that.
//
// AN OPERATOR READS EVERY REPOSITORY (ADR-0088: a person maintains every one,
// and a token reads every one), so a person's list is cut without a read — and
// a person's call is no deputy, so it reads for nobody else either. A SEAT is
// held to its own grant, through the code tools' one resolution of a name
// ([CodeDeps.repo]'s read, at the seat's level, judged by the node that
// answers) — and, in a turn a colleague seat woke, to that seat's too, judged
// by the same node on the same read: a repository either may not read, one the
// name no longer names, or one the name names under another id — renamed since
// the Gitlet node counted it — is left out, never shown on a guess. A read that
// could not be made fails the call, unavailable, rather than leaving out what
// it could not judge ([unjudged]).
//
// # Hottest first, and only to the cut
//
// The list keeps at most cache.HotRepos of each window however large the
// fleet, and repository homes are spread over the Gitlet nodes by rendezvous, so
// the nodes' own lists are mostly disjoint and the fleet names some 2 ×
// HotRepos repositories for every Gitlet node. Judging each of them was a
// linearizable read apiece — a barrier on the code log for every wave of reads
// — growing with the Gitlet nodes rather than with the answer: a hundred and
// twenty reads on three of them, four hundred on ten, for a list of forty. So
// the list is cut as though every repository nobody has judged yet were
// readable; the repositories it keeps that nobody has judged are judged in a
// wave ([codeStatus.judgeWave]), hottest first; and it is cut again without
// those found unreadable, until every repository it keeps has been judged
// readable. What it keeps then is exactly what judging every repository would
// have kept: a repository the cut passes over trails at least HotRepos
// readable ones in each window, so no verdict on it could bring it in. The
// reads are bounded by the answer — its own repositories, and those ranked
// above them that the caller may not read — and so is `withheld`, which counts
// those: a repository below the cut is not in the list whether or not the
// caller may read it.
func (t *codeStatus) hottest(ctx context.Context, turn *turnctx.Turn, c codeCaller,
	entries []cache.RepoHeat) ([]int, int, *tools.Result) {

	if c.asker.Kind != code.PrincipalAgent {
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
			if name := names[id]; name == "" || !code.ValidName(name) {
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

// judgeWave is whether c may read each repository of wave, by the name a Gitlet
// node counted it by ([codeStatus.readable]): every read of the wave at once,
// the wave being at most codeStatusJudges long.
//
// THE FIRST READ THAT FAILS ENDS THE JUDGEMENT. A failure answers the whole
// call, so a read still in flight or not yet started could change nothing but
// how long the caller waits for that answer: the reads run in one group whose
// first error cancels every read in flight (errgroup.WithContext), no read
// starts once it has, and at most codeStatusJudges of them run — and exist —
// at once. Left to run on, they were the incident's cost: with the code log
// out of quorum a linearizable read fails only once its barrier's budget is
// spent (statelog.DefaultBarrierBudget, five seconds), so each wave of reads
// still started held the caller for another such budget and appended another
// barrier to the log that was failing them, at the moment an alarm's wake
// sends every granted seat to look.
func (t *codeStatus) judgeWave(ctx context.Context, turn *turnctx.Turn, c codeCaller, wave []string,
	names map[string]string) (map[string]bool, *tools.Result) {

	verdicts := make(map[string]bool, len(wave))
	var mu sync.Mutex
	group, judging := errgroup.WithContext(ctx)
	group.SetLimit(codeStatusJudges)
	for _, id := range wave {
		if judging.Err() != nil {
			// A READ FAILED, or the call was abandoned: nothing read now
			// changes the answer.
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
// not be made: unavailable where Gitlet could not answer now — the
// conditions [codeFailure] answers so — and a fault of this node otherwise.
//
// IT NAMES NO REPOSITORY. The read that failed was about one a Gitlet node named,
// which the caller may well not read, and a failure that named it would tell a
// seat the very name the answer exists to withhold.
func unjudged(ctx context.Context, tool string, err error) tools.Result {
	var (
		unready  *code.ErrGitUnready
		unserved *estate.ErrUnserved
	)
	why, transient := Condition(err)
	switch {
	case errors.As(err, &unready), errors.Is(err, code.ErrNotServing):
		why, transient = "no node holding Gitlet's rows could answer in time", true
	case errors.As(err, &unserved) && unserved.Git:
		why, transient = "none of the Gitlet nodes could answer in time", true
	}
	if transient {
		return refusedBy(tools.RefusalUnavailable, err, fmt.Sprintf("%s could not check which of the "+
			"repositories the Gitlet nodes named you may read (%s), so it answers nothing rather than name "+
			"one you may not. This is NOT an answer about the fleet: try again shortly.", tool, why))
	}
	return faulted(ctx, tool, err, fmt.Sprintf("%s could not check which of the repositories the Gitlet "+
		"nodes named you may read: %s. This is NOT an answer about the fleet: say you could not read it.",
		tool, faultSaid))
}

// readable reports whether c may read the repository a Gitlet node counted as id
// by name: true where the code tools' resolution of the name admits the asker
// — and whoever the call reads for — and names that id, false where it refuses
// either or the name names no repository or another one now, and an error
// where it could not say.
//
// THE CODE TOOLS' ONE READ ([CodeDeps.read]), so the seat a turn reads for
// rides on it ([code.ReadQuery.CeilingSeat]) exactly as on theirs, and a
// repository the asker may read and that seat may not is answered
// code.ErrAboveCeiling — left out here as the asker's own refusal is. A
// resolution the turn already holds (held) was made by that read, for the
// same asker and in the same turn, so for the same ceiling, and answers with
// no read.
//
// WHAT IT READS IT KEEPS NOWHERE, and it takes no turn to keep it in: the hot
// list names repositories no call of the turn chose, so it holds none of them
// from a scratch repository's expiry — as code_list_repos holds none — and a
// resolution it kept in the turn's lookups would be one a later code tool
// reused with no hold taken. Only [CodeDeps.repoRead] keeps a resolution,
// once the turn's hold on it is taken ([HoldRepo]).
func (t *codeStatus) readable(ctx context.Context, c codeCaller, held *code.RepoRef,
	id, name string) (bool, error) {

	if held != nil {
		return held.ID == id, nil
	}
	answer, err := t.deps.Code.read(ctx, c, code.ReadQuery{Kind: code.ReadRepo, Repo: name, Fresh: seatRead})
	switch {
	case errors.Is(err, code.ErrNotPermitted), errors.Is(err, code.ErrAboveCeiling),
		errors.Is(err, code.ErrRepoNotFound):
		return false, nil
	case err != nil:
		return false, err
	case answer.Repo == nil:
		return false, fmt.Errorf("builtin: Gitlet answered repository %s's read with no detail", name)
	}
	return answer.Repo.ID == id, nil
}
