package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/api/httpjson"
	"github.com/crewlet/crewlet/internal/coord"
	coordmemory "github.com/crewlet/crewlet/internal/coord/memory"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// fakeBackupRegister records the point the acknowledgement wrote.
type fakeBackupRegister struct {
	point coord.BackupPoint
	calls int
}

func (f *fakeBackupRegister) PutBackupPoint(_ context.Context, p coord.BackupPoint) error {
	f.calls++
	f.point = p
	return nil
}

// fakeStateLog answers for the domains one node is running, keyed by STREAM —
// which is the key space an acknowledgement is filed under, and the one
// neither the published floors nor the positions register has.
//
// PER-STREAM GENERATIONS, because that is the estate a reanchor produces: the
// verb names one stream, so a fleet that has re-anchored its tracker and not
// its pages log stands at two different generations at once.
//
// SAFE FOR CONCURRENT REQUESTS, because every app a fixture builds without one
// is handed a fresh one of these and a case's parallel subtests share that app:
// the posture matrix's rows reach the acknowledgement and the reanchor read at
// once, and each writes what it was asked. A case reads the fields after its
// own synchronous request has answered.
type fakeStateLog struct {
	mu sync.Mutex

	generations map[string]uint32
	asked       string

	// capacity is the last resize asked for, which is where a case about
	// who pressed it looks — and abandoned and excluded are who the last
	// abandon and exclusion were handed.
	capacity  engine.CapacityRequest
	abandoned iam.Actor
	excluded  iam.Actor

	// confirmed is the operation the last abandon's confirmation named, and
	// abandonErr what the engine answers it. abandonClears answers it as an
	// abandonment from `opened`, which clears the window and answers none.
	confirmed     string
	abandonErr    error
	abandonClears bool

	// openWindows and unreadWindows are every window open on any log and
	// every log whose window couldn't be read, as a gesture's answer names
	// them.
	openWindows   []coord.MaintenanceOperation
	unreadWindows []string

	// window is the operation the capacity status reads, with the
	// acknowledgements and admissions beside it; a nil one is unread.
	window     *coord.MaintenanceOperation
	acks       []coord.MaintenanceAck
	admissions []coord.Admission
	// acksErr and admissionsErr are those two reads failing after the
	// window answered.
	acksErr, admissionsErr error

	// unreadable is the broker's answer to a LIVE read of a stream's
	// instant, nil while it answers.
	unreadable error

	// view is the case the status reports, beside the generation above.
	view engine.ReanchorView

	// reanchored is the last reanchor asked for, and plan what it answers.
	reanchored engine.ReanchorRequest
	plan       statelog.ReanchorPlan

	// gestureErr, where set, is what every capacity gesture, the reanchor and
	// the maintenance status answer instead of their defaults.
	gestureErr error
}

func (f *fakeStateLog) ReanchorStatus(_ context.Context, stream string) (
	engine.ReanchorView, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	generation, err := f.generationOf(stream)
	if err != nil {
		return engine.ReanchorView{}, err
	}
	if f.unreadable != nil {
		return engine.ReanchorView{}, fmt.Errorf("engine: read %s's creation "+
			"instant: %w", stream, f.unreadable)
	}
	view := f.view
	view.CreatedAt, view.Generation = time.Unix(1700000000, 0).UTC(), generation
	return view, nil
}

func (f *fakeStateLog) StreamGeneration(stream string) (uint32, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.generationOf(stream)
}

// generationOf is StreamGeneration with the lock already held, so the status
// read — which needs the generation too — takes the lock once.
func (f *fakeStateLog) generationOf(stream string) (uint32, error) {
	f.asked = stream
	generation, runs := f.generations[stream]
	if !runs {
		streams := slices.Sorted(maps.Keys(f.generations))
		return 0, &engine.UnknownStreamError{Stream: stream, Streams: streams}
	}
	return generation, nil
}

func (f *fakeStateLog) Reanchor(_ context.Context, req engine.ReanchorRequest) (
	statelog.ReanchorPlan, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	f.reanchored = req
	if f.gestureErr != nil {
		return statelog.ReanchorPlan{}, f.gestureErr
	}
	if f.plan.Generation == 0 {
		return statelog.ReanchorPlan{}, errors.New("not exercised here")
	}
	return f.plan, nil
}

func (f *fakeStateLog) SetCapacity(_ context.Context, req engine.CapacityRequest) (
	coord.MaintenanceOperation, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	f.capacity = req
	if f.gestureErr != nil {
		return coord.MaintenanceOperation{}, f.gestureErr
	}
	// THE REFUSAL A NODE IN NORMAL MODE GIVES, which [fakeStateLog.Mode] is.
	return coord.MaintenanceOperation{}, &statelog.Telling{Say: "this node is " +
		"publishing, so a capacity change can't run here"}
}

func (f *fakeStateLog) AbandonCapacity(_ context.Context, _, operationID string,
	by iam.Actor) (coord.MaintenanceOperation, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	f.abandoned = by
	f.confirmed = operationID
	if f.abandonErr != nil {
		return coord.MaintenanceOperation{Stream: "CREWLET_TRACKER_LOG",
			OperationID: "capacity:open", Phase: coord.PhaseBaselined}, f.abandonErr
	}
	if f.gestureErr != nil {
		return coord.MaintenanceOperation{}, f.gestureErr
	}
	if f.abandonClears {
		return coord.MaintenanceOperation{}, nil
	}
	if f.window != nil {
		// AN ABANDONMENT PAST `opened` answers the window it is carrying.
		return *f.window, nil
	}
	return coord.MaintenanceOperation{}, errors.New("not exercised here")
}

func (f *fakeStateLog) ExcludeParticipant(_ context.Context, _, _ string,
	by iam.Actor) (coord.MaintenanceOperation, error) {

	f.mu.Lock()
	defer f.mu.Unlock()
	f.excluded = by
	if f.gestureErr != nil {
		return coord.MaintenanceOperation{}, f.gestureErr
	}
	return coord.MaintenanceOperation{}, errors.New("not exercised here")
}

func (f *fakeStateLog) CapacityStatus(context.Context, string) (engine.CapacityState, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.gestureErr != nil {
		return engine.CapacityState{}, f.gestureErr
	}
	if f.window == nil {
		return engine.CapacityState{}, errors.New("not exercised here")
	}
	return engine.CapacityState{Op: *f.window, Found: true, Acks: f.acks,
		AcksErr: f.acksErr, Admissions: f.admissions, AdmissionsErr: f.admissionsErr}, nil
}

func (f *fakeStateLog) CapacityAcks(context.Context, string) ([]coord.MaintenanceAck, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.acks, f.acksErr
}

func (f *fakeStateLog) CapacityWindows(context.Context) ([]coord.MaintenanceOperation, []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.openWindows, f.unreadWindows
}

func (f *fakeStateLog) Mode() statelog.MaintenanceMode { return statelog.ModeNormal }

// ackApp mounts the retention routes over the two seams the ack needs.
func ackApp(t *testing.T, register *fakeBackupRegister, log *fakeStateLog) *api.App {
	t.Helper()
	b := closedPosture()
	return newApp(t, api.Options{Bootstrap: &b, Retention: register, Capacity: log})
}

// postAck runs one authenticated acknowledgement.
func postAck(t *testing.T, a *api.App, path string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, req)
	var body map[string]any
	_ = json.NewDecoder(rec.Result().Body).Decode(&body)
	return rec.Code, body
}

// A WINDOW THE STATUS READ IS ANSWERED, WHATEVER FAILED AFTER IT. The window is
// read first and the acknowledgements and the admissions after it, and either
// failing was answered as the window's own 503: "couldn't read whether a
// capacity window is open" about a window the node had just read open. Each
// part is said as unread beside the window, and nothing is formed from it.
//
// Mutation: answer a part's failure as the window's 503, or form a list from
// the part that failed, and this fails.
func TestAStatusAnswersTheWindowItReadAndNamesWhatItCouldNot(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		acksErr, admissionsErr error
		unread, absent         []string
	}{
		"acknowledgements": {acksErr: errors.New("store unreachable"),
			unread: []string{"acks_unread"}, absent: []string{"acks", "restarts_missing",
				"admissions_unread"}},
		"admissions": {admissionsErr: errors.New("store unreachable"),
			unread: []string{"admissions_unread"}, absent: []string{"admissions",
				"admissions_blocking", "acks_unread"}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			log := &fakeStateLog{
				window: &coord.MaintenanceOperation{
					Stream: "CREWLET_TRACKER_LOG", OperationID: "capacity:open",
					Phase: coord.PhaseOpened, Attempt: 1,
					Participants: []string{"node-1", "node-2"},
				},
				acksErr: tc.acksErr, admissionsErr: tc.admissionsErr,
			}
			b := closedPosture()
			a := newApp(t, api.Options{Bootstrap: &b, Retention: &fakeBackupRegister{},
				Capacity: log, Nodes: &fakeNodeGate{}})
			req := httptest.NewRequest(http.MethodGet,
				"/worklet/retention/maintenance?stream=CREWLET_TRACKER_LOG", nil)
			req.Header.Set("Authorization", "Bearer secret")
			rec := httptest.NewRecorder()
			a.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("a window read beside a part that failed answered %d: %s",
					rec.Code, rec.Body)
			}
			var body map[string]any
			if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
				t.Fatal(err)
			}
			if body["open"] != true || body["operation"] == nil {
				t.Errorf("the window read is not answered: %v", body)
			}
			for _, key := range tc.unread {
				if body[key] != true {
					t.Errorf("%s = %v, want true", key, body[key])
				}
			}
			for _, key := range tc.absent {
				if _, said := body[key]; said {
					t.Errorf("the status answers %s = %v beside a part it couldn't read",
						key, body[key])
				}
			}
		})
	}
}

