package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/backup"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// post runs one POST and returns the status and decoded body.
func post(t *testing.T, a *api.App, path, token string) (int, map[string]any) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	a.ServeHTTP(rec, req)
	res := rec.Result()
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return res.StatusCode, body
}

// guarded is a bootstrap carrying one credential, and the authority behind it.
//
// IT CARRIED NO GRANTS, and every case over it passed: the deployment's own
// controls asked only whether somebody resolved, so a token declaring nothing
// on a node conferring nothing took backups. They take `admin` now, so the
// fixture states the authority its cases are about — and TestThePostureMatrix
// states what a narrower one is refused.
func guarded() *config.Bootstrap {
	b := config.DefaultBootstrap()
	authorize(&b, config.APIToken{ID: "ops", Token: "t0ken"})
	return &b
}

// fakeBackup records what the route asked it for.
type fakeBackup struct {
	dir  string
	err  error
	took backup.Manifest
}

func (f *fakeBackup) Take(_ context.Context, dir string) (backup.Manifest, error) {
	f.dir = dir
	if f.err != nil {
		return backup.Manifest{}, f.err
	}
	return f.took, nil
}

// A backup copies every credential the company holds and every seat's memory
// to a path the caller names, so a caller presenting no credential is refused
// before the backup subsystem is reached.
func TestABackupIsRefusedWithoutAToken(t *testing.T) {
	t.Parallel()
	taker := &fakeBackup{}
	a := newApp(t, api.Options{Bootstrap: guarded(), Backup: taker})

	status, _ := post(t, a, "/backup?dir=/tmp/x", "")
	if status == http.StatusOK {
		t.Fatal("an unauthenticated caller took a backup")
	}
	if taker.dir != "" {
		t.Errorf("the refused request reached the backup subsystem anyway (dir=%q)", taker.dir)
	}
}

// The destination is not optional and has no default: a default would put a
// company's entire durable state somewhere nobody chose.
func TestABackupWithoutADestinationIsRefused(t *testing.T) {
	t.Parallel()
	taker := &fakeBackup{}
	a := newApp(t, api.Options{Bootstrap: guarded(), Backup: taker})

	status, body := post(t, a, "/backup", "t0ken")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", status)
	}
	if body["error"] != "no_destination" {
		t.Errorf("error = %v", body["error"])
	}
	if taker.dir != "" {
		t.Error("a request with no destination still reached the subsystem")
	}
}

// A destination the caller named badly is THEIR mistake, and answering 500
// would send an operator to the engine's logs to debug their own command.
func TestABadDestinationIsTheCallersMistakeNotTheNodes(t *testing.T) {
	t.Parallel()
	taker := &fakeBackup{err: backup.ErrNotEmpty}
	a := newApp(t, api.Options{Bootstrap: guarded(), Backup: taker})

	status, body := post(t, a, "/backup?dir=/tmp/occupied", "t0ken")
	if status != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for a destination problem", status)
	}
	// The detail is returned here, unlike every other route's internal
	// reason, precisely because it is actionable by the caller.
	if body["detail"] == nil {
		t.Error("the caller was not told what was wrong with their destination")
	}
}

// An engine failure gives up NO detail, matching every other route on this
// surface: the reason is for the log, and a caller who cannot act on it
// should not be handed the shape of the engine's internals.
func TestAnEngineFailureKeepsItsReasonInTheLog(t *testing.T) {
	t.Parallel()
	taker := &fakeBackup{err: errors.New("/mnt/secret-path: input/output error")}
	a := newApp(t, api.Options{Bootstrap: guarded(), Backup: taker})

	_, body := post(t, a, "/backup?dir=/tmp/x", "t0ken")
	if body["detail"] != nil {
		t.Errorf("an engine-side reason reached the caller: %v", body["detail"])
	}
}

// A failure of the NODE is a 500 and its reason goes to the log, matching
// every other route on this surface.
func TestAFailedBackupIsAnEngineError(t *testing.T) {
	t.Parallel()
	taker := &fakeBackup{err: errors.New("the disk went away")}
	a := newApp(t, api.Options{Bootstrap: guarded(), Backup: taker})

	status, body := post(t, a, "/backup?dir=/tmp/x", "t0ken")
	if status != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500", status)
	}
	if body["error"] != "backup_failed" {
		t.Errorf("error = %v", body["error"])
	}
}

