package sandbox

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/httpx/httpxtest"
)

// scriptedRuntime writes an executable named name in a fresh directory that runs
// script, and answers its path — written where no fork can hold it open
// ([writeExecutable]), since the probe execs it at once.
func scriptedRuntime(t *testing.T, name, script string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	writeExecutable(t, path, "#!/bin/sh\n"+script+"\n")
	return path
}

// THE CONTAINER CELL IS READY WHEN ITS DAEMON ANSWERS, and not otherwise: a
// runtime on PATH whose daemon is down launches no box, and one that hangs is
// cut off at the probe's budget rather than holding the probe. Each refusal
// names the one thing to do.
//
// THE HUNG DAEMON SLEEPS TEN MINUTES, far past the minute the case waits, so
// the probe answers only by ending its read at its budget, which its detail
// names; no bound on the machine's clock decides it.
//
// Mutation: report ready on a resolved binary alone, or wait on a hung daemon
// rather than end its read at the budget, and this fails.
func TestTheContainerCellIsReadyWhenItsDaemonAnswers(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		script string
		ready  bool
		detail string
		budget time.Duration
	}{
		"a daemon that answers": {script: `echo 24.0.7`, ready: true, detail: "Docker 24.0.7", budget: 5 * time.Second},
		"a daemon that is down": {script: `echo "Cannot connect to the Docker daemon" >&2; exit 1`, detail: "Cannot connect to the Docker daemon", budget: 5 * time.Second},
		"a daemon that never answers": {script: `exec sleep 600`, detail: "did not answer a version read within 200ms",
			budget: 200 * time.Millisecond},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			path := scriptedRuntime(t, "docker", tc.script)
			probed := make(chan CellProbe, 1)
			go func() { probed <- probeRuntime(t.Context(), path, tc.budget) }()
			var got CellProbe
			select {
			case got = <-probed:
			case <-time.After(time.Minute): //fixedwait:liveness
				t.Fatal("the probe is still held by a daemon that has not answered; want its read ended at the budget")
			}
			if got.Ready != tc.ready || !strings.Contains(got.Detail, tc.detail) {
				t.Fatalf("probe = %+v, want ready %v saying %q", got, tc.ready, tc.detail)
			}
			if !tc.ready && got.Fix == "" {
				t.Error("a cell that is not ready names nothing to do about it")
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

// THE E2B CELL IS READY WHEN ITS KEY RESOLVES AND THE API ANSWERS IT: an
// authenticated read, and the floor's routes served, with no box made. A key
// that resolves to nothing, one the API refuses, and an API that does not
// answer are three different fixes.
//
// Mutation: report ready on a resolved key alone, or read a refusal as a
// network fault, and this fails.
func TestTheE2BCellIsReadyWhenTheAPIAnswersItsKey(t *testing.T) {
	t.Parallel()
	build := e2bBuildAnswering(t, http.StatusBadRequest, http.StatusBadRequest)
	if got := ProbeE2B(t.Context(), E2BOptions{APIKey: "e2b_good", Domain: "test.invalid", HTTP: build.client}); !got.Ready {
		t.Fatalf("probe with a good key = %+v, want ready", got)
	}
	if limit := build.limit(); limit != "1" {
		t.Errorf("the probe listed with limit %q, want one box at most", limit)
	}
	if made := build.madeOrWoken(); made != 0 {
		t.Errorf("the probe sent %d bodies a build would act on, making or waking a box", made)
	}
	refused := ProbeE2B(t.Context(), E2BOptions{APIKey: "e2b_revoked", Domain: "test.invalid", HTTP: build.client})
	if refused.Ready || !strings.Contains(refused.Fix, "Replace the e2b API key") {
		t.Errorf("probe with a refused key = %+v, want not ready, replacing the key", refused)
	}
	if missing := ProbeE2B(t.Context(), E2BOptions{Domain: "test.invalid", HTTP: build.client}); missing.Ready ||
		!strings.Contains(missing.Fix, "Settings › Secrets") {
		t.Errorf("probe with no key = %+v, want not ready, naming where the key is set", missing)
	}
}

// A CLUSTER OLDER THAN THE FLOOR IS NOT READY, NAMING IT, never ready and then
// failing every create: its list answers the key, but a build from before
// e2b-dev/runtime f5dc6426e answers the connect's route 404 and, from 2026.04
// on, the create's too (before, the create's path served for a list answers its
// POST 400, as a current build answers the probe's unreadable body). The cloud
// is told to report it, never a cluster's build.
//
// Mutation: probe the create's route alone, or read its 400 as missing, or skip
// the routes, and a row goes red.
func TestAClusterOlderThanTheFloorIsNotReady(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name            string
		domain          string
		create, connect int
		ready           bool
		fix             string
	}{
		{"a build at the floor", "test.invalid", http.StatusBadRequest, http.StatusBadRequest, true, ""},
		{"a build from 2026.04 to 2026.30", "test.invalid", http.StatusNotFound, http.StatusNotFound, false,
			"Run e2b-dev/runtime f5dc6426e or later on the cluster at test.invalid"},
		{"a build before 2026.04", "test.invalid", http.StatusBadRequest, http.StatusNotFound, false,
			"Run e2b-dev/runtime f5dc6426e or later on the cluster at test.invalid"},
		{"E2B's cloud without the routes", "", http.StatusNotFound, http.StatusNotFound, false,
			"Report it to E2B"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			build := e2bBuildAnswering(t, tc.create, tc.connect)
			got := ProbeE2B(t.Context(), E2BOptions{APIKey: "e2b_good", Domain: tc.domain, HTTP: build.client})
			if got.Ready != tc.ready {
				t.Fatalf("probe = %+v, want ready %t", got, tc.ready)
			}
			if tc.ready {
				return
			}
			if !strings.Contains(got.Detail, "e2b-dev/runtime f5dc6426e") || !strings.Contains(got.Fix, tc.fix) {
				t.Errorf("probe = %+v, want the floor named and the fix %q", got, tc.fix)
			}
		})
	}
}

