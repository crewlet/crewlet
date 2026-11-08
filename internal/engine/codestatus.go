package engine

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// GITLET'S STATUS, ANSWERED FOR THE FLEET: a node serving Gitlet
// answers every peer's question about the fleet's Gitlet nodes on
// topics.CodeStatus with what its host says of itself (host.Server.Status) —
// its cache against its budget, the repositories it holds a copy of, and the
// ones it served most this hour and today, with their copies in service.
//
// REGISTERED WITH THE HOST AND WITHDRAWN BEFORE IT STOPS ([Engine.buildGitletHost],
// [native.stopGitletHost]), on the broker's ephemeral verbs: a node whose company
// hosts no code, or that has no gitlet role, builds no host and answers nothing,
// which an asker reads as a node serving no git — the truth — and a node
// stopping its host stops answering for it before the cache it reads goes.
// UNSIGNED, unlike the estate service and the steer desk: a node answers it
// about itself and carries nothing out for the asker (internal/signedask's own
// rule), so a forged question gets an answer nobody needed and moves nothing.
//
// AND ASKED BY ANY NODE ([Engine.CodeStatus]), whatever its roles: the
// code_status tool a granted seat or an administrator's assistant calls reads
// the Gitlet nodes off the presence leases ([Engine.GitletNodes]), asks the page of
// them it shows, and merges what came back, with the coverage the fleet's
// every fanned answer carries.
//
// A QUESTION NAMES THE NODES THAT ANSWER IT, as a scatter divides its work by
// what it names (queue.EventQueue.Serve): every Gitlet node receives every
// question, and one it does not name answers nothing, so a page of a few Gitlet
// nodes is answered by those few however large the fleet — not by all of
// them, each with a hot list the page throws away.

// codeStatusProtocol is the version of a code status question and its answer.
// An answerer refuses a question at a version it does not speak — answering
// nothing, which its asker reads as a node that did not answer — and an asker
// drops an answer at a version it does not, rather than guess at its fields.
//
// STILL 1 WITH Nodes AND WithoutHot, which are additive, each zero value the
// question every build before them asked — every node, with its hot list: an
// older answerer ignores both and answers, whatever the question names and
// with its hot list, and the asker drops a reply from a node it did not ask
// ([codeStatusReport]), so a rolling upgrade reads each page correctly, at the
// older answer's cost; a newer answerer asked by an older asker answers it
// whole, as that asker expects.
const codeStatusProtocol = 1

// codeStatusQuestion is what an asker scatters on topics.CodeStatus.
type codeStatusQuestion struct {
	Version int `json:"version"`
	// Repo is the id of a repository to report beside the node — its heat and
	// its copy in service, whether or not it is among the hottest; "" asks
	// about the node alone.
	Repo string `json:"repo,omitempty"`
	// Nodes names the Gitlet nodes that answer, by id; none named asks every
	// one. A node it does not name answers nothing ([answerCodeStatus]).
	Nodes []string `json:"nodes,omitempty"`
	// WithoutHot leaves the node's hot list out of its answer
	// (host.StatusAsk.Hot) — false, the zero value, asks for it, as every
	// question did before the field.
	WithoutHot bool `json:"without_hot,omitempty"`
}

// codeStatusAnswer is one node's answer.
type codeStatusAnswer struct {
	Version int             `json:"version"`
	Status  host.CodeStatus `json:"status"`
}

// errCodeStatusQuestion is a question this node cannot read: not JSON, or at a
// version it does not speak. Answered with nothing.
var errCodeStatusQuestion = errors.New("engine: a code status question this node cannot read")

// errCodeStatusNotAsked is a question that names Gitlet nodes and not this one:
// answered with nothing, which its asker — who did not ask this node — never
// waits for.
var errCodeStatusNotAsked = errors.New("engine: a code status question that does not name this node")

// serveCodeStatus makes this node, node, an answerer of topics.CodeStatus over
// h, and answers what withdraws it. A node with no broker answers nobody, and
// is handed a withdrawal that does nothing.
func (e *Engine) serveCodeStatus(ctx context.Context, node string, h *host.Server) (queue.Unsubscribe, error) {
	if e.backends == nil || e.backends.Queue == nil {
		// A NODE WITH NO BROKER HAS NO PEER to be asked by: the fleet it would
		// answer is itself, which reads its own host.
		return func(context.Context) error { return nil }, nil
	}
	return e.backends.Queue.Serve(ctx, topics.CodeStatus, queue.ConcurrentAnswers,
		answerCodeStatus(node, h.Status))
}

