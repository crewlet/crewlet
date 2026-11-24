package executor_test

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/shellword"
)

// AN OPENCODE SESSION IS PACKED ON DEMAND BY ITS OWN STANDALONE EXPORT, AND KEPT
// WITHOUT A SECRET THE ENGINE PUT IN ITS BOX. A job that never packed leaves no
// server the engine can reach (only its script held the address and password),
// so the pack asks the box's OpenCode itself (`--standalone`) for the recorded
// session: one it does not hold is nothing to pack, one it holds is exported,
// gzipped and its digest kept in the box. Scrubbed, sealed, stored and opened
// as a later launch opens it, the export is one JSON document of that session
// holding no value the engine named (in text or as a key), no credential its
// shape gives away, and no value under a password's key.
//
// Mutation: export through the run's server, export a session the box does not
// hold, skip the document's rewrite, or write a key as it came: a check is red.
func TestAnOpenCodeSessionIsPackedOnDemandAndKeptScrubbed(t *testing.T) {
	t.Parallel()
	runner, b := openCodeBox(t, t.TempDir())
	dir := t.TempDir()
	writeBoxStandIn(t, b, paths(b).BinDir()+"/opencode",
		strings.Replace(fakeOpenCode, `d="$FAKE_DIR"`, "d="+shellword.Quote(dir), 1))
	const session = "ses_0123456789abcdef0123456789"
	if err := b.WriteFile(t.Context(), paths(b).SessionRecord(),
		[]byte("id "+session+"\nrelease 2.3.2\n")); err != nil {
		t.Fatal(err)
	}

	described, body, _, err := runner.PackCapture(t.Context(), b, false)
	if err != nil || body != nil {
		t.Fatalf("PackCapture of a session the box does not hold = %+v, %v, %v; want nothing",
			described, body, err)
	}

	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sessions", session), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	messages := `[{"id":"msg_1","type":"user","text":"the token is ` + plantedSecret + ` and ` + plantedShape +
		`"},{"id":"msg_2","type":"tool","input":{"password":"` + plantedKeyed + `"},"output":{"` +
		plantedSecret + `":"named by it"}}]`
	if err := os.WriteFile(filepath.Join(dir, "export-messages"), []byte(messages), 0o600); err != nil {
		t.Fatal(err)
	}
	taken, err := runner.TakeSession(t.Context(), b, sandbox.SessionTake{Spool: testSpool(t), Pack: true,
		Scrub: []string{plantedSecret}, MaxBytes: takeBound})
	if err != nil || taken.Body == nil {
		t.Fatalf("TakeSession = %+v, %v", taken, err)
	}
	if taken.Archive.Harness != executor.OpenCodeName || taken.Archive.SessionID != session {
		t.Errorf("the take describes %+v, want OpenCode's session %s", taken.Archive, session)
	}
	opened := keptAndOpened(t, taken)
	if err := taken.Body.Close(); err != nil {
		t.Fatalf("closing the taken session: %v", err)
	}
	export := gunzipped(t, opened)
	if !json.Valid(export) || !strings.Contains(string(export), `"id":"`+session+`"`) {
		t.Fatalf("the export was kept as %q, want one JSON document of the session", export)
	}
	for _, secret := range []string{plantedSecret, plantedShape, plantedKeyed} {
		if strings.Contains(string(export), secret) {
			t.Errorf("the kept export still holds %q:\n%s", secret, export)
		}
	}
	if calls := readIn(t, dir, "calls"); !strings.Contains(calls, "session export --standalone "+session) {
		t.Errorf("the pack did not export the session standalone; the harness was asked:\n%s", calls)
	}
	if err := taken.Keep(t.Context()); err != nil {
		t.Fatalf("Keep: %v", err)
	}
	if digests := boxFile(t, b, paths(b).SessionDigests()); !strings.Contains(digests,
		session+" "+digestOf(t, opened)+"\n") {
		t.Errorf("the box keeps no digest of the export that left it: %q", digests)
	}
}

