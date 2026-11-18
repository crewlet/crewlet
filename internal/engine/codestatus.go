package engine

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/eventfan"
	"github.com/crewlet/crewlet/internal/gitlet/host"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
)

// GITLET'S STATUS, ANSWERED FOR THE FLEET: a node serving Gitlet answers
// topics.CodeStatus with what its host says of itself (host.Server.Status),
// registered with the host and withdrawn before it stops
// ([Engine.buildGitletHost], [native.stopGitletHost]): a node with no host
// answers nothing, which an asker reads as serving no git, the truth. UNSIGNED,
// unlike the estate service: a node answers about itself and carries nothing
// out (internal/signedask's rule), so a forged question moves nothing.
//
// Any node asks ([Engine.CodeStatus]) the page of Gitlet nodes it shows, off
// the presence leases ([Engine.GitletNodes]). A question names the nodes that
// answer it (queue.EventQueue.Serve): one it does not name answers nothing, so
// a page costs those few nodes however large the fleet.

// codeStatusProtocol is the version of a code status question and its answer.
// An answerer answers nothing at a version it does not speak (its asker reads a
// node that did not answer), and an asker drops an answer at one it does not.
//
// STILL 1 WITH Nodes AND WithoutHot: both are additive, each zero value the
// older question (every node, with its hot list). An older answerer ignores
// both, and the asker drops a reply from a node it did not ask
// ([codeStatusReport]), so a rolling upgrade reads each page correctly.
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
	// (host.StatusAsk.Hot); false, the zero value, asks for it.
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
// eventfan.FleetReadBudget, borrowed rather than chosen (ADR-0015). An answer
// is in-memory totals (host.Server.Status reads no row and no disk), so a node
// silent past a store read's bound is named in the coverage, not waited for.
const codeStatusBudget = eventfan.FleetReadBudget

// GitletNodes is every live Gitlet node by id, the roster code_status pages by
// (builtin.StatusAsker), read off the presence leases ([gitletNodesOf]) with no
// Gitlet node asked. A node with no broker is its whole fleet: itself where it
// serves Gitlet, none where it does not.
func (e *Engine) GitletNodes(ctx context.Context) ([]string, error) {
	if e.backends == nil || e.backends.Queue == nil {
		if h, ok := e.GitletHost(); ok {
			return []string{h.Status(host.StatusAsk{}).Node}, nil
		}
		return []string{}, nil
	}
	return e.gitRoster(ctx)
}

// CodeStatus asks the Gitlet nodes ask.Nodes names for their host's status,
// with repository ask.Repo beside it where not "" and the hot list where
// ask.Hot: the code_status tool's report (builtin.StatusAsker). The question
// names ask.Nodes unless ask.Every, so it stays a few bytes for the whole
// fleet. The ask ends once every node asked has answered ([askCodeStatus]) or
// at the budget, a silent node named in the coverage; only the asked nodes'
// replies are read ([codeStatusReport]). An ask of no node asks nothing, and a
// node with no broker asks its own host.
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
// queue's AskEach) and collects the replies until every node of asked has
// answered, or ctx is done.
//
// IT ENDS ON WHO HAS ANSWERED, NOT HOW MANY: a node not asked may answer too
// (an older build answers whatever the question names, and a draining node off
// the roster serves until it stops), so a count of replies could end before an
// asked node's answer. A reply names its node only in [codeStatusAnswer]'s
// status, since a scatter's replies carry no sender.
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

// codeStatusReport is what replies answer of the nodes asked: one status a
// node, and the coverage, complete only where every asked node answered. A
// reply from a node not asked is dropped, since a page shows the asked alone.
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
			// Unreadable: its node, if asked, stays named as not answering.
			continue
		}
		prior, wasAsked := nodes[a.Status.Node]
		switch {
		case !wasAsked:
			continue
		case prior.Answered:
			// ONE STATUS A NODE: a duplicate never unsays one already read.
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
// its own host's status where it serves one, every other node ask names
// unanswered, read off the same reply a peer's answer is ([codeStatusReport]).
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
