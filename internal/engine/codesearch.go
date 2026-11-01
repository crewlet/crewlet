package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/crewlet/crewlet/internal/codesearch"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/topics"
	"github.com/crewlet/crewlet/internal/statelog"
)

// THE CODE SEARCH SCATTER, wired (internal/codesearch): every git node answers
// topics.CodeSearch for the repositories a request assigns it, searching them
// through its own git reads (internal/code/query) as the request's asker, and
// any node asks it ([Engine.CodeSearch]).
//
// REGISTERED WITH THE HOST AND WITHDRAWN BEFORE IT STOPS ([Engine.buildCodeHost],
// [native.stopCodeHost]), as the status answerer is: a node that builds no
// code host answers nothing, which an asker reads as a node that did not
// answer and names in its coverage. SIGNED, unlike the status answerer: a
// node carries the request out as the asker it names, so the envelopes are
// the scatter's own signedask channel, built once from the fleet keyring
// beside the steer desk's ([Engine.codeSearchWire]).

// serveCodeSearch makes this node an answerer of topics.CodeSearch over its
// git reads, and answers what withdraws it — and, on a node with no broker,
// keeps the answerer as the one its own searches are handed to, its fleet
// being itself.
func (e *Engine) serveCodeSearch(ctx context.Context, nodeID string, reads codesearch.Reads) (queue.Unsubscribe,
	error) {
	srv, err := codesearch.NewServer(e.codeSearchWire, nodeID, reads)
	if err != nil {
		return nil, err
	}
	if e.backends == nil || e.backends.Queue == nil {
		e.codeSearchLocal.Store(srv)
		return func(context.Context) error {
			e.codeSearchLocal.CompareAndSwap(srv, nil)
			return nil
		}, nil
	}
	return e.backends.Queue.Serve(ctx, topics.CodeSearch, queue.ConcurrentAnswers, srv.Answer)
}

// errNoCodeSearch is a search asked of a node that cannot ask one: its
// runtime hosts no code, or it holds no envelopes to sign one with.
var errNoCodeSearch = errors.New("engine: this node does not serve the code host, so it searches no code")

// CodeSearch answers a code search over every git node of the fleet
// ([codesearch.Searcher.Search]): one repository through the keyed walk, more
// in one signed scatter on topics.CodeSearch. The code_search tool and route
// ask it.
func (e *Engine) CodeSearch(ctx context.Context, q codesearch.Query) (codesearch.Answer, error) {
	seam, ok := OperatorCode(e)
	if !ok || e.codeSearchWire == nil {
		return codesearch.Answer{}, errNoCodeSearch
	}
	s, err := codesearch.New(codesearch.Options{Estate: seam, Placement: e.router, Ask: e.askCodeSearch,
		Wire: e.codeSearchWire, Metrics: e.metrics, Log: log})
	if err != nil {
		return codesearch.Answer{}, err
	}
	return s.Search(ctx, q)
}

// askCodeSearch scatters one sealed request on topics.CodeSearch, or, on a
// node with no broker, hands it to this node's own answerer.
func (e *Engine) askCodeSearch(ctx context.Context, request []byte, each func([]byte) bool) error {
	if e.backends != nil && e.backends.Queue != nil {
		return e.backends.Queue.AskEach(ctx, topics.CodeSearch, request, each)
	}
	srv := e.codeSearchLocal.Load()
	if srv == nil {
		// NO ANSWERER IS NO ANSWER, as a scatter nobody serves is: every
		// repository is named in the coverage as unanswered.
		return nil
	}
	reply, err := srv.Answer(ctx, request)
	if err != nil {
		return nil
	}
	each(reply)
	return nil
}

// newCodeSearchWire builds this node's scatter envelopes over the fleet
// keyring every state-log record is signed under.
func newCodeSearchWire(ring statelog.Keyring) (*codesearch.Wire, error) {
	wire, err := codesearch.NewWire(ring)
	if err != nil {
		return nil, fmt.Errorf("engine: the code search channel's envelopes: %w", err)
	}
	return wire, nil
}
