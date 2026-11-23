package builtin

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/agent/steer"
	"github.com/crewlet/crewlet/internal/caveat"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
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
// Failed rather than an error: the turn is fine, this call is not, so the model
// can retry with a better argument instead of the loop tearing down.
//
// THE CLASS IS AN ARGUMENT, never inferred from the sentence: the sentence is
// prompt text, and rewording it must not move a person's surface to another
// status. It is the result's CAUSE ([tools.Refusal] is an error), the one place
// a reader asks ([tools.RefusalOf]); a class beside the cause would be a second
// classifier of one failure.
func refused(code tools.Refusal, msg string) tools.Result {
	return tools.Result{Output: msg, Failed: true, Cause: code}
}

// failed is the argument refusal — [tools.RefusalInvalid] — which is what the
// great majority of these are: a missing, malformed or unknown argument whose
// sentence names the field to change. Anything else says its class with
// [refused] or [refusedBy].
func failed(msg string) tools.Result { return refused(tools.RefusalInvalid, msg) }

// failedArg is [failed] for a refusal of ONE argument, carrying its name as a
// value beside the class ([argRefused]) so a surface can say in its own words
// which value was refused (ADR-0060).
func failedArg(field, msg string) tools.Result {
	return tools.Result{Output: msg, Failed: true, Cause: tools.Classify(tools.RefusalInvalid, argRefused(field))}
}

// argRefused is the argument a tool refused, by the name the call spells it.
type argRefused string

func (a argRefused) Error() string { return "builtin: refused argument " + string(a) }

// RefusedField is the argument, as a surface outside the tools asks it of a cause's chain.
func (a argRefused) RefusedField() string { return string(a) }

// refusedBy is [refused] for a refusal something underneath decided, carrying
// that error BENEATH the class ([tools.Classify]), so a caller branching on the
// domain's sentinel and a reader asking for the class read one value.
//
// The message is still the whole answer to a model. The cause lets a caller
// answering in STATUS CODES tell a missing item from a node that could not
// decide. The class is an argument because it describes THIS call: a missing
// item is not-found when the call is about it, invalid when it merely named it.
//
// THE PERSON'S WORDS ARE THE CAUSE'S (ADR-0060): phrasing a writer put beside
// its error ([caveat.Saying]) is lifted here, the one place a refusal with a
// cause is made, so no tool has to remember to carry it.
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
// CONDITION of this node, one WAITING can clear, and the clause a model may be
// told it in; ok is false for a FAULT, which waiting does not clear and whose
// words are the log's ([faulted]).
//
// Each condition is a sentinel its own package marks, never a reading of an
// error's words: a "database is locked" met for any other reason is not a wait.
// See [condition] for the set and why the line is drawn here. It is told twice
// from one classification: this is the model's clause, [ConditionSentence] the
// person's whole sentence (the act route tells a person only the refusal
// catalog's words, ADR-0060). EXPORTED for the human write surface, whose
// tool-less writers class errors by this rule (internal/api/operator's ClassifyWrite).
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

