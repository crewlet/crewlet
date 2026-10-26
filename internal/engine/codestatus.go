package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/code/host"
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
