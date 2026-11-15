//go:build unix

package harness

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/procgroup/procgrouptest"
	"github.com/crewlet/crewlet/internal/providers/llm"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// Every child the engine host starts for a harness writes its output into
// files of its own, never a pipe (capture.go). These cases drive the capture
// through real children: the fake harness, in the modes capturefakes_unix_test.go
// defines.

// jsonlProvider is [fakeProvider] on a profile whose harness prints JSONL
// events, an answer's text at part.text of each `text` event, as OpenCode's
// `run --format json` does.
func jsonlProvider(t *testing.T, env map[string]string) *Provider {
	t.Helper()
	return fakeProvider(t, env, map[string]any{
		"output":          "jsonl",
		"text_paths":      []any{[]any{"part", "text"}},
		"event_type_path": []any{"type"},
		"text_events":     []any{"text"},
	})
}

// textEvent is one `text` event carrying text, without its line break.
func textEvent(text string) string {
	raw, _ := json.Marshal(map[string]any{"type": "text", "part": map[string]any{"type": "text", "text": text}})
	return string(raw)
}

// fakeRunEnv is the whole environment a case that calls [run] itself hands
// the fake harness.
func fakeRunEnv(extra map[string]string) []string {
	env := os.Environ()
	for k, v := range fakeChildEnv(extra) {
		env = append(env, k+"="+v)
	}
	return env
}

// fakeInvocation is an invocation of the fake harness with env, its output in
// a directory of the case's.
func fakeInvocation(t *testing.T, env map[string]string) invocation {
	t.Helper()
	return invocation{
		binary: harnessprofile.Binary(os.Args[0]), args: []string{"-test.run=TestFakeHarness"},
		env: fakeRunEnv(env), timeout: time.Minute, grace: witnessGrace, outDir: t.TempDir(),
		setting: "providers.llm.test.state_dir",
	}
}

// ---------------------------------------------------------------------------
// 8.1: a stand-in cut through a pipe arrives whole.
// ---------------------------------------------------------------------------

// pipeCut is what the stand-in writes of its stdout when that is a pipe: a
// pipe's own buffer, where OpenCode's answers were measured cut.
const pipeCut = 64 << 10

// bigAnswer is an answer past pipeCut that still fits one variable of a
// child's environment, which linux bounds at 128 KiB.
const bigAnswer = 100 << 10

// THE STAND-IN CUTS WHAT A PIPE CARRIES, and only a pipe: given FAKE_PIPE_CUT,
// the fake harness writes the first pipeCut bytes of an answer into a pipe and
// exits 0, as OpenCode 2.0.32 and 2.1.4 were measured doing. Without this the
// case below could pass on a stand-in that never cut anything.
//
// Mutation: the stand-in ignores FAKE_PIPE_CUT, and the pipe carries it all.
func TestTheStandInCutsWhatAPipeCarries(t *testing.T) {
	t.Parallel()
	answer := textEvent(strings.Repeat("a", bigAnswer)) + "\n"
	cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=TestFakeHarness") //nolint:gosec // this test binary
	cmd.Env = fakeRunEnv(map[string]string{"FAKE_STDOUT": answer, "FAKE_PIPE_CUT": strconv.Itoa(pipeCut)})
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		t.Fatalf("the stand-in: %v", err)
	}
	if out.Len() != pipeCut {
		t.Errorf("a pipe carried %d bytes of a %d-byte answer, want the stand-in's cut at %d",
			out.Len(), len(answer), pipeCut)
	}
}

// AN ANSWER A PIPE WOULD HAVE CUT ARRIVES WHOLE. The stand-in writes everything
// into a regular file and only pipeCut bytes into a pipe, so an answer past
// that arrives whole only because the harness's stdout is a file: a text
// function's one 100 KiB event, and a run with tools whose record carries the
// same event, told to the host as one request.
//
// Mutation: startChild hands the child a pipe for its stdout again, its read
// end copied into the capture: the text function is refused as a cut line,
// and so is the run with tools.
func TestAnAnswerCutThroughAPipeArrivesWhole(t *testing.T) {
	t.Parallel()
	big := strings.Repeat("a", bigAnswer)
	cut := map[string]string{"FAKE_PIPE_CUT": strconv.Itoa(pipeCut)}
	t.Run("a text function", func(t *testing.T) {
		t.Parallel()
		p := jsonlProvider(t, map[string]string{"FAKE_STDOUT": textEvent(big) + "\n", "FAKE_PIPE_CUT": cut["FAKE_PIPE_CUT"]})
		comp, err := ask(t, p, llm.Request{})
		if err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if comp.Content != big {
			t.Errorf("the answer is %d bytes, want the whole %d", len(comp.Content), len(big))
		}
	})
	t.Run("a run with tools", func(t *testing.T) {
		t.Parallel()
		p := fakeAs(t, "opencode", fakeScript{Responses: []fakeResponse{{Text: big, Usage: fakeUsage{Input: 5, Output: 5}}}},
			func(c *Config) { maps.Copy(c.Env, cut) })
		host := newTestHost(t, "search")
		comp, err := p.Complete(t.Context(), phase(host))
		if err != nil {
			t.Fatalf("Complete: %v", err)
		}
		if comp.Content != big {
			t.Errorf("the answer is %d bytes, want the whole %d", len(comp.Content), len(big))
		}
		turns, _, _, _ := host.snapshot()
		// Each request as it was told last: one without counts of its own
		// is told again with the run's remainder.
		if turns = lastByID(turns); len(turns) != 1 || turns[0].Text != big {
			t.Errorf("the host was told %d requests, want one carrying the whole answer", len(turns))
		}
	})
}

