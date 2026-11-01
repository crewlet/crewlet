package stream

import (
	"context"
	"errors"
	"log/slog"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/statelog"
)

// Unavailable is what an `unavailable` answer says beyond its code: the state
// log's own refusal behind it where there is one, a person's sentence for that
// refusal, and whether — and when — to ask THIS node again.
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

	// Detail is the condition as a person is told it: what happened and
	// what to do, in the sentence every person surface tells it in
	// ([builtin.ConditionSentence] — the state log's own for a refusal),
	// whose advice agrees with RetryAfter.
	// NEVER THE REFUSAL'S OWN DETAIL ([statelog.Refused.Detail],
	// [statelog.Unavailable.Detail]), which names positions, record
	// versions, settings and the broker's words for whoever runs the node:
	// it was this field, so a screen read "the published trim floor was
	// last read 41s ago". The surface answering the refusal logs it
	// ([LogUnavailable]).
	//
	// NOT MARSHALLED FROM HERE: a socket frame carries it as the envelope's
	// own `detail` ([Envelope.Detail]), the one key a `bad_params` refusal's
	// sentence travels under too — two `detail` fields flattened into one
	// frame would each shadow the other — and the REST envelope reads it
	// through [Unavailable.Fields].
	Detail string `json:"-"`

	// RetryAfter is how many whole seconds to wait before asking THIS node
	// again, by [builtin.RetryAfter]'s rule — the refusal's own derived
	// hint, [HealthInterval] where there is none — and ZERO when waiting
	// cannot change the answer: ask another node, or an operator acts.
	// Never omitted on a frame that carries it, because its zero is the
	// answer.
	RetryAfter int `json:"retry_after"`
}

// ErrNoCompany is a question only Worklet, Doclet or
// Chatlet answers, asked of a node that has not been handed a company yet — so no
// such half is running there. It is an `unavailable` answer whose refusal is
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
//
// THE WORDS ARE EVERY CONDITION'S, not the state log's alone: a coordination
// store that did not answer, no data node serving, a store busy or closed, a
// queue down and a deadline are `unavailable` too ([builtin.Condition] classes
// them for the query registry), and read only for a state-log refusal they
// carried no detail at all — so one item read on the act route said "No node
// could serve this right now" while the same question over REST said the
// table's fixed words and on the socket nothing. [builtin.ConditionSentence]
// is the one switch every person surface tells a condition through.
func UnavailableOf(err error) Unavailable {
	u := Unavailable{
		RetryAfter: httpjson.RetrySeconds(builtin.RetryAfter(err, HealthInterval)),
	}
	var read *statelog.Refused
	var write *statelog.Unavailable
	switch {
	case errors.Is(err, ErrNoCompany):
		u.Refusal = string(httpjson.CodeNoActiveRevision)
		u.Detail = httpjson.NativeHalvesNotUp
		u.RetryAfter = httpjson.RetrySeconds(httpjson.NoActiveRevisionRetry)
		return u
	case errors.As(err, &read):
		u.Refusal = string(read.Code)
	case errors.As(err, &write):
		u.Refusal = string(write.Reason)
	}
	u.Detail, _ = builtin.ConditionSentence(err)
	return u
}

// LogUnavailable writes what an `unavailable` answer's cause says for whoever
// runs the node — a state-log refusal's own detail among it — through the
// answering surface's own logger under event, with its own fields: the answer
// carries a person's sentence for it ([Unavailable.Detail]) and never the
// error. At DEBUG for a condition that clears, since a screen asks a question
// again after every hint for as long as a node is catching up, and a line per
// ask is noise beside the alarm that names it; at WARN for one no wait clears,
// which is somebody's to act on.
//
// WHICH IS WHICH IS THE ANSWER'S OWN HINT ([builtin.RetryAfter], the one
// [UnavailableOf] sends), never the state log's alone: read through
// [statelog.ConditionLevel], every condition that is not a state-log refusal
// (a company's file storage full, a fleet whose nodes don't hold the same
// keys) went out with no Retry-After and was written at DEBUG, where nobody
// running the node reads it.
//
// A FAILURE ITS OWN NODE HAS LOGGED ALREADY ([loggedAtSource]) is written at
// DEBUG whatever it is: the node that met it wrote it at its level with its
// text (an executor's output read on the node that owns the run), so this
// line is the trace of the answer, and at WARN it was the same failure twice,
// on two nodes or on one.
func LogUnavailable(ctx context.Context, logger *slog.Logger, event string, err error,
	fields ...any) {

	level := slog.LevelDebug
	if builtin.RetryAfter(err, HealthInterval) == 0 && !atSource(err) {
		level = slog.LevelWarn
	}
	logger.Log(ctx, level, event,
		append(fields[:len(fields):len(fields)], "error", err.Error())...)
}

// LogFailed writes a question's fault (the answer a surface gives as a 500)
// through the answering surface's logger under event: at WARN, since a fault
// is somebody's to look at, and at DEBUG where its own node has logged it
// already ([loggedAtSource]), for [LogUnavailable]'s reason.
func LogFailed(ctx context.Context, logger *slog.Logger, event string, err error,
	fields ...any) {

	level := slog.LevelWarn
	if atSource(err) {
		level = slog.LevelDebug
	}
	logger.Log(ctx, level, event,
		append(fields[:len(fields):len(fields)], "error", err)...)
}

// loggedAtSource is a failure the node that met it has logged already, with its
// class and its text, so a surface answering it writes a trace. Declared here,
// by its reader; an executor's output read is what marks one today.
type loggedAtSource interface{ LoggedAtSource() bool }

// atSource reports whether err, or anything it wraps, was logged where it was
// met.
func atSource(err error) bool {
	var logged loggedAtSource
	return errors.As(err, &logged) && logged.LoggedAtSource()
}

// Fields is the answer as a REST envelope's extra keys: the refusal where one
// is behind it, and the person's sentence wherever there is one — every
// condition has one — or nil where neither is. The hint is the response's
// Retry-After rather than a key, which is where every other 503 says it.
func (u Unavailable) Fields() httpjson.Detail {
	fields := httpjson.Detail{}
	if u.Refusal != "" {
		fields["refusal"] = u.Refusal
	}
	if u.Detail != "" {
		fields["detail"] = u.Detail
	}
	if len(fields) == 0 {
		return nil
	}
	return fields
}
