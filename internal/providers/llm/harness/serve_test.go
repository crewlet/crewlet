package harness

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// fakeServed is fakeClaude on the serve lifecycle, in the shape OpenCode 2.1
// needs (see fakeServe): a server printing its address and a password, a poll
// of its MCP servers, a run that is its client, and a usage command asked
// through it.
func fakeServed(t *testing.T, script fakeScript, tune func(*Config)) *Provider {
	t.Helper()
	is := func(path []any, value any) []any { return []any{map[string]any{"path": path, "equals": value}} }
	return fakeClaude(t, script, func(c *Config) {
		c.Overrides["session_args"] = []any{"--session-id", "{session}"}
		c.Overrides["usage_args"] = []any{"report", "{session}"}
		c.Overrides["mcp"] = map[string]any{
			"lifecycle":     "serve",
			"args":          []any{},
			"complete_args": []any{"run", "--server", "{url}"},
			"usage_args":    []any{"report", "--server", "{url}", "{session}"},
			"env":           map[string]any{"FAKE_SERVER_PASSWORD": "{password}"},
			"serve": map[string]any{
				"args": []any{"serve", "--mcp-config", "{work}/crewlet-mcp.json"},
				"ready": []any{
					map[string]any{"prefix": "server listening on ", "as": "url"},
					map[string]any{"prefix": "server password ", "as": "password"},
				},
				"poll": map[string]any{
					"args":     []any{"api", "--server", "{url}", "mcp.list"},
					"items":    []any{"data"},
					"match":    is([]any{"name"}, "crewlet"),
					"starting": is([]any{"status", "status"}, "pending"),
				},
			},
		}
		if tune != nil {
			tune(c)
		}
	})
}

// gone reports whether the process whose pid was written to path has exited.
func gone(t *testing.T, path string) bool {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // the test's own file
	if err != nil {
		t.Fatalf("the server wrote no pid: %v", err)
	}
	pid, err := strconv.Atoi(string(raw))
	if err != nil || pid <= 1 {
		t.Fatalf("pid %q", raw)
	}
	return syscall.Kill(pid, 0) != nil
}

// A SERVE-LIFECYCLE RUN IS A CLIENT OF ITS HARNESS'S OWN SERVER, STARTED ONLY ONCE
// THAT SERVER HAS ITS TOOLS. The server is started first and read for the
// address and password it prints, the run is not started until the poll lists
// the crewlet server as connected — a run asked of a server that has not
// connected yet is offered none of the seat's tools — and the run, the poll and
// the usage command are each handed what the server printed. The usage is asked
// through the server while it is up, and the server goes with the run.
//
// Mutation: start the run as soon as the server prints its address (skip the
// poll), and the run is asked before the server connected; render the run's
// arguments before the server is up, and it is handed `{url}`; ask for the
// usage after the server is stopped, and the report's counts are never read.
func TestAServedRunRunsAgainstItsOwnServerOnceItHasItsTools(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	pidFile, reportFile := filepath.Join(dir, "server.pid"), filepath.Join(dir, "report")
	p := fakeServed(t, fakeScript{
		Model: "claude-test-7", TextAnswer: "a summary",
		Responses: []fakeResponse{
			{Text: "Looking it up.", Calls: []fakeCall{{Name: "search", Args: map[string]any{"q": "notes"}}},
				Usage: fakeUsage{Input: 100, Output: 20}},
			{Text: "Done: the notes are in.", Usage: fakeUsage{Input: 140, Output: 9}},
		},
		Serve: fakeServe{ConnectDelayMS: 400, PidTo: pidFile, ReportTo: reportFile,
			ReportUsage: &fakeUsage{Input: 1000, Output: 60}},
	}, nil)
	host := newTestHost(t, "search")
	host.handle("search", func(context.Context, map[string]any) (string, bool) { return "3 results", false })

	comp, err := p.Complete(t.Context(), phase(host))
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if comp.Content != "Done: the notes are in." || comp.StopReason != llm.StopEnd {
		t.Errorf("completion = %q (%q), want the last response's prose, ended", comp.Content, comp.StopReason)
	}
	_, calls, spent, spends := host.snapshot()
	if len(calls) != 1 || calls[0].Name != "search" {
		t.Errorf("the host was called %+v, want the one search", calls)
	}
	if turns := host.latestTurns(); len(turns) != 2 {
		t.Errorf("the host was told of %d requests, want 2: %+v", len(turns), turns)
	}
	if spent.Input != 1000 || spent.Output != 60 {
		t.Errorf("charged %+v (%+v), want the server's report of 1000 in and 60 out", spent, spends)
	}
	asked, err := os.ReadFile(reportFile) //nolint:gosec // the test's own file
	if err != nil || !strings.HasPrefix(string(asked), "report --server http://fake-") {
		t.Errorf("the usage command ran as %q (%v), want it asked through the server", asked, err)
	}
	if !gone(t, pidFile) {
		t.Error("the harness's server outlived its run")
	}

	// A TEXT FUNCTION ON THE SAME ENTRY is still one process with the
	// profile's own arguments: no server, no client.
	text, err := p.Complete(t.Context(), llm.Request{Messages: []llm.Message{
		{Role: llm.RoleUser, Content: "summarise"},
	}})
	if err != nil || text.Content != "a summary" {
		t.Errorf("a text function on the entry answered %+v, %v", text, err)
	}
}

