package api_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/crewlet/crewlet/internal/api"
	"github.com/crewlet/crewlet/internal/backup"
)

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
// to a path the caller names. It is a write, so the anonymous-read posture —
// which is ON by default — must never open it.
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

// A node with neither estate says so rather than 404ing: the route exists on
// this build, and a 404 sends an operator looking for a version mismatch that
// is not there.
func TestABackupOnANodeWithNothingToCopyReportsWhyRatherThan404(t *testing.T) {
	t.Parallel()
	a := newApp(t, api.Options{Bootstrap: guarded()})

	status, body := post(t, a, "/backup?dir=/tmp/x", "t0ken")
	if status != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", status)
	}
	if body["error"] != "nothing_to_back_up" {
		t.Errorf("error = %v", body["error"])
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

// The manifest is the answer: it is what the CLI renders and what a restore
// reads, so the route must hand it back rather than a bare acknowledgement.
func TestASuccessfulBackupAnswersWithItsManifest(t *testing.T) {
	t.Parallel()
	taker := &fakeBackup{took: backup.Manifest{
		NodeID: "node-7",
		Store:  &backup.StoreArtifact{File: "store.db", Bytes: 4096},
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