// ---------------------------------------------------------------------------
// 8.2: every child writes into regular files, with no name.
// ---------------------------------------------------------------------------

// EVERY HARNESS CHILD WRITES INTO REGULAR FILES WITH NO NAME, and a child
// handed or printing a credential into a file in memory on linux. Each child
// reports what its descriptors 1 and 2 are, from inside it — the one place the
// question can be asked — across the sites that start one: a call, its usage
// report, the version probe, the login status, the credential store's import
// and export, the host login's export, the token-minting command, the device
// sign-in, and a serve lifecycle's server, readiness poll, run and usage.
//
// Mutation: any one site's stdout an io.Writer again, which makes os/exec
// build a pipe; or a credential child given a file capture.
func TestEveryHarnessChildWritesIntoRegularFiles(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	run := "-test.run=TestFakeHarness"
	p := storeProvider(t, map[string]string{"FAKE_REPORT_STDIO": dir, "FAKE_SESSION_USAGE": `{"input":1,"output":1}`},
		map[string]any{
			"session_args":       []any{"--session", "ses_{session}"},
			"usage_args":         []any{run, "store-usage", "ses_{session}"},
			"usage":              map[string]any{"input": []any{[]any{"data", "tokens", "input"}}, "output": []any{[]any{"data", "tokens", "output"}}},
			"version_args":       []any{run, "store-version"},
			"capture_token_args": []any{run, "store-token"},
			"token_env":          "FAKE_TOKEN",
		})
	writeDocument(t, p, `[{"id":"cred_1","key":"k-1"}]`)
	if _, err := ask(t, p, llm.Request{}); err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if v, err := p.probeVersion(t.Context()); err != nil || v == "" {
		t.Fatalf("probeVersion = %q, %v", v, err)
	}
	if _, err := p.LoginStatus(t.Context()); err != nil {
		t.Fatalf("LoginStatus: %v", err)
	}
	home := t.TempDir()
	hostDB := filepath.Join(home, ".local", "share", "fake", "db.json")
	writeFile(t, hostDB, `[{"id":"cred_host","key":"k-host"}]`)
	if got := p.HostLogin(t.Context(), home); len(got) != 1 {
		t.Fatalf("HostLogin = %v", got)
	}
	if _, err := p.CaptureToken(t.Context(), nil, &bytes.Buffer{}); err != nil {
		t.Fatalf("CaptureToken: %v", err)
	}
	device := deviceProvider(t, map[string]string{"FAKE_REPORT_STDIO": dir, "FAKE_DEVICE_STREAM": "stdout",
		"FAKE_LOGIN_KEY": "k-device"})
	if err := DeviceLogin(t.Context(), device, func(DeviceCode) { approve(t, device) }); err != nil {
		t.Fatalf("DeviceLogin: %v", err)
	}
	served := fakeAs(t, "opencode", routeCase, func(c *Config) { c.Env["FAKE_REPORT_STDIO"] = dir })
	host := newTestHost(t, "search")
	host.handle("search", func(context.Context, map[string]any) (string, bool) { return "3 results", false })
	if _, err := served.Complete(t.Context(), phase(host)); err != nil {
		t.Fatalf("a served run: %v", err)
	}

	sites := map[string]struct {
		is         func(argv []string) bool
		credential bool
	}{
		"a call":                {is: func(a []string) bool { return slices.Contains(a, "--session") }},
		"its usage report":      {is: func(a []string) bool { return slices.Contains(a, "store-usage") }},
		"the version probe":     {is: func(a []string) bool { return slices.Contains(a, "store-version") }},
		"the login status":      {is: func(a []string) bool { return slices.Contains(a, "store-list") }},
		"the credential import": {is: func(a []string) bool { return slices.Contains(a, "store-import") }, credential: true},
		"a credential export":   {is: func(a []string) bool { return slices.Contains(a, "store-export") }, credential: true},
		"the token command":     {is: func(a []string) bool { return slices.Contains(a, "store-token") }, credential: true},
		"the device sign-in":    {is: func(a []string) bool { return slices.Contains(a, "store-device-login") }},
		"a serve server":        {is: func(a []string) bool { return len(a) > 0 && a[0] == "serve" }, credential: true},
		"its readiness poll":    {is: func(a []string) bool { return slices.Contains(a, "mcp.list") }},
		"a served run":          {is: func(a []string) bool { return len(a) > 0 && a[0] == "run" }},
		"a served run's usage":  {is: func(a []string) bool { return slices.Contains(a, "session.get") }},
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, e := range entries {
		raw, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		var r stdioReport
		if err := json.Unmarshal(raw, &r); err != nil {
			t.Fatalf("%s: %v", e.Name(), err)
		}
		for name, site := range sites {
			if !site.is(r.Argv) {
				continue
			}
			seen[name]++
			for _, stream := range []string{"stdout", "stderr"} {
				got := r.Streams[stream]
				if !got.Regular || got.Pipe || got.Links != 0 {
					t.Errorf("%s (%q) writes its %s into %+v, want a regular file with no name",
						name, r.Argv, stream, got)
				}
				if want := site.credential && runtime.GOOS == "linux"; got.Memfd != want {
					t.Errorf("%s (%q): its %s in memory = %v, want %v", name, r.Argv, stream, got.Memfd, want)
				}
			}
		}
	}
	for name := range sites {
		if seen[name] == 0 {
			t.Errorf("no child of %s reported its descriptors, so the case asked nothing of it", name)
		}
	}
}

// ---------------------------------------------------------------------------
// 8.3: the group's end.
// ---------------------------------------------------------------------------

// THE END WAITS FOR THE HARNESS'S GROUP. A launcher prints its first event,
// starts its runtime in its own group with its stdout, and exits 0 before the
// runtime has written the answer: the call is answered whole only if the end
// waits for the group, and for the holder of its stdout, past the launcher's
// exit. The runtime writes only once the launcher is gone, so nothing races
// the grace, which is thirty seconds.
//
// Mutation: take the end at the harness's exit, the group and the holders
// unasked: the answer is empty and the stop `end`, the defect this pull
// request removes.
func TestTheEndWaitsForTheHarnesssGroup(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	harnessPid, writerPid := filepath.Join(dir, "harness.pid"), filepath.Join(dir, "writer.pid")
	p := jsonlProvider(t, map[string]string{
		"FAKE_LATE_GRANDCHILD": "1", "FAKE_HARNESS_PIDFILE": harnessPid, "FAKE_PIDFILE": writerPid,
		"FAKE_STDOUT":      `{"type":"step_start"}` + "\n",
		"FAKE_STDOUT_LATE": textEvent("the whole answer") + "\n",
	})
	p.termGrace = 30 * time.Second
	done := make(chan completed, 1)
	go func() { done <- completeOf(ask(t, p, llm.Request{})) }()
	harness, writer := awaitPidFile(t, harnessPid), awaitPidFile(t, writerPid)
	t.Cleanup(func() { reapTree(writer) })
	if !procgrouptest.AwaitGone(t, harness, 30*time.Second) {
		t.Fatalf("the launcher %d never exited", harness)
	}
	if err := syscall.Kill(writer, syscall.SIGUSR1); err != nil {
		t.Fatalf("telling the runtime to write: %v", err)
	}
	got := <-done
	if got.err != nil {
		t.Fatalf("Complete: %v", got.err)
	}
	if got.comp.Content != "the whole answer" {
		t.Errorf("Content = %q, want the answer the runtime wrote after the launcher exited", got.comp.Content)
	}
}

// completed is what a call answered.
type completed struct {
	comp *llm.Completion
	err  error
}

func completeOf(comp *llm.Completion, err error) completed { return completed{comp, err} }

// A HOLDER THAT ESCAPED THE GROUP KEEPS THE ANSWER BACK. A helper started with
// setsid leaves the harness's group with its stdout: the group's end cannot see
// it, and the pin can. The call is refused as a server failure saying its
// stdout was held, and the holder is not killed — procgroup addresses groups,
// and it left the one recorded.
//
// Mutation: drop the pin's question from the end, and the call is answered.
func TestAHolderThatEscapedTheGroupKeepsTheAnswerBack(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "holder.pid")
	p := fakeProvider(t, map[string]string{
		"FAKE_HOLD_STREAM": "stdout", "FAKE_HOLD_ESCAPE": "1", "FAKE_PIDFILE": pidFile, "FAKE_STDOUT": "the answer",
	}, nil)
	p.termGrace = witnessGrace
	holderOf(t, pidFile)

	comp, err := ask(t, p, llm.Request{})
	if err == nil {
		t.Fatalf("a stdout an escaped helper held was read as the answer %q", comp.Content)
	}
	if got := llm.KindOf(err); got != llm.KindServer || !strings.Contains(err.Error(), "held its stdout open") {
		t.Fatalf("err = %v (%v), want a server failure saying the stdout was held", err, got)
	}
	holder := awaitPidFile(t, pidFile)
	if running, err := procgrouptest.Running(holder); err != nil || !running {
		t.Errorf("the escaped holder %d was ended (%v): nothing may signal outside the group recorded", holder, err)
	}
}