// conditionSaid is one condition as each of its two readers is told it, and
// the kind a failed phase or turn records it under ([ConditionKind]).
type conditionSaid struct {
	// kind is the condition's word on a failed phase's or turn's record.
	kind string

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

// waitsUntil is the cause of an unavailable refusal a wait clears at a known
// instant and no condition names: a model at a usage limit its harness reported,
// which its backend sits out until at (ADR-0064). [RetryAfter] answers the
// wait left until then, never less than a second, where this node's undecided
// hint told a person to come back in two seconds to a limit lifting in an
// hour; a zero instant, a limit nothing said the end of, takes the hint as any
// cause [RetryAfter] doesn't know.
func waitsUntil(at time.Time) error { return &waitUntil{at: at} }

// waitUntil is [waitsUntil]'s mark: the instant, and nothing of what was met,
// for [waitless]'s reason.
type waitUntil struct{ at time.Time }

func (w *waitUntil) Error() string {
	return "builtin: waiting clears this at " + w.at.UTC().Format(time.RFC3339)
}

// errWaitless is the mark beneath an unavailable refusal that no wait clears
// and no condition names ([waitless]).
var errWaitless = errors.New("builtin: no wait clears this")

// waitless is the cause of an unavailable refusal no wait clears and no
// condition names: a company that runs no model for the call, a login the
// model's harness reports refused or missing, a call the engine itself cannot make
// (llm.KindFatal). The class says "this node cannot serve it", which is true,
// and it was answered as every unavailable refusal is, under this node's
// undecided hint, so a person told "an administrator needs to set one up" was
// told in the same answer to come back in two seconds. [RetryAfter] answers the
// mark with no hint. The mark carries no error of its own: the tool logs what
// it met, and one beneath the mark could be read as a condition with its own
// words.
func waitless() error { return errWaitless }

// errWaits is the mark beneath an unavailable refusal a wait can clear
// ([waits]).
var errWaits = errors.New("builtin: waiting can clear this")

// waits is the cause of an unavailable refusal a wait can clear and no
// condition names: a read that didn't answer, an index still being built.
// [RetryAfter] answers it with the hint. It is a mark rather than nothing so
// every unavailable refusal says which it is, waits, [waitsUntil] or
// [waitless], never guessed from wording
// (TestEveryUnavailableRefusalSaysWhetherAWaitClearsIt).
func waits() error { return errWaits }

// RetryAfter is how long somebody told about err should wait before asking
// again: NONE for a condition no wait clears ([condition]'s stuck — a log at
// its ceiling, a company whose file storage is full) and for a refusal a tool
// marked as one ([waitless]), the wait until the instant a tool marked
// ([waitsUntil]), the condition's own soonest where it knows one, and
// otherwise [statelog.RetryAfter]'s reading of err, with otherwise as this
// node's hint where nothing derived one.
//
// THE ONE HINT every surface answering in status codes takes, BESIDE
// [ConditionSentence], so the words and the Retry-After come from one
// classification: from the state log's rule alone, a full file store was told
// to make room under a two-second Retry-After.
func RetryAfter(err error, otherwise time.Duration) time.Duration {
	if errors.Is(err, errWaitless) {
		return 0
	}
	var until *waitUntil
	if errors.As(err, &until) && !until.at.IsZero() {
		return max(time.Until(until.at), time.Second)
	}
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
//
// The line matters: [tools.RefusalUnavailable] answers `503` with a
// `Retry-After` and tells a model to retry, a lie about an unreadable store, so
// every classifier asks this first and classes the rest
// [tools.RefusalInternalError]. A state-log refusal and an unserved estate
// ([estate.ErrUnserved]) stay conditions; an outcome the router could not
// account for ([estate.ErrOutcomeUnknown]) is [UnknownCause], never this.
// Nobody is told the error's own text (hosts, ports, nodes): a state-log
// refusal is told in [statelog.ConditionWords], the rest in fixed words, and a
// condition no wait clears says so ([conditionNext]).
func condition(err error) (conditionSaid, bool) {
	var unknownKeys *estate.ErrUnknownKeys
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
			person: objstore.FullWords, stuck: true, kind: KindFileStoreFull}, true
	case errors.Is(err, objstore.ErrUnanswered):
		// THE FILE STORE DID NOT TAKE THE BYTES, which clears when it
		// answers again: the default store is the data nodes' own bucket,
		// which answers no sooner than its group elects.
		return conditionSaid{clause: "the company's file storage did not answer",
			person: "The company's files couldn't be saved just now. " +
				statelog.TryAgainSoon, after: statelog.ElectionRetryHint,
			kind: KindFileStoreUnavailable}, true
	case errors.Is(err, coord.ErrUnavailable):
		return conditionSaid{clause: "the coordination store did not answer",
			person: "This node couldn't reach the rest of the fleet. " +
				statelog.TryAgainSoon, kind: KindCoordinationUnavailable}, true
	case errors.As(err, &unknownKeys):
		// THE FLEET RUNS TWO BUILDS: the data nodes that would serve this
		// cannot read the request whole, so none of it ran rather than run
		// without what they could not read — or this node cannot read whole
		// the answer one of them gave, so it was not acted on. Each clause
		// says its own direction: the answer RAN where it was given. Read
		// BEFORE an estate nobody served, which the first wraps: waiting
		// clears either only once the upgrade reaches the nodes still on the
		// older build, which is an operator's to finish.
		clause := "the fleet's nodes run two different builds, and the data nodes that would serve " +
			"this can't read all of it, so none of it ran"
		if unknownKeys.Answer {
			clause = "this node runs an older build than the data node that answered, and can't read " +
				"all of its answer, so it was not acted on"
		}
		return conditionSaid{clause: clause,
			person: "This node and the company's other nodes run different versions of Crewlet, so " +
				"this couldn't be done. An administrator needs to finish upgrading them.",
			stuck: true, kind: KindFleetBuildsDiffer}, true
	case errors.As(err, new(*estate.ErrUnserved)):
		return conditionSaid{clause: "no data node could serve it right now — " +
			"every node holding the company's records answered that its copy is " +
			"out of service or behind, or, asked to read, did not answer at all",
			person: "No node could serve this right now. " + statelog.TryAgainSoon,
			kind:   KindNoDataNode}, true
	case errors.Is(err, store.ErrNoEstate):
		return conditionSaid{clause: "this node's store is closed while it adopts " +
			"a peer's snapshot or shuts down",
			person: "This node is catching up from another node or shutting " +
				"down. " + statelog.TryAgainSoon, kind: KindEstateClosed}, true
	case errors.Is(err, store.ErrBusy):
		return conditionSaid{clause: "this node's store was busy with other writes " +
			"and could not take this one in time",
			person: "This node was too busy to take this in time. " +
				statelog.TryAgainSoon, kind: KindStoreBusy}, true
	case errors.Is(err, queue.ErrNotLive):
		return conditionSaid{clause: "this node's event queue is not running — it " +
			"is starting or shutting down",
			person: "This node is starting up or shutting down. " +
				statelog.TryAgainSoon, kind: KindQueueNotLive}, true
	case errors.Is(err, queue.ErrUnavailable):
		return conditionSaid{clause: "this node could not reach its event broker — " +
			"the connection is reconnecting, or the broker did not answer in time",
			person: "This node lost touch with the rest of the fleet. " +
				statelog.TryAgainSoon, kind: KindQueueUnavailable}, true
	case errors.Is(err, sandbox.ErrTailUnanswered):
		// THE NODE RUNNING AN EXECUTOR'S BOX COULD NOT READ IT FOR A REASON
		// OF ITS OWN a wait clears: the executor's seat moving, the node
		// stopping, or what it reads the executor's record from not
		// answering. Its own log has which.
		return conditionSaid{clause: "the node running the executor's box couldn't " +
			"answer just now",
			person: "The node running this executor's box couldn't answer just now. " +
				statelog.TryAgainSoon, kind: KindExecutorUnanswered}, true
	case errors.Is(err, steer.ErrUnverified):
		// THE FLEET'S NODES DO NOT HOLD THE SAME KEYS, so the node running a
		// turn took nothing it could not verify: only an operator giving them
		// the same keys clears it. The keys go to the log; the person's
		// sentence says who fixes it.
		return conditionSaid{clause: "the node running the turn couldn't verify " +
			"the note, because the two nodes don't hold the same keys",
			person: "The node running this turn couldn't verify your note, so it " +
				"took nothing. An administrator needs to give the fleet's nodes the " +
				"same keys.", stuck: true, kind: KindFleetKeysDiffer}, true
	case errors.Is(err, steer.ErrSeatKeyUnknown):
		// THE SAME FACT ON THE RUN'S RECORD: the seat it names, which a
		// person's authority to steer is decided on, is sealed under a key
		// this node does not hold, so nothing is decided and nothing is sent
		// until an operator gives the nodes the same keys.
		return conditionSaid{clause: "this node couldn't tell whose turn it is, because " +
			"the fleet's nodes don't hold the same keys",
			person: "This node couldn't tell whose turn this is, so your note wasn't " +
				"sent. An administrator needs to give the fleet's nodes the same keys.",
			stuck: true, kind: KindFleetKeysDiffer}, true
	}
	// THE SANDBOX'S CLASS, read before a deadline or a cancellation its own
	// chain may carry: the backend that met the failure knew what it was,
	// and a runtime command that ran out of time is the box or its runtime
	// not answering, which a class says and a deadline does not.
	if why, ok := sandbox.WhyOf(err); ok {
		if said, ok := sandboxConditions[why]; ok {
			said.kind = SandboxKindPrefix + string(why)
			return said, true
		}
	}
	if said, ok := hostCondition(err); ok {
		return said, true
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return conditionSaid{clause: "it ran out of time",
			person: "This took too long, so it stopped. " + statelog.TryAgainSoon,
			kind:   llm.KindTimeout.String()}, true
	case errors.Is(err, context.Canceled):
		return conditionSaid{clause: "it was abandoned before it finished",
			person: "This stopped before it finished. " + statelog.TryAgainSoon,
			kind:   llm.KindCancelled.String()}, true
	}
	return conditionSaid{}, false
}

