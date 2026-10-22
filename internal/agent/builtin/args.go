package builtin

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tools"
	"github.com/crewlet/crewlet/internal/tools/argval"
	"github.com/crewlet/crewlet/internal/worklet"
)

// The shared argument and output helpers.
//
// Arguments arrive from a MODEL, so every read here treats a missing, wrongly
// typed or absurdly long value as an ordinary case rather than an error: the
// tool's job is to tell the model what it should have sent, in a message the
// model can act on.

// argString reads a string argument, tolerating a number or bool the model
// sent where a string belongs — which they do, and refusing it teaches
// nothing.
func argString(args map[string]any, key string) string {
	s, _ := argval.String(args[key])
	return s
}

// argInt reads an integer argument, or the fallback when it is absent.
//
// THE CHECK'S OWN READER ([argval.Whole]), so a value the check at the surface
// admits is the value read here, and one it refuses never reaches the tool.
// The fallback is for an absent argument; a present one the reader cannot
// read was refused before the tool ran (ADR-0059).
func argInt(args map[string]any, key string, fallback int) int {
	if n, ok := argval.Whole(args[key]); ok {
		return n
	}
	return fallback
}

// argStrings reads a list-of-strings argument, tolerating the single string a
// model sends where a list belongs — which they do, constantly, and refusing
// it costs a round to teach nothing.
func argStrings(args map[string]any, key string) []string {
	list, _ := argval.Strings(args[key])
	return list
}

// argBool reads a boolean argument, false when absent or not one.
//
// ABSENT AND FALSE ARE THE SAME ANSWER here, unlike in the query grammar,
// because every caller of this one is a flag a tool sets rather than a filter
// with three states: "do not protect this view" and "say nothing about
// protecting it" are the same instruction.
func argBool(args map[string]any, key string) bool {
	b, _ := argval.Bool(args[key])
	return b
}

// argFloat reads a numeric argument, zero when absent or not one.
//
// ZERO RATHER THAN A REFUSAL, because every caller of this one has a field
// whose zero IS its default — a target that starts at zero, a current nobody
// has moved. A field where zero were a distinct setting would take a pointer,
// which is the rule the config models follow for the same reason.
//
// THE CHECK'S OWN READER ([argval.Number]), as for [argInt].
func argFloat(args map[string]any, key string) float64 {
	f, _ := argval.Number(args[key])
	return f
}