// AN IN-GROUP HELPER HOLDING NO STREAM IS WAITED FOR, THEN ENDED. The harness
// answers and exits 0, leaving a helper in its group with nothing of its
// output, deaf to SIGTERM: only the group's question of the end sees it. The
// call answers, nothing marked held, and the helper is gone when it returns.
// The grace is a second, which the case pays once.
//
// Mutation: drop the group's question from the end, and the helper lives on.
func TestAnInGroupHelperHoldingNoStreamIsWaitedForThenEnded(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "helper.pid")
	p := fakeProvider(t, map[string]string{
		"FAKE_IDLE_HELPER": "1", "FAKE_PIDFILE": pidFile, "FAKE_STDOUT": "the answer",
	}, nil)
	p.termGrace = time.Second
	comp, err := ask(t, p, llm.Request{})
	helper := awaitPidFile(t, pidFile)
	t.Cleanup(func() { reapTree(helper) })
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if comp.Content != "the answer" {
		t.Errorf("Content = %q, want the answer", comp.Content)
	}
	if !procgrouptest.AwaitGone(t, helper, 10*time.Second) {
		t.Errorf("the helper %d in the harness's group outlived the call", helper)
	}
}

// ---------------------------------------------------------------------------
// 8.4: the bound.
// ---------------------------------------------------------------------------

