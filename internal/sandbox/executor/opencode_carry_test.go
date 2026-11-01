package executor_test

import (
	"bytes"
	"compress/gzip"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// The OpenCode run's session, carried between boxes, over the fake opencode of
// opencode_script_test.go ([fakeOpenCode]).

// openCodeExport is the export document the fake prints for session: what a
// session carried into a box was packed from.
func openCodeExport(session string) string {
	return `{"info":{"id":"` + session + `","title":"crewlet"},"messages":[{"id":"msg_1","type":"user","text":"fix the flake"}]}` + "\n"
}

const (
	// carriedOpenCode is the session a continued run is asked to take up,
	// and freshOpenCode the one it was minted to start in its place.
	carriedOpenCode = "ses_0123456789abcdef0123456789abcdef"
	freshOpenCode   = "ses_fedcba9876543210fedcba9876543210"
)

// continuingOpenCode is the session of a run that continues carriedOpenCode.
var continuingOpenCode = executor.Session{ID: carriedOpenCode, Continues: true, Fresh: freshOpenCode}

// gzipped is text as a box's script packs it: gzip, as Go writes it, which
// is the same bytes for the same text every time — so a case can name the
// digest of what it carried in.
func gzipped(t *testing.T, text string) []byte {
	t.Helper()
	var packed bytes.Buffer
	zw := gzip.NewWriter(&packed)
	if _, err := zw.Write([]byte(text)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return packed.Bytes()
}

// digestOf is what a box's script records of an archive: POSIX `cksum`'s
// answer for its bytes read on stdin — their CRC and their length.
func digestOf(t *testing.T, archive []byte) string {
	t.Helper()
	cmd := exec.Command("cksum")
	cmd.Stdin = bytes.NewReader(archive)
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("cksum: %v", err)
	}
	return strings.TrimSpace(string(out))
}

// keepDigest records archive among the box's own archives of session
// ([executor.Paths.SessionDigests]), as the script of a box that packed or
// restored it does.
func keepDigest(t *testing.T, paths executor.Paths, session string, archive []byte) {
	t.Helper()
	line := session + " " + digestOf(t, archive) + "\n"
	if err := os.WriteFile(paths.SessionDigests(), []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
}

// keeps reports whether the box's digests hold archive's for session.
func keeps(t *testing.T, run scriptRun, session string, archive []byte) bool {
	t.Helper()
	want := session + " " + digestOf(t, archive)
	for _, line := range strings.Split(readIn(t, filepath.Join(run.home, ".crewlet"), "session-digests"), "\n") {
		if line == want {
			return true
		}
	}
	return false
}

// sessionOf is the session the run's `opencode run` was pointed at.
func sessionOf(t *testing.T, run scriptRun) string {
	t.Helper()
	at := run.callIndex("run ")
	if at < 0 {
		t.Fatalf("the brief was never run:\n%s", strings.Join(run.calls, "\n"))
	}
	_, session, _ := strings.Cut(run.calls[at], "--session ")
	session, _, _ = strings.Cut(session, " ")
	return session
}

// recordOf is the run's record of its session, as its script left it.
func recordOf(t *testing.T, run scriptRun) string {
	t.Helper()
	return readIn(t, filepath.Join(run.home, ".crewlet"), "session")
}

// recordedWhy is the `why` line of the run's record of its session.
func recordedWhy(t *testing.T, run scriptRun) string {
	t.Helper()
	for _, line := range strings.Split(recordOf(t, run), "\n") {
		if why, ok := strings.CutPrefix(line, "why "); ok {
			return why
		}
	}
	return ""
}

// A SESSION CARRIED INTO A BOX IS IMPORTED BEFORE THE RUN, AND CONTINUED. The
// run's script unpacks the export it was handed and imports it through a
// private server of its own (`--standalone`) — never the background service
// every box on a host shares — into the directory the run works in, before the
// run's server starts, since that server is handed the session it runs; then
// the run takes the session up, the report before it taken after the import,
// so the totals the import restored are among what is taken off. The archive
// is removed once read, its digest is kept as one the box's copy holds, and
// the record says the run continued.
//
// Mutation: import after the server starts, through the background service, or
// into another directory, run the minted session after an import, or keep no
// digest of what was imported, and the case goes red.
func TestAnOpenCodeSessionCarriedIntoABoxIsImportedBeforeTheRun(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		run := runScript(t, scriptOpts{shell: sh, session: &continuingOpenCode,
			carried: openCodeExport(carriedOpenCode),
			req:     sandbox.RunRequest{Resume: carriedOpenCode, MCPServers: bridgeOnly, Steer: steerWiring}})
		if run.code != 0 {
			t.Fatalf("exit %d:\n%s", run.code, run.stderr)
		}
		imported, serve := run.callIndex("session import --standalone --directory "), run.callIndex("serve ")
		if imported < 0 || serve < 0 || imported > serve {
			t.Fatalf("the session was not imported through a private server before the run's own "+
				"started:\n%s", strings.Join(run.calls, "\n"))
		}
		if run.callIndex("session delete") >= 0 {
			t.Errorf("a box that held no copy deleted one:\n%s", strings.Join(run.calls, "\n"))
		}
		if got := readIn(t, run.dir, "imported-session"); got != openCodeExport(carriedOpenCode) {
			t.Errorf("the import was handed %q, want the carried export unpacked", got)
		}
		dir, err := filepath.EvalSymlinks(run.dir)
		if err != nil {
			t.Fatal(err)
		}
		if got, _ := filepath.EvalSymlinks(readIn(t, run.dir, "import-directory")); got != dir {
			t.Errorf("the session was imported into %q, want the directory the run works in, %q", got, dir)
		}
		if got := sessionOf(t, run); got != carriedOpenCode {
			t.Errorf("the run worked in %q, want the carried %q", got, carriedOpenCode)
		}
		if !strings.Contains(run.stdout, `"type":"crewlet.usage_before"`) {
			t.Errorf("a continued run printed no report before it:\n%s", run.stdout)
		}
		if want := "id " + carriedOpenCode + "\ncontinued yes\nwhy \n"; !strings.HasPrefix(recordOf(t, run), want) {
			t.Errorf("the record is %q, want it to begin %q", recordOf(t, run), want)
		}
		if !keeps(t, run, carriedOpenCode, gzipped(t, openCodeExport(carriedOpenCode))) {
			t.Errorf("the box keeps no digest of the export it imported: %q",
				readIn(t, filepath.Join(run.home, ".crewlet"), "session-digests"))
		}
		for _, line := range strings.Split(strings.TrimSpace(readIn(t, run.dir, "steered")), "\n") {
			if handed, call, _ := strings.Cut(line, "|"); strings.HasPrefix(call, "serve ") && handed != carriedOpenCode {
				t.Errorf("the server was handed the session %q, want the carried %q", handed, carriedOpenCode)
			}
		}
		if _, err := os.Stat(filepath.Join(run.home, ".crewlet", "session-carried")); !os.IsNotExist(err) {
			t.Errorf("the carried archive was left in the box (%v)", err)
		}
	})
}