// sandboxConditions is every class a coding sandbox's backend marks a failure
// with ([sandbox.Why]), told to a model and a person. A class no wait clears
// ([sandbox.Why.Waits]) is stuck and offered no retry: its setup is somebody's
// to change. A box read words its own ([boxFailure]); this is every other
// reader's, the tail query ([sandbox.TailReader]) included.
// TestEverySandboxClassIsACondition holds every class to an entry here.
//
// Every class a wait clears clears soon: a setup download that failed for now
// ([sandbox.WhySetupUnavailable], exit 75 after its own retries) and a box or
// runtime that did not answer are "in a moment", the caller's hint.
var sandboxConditions = map[sandbox.Why]conditionSaid{
	sandbox.WhyConfiguration: {clause: "the coding sandbox isn't set up for this",
		person: "The coding sandbox isn't set up for this. An administrator needs " +
			"to fix its setup.", stuck: true},
	sandbox.WhySetupUnavailable: {clause: "a download the coding sandbox's setup " +
		"makes failed for now",
		person: "A download the coding sandbox's setup makes failed for now. " +
			statelog.TryAgainSoon},
	sandbox.WhyProviderUnavailable: {clause: "the coding sandbox or the executor's " +
		"box didn't answer, or failed on its side",
		person: "The coding sandbox didn't answer. " + statelog.TryAgainSoon},
}

