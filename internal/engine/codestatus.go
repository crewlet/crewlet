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

// THE CODE HOST'S STATUS, ANSWERED FOR THE FLEET: a node serving the code host
// answers every peer's question about the fleet's git nodes on
// topics.CodeStatus with what its host says of itself (host.Server.Status) —
// its cache against its budget, the repositories it holds a copy of, and the
// ones it served most this hour and today, with their copies in service.
//
// REGISTERED WITH THE HOST AND WITHDRAWN BEFORE IT STOPS ([Engine.buildCodeHost],
// [native.stopCodeHost]), on the broker's ephemeral verbs: a node whose company
// hosts no code, or that has no git role, builds no host and answers nothing,
// which an asker reads as a node serving no git — the truth — and a node
// stopping its host stops answering for it before the cache it reads goes.
// UNSIGNED, unlike the estate service and the steer desk: a node answers it
// about itself and carries nothing out for the asker (internal/signedask's own
// rule), so a forged question gets an answer nobody needed and moves nothing.
//
// AND ASKED BY ANY NODE ([Engine.CodeStatus]), whatever its roles: the
// code_status tool a granted seat or an administrator's assistant calls
// scatters the question and merges what came back, with the coverage the
// fleet's every fanned answer carries.

// codeStatusProtocol is the version of a code status question and its answer.
// An answerer refuses a question at a version it does not speak — answering
// nothing, which its asker reads as a node that did not answer — and an asker
// drops an answer at a version it does not, rather than guess at its fields.
const codeStatusProtocol = 1

// codeStatusQuestion is what an asker scatters on topics.CodeStatus.
type codeStatusQuestion struct {
	Version int `json:"version"`
	// Repo is the id of a repository to report beside the node — its heat and
	// its copy in service, whether or not it is among the hottest; "" asks
	// about the node alone.
	Repo string `json:"repo,omitempty"`
}

// codeStatusAnswer is one node's answer.
type codeStatusAnswer struct {
	Version int             `json:"version"`
	Status  host.CodeStatus `json:"status"`
}

// errCodeStatusQuestion is a question this node cannot read: not JSON, or at a
// version it does not speak. Answered with nothing.
var errCodeStatusQuestion = errors.New("engine: a code status question this node cannot read")

// serveCodeStatus makes this node an answerer of topics.CodeStatus over h, and
// answers what withdraws it. A node with no broker answers nobody, and is
// handed a withdrawal that does nothing.
func (e *Engine) serveCodeStatus(ctx context.Context, h *host.Server) (queue.Unsubscribe, error) {
	if e.backends == nil || e.backends.Queue == nil {
		// A NODE WITH NO BROKER HAS NO PEER to be asked by: the fleet it would
		// answer is itself, which reads its own host.
		return func(context.Context) error { return nil }, nil
	}
	return e.backends.Queue.Serve(ctx, topics.CodeStatus, queue.ConcurrentAnswers, answerCodeStatus(h))
}

// answerCodeStatus is the answerer of one question: h's status, at the
// version the question speaks.
func answerCodeStatus(h *host.Server) queue.AnswerFunc {
	return func(_ context.Context, raw []byte) ([]byte, error) {
		var q codeStatusQuestion
		if err := json.Unmarshal(raw, &q); err != nil {
			return nil, fmt.Errorf("%w: %w", errCodeStatusQuestion, err)
		}
		if q.Version != codeStatusProtocol {
			return nil, fmt.Errorf("%w: version %d, and this build speaks %d", errCodeStatusQuestion,
				q.Version, codeStatusProtocol)
		}
		return json.Marshal(codeStatusAnswer{Version: codeStatusProtocol, Status: h.Status(q.Repo)})
	}
}

// codeStatusBudget is how long a status question waits for the git nodes:
// eventfan.FleetReadBudget, the bound every fleet read a person or a seat
// waits on is held to. Borrowed rather than chosen (ADR-0015's rule for an
// alarm, kept for a wait): a node's answer is running totals and windows in
// its memory (host.Server.Status reads no row and no disk), so a git node that
// has not answered inside the bound a store read is given is one worth naming
// in the coverage rather than waiting for.
const codeStatusBudget = eventfan.FleetReadBudget

// CodeStatus asks every git node of the fleet what its code host says of
// itself — and of the repository whose id is repoID beside it, where repoID is
// not "" — and answers what came back with which of the git nodes it asked
// answered: the code_status tool's report (builtin.StatusAsker).
//
// THE GIT NODES ARE THE ROSTER, read off the presence leases as every keyed
// git walk reads them (gitNodesOf): a git node that did not answer inside the
// budget is named in the coverage, never dropped from it, an answer from a
// node the roster did not list (one that joined since, or one draining) is
// kept, and the ask ends once every node the roster names has answered —
// never at a count of replies ([askCodeStatus]). A roster
// that could not be read leaves the coverage incomplete — nobody can say who
// was missing — and the ask waits out its budget for whoever answers. A fleet
// with no git node is asked nothing.
//
// A NODE WITH NO BROKER asks its own host: its fleet is itself.
func (e *Engine) CodeStatus(ctx context.Context, repoID string) (builtin.CodeStatusReport, error) {
	ctx, cancel := context.WithTimeout(ctx, codeStatusBudget)
	defer cancel()
	if e.backends == nil || e.backends.Queue == nil {
		return e.ownCodeStatus(repoID), nil
	}
	roster, rosterErr := e.gitRoster(ctx)
	if rosterErr == nil && len(roster) == 0 {
		return builtin.CodeStatusReport{Coverage: eventfan.Coverage{
			Nodes: []eventfan.NodeCoverage{}, Complete: true}}, nil
	}
	question, err := json.Marshal(codeStatusQuestion{Version: codeStatusProtocol, Repo: repoID})
	if err != nil {
		return builtin.CodeStatusReport{}, fmt.Errorf("engine: encode a code status question: %w", err)
	}
	replies, err := askCodeStatus(ctx, e.backends.Queue.AskEach, question, roster, rosterErr)
	if err != nil {
		return builtin.CodeStatusReport{}, fmt.Errorf("engine: ask the git nodes their status: %w", err)
	}
	return codeStatusReport(roster, rosterErr, replies), nil
}