// answerCodeStatus is node's answerer of one question: its status as status
// reads it, at the version the question speaks — or nothing, before status is
// read, where the question names other nodes than this one.
func answerCodeStatus(node string, status func(host.StatusAsk) host.CodeStatus) queue.AnswerFunc {
	return func(_ context.Context, raw []byte) ([]byte, error) {
		var q codeStatusQuestion
		if err := json.Unmarshal(raw, &q); err != nil {
			return nil, fmt.Errorf("%w: %w", errCodeStatusQuestion, err)
		}
		if q.Version != codeStatusProtocol {
			return nil, fmt.Errorf("%w: version %d, and this build speaks %d", errCodeStatusQuestion,
				q.Version, codeStatusProtocol)
		}
		if len(q.Nodes) > 0 && !slices.Contains(q.Nodes, node) {
			return nil, errCodeStatusNotAsked
		}
		return json.Marshal(codeStatusAnswer{Version: codeStatusProtocol,
			Status: status(host.StatusAsk{Repo: q.Repo, Hot: !q.WithoutHot})})
	}
}

// codeStatusBudget is how long a status question waits for the Gitlet nodes:
// eventfan.FleetReadBudget, the bound every fleet read a person or a seat
// waits on is held to. Borrowed rather than chosen (ADR-0015's rule for an
// alarm, kept for a wait): a node's answer is running totals and windows in
// its memory (host.Server.Status reads no row and no disk), so a Gitlet node that
// has not answered inside the bound a store read is given is one worth naming
// in the coverage rather than waiting for.
const codeStatusBudget = eventfan.FleetReadBudget

// GitletNodes is every live Gitlet node by id: the roster code_status pages by
// (builtin.StatusAsker), read off the presence leases as every keyed git walk
// reads them ([gitletNodesOf]) — a read of the coordination store this node
// holds, no Gitlet node asked. A node with no broker is the whole of its fleet:
// itself where it serves Gitlet, and no Gitlet node where it does not.
func (e *Engine) GitletNodes(ctx context.Context) ([]string, error) {
	if e.backends == nil || e.backends.Queue == nil {
		if h, ok := e.GitletHost(); ok {
			return []string{h.Status(host.StatusAsk{}).Node}, nil
		}
		return []string{}, nil
	}
	return e.gitRoster(ctx)
}

// CodeStatus asks the Gitlet nodes ask names what each one's host says of itself
// — beside it the repository whose id is ask.Repo where that is not "", and its
// hot list where ask.Hot — and answers what came back with which of them
// answered: the code_status tool's report (builtin.StatusAsker).
//
// THE QUESTION NAMES ask.Nodes, so no other Gitlet node answers it, unless
// ask.Every says they are every Gitlet node, when it names none and stays the
// same few bytes however large the fleet. Either way the ask ends once every
// node of ask.Nodes has answered — never at a count of replies
// ([askCodeStatus]) — or at the budget, a node of ask.Nodes that did not
// answer by then named in the coverage, never dropped from it; and only those
// nodes' replies are read, whoever else answered ([codeStatusReport]). An ask
// of no node asks nothing.
//
// A NODE WITH NO BROKER asks its own host: its fleet is itself.
func (e *Engine) CodeStatus(ctx context.Context, ask builtin.CodeStatusAsk) (builtin.CodeStatusReport, error) {
	ctx, cancel := context.WithTimeout(ctx, codeStatusBudget)
	defer cancel()
	if len(ask.Nodes) == 0 {
		return builtin.CodeStatusReport{Coverage: eventfan.Coverage{
			Nodes: []eventfan.NodeCoverage{}, Complete: true}}, nil
	}
	if e.backends == nil || e.backends.Queue == nil {
		return e.ownCodeStatus(ask)
	}
	q := codeStatusQuestion{Version: codeStatusProtocol, Repo: ask.Repo, WithoutHot: !ask.Hot}
	if !ask.Every {
		q.Nodes = ask.Nodes
	}
	question, err := json.Marshal(q)
	if err != nil {
		return builtin.CodeStatusReport{}, fmt.Errorf("engine: encode a code status question: %w", err)
	}
	replies, err := askCodeStatus(ctx, e.backends.Queue.AskEach, question, ask.Nodes)
	if err != nil {
		return builtin.CodeStatusReport{}, fmt.Errorf("engine: ask the Gitlet nodes their status: %w", err)
	}
	return codeStatusReport(ask.Nodes, replies), nil
}