// writtenAt reads the size a cut's message says the stream had.
var writtenAt = regexp.MustCompile(`\((\d+) written\)`)

// AN ANSWER PAST ITS BOUND ENDS THE RUN, AT ONCE AND BY KILLING IT. The harness
// writes 33 MiB of one answer and holds, beside a SIGTERM-deaf helper in its
// group; its own SIGTERM handler would write a marker and more. The call is
// refused as a server failure naming stdout and the bound, both processes are
// gone, no marker was written — the kill came before any SIGTERM — and the
// cut was measured at most at 33 MiB.
//
// Mutation: no bound check in the watcher, and the call runs to its timeout;
// cancel the call at the bound without killing the group, and Cancel's
// SIGTERM reaches the handler first.
func TestAnAnswerPastItsBoundEndsTheRun(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	harnessPid, helperPid, marker := filepath.Join(dir, "harness.pid"), filepath.Join(dir, "helper.pid"),
		filepath.Join(dir, "terminated")
	p := fakeProvider(t, map[string]string{
		"FAKE_FLOOD": "stdout", "FAKE_FLOOD_HOLD": "1", "FAKE_HARNESS_PIDFILE": harnessPid,
		"FAKE_PIDFILE": helperPid, "FAKE_TERM_MARKER": marker,
	}, nil)
	p.timeout = 15 * time.Second
	_, err := ask(t, p, llm.Request{})
	harness, helper := awaitPidFile(t, harnessPid), awaitPidFile(t, helperPid)
	t.Cleanup(func() { reapTree(helper) })
	if got := llm.KindOf(err); got != llm.KindServer {
		t.Fatalf("kind = %v, want server (err %v)", got, err)
	}
	if !strings.Contains(err.Error(), "stdout") || !strings.Contains(err.Error(), strconv.Itoa(maxOutput)) {
		t.Errorf("err = %v, want it to name stdout and the bound", err)
	}
	for _, pid := range []int{harness, helper} {
		if !procgrouptest.AwaitGone(t, pid, 10*time.Second) {
			t.Errorf("%d outlived a run ended at its bound", pid)
		}
	}
	if _, statErr := os.Stat(marker); statErr == nil {
		t.Error("the harness was sent SIGTERM before it was killed: a grace lets a runaway writer write on")
	}
	m := writtenAt.FindStringSubmatch(err.Error())
	if m == nil {
		t.Fatalf("err = %v, want the size the cut was measured at", err)
	}
	if size, _ := strconv.Atoi(m[1]); size > 33<<20 {
		t.Errorf("the cut was measured at %d bytes, past the 33 MiB the harness wrote", size)
	}
}

// THE BOUND IS KEPT WHILE A LINE'S CALLBACK IS HELD. A streaming text
// function's first delta blocks in the caller's OnDelta while the harness
// floods its stdout past the bound: the watcher, which waits on no callback,
// ends the harness all the same, and once the callback lets go the call is
// the bound's refusal.
//
// Mutation: check the bound in the reader, between callbacks: nothing ends
// the harness while the callback is held, and the call ends by its timeout.
func TestTheBoundIsKeptWhileALineCallbackIsHeld(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	harnessPid, helperPid := filepath.Join(dir, "harness.pid"), filepath.Join(dir, "helper.pid")
	p := jsonlProvider(t, map[string]string{
		"FAKE_FLOOD": "stdout", "FAKE_FLOOD_HOLD": "1", "FAKE_FLOOD_LINE": textEvent(strings.Repeat("x", 1000)),
		"FAKE_HARNESS_PIDFILE": harnessPid, "FAKE_PIDFILE": helperPid,
		"FAKE_TERM_MARKER": filepath.Join(dir, "terminated"),
	})
	p.timeout = 15 * time.Second
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	req := llm.Request{
		Messages: []llm.Message{{Role: llm.RoleUser, Content: "hi"}},
		OnDelta:  func(llm.Delta) { once.Do(func() { close(entered); <-release }) },
	}
	done := make(chan completed, 1)
	go func() { done <- completeOf(p.Complete(t.Context(), req)) }()
	<-entered
	harness, helper := awaitPidFile(t, harnessPid), awaitPidFile(t, helperPid)
	t.Cleanup(func() { reapTree(helper) })
	gone := procgrouptest.AwaitGone(t, harness, 30*time.Second)
	close(release)
	got := <-done
	if !gone {
		t.Fatal("the harness lived on past its bound while a line's callback was held")
	}
	if kind := llm.KindOf(got.err); kind != llm.KindServer || !strings.Contains(got.err.Error(), "bound") {
		t.Errorf("err = %v (%v), want the bound's server refusal", got.err, kind)
	}
}

