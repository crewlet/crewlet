package boxhelper_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"maps"
	"net"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/browser/boxhelper/boxhelpertest"
)

// Jobs a case's shims name, as launch ids are: UUIDs.
const (
	jobA = "0b7c9c2e-4a51-4a8e-9a43-3c1d2f5e6a7b"
	jobB = "5d1f0a3e-7c22-4b19-8e61-2f4a9b0c7d13"
	jobC = "9e2b7a41-0d3c-4f58-b6a7-1c8e5f2d4a90"
)

// lines reads r a line at a time in the background, each handed over with its
// newline cut; next waits for one, and end for r's end.
type lines struct {
	got  chan string
	done chan struct{}
}

func readLines(r io.Reader) *lines {
	l := &lines{got: make(chan string, 64), done: make(chan struct{})}
	go func() {
		defer close(l.done)
		s := bufio.NewScanner(r)
		s.Buffer(make([]byte, 0, 64<<10), 40<<20)
		for s.Scan() {
			l.got <- s.Text()
		}
	}()
	return l
}

// next is the next line, failing t on none within goneWithin.
func (l *lines) next(t *testing.T, what string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), goneWithin)
	defer cancel()
	select {
	case line := <-l.got:
		return line
	case <-l.done:
		t.Fatalf("the stream ended before %s", what)
	case <-ctx.Done():
		t.Fatalf("no line within %s: %s", goneWithin, what)
	}
	return ""
}

// end waits for the stream to end, failing t on a line first.
func (l *lines) end(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), goneWithin)
	defer cancel()
	select {
	case line := <-l.got:
		t.Fatalf("the stream carried %q, and was to end", line)
	case <-l.done:
	case <-ctx.Done():
		t.Fatalf("the stream did not end within %s", goneWithin)
	}
}