// A copy naming objects the store did not answer for is the FLEET's condition,
// and one that clears: a 500 would send an operator to this node's logs for a
// store that is unreachable somewhere else, and the list of objects stays in
// the log.
func TestUnreachableObjectsAreTheFleetsStateNotTheNodes(t *testing.T) {
	t.Parallel()
	taker := &fakeBackup{err: fmt.Errorf("%w: 3 of 40 objects (ENG/q3.csv …)", backup.ErrObjectsUnreachable)}
	a := newApp(t, api.Options{Bootstrap: guarded(), Backup: taker})

	status, body := post(t, a, "/backup?dir=/tmp/x", "t0ken")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", status)
	}
	if body["error"] != "objects_unreachable" {
		t.Errorf("error = %v, want objects_unreachable", body["error"])
	}
	if body["detail"] != nil {
		t.Errorf("the list of objects reached the caller: %v", body["detail"])
	}
	if body["hint"] == nil {
		t.Error("the caller was not told what to do about it")
	}
}

// A BACKUP THE FLEET'S OWN STATE REFUSES SAYS WHAT TO DO ABOUT IT. A rolling
// upgrade's newer record on a log the backup replays, a record signed under a
// key this node's keyring does not hold yet, a copy keyed to a log that has
// since been recreated, and a log missing records the copy needs are
// each a state with a remedy the engine knows — another node, or this one
// later, takes the backup — so each is a code and a fixed hint rather than a
// 500 that sends an operator to a node with nothing wrong on it. And the
// error's own text, which names positions and files, stays in the log.
func TestABackupTheFleetsStateRefusesSaysWhatToDo(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		err    error
		status int
		code   string
	}{
		{fmt.Errorf("backup: the record at CREWLET_TRACKER_LOG sequence 9: %w: version 2",
			objstore.ErrRecordUnreadable), http.StatusConflict, "record_unreadable"},
		{fmt.Errorf("backup: the record at CREWLET_TRACKER_LOG sequence 9: %w (this node holds \"k1\")",
			statelog.ErrRecordUnverified), http.StatusConflict, "record_unverified"},
		{fmt.Errorf("backup: the copy's checkpoint on CREWLET_TRACKER_LOG: %w",
			statelog.ErrStreamRecreated), http.StatusConflict, "log_recreated"},
		{fmt.Errorf("%w: CREWLET_TRACKER_LOG holds no record 5", backup.ErrLogGap),
			http.StatusServiceUnavailable, "log_gap"},
	} {
		t.Run(c.code, func(t *testing.T) {
			t.Parallel()
			a := newApp(t, api.Options{Bootstrap: guarded(), Backup: &fakeBackup{err: c.err}})
			status, body := post(t, a, "/backup?dir=/tmp/x", "t0ken")
			if status != c.status {
				t.Fatalf("status = %d, want %d", status, c.status)
			}
			if body["error"] != c.code {
				t.Errorf("error = %v, want %s", body["error"], c.code)
			}
			if hint, _ := body["hint"].(string); hint == "" {
				t.Error("the caller was not told what to do about it")
			}
			if body["detail"] != nil {
				t.Errorf("the error's own text reached the caller: %v", body["detail"])
			}
		})
	}
}

// A BACKUP ASKED FOR WHILE ONE RUNS IS A 409. Nothing is wrong with the node
// or the command — the first one is still copying — and a 500 would send an
// operator whose cron overlapped to the engine's logs for a backup that is in
// fact under way.
func TestABackupWhileOneRunsAnswers409(t *testing.T) {
	t.Parallel()
	taker := &fakeBackup{err: backup.ErrBackupRunning}
	a := newApp(t, api.Options{Bootstrap: guarded(), Backup: taker})

	status, body := post(t, a, "/backup?dir=/tmp/x", "t0ken")
	if status != http.StatusConflict {
		t.Fatalf("status = %d, want 409", status)
	}
	if body["error"] != "backup_running" || body["hint"] == nil {
		t.Errorf("body = %v, want backup_running with a hint", body)
	}
}

// The manifest is the answer: it is what the CLI renders and what a restore
// reads, so the route must hand it back rather than a bare acknowledgement.
func TestASuccessfulBackupAnswersWithItsManifest(t *testing.T) {
	t.Parallel()
	taker := &fakeBackup{took: backup.Manifest{
		NodeID: "node-7",
		Stores: []backup.StoreArtifact{{File: "store.db", Bytes: 4096}},
		Streams: []backup.StreamArtifact{
			{Name: "CREWLET_AGENT", File: "streams/CREWLET_AGENT.snapshot", Messages: 3},
		},
	}}
	a := newApp(t, api.Options{Bootstrap: guarded(), Backup: taker})

	status, body := post(t, a, "/backup?dir=/var/backups/one", "t0ken")
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200", status)
	}
	if taker.dir != "/var/backups/one" {
		t.Errorf("the subsystem was asked for %q", taker.dir)
	}
	if body["node_id"] != "node-7" {
		t.Errorf("node_id = %v", body["node_id"])
	}
	streams, ok := body["streams"].([]any)
	if !ok || len(streams) != 1 {
		t.Fatalf("streams = %v", body["streams"])
	}
}