// THE FLEET'S CONTROLS RECORD WHO PRESSED THEM: the eviction gate is handed
// the caller whole, and a resize and a re-anchor name the caller's author and
// the credential beside it — as do an abandon and an exclusion, which the
// capacity window records against the gesture.
//
// The gate was handed nothing about its caller, so an eviction recorded this
// node's own writer; a resize recorded one name, the credential's; and an
// abandon and an exclusion recorded nobody, their caller said only in the
// serving node's log. Mutation: hand any of them an empty party, or drop the
// credential from the resize, and the matching half fails.
func TestTheFleetControlsRecordWhoPressedThem(t *testing.T) {
	t.Parallel()
	gate := &fakeNodeGate{}
	log := &fakeStateLog{}
	b := closedPosture()
	a := newApp(t, api.Options{Bootstrap: &b, Retention: &fakeBackupRegister{},
		Capacity: log, Nodes: gate})
	want := iam.Actor{Name: "token:founder", Kind: iam.ActorOperator,
		OperatorID: "token:founder"}

	for _, path := range []string{"/worklet/retention/evict/n1?confirm=n1",
		"/worklet/retention/readmit/n1?confirm=n1"} {
		if code, body := postAck(t, a, path); code != http.StatusOK {
			t.Fatalf("%s answered %d: %v", path, code, body)
		}
	}
	if len(gate.asked) != 2 {
		t.Fatalf("the gate was asked %d times, want an eviction and a readmission",
			len(gate.asked))
	}
	for _, req := range gate.asked {
		// THE PRINCIPAL WHOLE, from which every log's writer takes the
		// author, the kind and the credential — and the grants the org
		// chart's and the identity estate's writers judge the record on.
		if got := iam.ActorFor(req.By); got != want {
			t.Errorf("the gate was written as %+v, want %+v", got, want)
		}
	}

	postAck(t, a, "/worklet/retention/capacity?stream=CREWLET_TRACKER_LOG"+
		"&bytes=1048576&confirm=1048576")
	if log.capacity.By != want.Name || log.capacity.OperatorID != want.OperatorID {
		t.Errorf("the resize names %q through %q, want %q through %q",
			log.capacity.By, log.capacity.OperatorID, want.Name, want.OperatorID)
	}

	postAck(t, a, "/worklet/retention/reanchor?stream=CREWLET_TRACKER_LOG"+
		"&confirm=2031-04-02T03:00:00Z")
	if log.reanchored.By != want {
		t.Errorf("the reanchor was handed %+v, want %+v", log.reanchored.By, want)
	}

	postAck(t, a, "/worklet/retention/maintenance/abandon?stream=CREWLET_TRACKER_LOG"+
		"&confirm=capacity:open")
	if log.abandoned != want {
		t.Errorf("the abandon was handed %+v, want %+v", log.abandoned, want)
	}
	postAck(t, a, "/worklet/retention/maintenance/exclude?stream=CREWLET_TRACKER_LOG"+
		"&node=n1&confirm=n1")
	if log.excluded != want {
		t.Errorf("the exclusion was handed %+v, want %+v", log.excluded, want)
	}
}

// AN ABANDONMENT IS CONFIRMED BEFORE IT RUNS.
//
// ?confirm= repeats the open operation's id. The route refuses one that is
// missing before the engine is asked, and answers the engine's refusal of one
// naming another operation as the same refusal, carrying the operation that is
// open, rather than as a refused capacity change. The CLI used to compare the
// id after the abandonment had already run.
//
// Mutation: drop the missing-confirm check and the first half fails (the
// engine is asked); map the mismatch through the generic refusal and the
// second half fails.
func TestAnAbandonmentIsConfirmedBeforeItRuns(t *testing.T) {
	t.Parallel()
	log := &fakeStateLog{}
	b := closedPosture()
	a := newApp(t, api.Options{Bootstrap: &b, Retention: &fakeBackupRegister{},
		Capacity: log, Nodes: &fakeNodeGate{}})

	code, body := postAck(t, a, "/worklet/retention/maintenance/abandon?stream=CREWLET_TRACKER_LOG")
	if code != http.StatusBadRequest || body["error"] != "confirm_required" {
		t.Fatalf("an abandonment with no confirmation answered %d %v, want 400 "+
			"confirm_required", code, body)
	}
	// THE STATUS READ AS THE CLI TAKES IT, here and on a mismatch below: the
	// CLI refuses one that names no log.
	const status = "`crewlet retention maintenance status -stream CREWLET_TRACKER_LOG`"
	if detail, _ := body["detail"].(string); !strings.Contains(detail, status) {
		t.Errorf("the refusal %q doesn't name %s", detail, status)
	}
	if log.abandoned != (iam.Actor{}) {
		t.Fatalf("the engine was asked to abandon with no confirmation: %+v", log.abandoned)
	}

	log.abandonErr = &engine.ConfirmMismatchError{Stream: "CREWLET_TRACKER_LOG",
		Open: "capacity:open", Named: "capacity:other"}
	code, body = postAck(t, a, "/worklet/retention/maintenance/abandon?stream=CREWLET_TRACKER_LOG"+
		"&confirm=capacity:other")
	if code != http.StatusBadRequest || body["error"] != "confirm_required" {
		t.Fatalf("a confirmation naming another operation answered %d %v, want "+
			"400 confirm_required", code, body)
	}
	if log.confirmed != "capacity:other" {
		t.Errorf("the engine was handed confirmation %q, want the one sent", log.confirmed)
	}
	detail, _ := body["detail"].(string)
	if !strings.Contains(detail, "capacity:open") || !strings.Contains(detail, status) {
		t.Errorf("the refusal %q doesn't name the operation that is open and %s",
			detail, status)
	}
	if op, _ := body["operation"].(map[string]any); op["operation_id"] != "capacity:open" {
		t.Errorf("the refusal carries operation %v, want the open one", body["operation"])
	}
}

