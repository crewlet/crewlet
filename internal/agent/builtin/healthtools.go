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

	"github.com/crewlet/crewlet/internal/agent/turnctx"
	"github.com/crewlet/crewlet/internal/authz"
	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/mcp"
	"github.com/crewlet/crewlet/internal/seat/placement"
	"github.com/crewlet/crewlet/internal/tools"
)

// THE HEALTH TOOLS: fleet_health, every live node as its own heartbeat states
// it with the alarms its own table has firing, and code_status, the git nodes'
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
// rather than reading as healthy. code_status reports each git node's own
// account (internal/code/host, Server.Status): its cache against its budget,
// what it spent on its own account, and what it served most. Neither tool
// ranks a node, flags a number or names a level; a seat that wants to know
// whether something is wrong reads the marks.
//
// # A repository is its grant's, the health grant or not
//
// The health grant gives a seat the deployment's view, never a repository's
// content or rows beyond its own repository grant (ADR-0058): code_status's
// one-repository form resolves the name as the code tools do, refused
// `not_permitted` where the seat may not read it, and the hottest repositories
// a git node names are held to the same grant — a seat's answer leaves out
// every repository its grant does not read and says how many it left out
// (`withheld`). A person reads every repository, so a person's answer leaves
// nothing out.

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

// StatusAsker asks every git node of the fleet what its code host says of
// itself, about one repository beside the node where repoID is not "" —
// internal/engine's scatter on crewlet.code.status, declared by its consumer.
type StatusAsker interface {
	CodeStatus(ctx context.Context, repoID string) (CodeStatusReport, error)
}

// CodeStatusReport is the git nodes' answers to one status question, as the
// node that asked heard them: each node's own account, and which of the git
// nodes it asked answered — the one coverage shape every fanned answer carries
// (internal/eventfan).
type CodeStatusReport struct {
	Statuses []host.CodeStatus
	Coverage eventfan.Coverage
}

// HealthDeps are what the health tools read through. The zero value serves
// neither.
type HealthDeps struct {
	// Leases is the fleet's live leases. Nil omits fleet_health.
	Leases LeaseLister

	// Self is this node's id, which fleet_health's answer marks.
	Self string

	// Now is the clock a lease's lifetime is read against. Nil is the wall
	// clock.
	Now func() time.Time

	// Status asks the git nodes' status. Nil omits code_status, as does a
	// nil Code.Code: a company that hosts no code has no git node to ask.
	Status StatusAsker

	// Code is the code tools' own deps: code_status names a repository by
	// name and judges a seat's grant over what it reports exactly as they
	// do, through the same resolution, and Code.Actor names the caller.
	Code CodeDeps
}