// A PROMPT HELD UNREAD COSTS ONE GRACE, NOT TWO. A helper in the harness's
// group holds its stdin with more of the prompt unread than a pipe buffers:
// os/exec cuts the prompt's copy a grace after the harness exits, and only
// then does Wait return. The end's deadline is that very instant, so the
// helper is ended there, rather than a grace after Wait came back.
//
// Mutation: count the end's grace from Wait's return, and its deadline is a
// grace past the cut.
func TestAPromptHeldUnreadCostsOneGrace(t *testing.T) {
	t.Parallel()
	pidFile := filepath.Join(t.TempDir(), "holder.pid")
	in := fakeInvocation(t, map[string]string{
		"FAKE_HOLD_STREAM": "stdin", "FAKE_PIDFILE": pidFile, "FAKE_STDOUT": "the answer",
	})
	in.stdin = strings.Repeat("a prompt longer than a pipe holds ", 1<<15)
	holderOf(t, pidFile)
	var cut, deadline atomic.Pointer[time.Time]
	in.hooks = &captureHooks{
		promptCut: func(at time.Time) { cut.Store(&at) },
		deadline: func(at time.Time) <-chan time.Time {
			deadline.Store(&at)
			due := make(chan time.Time)
			close(due)
			return due
		},
	}
	res, err := run(t.Context(), in)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !errors.Is(res.waitErr, exec.ErrWaitDelay) {
		t.Fatalf("waitErr = %v, want os/exec's cut of the prompt held unread", res.waitErr)
	}
	at, by := cut.Load(), deadline.Load()
	if at == nil || by == nil {
		t.Fatalf("the prompt's cut (%v) or the end's deadline (%v) was never seen", at, by)
	}
	if !by.Equal(*at) {
		t.Errorf("the end's deadline was %v, want the instant the prompt was cut, %v: the grace was "+
			"spent in the wait", *by, *at)
	}
}

// A RECORD PAST THE BOUND IS READ WHOLE AND GIVEN BACK. A run whose record is
// its stdout, 60 MiB of an event no rule reads and then its answer: every line
// reaches the record's reader, past the bound too, as a pipe's did; the first
// 32 MiB are kept for the end; and what was read past them is given back, so
// the capture's blocks never take more than 64 MiB — the kept head and the
// room ahead — and a tick's slack.
//
// Mutation: give nothing back, and the blocks grow with the record; read the
// record as one answer, and the run is ended at 32 MiB.
func TestARecordPastTheBoundIsReadWholeAndGivenBack(t *testing.T) {
	t.Parallel()
	noise := `{"type":"noise","said":"` + strings.Repeat("n", 100) + `"}`
	in := fakeInvocation(t, map[string]string{
		"FAKE_FLOOD": "stdout", "FAKE_FLOOD_MIB": "60", "FAKE_FLOOD_PACE_MS": "20", "FAKE_FLOOD_LINE": noise,
		"FAKE_STDOUT": textEvent("the answer") + "\n",
	})
	var lines atomic.Int64
	var most atomic.Int64
	in.readToEnd, in.lineStream = true, true
	in.onLine = func(string) { lines.Add(1) }
	in.hooks = &captureHooks{watched: func(stream string, _, allocated int64) {
		if stream == "stdout" && allocated > most.Load() {
			most.Store(allocated)
		}
	}}
	res, err := run(t.Context(), in)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.unfinished() != "" || res.exitCode != 0 {
		t.Fatalf("the run did not finish as one: %s (exit %d)", res.unfinished(), res.exitCode)
	}
	perChunk := int64((1 << 20) / (len(noise) + 1))
	if want := 60*perChunk + 1; lines.Load() != want {
		t.Errorf("the record's reader was handed %d lines, want every one of the %d", lines.Load(), want)
	}
	if len(res.stdout) != maxOutput {
		t.Errorf("the end kept %d bytes of the record, want its first %d", len(res.stdout), maxOutput)
	}
	if limit := int64(2*maxOutput + 6<<20); most.Load() > limit {
		t.Errorf("the capture's blocks took %d bytes, past the kept head and the room ahead (%d)", most.Load(), limit)
	}
	if most.Load() == 0 {
		t.Error("the watcher never measured the capture, so the case measured nothing")
	}
}