// askCodeStatus scatters question on topics.CodeStatus through askEach (the
// queue's AskEach) and collects the replies until every git node the roster
// names has answered, or ctx is done; with no roster (rosterErr), until ctx
// is done.
//
// THE ASK ENDS ON WHO HAS ANSWERED, NOT ON HOW MANY. Every git node's host
// serves the question, one the roster does not hold among them — a draining
// git node gives its presence back first and its host serves until it stops,
// and one may have joined since the roster was read — and a scatter's replies
// carry no sender, so an ask told to stop at as many replies as the roster
// names ended on such a node's answer before a named one's, which the
// coverage then called silent for the whole budget and the report went
// without. Each reply names its node ([codeStatusAnswer]'s status).
func askCodeStatus(ctx context.Context,
	askEach func(ctx context.Context, subject string, request []byte, each func([]byte) bool) error,
	question []byte, roster []string, rosterErr error,
) ([][]byte, error) {
	unheard := map[string]bool{}
	for _, id := range roster {
		unheard[id] = true
	}
	var replies [][]byte
	err := askEach(ctx, topics.CodeStatus, question, func(raw []byte) bool {
		replies = append(replies, raw)
		var a codeStatusAnswer
		if json.Unmarshal(raw, &a) == nil {
			delete(unheard, a.Status.Node)
		}
		return rosterErr != nil || len(unheard) > 0
	})
	return replies, err
}

// codeStatusReport is what replies answer, against the roster of git nodes the
// asker read (or why it read none): every reply this build reads, one status a
// node, and the coverage — each git node of the roster answered or named, a
// node that answered outside the roster answered, and complete only where the
// roster was read and every node on it answered.
func codeStatusReport(roster []string, rosterErr error, replies [][]byte) builtin.CodeStatusReport {
	nodes := map[string]eventfan.NodeCoverage{}
	for _, id := range roster {
		nodes[id] = eventfan.NodeCoverage{ID: id, Error: fmt.Sprintf(
			"did not answer inside %s", codeStatusBudget)}
	}
	var statuses []host.CodeStatus
	for _, raw := range replies {
		var a codeStatusAnswer
		if err := json.Unmarshal(raw, &a); err != nil || a.Status.Node == "" {
			// A REPLY THIS BUILD CANNOT READ names nobody it could put in
			// the coverage; its node, if the roster lists it, stays named
			// as not answering.
			continue
		}
		if prior, seen := nodes[a.Status.Node]; seen && prior.Answered {
			// ONE STATUS A NODE, whatever its broker delivered twice — and a
			// later reply never unsays one this build already read.
			continue
		}
		if a.Version != codeStatusProtocol {
			nodes[a.Status.Node] = eventfan.NodeCoverage{ID: a.Status.Node, Error: fmt.Sprintf(
				"answered at status protocol %d, and this build reads %d", a.Version,
				codeStatusProtocol)}
			continue
		}
		nodes[a.Status.Node] = eventfan.NodeCoverage{ID: a.Status.Node, Answered: true}
		statuses = append(statuses, a.Status)
	}
	cover := eventfan.Coverage{Nodes: make([]eventfan.NodeCoverage, 0, len(nodes)),
		Complete: rosterErr == nil}
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

// ownCodeStatus is the report of a node with no broker: its own host's status,
// where it serves one, and itself as the whole of the fleet.
func (e *Engine) ownCodeStatus(repoID string) builtin.CodeStatusReport {
	h, ok := e.CodeHost()
	if !ok {
		return builtin.CodeStatusReport{Coverage: eventfan.Coverage{
			Nodes: []eventfan.NodeCoverage{}, Complete: true}}
	}
	s := h.Status(repoID)
	return builtin.CodeStatusReport{Statuses: []host.CodeStatus{s}, Coverage: eventfan.Coverage{
		Nodes: []eventfan.NodeCoverage{{ID: s.Node, Answered: true}}, Complete: true}}
}

// gitRoster is the live git nodes, off the presence leases.
func (e *Engine) gitRoster(ctx context.Context) ([]string, error) {
	if e.backends == nil || e.backends.Coord == nil {
		return nil, fmt.Errorf("engine: this node has no coordination store to read the git nodes from")
	}
	held, err := e.backends.Coord.ListLive(ctx, coord.ClassNode)
	if err != nil {
		return nil, fmt.Errorf("engine: read the live git nodes: %w", err)
	}
	return gitNodesOf(held), nil
}
