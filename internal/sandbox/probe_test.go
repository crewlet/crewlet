package sandbox

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
// authenticated read and, of a self-hosted cluster, the floor's connect, asked
// in a request E2B's spec admits and its handler refuses before acting. A key
// that resolves to nothing, one the API refuses, and an API that does not
// answer are three different fixes.
//
// Mutation: report ready on a resolved key alone, read a refusal as a network
// fault, or send the connect a body or an id a box could carry, and this fails.
func TestTheE2BCellIsReadyWhenTheAPIAnswersItsKey(t *testing.T) {
	t.Parallel()
	cluster := e2bClusterOf(t, e2bAtTheFloor)
	if got := ProbeE2B(t.Context(), E2BOptions{APIKey: "e2b_good", Domain: "test.invalid", HTTP: cluster.client}); !got.Ready {
		t.Fatalf("probe with a good key = %+v, want ready", got)
	}
	_, limits, intrusive := cluster.seen()
	if len(limits) != 1 || limits[0] != "1" {
		t.Errorf("the probe listed with limits %q, want one list of one box at most", limits)
	}
	if len(intrusive) != 0 {
		t.Errorf("the probe sent %q, each acted on or refused as malformed, want only "+
			"requests a build refuses before acting", intrusive)
	}
	refused := ProbeE2B(t.Context(), E2BOptions{APIKey: "e2b_revoked", Domain: "test.invalid", HTTP: cluster.client})
	if refused.Ready || !strings.Contains(refused.Fix, "Replace the e2b API key") {
		t.Errorf("probe with a refused key = %+v, want not ready, replacing the key", refused)
	}
	if missing := ProbeE2B(t.Context(), E2BOptions{Domain: "test.invalid", HTTP: cluster.client}); missing.Ready ||
		!strings.Contains(missing.Fix, "Settings › Secrets") {
		t.Errorf("probe with no key = %+v, want not ready, naming where the key is set", missing)
	}
}

// EVERY CLUSTER OLDER THAN THE FLOOR IS NOT READY, NAMING IT, never ready and
// then failing every create, nor sent to check its network: a build from before
// e2b-dev/runtime f5dc6426e lacks the connect, and one from before the v2 list
// (2025-04) the list too, answering it 404, or 400 from an early build's
// router, which answered every path it lacked so.
//
// Mutation: skip the connect, read its 400 as missing, or leave a list the
// cluster lacks to the network's fix, and a row goes red.
func TestAClusterOlderThanTheFloorIsNotReady(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name  string
		age   e2bBuildAge
		ready bool
	}{
		{"a build at the floor", e2bAtTheFloor, true},
		{"a build with the v2 list, before the floor", e2bBelowTheFloor, false},
		{"a build before the v2 list", e2bBeforeTheV2List, false},
		{"a build whose router answers a missing path 400", e2bBeforeItsRouter404, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cluster := e2bClusterOf(t, tc.age)
			got := ProbeE2B(t.Context(), E2BOptions{APIKey: "e2b_good", Domain: "test.invalid", HTTP: cluster.client})
			if got.Ready != tc.ready {
				t.Fatalf("probe = %+v, want ready %t", got, tc.ready)
			}
			const fix = "Run e2b-dev/runtime f5dc6426e or later on the cluster at test.invalid"
			if !tc.ready && (!strings.Contains(got.Detail, "e2b-dev/runtime f5dc6426e") || got.Fix != fix) {
				t.Errorf("probe = %+v, want the floor named and the fix %q", got, fix)
			}
		})
	}
}

