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

// guarded is a bootstrap on the default anonymous posture with one ADMIN key,
// `ops` presenting "t0ken", and one MEMBER key, `ada` presenting "m3mber" — so
// a case can show an admin route refusing all three kinds of caller but one.
func guarded() *config.Bootstrap {
	b := config.DefaultBootstrap()
	b.API.Auth.Tokens = []config.APIToken{
		{ID: "ops", Role: config.RoleAdmin, Token: "t0ken"},
		{ID: "ada", Role: config.RoleMember, Token: "m3mber"},
	}
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
// to a path the caller names. That is running the engine, so it is an ADMIN's:
// a caller with no key is told to sign in, and a member key — one a teammate
// holds — is forbidden it.
func TestABackupIsAnAdminsAlone(t *testing.T) {
	t.Parallel()
	taker := &fakeBackup{}
	a := newApp(t, api.Options{Bootstrap: guarded(), Backup: taker})

	if status, _ := post(t, a, "/backup?dir=/tmp/x", ""); status != http.StatusUnauthorized {
		t.Fatalf("an unauthenticated caller's backup = %d, want 401", status)
	}
	status, body := post(t, a, "/backup?dir=/tmp/x", "m3mber")
	if status != http.StatusForbidden || body["error"] != "forbidden" {
		t.Fatalf("a member key's backup = %d %v, want 403 forbidden", status, body)
	}
	if taker.dir != "" {
		t.Errorf("a refused request reached the backup subsystem anyway (dir=%q)", taker.dir)
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
