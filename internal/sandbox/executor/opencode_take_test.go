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

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// AN OPENCODE SESSION IS PACKED ON DEMAND BY ITS OWN STANDALONE EXPORT, AND KEPT
// WITHOUT A SECRET THE ENGINE PUT IN ITS BOX. A job that never packed — stopped,
// cut off at its turn's cap, lost with its engine — has no server left whose
// address and password only its script held, so the pack asks the box's
// OpenCode itself (`--standalone`) whether it holds the session the run's
// record names: an id it does not hold — a harness that failed before it made
// the session — is nothing to pack, and one it holds is exported, gzipped and
// its digest kept in the box. Taken out through the scrub of a document, then
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

	described, body, err := runner.PackCapture(t.Context(), b)
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
