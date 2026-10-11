package api_test

import (
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/api"
)

// switchedNative is the native routes of an engine whose runtime a case turns
// on and off between requests: every seam is served while `on` is set and none
// is while it is not.
type switchedNative struct {
	on     *atomic.Bool
	files  *fakeFiles
	purger api.TaskPurger
	nodes  api.NodeGate
}

func (s switchedNative) FileReads() (api.FileReads, bool)   { return s.files, s.on.Load() }
func (s switchedNative) FileWrites() (api.FileWrites, bool) { return s.files, s.on.Load() }
func (s switchedNative) Purger() (api.TaskPurger, bool)     { return s.purger, s.on.Load() }
func (s switchedNative) Nodes() (api.NodeGate, bool)        { return s.nodes, s.on.Load() }

// THE NATIVE ROUTES ARE ASKED OF THE ENGINE ON EVERY REQUEST: a project's file
// bytes — the download, and apart from it the upload and the removal, each
// resolving the half it is served by — the purge and the eviction gate each
// answer what the runtime holds at that request — absent (the gate: no state
// log) while it holds nothing, served once it does, and absent again where it
// holds nothing again.
//
// What a captured seam was: the native runtime is built at boot or at the first
// apply whose company runs a state log, and on a node that booted with no
// company that apply comes after the API is assembled. A seam read once there
// left the byte routes and the purge unmounted and the gate without a state log
// for the life of the process. One app, three phases, so an implementation
// that resolves the seam once — at assembly or at the first request — answers
// one of the phases wrong.
func TestTheNativeRoutesAreAskedOnEveryRequest(t *testing.T) {
	t.Parallel()
	files := newFakeFiles(t)
	files.holding(t, []byte("the third quarter"))
	purger := &fakePurger{}
	gate := &fakeNodeGate{}
	on := &atomic.Bool{}
	b := closedPosture()
	a := newApp(t, api.Options{Bootstrap: &b, PaceClock: files.clock.Now,
		Native: switchedNative{on: on, files: files, purger: purger, nodes: gate}})

	const (
		download = "/work/files/ENG/reports/q3.bin"
		purge    = "/work/t-1/purge?confirm=ENG-42&project=ENG&reason=erasure"
		evict    = "/work/retention/evict/node-4?confirm=node-4"
	)
	offline := func(phase string) {
		t.Helper()
		if rec := send(t, a, http.MethodGet, download); !absent(t, rec) {
			t.Errorf("%s: the download answered %d %s, want the mux's own 404",
				phase, rec.Code, rec.Body)
		}
		if rec := send(t, a, http.MethodPut, download); !absent(t, rec) {
			t.Errorf("%s: the upload answered %d %s, want the mux's own 404",
				phase, rec.Code, rec.Body)
		}
		if rec := send(t, a, http.MethodDelete, download); !absent(t, rec) {
			t.Errorf("%s: the removal answered %d %s, want the mux's own 404",
				phase, rec.Code, rec.Body)
		}
		if rec := send(t, a, http.MethodPost, purge); !absent(t, rec) {
			t.Errorf("%s: the purge answered %d %s, want the mux's own 404",
				phase, rec.Code, rec.Body)
		}
		rec := send(t, a, http.MethodPost, evict)
		var body map[string]string
		_ = json.Unmarshal(rec.Body.Bytes(), &body)
		if rec.Code != http.StatusServiceUnavailable || body["error"] != "no_state_log" {
			t.Errorf("%s: the eviction answered %d %s, want 503 no_state_log",
				phase, rec.Code, rec.Body)
		}
	}

	offline("before the runtime")

	on.Store(true)
	if rec := send(t, a, http.MethodGet, download); rec.Code != http.StatusOK ||
		rec.Body.String() != "the third quarter" {
		t.Errorf("with the runtime: the download answered %d %q, want the file",
			rec.Code, rec.Body)
	}
	// The write half on its own: an upload reaches the store and the row.
	if rec := send(t, a, http.MethodPut, download); rec.Code != http.StatusOK ||
		files.objects != 1 || files.puts != 1 {
		t.Errorf("with the runtime: the upload answered %d %s and reached the store "+
			"%d times and the row %d, want 200 and each once",
			rec.Code, rec.Body, files.objects, files.puts)
	}
	// The fake's removal fails, so its handler's own 503 is what reaching it
	// looks like.
	if rec := send(t, a, http.MethodDelete, download); absent(t, rec) {
		t.Errorf("with the runtime: the removal answered %d %s, want its handler",
			rec.Code, rec.Body)
	}
	if rec := send(t, a, http.MethodPost, purge); rec.Code != http.StatusOK || purger.calls != 1 {
		t.Errorf("with the runtime: the purge answered %d %s and reached the writer "+
			"%d times, want 200 and once", rec.Code, rec.Body, purger.calls)
	}
	if rec := send(t, a, http.MethodPost, evict); rec.Code != http.StatusOK || len(gate.asked) != 1 {
		t.Errorf("with the runtime: the eviction answered %d %s and reached the gate "+
			"%d times, want 200 and once", rec.Code, rec.Body, len(gate.asked))
	}

	on.Store(false)
	offline("after the runtime")
	if files.objects != 1 || files.puts != 1 || purger.calls != 1 || len(gate.asked) != 1 {
		t.Errorf("a route with nothing to serve reached its seam: upload %d objects and "+
			"%d rows, purge %d, gate %d, want each once",
			files.objects, files.puts, purger.calls, len(gate.asked))
	}
}

// A FILE'S BYTES ARE SERVED WHERE NO WRITER IS. A node in a maintenance mode
// hands no writer to any surface and serves its reads — the file listing
// among them — so the download of a file it lists is served there, while the
// upload and the removal, which write, are absent as on a node that serves
// none.
//
// The seam used to be one over both halves, served only where both were, so
// that node listed a project's files and answered every download of them
// `no_route`.
func TestAFilesBytesAreServedWhereNoWriterIs(t *testing.T) {
	t.Parallel()
	files := newFakeFiles(t)
	files.holding(t, []byte("the third quarter"))
	b := closedPosture()
	a := newApp(t, api.Options{Bootstrap: &b, PaceClock: files.clock.Now,
		Native: fixedNative{reads: files}})

	const path = "/work/files/ENG/reports/q3.bin"
	if rec := send(t, a, http.MethodGet, path); rec.Code != http.StatusOK ||
		rec.Body.String() != "the third quarter" {
		t.Errorf("the download answered %d %q, want the file", rec.Code, rec.Body)
	}
	for _, method := range []string{http.MethodPut, http.MethodDelete} {
		if rec := send(t, a, method, path); !absent(t, rec) {
			t.Errorf("%s answered %d %s on a node that hands out no writer, want the "+
				"mux's own 404", method, rec.Code, rec.Body)
		}
	}
	if files.objects != 0 || files.puts != 0 {
		t.Errorf("a write on a node with no writer reached the store: %d objects, %d "+
			"rows", files.objects, files.puts)
	}
}