// argStringMap reads an object argument whose values are strings.
//
// A NON-STRING VALUE IS RENDERED rather than dropped, because a model writing
// `{"limit": 10}` means the same thing as `{"limit": "10"}` and the grammar
// these maps feed reads both identically — see the API's own parameter bag.
// Dropping it
// would save a saved view with a filter the caller believes is in it.
func argStringMap(args map[string]any, key string) map[string]string {
	raw, ok := args[key].(map[string]any)
	if !ok || len(raw) == 0 {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		if k = strings.TrimSpace(k); k == "" || v == nil {
			continue
		}
		// BY THE CHECK'S OWN READER ([argval.String]), which renders a
		// number or a boolean as written and reads nothing from an object
		// or a list (refused at the surface before the tool runs).
		if s, ok := argval.String(v); ok {
			out[k] = s
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// refused is a tool result the model can act on, carrying the class a reader
// that is not a model acts on instead.
//
// Failed rather than an error: the turn is fine, this call is not, and the
// difference is what lets the model try again with a better argument instead
// of the loop tearing down.
//
// THE CLASS IS AN ARGUMENT, never inferred from the sentence, because the
// sentence is prompt text tuned against model behaviour and rewording it must
// not move a person's surface to a different status. It is the result's
// CAUSE ([tools.Refusal] is an error), which is the one place a reader asks
// ([tools.RefusalOf]): a class beside the cause would be a second classifier
// of one failure, free to tell the audit one story and the write surface
// another.
func refused(code tools.Refusal, msg string) tools.Result {
	return tools.Result{Output: msg, Failed: true, Cause: code}
}

// failed is the argument refusal — [tools.RefusalInvalid] — which is what the
// great majority of these are: a missing, malformed or unknown argument whose
// sentence names the field to change. Anything else says its class with
// [refused] or [refusedBy].
func failed(msg string) tools.Result { return refused(tools.RefusalInvalid, msg) }

// refusedBy is [refused] for a refusal something underneath decided — a read
// that found nothing, a write the log refused — carrying that error BENEATH
// the class ([tools.Classify]), so a caller branching on the domain's own
// sentinel and a reader asking for the class read one value.
//
// THE MESSAGE IS STILL THE WHOLE ANSWER to a model. What the cause buys is
// that a caller answering in STATUS CODES can tell an item that does not exist
// from a version somebody moved from a node that could not decide, without
// parsing a sentence written for a model — and the class is the tool's
// statement about THIS call, which is why it is an argument: an item that does
// not exist is not-found when the call is about it and an invalid argument
// when the call merely named it.
//
// AND THE PERSON'S WORDS ARE THE CAUSE'S (ADR-0060): a writer that refused
// with a person's phrasing beside its error ([caveat.Saying]) has it lifted
// onto the result here, the one place a refusal with a cause is made, so no
// tool answering that error has to remember to carry it.
func refusedBy(class tools.Refusal, cause error, msg string) tools.Result {
	return tools.Result{Output: msg, Failed: true, Cause: tools.Classify(class, cause),
		PersonRefusal: caveat.RefusalOf(cause)}
}

// toPerson is a refusal with a person's phrasing of it ([caveat.Refusal]),
// for a refusal the tool itself decided: the model's sentence is untouched,
// and the act route answers the phrasing in its place (ADR-0060).
func toPerson(r tools.Result, words caveat.Refusal) tools.Result {
	r.PersonRefusal = words
	return r
}

// Condition says whether a failure no sentinel of the caller's claimed is a
// CONDITION of this node — one WAITING can clear — and the words a caller may
// be told it in; ok is false for a FAULT of the node, which waiting does not
// clear and whose words are the log's ([faulted]).
//
// # The conditions
//
// A refusal or an outcome the state log composed ([statelog.ErrUnavailable]:
// a node behind its log, below its floor, holding a record it cannot decode, a
// log that refused the append); a coordination store that did not answer
// ([coord.ErrUnavailable]); the estate's router finding no data node to run
// the call on ([estate.ErrUnserved]) — every node holding the company's
// records answered that its copy is out of service or behind, or, for a read,
// did not answer at all; an estate held closed while a peer's snapshot is adopted
// ([store.ErrNoEstate]); a store whose write lock the writers ahead of this one
// held for longer than the write would wait ([store.ErrBusy]); an event queue
// that is not live while this node starts or stops ([queue.ErrNotLive]); an
// event broker this node could not reach — its connection reconnecting, or the
// broker not answering in time ([queue.ErrUnavailable]); a wait that ran out of
// time or was abandoned. Everything else that reaches a tool unmarked — a read
// of this node's own store that broke, a database that refused a statement, a
// path in its own code that went wrong — is a fault.
//
// EACH IS A SENTINEL ITS OWN PACKAGE MARKS, never a reading of an error's
// words: the store marks only the lock it never took, because a "database is
// locked" a statement met for any other reason is not a wait, and the queue
// marks only what its broker did not answer, because "connection closed" from a
// client closed for good is ErrNotLive and a broker's own refusal is neither.
// A classifier matching text here would call a disk error with "locked" in its
// path a condition.
//
// # Why the line is drawn here
//
// Because the two send a caller opposite ways. [tools.RefusalUnavailable] is
// answered `503` with a `Retry-After`, and a model is told to try again —
// right for a node behind its log, which catches up, and a lie about a store
// that cannot be read, which a client polled every two seconds until somebody
// fixed the node. So every classifier that called each unmarked failure
// "unavailable" asks this first, and classes the rest
// [tools.RefusalInternalError].
//
// A STATE-LOG REFUSAL STAYS A CONDITION EVEN WHERE WAITING CANNOT CLEAR IT —
// an evicted node, a record this node cannot decode — because the refusal says
// so itself: [statelog.RetryAfter] answers zero for those, and a surface
// answers them `503` with no `Retry-After`. A fault is what NOTHING classified.
//
// AND AN ESTATE NOBODY SERVED IS A CONDITION OF THE FLEET, not a fault of this
// node: every read and write a seat or a person makes of the tracker and the
// knowledge base goes through the estate's router, which answers
// [estate.ErrUnserved] once every data node it asked ran nothing — a copy out
// of service, a node restarting, a peer that did not answer. None of that is
// broken code, and all of it clears when a data node serves again. It wraps no
// sentinel this rule already knew, so until it was named here every such call
// was answered `500` and told the caller sending it again would not help.
// What the router COULD NOT ACCOUNT FOR is a different answer, never this one:
// a write it sent and never heard back about — from the one node a write with
// no key of its caller's goes to, or from every node a repeatable write was
// asked of ([estate.ErrOutcomeUnknown]) — may have landed, and is an unknown
// outcome ([UnknownCause]), told in [Unanswered].
//
// # Two readers, and the words each is told
//
// A condition reaches a MODEL inside a tool's sentence ("Could not read your
// notes (…). Try again shortly."), and a PERSON on a screen, through every
// surface that answers one: the operator's, the human write surface, the
// identity directory, the artifacts. So each condition is told twice, from one
// classification ([condition]), and the two can never cover different sets:
// [Condition] is the model's clause and [ConditionSentence] the person's
// whole sentence, what happened and what to do, in the short voice a screen
// is written in.
//
// NEITHER IS EVER THE ERROR'S OWN TEXT. A state-log refusal is told in the
// state log's own words for it ([statelog.ConditionWords]), one per code: its
// error opens with the framework's name and its reason's spelling, and its
// detail ([statelog.Unavailable.Detail], [statelog.Refused.Detail]) names
// positions, settings and the broker's words, written for whoever runs the
// node — so it goes to the node's log, where the surface answering it puts
// it, and to nobody's prompt or screen. It used to be handed whole to both,
// so a model read "statelog: unavailable (behind): …" and a person read the
// same under "this node could not read what the decision is about". Every
// other condition is told in fixed words: a coordination backend wraps its
// transport's error, which names hosts and ports, a queue's names its broker,
// and the router's names every node it asked and what each said — which it
// logs where it concludes it.
//
// WHAT TO DO IS THE CONDITION'S TO SAY where waiting does not clear it
// ([conditionNext]): a tool's sentence told a model to try again shortly
// whatever the condition was, so a full log or an evicted node was retried for
// as long as the turn lasted.
//
// EXPORTED for the human write surface, whose tool-less writers class their
// writers' errors by this same rule (internal/api/operator's ClassifyWrite).
func Condition(err error) (words string, ok bool) {
	said, ok := condition(err)
	return said.clause, ok
}

// ConditionSentence is the condition err is, as a PERSON is told it: what
// happened and what to do, with nothing of the error's own text — see
// [Condition], which is the same classification told to a model. False for a
// fault, and for anything else no condition claims.
func ConditionSentence(err error) (string, bool) {
	said, ok := condition(err)
	return said.person, ok
}

// conditionSaid is one condition as each of its two readers is told it.
type conditionSaid struct {
	// clause is what happened, a clause a tool's sentence carries, for a
	// model.
	clause string

	// person is what happened and what to do, a whole sentence, for a
	// person.
	person string

	// stuck is a condition no wait clears ([statelog.RetryAfter] is zero):
	// a tool then says so in place of its own invitation to try again, in
	// words of a model's own ([stuckRead], [stuckWrite]) — never the
	// person's advice beside [statelog.ConditionSentence], which sends them
	// to another node or an administrator, neither of which a model can
	// reach.
	stuck bool

	// after is the soonest a wait can clear it, where the condition knows
	// one the state log's rule does not ([RetryAfter]); zero leaves the
	// hint to that rule.
	after time.Duration
}

// RetryAfter is how long somebody told about err should wait before asking
// again: NONE for a condition no wait clears ([condition]'s stuck — a log at
// its ceiling, a company whose file storage is full), the condition's own
// soonest where it knows one, and otherwise [statelog.RetryAfter]'s reading of
// err, with otherwise as this node's hint where nothing derived one.
//
// THE ONE HINT every surface answering a refusal or a condition in status
// codes takes, BESIDE [ConditionSentence], so the words and the Retry-After
// come from one classification: read from the state log's rule alone, a full
// file store was told "an administrator needs to make room" under a
// Retry-After of two seconds on the operator's surface, and an object store
// that did not answer was told an election's wait on one surface and none on
// the next.
func RetryAfter(err error, otherwise time.Duration) time.Duration {
	if said, ok := condition(err); ok {
		switch {
		case said.stuck:
			return 0
		case said.after > 0:
			return said.after
		}
	}
	return statelog.RetryAfter(err, otherwise)
}

// condition is the one classification [Condition] and [ConditionSentence]
// read: the condition err is, or false for a fault.
func condition(err error) (conditionSaid, bool) {
	switch {
	case err == nil:
		return conditionSaid{}, false
	case errors.Is(err, statelog.ErrUnavailable):
		return stateLogSaid(err), true
	case errors.Is(err, objstore.ErrFull):
		// THE DEPLOYMENT'S FILE STORAGE AT ITS RESERVATION, which no wait
		// clears and nobody uploading can change: somebody has to make
		// room ([objstore.FullWords]).
		return conditionSaid{clause: "the company's file storage is full",
			person: objstore.FullWords, stuck: true}, true
	case errors.Is(err, objstore.ErrUnanswered):
		// THE FILE STORE DID NOT TAKE THE BYTES, which clears when it
		// answers again: the default store is the data nodes' own bucket,
		// which answers no sooner than its group elects.
		return conditionSaid{clause: "the company's file storage did not answer",
			person: "The company's files couldn't be saved just now. " +
				statelog.TryAgainSoon, after: statelog.ElectionRetryHint}, true
	case errors.Is(err, coord.ErrUnavailable):
		return conditionSaid{clause: "the coordination store did not answer",
			person: "This node couldn't reach the rest of the fleet. " +
				statelog.TryAgainSoon}, true
	case errors.As(err, new(*estate.ErrUnserved)):
		return conditionSaid{clause: "no data node could serve it right now — " +
			"every node holding the company's records answered that its copy is " +
			"out of service or behind, or, asked to read, did not answer at all",
			person: "No node could serve this right now. " + statelog.TryAgainSoon}, true
	case errors.Is(err, store.ErrNoEstate):
		return conditionSaid{clause: "this node's store is closed while it adopts " +
			"a peer's snapshot or shuts down",
			person: "This node is catching up from another node or shutting " +
				"down. " + statelog.TryAgainSoon}, true
	case errors.Is(err, store.ErrBusy):
		return conditionSaid{clause: "this node's store was busy with other writes " +
			"and could not take this one in time",
			person: "This node was too busy to take this in time. " +
				statelog.TryAgainSoon}, true
	case errors.Is(err, queue.ErrNotLive):
		return conditionSaid{clause: "this node's event queue is not running — it " +
			"is starting or shutting down",
			person: "This node is starting up or shutting down. " +
				statelog.TryAgainSoon}, true
	case errors.Is(err, queue.ErrUnavailable):
		return conditionSaid{clause: "this node could not reach its event broker — " +
			"the connection is reconnecting, or the broker did not answer in time",
			person: "This node lost touch with the rest of the fleet. " +
				statelog.TryAgainSoon}, true
	case errors.Is(err, context.DeadlineExceeded):
		return conditionSaid{clause: "it ran out of time",
			person: "This took too long, so it stopped. " + statelog.TryAgainSoon}, true
	case errors.Is(err, context.Canceled):
		return conditionSaid{clause: "it was abandoned before it finished",
			person: "This stopped before it finished. " + statelog.TryAgainSoon}, true
	}
	return conditionSaid{}, false
}

// stateLogSaid is how a state-log refusal reads: the refusal read out of the
// chain — a read refused ([statelog.Refused]), a write ([statelog.Unavailable])
// or a bare sentinel somebody wrapped — in the state log's own words for it
// ([statelog.ConditionWords]), and never its error or its detail, which are
// the log's.
//
// THE REFUSAL ITSELF, never the chain's whole message: a wrapper is free to
// join an error of its own beside it (`%w: %w`), and that one may be a
// driver's.
func stateLogSaid(err error) conditionSaid {
	what, _, _ := statelog.ConditionWords(err)
	person, _ := statelog.ConditionSentence(err)
	return conditionSaid{clause: what, person: person,
		stuck: statelog.RetryAfter(err, time.Second) == 0}
}

// What a model is told about a condition no wait clears, in place of a tool's
// invitation to try again: that trying again does not help, and what to say
// instead — the one thing a model can do about a full log or an evicted node,
// since it neither chooses the node a call goes to nor has an administrator to
// ask.
const (
	// stuckRead is a read's: it says it could not check, as every read
	// failure's sentence asks.
	stuckRead = "Trying again won't clear this, so say you could not check, " +
		"and why."

	// stuckWrite is a write's, or a call's that may be either.
	stuckWrite = "Trying again won't clear this, so say it could not be done, " +
		"and why."
)

// conditionNext is what a tool's sentence about a condition advises: soon, the
// tool's own invitation to try again, where waiting clears it — and where it
// does not, stuck ([stuckRead] or [stuckWrite]), since a model told to try
// again shortly retries a full log or an evicted node for as long as its turn
// lasts.
func conditionNext(err error, soon, stuck string) string {
	if said, ok := condition(err); ok && said.stuck {
		return stuck
	}
	return soon
}

// rightNow is " right now" for a condition waiting clears and nothing for one
// it does not, so a sentence saying a tool could not do something "right now"
// does not promise a wait its own advice rules out.
func rightNow(err error) string {
	if said, ok := condition(err); ok && said.stuck {
		return ""
	}
	return " right now"
}

// faultSaid is the clause a FAULT is told in, wherever a sentence names one:
// whose it is, that trying again does not clear it, and where its reason is.
// One clause for every tool, because a model reading "try again" in one tool's
// sentence and "do not" in another's about the same broken store learns
// nothing it can use from either.
const faultSaid = "a fault in this node itself — not anything about this call, " +
	"and not something trying again clears; the node's log has the details"

// faulted is the failed result of a call that met a FAULT of this node
// ([Condition] answered no): [tools.RefusalInternalError] with the error
// beneath it, under sentence — words that must NOT carry the error, and that
// name the fault with [faultSaid].
//
// THE ERROR GOES TO THE LOG, here and only here, with the tool's name and the
// trace the call's context carries: a store's error is a driver's message, a
// SQL fragment or a database path, and the sentence reaches a model's prompt,
// an operator's assistant and a person's screen. None of them can act on it,
// and the one reader who can — whoever runs the node — reads the log. It rides
// beneath the class for errors.Is, never for display: a surface answering the
// class says only that the engine broke ([tools.RefusalInternalError]).
func faulted(ctx context.Context, tool string, err error, sentence string) tools.Result {
	log.ErrorContext(ctx, "builtin_tool_fault", "tool", tool, "error", err.Error())
	return refusedBy(tools.RefusalInternalError, err, sentence)
}

// toldOf is how a failed result's reason reads inside a sentence that names
// what it stopped: [faultSaid] for a fault, whose error is the log's;
// [Unanswered] for a write no data node answered, whose error names every node
// it was sent to; a refusal a native domain or the framework decided, in the
// deciding one's words ([WriterSentence]); a condition's words ([Condition]);
// and otherwise the error's own.
//
// THE UNANSWERED WRITE GOES BEFORE THE CONDITION, because one whose caller's
// context ended while it waited carries that context's error too, and "it ran
// out of time" read beside "did not finish" says the write did not land.
//
// AND THE DECIDED REFUSAL GOES BEFORE IT TOO: a write about what a deletion
// keeps deleted is a refusal the framework states as one of its conditions, so
// asked second it was told as the framework's whole text, the deleted object's
// broker subject included, and the domain's own refusals as their Go error,
// package name and all.
func toldOf(failure tools.Result, err error) string {
	if tools.RefusalOf(failure) == tools.RefusalInternalError {
		return faultSaid
	}
	if errors.Is(err, estate.ErrOutcomeUnknown) {
		return Unanswered
	}
	if said, ok := WriterSentence(err); ok {
		return said
	}
	if why, ok := Condition(err); ok {
		return why
	}
	return err.Error()
}

// nodeFailure is the failed result of a call this node could not serve for a
// reason of its OWN — a store, the coordination plane, the queue — that no
// domain sentinel claimed: stopped is the clause naming what could not be done
// ("Could not read your notes"), and fact what is true either way ("Nothing
// was changed."), or empty.
//
// THE ONE RULE [readFailure] AND [writeFailure] FOLLOW, for the tools that
// have no domain of their own to classify by: a CONDITION ([Condition]) is
// [tools.RefusalUnavailable] in its own words and invites the retry that
// clears it, and anything else is a FAULT ([faulted]) — its error in the log,
// never in the sentence, and no retry invited. These tools used to answer
// every failure `unavailable` carrying the error's own text, which put a
// database path into a model's prompt and told a client to poll a store that
// could not be read.
func nodeFailure(ctx context.Context, tool string, err error, stopped, fact string) tools.Result {
	if why, ok := Condition(err); ok {
		return refusedBy(tools.RefusalUnavailable, err,
			sentence(stopped+" ("+why+").", fact, conditionNext(err, "Try again shortly.",
				stuckWrite)))
	}
	return faulted(ctx, tool, err, sentence(stopped+": "+faultSaid+".", fact))
}

// sentence joins the clauses of a failure's sentence that are present, one
// space apart.
func sentence(clauses ...string) string {
	kept := clauses[:0:0]
	for _, c := range clauses {
		if c != "" {
			kept = append(kept, c)
		}
	}
	return strings.Join(kept, " ")
}

// ErrOutcomeUnknown is the cause a write tool's failed result carries when
// whether its write LANDED is unknown — see [unknownWrite] for why such a
// write is a failed result at all rather than a receipt.
//
// THE TOOL CONTRACT'S OWN SENTINEL ([tools.ErrOutcomeUnknown]) rather than one
// of this package's, because the readers that must tell an unknown from a
// refusal — the audit, the loop's record, a person's write surface — sit
// beside or below this package and ask [tools.UnknownOf], which is exactly
// [errors.Is] against it. A sentinel of its own here was a third answer to "may
// this have landed" that none of them could see. It is also
// [tools.RefusalUnavailable] by [tools.RefusalOf]: the node cannot say what
// became of the write, and nothing about the request was wrong.
// [UnknownOutcome] is the value, and it matches this with [errors.Is].
var ErrOutcomeUnknown = tools.ErrOutcomeUnknown

// UnknownOutcome is [ErrOutcomeUnknown] with the facts a caller acts on.
type UnknownOutcome struct {
	// OpID is the operation the caller retries under: the `op_id` a caller
	// with no turn brought back, or the one the tool wrote under. Empty for
	// a walking gesture a seat made, whose steps' ids are its turn's and
	// never a caller's to name — the same call made again is the retry.
	OpID string

	// Unvouched says this node's operation ledger cannot vouch for the
	// operation ([statelog.Result.Unvouched]): the same operation asked
	// HERE answers the same way until the write reaches this node, so the
	// retry that can say sooner is one made on another node.
	Unvouched bool

	// Err is what the writer answered, for a gesture that STOPPED at the
	// unknown step ([worklet.ErrStepUnresolved]); nil for a write that
	// answered `unknown` with no error of its own.
	Err error
}

func (e *UnknownOutcome) Error() string {
	what := "the write"
	if e.OpID != "" {
		what = "operation " + e.OpID
	}
	msg := fmt.Sprintf("builtin: whether %s landed is unknown", what)
	if e.Unvouched {
		msg += ", and this node's operation ledger cannot vouch for it"
	}
	if e.Err != nil {
		msg += ": " + e.Err.Error()
	}
	return msg
}

// Unwrap makes every [UnknownOutcome] an [ErrOutcomeUnknown] — and through it
// [tools.RefusalUnavailable] — and keeps the writer's own error reachable
// beside it. The sentinel comes FIRST, so the class [tools.RefusalOf] meets is
// the unknown's rather than one the writer's error happens to carry.
func (e *UnknownOutcome) Unwrap() []error {
	if e.Err == nil {
		return []error{ErrOutcomeUnknown}
	}
	return []error{ErrOutcomeUnknown, e.Err}
}

// failedUnknown is the failed result of a write whose outcome is unknown,
// carrying [UnknownOutcome] as its cause.
func failedUnknown(opID string, unvouched bool, msg string) tools.Result {
	return tools.Result{Output: msg, Failed: true,
		Cause: &UnknownOutcome{OpID: opID, Unvouched: unvouched}}
}

// writeCause is [UnknownCause] under the actor's operation.
func writeCause(actor Actor, err error) error {
	return UnknownCause(actor.Operation, err)
}

// UnknownCause is the error a write's failed result carries beneath its class:
// the error itself, or — for a write whose outcome is UNKNOWN — an
// [UnknownOutcome] wrapping it, under the operation opID names. Two errors are
// that:
//
//   - a walking gesture that stopped at a step whose outcome is unknown
//     ([worklet.ErrStepUnresolved]): the steps before it landed and the step
//     itself may have;
//   - a write the estate's router SENT and never heard back about
//     ([estate.ErrOutcomeUnknown]): a page write that names no operation of
//     its own, which goes to one data node only, or a write carrying its
//     caller's operation that no data node it was asked of answered — each
//     node that went quiet may have run it.
//
// BECAUSE EACH IS AN UNKNOWN, not a refusal: a caller answering in status
// codes owes it the answer it gives every unknown — read back before writing
// again, or the same operation retried — and read as the error alone the first
// was a refusal saying nothing had been written, the second a fault telling a
// person that sending it again could not help. The error stays inside it, so
// errors.Is still answers every question it did.
//
// EXPORTED for the human write surface, whose tool-less writers are read by
// this one rule (internal/api/operator's ClassifyWrite) — a second list of
// which writer errors may have landed is a list that misses one.
func UnknownCause(opID string, err error) error {
	switch {
	case errors.Is(err, worklet.ErrStepUnresolved):
		return &UnknownOutcome{OpID: opID,
			Unvouched: errors.Is(err, worklet.ErrStepUnvouched), Err: err}
	case errors.Is(err, estate.ErrOutcomeUnknown):
		return &UnknownOutcome{OpID: opID, Err: err}
	}
	return err
}

// Unanswered is how a write the estate's router sent and never heard back
// about ([estate.ErrOutcomeUnknown]) reads to whoever made it — in fixed
// words, because the router's own name every node it asked. "A" DATA NODE,
// NEVER "THE": a write with no key of its caller's goes to one data node, and
// a repeatable one to every data node in turn, which ends unknown when none
// ran it and any one of them stayed silent — the others may have answered
// that they did not run it, so "none answered" would not be true either.
const Unanswered = "a data node this write was sent to did not answer, so " +
	"whether it landed is unknown"

// clip flattens a caller-supplied string echoed back into a tool result or a
// log line.
//
// NEWLINES ONLY, no length cut. A smuggled newline genuinely breaks a
// line-structured render, so folding whitespace earns its place. Cutting the
// string did not: what is echoed here is the model's OWN argument, quoted back
// so it can see what failed to match, and a shortened echo names a query the
// model never sent — which is worse than a long line, because the model then
// retries against the wrong string.
func clip(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func sortStrings(s []string) { slices.Sort(s) }

func sortedKeys(m map[string]string) []string {
	out := slices.Sorted(maps.Keys(m))
	return out
}

// argIntValue and argFloatValue read a number, reporting whether they COULD.
//
// # Why not [argInt] and [argFloat]
//
// Because both answer a fallback for a value they cannot read, and every
// caller of these two holds a value whose zero is a SETTING rather than an
// absence: zero minutes and zero points both mean UNESTIMATED, a `min` of 0 is
// a real floor, and a `precision` of 0 declares a field exact to whole
// numbers. So an unreadable value became `&0` and the write succeeded —
// `estimate_minutes: "two hours"` answered `applied` and wiped the estimate,
// which is the exact failure the schedule reader's own header says it exists
// to prevent, in the half of it that was not written to the rule. [argFloat]'s
// doc states the condition under which its zero is right — "every caller of
// this one has a field whose zero IS its default" — and these are the callers
// that broke it.
//
// The parse is the WHOLE string rather than [fmt.Sscanf]'s prefix, which is
// the other half of the same bug: `Sscanf("%d")` reads "2 days" as 2, so a
// two-day estimate was stored as two MINUTES and nothing was refused.
//
// THEY LIVE HERE rather than beside the schedule, because the custom-field
// declaration reads its `precision` and its `min`/`max` bounds by the same
// rule and a second spelling of the json.Number / finiteness discipline is how
// one of them stops matching the other — the objection [textcut] and [whsec]
// each record for a grammar that was written twice.
func argIntValue(raw any) (int, bool) { return argval.Whole(raw) }

func argFloatValue(raw any) (float64, bool) { return argval.Number(raw) }