// askCodeStatus scatters question on topics.CodeStatus through askEach (the
// queue's AskEach) and collects the replies until every Gitlet node of asked has
// answered, or ctx is done.
//
// THE ASK ENDS ON WHO HAS ANSWERED, NOT ON HOW MANY. A node asked does not
// always answer alone — an older build answers whatever the question names,
// and a question naming every node is answered by one the roster does not
// hold too (a draining Gitlet node gives its presence back first and its host
// serves until it stops) — and a scatter's replies carry no sender, so an ask
// told to stop at as many replies as it asked nodes ended on such a node's
// answer before an asked one's, which the coverage then called silent and the
// report went without. Each reply names its node ([codeStatusAnswer]'s status).
func askCodeStatus(ctx context.Context,
	askEach func(ctx context.Context, subject string, request []byte, each func([]byte) bool) error,
	question []byte, asked []string,
) ([][]byte, error) {
	unheard := map[string]bool{}
	for _, id := range asked {
		unheard[id] = true
	}
	var replies [][]byte
	err := askEach(ctx, topics.CodeStatus, question, func(raw []byte) bool {
		replies = append(replies, raw)
		var a codeStatusAnswer
		if json.Unmarshal(raw, &a) == nil {
			delete(unheard, a.Status.Node)
		}
		return len(unheard) > 0
	})
	return replies, err
}

// codeStatusReport is what replies answer of the Gitlet nodes asked: every reply
// this build reads from one of them, one status a node, and the coverage —
// each node asked answered or named — complete only where every one of them
// answered. A reply from a node not asked is dropped: the report is about the
// nodes asked, and a page shows those alone.
func codeStatusReport(asked []string, replies [][]byte) builtin.CodeStatusReport {
	nodes := make(map[string]eventfan.NodeCoverage, len(asked))
	for _, id := range asked {
		nodes[id] = eventfan.NodeCoverage{ID: id, Error: fmt.Sprintf(
			"did not answer inside %s", codeStatusBudget)}
	}
	var statuses []host.CodeStatus
	for _, raw := range replies {
		var a codeStatusAnswer
		if err := json.Unmarshal(raw, &a); err != nil || a.Status.Node == "" {
			// A REPLY THIS BUILD CANNOT READ names nobody it could put in
			// the coverage; its node, if it was asked, stays named as not
			// answering.
			continue
		}
		prior, wasAsked := nodes[a.Status.Node]
		switch {
		case !wasAsked:
			// A NODE NOT ASKED — an older build, which answers whatever the
			// question names, or one the roster does not hold — is no part
			// of this report.
			continue
		case prior.Answered:
			// ONE STATUS A NODE, whatever its broker delivered twice — and a
			// later reply never unsays one this build already read.
			continue
		case a.Version != codeStatusProtocol:
			nodes[a.Status.Node] = eventfan.NodeCoverage{ID: a.Status.Node, Error: fmt.Sprintf(
				"answered at status protocol %d, and this build reads %d", a.Version,
				codeStatusProtocol)}
			continue
		}
		nodes[a.Status.Node] = eventfan.NodeCoverage{ID: a.Status.Node, Answered: true}
		statuses = append(statuses, a.Status)
	}
	cover := eventfan.Coverage{Nodes: make([]eventfan.NodeCoverage, 0, len(nodes)), Complete: true}
	for _, n := range nodes {
		cover.Nodes = append(cover.Nodes, n)
		if !n.Answered {
			cover.Complete = false
		}
	}
	slices.SortFunc(cover.Nodes, func(a, b eventfan.NodeCoverage) int { return cmp.Compare(a.ID, b.ID) })
	slices.SortFunc(statuses, func(a, b host.CodeStatus) int { return cmp.Compare(a.Node, b.Node) })
	return builtin.CodeStatusReport{Statuses: statuses, Coverage: cover}
}

// ownCodeStatus is the report of a node with no broker, whose fleet is itself:
// its own host's status where it serves one and ask names it, each other node
// ask names unanswered — there is no peer to ask — read off the same reply
// as a peer's answer is ([codeStatusReport]).
func (e *Engine) ownCodeStatus(ask builtin.CodeStatusAsk) (builtin.CodeStatusReport, error) {
	var replies [][]byte
	if h, ok := e.GitletHost(); ok {
		raw, err := json.Marshal(codeStatusAnswer{Version: codeStatusProtocol,
			Status: h.Status(host.StatusAsk{Repo: ask.Repo, Hot: ask.Hot})})
		if err != nil {
			return builtin.CodeStatusReport{}, fmt.Errorf("engine: encode this node's code status: %w", err)
		}
		replies = append(replies, raw)
	}
	return codeStatusReport(ask.Nodes, replies), nil
}

// gitRoster is the live Gitlet nodes, off the presence leases.
func (e *Engine) gitRoster(ctx context.Context) ([]string, error) {
	if e.backends == nil || e.backends.Coord == nil {
		return nil, fmt.Errorf("engine: this node has no coordination store to read the Gitlet nodes from")
	}
	held, err := e.backends.Coord.ListLive(ctx, coord.ClassNode)
	if err != nil {
		return nil, fmt.Errorf("engine: read the live Gitlet nodes: %w", err)
	}
	return gitletNodesOf(held), nil
}