// stateLogSaid is how a state-log refusal reads: the refusal read out of the
// chain — a read refused ([statelog.Refused]), a write ([statelog.Unavailable])
// or a bare sentinel somebody wrapped — in the state log's own words for it
// ([statelog.ConditionWords]), and never its error or its detail, which are
// the log's. Its kind is [KindStateLogWriteRefused] for a write refused for
// what it is ([statelog.Reason.RefusesTheWrite]) and [KindStateLogUnavailable]
// for every other refusal.
//
// THE REFUSAL ITSELF, never the chain's whole message: a wrapper is free to
// join an error of its own beside it (`%w: %w`), and that one may be a
// driver's.
func stateLogSaid(err error) conditionSaid {
	what, _, _ := statelog.ConditionWords(err)
	person, _ := statelog.ConditionSentence(err)
	kind := KindStateLogUnavailable
	var refused *statelog.Unavailable
	if errors.As(err, &refused) && refused.Reason.RefusesTheWrite() {
		kind = KindStateLogWriteRefused
	}
	return conditionSaid{clause: what, person: person,
		stuck: statelog.RetryAfter(err, time.Second) == 0, kind: kind}
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
// call's trace: a store's error is a driver message, SQL or a database path,
// and the sentence reaches a prompt and a screen, whose readers cannot act on
// it; whoever runs the node reads the log. It rides beneath the class for
// errors.Is, never for display ([tools.RefusalInternalError]).
func faulted(ctx context.Context, tool string, err error, sentence string) tools.Result {
	log.ErrorContext(ctx, "builtin_tool_fault", "tool", tool, "error", err.Error())
	return refusedBy(tools.RefusalInternalError, err, sentence)
}

// toldOf is how a failed result's reason reads inside a sentence that names
// what it stopped: [faultSaid] for a fault; [Unanswered] for a write no data
// node answered; a refusal a domain or the framework decided, in its own words
// ([WriterSentence]); a condition's words ([Condition]); else the error's own.
//
// THE UNANSWERED WRITE GOES BEFORE THE CONDITION: one whose caller's context
// ended carries that context's error too, and "it ran out of time" would say
// the write did not land. THE DECIDED REFUSAL GOES BEFORE IT TOO: asked
// second, it would be told as its whole Go text, package name and broker
// subject included.
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
// reason of its OWN that no domain sentinel claimed: stopped names what could
// not be done ("Could not read your notes"), fact what is true either way
// ("Nothing was changed."), or empty.
//
// THE ONE RULE [readFailure] AND [writeFailure] FOLLOW: a CONDITION ([Condition]) is
// [tools.RefusalUnavailable] in its own words and invites the retry that
// clears it, and anything else is a FAULT ([faulted]) — its error in the log,
// never in the sentence, and no retry invited: the error's own text would put
// a database path into a prompt, and `unavailable` would tell a client to poll
// a store that cannot be read.
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
// THE TOOL CONTRACT'S OWN SENTINEL ([tools.ErrOutcomeUnknown]), not one of this
// package's: the readers that must tell an unknown from a refusal (the audit,
// the loop's record, a person's write surface) sit beside or below this package
// and ask [tools.UnknownOf], exactly [errors.Is] against it. It is also
// [tools.RefusalUnavailable] by [tools.RefusalOf]: nothing about the request was
// wrong. [UnknownOutcome] is the value, and it matches this with [errors.Is].
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
// the error itself, or, when the outcome is UNKNOWN, an [UnknownOutcome]
// wrapping it under operation opID. Two errors are unknown:
//
//   - a walking gesture stopped at an unknown step ([worklet.ErrStepUnresolved]);
//   - a write the estate's router SENT and never heard back about
//     ([estate.ErrOutcomeUnknown]): each silent node may have run it.
//
// A caller answering in status codes owes an unknown its own answer (read back,
// or retry the same operation), where the bare error read as a refusal or a
// fault; the error stays inside for errors.Is. EXPORTED for the human write
// surface (internal/api/operator's ClassifyWrite), so no second list misses one.
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
// Not [argInt] and [argFloat]: both answer a fallback for an unreadable value,
// and every caller of these holds a value whose zero is a SETTING, not an
// absence (zero minutes means UNESTIMATED, a `min` of 0 is a real floor). An
// unreadable `estimate_minutes: "two hours"` would become `&0`, answer
// `applied` and wipe the estimate. [argFloat]'s zero is right only where
// "every caller of this one has a field whose zero IS its default".
//
// The parse is the WHOLE string, not [fmt.Sscanf]'s prefix: "2 days" would
// read as 2, a two-day estimate stored as two MINUTES. They live here because
// custom-field `precision` and `min`/`max` read by the same rule.
func argIntValue(raw any) (int, bool) { return argval.Whole(raw) }

func argFloatValue(raw any) (float64, bool) { return argval.Number(raw) }