// A STDERR PAST THE BOUND LEAVES THE ANSWER STANDING. 40 MiB on stderr costs
// only diagnosis: the answer stands, and the first 32 MiB of stderr are kept;
// and a failure marker written after 33 MiB still stops the run, since every
// line is delivered to the markers, past what is kept.
//
// Mutation: read stderr as one answer, and the run is refused at 32 MiB; read
// the markers only from the kept bytes, and the late marker goes unseen.
func TestAStderrPastTheBoundLeavesTheAnswerStanding(t *testing.T) {
	t.Parallel()
	t.Run("40 MiB of it", func(t *testing.T) {
		t.Parallel()
		flood := map[string]string{"FAKE_FLOOD": "stderr", "FAKE_FLOOD_MIB": "40", "FAKE_FLOOD_PACE_MS": "20",
			"FAKE_STDOUT": "the answer"}
		comp, err := ask(t, fakeProvider(t, flood, nil), llm.Request{})
		if err != nil || comp.Content != "the answer" {
			t.Fatalf("Complete = %v, %v; want the answer standing", comp, err)
		}
		res, err := run(t.Context(), fakeInvocation(t, flood))
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if res.cut != nil || len(res.stderr) != maxOutput || res.stdout != "the answer" {
			t.Errorf("cut %v, %d bytes of stderr kept, stdout %q; want no cut, its first %d, the answer",
				res.cut, len(res.stderr), res.stdout, maxOutput)
		}
	})
	t.Run("a marker after 33 MiB", func(t *testing.T) {
		t.Parallel()
		in := fakeInvocation(t, map[string]string{
			"FAKE_FLOOD": "stderr", "FAKE_FLOOD_PACE_MS": "20", "FAKE_FLOOD_AFTER": "CONFINEMENT_FAILED: no policy\n",
			"FAKE_STDOUT": "the answer",
		})
		in.failOn = []string{"CONFINEMENT_FAILED"}
		res, err := run(t.Context(), in)
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if !strings.Contains(res.failureMarker, "CONFINEMENT_FAILED") {
			t.Errorf("a marker written past the kept bytes went unseen (marker %q)", res.failureMarker)
		}
	})
}

// A STREAM THE ENGINE FELL BEHIND ON ENDS THE RUN. The record's reader is held
// in its callback by the first line — as the host's charge of a request can
// hold it — while the harness writes 40 MiB more and holds: the capture runs
// more than 32 MiB ahead of the reading, and the watcher ends the run,
// naming stdout and why.
//
// Mutation: no check of how far ahead the writer is: nothing ends the harness.
func TestAStreamTheEngineFellBehindOnEndsTheRun(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	harnessPid, helperPid := filepath.Join(dir, "harness.pid"), filepath.Join(dir, "helper.pid")
	in := fakeInvocation(t, map[string]string{
		"FAKE_FLOOD": "stdout", "FAKE_FLOOD_MIB": "40", "FAKE_FLOOD_HOLD": "1",
		"FAKE_FLOOD_FIRST": `{"type":"step_finish"}`, "FAKE_HARNESS_PIDFILE": harnessPid, "FAKE_PIDFILE": helperPid,
		"FAKE_TERM_MARKER": filepath.Join(dir, "terminated"),
	})
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	in.readToEnd, in.lineStream = true, true
	in.onLine = func(string) { once.Do(func() { close(entered); <-release }) }
	type ran struct {
		res *rawResult
		err error
	}
	done := make(chan ran, 1)
	go func() {
		res, err := run(t.Context(), in)
		done <- ran{res, err}
	}()
	<-entered
	harness, helper := awaitPidFile(t, harnessPid), awaitPidFile(t, helperPid)
	t.Cleanup(func() { reapTree(helper) })
	gone := procgrouptest.AwaitGone(t, harness, 30*time.Second)
	close(release)
	got := <-done
	if !gone {
		t.Fatal("the harness lived on 40 MiB ahead of the engine's reading")
	}
	if got.err != nil {
		t.Fatalf("run: %v", got.err)
	}
	if got.res.cut == nil || got.res.cut.why != cutBehind || got.res.cut.stream != "stdout" {
		t.Errorf("cut = %+v, want stdout's, behind", got.res.cut)
	}
}

// A CHILD WHOSE ROOM CANNOT BE RESERVED IS NOT STARTED. A file into which a
// write may fail for room is a file into which a Node harness may lose the
// tail of its answer silently, so a volume that will not reserve maxOutput
// keeps the harness from starting: the call is refused as fatal, naming
// state_dir and the room it needed, and the fake never ran.
//
// Mutation: ignore the reservation's error, and the fake runs.
func TestAChildWhoseRoomCannotBeReservedIsNotStarted(t *testing.T) {
	t.Parallel()
	argv := t.TempDir()
	p := fakeProvider(t, map[string]string{"FAKE_RECORD_ARGV": argv, "FAKE_STDOUT": "the answer"}, nil)
	p.hooks = &captureHooks{reserve: func(*os.File, int64, int64) error { return syscall.ENOSPC }}
	_, err := ask(t, p, llm.Request{})
	if got := llm.KindOf(err); got != llm.KindFatal {
		t.Fatalf("kind = %v, want fatal (err %v)", got, err)
	}
	for _, want := range []string{"providers.llm.sub.state_dir", strconv.Itoa(maxOutput), "room"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to name %q", err, want)
		}
	}
	if entries, _ := os.ReadDir(argv); len(entries) != 0 {
		t.Errorf("the harness ran (%d invocations recorded) without the room its output needs", len(entries))
	}
}