// AN IMPORT THAT DID NOT HAPPEN IS NO CONTINUATION. OpenCode answers an id its
// database already holds with "Session already exists" and success, and an
// import can also say nothing at all — and `run --session` on an id it does
// not hold creates an empty session under it, which the brief would then be
// answered in as if it remembered. So the import must say it imported THAT
// session; where it did not, where the box's own copy that the export would
// replace could not be deleted, or where nothing was carried and the box holds
// none, the run starts the session minted for it in its place, says why on its
// error stream and in its record, prints no report before it, and its record
// says it did not continue.
//
// Mutation: take the import's success as the session's, run the asked session
// after a failed one, or record no reason, and a row goes red.
func TestAnOpenCodeImportThatDidNotHappenIsNoContinuation(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		for name, tc := range map[string]struct {
			env     []string
			held    []string
			carried string
			said    string
		}{
			"a conflict": {env: []string{"FAKE_IMPORT=409"}, carried: openCodeExport(carriedOpenCode),
				said: "Session already exists"},
			"an import that says nothing": {env: []string{"FAKE_IMPORT=empty"},
				carried: openCodeExport(carriedOpenCode), said: "OpenCode did not import it"},
			"a copy the export replaces, which will not go": {env: []string{"FAKE_DELETE=fail"},
				held: []string{carriedOpenCode}, carried: openCodeExport(carriedOpenCode),
				said: "OpenCode did not delete the box's own copy"},
			"nothing carried": {said: "it is not in this box"},
		} {
			t.Run(name, func(t *testing.T) {
				t.Parallel()
				run := runScript(t, scriptOpts{shell: sh, session: &continuingOpenCode, carried: tc.carried,
					held: tc.held, env: tc.env, req: sandbox.RunRequest{Resume: carriedOpenCode,
						MCPServers: bridgeOnly, Steer: steerWiring}})
				if run.code != 0 {
					t.Fatalf("exit %d:\n%s", run.code, run.stderr)
				}
				if got := sessionOf(t, run); got != freshOpenCode {
					t.Errorf("the run worked in %q, want the session minted in the carried one's place, %q",
						got, freshOpenCode)
				}
				if !strings.Contains(run.stderr, "starts a new session, "+freshOpenCode) ||
					!strings.Contains(run.stderr, tc.said) {
					t.Errorf("the error stream does not say why the run started anew (%q):\n%s", tc.said, run.stderr)
				}
				if strings.Contains(run.stdout, `"type":"crewlet.usage_before"`) {
					t.Errorf("a run that started its session printed a report before it:\n%s", run.stdout)
				}
				if want := "id " + freshOpenCode + "\ncontinued no\n"; !strings.HasPrefix(recordOf(t, run), want) {
					t.Errorf("the record is %q, want it to begin %q", recordOf(t, run), want)
				}
				if why := recordedWhy(t, run); !strings.Contains(why, tc.said) {
					t.Errorf("the record gives the reason %q, want one saying %q", why, tc.said)
				}
				for _, line := range strings.Split(strings.TrimSpace(readIn(t, run.dir, "steered")), "\n") {
					if handed, call, _ := strings.Cut(line, "|"); strings.HasPrefix(call, "serve ") &&
						handed != freshOpenCode {
						t.Errorf("the server was handed the session %q, want the one the run works in, %q",
							handed, freshOpenCode)
					}
				}
			})
		}
	})
}

