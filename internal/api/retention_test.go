package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
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
type fakeStateLog struct {
	generations map[string]uint32
	asked       string

	// reanchor is how a reanchor ends; nil lands it at the next generation.
	reanchor error
}

func (f *fakeStateLog) ReanchorStatus(_ context.Context, stream string) (
	time.Time, uint32, error) {

	f.asked = stream
	generation, runs := f.generations[stream]
	if !runs {
		return time.Time{}, 0, fmt.Errorf("%w: %q", engine.ErrNotADomainLog, stream)
	}
	return time.Unix(1700000000, 0).UTC(), generation, nil
}

func (f *fakeStateLog) StreamGeneration(stream string) (uint32, error) {
	f.asked = stream
	generation, runs := f.generations[stream]
	if !runs {
		return 0, fmt.Errorf("%w: %q", engine.ErrNotADomainLog, stream)
	}
	return generation, nil
}

func (f *fakeStateLog) Reanchor(_ context.Context, req engine.ReanchorRequest) (uint32, error) {
	if f.reanchor != nil {
		return 0, f.reanchor
	}
	return f.generations[req.Stream] + 1, nil
}

func (f *fakeStateLog) SetCapacity(context.Context, engine.CapacityRequest) (
	coord.MaintenanceOperation, error) {

	return coord.MaintenanceOperation{}, errors.New("not exercised here")
}

func (f *fakeStateLog) AbandonCapacity(context.Context, string) (
	coord.MaintenanceOperation, error) {

	return coord.MaintenanceOperation{}, errors.New("not exercised here")
}

func (f *fakeStateLog) ExcludeParticipant(context.Context, string, string) (
	coord.MaintenanceOperation, error) {

	return coord.MaintenanceOperation{}, errors.New("not exercised here")
}

func (f *fakeStateLog) CapacityStatus(context.Context, string) (
	coord.MaintenanceOperation, []coord.MaintenanceAck, []coord.Admission, bool, error) {

	return coord.MaintenanceOperation{}, nil, nil, false, errors.New("not exercised here")
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
		"/work/retention/ack?stream=CREWLET_PAGES_LOG&position=918100000")
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
		"/work/retention/ack?stream=CREWLET_TRAKCER_LOG&position=918100000")
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

// fakeGate answers the eviction routes and records what each was asked.
type fakeGate struct {
	asked   []engine.GateRequest
	results []engine.GateResult
	err     error
}

func (f *fakeGate) EvictNode(_ context.Context, req engine.GateRequest) (
	[]engine.GateResult, error) {

	f.asked = append(f.asked, req)
	return f.results, f.err
}

func (f *fakeGate) ReadmitNode(_ context.Context, req engine.GateRequest) (
	[]engine.GateResult, error) {

	f.asked = append(f.asked, req)
	return f.results, f.err
}

// AN EVICTION ANSWERS FOR EVERY LOG IT REACHED, AND NAMES WHO RAN IT.
//
// A node is evicted from each log whose applier installs the gate, and each
// record lands — or stays pending — on its own; an answer carrying one outcome
// would report two different facts as one. The records name the operator who
// pressed, which is what the trim's tombstone and the status report show. And
// a refusal because the node is still reaching coordination is the caller's to
// act on, not a server fault — with whatever did land still named.
//
// Mutation: answer one outcome for the gesture and the second log's is lost;
// map the permission refusal to a 500 and the operator is told the node failed.
func TestAnEvictionAnswersForEveryLogItReached(t *testing.T) {
	t.Parallel()
	gate := &fakeGate{results: []engine.GateResult{
		{Domain: "tracker", Stream: "CREWLET_TRACKER_LOG", Result: statelog.Result{
			Outcome:  statelog.OutcomeApplied,
			Position: statelog.Position{Stream: "CREWLET_TRACKER_LOG", Seq: 41},
		}},
		{Domain: "pages", Stream: "CREWLET_PAGES_LOG", Result: statelog.Result{
			Outcome:  statelog.OutcomePending,
			Position: statelog.Position{Stream: "CREWLET_PAGES_LOG", Seq: 9},
		}},
	}}
	b := closedPosture()
	a := newApp(t, api.Options{Bootstrap: &b, Nodes: gate})

	code, body := postAck(t, a, "/work/retention/evict/node-b?confirm=node-b")
	if code != http.StatusOK {
		t.Fatalf("the route answered %d: %v", code, body)
	}
	domains, _ := body["domains"].([]any)
	if len(domains) != 2 {
		t.Fatalf("the answer carries %v, want one entry per log reached", body["domains"])
	}
	second, _ := domains[1].(map[string]any)
	if second["domain"] != "pages" || second["outcome"] != "pending" {
		t.Errorf("the second log's entry is %v, want pages pending", second)
	}
	if len(gate.asked) != 1 || gate.asked[0].By != "founder" ||
		gate.asked[0].Node != "node-b" || gate.asked[0].OpID == "" {
		t.Errorf("the gate was asked %+v, want node-b, a fresh op id and the "+
			"operator who pressed", gate.asked)
	}

	gate.err = &statelog.EvictionRefusal{NodeID: "node-b", Detail: "it holds a live lease"}
	gate.results = gate.results[:1]
	code, body = postAck(t, a, "/work/retention/evict/node-b?confirm=node-b")
	if code != http.StatusConflict || body["error"] != "eviction_refused" {
		t.Fatalf("a refused permission answered %d %v, want 409 eviction_refused",
			code, body)
	}
	if reached, _ := body["domains"].([]any); len(reached) != 1 {
		t.Errorf("the refusal names %v, want the one log that did land", body["domains"])
	}

	if code, _ := postAck(t, a, "/work/retention/evict/node-b?confirm=node-c"); code != http.StatusBadRequest {
		t.Errorf("a confirmation naming another node answered %d, want 400", code)
	}
}

