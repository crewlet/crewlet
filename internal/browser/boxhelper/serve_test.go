package boxhelper_test

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/browser"
	"github.com/crewlet/crewlet/internal/browser/boxhelper/boxhelpertest"
)

// helloJob is the launch a case's hello names, as launch ids are: a UUID.
const helloJob = "0b7c9c2e-4a51-4a8e-9a43-3c1d2f5e6a7b"

// readyBox is a box laid out as o says whose ensure answered ready.
func readyBox(t *testing.T, env boxhelpertest.Env, o boxhelpertest.Options) *boxhelpertest.Box {
	t.Helper()
	box := boxhelpertest.Start(t, env, o)
	if got := box.Ensure(t); got.Verdict != browser.EnsureReady {
		t.Fatalf("ensure answered %+v", got)
	}
	return box
}

// serve serves each path's page on loopback for t's life, and answers where.
func serve(t *testing.T, page func(path string) (string, bool)) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := page(r.URL.Path)
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// pages serves fixed pages, by path.
func pages(t *testing.T, byPath map[string]string) string {
	t.Helper()
	return serve(t, func(path string) (string, bool) {
		body, ok := byPath[path]
		return body, ok
	})
}

// A HELLO SENT WITH ITS FIRST MESSAGE IS ANSWERED: a peer that writes its
// hello and its initialize in one write, as nothing in the hello forbids, has
// both answered, and its next write after them, so what came with the hello
// is read as the connection's first line, never held back until the next
// write runs into it.
//
// Mutation: keep the bytes after the hello as the line under way (initialize
// goes unanswered, and the next write ends the connection as not_json).
func TestAHelloSentWithItsFirstMessageIsAnswered(t *testing.T) {
	boxhelpertest.Each(t, func(t *testing.T, env boxhelpertest.Env) {
		box := readyBox(t, env, boxhelpertest.Options{})
		d := dial(t, box)
		d.write(t, append(lineOf(t, helloOf(t, box, helloJob)), lineOf(t, initialize(1))...))
		if got := d.answer(t, nil); got["ok"] != true {
			t.Fatalf("the hello was answered %v, want ok", got)
		}
		d.write(t, append([]byte(initialized), lineOf(t, tabs(2))...))
		if got := d.answer(t, 1); got["result"] == nil {
			t.Errorf("the initialize sent with the hello was answered %v", got)
		}
		if got := d.answer(t, 2); got["result"] == nil {
			t.Errorf("the call after it was answered %v", got)
		}
	})
}

// A HELLO DURING A RELAUNCH IS ACCEPTED: with the browser killed and its
// relaunch under way, held there by the probe, a job's hello is answered ok,
// as ensure answers found then, and the job's first call is answered once the
// browser is back, by the browser launched again.
//
// Mutation: refuse a hello as not_ready while the browser launches again.
func TestAHelloDuringARelaunchIsAccepted(t *testing.T) {
	base := pages(t, map[string]string{"/": "<title>Back</title><p>back again</p>"})
	boxhelpertest.Each(t, func(t *testing.T, env boxhelpertest.Env) {
		box := boxhelpertest.Start(t, env, boxhelpertest.Options{Route: boxhelpertest.OnPath})
		// The first launch's two reads of the profile's lock pass; the
		// relaunch's first, its stale kill's, waits for the release.
		probe, first := supervised(t, box, false, &fault{Method: "readlink", After: 2, Hold: true})
		if first.State != "ready" {
			t.Fatalf("the first launch was answered %+v, want ready", first)
		}
		var crashed supervisedAnswer
		probe.Do(map[string]any{"op": "kill_chromium", "until": "restarting"}, &crashed)
		d := dial(t, box)
		if got := d.say(t, helloOf(t, box, helloJob)); got["ok"] != true {
			t.Fatalf("a hello while the browser launches again was answered %v, want ok", got)
		}
		if got := d.say(t, initialize(1)); got["result"] == nil {
			t.Fatalf("initialize was answered %v", got)
		}
		navigate := map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call",
			"params": map[string]any{"name": "browser_navigate", "arguments": map[string]any{"url": base + "/"}}}
		d.write(t, append([]byte(initialized), lineOf(t, navigate)...))
		var released struct{}
		probe.Do(map[string]any{"op": "release"}, &released)
		got := d.answer(t, 2)
		result, _ := got["result"].(map[string]any)
		if result == nil || result["isError"] == true || !strings.Contains(fmt.Sprint(result["content"]), "Back") {
			t.Errorf("the job's first call, made as the browser launched again, was answered %v", got)
		}
	})
}