// A BLOCKED OPERATION CARRIES ITS SENTENCE BESIDE ITS CODE, on every answer
// that carries one.
//
// The operation's `blocked` is a code for scripts, and every client printed it
// as the reason a window needed a person. Each answer carrying an operation
// now carries `blocked_detail` beside it ([statelog.BlockedDetail]), so a
// client shows the node's words and never has to make its own. An abandoned
// window's sentence is decided by its seal, so every answer reads the window's
// acknowledgements for it. Mutation: answer the bare record from the status or
// from a gesture, or word a gesture's window without its acknowledgements, and
// its half fails.
func TestABlockedOperationCarriesItsSentenceBesideItsCode(t *testing.T) {
	t.Parallel()
	window := coord.MaintenanceOperation{
		Stream: "CREWLET_TRACKER_LOG", OperationID: "capacity:open",
		Phase: coord.PhaseSealing, Attempt: 1, TargetMaxBytes: 8 << 30,
		Participants: []string{"node-1"}, Blocked: coord.BlockAbandoned,
		AbandonedBy:       coord.MaintenanceParty{By: "ana.admin"},
		WriteIncarnations: map[string]string{"node-1": "node-1:old"},
	}
	// THE SEAL HOLDS on these, so the sentence says one set-capacity closes
	// the window, which only an answer that read them can say.
	acks := []coord.MaintenanceAck{{Stream: "CREWLET_TRACKER_LOG", NodeID: "node-1",
		OperationID: "capacity:open", Attempt: 1, Incarnation: "node-1:new", Mode: "seal"}}
	log := &fakeStateLog{window: &window, acks: acks}
	b := closedPosture()
	a := newApp(t, api.Options{Bootstrap: &b, Retention: &fakeBackupRegister{},
		Capacity: log, Nodes: &fakeNodeGate{}})
	for name, req := range map[string]*http.Request{
		"the status": httptest.NewRequest(http.MethodGet,
			"/worklet/retention/maintenance?stream=CREWLET_TRACKER_LOG", nil),
		"an abandonment": httptest.NewRequest(http.MethodPost,
			"/worklet/retention/maintenance/abandon?stream=CREWLET_TRACKER_LOG&confirm=capacity:open", nil),
	} {
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		a.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s answered %d: %s", name, rec.Code, rec.Body)
		}
		var body struct {
			Operation struct {
				Blocked       string `json:"blocked"`
				BlockedDetail string `json:"blocked_detail"`
			} `json:"operation"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body.Operation.Blocked != string(coord.BlockAbandoned) {
			t.Errorf("%s's operation carries blocked %q, want the code for scripts",
				name, body.Operation.Blocked)
		}
		if want := statelog.BlockedDetail(window, acks, nil); body.Operation.BlockedDetail != want ||
			!strings.Contains(want, "waiting on no node") {
			t.Errorf("%s's operation carries blocked_detail %q, want %q", name,
				body.Operation.BlockedDetail, want)
		}
	}
}

// A WINDOW'S STATUS ANSWERS WHAT ITS NEXT STEP WAITS ON.
//
// At opened the next step is the baseline, which reads a restart into a
// maintenance mode from every participant and the admissions, so that is what
// the status names. The seal's list there named every node restarted before
// the window as owing a seal restart the procedure hadn't asked for yet. Past
// opened the seal is the next step and no step reads an admission again, so
// the answer swaps; once it has sealed, nothing waits on a node.
//
// Mutation: answer the seal's list at every phase and the opened half fails;
// answer the admissions at every phase and the baselined half fails.
func TestAWindowsStatusAnswersWhatItsNextStepWaitsOn(t *testing.T) {
	t.Parallel()
	log := &fakeStateLog{
		window: &coord.MaintenanceOperation{
			Stream: "CREWLET_TRACKER_LOG", OperationID: "capacity:open",
			Phase: coord.PhaseOpened, Attempt: 1,
			Participants: []string{"node-1", "node-2", "node-3"},
		},
		acks: []coord.MaintenanceAck{
			{Stream: "CREWLET_TRACKER_LOG", NodeID: "node-1", Incarnation: "node-1:2",
				Mode: "maintenance"},
		},
		admissions: []coord.Admission{{NodeID: "node-3", Incarnation: "node-3:1"}},
	}
	b := closedPosture()
	a := newApp(t, api.Options{Bootstrap: &b, Retention: &fakeBackupRegister{},
		Capacity: log, Nodes: &fakeNodeGate{}})
	read := func() map[string]any {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet,
			"/worklet/retention/maintenance?stream=CREWLET_TRACKER_LOG", nil)
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		a.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("the status answered %d: %s", rec.Code, rec.Body)
		}
		var body map[string]any
		if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		return body
	}

	opened := read()
	if got := fmt.Sprint(opened["restarts_missing"]); got != "[node-2 node-3]" {
		t.Errorf("restarts_missing at opened = %s, want node-2 and node-3", got)
	}
	if got := fmt.Sprint(opened["admissions_blocking"]); got != "[node-3]" {
		t.Errorf("admissions_blocking at opened = %s, want node-3", got)
	}
	if _, said := opened["blocking"]; said {
		t.Errorf("the status at opened names the seal's list: %v", opened["blocking"])
	}

	log.mu.Lock()
	log.window.Phase = coord.PhaseBaselined
	log.window.WriteIncarnations = map[string]string{"node-1": "node-1:2",
		"node-2": "node-2:2", "node-3": "node-3:2"}
	log.mu.Unlock()
	baselined := read()
	if blocking, _ := baselined["blocking"].(string); !strings.Contains(blocking, "node-2") {
		t.Errorf("blocking past opened = %q, want the seal's list naming node-2", blocking)
	}
	if _, said := baselined["admissions_blocking"]; said {
		t.Errorf("the status past opened names admissions no step reads: %v",
			baselined["admissions_blocking"])
	}
	if _, said := baselined["restarts_missing"]; said {
		t.Errorf("the status past opened names the baseline's list: %v",
			baselined["restarts_missing"])
	}

	// AND ONCE IT HAS SEALED IT IS SEALED, whatever the nodes wrote since:
	// the seal's rule applied to node-2 restarted again said "not sealed"
	// under PHASE sealed, and nothing after the seal waits on a node.
	// Mutation: judge sealed and confirmed by the seal's rule and this fails.
	for _, phase := range []coord.MaintenancePhase{coord.PhaseSealed, coord.PhaseConfirmed} {
		log.mu.Lock()
		log.window.Phase = phase
		log.mu.Unlock()
		after := read()
		if after["sealed"] != true {
			t.Errorf("the status at %s says sealed is %v", phase, after["sealed"])
		}
		if blocking, said := after["blocking"]; said {
			t.Errorf("the status at %s names the seal's list: %v", phase, blocking)
		}
	}
}

// A GESTURE THAT CLOSES A WINDOW NAMES EVERY WINDOW STILL OPEN, and every log
// whose window couldn't be read.
//
// A node won't start in service while any log has a window open, or while it
// can't read whether one has, and only the node can say which: the CLI told
// an operator to restart in service whenever its own window closed, and with
// a second log's window still open every node refused to boot. An abandonment
// from `opened` clears its window, so it answers no operation, and the
// windows beside it.
//
// Mutation: drop the windows from the answer and this fails.
func TestAClosingGestureNamesTheWindowsStillOpen(t *testing.T) {
	t.Parallel()
	log := &fakeStateLog{
		abandonClears: true,
		openWindows: []coord.MaintenanceOperation{{
			Stream: "CREWLET_PAGES_LOG", OperationID: "capacity:pages",
			Phase: coord.PhaseApplied, TargetMaxBytes: 1 << 33,
		}},
		unreadWindows: []string{"CREWLET_VECTORS_LOG"},
	}
	b := closedPosture()
	a := newApp(t, api.Options{Bootstrap: &b, Retention: &fakeBackupRegister{},
		Capacity: log, Nodes: &fakeNodeGate{}})
	code, body := postAck(t, a, "/worklet/retention/maintenance/abandon?"+
		"stream=CREWLET_TRACKER_LOG&confirm=capacity:open")
	if code != http.StatusOK {
		t.Fatalf("the abandonment answered %d %v", code, body)
	}
	if op, said := body["operation"]; !said || op != nil {
		t.Errorf("a cleared window answered operation %v, want none", op)
	}
	if got := fmt.Sprint(body["open_windows"]); got != "[map[operation_id:capacity:pages "+
		"phase:applied stream:CREWLET_PAGES_LOG target_max_bytes:8.589934592e+09]]" {
		t.Errorf("open_windows = %s, want the pages window", got)
	}
	if got := fmt.Sprint(body["unread_windows"]); got != "[CREWLET_VECTORS_LOG]" {
		t.Errorf("unread_windows = %s, want the log nobody could read", got)
	}
}

// AN ACKNOWLEDGEMENT IS STAMPED WITH THE NAMED STREAM'S OWN GENERATION.
//
// The point is filed under a stream and the trim looks its reach up by stream,
// but the fleet's registers are keyed by DOMAIN and carry no stream name at
// all — so the route used to stamp the point with the first row of whichever
// register answered. On an estate where one domain has been re-anchored and
// another has not, that is a different log's number: the trim discards a point
// BELOW its generation and reads one at or above it as covering the log, so a
// neighbour's higher generation counts, at a sequence from a number space that
// is not this log's, and the trim deletes records the copy does not contain.
func TestAnAcknowledgementCarriesTheNamedStreamsOwnGeneration(t *testing.T) {
	t.Parallel()
	register := &fakeBackupRegister{}
	// The tracker log is listed first and stands one reanchor behind the
	// pages log, which is exactly the estate the old first-row read got
	// wrong.
	node := &fakeStateLog{generations: map[string]uint32{
		"CREWLET_TRACKER_LOG": 3,
		"CREWLET_PAGES_LOG":   7,
	}}

	code, body := postAck(t, ackApp(t, register, node),
		"/worklet/retention/ack?stream=CREWLET_PAGES_LOG&position=918100000")
	if code != http.StatusOK {
		t.Fatalf("the route answered %d: %v", code, body)
	}
	if node.asked != "CREWLET_PAGES_LOG" {
		t.Errorf("the generation was read for %q rather than the stream the "+
			"caller named", node.asked)
	}
	if body["generation"] != float64(7) {
		t.Errorf("the answer echoes generation %v, so an operator cannot see "+
			"which number space was acknowledged", body["generation"])
	}
	at, covers := register.point.Streams["CREWLET_PAGES_LOG"]
	if !covers {
		t.Fatalf("the point covers %v rather than the stream named",
			register.point.Streams)
	}
	if at.Generation != 7 {
		t.Errorf("the point was stamped at generation %d, which is another "+
			"log's number space — the trim reads a point at or above its own "+
			"generation as covering the log and would delete records this "+
			"backup does not contain", at.Generation)
	}
	if at.Seq != 918100000 || at.Stream != "CREWLET_PAGES_LOG" {
		t.Errorf("the point reaches %s@%d", at.Stream, at.Seq)
	}
	if !register.point.Verified || register.point.Owner != coord.OperatorBackupOwner {
		t.Errorf("the point is owned by %q (verified: %v), and under "+
			"backup_floor: operator only a verified operator point counts",
			register.point.Owner, register.point.Verified)
	}
}

// EVERY LOG'S ACKNOWLEDGEMENT STANDS BESIDE THE OTHERS.
//
// Under `backup_floor: operator` the procedure is one acknowledgement per
// log, in turn. The route writes each one to the fleet's register, and
// the register kept ONE operator row: each acknowledgement replaced the last,
// so following the procedure left exactly one log covered — whichever was
// acknowledged last — and the trim blocked on the others. Here the route writes
// to the real register, and every log acknowledged must still be covered, at
// its own reach, after the last.
func TestEveryLogsAcknowledgementStandsBesideTheOthers(t *testing.T) {
	t.Parallel()
	fleet := coordmemory.NewFleet()
	node := &fakeStateLog{generations: map[string]uint32{
		"CREWLET_TRACKER_LOG":     3,
		"CREWLET_PAGES_LOG":       7,
		"CREWLET_TRACKER_VECTORS": 1,
	}}
	b := closedPosture()
	a := newApp(t, api.Options{Bootstrap: &b, Retention: fleet, Capacity: node})
	acks := []struct {
		stream     string
		seq        uint64
		generation uint32
	}{
		{"CREWLET_TRACKER_LOG", 918100000, 3},
		{"CREWLET_PAGES_LOG", 40400, 7},
		{"CREWLET_TRACKER_VECTORS", 77000, 1},
	}
	for _, ack := range acks {
		code, body := postAck(t, a, fmt.Sprintf("/worklet/retention/ack?stream=%s&position=%d",
			ack.stream, ack.seq))
		if code != http.StatusOK {
			t.Fatalf("acknowledging %s answered %d: %v", ack.stream, code, body)
		}
	}

	points, err := fleet.BackupPoints(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	counted := coord.CountedBackups(points, true)
	for _, ack := range acks {
		newest, ok := coord.NewestBackupOf(counted, ack.stream)
		if !ok {
			t.Errorf("%s is covered by no acknowledgement after all three were "+
				"given — a later one replaced it (register %+v)", ack.stream, points)
			continue
		}
		at := newest.Streams[ack.stream]
		if at.Seq != ack.seq || at.Generation != ack.generation {
			t.Errorf("%s is acknowledged at %d (generation %d), want its own %d "+
				"(generation %d)", ack.stream, at.Seq, at.Generation, ack.seq,
				ack.generation)
		}
		if len(newest.Streams) != 1 {
			t.Errorf("the acknowledgement covering %s reaches %v — each names the "+
				"one log it was given for", ack.stream, newest.Streams)
		}
	}
}

// A STREAM THIS NODE DOES NOT RUN IS REFUSED, rather than acknowledged at some
// other log's generation.
//
// The registers answered a mistyped stream name as confidently as a real one,
// so the acknowledgement came back 200 with a generation — and the point it
// wrote was one nothing would ever read.
func TestAnAcknowledgementNamingAnUnknownStreamIsRefused(t *testing.T) {
	t.Parallel()
	register := &fakeBackupRegister{}
	node := &fakeStateLog{generations: map[string]uint32{"CREWLET_TRACKER_LOG": 3}}

	code, body := postAck(t, ackApp(t, register, node),
		"/worklet/retention/ack?stream=CREWLET_TRAKCER_LOG&position=918100000")
	// 404 rather than 503: nothing here is transient, and a 503 tells an
	// operator who typed the name wrong to send the identical request
	// again in a minute.
	if code != http.StatusNotFound {
		t.Fatalf("the route answered %d: %v", code, body)
	}
	if body["error"] != "unknown_stream" {
		t.Errorf("the refusal reads %q, and the CLI prints only this field",
			body["error"])
	}
	if register.calls != 0 {
		t.Errorf("a point was written anyway: %v", register.point)
	}
}

// A RETENTION ROUTE TELLS ITS OPERATOR THE REFUSAL AND KEEPS THE FAULT FOR ITS
// LOG.
//
// Every one of these routes answered `err.Error()` as its detail, so an
// operator's command line printed the package that wrote the refusal
// ("engine: ...", "statelog: reanchor refused: ...") and, where a read had
// failed underneath, the broker's own words. A refusal the engine tells
// ([statelog.Told]) is answered in its sentence alone; anything else is a
// fault, answered in the route's own words with a code of its own and never
// with the error's text. Mutation: answer `err.Error()` again at any one of
// these and its case fails.
func TestARetentionRouteTellsItsRefusalAndKeepsItsFaultForTheLog(t *testing.T) {
	t.Parallel()
	raw := errors.New("nats: timeout")
	told := &statelog.Telling{Say: "there's no capacity operation on CREWLET_TRACKER_LOG", Err: raw}
	reanchorTold := &statelog.Telling{Say: "the confirmation names \"x\"", Err: statelog.ErrReanchorRefused}
	// THE ENGINE'S ANSWER TO A STREAM NO WINDOW CAN BE OPEN ON, which every
	// window verb gives before it reads anything (the engine's own suite
	// holds that it does): here is what each route makes of it.
	unknown := &engine.UnknownStreamError{Stream: "NOPE", Streams: []string{"CREWLET_TRACKER_LOG"}}
	cases := []struct {
		name, method, path string
		err                error
		status             int
		code               httpjson.Code
		detail             string // the whole detail, or "" to check only that it is clean
	}{
		{"an unknown stream on an acknowledgement", http.MethodPost,
			"/worklet/retention/ack?stream=NOPE&position=9", nil,
			http.StatusNotFound, httpjson.CodeUnknownStream,
			`"NOPE" isn't a log this node keeps. Use "CREWLET_TRACKER_LOG".`},
		{"an unknown stream on a reanchor status", http.MethodGet,
			"/worklet/retention/reanchor?stream=NOPE", nil,
			http.StatusNotFound, httpjson.CodeUnknownStream,
			`"NOPE" isn't a log this node keeps. Use "CREWLET_TRACKER_LOG".`},
		{"a capacity refusal", http.MethodPost,
			"/worklet/retention/capacity?stream=CREWLET_TRACKER_LOG&bytes=9&confirm=9", told,
			http.StatusConflict, httpjson.CodeCapacityRefused,
			"There's no capacity operation on CREWLET_TRACKER_LOG."},
		{"a capacity fault", http.MethodPost,
			"/worklet/retention/capacity?stream=CREWLET_TRACKER_LOG&bytes=9&confirm=9", raw,
			http.StatusInternalServerError, httpjson.CodeCapacityFailed,
			"The capacity change on CREWLET_TRACKER_LOG stopped on an error, and " +
				"this node's log says which. Read where it stands with `crewlet " +
				"retention maintenance status -stream CREWLET_TRACKER_LOG` before going on."},
		{"an abandon fault", http.MethodPost,
			"/worklet/retention/maintenance/abandon?stream=CREWLET_TRACKER_LOG&confirm=capacity:open", raw,
			http.StatusInternalServerError, httpjson.CodeCapacityFailed,
			"The abandonment on CREWLET_TRACKER_LOG stopped on an error, and " +
				"this node's log says which. Read where it stands with `crewlet " +
				"retention maintenance status -stream CREWLET_TRACKER_LOG` before going on."},
		{"an exclusion fault", http.MethodPost,
			"/worklet/retention/maintenance/exclude?stream=CREWLET_TRACKER_LOG&node=n1&confirm=n1", raw,
			http.StatusInternalServerError, httpjson.CodeCapacityFailed,
			"The exclusion on CREWLET_TRACKER_LOG stopped on an error, and " +
				"this node's log says which. Read where it stands with `crewlet " +
				"retention maintenance status -stream CREWLET_TRACKER_LOG` before going on."},
		{"an unknown stream on an abandonment", http.MethodPost,
			"/worklet/retention/maintenance/abandon?stream=NOPE&confirm=capacity:open", unknown,
			http.StatusNotFound, httpjson.CodeUnknownStream,
			`"NOPE" isn't a log this node keeps. Use "CREWLET_TRACKER_LOG".`},
		{"an unknown stream on an exclusion", http.MethodPost,
			"/worklet/retention/maintenance/exclude?stream=NOPE&node=n1&confirm=n1", unknown,
			http.StatusNotFound, httpjson.CodeUnknownStream,
			`"NOPE" isn't a log this node keeps. Use "CREWLET_TRACKER_LOG".`},
		{"an unknown stream on a maintenance status", http.MethodGet,
			"/worklet/retention/maintenance?stream=NOPE", unknown,
			http.StatusNotFound, httpjson.CodeUnknownStream,
			`"NOPE" isn't a log this node keeps. Use "CREWLET_TRACKER_LOG".`},
		{"an exclusion refusal", http.MethodPost,
			"/worklet/retention/maintenance/exclude?stream=CREWLET_TRACKER_LOG&node=n1&confirm=n1", told,
			http.StatusConflict, httpjson.CodeCapacityRefused,
			"There's no capacity operation on CREWLET_TRACKER_LOG."},
		{"a reanchor refusal", http.MethodPost,
			"/worklet/retention/reanchor?stream=CREWLET_TRACKER_LOG&confirm=x", reanchorTold,
			http.StatusConflict, httpjson.CodeReanchorRefused,
			`The confirmation names "x".`},
		{"a reanchor fault", http.MethodPost,
			"/worklet/retention/reanchor?stream=CREWLET_TRACKER_LOG&confirm=x", raw,
			http.StatusInternalServerError, httpjson.CodeReanchorFailed, ""},
		{"an unreadable maintenance window", http.MethodGet,
			"/worklet/retention/maintenance?stream=CREWLET_TRACKER_LOG", nil,
			http.StatusServiceUnavailable, httpjson.CodeMaintenanceUnreadable,
			"Couldn't read whether a capacity window is open on CREWLET_TRACKER_LOG " +
				"just now. Ask again in a moment, and if it keeps failing, this " +
				"node's log says why."},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			node := &fakeStateLog{generations: map[string]uint32{"CREWLET_TRACKER_LOG": 3},
				gestureErr: c.err}
			b := closedPosture()
			a := newApp(t, api.Options{Bootstrap: &b, Retention: &fakeBackupRegister{},
				Capacity: node})
			req := httptest.NewRequest(c.method, c.path, nil)
			req.Header.Set("Authorization", "Bearer secret")
			rec := httptest.NewRecorder()
			a.ServeHTTP(rec, req)
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("the answer is not JSON: %v (%s)", err, rec.Body)
			}
			if rec.Code != c.status || body["error"] != string(c.code) {
				t.Fatalf("answered %d %v, want %d %s: %v", rec.Code, body["error"],
					c.status, c.code, body)
			}
			detail, _ := body["detail"].(string)
			if c.detail != "" && detail != c.detail {
				t.Errorf("detail = %q, want %q", detail, c.detail)
			}
			// A READ A WAIT CLEARS SAYS WHEN TO ASK AGAIN: every 503 here
			// is coordination or the broker not answering.
			if c.status == http.StatusServiceUnavailable && rec.Header().Get("Retry-After") == "" {
				t.Error("a 503 answered no Retry-After")
			}
			for _, leak := range []string{"nats:", "engine:", "statelog:", "not exercised"} {
				if strings.Contains(detail, leak) {
					t.Errorf("detail %q carries %q, which only the log should", detail, leak)
				}
			}
			if detail == "" {
				t.Error("the answer carries no detail")
			}
		})
	}
}

