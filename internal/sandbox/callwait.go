package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/events/types"
)

// A RUN PARKED ON ITS OWN BRIDGED CALLS, each answered still running before
// its harness's ceiling and promised a resume with its result: every such call
// is added to the row ([PendingRun.ParkedCalls], [Ask.Call]), whatever the job
// asked before it, and the resume tells each with what it returned. Where the
// job asked nothing else, the first park is its ask ([CallWait.Ask]), put to
// nobody and carried on by the engine once the job is parked; so the job is
// handed no item of a call nobody waited for ([CoordStore.fileLateItems]). Why,
// in internal/api/mcpbridge's package doc, "A call longer than its harness waits".

// CallWait is a bridged call a job parked on ([PendingRun.ParkedCalls]): the
// tool it called and its place among the run's calls ([BridgeCall.Seq]).
//
// A SECTION WITH AN EXTRA OF ITS OWN (wirekeys.go).
type CallWait struct {
	Tool string `json:"tool"`
	Seq  int    `json:"seq"`

	// Ask is whether this park is the job's ask, the job having asked nothing
	// before it ([PendingRun.WaitsOnOwnCalls]): set by the park's write alone
	// ([PendingStore.RecordAsk]), on the first call only. THE MARK LIVES HERE,
	// never in [PendingRun.WaitsFor]: a build without call parks reads a
	// WaitsFor as a wait on the run it names, and answers one naming none as
	// that run gone. Here an older holder parks the row as the question it
	// reads, put to the seat's lead chain, which a person answers truthfully.
	// Neither forward mechanism keeps it from the row: a protocol bump stops
	// only a newer node beside an older lease, and the row is a coordination
	// store value no state-log applier defers.
	Ask bool `json:"ask,omitempty"`

	// Job is the job the call parked ([PendingRun.LaunchID]), written by the
	// park's write whatever the ask carried. A park is its job's alone: a build
	// without call parks keeps the whole section in [PendingRun.Extra] through
	// its relaunch, which names a new job, and a call naming another job is read
	// as none ([PendingRun.forgetOtherJobs]), or the new job would inherit its
	// mark as the ask and its calls as returned.
	Job string `json:"job"`

	// Extra is every key of the section this build does not know: see
	// [RunSteer.Extra].
	Extra map[string]json.RawMessage `json:"-"`
}

// MaxParkedCalls bounds the calls one job parks on ([PendingRun.ParkedCalls]):
// AS MANY AS THE ROW'S SHARE FOR THEM HOLDS ([parkedRowBytes]), each at its
// worst ([parkedCallBytes]), once the field's own key and brackets are out:
// (13,312 - 18) / 862 = 15. The bridge admits no call that could park past it
// (internal/api/mcpbridge's binding.parksFullLocked), so a call past it is
// answered at once and not run; a park past it is still refused
// ([ErrParkedCallsFull]), and that call answered still running with no promise.
const MaxParkedCalls = (parkedRowBytes - len(`,"parked_calls":[]`)) / parkedCallBytes

// parkedCallBytes is one parked call at its worst as the row holds it: a tool
// name at [coord.MaxMCPToolNameBytes], each byte escaped to six (`\u0000`), its
// place at math.MaxInt's nineteen digits, its mark as the job's ask, its job
// (a launch id, which the store mints as a UUID's 36 characters), and the
// comma before the next: 862 bytes.
const parkedCallBytes = len(`{"tool":"`) + 6*coord.MaxMCPToolNameBytes + len(`","seq":`) +
	len("9223372036854775807") + len(`,"ask":true`) + len(`,"job":"`) + launchIDBytes + len(`"`) +
	len(`}`) + len(`,`)

// launchIDBytes is a launch id's length as the store mints it (uuid.NewString).
const launchIDBytes = len("00000000-0000-0000-0000-000000000000")

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
// own calls ([CallWait.Ask]): the ask the engine carries on once the job is
// parked, every call having returned or being past waiting for.
func (r PendingRun) WaitsOnOwnCalls() bool { return len(r.ParkedCalls) > 0 && r.ParkedCalls[0].Ask }

// EngineAnswers reports whether run's ask is put to nobody and carried on by
// the engine: a wait on a run ([PendingRun.WaitsFor]) or on its own calls.
func (r PendingRun) EngineAnswers() bool { return r.WaitsFor != nil || r.WaitsOnOwnCalls() }

// CallsReturned is run's parked calls as its resume tells them
// ([AskedRun.Called]).
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
