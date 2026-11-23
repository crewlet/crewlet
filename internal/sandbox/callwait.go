package sandbox

import (
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/crewlet/crewlet/internal/decision"
	"github.com/crewlet/crewlet/internal/events/types"
)

// A RUN PARKED ON ITS OWN BRIDGED CALL, answered still running before its
// harness's ceiling: a [RunWait] naming the call ([RunWait.Call]), put to nobody
// as `wait_for_run`'s (ADR-0176), answered by the engine once the job is parked
// and carried on by its relaunch, which tells the call with what it returned;
// so the job is handed no item of it ([CoordStore.fileLateItems]). Why, in
// internal/api/mcpbridge's package doc, "A call longer than its harness waits".

// CallWait is the bridged call a job parked on ([RunWait.Call]): the tool it
// called and its place among the run's calls ([BridgeCall.Seq]).
//
// A SECTION WITH AN EXTRA OF ITS OWN (wirekeys.go).
type CallWait struct {
	Tool string `json:"tool"`
	Seq  int    `json:"seq"`

	// Extra is every key of the section this build does not know: see
	// [RunSteer.Extra].
	Extra map[string]json.RawMessage `json:"-"`
}

// callWaitPrefix opens a call wait's question, the engine's words before the
// tool's name ([CallWaitQuestion]).
const callWaitPrefix = "Waiting for its call to return: "

// CallWaitQuestion is the question a job parked on its call to tool is drawn
// under, as a parked question's title is: the engine's words, then the tool.
func CallWaitQuestion(tool string) string { return callWaitPrefix + tool }

// validate is every fault of w.
func (w CallWait) validate() []error {
	var found []error
	if w.Tool == "" || len(callWaitPrefix)+len(w.Tool) > decision.MaxQuestion {
		found = append(found, fieldRefusal("tool", fmt.Sprintf("is empty or past %d bytes: a call "+
			"wait names the tool its job called", decision.MaxQuestion-len(callWaitPrefix))))
	}
	if w.Seq <= 0 {
		found = append(found, fieldRefusal("seq", "names no call of the run: a call wait names the "+
			"call's place among the run's calls"))
	}
	return found
}

// MarshalJSON writes the call wait with the keys a later build added to it.
func (w CallWait) MarshalJSON() ([]byte, error) {
	type plain CallWait
	return marshalCarrying(plain(w), wireKeys(reflect.TypeFor[CallWait]()), w.Extra)
}

// UnmarshalJSON reads the call wait and keeps the keys this build does not know.
func (w *CallWait) UnmarshalJSON(raw []byte) error {
	type plain CallWait
	var p plain
	extra, err := unmarshalCarrying(raw, &p, wireKeys(reflect.TypeFor[CallWait]()))
	if err != nil {
		return err
	}
	p.Extra = extra
	*w = CallWait(p)
	return nil
}

// WaitsOnCall reports whether run's job is parked on a bridged call of its own
// ([RunWait.Call]).
func (r PendingRun) WaitsOnCall() bool { return r.WaitsFor != nil && r.WaitsFor.Call != nil }

// calledOf is what an answer by turn says of the call run's job parked on:
// nil for none, and refused where the job parks on no call, or on another than
// the one named, so no answer says a call returned that the job never parked on.
func calledOf(run PendingRun, called *types.CallReturned) (*types.CallReturned, error) {
	switch {
	case called == nil:
		return nil, nil
	case !run.WaitsOnCall() || run.WaitsFor.Call.Tool != called.Tool || run.WaitsFor.Call.Seq != called.Seq:
		return nil, fmt.Errorf("sandbox: an answer says call %d to %s returned, and run %s's job does not "+
			"wait on it", called.Seq, called.Tool, run.TurnID)
	}
	out := *called
	return &out, nil
}