// AN OPERATION ID THE GRAMMAR REFUSES IS TOLD BY WHAT IS WRONG WITH IT, naming
// the field it came in, and never in the framework's own text, which named the
// tool argument a model sends rather than the parameter this route reads.
// Mutation: answer the error's text again and this fails.
func TestAGateRefusalOfAnOperationIDSaysWhatIsWrongWithIt(t *testing.T) {
	t.Parallel()
	b := closedPosture()
	code, body := postAck(t, newApp(t, api.Options{Bootstrap: &b, Nodes: &fakeNodeGate{}}),
		"/worklet/retention/evict/node-4?confirm=node-4&op_id=op-abc")
	if code != http.StatusBadRequest || body["field"] != "op_id" {
		t.Fatalf("an invented op_id answered %d %v, want 400 naming the field", code, body)
	}
	want := "The op_id is not an operation id this engine accepts, which is a " +
		"UUIDv7, optionally followed by a dot and a name. Send back the op_id an " +
		"earlier answer returned, unchanged, or leave it out to start the gesture afresh."
	if body["detail"] != want {
		t.Errorf("detail = %q, want %q", body["detail"], want)
	}
}

// A GATE THAT FAILED BEFORE ANYTHING WAS WRITTEN SAYS SO AND HANDS BACK ITS
// OPERATION, so the same request can be sent again: the detail is the route's
// words, the fault is the log's. Mutation: answer the error's text again, or
// drop the op_id, and this fails.
func TestAGateFaultSaysNothingWasWrittenAndHandsBackItsOperation(t *testing.T) {
	t.Parallel()
	b := closedPosture()
	gate := &fakeNodeGate{readmit: errors.New("engine: read the floors: nats: timeout")}
	code, body := postAck(t, newApp(t, api.Options{Bootstrap: &b, Nodes: gate}),
		"/worklet/retention/readmit/node-4?confirm=node-4")
	if code != http.StatusInternalServerError || body["error"] != "gate_failed" {
		t.Fatalf("an untyped failure answered %d %v, want 500 gate_failed", code, body)
	}
	detail, _ := body["detail"].(string)
	if strings.Contains(detail, "nats") || strings.Contains(detail, "engine:") ||
		!strings.Contains(detail, "nothing was written") {
		t.Errorf("detail = %q, want the route's own words saying nothing was written", detail)
	}
	if op, _ := body["op_id"].(string); op == "" || body["node"] != "node-4" {
		t.Errorf("the answer carries op_id %v and node %v, want both", body["op_id"], body["node"])
	}
}