// HealthTools are the health tools a seat and an operator share: ONE list, so
// a schema and a refusal's words are written once. Every one reads.
func HealthTools(deps HealthDeps) []tools.Callable {
	var out []tools.Callable
	if deps.Leases != nil {
		out = append(out, &fleetHealth{deps: deps})
	}
	if deps.Status != nil && deps.Code.Code != nil {
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

type fleetHealth struct{ deps HealthDeps }

var _ tools.Callable = (*fleetHealth)(nil)

func (t *fleetHealth) Name() string { return FleetHealthTool }

func (t *fleetHealth) Description() string {
	return "The fleet as each live node's own heartbeat states it: its roles, how its broker takes " +
		"part, the turns it is running, whether it is draining, what it concluded about its config, " +
		"the seats it holds — and every alarm its own table has firing, with when it was raised. " +
		"What is wrong is the alarms' to say: this reports them as the nodes did and judges nothing " +
		"else. A node with `alarms_evaluated: false` has no table it has evaluated (a node holding " +
		"no data, or one still booting), which is not the same as nothing firing."
}

func (t *fleetHealth) Parameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

// fleetHealthAnswer is fleet_health's answer.
type fleetHealthAnswer struct {
	ThisNode string       `json:"this_node"`
	Nodes    []nodeHealth `json:"nodes"`
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
	InFlight         int       `json:"in_flight"`
	Draining         bool      `json:"draining"`
	StartedAt        time.Time `json:"started_at,omitzero"`
	Posture          string    `json:"posture,omitempty"`
	ProjectionsReady int       `json:"projections_ready,omitempty"`
	ProjectionsTotal int       `json:"projections_total,omitempty"`
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

func (t *fleetHealth) Call(ctx context.Context, _ map[string]any) (tools.Result, error) {
	leases, err := t.deps.Leases.ListLive(ctx, coord.ClassNode)
	if err != nil {
		return refusedBy(tools.RefusalUnavailable, err, "fleet_health could not read the fleet's "+
			"presence from the coordination store just now, so it says nothing rather than show a "+
			"fleet with nodes missing. Try again shortly."), nil
	}
	now := time.Now()
	if t.deps.Now != nil {
		now = t.deps.Now()
	}
	answer := fleetHealthAnswer{ThisNode: t.deps.Self, Nodes: make([]nodeHealth, 0, len(leases))}
	for _, lease := range leases {
		profile, ok := placement.FromLease(lease)
		if !ok {
			continue
		}
		answer.Nodes = append(answer.Nodes, nodeHealthOf(profile, lease, now))
	}
	slices.SortFunc(answer.Nodes, func(a, b nodeHealth) int { return cmp.Compare(a.ID, b.ID) })
	return newForeignRead(nil, "").answer(ctx, t.Name(), answer,
		"The fleet is too large to read whole in one answer.")
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
		Posture: live.Posture, ProjectionsReady: live.ProjectionsReady,
		ProjectionsTotal: live.ProjectionsTotal, AlarmsEvaluated: live.AlarmsEvaluated,
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
// code_status answer names run at once: each is the code tools' one read of a
// repository at the seat's level, and the code log's barrier is single-flighted
// per node (internal/statelog), so a batch in flight shares one round. EIGHT,
// so a fleet of three git nodes, forty repositories named on each, is judged in
// some fifteen of those rounds, while more in flight would only queue behind
// the estate answerer's slots every other caller shares.
const codeStatusJudges = 8

type codeStatus struct{ deps HealthDeps }

var _ tools.SeatCallable = (*codeStatus)(nil)

func (t *codeStatus) Name() string { return CodeStatusTool }

func (t *codeStatus) Description() string {
	return fmt.Sprintf("The company's git nodes as each states itself: its cache's bytes against "+
		"its budget, how many repositories it holds a copy of, the copies it evicted and repaired "+
		"over the last hour and day, and the fleet's hottest repositories — each node's own count "+
		"of the fetches, clones, pushes and bytes it served of one, and its copy there, the %d "+
		"hottest of the hour and of the day. With `repo`, that one repository on every git node "+
		"instead. It reports what the nodes counted and judges nothing; `coverage` names a node "+
		"that did not answer.", cache.HotRepos)
}

func (t *codeStatus) Parameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"repo": map[string]any{
				"type": "string",
				"description": "One repository's name, as " + CodeListReposTool + " lists it, " +
					"to read on every git node; the fleet's hottest when left out.",
			},
		},
	}
}

func (t *codeStatus) Call(ctx context.Context, args map[string]any) (tools.Result, error) {
	return t.CallForTurn(ctx, nil, args)
}

// codeStatusFleet is code_status's answer about the fleet's git nodes.
type codeStatusFleet struct {
	Nodes []gitNodeStatus `json:"nodes"`
	Hot   []hotOnNode     `json:"hot"`
	// Withheld is how many repositories the answer leaves out because the
	// caller's grant does not read them — a seat's only.
	Withheld int               `json:"withheld,omitempty"`
	Coverage eventfan.Coverage `json:"coverage"`
}

// gitNodeStatus is one git node's account of its cache.
type gitNodeStatus struct {
	Node  string       `json:"node"`
	At    time.Time    `json:"at"`
	Cache cache.Status `json:"cache"`
}

// hotOnNode is one repository as one git node served it.
type hotOnNode struct {
	Node string `json:"node"`
	host.RepoStatus
}

// codeStatusRepo is code_status's answer about one repository.
type codeStatusRepo struct {
	Repo     string            `json:"repo"`
	ID       string            `json:"id"`
	Nodes    []repoOnNode      `json:"nodes"`
	Coverage eventfan.Coverage `json:"coverage"`
}

// repoOnNode is one repository on one git node: what it served of it in each
// window and its copy there.
type repoOnNode struct {
	Node string    `json:"node"`
	At   time.Time `json:"at"`
	host.RepoStatus
}

func (t *codeStatus) CallForTurn(ctx context.Context, turn *turnctx.Turn,
	args map[string]any) (tools.Result, error) {

	asker, refusal := t.deps.Code.asker(ctx, turn, t.Name())
	if refusal != nil {
		return *refusal, nil
	}
	if strings.TrimSpace(argString(args, "repo")) != "" {
		return t.oneRepo(ctx, turn, asker, args)
	}
	report, err := t.deps.Status.CodeStatus(ctx, "")
	if err != nil {
		return refusedBy(tools.RefusalUnavailable, err, "code_status could not ask the git nodes "+
			"just now. Try again shortly."), nil
	}
	answer := codeStatusFleet{Nodes: make([]gitNodeStatus, 0, len(report.Statuses)),
		Coverage: report.Coverage}
	var entries []cache.RepoHeat
	var pairs []hotOnNode
	for _, s := range report.Statuses {
		answer.Nodes = append(answer.Nodes, gitNodeStatus{Node: s.Node, At: s.At, Cache: s.Cache})
		for _, r := range s.Hot {
			entries = append(entries, r.RepoHeat)
			pairs = append(pairs, hotOnNode{Node: s.Node, RepoStatus: r})
		}
	}
	slices.SortFunc(answer.Nodes, func(a, b gitNodeStatus) int { return cmp.Compare(a.Node, b.Node) })
	readable, withheld, failure := t.judge(ctx, turn, asker, entries)
	if failure != nil {
		return *failure, nil
	}
	answer.Withheld = withheld
	var kept []cache.RepoHeat
	var keptPairs []hotOnNode
	for i, e := range entries {
		if readable[e.ID] {
			kept = append(kept, e)
			keptPairs = append(keptPairs, pairs[i])
		}
	}
	answer.Hot = make([]hotOnNode, 0, len(kept))
	for _, i := range cache.Hottest(kept, cache.HotRepos) {
		answer.Hot = append(answer.Hot, keptPairs[i])
	}
	return newForeignRead(turn, asker.Seat).page(ctx, t.Name(), answer,
		"Name one repository with `repo` to read it alone.")
}

// oneRepo is code_status about the repository args names, on every git node.
func (t *codeStatus) oneRepo(ctx context.Context, turn *turnctx.Turn, asker code.Principal,
	args map[string]any) (tools.Result, error) {

	name, refusal := repoArg(t.Name(), args)
	if refusal != nil {
		return *refusal, nil
	}
	repo, refusal := t.deps.Code.repo(ctx, turn, t.Name(), codeCaller{asker: asker}, name)
	if refusal != nil {
		return *refusal, nil
	}
	report, err := t.deps.Status.CodeStatus(ctx, repo.ID)
	if err != nil {
		return refusedBy(tools.RefusalUnavailable, err, "code_status could not ask the git nodes "+
			"just now. Try again shortly."), nil
	}
	answer := codeStatusRepo{Repo: repo.Name, ID: repo.ID, Nodes: make([]repoOnNode, 0, len(report.Statuses)),
		Coverage: report.Coverage}
	for _, s := range report.Statuses {
		on := repoOnNode{Node: s.Node, At: s.At, RepoStatus: host.RepoStatus{RepoHeat: cache.RepoHeat{ID: repo.ID}}}
		if s.Repo != nil && s.Repo.ID == repo.ID {
			on.RepoStatus = *s.Repo
		}
		answer.Nodes = append(answer.Nodes, on)
	}
	slices.SortFunc(answer.Nodes, func(a, b repoOnNode) int { return cmp.Compare(a.Node, b.Node) })
	return newForeignRead(turn, asker.Seat).page(ctx, t.Name(), answer,
		"Ask again: one repository's state on every git node does not outgrow an answer.")
}

// judge is which of the repositories entries name the asker may read, and how
// many it may not.
//
// AN OPERATOR READS EVERY REPOSITORY (ADR-0058: a person maintains every one,
// and a token reads every one), so a person's answer is judged without a read.
// A SEAT is held to its own grant, through the code tools' one resolution of a
// name ([CodeDeps.repo]'s read, at the seat's level, judged by the node that
// answers): a repository it may not read, one the name no longer names, or one
// the name names under another id — renamed since the git node counted it — is
// left out, never shown on a guess. A read that could not be made fails the
// call, unavailable, rather than leaving out what it could not judge.
func (t *codeStatus) judge(ctx context.Context, turn *turnctx.Turn, asker code.Principal,
	entries []cache.RepoHeat) (map[string]bool, int, *tools.Result) {

	readable := make(map[string]bool, len(entries))
	if asker.Kind != code.PrincipalAgent {
		for _, e := range entries {
			readable[e.ID] = true
		}
		return readable, 0, nil
	}
	names := map[string]string{} // id -> the name a node counted it by
	for _, e := range entries {
		if names[e.ID] == "" {
			names[e.ID] = e.Name
		}
	}
	ids := slices.Sorted(maps.Keys(names))
	var (
		mu      sync.Mutex
		failure error
		wg      sync.WaitGroup
		slots   = make(chan struct{}, codeStatusJudges)
	)
	for _, id := range ids {
		name := names[id]
		if name == "" || !code.ValidName(name) {
			continue
		}
		wg.Go(func() {
			slots <- struct{}{}
			defer func() { <-slots }()
			ok, err := t.readable(ctx, turn, asker, id, name)
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err != nil && failure == nil:
				failure = err
			case ok:
				readable[id] = true
			}
		})
	}
	wg.Wait()
	if failure != nil {
		r := codeFailure(ctx, t.Name(), "", failure)
		return nil, 0, &r
	}
	withheld := 0
	for _, id := range ids {
		if !readable[id] {
			withheld++
		}
	}
	return readable, withheld, nil
}

// readable reports whether asker may read the repository a git node counted as
// id by name: true where the code tools' resolution of the name admits the
// asker and names that id, false where it refuses the asker or the name names
// no repository or another one now, and an error where it could not say.
func (t *codeStatus) readable(ctx context.Context, turn *turnctx.Turn, asker code.Principal,
	id, name string) (bool, error) {

	key := codeRepoKey{asker: asker, name: name}
	if held, ok := turn.LookedUp().Load(key); ok {
		if ref, ok := held.(code.RepoRef); ok {
			return ref.ID == id, nil
		}
	}
	answer, err := t.deps.Code.Code.Read(ctx, code.ReadQuery{Kind: code.ReadRepo, Asker: asker,
		Repo: name, Fresh: seatRead})
	switch {
	case errors.Is(err, code.ErrNotPermitted), errors.Is(err, code.ErrRepoNotFound):
		return false, nil
	case err != nil:
		return false, err
	case answer.Repo == nil:
		return false, fmt.Errorf("builtin: the code host answered repository %s's read with no detail", name)
	}
	turn.LookedUp().Store(key, answer.Repo.RepoRef)
	return answer.Repo.ID == id, nil
}