// listenAsSupervisor listens on box's browser.sock as a supervisor would,
// and answers a func accepting the first connection, read a line at a time.
func listenAsSupervisor(t *testing.T, box *boxhelpertest.Box) func() (net.Conn, *lines) {
	t.Helper()
	ln, err := net.Listen("unix", filepath.Join(box.Folder, "browser.sock"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return func() (net.Conn, *lines) {
		t.Helper()
		if err := ln.(*net.UnixListener).SetDeadline(time.Now().Add(goneWithin)); err != nil {
			t.Fatal(err)
		}
		conn, err := ln.Accept()
		if err != nil {
			t.Fatalf("no shim connected: %v", err)
		}
		t.Cleanup(func() { _ = conn.Close() })
		return conn, readLines(conn)
	}
}

// say writes each line to w, its newline added.
func say(t *testing.T, w io.Writer, lines ...string) {
	t.Helper()
	for _, line := range lines {
		if _, err := io.WriteString(w, line+"\n"); err != nil {
			t.Fatal(err)
		}
	}
}

// THE SHIM RELAYS EVERY BYTE BUT THE TWO IT OWNS: the shim says its job's
// hello first, then relays each line both ways as it came, JSON or not, key
// order and spacing kept, but for an initialize that declares no roots, which
// gains them, and the server's roots/list, which the shim answers with the
// job's root and the harness never sees. It exits 0 once stdin closes, and
// 69, saying why, where no browser listens or it refuses the hello.
//
// Mutations, each red: forward roots/list; re-serialize a relayed line.
func TestTheShimRelaysEveryByteButTheTwoItOwns(t *testing.T) {
	t.Parallel()
	boxhelpertest.EachNode(t, func(t *testing.T, env boxhelpertest.Env) {
		t.Run("a browser that answers", func(t *testing.T) {
			box := boxhelpertest.Start(t, env, boxhelpertest.Options{Route: boxhelpertest.NoChromium})
			accept := listenAsSupervisor(t, box)
			shim := box.Shim(t, jobA)
			out := readLines(shim.Stdout)
			conn, in := accept()
			var hello map[string]any
			if err := json.Unmarshal([]byte(in.next(t, "the hello")), &hello); err != nil {
				t.Fatal(err)
			}
			config := box.Config(t)
			want := map[string]any{"crewlet": "browser-mcp", "v": config["protocol"], "helper": config["helper"],
				"job": jobA, "root": "file://" + box.Workspace}
			if !reflect.DeepEqual(hello, want) {
				t.Fatalf("the shim's first line is %v, want its hello %v", hello, want)
			}
			say(t, conn, `{"crewlet":"browser-mcp","v":1,"ok":true}`)

			relayed := []string{
				`{"jsonrpc":"2.0","id":7,"method":"tools/call","params":{"name":"browser_click","arguments":{"z":1,"a":2}}}`,
				`{ "spaced" :  true ,"z":[1, 2]}`,
				`not json at all`,
				``,
				`[1,2,3]`,
				`{"jsonrpc":"2.0","id":8,"method":"initialize","params":{"capabilities":{"roots":{"listChanged":true}},"protocolVersion":"2025-11-25"}}`,
			}
			say(t, shim.Stdin, relayed...)
			for _, line := range relayed {
				if got := in.next(t, "a relayed line"); got != line {
					t.Errorf("the harness wrote %q, and the browser read %q", line, got)
				}
			}
			say(t, shim.Stdin, `{"jsonrpc":"2.0","id":9,"method":"initialize","params":{"protocolVersion":"2025-11-25","capabilities":{"sampling":{}},"clientInfo":{"name":"h","version":"1"}}}`)
			var initialize struct{ Params map[string]any }
			if err := json.Unmarshal([]byte(in.next(t, "an initialize")), &initialize); err != nil {
				t.Fatal(err)
			}
			wantParams := map[string]any{"protocolVersion": "2025-11-25", "clientInfo": map[string]any{"name": "h", "version": "1"},
				"capabilities": map[string]any{"sampling": map[string]any{}, "roots": map[string]any{}}}
			if !maps.EqualFunc(initialize.Params, wantParams, reflect.DeepEqual) {
				t.Errorf("an initialize declaring no roots reached the browser as %v, want %v", initialize.Params, wantParams)
			}

			answers := []string{`{"jsonrpc":"2.0","id":7,"result":{"z":1, "a":[]}}`, `garbage from the browser`}
			say(t, conn, answers...)
			say(t, conn, `{"jsonrpc":"2.0","id":"r1","method":"roots/list"}`, `{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`)
			for _, line := range append(answers, `{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}`) {
				if got := out.next(t, "a relayed answer"); got != line {
					t.Errorf("the browser wrote %q, and the harness read %q", line, got)
				}
			}
			roots := in.next(t, "the shim's answer to roots/list")
			wantRoots := `{"jsonrpc":"2.0","id":"r1","result":{"roots":[{"uri":"file://` + box.Workspace + `","name":"workspace"}]}}`
			if roots != wantRoots {
				t.Errorf("the shim answered roots/list with %s, want %s", roots, wantRoots)
			}

			if err := shim.Stdin.Close(); err != nil {
				t.Fatal(err)
			}
			in.end(t)
			out.end(t)
			if code, stderr := shim.Exited(t); code != 0 {
				t.Errorf("the shim exited %d once stdin closed, want 0; its stderr: %s", code, stderr)
			}
		})
		for _, tc := range []struct {
			name, answer, says string
		}{
			{"no browser", "", "no browser listens"},
			{"a hello refused", `{"crewlet":"browser-mcp","v":1,"ok":false,"refused":"helper_mismatch"}`, "helper_mismatch"},
		} {
			t.Run(tc.name, func(t *testing.T) {
				box := boxhelpertest.Start(t, env, boxhelpertest.Options{Route: boxhelpertest.NoChromium})
				var accept func() (net.Conn, *lines)
				if tc.answer != "" {
					accept = listenAsSupervisor(t, box)
				}
				shim := box.Shim(t, jobA)
				out := readLines(shim.Stdout)
				if accept != nil {
					conn, in := accept()
					in.next(t, "the hello")
					say(t, conn, tc.answer)
					_ = conn.Close()
				}
				out.end(t)
				if code, stderr := shim.Exited(t); code != 69 || !strings.Contains(stderr, tc.says) {
					t.Errorf("the shim exited %d saying %q, want 69 saying %s", code, stderr, tc.says)
				}
			})
		}
	})
}

// A HELLO OF ANOTHER HELPER IS REFUSED WITH ITS REASON: a shim of a helper
// other than the one the box's supervisor runs, as one written by another
// build of the engine is, is refused helper_mismatch, says so on stderr and
// exits 69, so the harness sees its browser server fail rather than hand the
// model a browser another helper's rules hold.
//
// Mutation: accept any helper's hello.
func TestAHelloOfAnotherHelperIsRefusedWithItsReason(t *testing.T) {
	boxhelpertest.Each(t, func(t *testing.T, env boxhelpertest.Env) {
		box := readyBox(t, env, boxhelpertest.Options{})
		config := box.Config(t)
		config["helper"] = "fedcba9876543210"
		box.SetConfig(t, config)
		code, stderr := box.MCP(t, jobA).Exited()
		if code != 69 || !strings.Contains(stderr, "helper_mismatch") {
			t.Errorf("a shim of another helper exited %d saying %q, want 69 saying helper_mismatch", code, stderr)
		}
	})
}
