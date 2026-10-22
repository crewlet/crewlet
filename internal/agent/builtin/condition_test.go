package builtin_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/estate"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
	"github.com/crewlet/crewlet/internal/tools"
)

// conditionDetail is what a refusal says for whoever runs the node: a
// position, a stream setting and a subject, none of which a model or a person
// acts on.
const conditionDetail = "the write needs 3:4182 on crewlet.tracker.task.t-1; " +
	"raise stream.tracker_max_bytes"

// errTransport is a backend's own words beneath its mark: a host and a
// port, which belong in the node's log.
var errTransport = errors.New("nats: dial tcp 10.0.0.7:4222: connection refused")

// everyCondition is one error per condition [builtin.Condition] names, each
// wrapped as a domain or a backend wraps it: every write reason and read
// refusal of the state log, a bare state-log sentinel, and every other mark.
func everyCondition() map[string]error {
	out := map[string]error{
		"a bare state-log sentinel": fmt.Errorf("chatlet: %w", statelog.ErrUnavailable),
		"a coordination store": fmt.Errorf("read pins: %w: %w", coord.ErrUnavailable,
			errTransport),
		"an estate nobody served": fmt.Errorf("estate: tracker.task: %w",
			&estate.ErrUnserved{Detail: unservedDetail}),
		"an estate held closed": fmt.Errorf("worklet: %w", store.ErrNoEstate),
		"a busy store": fmt.Errorf("worklet: %w: %w", store.ErrBusy,
			errors.New("turso: error: database is locked")),
		"a queue not live": fmt.Errorf("publish: %w", queue.ErrNotLive),
		"a broker blip": fmt.Errorf("ask: %w: %w", queue.ErrUnavailable,
			errTransport),
		"a wait that ran out": fmt.Errorf("read: %w", context.DeadlineExceeded),
		"a wait abandoned":    fmt.Errorf("read: %w", context.Canceled),
		"a full file store": fmt.Errorf("objstore: store the object: natsobj: put files/x: "+
			"%w: bucket crewlet_files: %w", objstore.ErrFull, errTransport),
		"a file store that did not answer": fmt.Errorf("objstore: store the object: %w: %w",
			objstore.ErrUnanswered, errTransport),
	}
	for _, r := range statelog.Reasons() {
		out["write "+string(r)] = fmt.Errorf("worklet: decide: %w", &statelog.Unavailable{
			Reason: r, Detail: conditionDetail, OpID: "op-1"})
	}
	for _, c := range statelog.ReadRefusals {
		out["read "+string(c)] = fmt.Errorf("doclet: read: %w", &statelog.Refused{
			Code: c, Level: statelog.ReadLinearizable, Detail: conditionDetail,
			RetryAfter: statelog.RetryHint(c, 10, 1)})
	}
	return out
}

// EVERY CONDITION IS TOLD TO BOTH ITS READERS, AND TO NEITHER IN ITS OWN TEXT.
//
// A model reads a condition as a clause inside a tool's sentence
// ([builtin.Condition]), and a person as a whole sentence on a screen
// ([builtin.ConditionSentence]); both come from one classification, so
// neither can name a condition the other does not. The model's clause and the
// person's sentence carry nothing of the error: no framework or package name,
// no reason's spelling, no position, setting, subject or host. The person's
// sentence is in a screen's voice — no em dash and none of the engine's own
// words for its parts — and asks for a try in a moment exactly where the
// answer carries a Retry-After; a state-log refusal is told in the state log's
// own sentence for its code.
//
// Mutation: drop an arm from the classification and its row goes red; answer a
// refusal's message ([statelog.Unavailable.Error]) or its detail and every
// state-log row does; give a person's sentence an em dash, or advice that
// disagrees with the Retry-After, and that row does.
func TestAConditionIsToldToItsTwoReadersWithoutItsOwnText(t *testing.T) {
	t.Parallel()
	for name, err := range everyCondition() {
		clause, ok := builtin.Condition(err)
		person, said := builtin.ConditionSentence(err)
		if !ok || !said || clause == "" || person == "" {
			t.Errorf("%s is told %q (%v) to a model and %q (%v) to a person", name,
				clause, ok, person, said)
			continue
		}
		for _, leak := range []string{"statelog", "crewlet.", "3:4182", "tracker_max",
			"op-1", "10.0.0.7", "data-b", "turso", "_", "unavailable ("} {
			if strings.Contains(clause, leak) {
				t.Errorf("%s reaches a model with %q in it: %s", name, leak, clause)
			}
			if strings.Contains(person, leak) {
				t.Errorf("%s reaches a person with %q in it: %s", name, leak, person)
			}
		}
		lower := strings.ToLower(person)
		for _, internal := range []string{"—", "coordination", "broker", "queue",
			"estate", "snapshot", "store", "data node", "log", "peer"} {
			if strings.Contains(lower, internal) {
				t.Errorf("%s is told to a person with %q in it: %s", name, internal, person)
			}
		}
		// THE HINT IS THE ONE EVERY SURFACE SENDS BESIDE THESE WORDS
		// ([builtin.RetryAfter]), which reads the same classification.
		waits := builtin.RetryAfter(err, time.Second) > 0
		if soon := strings.HasSuffix(person, statelog.TryAgainSoon); soon != waits {
			t.Errorf("%s carries a Retry-After: %v, and tells a person %q", name,
				waits, person)
		}
		if words, isRefusal := statelog.ConditionSentence(err); isRefusal && words != person {
			t.Errorf("%s is told to a person as %q, not in the state log's own "+
				"sentence %q", name, person, words)
		}
	}
	for _, fault := range []error{nil, errors.New("disk I/O error"),
		fmt.Errorf("worklet: read ENG-1: %w", errors.New("database is locked"))} {
		if clause, ok := builtin.Condition(fault); ok {
			t.Errorf("a fault %v is a condition to a model: %q", fault, clause)
		}
		if person, ok := builtin.ConditionSentence(fault); ok {
			t.Errorf("a fault %v is a condition to a person: %q", fault, person)
		}
	}
}