// AN ENDED OPENCODE JOB'S SESSION IS TAKEN AS ITS SCRIPT EXPORTED IT. A job the
// engine's read ends while it works (internal/sandbox's endJob) has its script
// interrupt the session through the run's own server, export it through that
// server while it still serves, and only then stop it. The take that follows
// ([sandbox.SessionTake.Pack] false) reads that export and packs nothing again,
// so no `--standalone` runtime meets the database the run's server held, which
// it could find busy and refuse the pack.
//
// Mutation: have a take of a job that is over pack the session anew, and the
// harness is asked for a runtime of its own; export the ended run's session
// with `--standalone`, or after its server is stopped, and the export is
// recorded so.
func TestAnEndedOpenCodeJobsSessionIsTakenAsItsScriptExportedIt(t *testing.T) {
	t.Parallel()
	runner, b := openCodeBox(t, t.TempDir())
	dir := t.TempDir()
	// A CLIENT STILL WORKING AS ITS JOB IS ENDED: it works until its session
	// is interrupted, and exits 7 however it is ended.
	writeBoxStandIn(t, b, paths(b).BinDir()+"/opencode", strings.Replace(fakeOpenCode, `d="$FAKE_DIR"`,
		"d="+shellword.Quote(dir)+" FAKE_RUN_UNTIL=interrupted FAKE_RUN_EXIT=7 FAKE_RUN_TERM_EXIT=7", 1))
	handle, err := runner.Start(t.Context(), b, sandbox.RunRequest{Brief: "fix the flake"})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitForFile(t, filepath.Join(dir, "refreshed"), "the run's client to start working")
	if state, err := runner.Poll(t.Context(), b, handle); err != nil || state != sandbox.JobRunning {
		t.Fatalf("the premise: the job is %q (%v) as it is ended, want it still working", state, err)
	}

	// THE READ THAT ENDS IT, as the coordinator makes one: the job ended first,
	// its result read, and its session taken as that of a job that is over
	// ([sandbox.SessionTake.Pack] false).
	askEnd(t, runner, b, handle)
	awaitOver(t, runner, b, handle, promptEnd, "the end gives its client five seconds")
	res, err := runner.Collect(t.Context(), b, handle, queue.NewAnswerBudget(nil))
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if res.JobEnd != sandbox.JobEndAsked || res.Captured == nil {
		t.Fatalf("the ended job collected as ended %q with capture %+v; want it ended on request, its "+
			"session exported:\n%s", res.JobEnd, res.Captured, boxFile(t, b, paths(b).Err()))
	}
	taken, err := runner.TakeSession(t.Context(), b, sandbox.SessionTake{Spool: testSpool(t), MaxBytes: takeBound})
	if err != nil || taken.Body == nil {
		t.Fatalf("TakeSession of the ended job = %+v, %v", taken, err)
	}
	export := gunzipped(t, keptAndOpened(t, taken))
	if err := taken.Body.Close(); err != nil {
		t.Fatalf("closing the taken session: %v", err)
	}
	if !json.Valid(export) || !strings.Contains(string(export), `"id":"`+handle.SessionID+`"`) {
		t.Fatalf("the session was kept as %q, want the export of the run's session %s", export, handle.SessionID)
	}

	calls := readIn(t, dir, "calls")
	standalone := false
	for line := range strings.Lines(calls) {
		// The launch's own policy read runs one before the run (openCodePolicy).
		standalone = standalone || strings.Contains(line, "--standalone") && !strings.Contains(line, "config.get")
	}
	if !strings.Contains(calls, "session export --server ") || standalone {
		t.Errorf("want the session exported through the run's own server, and no runtime of OpenCode's own "+
			"started beside it; the harness was asked:\n%s", calls)
	}
	if exported := readIn(t, dir, "exported"); exported != handle.SessionID+" serving\n" {
		t.Errorf("the session was exported %q, want once, through the run's server while it still served",
			exported)
	}
}

// A SESSION THE BOX'S OPENCODE COULD NOT BE ASKED ABOUT IS A PACK THAT FAILED,
// never "no session". Only OpenCode's own answer that it holds no such id means
// nothing to pack; a probe that failed otherwise (its server would not start,
// its database was busy, it crashed) may hide a session the box holds, so the
// capture is refused with what OpenCode said, and the turn's end names the
// capture not kept rather than an older one as its last.
//
// Mutation: read every failed probe as nothing to pack, or drop what the probe
// said from the refusal, and a check here goes red.
func TestAnOpenCodeSessionTheBoxCouldNotBeAskedAboutIsAFailedPack(t *testing.T) {
	t.Parallel()
	runner, b := openCodeBox(t, t.TempDir())
	dir := t.TempDir()
	const answers = `held "${3#sessionID=}" || { echo "Error: Session not found: ${3#sessionID=}" >&2; exit 1; }`
	if !strings.Contains(fakeOpenCode, answers) {
		t.Fatal("the fake OpenCode no longer answers session.get as this case breaks it; follow it here")
	}
	broken := strings.Replace(fakeOpenCode, answers, `echo "Error: database is locked" >&2; exit 1`, 1)
	writeBoxStandIn(t, b, paths(b).BinDir()+"/opencode",
		strings.Replace(broken, `d="$FAKE_DIR"`, "d="+shellword.Quote(dir), 1))
	const session = "ses_0123456789abcdef0123456789"
	if err := b.WriteFile(t.Context(), paths(b).SessionRecord(),
		[]byte("id "+session+"\nrelease 2.3.2\n")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sessions", session), nil, 0o600); err != nil {
		t.Fatal(err)
	}

	described, body, _, err := runner.PackCapture(t.Context(), b, false)
	if err == nil || body != nil {
		t.Fatalf("PackCapture of a session OpenCode could not be asked about = %+v, %v, %v; want a "+
			"pack that failed", described, body, err)
	}
	if !strings.Contains(err.Error(), "database is locked") {
		t.Errorf("the refusal says %q, want what OpenCode said", err)
	}
}

// gunzipped is a gzipped stream's content.
func gunzipped(t *testing.T, packed []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(packed))
	if err != nil {
		t.Fatalf("the archive is not gzip: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("reading the archive: %v", err)
	}
	return out
}