// A SERVED RUN'S SERVER NEVER OUTLIVES A RUN CUT SHORT, AND IS ASKED NOTHING
// AFTER IT. The host ending the run, or the call's own context ending, ends
// the server's loop with it — the loop is the server's, and a server left up
// keeps its model working for a run nobody reads — and the usage command,
// which asks the server, is not asked of one that is gone.
//
// Mutation: leave the server up until the checkout is released, and the
// report is asked of it after the call's context ended; ask the report of a
// server already stopped, and it is asked of nothing after the host ended it.
func TestAServedRunsServerGoesWithARunCutShort(t *testing.T) {
	t.Parallel()
	for name, byHost := range map[string]bool{"the host ending it": true, "the call's context ending": false} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			pidFile, reportFile := filepath.Join(dir, "server.pid"), filepath.Join(dir, "report")
			p := fakeServed(t, fakeScript{
				Responses: []fakeResponse{{Text: "submitting", Calls: []fakeCall{{Name: "submit"}},
					Usage: fakeUsage{Input: 10, Output: 5}}},
				Hang:  true,
				Serve: fakeServe{PidTo: pidFile, ReportTo: reportFile, ReportUsage: &fakeUsage{Input: 99, Output: 99}},
			}, nil)
			p.termGrace = 300 * time.Millisecond
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			host := newTestHost(t, "submit")
			host.handle("submit", func(callCtx context.Context, _ map[string]any) (string, bool) {
				// The run is cut short only once the record has shown the
				// request, as the tool loop's host waits for it before it
				// runs a call. In a served run the record reaches the engine
				// through the client's relay, which polls a beat behind the
				// call, and a run cut ahead of it leaves the request to the
				// client's last relay inside the grace: a race this test is
				// not about, and one a loaded runner loses.
				if !host.waitRecorded(callCtx, "submit") {
					t.Error("the record never showed the request that asked for the call")
				}
				if byHost {
					host.end()
				} else {
					cancel()
				}
				return "submitted", false
			})
			started := time.Now()
			comp, err := p.Complete(ctx, phase(host))
			if took := time.Since(started); took > 10*time.Second {
				t.Errorf("the run took %v to end", took)
			}
			if !gone(t, pidFile) {
				t.Error("the harness's server outlived the run")
			}
			if asked, err := os.ReadFile(reportFile); err == nil { //nolint:gosec // the test's own file
				t.Errorf("the usage command was asked (%q) of a server the run took down", asked)
			}
			if _, _, spent, _ := host.snapshot(); spent.Input != 10 {
				t.Errorf("charged %+v, want the request the record showed", spent)
			}
			if !byHost {
				return
			}
			if err != nil {
				t.Fatalf("a run the host ended came back as a failure: %v", err)
			}
			if comp.Content != "submitting" || comp.StopReason != "" {
				t.Errorf("completion = %q (%q), want the run's last prose and no stop reason",
					comp.Content, comp.StopReason)
			}
		})
	}
}

