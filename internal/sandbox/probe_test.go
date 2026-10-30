package sandbox

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
)

// scriptedRuntime writes an executable named name in a fresh directory that runs
// script, and answers its path.
func scriptedRuntime(t *testing.T, name, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+script+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// THE CONTAINER CELL IS READY WHEN ITS DAEMON ANSWERS, and not otherwise: a
// runtime on PATH whose daemon is down launches no box, and one that hangs is
// said within the budget rather than holding the probe. Each refusal names
// the one thing to do.
//
// Mutation: report ready on a resolved binary alone, or wait on a hung daemon
// past the budget, and this fails.
func TestTheContainerCellIsReadyWhenItsDaemonAnswers(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		script  string
		ready   bool
		detail  string
		budget  time.Duration
		elapsed time.Duration
	}{
		"a daemon that answers": {script: `echo 24.0.7`, ready: true, detail: "Docker 24.0.7", budget: 5 * time.Second},
		"a daemon that is down": {script: `echo "Cannot connect to the Docker daemon" >&2; exit 1`, detail: "Cannot connect to the Docker daemon", budget: 5 * time.Second},
		"a daemon that never answers": {script: `exec sleep 30`, detail: "did not answer a version read within 200ms",
			budget: 200 * time.Millisecond, elapsed: 3 * time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			began := time.Now()
			got := probeRuntime(t.Context(), scriptedRuntime(t, "docker", tc.script), tc.budget)
			if got.Ready != tc.ready || !strings.Contains(got.Detail, tc.detail) {
				t.Fatalf("probe = %+v, want ready %v saying %q", got, tc.ready, tc.detail)
			}
			if !tc.ready && got.Fix == "" {
				t.Error("a cell that is not ready names nothing to do about it")
			}
			if tc.elapsed > 0 && time.Since(began) > tc.elapsed {
				t.Errorf("the probe took %v on a hung daemon, past its budget", time.Since(began))
			}
		})
	}
}

// A RUNTIME THAT IS NOT INSTALLED IS NOT READY, and the fix says PATH is read
// at start: installing it under a running engine changes nothing until the
// engine restarts.
func TestAMissingRuntimeIsNotReady(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	got := ProbeContainer(t.Context(), "docker")
	if got.Ready || !strings.Contains(got.Fix, "restart the engine") {
		t.Fatalf("probe = %+v, want not ready, saying to restart after installing", got)
	}
}

// THE DIRECT CELL IS READY WHERE ITS BOX ROOT CAN BE WRITTEN, and not where it
// cannot.
func TestTheDirectCellNeedsAWritableBoxRoot(t *testing.T) {
	t.Parallel()
	if got := ProbeDirect(t.TempDir()); !got.Ready {
		t.Fatalf("probe of a writable state dir = %+v, want ready", got)
	}
	blocked := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocked, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	// A state dir that is a FILE: its box root cannot be made under it.
	if got := ProbeDirect(blocked); got.Ready || got.Fix == "" {
		t.Fatalf("probe of an unwritable state dir = %+v, want not ready with a fix", got)
	}
}

// THE E2B CELL IS READY WHEN ITS KEY RESOLVES AND THE API ANSWERS IT: one
// authenticated read, no box made. A key that resolves to nothing, one the API
// refuses, and an API that does not answer are three different fixes.
//
// Mutation: report ready on a resolved key alone, or read a refusal as a
// network fault, and this fails.
func TestTheE2BCellIsReadyWhenTheAPIAnswersItsKey(t *testing.T) {
	t.Parallel()
	var sawLimit, sawKey string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawLimit, sawKey = r.URL.Query().Get("limit"), r.Header.Get("X-API-Key")
		if r.URL.Path != "/v2/sandboxes" {
			http.NotFound(w, r)
			return
		}
		if sawKey != "e2b_good" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":401,"message":"Invalid API key"}`))
			return
		}
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(server.Close)
	client := &http.Client{Transport: httpxtest.Rewrite(t, server)}

	if got := ProbeE2B(t.Context(), E2BOptions{APIKey: "e2b_good", Domain: "test.invalid", HTTP: client}); !got.Ready {
		t.Fatalf("probe with a good key = %+v, want ready", got)
	}
	if sawLimit != "1" {
		t.Errorf("the probe listed with limit %q, want one box at most", sawLimit)
	}
	refused := ProbeE2B(t.Context(), E2BOptions{APIKey: "e2b_revoked", Domain: "test.invalid", HTTP: client})
	if refused.Ready || !strings.Contains(refused.Fix, "Replace the e2b API key") {
		t.Errorf("probe with a refused key = %+v, want not ready, replacing the key", refused)
	}
	if missing := ProbeE2B(t.Context(), E2BOptions{Domain: "test.invalid", HTTP: client}); missing.Ready ||
		!strings.Contains(missing.Fix, "Settings › Secrets") {
		t.Errorf("probe with no key = %+v, want not ready, naming where the key is set", missing)
	}
}
