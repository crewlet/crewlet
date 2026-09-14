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
}

func (f *fakeStateLog) ReanchorStatus(_ context.Context, stream string) (
	time.Time, uint32, error) {

	f.asked = stream
	generation, runs := f.generations[stream]
	if !runs {
		return time.Time{}, 0, fmt.Errorf(
			"engine: %q is not a domain log this build runs", stream)
	}
	return time.Unix(1700000000, 0).UTC(), generation, nil
}

func (f *fakeStateLog) Reanchor(context.Context, engine.ReanchorRequest) (uint32, error) {
	return 0, errors.New("not exercised here")
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
