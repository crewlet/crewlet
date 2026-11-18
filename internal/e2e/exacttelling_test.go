package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/agent/builtin"
	"github.com/crewlet/crewlet/internal/api/opkey"
	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A CUT-OFF SESSION'S RELAUNCH, TOLD BY A READING OF WHAT IT HOLDS, END TO END.
//
// The seat's job runs in [codingModeCutAsk]: it makes [codingCutCalls] calls
// over its bridge, files only the first [codingCutKept] in its session as a
// harness killed mid-stream does, asks a person, and exits failing. Its
// collection parks it on the question and keeps its session marked cut off;
// the person's answer relaunches the turn, which continues that kept session
// (`--resume`). What the brief tells of the job's calls is decided across every
// component a unit test stubs: the stand-in's records, the session's capture
// and archive in the object store, the engine's reading of that archive, the
// runner's telling by it, and the launch's choice of brief by the release the
// relaunch's box answers.

// exactModel is the model the coder entry of a company here names: one whose
// window the engine knows (internal/sandbox's claudeWindows), so the
// relaunch's first request can be judged to send the session whole, which a
// telling by a reading needs. [codingModel] is unknown, so no other case's
// verdict moves.
const exactModel = "claude-opus-5-5"

// The releases a case here has its boxes answer: the job's, the release the
// measured reading reads, and another one the same reader reads.
const (
	exactJobRelease   = "2.1.310"
	exactOtherRelease = "2.1.322"
)

// Words of the two headers a cut-off session's calls after its save point are
// told under (internal/agent/prompts): the one a list a reading thinned
// heads, and the one heading every call since the point.
const (
	exactReadHeaderSays  = "less any it found the session holds"
	exactSinceHeaderSays = "they are every call the run working in it made"
	exactCallsHeading    = "## Calls this run made that the session above may not show"
)

// cutOffRelaunch is what a case here reads off one cut-off job's relaunch: the
// brief it was handed, the run that parked, and the log lines of the turn's
// trace.
type cutOffRelaunch struct {
	n      *codingNode
	parked sandbox.PendingRun
	brief  string
	lines  []map[string]any
}

