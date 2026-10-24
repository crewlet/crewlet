package stream

import (
	"errors"

	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/statelog"
)

// Unavailable is what an `unavailable` answer says beyond its code: the state
// log's own refusal behind it where there is one, that refusal's words, and
// whether — and when — to ask THIS node again.
//
// # Why the code alone was not enough
//
// `unavailable` covers two opposite facts. A node that is behind, or whose
// coordination store blinked, answers the same question a few seconds later;
// a node holding a record it cannot decode, evicted, or whose log is full or
// was refused by its broker answers it identically however often it is asked.
// Folded into one code, a client either polled the second kind for ever or —
// which is what the query surface did instead — the second kind was reported
// as a FAULT, a 500 `query_failed` whose remedy went only to the node's log.
// So the answer carries the refusal and its hint, and a hint of ZERO is the
// statement that waiting does not change it.
//
// ONE READING FOR BOTH TRANSPORTS ([UnavailableOf]): the REST route renders
// it as a Retry-After and two body keys, the socket as three frame keys, and
// neither decides the hint for itself.
type Unavailable struct {
	// Refusal is the state log's own code — a read's refusal (`behind`,
	// `log_full`, `deferred`, …) or a write's reason — or
	// `no_active_revision` for a node that has not been handed a company
	// ([ErrNoCompany]), and empty where neither is behind the answer, such
	// as an unreachable coordination store.
	Refusal string `json:"refusal,omitempty"`

	// Detail is that refusal's own words: what it is about and what
	// changes it. Written for a caller, which is why it may be sent where
	// a failure's text may not — a fault can carry a database path, and a
	// refusal names positions, record versions and settings. The state log
	// holds itself to that: it composes every refusal's detail and sends
	// the error behind one — a store's, a transport's — to its log
	// ([statelog.Unavailable]).
	//
	// NOT MARSHALLED FROM HERE: a socket frame carries it as the envelope's
	// own `detail` ([Envelope.Detail]), the one key a `bad_params` refusal's
	// sentence travels under too — two `detail` fields flattened into one
	// frame would each shadow the other — and the REST envelope reads it
	// through [Unavailable.Fields].
	Detail string `json:"-"`

	// RetryAfter is how many whole seconds to wait before asking THIS node
	// again, by [statelog.RetryAfter]'s rule — the refusal's own derived
	// hint, [HealthInterval] where there is none — and ZERO when waiting
	// cannot change the answer: ask another node, or an operator acts.
	// Never omitted on a frame that carries it, because its zero is the
	// answer.
	RetryAfter int `json:"retry_after"`
}

// ErrNoCompany is a question only the company's own tracker and knowledge base
// answer, asked of a node that has not been handed a company yet — so neither
// half is running there. It is an `unavailable` answer whose refusal is
// `no_active_revision` and whose hint is the reconcile poll that brings the
// company ([httpjson.NoActiveRevisionRetry]), on the socket as over REST.
//
// READ HERE, by [UnavailableOf], rather than by the REST route alone: the REST
// route answered it `503 no_active_revision` with the poll as its wait and the
// halves' words, while the socket read the same answer through this function,
// found no state-log refusal and sent a bare `unavailable` at the health
// tick's five seconds — one question, two answers about when to come back and
// only one about why, which is the disagreement this one reading exists to
// make impossible.
var ErrNoCompany = errors.New("stream: this node has not been handed a company yet")

// UnavailableOf reads an `unavailable` answer's cause.
func UnavailableOf(err error) Unavailable {
	u := Unavailable{
		RetryAfter: httpjson.RetrySeconds(statelog.RetryAfter(err, HealthInterval)),
	}
	var read *statelog.Refused
	var write *statelog.Unavailable
	switch {
	case errors.Is(err, ErrNoCompany):
		u.Refusal = string(httpjson.CodeNoActiveRevision)
		u.Detail = httpjson.NativeHalvesNotUp
		u.RetryAfter = httpjson.RetrySeconds(httpjson.NoActiveRevisionRetry)
	case errors.As(err, &read):
		u.Refusal, u.Detail = string(read.Code), read.Detail
	case errors.As(err, &write):
		u.Refusal, u.Detail = string(write.Reason), write.Detail
	}
	return u
}

// Fields is the refusal as a REST envelope's extra keys, or nil where no
// state-log refusal is behind the answer. The hint is the response's
// Retry-After rather than a key, which is where every other 503 says it.
func (u Unavailable) Fields() httpjson.Detail {
	if u.Refusal == "" {
		return nil
	}
	return httpjson.Detail{"refusal": u.Refusal, "detail": u.Detail}
}