// THE DEPLOYMENT'S CONTROLS REFUSE IN THE ENVELOPE EVERY OTHER ROUTE USES.
//
// A backup, a budget reset, the retention gestures and the capacity window
// each wrote a bare `{"error": code}` of their own, so the surface an operator
// reaches for when something is already wrong was the one whose refusals
// carried no `message` — a screen rendering the envelope had nothing to show
// for it, and nothing held these codes to the vocabulary at all. Each case is
// a refusal the handler itself makes, past the grant, so what is asserted is
// the handler's answer rather than the guard's.
func TestTheDeploymentControlsRefuseInTheEnvelope(t *testing.T) {
	t.Parallel()
	node := &fakeStateLog{generations: map[string]uint32{"CREWLET_TRACKER_LOG": 3}}
	b := closedPosture()
	a := newApp(t, api.Options{
		Bootstrap: &b,
		Backup:    &fakeBackup{err: errors.New("the disk went away")},
		Retention: &fakeBackupRegister{},
		Capacity:  node,
	})

	cases := []struct {
		method, path string
		status       int
		code         httpjson.Code
	}{
		{http.MethodPost, "/backup", http.StatusBadRequest, httpjson.CodeNoDestination},
		{http.MethodPost, "/backup?dir=/tmp/x", http.StatusInternalServerError, httpjson.CodeBackupFailed},
		{http.MethodPost, "/worklet/retention/ack?stream=CREWLET_TRACKER_LOG", http.StatusBadRequest, httpjson.CodePositionRequired},
		{http.MethodPost, "/worklet/retention/ack?stream=NOPE&position=9", http.StatusNotFound, httpjson.CodeUnknownStream},
		{http.MethodPost, "/worklet/retention/evict/node-2", http.StatusBadRequest, httpjson.CodeConfirmRequired},
		{http.MethodGet, "/worklet/retention/reanchor", http.StatusBadRequest, httpjson.CodeStreamRequired},
		{http.MethodPost, "/worklet/retention/reanchor?stream=CREWLET_TRACKER_LOG", http.StatusBadRequest, httpjson.CodeConfirmRequired},
		{http.MethodPost, "/worklet/retention/capacity?stream=CREWLET_TRACKER_LOG", http.StatusBadRequest, httpjson.CodeTargetRequired},
		{http.MethodPost, "/worklet/retention/capacity?stream=CREWLET_TRACKER_LOG&bytes=1024&confirm=1024", http.StatusConflict, httpjson.CodeCapacityRefused},
		{http.MethodGet, "/worklet/retention/maintenance?stream=CREWLET_TRACKER_LOG", http.StatusServiceUnavailable, httpjson.CodeMaintenanceUnreadable},
	}
	for _, tc := range cases {
		t.Run(tc.method+" "+tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, nil)
			req.Header.Set("Authorization", "Bearer secret")
			rec := httptest.NewRecorder()
			a.ServeHTTP(rec, req)
			var body map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
				t.Fatalf("the refusal is not JSON: %v (%s)", err, rec.Body)
			}
			if rec.Code != tc.status || body["error"] != string(tc.code) {
				t.Fatalf("answered %d %v, want %d %s", rec.Code, body["error"],
					tc.status, tc.code)
			}
			if got := rec.Header().Get("Content-Type"); got != "application/json" {
				t.Errorf("Content-Type = %q", got)
			}
			if tc.code.Message() == "" || body["message"] != tc.code.Message() {
				t.Errorf("message = %v, want the vocabulary's sentence for %s — "+
					"a refusal a person is shown nothing for", body["message"], tc.code)
			}
		})
	}
}