// A ROOM REFUSED MID-RUN ENDS IT. A stream read to its end is given room
// ahead of its reader as it goes; a volume that refuses it once stderr has
// passed what it keeps ends the run as a server failure naming the volume and
// state_dir.
//
// Mutation: ignore the refusal of the room ahead, and the run goes on to its
// timeout.
func TestARoomRefusedMidRunEndsIt(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	helperPid := filepath.Join(dir, "helper.pid")
	p := fakeProvider(t, map[string]string{
		"FAKE_FLOOD": "stderr", "FAKE_FLOOD_MIB": "40", "FAKE_FLOOD_PACE_MS": "20", "FAKE_FLOOD_HOLD": "1",
		"FAKE_PIDFILE":     helperPid,
		"FAKE_TERM_MARKER": filepath.Join(dir, "terminated"),
	}, nil)
	p.timeout = 15 * time.Second
	p.hooks = &captureHooks{reserve: func(f *os.File, off, n int64) error {
		if info, err := f.Stat(); err == nil && off > 0 && info.Size() > maxOutput {
			return syscall.ENOSPC
		}
		return reserveRoom(f, off, n)
	}}
	_, err := ask(t, p, llm.Request{})
	t.Cleanup(func() { reapTree(awaitPidFile(t, helperPid)) })
	if got := llm.KindOf(err); got != llm.KindServer {
		t.Fatalf("kind = %v, want server (err %v)", got, err)
	}
	for _, want := range []string{"stderr", "room", "state_dir", p.ws.Root()} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to name %q", err, want)
		}
	}
}

// ---------------------------------------------------------------------------
// 8.5: what an escaped writer can do after the end.
// ---------------------------------------------------------------------------

// AN ESCAPED WRITER CANNOT GROW ITS CAPTURE AFTER THE END. A helper that left
// the harness's group with its stdout writes on after the call is over, a
// mebibyte on each of the case's SIGUSR1s. The capture went to the provider's
// janitor at the end, and every tick of it — the case's own — empties it: after
// each step and tick the file's blocks are at most the one step. When the
// holder exits, the janitor lets go and says so, with what it discarded.
//
// Sequential: the log is the process's.
//
// Mutation: no janitor — the end closes a capture still held — and nothing
// empties it.
func TestAnEscapedWriterCannotGrowItsCaptureAfterTheEnd(t *testing.T) {
	logs := captureLogs(t)
	pidFile, acks := filepath.Join(t.TempDir(), "holder.pid"), t.TempDir()
	p := fakeProvider(t, map[string]string{
		"FAKE_HOLD_STREAM": "stdout", "FAKE_HOLD_ESCAPE": "1", "FAKE_PIDFILE": pidFile,
		"FAKE_HOLD_WRITES": acks, "FAKE_STDOUT": "the answer",
	}, nil)
	p.termGrace = witnessGrace
	ticks, asked, observed := make(chan time.Time), make(chan struct{}, 1), make(chan int64)
	p.janitor.tick = func(time.Duration) (<-chan time.Time, func()) {
		select {
		case asked <- struct{}{}:
		default:
		}
		return ticks, func() {}
	}
	p.janitor.observe = func(_ string, allocated int64) { observed <- allocated }
	holderOf(t, pidFile)

	if _, err := ask(t, p, llm.Request{}); llm.KindOf(err) != llm.KindServer {
		t.Fatalf("err = %v, want the held stdout refused", err)
	}
	holder := awaitPidFile(t, pidFile)
	select {
	case <-asked:
	case <-time.After(30 * time.Second): //fixedwait:liveness
		t.Fatal("no janitor took the capture an escaped writer still held")
	}
	tick := func() int64 {
		ticks <- time.Now()
		return <-observed
	}
	const step = 1 << 20
	slack := int64(64 << 10)
	if left := tick(); left > slack {
		t.Fatalf("the janitor's first tick left %d bytes of the capture's blocks", left)
	}
	for n := 1; n <= 33; n++ {
		if err := syscall.Kill(holder, syscall.SIGUSR1); err != nil {
			t.Fatalf("step %d: %v", n, err)
		}
		ack := filepath.Join(acks, fmt.Sprintf("ack-%d", n))
		awaitFile(t, ack)
		raw, err := os.ReadFile(ack)
		if err != nil {
			t.Fatal(err)
		}
		if held, _ := strconv.ParseInt(string(raw), 10, 64); held > step+slack {
			t.Fatalf("after step %d the capture's blocks took %d bytes, more than the one step", n, held)
		}
		tick()
	}
	if err := syscall.Kill(holder, syscall.SIGKILL); err != nil {
		t.Fatal(err)
	}
	if !procgrouptest.AwaitGone(t, holder, 10*time.Second) {
		t.Fatalf("the holder %d outlived its SIGKILL", holder)
	}
	tick()
	line := logs.String()
	m := regexp.MustCompile(`harness_capture_released.*discarded_bytes=(\d+)`).FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("the janitor did not say it let go of the capture: %s", line)
	}
	if discarded, _ := strconv.Atoi(m[1]); discarded < 33*step {
		t.Errorf("the janitor discarded %d bytes, want the 33 MiB the holder wrote", discarded)
	}
}

