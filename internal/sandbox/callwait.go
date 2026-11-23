package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"slices"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events/types"
)

// A RUN PARKED ON ITS OWN BRIDGED CALLS, each answered still running before
// its harness's ceiling and promised a resume with its result: every such call
// is added to the row ([PendingRun.ParkedCalls], [Ask.Call]), whatever the job
// asked before it, and the resume tells each with what it returned. Where the
// job asked nothing else, the first park is its ask, put to nobody and waiting
// on its own calls ([RunWait.OwnCalls]), answered by the engine once the job
// is parked; so the job is handed no item of a call nobody waited for
// ([CoordStore.fileLateItems]). Why, in internal/api/mcpbridge's package doc,
// "A call longer than its harness waits".

// CallWait is a bridged call a job parked on ([PendingRun.ParkedCalls]): the
// tool it called and its place among the run's calls ([BridgeCall.Seq]).
//
// A SECTION WITH AN EXTRA OF ITS OWN (wirekeys.go).
type CallWait struct {
	Tool string `json:"tool"`
	Seq  int    `json:"seq"`

	// Extra is every key of the section this build does not know: see
	// [RunSteer.Extra].
	Extra map[string]json.RawMessage `json:"-"`
}

// MaxParkedCalls bounds the calls one job parks on ([PendingRun.ParkedCalls]).
// SIXTEEN, A JUDGEMENT CALL: the first park tells the job to end its turn, so
// a later one comes from a call already running beside it, a harness's
// parallel batch, which no harness measured sends past a handful. A park past
// it is refused ([ErrParkedCallsFull]): the call is answered still running with
// no promise, and its result reaches the run as a call nobody waited for.
const MaxParkedCalls = 16

// ErrParkedCallsFull is a park past [MaxParkedCalls].
var ErrParkedCallsFull = errors.New("sandbox: the job is parked on as many calls as it may be")

// callWaitPrefix opens a call wait's question, the engine's words before the
// tool's name ([CallWaitQuestion]).
const callWaitPrefix = "Waiting for its call to return: "

// CallWaitQuestion is the question a job parked on its call to tool is drawn
// under, as a parked question's title is: the engine's words, then the tool.
func CallWaitQuestion(tool string) string { return callWaitPrefix + tool }

// validate is every fault of w.
func (w CallWait) validate() []error {
	var found []error
	if w.Tool == "" || len(w.Tool) > coord.MaxMCPToolNameBytes {
		found = append(found, fieldRefusal("tool", fmt.Sprintf("is empty or past %d bytes: a call "+
			"wait names the tool its job called", coord.MaxMCPToolNameBytes)))
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
// ([PendingRun.ParkedCalls]), whatever else it asked.
func (r PendingRun) WaitsOnCall() bool { return len(r.ParkedCalls) > 0 }

// WaitsOnOwnCalls reports whether run's job asked nothing but to wait on its
// own calls ([RunWait.OwnCalls]): the ask the engine answers once the job is
// parked, every call having returned or being past waiting for.
func (r PendingRun) WaitsOnOwnCalls() bool { return r.WaitsFor != nil && r.WaitsFor.OwnCalls }

// CallsReturned is run's parked calls as an answer names them
// ([types.ExecutorAnswerGiven.Called]).
func (r PendingRun) CallsReturned() []types.CallReturned {
	if len(r.ParkedCalls) == 0 {
		return nil
	}
	out := make([]types.CallReturned, len(r.ParkedCalls))
	for i, c := range r.ParkedCalls {
		out[i] = types.CallReturned{Tool: c.Tool, Seq: c.Seq}
	}
	return out
}

// calledOf is what an answer by turn says of the calls run's job parked on:
// nil for none, and refused unless the job waits on its own calls alone and
// the answer names every one of them, in order, so no answer says a call
// returned that the job never parked on, nor leaves one untold.
func calledOf(run PendingRun, called []types.CallReturned) ([]types.CallReturned, error) {
	switch {
	case len(called) == 0:
		return nil, nil
	case !run.WaitsOnOwnCalls() || !slices.Equal(run.CallsReturned(), called):
		return nil, fmt.Errorf("sandbox: an answer says calls %v returned, and run %s's job does not "+
			"wait on its own calls %v", called, run.TurnID, run.CallsReturned())
	}
	return slices.Clone(called), nil
}