// fakeNodeGate answers the two gestures with what it was built with and
// remembers what it was asked.
//
// SAFE FOR CONCURRENT CALLS, because the app every default test builds holds
// one and the posture matrix drives its routes from parallel subtests: an
// unguarded append raced there.
type fakeNodeGate struct {
	readmit error
	evict   error

	// result is what a gesture past the judgement answers; empty is every
	// log applied.
	result *engine.GateResult

	mu    sync.Mutex
	asked []engine.GateRequest
}

func (f *fakeNodeGate) answer(req engine.GateRequest, verb string, refusal error) (engine.GateResult, error) {
	f.mu.Lock()
	f.asked = append(f.asked, req)
	f.mu.Unlock()
	if refusal != nil {
		return engine.GateResult{}, refusal
	}
	if f.result != nil {
		out := *f.result
		out.Node, out.OpID = req.Node, req.OpID
		return out, nil
	}
	// EVERY LOG A NODE'S GESTURE WRITES, applied ([gateLogs]).
	var domains []engine.DomainGate
	for i, domain := range gateLogs() {
		domains = append(domains, appliedOn(i, domain, req.OpID, verb, req.Node))
	}
	return engine.GateResult{Node: req.Node, OpID: req.OpID, Domains: domains}, nil
}

func (f *fakeNodeGate) Evict(_ context.Context, req engine.GateRequest) (engine.GateResult, error) {
	return f.answer(req, "evict", f.evict)
}

func (f *fakeNodeGate) Readmit(_ context.Context, req engine.GateRequest) (engine.GateResult, error) {
	return f.answer(req, "readmit", f.readmit)
}

// A READMISSION REFUSED BELOW THE FLOOR IS A 409 CARRYING BOTH NUMBERS, and a
// failure stays a failure.
//
// The refusal is the answer the operator documentation promises, and `crewlet
// retention readmit` prints the error, the detail and the hint beneath the
// status. Reported as the route's generic `500 gate_failed`, it read as an
// engine fault where there is a node still catching up — and the numbers that
// are the whole reason sat unlabelled in a wrapped error string.
func TestAReadmissionBelowTheFloorIsRefusedAsAnAnswer(t *testing.T) {
	t.Parallel()
	b := closedPosture()
	refusal := &statelog.ReadmissionRefusal{
		NodeID: "node-4", Domain: "tracker", Published: true,
		Generation: 2, Seq: 1_200,
		Bound: statelog.ReadmissionBound{Domain: "tracker", Generation: 2,
			Floor: 9_000, First: 8_800},
	}
	gate := &fakeNodeGate{readmit: fmt.Errorf("engine: readmit node node-4: %w", refusal)}
	a := newApp(t, api.Options{Bootstrap: &b, Nodes: gate})

	code, body := postAck(t, a, "/worklet/retention/readmit/node-4?confirm=node-4")
	if code != http.StatusConflict {
		t.Fatalf("a refused readmission answered %d: %v", code, body)
	}
	if body["error"] != "readmission_refused" {
		t.Errorf("error = %v, want readmission_refused", body["error"])
	}
	detail, _ := body["detail"].(string)
	if detail != httpjson.Sentence(refusal.Tell()) {
		t.Errorf("detail = %q, want the refusal's own sentence %q", detail, refusal.Tell())
	}
	if hint, _ := body["hint"].(string); hint != httpjson.Sentence(refusal.Remedy().Detail) {
		t.Errorf("hint = %q, want the refusal's remedy", hint)
	}
	// WHAT TO DO, AS A VALUE A SURFACE SWITCHES ON: waiting for the node to
	// catch up — never forcing, which a readmission has no meaning for.
	if got := actionsOf(body); len(got) != 1 || got[0] != string(statelog.GateWait) {
		t.Errorf("actions = %v, want only wait", got)
	}
	for field, want := range map[string]any{
		"node": "node-4", "domain": "tracker", "position": float64(1_200),
		"floor": float64(9_000), "first_seq": float64(8_800), "published": true,
	} {
		if body[field] != want {
			t.Errorf("%s = %v, want %v", field, body[field], want)
		}
	}

	// AND A JUDGEMENT NOBODY COULD MAKE IS NOT A REFUSAL OF THE NODE: it is
	// a 503 carrying what to do, as an unjudged eviction is — never a 500
	// an operator reads as an engine fault.
	gate.readmit = fmt.Errorf("engine: readmit node node-4: %w", &engine.ReadmissionUnjudged{
		Node: "node-4", Err: errors.New("read the positions register: unreachable")})
	code, body = postAck(t, a, "/worklet/retention/readmit/node-4?confirm=node-4")
	if code != http.StatusServiceUnavailable || body["error"] != "readmission_unjudged" {
		t.Fatalf("an unreadable register answered %d %v, want 503 readmission_unjudged", code, body)
	}
	if got := actionsOf(body); !slices.Equal(got, []string{string(statelog.GateRetrySameOp),
		string(statelog.GateOtherNode)}) {
		t.Errorf("actions = %v, want the same gesture again, here or through another node", got)
	}

	// AND A FAILURE THE ENGINE DID NOT TYPE STAYS A FAILURE.
	gate.readmit = errors.New("engine: something nobody anticipated")
	if code, body := postAck(t, a, "/worklet/retention/readmit/node-4?confirm=node-4"); code !=
		http.StatusInternalServerError || body["error"] != "gate_failed" {
		t.Fatalf("an untyped failure answered %d %v, want 500 gate_failed", code, body)
	}
}

// AN EVICTION OF A NODE STILL HOLDING ITS PRESENCE LEASE IS A 409, and
// `force=true` is what the engine is asked to override it with — never
// assumed, and never sent on a readmission.
//
// The judgement existed and nothing called it: the route evicted whatever node
// id it was given, so a mistyped id took a running machine out of the fleet.
func TestAnEvictionOfALiveNodeIsRefusedAsAnAnswer(t *testing.T) {
	t.Parallel()
	b := closedPosture()
	refusal := &statelog.EvictionRefusal{NodeID: "node-4", Detail: "it holds a live presence lease"}
	gate := &fakeNodeGate{evict: fmt.Errorf("engine: evict node node-4: %w", refusal)}
	a := newApp(t, api.Options{Bootstrap: &b, Nodes: gate})

	code, body := postAck(t, a, "/worklet/retention/evict/node-4?confirm=node-4")
	if code != http.StatusConflict || body["error"] != "eviction_refused" {
		t.Fatalf("a refused eviction answered %d %v, want 409 eviction_refused", code, body)
	}
	if hint, _ := body["hint"].(string); hint != httpjson.Sentence(refusal.Remedy().Detail) {
		t.Errorf("hint = %q, want the refusal's remedy %q", hint, refusal.Remedy().Detail)
	}
	// FORCE IS OFFERED AS AN ACTION, which is what lets a surface with no
	// -force flag — the dashboard — show the control that sends force=true.
	if got := actionsOf(body); !slices.Contains(got, string(statelog.GateForce)) {
		t.Errorf("actions = %v, want force among them", got)
	}
	if len(gate.asked) != 1 || gate.asked[0].Force {
		t.Fatalf("the engine was asked %+v, want one unforced eviction", gate.asked)
	}

	gate.evict = nil
	if code, body := postAck(t, a,
		"/worklet/retention/evict/node-4?confirm=node-4&force=true"); code != http.StatusOK {
		t.Fatalf("a forced eviction answered %d %v", code, body)
	}
	if !gate.asked[1].Force {
		t.Fatal("force=true never reached the engine")
	}
	if code, _ := postAck(t, a,
		"/worklet/retention/readmit/node-4?confirm=node-4&force=true"); code != http.StatusOK ||
		gate.asked[2].Force {
		t.Fatalf("a readmission carried force (%+v), which it has no meaning for", gate.asked[2])
	}
}