// e2bBuild is a stand-in E2B API: its list answers the key e2b_good, and its
// create and connect answer the statuses a build of a given age answers the
// probe's bodies, counting any body a build would act on.
type e2bBuild struct {
	client *http.Client
	mu     sync.Mutex
	limits []string
	acted  int
}

func (b *e2bBuild) limit() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.limits) == 0 {
		return ""
	}
	return b.limits[0]
}

func (b *e2bBuild) madeOrWoken() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.acted
}

// e2bBuildAnswering is an [e2bBuild] whose create answers create and whose
// connect answers connect, where a body it could act on would be 201.
func e2bBuildAnswering(t *testing.T, create, connect int) *e2bBuild {
	t.Helper()
	b := &e2bBuild{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") != "e2b_good" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":401,"message":"Invalid API key"}`))
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		status := http.StatusNotFound
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v2/sandboxes":
			b.mu.Lock()
			b.limits = append(b.limits, r.URL.Query().Get("limit"))
			b.mu.Unlock()
			_, _ = w.Write([]byte(`[]`))
			return
		case r.Method == http.MethodPost && r.URL.Path == "/v2/sandboxes":
			status = create
			if _, readable := body["templateID"].(string); readable && create == http.StatusBadRequest {
				status = http.StatusCreated
			}
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/connect"):
			status = connect
			if _, readable := body["timeout"].(float64); readable && connect == http.StatusBadRequest {
				status = http.StatusCreated
			}
		}
		if status == http.StatusCreated {
			b.mu.Lock()
			b.acted++
			b.mu.Unlock()
		}
		w.WriteHeader(status)
	}))
	t.Cleanup(server.Close)
	b.client = &http.Client{Transport: httpxtest.Rewrite(t, server)}
	return b
}
