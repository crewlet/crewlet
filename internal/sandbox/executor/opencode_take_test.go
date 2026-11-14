package executor_test

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// AN OPENCODE SESSION IS PACKED ON DEMAND BY ITS OWN STANDALONE EXPORT, AND KEPT
// WITHOUT A SECRET THE ENGINE PUT IN ITS BOX. A job that never packed — killed
// before its script could, lost with its engine, still working where its box
// would not end it — leaves no server the engine can reach, whose address and
// password only its script held, so the pack asks the box's OpenCode itself
// (`--standalone`) whether it holds the session the run's record names: an id
// it does not hold — a harness that failed before it made the session — is
// nothing to pack, and one it holds is exported, gzipped and its digest kept
// in the box. Taken out through the scrub of a document, then
// sealed, uploaded and opened from the store as a later launch opens it, the
// export is still one JSON document of that session, holding no value the
// engine named — in a message's text or as a key — no credential only its
// shape gives away, and no value under a password's key.
//
// Mutation: export through the run's server, export a session the box does not
// hold, skip the document's rewrite, or write a key as it came, and a check
// here goes red.
func TestAnOpenCodeSessionIsPackedOnDemandAndKeptScrubbed(t *testing.T) {
	t.Parallel()
	runner, b := openCodeBox(t, t.TempDir())
	dir := t.TempDir()
	writeBoxStandIn(t, b, paths(b).BinDir()+"/opencode",
		strings.Replace(fakeOpenCode, `d="$FAKE_DIR"`, "d="+shQuote(dir), 1))
	const session = "ses_0123456789abcdef0123456789"
	if err := b.WriteFile(t.Context(), paths(b).SessionRecord(),
		[]byte("id "+session+"\nrelease 2.0.32\n")); err != nil {
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
	taken, err := runner.TakeSession(t.Context(), b, sandbox.SessionTake{Pack: true,
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

// AN OPENCODE SESSION PACKED ON DEMAND IS PACKED WHOLE BY AN OPENCODE THAT CUTS
// WHAT IT WRITES INTO A PIPE. The pack exports with OpenCode's own runtime
// (`--standalone`) and piped that export into gzip, which 2.0.32 and 2.1.4 stop
// writing after a 64 KiB multiple with status 0: a 16 MiB session's standalone
// export was packed as 3,080,192 bytes of it on 2.0.32 and 7,077,888 on 2.1.4,
// and the take's scrub refused the cut archive, so the pack failed. Written to
// a file and gzipped from there, the archive of a session whose export runs
// past the cut gunzips to all of it, and that whole archive's digest is the one
// the box keeps.
//
// Mutation: pipe the export into gzip again, and the archive holds the first
// 64 KiB of it.
func TestAnOpenCodeSessionPackedOnDemandIsPackedWhole(t *testing.T) {
	t.Parallel()
	runner, b := openCodeBox(t, t.TempDir())
	dir := t.TempDir()
	writeBoxStandIn(t, b, paths(b).BinDir()+"/opencode", strings.Replace(fakeOpenCode, `d="$FAKE_DIR"`,
		"d="+shQuote(dir)+" FAKE_PIPE_CUT="+strconv.Itoa(pipeCut), 1))
	const session = "ses_0123456789abcdef0123456789"
	if err := b.WriteFile(t.Context(), paths(b).SessionRecord(),
		[]byte("id "+session+"\nrelease 2.0.32\n")); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sessions"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sessions", session), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	messages := longMessages(3 * pipeCut)
	if err := os.WriteFile(filepath.Join(dir, "export-messages"), []byte(messages), 0o600); err != nil {
		t.Fatal(err)
	}

	_, body, release, err := runner.PackCapture(t.Context(), b, false)
	if err != nil || body == nil {
		t.Fatalf("PackCapture of a session the box holds = %v, %v; want its archive", body, err)
	}
	archive, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil {
		t.Fatalf("reading the archive: %v", err)
	}
	want := exportOf(session, messages)
	if export := gunzipped(t, archive); string(export) != want {
		t.Errorf("the archive holds %d bytes of the session's %d-byte export, want all of it", len(export), len(want))
	}
	if digests := boxFile(t, b, paths(b).SessionDigests()); !strings.Contains(digests,
		session+" "+digestOf(t, archive)+"\n") {
		t.Errorf("the box keeps no digest of the archive the pack made: %q", digests)
	}
	if calls := readIn(t, dir, "calls"); !strings.Contains(calls, "session export --standalone "+session) {
		t.Errorf("the pack did not export the session standalone; the harness was asked:\n%s", calls)
	}
	release(t.Context())
}

// AN ENDED OPENCODE JOB'S SESSION IS TAKEN AS ITS SCRIPT EXPORTED IT. A job the
// engine's read ends while it still works — a stop's, a loss's, a collection's
// past its turn's cap (internal/sandbox's endJob) — has its script, asked,
// interrupt the session through the run's own server, export it through that
// server while it still serves, and only then stop it; the runner's take that
// follows, of a job that is over ([sandbox.SessionTake.Pack] false), reads
// that export as the script left it and packs nothing again: no runtime of
// OpenCode's own (`--standalone`) is started against the database the run's
// server held, where it could find the database busy and refuse the pack.
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
		"d="+shQuote(dir)+" FAKE_RUN_UNTIL=interrupted FAKE_RUN_EXIT=7 FAKE_RUN_TERM_EXIT=7", 1))
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
	res, err := runner.Collect(t.Context(), b, handle)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if res.JobEnd != sandbox.JobEndAsked || res.Captured == nil {
		t.Fatalf("the ended job collected as ended %q with capture %+v; want it ended on request, its "+
			"session exported:\n%s", res.JobEnd, res.Captured, boxFile(t, b, paths(b).Err()))
	}
	taken, err := runner.TakeSession(t.Context(), b, sandbox.SessionTake{MaxBytes: takeBound})
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
	if !strings.Contains(calls, "session export --server ") || strings.Contains(calls, "--standalone") {
		t.Errorf("want the session exported through the run's own server, and no runtime of OpenCode's own "+
			"started beside it; the harness was asked:\n%s", calls)
	}
	if exported := readIn(t, dir, "exported"); exported != handle.SessionID+" serving\n" {
		t.Errorf("the session was exported %q, want once, through the run's server while it still served",
			exported)
	}
}

// A SESSION THE BOX'S OPENCODE COULD NOT BE ASKED ABOUT IS A PACK THAT FAILED,
// never "no session". Only OpenCode's own answer that it holds no such id says
// the box has nothing to pack; a probe that failed any other way — its private
// server would not start, its database was busy under a job whose server still
// ran, it crashed — is a session the box may well hold, and the capture is
// refused with what OpenCode said, so the turn's end names the capture that was
// not kept rather than an older one as the turn's last.
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
		strings.Replace(broken, `d="$FAKE_DIR"`, "d="+shQuote(dir), 1))
	const session = "ses_0123456789abcdef0123456789"
	if err := b.WriteFile(t.Context(), paths(b).SessionRecord(),
		[]byte("id "+session+"\nrelease 2.0.32\n")); err != nil {
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