// THE GESTURE ANSWERS PER LOG, NAMES WHO RAN IT, AND CARRIES THE OPERATION ID
// A RETRY FINISHES IT WITH.
//
// An eviction is a record on every identity-claiming log, and the logs answer
// independently — so the route reports each one's three-valued outcome, or the
// refusal that stopped it, and `complete` says whether every log holds the
// record. A gesture that reached the tracker's log and not the pages log is a
// 200 with `complete: false`, not a failure: the tracker's record stands, and
// the same request sent again with the `op_id` it answered with is what writes
// the rest.
func TestTheGateAnswersPerLogAndCarriesItsOperation(t *testing.T) {
	t.Parallel()
	b := closedPosture()
	gate := &fakeNodeGate{result: &engine.GateResult{Domains: []engine.DomainGate{
		{Domain: "tracker", Stream: "CREWLET_TRACKER_LOG", OpID: "op-1.evict.tracker",
			Outcome:  statelog.OutcomePending,
			Position: statelog.Position{Stream: "CREWLET_TRACKER_LOG", Seq: 41}},
		{Domain: "pages", Stream: "CREWLET_PAGES_LOG", OpID: "op-1.evict.pages",
			Err: fmt.Errorf("pages: %w", &statelog.Unavailable{
				Reason: statelog.ReasonLogFull, Detail: "the broker refused"})},
	}}}
	a := newApp(t, api.Options{Bootstrap: &b, Nodes: gate})
	op := statelog.NewOpID(time.Now().Add(-time.Minute), "evict-node-4")

	code, body := postAck(t, a, "/worklet/retention/evict/node-4?confirm=node-4&op_id="+
		url.QueryEscape(op))
	if code != http.StatusOK {
		t.Fatalf("a partial gesture answered %d %v, want 200 — the logs that "+
			"answered hold their record", code, body)
	}
	if body["complete"] != false || body["op_id"] != op {
		t.Fatalf("complete = %v, op_id = %v, want false and the caller's own %s",
			body["complete"], body["op_id"], op)
	}
	if len(gate.asked) != 1 || gate.asked[0].OpID != op ||
		gate.asked[0].By.Login != "token:founder" {
		t.Fatalf("the engine was asked %+v, want %s run by the token's own "+
			"principal — every log's record names who ran it", gate.asked, op)
	}
	domains, _ := body["domains"].([]any)
	if len(domains) != 2 {
		t.Fatalf("domains = %v, want one entry per log", body["domains"])
	}
	tracker, _ := domains[0].(map[string]any)
	pages, _ := domains[1].(map[string]any)
	if tracker["outcome"] != "pending" || tracker["op_id"] != "op-1.evict.tracker" {
		t.Errorf("the tracker's entry is %v, want its pending outcome under its "+
			"own derived operation", tracker)
	}
	if position, _ := tracker["position"].(map[string]any); position["seq"] != float64(41) {
		t.Errorf("the tracker's entry carries position %v, want seq 41", tracker["position"])
	}
	if _, has := pages["outcome"]; has || pages["reason"] != "log_full" ||
		pages["error"] == nil {
		t.Errorf("the pages entry is %v, want no outcome, reason log_full and "+
			"the error — a refusal is not one of the three outcomes", pages)
	}
	// WHETHER THE SAME REQUEST AGAIN CAN FINISH A LOG, and what to do
	// otherwise — on the log the gesture did not finish, and on no other.
	if _, has := tracker["actions"]; has {
		t.Errorf("the tracker's entry, which holds its record, carries actions: %v", tracker)
	}
	if got := actionsOf(pages); len(got) != 1 || got[0] != string(statelog.GateSetCapacity) ||
		pages["hint"] == nil {
		t.Errorf("the full pages log answered actions=%v hint=%v, want only "+
			"set_capacity — the same request refuses the same way for ever",
			pages["actions"], pages["hint"])
	}

	// AND WITHOUT ONE, A FRESH OPERATION — which it answers with, because a
	// retry needs it.
	gate.result = nil
	code, body = postAck(t, a, "/worklet/retention/evict/node-4?confirm=node-4")
	if code != http.StatusOK || body["complete"] != true {
		t.Fatalf("a complete gesture answered %d %v", code, body)
	}
	if opID, _ := body["op_id"].(string); opID == "" || opID != gate.asked[1].OpID {
		t.Fatalf("the answer's op_id %v is not the one the engine ran, %q",
			body["op_id"], gate.asked[1].OpID)
	}
	if _, minted := statelog.OpMintedAt(gate.asked[1].OpID); !minted {
		t.Errorf("the route minted %q, which carries no mint instant — every "+
			"log's id derived from it would be refused on a swept ledger",
			gate.asked[1].OpID)
	}
}

// A GATE REQUEST NOBODY COULD CARRY OUT IS THE CALLER'S TO FIX, AND A LEASE
// LISTING THIS NODE COULD NOT READ IS THIS NODE'S — neither is an engine fault.
//
// Both answered `500 gate_failed`: a typo'd node id read as a broken engine,
// and a coordination blip read the same while the way past it — `-force` for a
// node the operator knows is gone — was nowhere in the answer.
func TestAGateTheEngineCouldNotJudgeSaysWhose(t *testing.T) {
	t.Parallel()
	b := closedPosture()
	gate := &fakeNodeGate{evict: fmt.Errorf("%w: \"node*\" is not a node id",
		engine.ErrInvalidGate)}
	a := newApp(t, api.Options{Bootstrap: &b, Nodes: gate})
	if code, body := postAck(t, a, "/worklet/retention/evict/node*?confirm=node*"); code !=
		http.StatusBadRequest || body["error"] != "invalid_gate" {
		t.Fatalf("an invalid gate answered %d %v, want 400 invalid_gate", code, body)
	}

	unjudged := &engine.GateUnjudged{Node: "node-4", Err: errors.New("coordination is unreachable")}
	gate.evict = unjudged
	code, body := postAck(t, a, "/worklet/retention/evict/node-4?confirm=node-4")
	if code != http.StatusServiceUnavailable || body["error"] != "eviction_unjudged" {
		t.Fatalf("an unjudged eviction answered %d %v, want 503 eviction_unjudged", code, body)
	}
	if hint, _ := body["hint"].(string); hint != httpjson.Sentence(unjudged.Remedy().Detail) {
		t.Errorf("hint = %q, want the remedy %q", hint, unjudged.Remedy().Detail)
	}
	got := actionsOf(body)
	if !slices.Contains(got, string(statelog.GateForce)) ||
		!slices.Contains(got, string(statelog.GateRetrySameOp)) {
		t.Errorf("actions = %v, want asking again and forcing — the way past a "+
			"lease listing nobody could read", got)
	}
}

// actionsOf is a refusal's or a log's `actions`, as strings.
func actionsOf(body map[string]any) []string {
	raw, _ := body["actions"].([]any)
	out := make([]string, 0, len(raw))
	for _, a := range raw {
		s, _ := a.(string)
		out = append(out, s)
	}
	return out
}

// THE REANCHOR STATUS TELLS A NAME NOBODY RUNS FROM A LOG NOBODY COULD READ.
//
// The instant it answers is read LIVE from the broker — it is the one the
// confirmation is checked against — so a broker that did not answer is a
// failure of its own: worth retrying, and nothing to do with the name. Folded
// into `unknown_stream`, it sent an operator looking for a typo in a stream
// name that was right. And the acknowledgement route, which only needs the
// generation, must not depend on that broker read at all.
func TestTheReanchorStatusTellsAnUnknownStreamFromAnUnreadableOne(t *testing.T) {
	t.Parallel()
	node := &fakeStateLog{generations: map[string]uint32{"CREWLET_PAGES_LOG": 1}}
	register := &fakeBackupRegister{}
	a := ackApp(t, register, node)
	get := func(path string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		a.ServeHTTP(rec, req)
		var body map[string]any
		_ = json.NewDecoder(rec.Result().Body).Decode(&body)
		return rec.Code, body
	}

	if code, body := get("/worklet/retention/reanchor?stream=CREWLET_PAGES_LOG"); code != http.StatusOK ||
		body["created_at"] == nil || body["generation"] != float64(1) {
		t.Fatalf("a readable stream answered %d: %v", code, body)
	}
	// THE CASE A REANCHOR WOULD ANSWER is in the same answer, so the
	// operator confirms knowing whether the log is followed from its first
	// record or from its end — and, with no case, why there is nothing to do.
	node.view = engine.ReanchorView{Case: statelog.ReanchorRestored, Cursor: 7_000}
	if code, body := get("/worklet/retention/reanchor?stream=CREWLET_PAGES_LOG"); code != http.StatusOK ||
		body["case"] != "restored" || body["cursor"] != float64(7_000) {
		t.Fatalf("a restored stream's status answered %d: %v, want the restored "+
			"case at 7000", code, body)
	}
	node.view = engine.ReanchorView{Refusal: "nothing to re-anchor: the applier follows it"}
	if code, body := get("/worklet/retention/reanchor?stream=CREWLET_PAGES_LOG"); code != http.StatusOK ||
		body["case"] != nil || body["nothing_to_reanchor"] != node.view.Refusal {
		t.Fatalf("a stream with nothing to re-anchor answered %d: %v, want no case "+
			"and the reason", code, body)
	}
	node.view = engine.ReanchorView{}
	if code, body := get("/worklet/retention/reanchor?stream=CREWLET_PAGSE_LOG"); code != http.StatusNotFound ||
		body["error"] != "unknown_stream" {
		t.Fatalf("a stream this node does not run answered %d: %v", code, body)
	}

	node.unreadable = errors.New("nats: timeout")
	code, body := get("/worklet/retention/reanchor?stream=CREWLET_PAGES_LOG")
	if code != http.StatusServiceUnavailable || body["error"] != "stream_unreadable" {
		t.Fatalf("a stream the broker did not answer for answered %d: %v — "+
			"reported as unknown, the operator goes looking for a typo", code, body)
	}
	if detail, _ := body["detail"].(string); strings.Contains(detail, "nats") ||
		!strings.Contains(detail, "CREWLET_PAGES_LOG") {
		t.Errorf("detail = %q, want the stream named and the broker's words left "+
			"to the log", detail)
	}
	// THE ACKNOWLEDGEMENT DOES NOT ASK THE BROKER FOR AN INSTANT IT DOES NOT
	// USE: the generation is the node's own.
	if code, body := postAck(t, a,
		"/worklet/retention/ack?stream=CREWLET_PAGES_LOG&position=918100000"); code != http.StatusOK {
		t.Fatalf("an acknowledgement while the broker could not answer a status "+
			"read was refused %d: %v", code, body)
	}
	if register.calls != 1 {
		t.Fatalf("%d point(s) written, want one", register.calls)
	}
}

