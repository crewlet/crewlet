package stream

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/statelog"
)

// Unavailable is what an `unavailable` answer says beyond its code: the state
// log's own refusal behind it where there is one, a person's sentence for it,
// and whether, and when, to ask THIS node again.
//
// `unavailable` covers two opposite facts: a node that is behind or whose
// coordination store blinked answers a few seconds later, while one holding a
// record it cannot decode, or whose log is full, answers identically however
// often asked. Folded into one code, a client polled the second kind for ever
// or reported it as a 500 FAULT. A hint of ZERO says waiting does not change
// it. [UnavailableOf] is the one reading for both transports (REST renders it
// as Retry-After and two body keys, the socket as three frame keys).
type Unavailable struct {
	// Refusal is the state log's own code — a read's refusal (`behind`,
	// `log_full`, `deferred`, …) or a write's reason — or the code of a
	// surface this node serves none of now ([Unserved]: `no_active_revision`
	// for a node that has not been handed a company, Gitlet's four),
	// and empty where none is behind the answer, such as an unreachable
	// coordination store.
	Refusal string `json:"refusal,omitempty"`

	// Detail is the condition as a person is told it, in the sentence every
	// person surface uses ([builtin.ConditionSentence]; the state log's own for a
	// refusal), whose advice agrees with RetryAfter. NEVER THE REFUSAL'S OWN
	// DETAIL ([statelog.Refused.Detail], [statelog.Unavailable.Detail]): it names
	// positions, versions, settings and the broker's words for whoever runs the
	// node. The answering surface logs it ([LogUnavailable]).
	//
	// NOT MARSHALLED FROM HERE: a socket frame carries it as the envelope's
	// `detail` ([Envelope.Detail]), the key a `bad_params` sentence travels under
	// too (two `detail` fields in one frame would shadow each other), and REST
	// reads it through [Unavailable.Fields].
	Detail string `json:"-"`

	// RetryAfter is how many whole seconds to wait before asking THIS node
	// again, by [builtin.RetryAfter]'s rule — the refusal's own derived
	// hint, [HealthInterval] where there is none — and ZERO when waiting
	// cannot change the answer: ask another node, or an operator acts.
	// Never omitted on a frame that carries it, because its zero is the
	// answer.
	RetryAfter int `json:"retry_after"`
}

// Unserved is an `unavailable` answer about a SURFACE this node serves none of
// now, not a read its state log refused, under a code of its own: what the
// surface is waiting for, in the node's words, and when, if ever, to ask THIS
// node again. REST answers `503 <code>` with the wait as Retry-After and the
// words as `detail`; a socket frame as `unavailable` whose `refusal` is the
// code, with the same words and wait.
//
// One type for every such surface, read by [UnavailableOf] for both
// transports: each was a sentinel with its own arm in each transport's
// mapping (a node with no company, [ErrNoCompany]; Gitlet not served, REST
// only), and a second sentinel is a second place the two could disagree about
// when to come back.
type Unserved struct {
	// Code is the answer's own code — a REST 503's `error` and a socket
	// frame's `refusal` — and is on the REST refusal table with its sentence.
	Code httpjson.Code

	// Detail is what this node found, written for the caller who reads it.
	Detail string

	// RetryAfter is when to ask THIS node again, ZERO where waiting clears
	// nothing — somebody acts, or another node answers.
	RetryAfter time.Duration
}

// Error is the code and the words, for a log.
func (u *Unserved) Error() string { return "stream: " + string(u.Code) + ": " + u.Detail }

// ErrNoCompany is a question only Worklet, Doclet or Chatlet answers, asked of
// a node not yet handed a company, so no such half is running. It is an
// `unavailable` answer whose refusal is `no_active_revision` and whose hint is
// the reconcile poll ([httpjson.NoActiveRevisionRetry]), on the socket as over
// REST.
//
// Read by [UnavailableOf] rather than by the REST route alone: the socket
// otherwise found no state-log refusal and sent a bare `unavailable` at the
// health tick, so one question got two answers about when to come back.
var ErrNoCompany error = &Unserved{Code: httpjson.CodeNoActiveRevision,
	Detail: httpjson.NativeHalvesNotUp, RetryAfter: httpjson.NoActiveRevisionRetry}

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
	u := Unavailable{RetryAfter: httpjson.RetrySeconds(retryAfter(err))}
	var unserved *Unserved
	var read *statelog.Refused
	var write *statelog.Unavailable
	switch {
	case errors.As(err, &unserved):
		u.Refusal, u.Detail = string(unserved.Code), unserved.Detail
		return u
	case errors.As(err, &read):
		u.Refusal = string(read.Code)
	case errors.As(err, &write):
		u.Refusal = string(write.Reason)
	}
	u.Detail, _ = builtin.ConditionSentence(err)
	return u
}

// retryAfter is the hint an `unavailable` answer about err sends: an unserved
// surface's own ([Unserved.RetryAfter]), and every other condition's
// ([builtin.RetryAfter]) over the health tick. ONE READING for the answer and
// its log line, so an unserved surface no wait clears (an apply that failed)
// is written where somebody acts on it, as the hint it was answered with says.
func retryAfter(err error) time.Duration {
	var unserved *Unserved
	if errors.As(err, &unserved) {
		return unserved.RetryAfter
	}
	return builtin.RetryAfter(err, HealthInterval)
}

// LogUnavailable writes what an `unavailable` answer's cause says for whoever
// runs the node (a state-log refusal's own detail among it) through the
// answering surface's logger under event; the answer carries only
// [Unavailable.Detail]. DEBUG for a condition that clears, since a screen
// re-asks after every hint; WARN for one no wait clears, which is somebody's
// to act on.
//
// Which is which is the answer's own hint ([builtin.RetryAfter], as
// [UnavailableOf] sends), never the state log's alone, or file storage full
// and fleet key mismatch would sit at DEBUG unread. A failure its own node
// logged already ([loggedAtSource]) is DEBUG whatever it is: WARN would log
// the same failure twice.
func LogUnavailable(ctx context.Context, logger *slog.Logger, event string, err error,
	fields ...any) {

	level := slog.LevelDebug
	if retryAfter(err) == 0 && !atSource(err) {
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