// ---------------------------------------------------------------------------
// 8.6: the final line.
// ---------------------------------------------------------------------------

// A LINE STREAM CUT INSIDE AN EVENT IS REFUSED, on both paths: a text
// function's JSONL stdout, and a run with tools' record, each ending in bytes
// that begin an event and do not decode. Nothing read of it can be shown to be
// whole: a server failure saying so.
//
// Mutation: drop the cut line from unfinished's reasons, and the text
// function answers with what came before it.
func TestALineStreamCutInsideAnEventIsRefused(t *testing.T) {
	t.Parallel()
	t.Run("a text function", func(t *testing.T) {
		t.Parallel()
		p := jsonlProvider(t, map[string]string{"FAKE_STDOUT": textEvent("whole") + "\n" + `{"type":"text","part":{"text":"cu`})
		_, err := ask(t, p, llm.Request{})
		if llm.KindOf(err) != llm.KindServer || err == nil || !strings.Contains(err.Error(), "ended inside a line") {
			t.Fatalf("err = %v, want a server failure saying its stdout ended inside a line", err)
		}
	})
	t.Run("a run with tools", func(t *testing.T) {
		t.Parallel()
		p := fakeAs(t, "opencode", fakeScript{Responses: []fakeResponse{{Text: "the answer"}}}, func(c *Config) {
			c.Env["FAKE_RECORD_TAIL"] = `{"type":"text","part":{"messageID":"msg_9","type":"text","text":"cu`
		})
		_, err := p.Complete(t.Context(), phase(newTestHost(t, "search")))
		if llm.KindOf(err) != llm.KindServer || err == nil || !strings.Contains(err.Error(), "ended inside a line") {
			t.Fatalf("err = %v, want a server failure saying its stdout ended inside a line", err)
		}
	})
}

// A WHOLE LAST EVENT WITHOUT ITS LINE BREAK IS READ, on both paths: the text
// function answers it, and the run with tools' record reader is handed it as
// the line it is, so the run's last request carries the answer.
//
// Mutation: never hand on the unterminated last line, and the run with tools
// answers without it.
func TestAWholeLastEventWithoutItsLineBreakIsRead(t *testing.T) {
	t.Parallel()
	t.Run("a text function", func(t *testing.T) {
		t.Parallel()
		comp, err := ask(t, jsonlProvider(t, map[string]string{"FAKE_STDOUT": textEvent("the answer")}), llm.Request{})
		if err != nil || comp.Content != "the answer" {
			t.Fatalf("Complete = %v, %v; want the answer", comp, err)
		}
	})
	t.Run("a run with tools", func(t *testing.T) {
		t.Parallel()
		script := fakeScript{Responses: []fakeResponse{
			{Text: "Looking it up.", Calls: []fakeCall{{Name: "search"}}, Usage: fakeUsage{Input: 5, Output: 5}},
			{Text: "the answer", Usage: fakeUsage{Input: 5, Output: 5}},
		}}
		p := fakeAs(t, "opencode", script, func(c *Config) { c.Env["FAKE_RECORD_UNTERMINATED"] = "1" })
		host := newTestHost(t, "search")
		host.handle("search", func(context.Context, map[string]any) (string, bool) { return "3 results", false })
		comp, err := p.Complete(t.Context(), phase(host))
		if err != nil || comp.Content != "the answer" {
			t.Fatalf("Complete = %v, %v; want the answer", comp, err)
		}
		turns, _, _, _ := host.snapshot()
		if turns = lastByID(turns); len(turns) != 2 || turns[1].Text != "the answer" {
			t.Errorf("the host was told %+v, want the last request carrying the answer", turns)
		}
	})
}

// A TEXT ANSWER WITHOUT A LINE BREAK IS WHOLE, even one that begins with a
// brace: a text answer is the answer itself, and the line rule is a JSONL
// stream's alone.
//
// Mutation: apply the line rule to every output mode, and the answer is
// refused as a stream cut inside an event.
func TestATextAnswerWithoutALineBreakIsWhole(t *testing.T) {
	t.Parallel()
	const answer = "{a set, as the model wrote it"
	comp, err := ask(t, fakeProvider(t, map[string]string{"FAKE_STDOUT": answer}, nil), llm.Request{})
	if err != nil || comp.Content != answer {
		t.Fatalf("Complete = %v, %v; want %q", comp, err, answer)
	}
}