// A BOX'S COPY THAT HOLDS THE CARRIED EXPORT IS NEVER IMPORTED OVER: a box
// that packed or restored that export keeps its digest, and its copy is the
// export's or newer — a run went on in the box after it, whose own export may
// have failed — so a run there takes the box's copy up without an import.
//
// Mutation: import whenever an export was carried into a box holding the
// session, and the box's copy is deleted for an older one.
func TestAnOpenCodeBoxWhoseCopyHoldsTheExportIsNotImportedOver(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		run := runScript(t, scriptOpts{shell: sh, session: &continuingOpenCode, held: []string{carriedOpenCode},
			carried: openCodeExport(carriedOpenCode), ownsCarried: true, env: []string{"FAKE_IMPORT=409"},
			req: sandbox.RunRequest{Resume: carriedOpenCode, MCPServers: bridgeOnly}})
		if run.code != 0 {
			t.Fatalf("exit %d:\n%s", run.code, run.stderr)
		}
		if run.callIndex("session import") >= 0 || run.callIndex("session delete") >= 0 {
			t.Errorf("a box whose copy holds the export replaced it:\n%s", strings.Join(run.calls, "\n"))
		}
		if got := sessionOf(t, run); got != carriedOpenCode {
			t.Errorf("the run worked in %q, want the session its box holds, %q", got, carriedOpenCode)
		}
		if want := "id " + carriedOpenCode + "\ncontinued yes\n"; !strings.HasPrefix(recordOf(t, run), want) {
			t.Errorf("the record is %q, want it to begin %q", recordOf(t, run), want)
		}
	})
}

