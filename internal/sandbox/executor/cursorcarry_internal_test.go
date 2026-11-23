package executor

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/prereqtest"
)

// cursorSecret is the stand-in for a value the engine handed a box that the
// fixture's conversation holds: the prompt and a shell call's command and
// output named it.
const cursorSecret = "STANDIN-SECRET-0123456789abcdef"

// cursorNodeBin is the node the helper's cases run on.
func cursorNodeBin(t *testing.T) string {
	t.Helper()
	return prereqtest.OnPath(t, "node", "the Cursor Agent runner's helper runs under it",
		"install any current node, as ci.yml does (CONTRIBUTING.md, \"A skip is not a pass\")")
}

// cursorHelperRun runs the helper's command with args, its exit status and its
// error stream.
func cursorHelperRun(t *testing.T, args ...string) (int, string) {
	t.Helper()
	cmd := exec.Command(cursorNodeBin(t), append([]string{"cursor_helper.mjs"}, args...)...) //nolint:gosec // the suite's own helper
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if exit, ok := errors.AsType[*exec.ExitError](err); ok {
		return exit.ExitCode(), stderr.String()
	}
	if err != nil {
		t.Fatalf("running the helper: %v", err)
	}
	return 0, stderr.String()
}

// cursorExport exports a copy of the store fixture named store (the export
// opens a store it recovers for writing), its messages, its exit status and
// its error stream.
func cursorExport(t *testing.T, store string) ([]byte, int, string) {
	t.Helper()
	dir := t.TempDir()
	db := filepath.Join(dir, "store.db")
	if err := os.WriteFile(db, cursorFixture(t, "stores/"+store+".store.sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(dir, "out.jsonl")
	code, stderr := cursorHelperRun(t, "export", db, out)
	raw, _ := os.ReadFile(out)
	return raw, code, stderr
}

// cursorArchiveOf is lines as a session's archive: one gzipped tar of one
// member, as a run packs it.
func cursorArchiveOf(t *testing.T, session string, lines []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	if err := tw.WriteHeader(&tar.Header{Name: cursorArchiveDir + "/" + session + ".jsonl", Mode: 0o600,
		Size: int64(len(lines)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(lines); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// cursorMember is the one member of a session's archive.
func cursorMember(t *testing.T, archive []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(zr)
	if _, err = tr.Next(); err != nil {
		t.Fatal(err)
	}
	raw, err := io.ReadAll(tr)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// cursorScrubbed is lines carried out of a box handed values: packed,
// scrubbed as a capture is, and read back.
func cursorScrubbed(t *testing.T, lines []byte, values ...string) []byte {
	t.Helper()
	archive := cursorArchiveOf(t, "11111111-2222-4333-8444-555555555555", lines)
	var kept bytes.Buffer
	if err := scrubArchive((CursorAgent{}).layout(), bytes.NewReader(archive), reopenOf(archive), &kept,
		newScrubber(values), testBounds); err != nil {
		t.Fatalf("the scrub: %v", err)
	}
	return cursorMember(t, kept.Bytes())
}

// A CURSOR CONVERSATION IS CARRIED SCRUBBED, WHOLE AND IN ORDER: a real chat
// store of the release (a shell call's turn, then a resumed one) holding a
// handed value, exported, scrubbed as a capture is and made the history a
// continued run hands Cursor, holds the scrub's marker and never the value,
// every message in the store's order, and not Cursor's own account of the box.
// Measured beside it: this history, imported by the release's local-loop
// build, put the marker and never the value in the model's request, in order.
//
// Mutation: walk the root's fields out of order, keep the user_info message,
// or write the export as one document the scrub reads as text: this goes red.
func TestACursorConversationIsCarriedScrubbedWholeAndInOrder(t *testing.T) {
	t.Parallel()
	lines, code, stderr := cursorExport(t, "plain")
	if code != 0 {
		t.Fatalf("the export refused a store of the measured release (exit %d): %s", code, stderr)
	}
	if !bytes.Contains(lines, []byte(cursorSecret)) {
		t.Fatal("the fixture's conversation holds no handed value, so nothing tests the scrub")
	}
	kept := cursorScrubbed(t, lines, cursorSecret)
	dir := t.TempDir()
	in, doc := filepath.Join(dir, "in.jsonl"), filepath.Join(dir, "history.json")
	if err := os.WriteFile(in, kept, 0o600); err != nil {
		t.Fatal(err)
	}
	if code, stderr = cursorHelperRun(t, "doc", in, doc); code != 0 {
		t.Fatalf("the scrubbed conversation was refused as a history (exit %d): %s", code, stderr)
	}
	raw, err := os.ReadFile(doc)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(raw, []byte(cursorSecret)) || !bytes.Contains(raw, []byte(scrubMarker)) {
		t.Fatalf("the history holds the handed value, or not the scrub's marker:\n%s", raw)
	}
	var history struct {
		Messages []map[string]json.RawMessage `json:"messages"`
	}
	if err = json.Unmarshal(raw, &history); err != nil {
		t.Fatalf("the history is no document Cursor reads: %v", err)
	}
	var kinds []string
	for _, m := range history.Messages {
		for kind := range m {
			kinds = append(kinds, kind)
		}
	}
	if want := []string{"user", "assistant", "tool", "assistant", "user", "assistant"}; !slices.Equal(kinds, want) {
		t.Errorf("the history's messages are %v, want %v", kinds, want)
	}
	if bytes.Contains(raw, []byte("<user_info>")) {
		t.Error("the history carries Cursor's own account of the box, which the next run writes afresh")
	}
	if !strings.Contains(string(raw), "PROMPT-TWO") {
		t.Error("the history lost the resumed turn")
	}
}

// THE EXPORT REFUSES WHAT IT DOES NOT KNOW: a store whose conversation holds a
// part of a kind it does not carry, a message that is no JSON blob, or a role
// it does not carry is refused with a reason naming it, and nothing is
// written, so the capture fails and the next run starts afresh rather than
// continue a conversation with a part left out.
//
// Mutation: skip an unknown part, an unknown blob or an unknown role, or write
// what was read before the refusal: a row goes red.
func TestTheCursorExportRefusesWhatItDoesNotKnow(t *testing.T) {
	t.Parallel()
	for store, says := range map[string]string{
		"unknown-part":   `part of type "file"`,
		"binary-message": "not a JSON message",
		"unknown-role":   `the role "developer"`,
	} {
		lines, code, stderr := cursorExport(t, store)
		if code != 3 || !strings.Contains(stderr, says) || len(lines) != 0 {
			t.Errorf("%s: exit %d, wrote %d bytes, said %q; want exit 3, nothing written, naming %q", store,
				code, len(lines), stderr, says)
		}
	}
}

// A CARRIED CURSOR CONVERSATION KEEPS NO CREDENTIAL: the values a run is handed
// to sign Cursor in (CURSOR_API_KEY's key; a login's accessToken and
// refreshToken; the apiKey Cursor writes beside them in auth.json) are taken
// out of the exported conversation wherever a turn wrote them, the model's
// words, a call's arguments and its output alike; a JWT-shaped token the box
// minted itself is taken out by its shape.
//
// Mutation: scrub the archive as a document, or leave a JSON string's escaped
// text unscrubbed: this goes red.
func TestACarriedCursorConversationKeepsNoCredential(t *testing.T) {
	t.Parallel()
	const (
		key     = "crsr-standin-api-key-0123456789"
		access  = "standin-access-token-0123456789"
		refresh = "standin-refresh-token-0123456789"
		minted  = "eyJhbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJhdXRoMHxTVEFORElOIn0.c3RhbmRpbi1zaWduYXR1cmUtMDEyMzQ1Njc4OQ"
	)
	auth := `{"accessToken":"` + access + `","refreshToken":"` + refresh + `","apiKey":"` + key + `"}`
	var lines bytes.Buffer
	for _, m := range []any{
		map[string]any{"user": map[string]any{"content": []any{map[string]any{"text": map[string]any{
			"text": "sign in with " + key}}}}},
		map[string]any{"assistant": map[string]any{"content": []any{map[string]any{"toolCall": map[string]any{
			"toolCallId": "call_1", "toolName": "Shell",
			"argsJson": `{"command":"cat ~/.config/cursor/auth.json; echo ` + minted + `"}`}}}}},
		map[string]any{"tool": map[string]any{"toolCallId": "call_1", "toolName": "Shell",
			"content": []any{map[string]any{"text": map[string]any{"text": auth + "\n" + minted}}}}},
	} {
		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		lines.Write(append(raw, '\n'))
	}
	kept := string(cursorScrubbed(t, lines.Bytes(), key, access, refresh))
	for name, value := range map[string]string{"CURSOR_API_KEY": key, "accessToken": access,
		"refreshToken": refresh, "a minted token": minted} {
		if strings.Contains(kept, value) {
			t.Errorf("%s survived the scrub:\n%s", name, kept)
		}
	}
	if strings.Count(kept, "\n") != 3 {
		t.Errorf("the scrub did not keep a message a line:\n%s", kept)
	}
}