// A RESTORED REANCHOR THAT WOULD DISCARD SAYS SO, AND DISCARDS ONLY ON THE WORD.
//
// The status names the newest record written after the restore that this
// node's rows do not hold, beside the node's own refusal; the transition
// passes `discard=true` on as the operator's word and nothing else does — not
// `force=true`, which answers another question — and its answer names what it
// discarded.
func TestARestoredReanchorThatWouldDiscardSaysSoAndDiscardsOnlyOnTheWord(t *testing.T) {
	t.Parallel()
	written := &statelog.TailRecord{Seq: 7_100, Kind: "task", Subject: "task.t-1",
		Writer: "node-b", OpID: "op-b-1", StoredAt: time.Unix(1700000100, 0).UTC()}
	node := &fakeStateLog{
		generations: map[string]uint32{"CREWLET_TRACKER_LOG": 1},
		view: engine.ReanchorView{Case: statelog.ReanchorRestored, Cursor: 7_100,
			Discards: written, Discarding: "statelog: reanchor refused: re-run with the discard flag"},
	}
	a := ackApp(t, &fakeBackupRegister{}, node)
	call := func(method, path string) (int, map[string]any) {
		t.Helper()
		req := httptest.NewRequest(method, path, nil)
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		a.ServeHTTP(rec, req)
		var body map[string]any
		_ = json.NewDecoder(rec.Result().Body).Decode(&body)
		return rec.Code, body
	}

	code, body := call(http.MethodGet, "/worklet/retention/reanchor?stream=CREWLET_TRACKER_LOG")
	discards, _ := body["discards"].(map[string]any)
	if code != http.StatusOK || discards["seq"] != float64(7_100) ||
		discards["writer"] != "node-b" || discards["subject"] != "task.t-1" ||
		body["discarding"] != node.view.Discarding {
		t.Fatalf("the status answered %d: %v, want the record it would discard", code, body)
	}

	node.plan = statelog.ReanchorPlan{Generation: 2, Case: statelog.ReanchorRestored,
		Cursor: 7_100}
	if code, body := call(http.MethodPost, "/worklet/retention/reanchor?stream=CREWLET_TRACKER_LOG"+
		"&confirm=2023-11-14T22:13:20Z&force=true"); code != http.StatusOK || body["discarded"] != nil {
		t.Fatalf("a forced reanchor answered %d: %v", code, body)
	}
	if node.reanchored.Discard {
		t.Fatal("force=true reached the node as the word to discard")
	}

	node.plan.Discarded = written
	code, body = call(http.MethodPost, "/worklet/retention/reanchor?stream=CREWLET_TRACKER_LOG"+
		"&confirm=2023-11-14T22:13:20Z&discard=true")
	if code != http.StatusOK || !node.reanchored.Discard {
		t.Fatalf("discard=true answered %d and reached the node as %+v", code, node.reanchored)
	}
	if discarded, _ := body["discarded"].(map[string]any); discarded["op_id"] != "op-b-1" {
		t.Fatalf("the answer %v does not name what it discarded", body)
	}
}

// AN OPERATION ID THE ENGINE DID NOT MINT IS REFUSED BY THE NODE GATE, not
// passed on.
//
// It carries no instant, so the state log reads it as older than every loss
// its ledger has had — and on any deployment old enough to have swept its
// ledger once, that answers the gesture `unknown` without publishing it, on
// the first attempt as on every retry: a gesture that can never run and never
// says why.
func TestAGateRefusesAnOperationIDTheEngineDidNotMint(t *testing.T) {
	t.Parallel()
	for _, verb := range []string{"evict", "readmit"} {
		b := closedPosture()
		gate := &fakeNodeGate{}
		code, body := postAck(t, newApp(t, api.Options{Bootstrap: &b, Nodes: gate}),
			"/worklet/retention/"+verb+"/node-4?confirm=node-4&op_id=op-abc")
		if code != http.StatusBadRequest ||
			body["error"] != string(httpjson.CodeOpIDInvalid) ||
			body["message"] != httpjson.CodeOpIDInvalid.Message() {
			t.Fatalf("an invented %s op_id answered %d %v, want 400 op_id_invalid "+
				"with its sentence", verb, code, body)
		}
		if len(gate.asked) != 0 {
			t.Errorf("the invented %s op_id still reached the gate: %+v", verb,
				gate.asked)
		}
	}
}

// A GESTURE'S OPERATION NAMES ITS NODE IN ONE STEP OF THE GRAMMAR, whatever
// the node's id holds.
//
// A node id may hold dots, and in the grammar a dot begins a step, so an id
// the route minted for `node-a.b` read as the name `evict-node-a` with a step
// `b`. The node goes into the name through [statelog.OpNamePart], as the
// command line's and the dashboard's do.
//
// Mutation: compose the name from the bare node and the id the route mints is
// named another way.
func TestAGateNamesADottedNodeInOneStep(t *testing.T) {
	t.Parallel()
	b := closedPosture()
	gate := &fakeNodeGate{}
	code, body := postAck(t, newApp(t, api.Options{Bootstrap: &b, Nodes: gate}),
		"/worklet/retention/evict/node-a.b?confirm=node-a.b")
	if code != http.StatusOK || len(gate.asked) != 1 {
		t.Fatalf("an eviction of a dotted node answered %d %v and ran as %+v",
			code, body, gate.asked)
	}
	if want := "evict-" + statelog.OpNamePart("node-a.b"); !statelog.OpNamed(gate.asked[0].OpID, want) {
		t.Errorf("the route minted %q, want an id named %q — the node's own dot "+
			"read as a step of the gesture", gate.asked[0].OpID, want)
	}
}

// AN OPERATION ID THE BROKER WOULD CARRY AS ANOTHER IS REFUSED BEFORE ANYTHING
// IS WRITTEN — minted by the engine or not.
//
// Only an id's first thirty-six bytes are the minted uuid; the rest is free
// text, and the whole id travels as the broker's message-id header, whose
// writer trims its ends and turns a line break into a space. So an engine id
// carrying either was deduplicated at the broker as another id than the one
// every ledger answers for, and the route's own trim answered with an id the
// caller never sent. The one an answer carried goes through byte for byte.
func TestAGateRefusesAnOperationIDTheBrokerWouldRewrite(t *testing.T) {
	t.Parallel()
	minted := statelog.NewOpID(time.Now().Add(-time.Minute), "evict-node-4")
	for name, opID := range map[string]string{
		"a leading space":       " " + minted,
		"a trailing space":      minted + " ",
		"an inner space":        minted + ".a b",
		"a line break":          minted + "\n",
		"a tab":                 minted + ".\ttab",
		"a byte past ASCII":     minted + ".é",
		"one byte over the cap": minted + "." + strings.Repeat("a", 128-len(minted)),
	} {
		b := closedPosture()
		gate := &fakeNodeGate{}
		code, body := postAck(t, newApp(t, api.Options{Bootstrap: &b, Nodes: gate}),
			"/worklet/retention/evict/node-4?confirm=node-4&"+
				url.Values{"op_id": {opID}}.Encode())
		if code != http.StatusBadRequest || body["error"] != string(httpjson.CodeOpIDInvalid) {
			t.Errorf("an eviction under an op_id with %s answered %d %v, want 400 "+
				"op_id_invalid", name, code, body)
		}
		if len(gate.asked) != 0 {
			t.Errorf("an eviction under an op_id with %s reached the gate", name)
		}
	}

	// THE CONTROL: a minted id at the cap, carrying punctuation from across
	// the visible range, is the caller's and goes through verbatim.
	good := minted + "." + strings.Repeat("a", 128-len(minted)-1-8) + "-_.:~!@9"
	if len(good) != 128 {
		t.Fatalf("the control is %d bytes, want exactly the cap", len(good))
	}
	b := closedPosture()
	gate := &fakeNodeGate{}
	code, body := postAck(t, newApp(t, api.Options{Bootstrap: &b, Nodes: gate}),
		"/worklet/retention/evict/node-4?confirm=node-4&"+
			url.Values{"op_id": {good}}.Encode())
	if code != http.StatusOK || len(gate.asked) != 1 || gate.asked[0].OpID != good {
		t.Fatalf("an eviction under a valid op_id answered %d %v and ran as %+v",
			code, body, gate.asked)
	}
}