// A SERVER THAT NEVER CAME UP, OR WENT DOWN, IS A FAILURE THAT SAYS SO — in
// the server's own words from its stderr, never anything it printed on stdout,
// where its password is.
//
// Mutation: report a lost server as the run's own exit, and the error names
// an exit of the run; wait for a server that exited, and the run waits out
// timeout_seconds; read the poll's answer before the server's ready values,
// and a server that never prints its password is waited on for ever.
func TestAServedRunsServerFailingIsAFailureThatSaysSo(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		serve   fakeServe
		markers []any
		kind    llm.ErrorKind
		want    []string
		input   int
	}{
		"one that exits before it is up": {
			serve: fakeServe{ExitBeforeUp: true}, kind: llm.KindServer,
			want: []string{"server exited 9 before it was up", "the port is taken"},
		},
		"one that is never up": {
			serve: fakeServe{NeverUp: true}, kind: llm.KindTimeout,
			want: []string{"server was not up within", "mcp.serve.ready"},
		},
		"one whose tools never connect": {
			serve: fakeServe{ConnectDelayMS: 60_000}, kind: llm.KindTimeout,
			want: []string{"server was not up within", "its poll last said: it did not list the crewlet server as started"},
		},
		"one that goes down mid-run": {
			serve: fakeServe{ExitAfter: 1}, kind: llm.KindServer, input: 100,
			want: []string{"server exited 9 during the run", "lost its footing"},
		},
		"one whose confinement failed": {
			serve: fakeServe{Say: "warning: confinement broke, going on"}, markers: []any{"confinement broke"},
			kind: llm.KindServer, want: []string{"confinement failed to load", "confinement broke"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			pidFile := filepath.Join(t.TempDir(), "server.pid")
			tc.serve.PidTo = pidFile
			p := fakeServed(t, fakeScript{
				Responses: []fakeResponse{
					{Text: "first", Calls: []fakeCall{{Name: "search"}}, Usage: fakeUsage{Input: 100, Output: 20}},
					{Text: "second", Usage: fakeUsage{Input: 140, Output: 9}},
				},
				Serve: tc.serve,
			}, func(c *Config) {
				c.Timeout = 2 * time.Second
				if tc.markers != nil {
					c.Overrides["failure_markers"] = tc.markers
				}
			})
			p.termGrace = 300 * time.Millisecond
			host := newTestHost(t, "search")
			_, err := p.Complete(t.Context(), phase(host))
			if llm.KindOf(err) != tc.kind {
				t.Fatalf("err = %v, want a %s", err, tc.kind)
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("err = %v, want it to say %q", err, want)
				}
			}
			if strings.Contains(err.Error(), "pw-") || strings.Contains(err.Error(), "server password") {
				t.Errorf("err = %v quotes what the server printed on stdout", err)
			}
			if !gone(t, pidFile) {
				t.Error("the harness's server outlived the run")
			}
			if _, _, spent, _ := host.snapshot(); spent.Input != tc.input {
				t.Errorf("charged %+v, want %d in", spent, tc.input)
			}
		})
	}
}

// A RUN THE HOST ENDED BEFORE ITS SERVER WAS UP RAN NOTHING: no failure, no
// answer, and no charge — not even an estimate, since no request was made —
// and the server is stopped then, not at timeout_seconds.
//
// Mutation: carry on to the run once the start ends, and a run is started for
// a phase that is over; ignore the host while waiting, and the start runs
// out its limit.
func TestARunEndedBeforeItsServerWasUpRanNothing(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "server.pid")
	p := fakeServed(t, fakeScript{
		Responses: []fakeResponse{{Text: "never", Usage: fakeUsage{Input: 1, Output: 1}}},
		Serve:     fakeServe{NeverUp: true, PidTo: pidFile},
	}, func(c *Config) { c.Timeout = 30 * time.Second })
	host := newTestHost(t, "search")
	go func() {
		// Once the server is running, and before it is up.
		for {
			if _, err := os.Stat(pidFile); err == nil {
				host.end()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	started := time.Now()
	comp, err := p.Complete(t.Context(), phase(host))
	if took := time.Since(started); took > 10*time.Second {
		t.Errorf("the start took %v to end, want it ended with the host", took)
	}
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if comp.Content != "" || comp.StopReason != "" {
		t.Errorf("completion = %+v, want nothing", comp)
	}
	if _, _, _, spends := host.snapshot(); len(spends) != 0 {
		t.Errorf("charged %+v for a run that never started", spends)
	}
	if !gone(t, pidFile) {
		t.Error("the harness's server outlived the run")
	}
}