// relaunchCutOff runs one cut-off job ([codingModeCutAsk]) on a fresh node,
// every box answering [exactJobRelease] until the job parks and relaunch
// after it, answers its question, and waits for the turn to settle.
func relaunchCutOff(t *testing.T, relaunch string) cutOffRelaunch {
	t.Helper()
	stateDir := t.TempDir()
	argv := filepath.Join(stateDir, "argv.txt")
	env := map[string]string{codingModeEnv: codingModeCutAsk, codingArgvEnv: argv}
	model := newScriptedModel(t)
	setRelease := func(release string) {
		t.Helper()
		if err := recordWhole(codingVersionFile(model.shim), []byte(release+" (Claude Code)\n")); err != nil {
			t.Fatalf("writing the release the boxes answer: %v", err)
		}
	}
	setRelease(exactJobRelease)
	doc := fmt.Sprintf(sandboxCompanyDoc, codingSandbox(stateDir, model.shim), model.entry("scripted"),
		codingEntryOn("coder", model.stateDir, exactModel))
	n := &codingNode{node: bootCompany(t, doc, model, env), stateDir: stateDir, argvPath: argv}
	// A CAPTURE KEEPS A SESSION ONLY UNDER THE FLEET'S ACTIVE REVISION.
	activateTheRunningCompany(t, n.node)
	waitFor(t, "the seat to be claimed", func() bool {
		return slices.Contains(n.engine.Node().Host().Held(), "swe")
	})
	// THE TURN'S OWN TRACE, watched before the wake: the relaunch is the
	// resumed turn, which runs under the trace its run's row names, so every
	// line of the reading and the launch carries it.
	hex := strings.ReplaceAll(uuid.NewString(), "-", "")
	trace := events.TraceContext{TraceID: hex, SpanID: hex[:16]}
	logs.watch(t, trace.TraceID)
	n.publishWake(t, "swe", "the api test is flaking, please fix it", codingConversation, trace)

	waitFor(t, "the cut-off job to park on its question", func() bool {
		for _, run := range n.activeRuns(t) {
			if run.Status == sandbox.StatusAwaiting && run.Question != "" {
				return true
			}
		}
		return false
	}, n.diagnose)
	parked := n.runFor(t, sandbox.StatusAwaiting)
	setRelease(relaunch)
	status, raw := bearer(t, n.node, http.MethodPost, "/operator/act/"+builtin.AnswerExecutorTool,
		map[string]any{"args": map[string]any{"turn_id": parked.TurnID,
			"launch_id": parked.LaunchID, "choice": "main"}},
		map[string]string{opkey.Header: uuid.Must(uuid.NewV7()).String()})
	if status != http.StatusOK {
		t.Fatalf("POST /operator/act/%s answered %d: %s", builtin.AnswerExecutorTool, status, raw)
	}
	waitFor(t, "the relaunched turn to settle", func() bool {
		return len(n.activeRuns(t)) == 0
	}, n.diagnose)

	// THE CUT-OFF SESSION CONTINUED, never a fresh run told the question.
	args := n.agentArgv(t)
	if i := slices.Index(args, "--resume"); parked.SessionID == "" || i < 0 || i+1 >= len(args) ||
		args[i+1] != parked.SessionID {
		t.Fatalf("the relaunch did not continue the session the job parked in (%q): argv = %v",
			parked.SessionID, args)
	}
	out := cutOffRelaunch{n: n, parked: parked, brief: n.agentBrief(t)}
	for _, line := range logs.linesFor(trace.TraceID) {
		var fields map[string]any
		if json.Unmarshal([]byte(line), &fields) == nil {
			out.lines = append(out.lines, fields)
		}
	}
	return out
}

// logged is every line of the turn's trace logged as msg.
func (r cutOffRelaunch) logged(msg string) []map[string]any {
	var out []map[string]any
	for _, fields := range r.lines {
		if fields["msg"] == msg {
			out = append(out, fields)
		}
	}
	return out
}

// calls is the section of the relaunch's brief telling the calls after the
// session's save point, from its heading to the next, failing where the brief
// has none.
func (r cutOffRelaunch) calls(t *testing.T) string {
	t.Helper()
	_, section, ok := strings.Cut(r.brief, exactCallsHeading)
	if !ok {
		t.Fatalf("the relaunch's brief tells no calls after the session's save point:\n%s", r.brief)
	}
	section, _, _ = strings.Cut(section, "\n## ")
	return section
}

// read is the turn's one `executor_session_holdings_read` line, failing where
// it logged none or several.
func (r cutOffRelaunch) read(t *testing.T) map[string]any {
	t.Helper()
	read := r.logged("executor_session_holdings_read")
	if len(read) != 1 {
		t.Fatalf("the turn logged %d executor_session_holdings_read lines, want one: %v; "+
			"its unread lines: %v", len(read), read, r.logged("executor_session_holdings_unread"))
	}
	return read[0]
}

// countsAre fails unless line says asked, held and told as given, and the
// session's writer as exactJobRelease.
func countsAre(t *testing.T, line map[string]any, asked, held, told int) {
	t.Helper()
	for key, want := range map[string]int{"asked": asked, "held": held, "told": told} {
		if got, _ := line[key].(float64); got != float64(want) {
			t.Errorf("the reading's line says %s=%v, want %d: %v", key, line[key], want, line)
		}
	}
	if line["writer"] != exactJobRelease {
		t.Errorf("the reading's line says writer=%v, want the job's %s: %v", line["writer"], exactJobRelease, line)
	}
}

