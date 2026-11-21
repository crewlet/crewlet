package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A write sent under an operation this command names.
//
// # One rule, written once
// Every command whose write carries an operation key (`worklet purge`, `seats
// pause` and `resume`, the retention gestures, `crewlet chat` and `crewlet
// artifacts` writes) mints that key before it asks and meets the same answers
// that do not say "done": no answer, or a 503 that is an unknown this node can
// settle, one it cannot, or a refusal that wrote nothing. What to do about each
// is the ENGINE's rule, not any command's (the retry is the same operation,
// through this node or another, never a fresh id), so it is said here once.
// `crewlet iam` mints here too ([keyedWrite.retry], [keyedWrite.confirmed]) but
// renders its own 503s ([iamRefusal]), whose fields no other surface carries.

// operationFor is the operation a write is sent under: the -op-id the operator
// brought, held to the rule the node holds a caller's id to, or one minted here,
// now, under name. MINTED HERE, BEFORE THE REQUEST. A write whose answer never came may have
// landed, and its key is the only handle on it: a node-minted key was lost with
// the answer, leaving a second write under a fresh one. Minted as the node would
// ([statelog.NewOpID]), carrying the instant a node judges the retry by.
// AN OPERATOR'S ID IS CHECKED ([statelog.CheckCallerOpID]) before anything is
// sent, and sent as given: the node refuses one it did not mint, or one altered
// on the way back, `400 op_id_invalid`, and here the error names the flag.
// Trimmed first: an id pasted from a terminal carries its newline.
func operationFor(name, given string) (string, error) {
	given = strings.TrimSpace(given)
	if given == "" {
		return statelog.NewOpID(time.Now(), name), nil
	}
	if err := statelog.CheckCallerOpID(given); err != nil {
		return "", fmt.Errorf("-op-id: %w", err)
	}
	return given, nil
}

// keyedWrite is one write sent under an operation, and how an answer that
// leaves its outcome open is finished.
type keyedWrite struct {
	// gesture names the write in a sentence: "purge", "pause", "post".
	gesture string

	// operation is the id the write was sent under ([operationFor]).
	//
	// IT IS WHAT EVERY RETRY NAMES, rather than the op id an answer echoes:
	// the node scopes the key it is sent by the caller's own identity, and
	// does so deterministically, so the key this command sent and the
	// scoped one an answer names are one operation — and only the first is
	// known whatever came back.
	operation string

	// check is what shows whether the write landed, for the one answer a
	// retry through this node cannot settle: "whether ENG-42 is gone",
	// "the seat", "the room".
	check string
}

// confirmed is nil for a 2xx whose outcome says where the write stands —
// applied, or pending — and for any other the error that finishes it, what
// naming the write: "pause swe", "purge ENG-42".
//
// THE OPEN OUTCOME A 2xx CARRIES. The act transport reports an outcome its
// build cannot read as `unknown` rather than as applied, and a node newer than
// this command may answer one THIS build cannot read. Printed and exited zero,
// as `seats` did, a script read a write nobody could vouch for as one that
// landed; read as an outcome, `worklet purge` printed "unknown" where it prints
// "applied". So it FAILS, as every open outcome [keyedWrite.settle] reads
// does, naming the same retry.
func (k keyedWrite) confirmed(what, outcome string) error {
	said := unconfirmed(outcome)
	if said == "" {
		return nil
	}
	return fmt.Errorf("%s: %s. The node could not confirm the write. To find out, %s",
		what, said, k.retry())
}

// unconfirmed is how the outcome a 2xx answer carried is said where it leaves
// the write open — `unknown`, beside the word a node newer than this command
// answered where it answered one — and "" where it says where the write
// stands: applied, or pending.
//
// A FUNCTION OF ITS OWN, because a write no key names reads the same answer
// the same way and finishes it differently: `crewlet iam`'s mint and reset
// link are run again as new ones ([iamPrinter.settled]), where every write
// [keyedWrite.confirmed] judges is retried under its operation.
func unconfirmed(outcome string) string {
	switch statelog.Outcome(outcome) {
	case statelog.OutcomeApplied, statelog.OutcomePending:
		return ""
	case "", statelog.OutcomeUnknown:
		return "unknown"
	}
	return fmt.Sprintf("unknown (the node answered %q, which this build cannot read)",
		outcome)
}

// retry is the sentence that finishes the write: the same command, under the
// same operation.
func (k keyedWrite) retry() string {
	return fmt.Sprintf("run the same command again with -op-id %s, so the retry is "+
		"this %s rather than a second one", k.operation, k.gesture)
}

// settle is err as the command reports it, saying how to finish the write where
// err leaves its outcome open (a 2xx is [keyedWrite.confirmed]'s). FOUR ANSWERS:
//   - NO ANSWER: the node may have finished the write, so retry under the same
//     operation, said on stderr.
//   - AN UNKNOWN THIS NODE CAN SETTLE: the same request again here.
//   - AN UNKNOWN IT CANNOT (`unvouched`): its ledger cannot vouch for the
//     operation, so retry through another node, under the same id.
//   - A 503 REFUSAL NAMING THE OPERATION: nothing was written; retry under the
//     same id once the node can take it.
//
// ONLY A 503: a reused key's 409 means that key already wrote something else, so
// the node sends it to a NEW key; this one would be refused for ever.
func (k keyedWrite) settle(stderr io.Writer, err error) error {
	if err == nil {
		return nil
	}
	var lost noAnswer
	if errors.As(err, &lost) {
		fmt.Fprintf(stderr, "The node did not answer, so whether the %s landed is "+
			"unknown: %s.\n", k.gesture, k.retry())
		return err
	}
	var refused *nodeRefusal
	if !errors.As(err, &refused) || refused.Status != http.StatusServiceUnavailable ||
		(!refused.Unsettled && refused.OpID == "") {
		return err
	}
	switch {
	case refused.Unvouched:
		return fmt.Errorf("%w\n\nThis node cannot tell whether it landed and will "+
			"answer the same way until it can: check %s, or run it through another "+
			"node with -api <that node> -op-id %s, never a fresh id", err, k.check,
			k.operation)
	case refused.Unsettled:
		return fmt.Errorf("%w\n\nNo acknowledgement: this is the one outcome to "+
			"retry: %s", err, k.retry())
	default:
		return fmt.Errorf("%w\n\nThis attempt wrote nothing. Once the node can take "+
			"it, run it again with -op-id %s, so every attempt stays the one operation",
			err, k.operation)
	}
}