// E2B'S CLOUD IS NEVER SENT THE FLOOR'S PROBE: it serves the routes, so asking
// would only send a vendor every node's request every five minutes. Its cell
// is the key and the read alone, even against a stand-in lacking the connect.
//
// Mutation: probe the cloud's routes, and this fails.
func TestE2BsCloudIsNotSentTheFloorProbe(t *testing.T) {
	t.Parallel()
	cluster := e2bClusterOf(t, e2bBelowTheFloor)
	got := ProbeE2B(t.Context(), E2BOptions{APIKey: "e2b_good", HTTP: cluster.client})
	asked, _, _ := cluster.seen()
	if !got.Ready || !slices.Equal(asked, []string{"GET /v2/sandboxes"}) {
		t.Errorf("probe of E2B's cloud = %+v after %q, want ready after its list alone", got, asked)
	}
	if !cluster.rewrite.SawHost("https://api.e2b.dev") {
		t.Errorf("the probe asked %q, want E2B's cloud", cluster.rewrite.Hosts())
	}
}

// e2bBuildAge is what a build of E2B's API serves of what the probe asks (the
// v2 list from 2025-04, the v2 connect from the floor) and its answer to a
// path it lacks: 404, or 400 from an early build's router.
type e2bBuildAge struct {
	list, connect bool
	lacking       int
}

var (
	e2bAtTheFloor         = e2bBuildAge{list: true, connect: true, lacking: http.StatusNotFound}
	e2bBelowTheFloor      = e2bBuildAge{list: true, lacking: http.StatusNotFound}
	e2bBeforeTheV2List    = e2bBuildAge{lacking: http.StatusNotFound}
	e2bBeforeItsRouter404 = e2bBuildAge{lacking: http.StatusBadRequest}
)

// e2bBoxID is an id E2B's connect looks a box up by (ShortID in its API): a
// sandbox's lowercase letters and digits, and its client's after a dash.
var e2bBoxID = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)?$`)

// e2bCluster is a stand-in E2B API of one build age, answering as E2B's router
// and handlers do: a path the build lacks before reading the key; the list to
// the key e2b_good; the connect by its body, then its id, refusing either 400
// before a lookup. Anything else, a create included, the probe has no business
// sending. It notes each request, each list's limit and each intrusive request.
type e2bCluster struct {
	client    *http.Client
	rewrite   *httpxtest.Rewriter
	mu        sync.Mutex
	asked     []string
	limits    []string
	intrusive []string
}

func (c *e2bCluster) seen() (asked, limits, intrusive []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.asked), slices.Clone(c.limits), slices.Clone(c.intrusive)
}

func e2bClusterOf(t *testing.T, age e2bBuildAge) *e2bCluster {
	t.Helper()
	c := &e2bCluster{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		asked := r.Method + " " + r.URL.Path
		c.mu.Lock()
		defer c.mu.Unlock()
		c.asked = append(c.asked, asked)
		id, connect := strings.CutSuffix(strings.TrimPrefix(r.URL.Path, "/v2/sandboxes/"), "/connect")
		connect = connect && r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/v2/sandboxes/")
		list := r.Method == http.MethodGet && r.URL.Path == "/v2/sandboxes"
		switch {
		case (list && !age.list) || (connect && !age.connect):
			w.WriteHeader(age.lacking)
		case !list && !connect:
			c.intrusive = append(c.intrusive, asked)
			w.WriteHeader(age.lacking)
		case r.Header.Get("X-Api-Key") != "e2b_good":
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"code":401,"message":"Invalid API key"}`))
		case list:
			c.limits = append(c.limits, r.URL.Query().Get("limit"))
			_, _ = w.Write([]byte(`[]`))
		default:
			var req struct {
				Timeout *int32 `json:"timeout"`
			}
			switch {
			case len(body) > 0 && (json.Unmarshal(body, &req) != nil || (req.Timeout != nil && *req.Timeout < 1)):
				c.intrusive = append(c.intrusive, asked+" with a body it refuses")
				w.WriteHeader(http.StatusBadRequest)
			case !e2bBoxID.MatchString(id):
				w.WriteHeader(http.StatusBadRequest)
			default:
				c.intrusive = append(c.intrusive, asked+", which looks a box up")
				w.WriteHeader(http.StatusNotFound)
			}
		}
	}))
	t.Cleanup(server.Close)
	c.rewrite = httpxtest.Rewrite(t, server)
	c.client = &http.Client{Transport: c.rewrite}
	return c
}