// A CUT-OFF SESSION'S RELAUNCH IS TOLD ONLY THE CALLS ITS SESSION LACKS.
//
// The job made four calls and its question; its session holds the first two,
// each call and result. The relaunch, in a box of the release that wrote the
// session, is told the last two calls and the question under the header a
// reading's list takes, and neither of the first two; the reading's line says
// asked five, held two, told three, for the job's release.
//
// Mutations: the stand-in's replies filed without their message's id, and
// the reading holds none, so the relaunch is told every call; every record
// filed as written by 2.1.305, under the reading's writer floor, or filed
// naming no release, and there is no reading.
func TestACutOffSessionsRelaunchIsToldOnlyTheCallsItsSessionLacks(t *testing.T) {
	t.Parallel()
	r := relaunchCutOff(t, exactJobRelease)
	calls := r.calls(t)
	if !strings.Contains(calls, exactReadHeaderSays) {
		t.Errorf("the relaunch's calls are not told under the header a reading's list takes:\n%s", calls)
	}
	for i := codingCutKept + 1; i <= codingCutCalls; i++ {
		if !strings.Contains(calls, codingCutQuery(i)) {
			t.Errorf("the relaunch is not told call %d, which its session lacks:\n%s", i, calls)
		}
	}
	if !strings.Contains(calls, "- "+builtin.AskAndWaitTool+"(") {
		t.Errorf("the relaunch is not told its question's call, which its session lacks:\n%s", calls)
	}
	for i := 1; i <= codingCutKept; i++ {
		if strings.Contains(r.brief, codingCutQuery(i)) {
			t.Errorf("the relaunch is told call %d, which its session holds:\n%s", i, r.brief)
		}
	}
	countsAre(t, r.read(t), codingCutCalls+1, codingCutKept, codingCutCalls+1-codingCutKept)
	if unused := r.logged("executor_session_holdings_unused"); len(unused) != 0 {
		t.Errorf("the brief told by the reading went unused: %v", unused)
	}
}

// A RELAUNCH IN A BOX OF ANOTHER RELEASE IS TOLD EVERY CALL AFTER THE SAVE
// POINT.
//
// The same job, its session read as holding its first two calls; but the box
// answers another release, whose resume may hand its model otherwise, so the
// launch hands the brief told by the save point: every call and the question
// under the header saying so, and the line saying why, naming both releases.
//
// Mutations: the launch hands the brief told by the reading whatever release
// the box answers, and the first two calls go untold here; the stand-in's
// replies filed without their message's id, and the reading holds none, so no
// brief is told by it and no line says why one went unused.
func TestARelaunchInABoxOfAnotherReleaseIsToldEveryCallAfterTheSavePoint(t *testing.T) {
	t.Parallel()
	r := relaunchCutOff(t, exactOtherRelease)
	calls := r.calls(t)
	if !strings.Contains(calls, exactSinceHeaderSays) {
		t.Errorf("the relaunch's calls are not told under the header saying they are every call "+
			"since the save point:\n%s", calls)
	}
	for i := 1; i <= codingCutCalls; i++ {
		if !strings.Contains(calls, codingCutQuery(i)) {
			t.Errorf("the relaunch is not told call %d:\n%s", i, calls)
		}
	}
	if !strings.Contains(calls, "- "+builtin.AskAndWaitTool+"(") {
		t.Errorf("the relaunch is not told its question's call:\n%s", calls)
	}
	countsAre(t, r.read(t), codingCutCalls+1, codingCutKept, codingCutCalls+1-codingCutKept)
	unused := r.logged("executor_session_holdings_unused")
	if len(unused) != 1 {
		t.Fatalf("the turn logged %d executor_session_holdings_unused lines, want one: %v", len(unused), unused)
	}
	detail, _ := unused[0]["detail"].(string)
	if !strings.Contains(detail, `"`+exactOtherRelease+`"`) || !strings.Contains(detail, `"`+exactJobRelease+`"`) {
		t.Errorf("the line saying why the reading's brief went unused names %q, want both releases, %s and %s",
			detail, exactOtherRelease, exactJobRelease)
	}
}