// A CONDITION NO WAIT CLEARS IS NOT RETRIED BY A MODEL. A tool's sentence
// about a condition told a model to try again whatever the condition was, so a
// log at its byte ceiling — refused the same way until an administrator makes
// room — was asked again for as long as the turn lasted. Now the sentence ends
// on what a model can do about it: say it could not check, or could not make
// the change, and why — never the person's advice, which sends them to another
// node or an administrator, neither of which a model reaches. Only a condition
// that clears on its own is offered the retry. A read and a write each,
// through the tools themselves, beside a control that clears; every other
// helper a tool answers a condition through is held to the same in
// TestEveryToolTellsAModelWhatAConditionNoWaitClearsLeavesIt.
//
// Mutation: answer every condition "Try again shortly" (or drop the next
// clause from the write's sentence) and the full log's rows go red; offer the
// person's advice instead and they go red too; offer the model's to a
// condition that clears and the control does.
func TestAConditionNoWaitClearsIsNotRetriedByAModel(t *testing.T) {
	t.Parallel()
	trk := newFakeTracker()
	reg := workRegistry(t, builtin.WorkDeps{Reader: trk, Writer: trk.as})
	read := func() tools.Result {
		return callWork(t, reg, builtin.GetWorkItemTool, map[string]any{"item": "ENG-1"})
	}
	write := func() tools.Result {
		return callWork(t, reg, builtin.UpdateWorkItemTool,
			map[string]any{"item": "ENG-1", "priority": "urgent"})
	}
	personal := func(said string) bool {
		return strings.Contains(said, "another node") ||
			strings.Contains(said, "administrator")
	}

	trk.readErr = &statelog.Refused{Code: statelog.RefuseLogFull,
		Level: statelog.ReadLinearizable, Detail: conditionDetail}
	got := read()
	if tools.RefusalOf(got) != tools.RefusalUnavailable ||
		!strings.Contains(got.Output, "say you could not check") ||
		strings.Contains(got.Output, "Try again") || personal(got.Output) {
		t.Errorf("a read refused on a full log is told %q (%s); want a model's own "+
			"advice and no retry", got.Output, tools.RefusalOf(got))
	}
	trk.readErr = nil
	trk.writeErr = fmt.Errorf("worklet: update ENG-1: %w", &statelog.Unavailable{
		Reason: statelog.ReasonLogFull, Detail: conditionDetail})
	got = write()
	if tools.RefusalOf(got) != tools.RefusalUnavailable ||
		!strings.Contains(got.Output, "say it could not be done") || personal(got.Output) {
		t.Errorf("a write refused on a full log is told %q (%s); want a model's own advice",
			got.Output, tools.RefusalOf(got))
	}

	trk.writeErr = nil
	trk.readErr = &statelog.Refused{Code: statelog.RefuseBehind,
		Level: statelog.ReadLinearizable, Detail: conditionDetail, RetryAfter: time.Second}
	got = read()
	if !strings.Contains(got.Output, "Try again") || personal(got.Output) ||
		strings.Contains(got.Output, "won't clear") {
		t.Errorf("a read refused for a node behind is told %q; want the retry and "+
			"nothing for an administrator", got.Output)
	}
}