// A REANCHOR IS ANSWERED BY HOW IT ENDED.
//
// Three endings send an operator three different ways: a stream no domain runs
// on is a name to correct, which the status read beside this route already
// answers `404`; a refusal is the transition declining with nothing written,
// its reason in the engine's own words — some clear and the same call then
// lands (a transition already running, a confirmation for another instant),
// some never do by calling again (a hydrated peer, a generation another
// reanchor holds); and a failure partway is one the transition's step order
// makes safe to run again. One conflict for all three would tell an operator
// who mistyped a stream, and one whose disk filled mid-transition, that the
// fleet had refused them.
//
// Mutation: answer every error `409 reanchor_refused` and the unknown stream
// and the failure are misfiled.
func TestAReanchorIsAnsweredByHowItEnded(t *testing.T) {
	t.Parallel()
	const path = "/work/retention/reanchor?stream=CREWLET_TRACKER_LOG" +
		"&confirm=2031-04-02T03:00:00Z"
	for name, tc := range map[string]struct {
		err  error
		code int
		want string
	}{
		"a stream no domain runs on": {
			err:  fmt.Errorf("%w: %q", engine.ErrNotADomainLog, "CREWLET_TRAKCER_LOG"),
			code: http.StatusNotFound, want: "unknown_stream",
		},
		"a hydrated peer": {
			err: fmt.Errorf("%w: 1 peer(s) are hydrated on the live stream",
				statelog.ErrReanchorRefused),
			code: http.StatusConflict, want: "reanchor_refused",
		},
		"a transition already running, which clears once it ends": {
			err: fmt.Errorf("%w: engine: this node is already rewriting its "+
				"replicated estate: another reanchor is running on it",
				statelog.ErrReanchorRefused),
			code: http.StatusConflict, want: "reanchor_refused",
		},
		"a confirmation naming another instant, which clears when re-sent": {
			err: fmt.Errorf("%w: the confirmation names \"2031-04-02T04:00:00Z\" "+
				"and the live stream was created at 2031-04-02T03:00:00Z",
				statelog.ErrReanchorRefused),
			code: http.StatusConflict, want: "reanchor_refused",
		},
		"a generation another reanchor holds": {
			err: fmt.Errorf("%w: tracker's generation 4 is held by another "+
				"reanchor's record: %w", statelog.ErrReanchorRefused,
				&statelog.ClaimedElsewhere{Holder: "reanchor:4:1:node-b"}),
			code: http.StatusConflict, want: "reanchor_refused",
		},
		"a failure partway": {
			err: errors.New("statelog: move tracker's cursor into generation 4: " +
				"the disk is full"),
			code: http.StatusInternalServerError, want: "reanchor_failed",
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			node := &fakeStateLog{
				generations: map[string]uint32{"CREWLET_TRACKER_LOG": 3},
				reanchor:    tc.err,
			}
			code, body := postAck(t, ackApp(t, &fakeBackupRegister{}, node), path)
			if code != tc.code || body["error"] != tc.want {
				t.Fatalf("the route answered %d %v, want %d %s", code, body,
					tc.code, tc.want)
			}
			if body["detail"] != tc.err.Error() {
				t.Errorf("the detail is %q, want the engine's own words %q",
					body["detail"], tc.err.Error())
			}
		})
	}

	node := &fakeStateLog{generations: map[string]uint32{"CREWLET_TRACKER_LOG": 3}}
	code, body := postAck(t, ackApp(t, &fakeBackupRegister{}, node), path)
	if code != http.StatusOK || body["generation"] != float64(4) {
		t.Fatalf("a landed reanchor answered %d %v, want 200 at generation 4",
			code, body)
	}
}