// A BOX'S COPY THAT DOES NOT HOLD THE CARRIED EXPORT IS REPLACED BY IT. The
// box held the session as it stood at an earlier point, and the export comes
// from the box the session went on in, so the box's own copy is deleted — only
// once the export unpacked — and the export imported in its place, before the
// run's server starts; the run then continues the session it was carried.
//
// Mutation: keep the box's copy whatever was carried, or delete it after the
// import, and the run continues an older session or none.
func TestAnOpenCodeBoxsCopyThatDoesNotHoldTheExportIsReplaced(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		run := runScript(t, scriptOpts{shell: sh, session: &continuingOpenCode, held: []string{carriedOpenCode},
			carried: openCodeExport(carriedOpenCode),
			req:     sandbox.RunRequest{Resume: carriedOpenCode, MCPServers: bridgeOnly}})
		if run.code != 0 {
			t.Fatalf("exit %d:\n%s", run.code, run.stderr)
		}
		deleted := run.callIndex("session delete --standalone " + carriedOpenCode)
		imported, serve := run.callIndex("session import --standalone "), run.callIndex("serve ")
		if deleted < 0 || imported < deleted || serve < imported {
			t.Fatalf("the box's copy was not deleted and the export imported in its place before the "+
				"run's server started:\n%s", strings.Join(run.calls, "\n"))
		}
		if got := sessionOf(t, run); got != carriedOpenCode {
			t.Errorf("the run worked in %q, want the carried %q", got, carriedOpenCode)
		}
		if want := "id " + carriedOpenCode + "\ncontinued yes\n"; !strings.HasPrefix(recordOf(t, run), want) {
			t.Errorf("the record is %q, want it to begin %q", recordOf(t, run), want)
		}
		if !keeps(t, run, carriedOpenCode, gzipped(t, openCodeExport(carriedOpenCode))) {
			t.Error("the box keeps no digest of the export it imported")
		}
	})
}

// THE SESSION IS EXPORTED ONCE THE RUN HAS ENDED, THROUGH THE RUN'S OWN SERVER,
// BEFORE IT STOPS. After the steers the run left are cancelled, the session the
// run worked in is exported — through the server that holds the password the
// script read — gzipped and moved into place whole, and its digest is kept as
// one the box's copy holds; never the database, which holds the login too. A
// run that started its session carries it on the same.
//
// Mutation: export after the server stops, or before the leftover steers are
// cancelled, gzip the database, or keep no digest of the export, and the case
// goes red.
func TestAnOpenCodeRunsSessionIsExportedBeforeItsServerStops(t *testing.T) {
	t.Parallel()
	forEachShell(t, func(t *testing.T, sh boxShell) {
		run := runScript(t, scriptOpts{shell: sh, login: seededLogin,
			req: sandbox.RunRequest{MCPServers: bridgeOnly, Steer: steerWiring}})
		if run.code != 0 {
			t.Fatalf("exit %d:\n%s", run.code, run.stderr)
		}
		session := sessionOf(t, run)
		exported := run.callIndex("session export --server http://127.0.0.1:4242 " + session)
		if exported < 0 || exported < run.callIndex("api --server http://127.0.0.1:4242 session.inbox.list") {
			t.Fatalf("the run's session was not exported through its server after its steers were "+
				"cancelled:\n%s", strings.Join(run.calls, "\n"))
		}
		if got := strings.TrimSpace(readIn(t, run.dir, "exported")); got != session+" serving" {
			t.Errorf("the export was made as %q, want %q: while the run's server served", got, session+" serving")
		}
		raw, err := os.ReadFile(filepath.Join(run.home, ".crewlet", "session-capture"))
		if err != nil {
			t.Fatalf("the session was not captured: %v\n%s", err, run.stderr)
		}
		zr, err := gzip.NewReader(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("the capture is not gzip: %v", err)
		}
		export, err := io.ReadAll(zr)
		if err != nil || string(export) != openCodeExport(session) {
			t.Errorf("the capture holds %q (%v), want the session's export", export, err)
		}
		if bytes.Contains(export, []byte("cred_1")) {
			t.Errorf("the capture holds the box's login:\n%s", export)
		}
		if !keeps(t, run, session, raw) {
			t.Errorf("the box keeps no digest of the export it packed: %q",
				readIn(t, filepath.Join(run.home, ".crewlet"), "session-digests"))
		}
		if want := "id " + session + "\ncontinued no\nwhy \n"; !strings.HasPrefix(recordOf(t, run), want) {
			t.Errorf("the record is %q, want it to begin %q", recordOf(t, run), want)
		}
	})
}
