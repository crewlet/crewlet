package executor_test

import (
	"bytes"
	"io"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// geminiCarryStandIn is the run of a stand-in Gemini CLI that keeps a session
// as 0.70.0 does, from its own captures: a new session (`--session-id`) is
// session-tools.jsonl under the pinned slug, renamed to the id; a resumed one
// (`--resume`) must already lie there, and gets one more record appended. It
// prints stream-tools.jsonl, renamed, and records its argv.
func geminiCarryStandIn(t *testing.T) string {
	t.Helper()
	session, err := filepath.Abs(filepath.Join("testdata", "gemini", "0.70.0", "session-tools.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	stream, err := filepath.Abs(filepath.Join("testdata", "gemini", "0.70.0", "stream-tools.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return `for a; do printf '%s\n' "$a"; done > "$HOME/argv"
s= prev=; for a; do case "$prev" in --session-id|--resume) s=$a;; esac; prev=$a; done
d="$HOME/.gemini/tmp/crewlet/chats"; mkdir -p "$d"; i8=$(printf %.8s "$s"); f=
for g in "$d"/session-*-"$i8".jsonl; do [ -f "$g" ] && f=$g; done
cat > /dev/null
case " $* " in
*" --resume "*) [ -n "$f" ] || { echo "no session $s to resume" >&2; exit 1; }
  printf '{"$set":{"lastUpdated":"2026-11-22T00:00:00.000Z"}}\n' >> "$f";;
*) sed "s/842120d9-2536-4808-8ba9-437d34456f75/$s/" ` + shQuote(session) + ` > "$d/session-2026-11-21T18-48-$i8.jsonl";;
esac
sed "s/6b8d5e13-304f-44a6-aa06-9f3ccff25600/$s/" ` + shQuote(stream) + "\n"
}

// geminiCarried is a Gemini CLI run's capture in a box of release, read out
// whole: what it is described as, and its bytes.
func geminiCarried(t *testing.T, release string) (sandbox.Result, sandbox.SessionArchive, []byte) {
	t.Helper()
	runner, a := geminiBoxAt(t, release, geminiCarryStandIn(t))
	_, ran := runToEnd(t, runner, a, sandbox.RunRequest{Brief: "begin"})
	described, body, err := runner.OpenCapture(t.Context(), a)
	if err != nil || body == nil {
		t.Fatalf("OpenCapture = %v, %v\n%s", body, err, boxFile(t, a, paths(a).Err()))
	}
	defer func() { _ = body.Close() }()
	raw, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	return ran, described, raw
}

// A GEMINI CLI SESSION CARRIED INTO A FRESH BOX IS RESTORED AND RESUMED. One
// run's capture is its chat file alone, named under chats/ relative to the
// pinned slug, described at the release that wrote it. Handed to a run in
// another box continuing the session, it is restored under that box's pinned
// slug before the harness starts and removed, and the harness resumes it
// (`--resume`, never a session of its own) and appends to it; the run reports
// it continued.
//
// Mutations, each red: pack the slug's directory rather than the chat file;
// restore the file anywhere but the pinned slug's chats; start
// `--session-id` on a run that continues; leave the carried archive in place.
func TestAGeminiSessionCarriedIntoAFreshBoxIsRestoredAndResumed(t *testing.T) {
	t.Parallel()
	ran, described, raw := geminiCarried(t, geminiCapturedRelease)
	file := "session-2026-11-21T18-48-" + ran.SessionID[:8] + ".jsonl"
	members := archiveMembers(t, bytes.NewReader(raw))
	if len(members) != 1 || !strings.Contains(members["chats/"+file], `"sessionId":"`+ran.SessionID+`"`) {
		t.Fatalf("the capture holds %v, want chats/%s alone", slices.Sorted(maps.Keys(members)), file)
	}
	if described.Harness != executor.GeminiCLIName || described.SessionID != ran.SessionID ||
		described.Format != executor.GeminiArchiveFormat || described.Version != geminiCapturedRelease {
		t.Errorf("the capture is described as %+v, want session %s of Gemini CLI %s", described, ran.SessionID,
			geminiCapturedRelease)
	}

	runner, b := geminiBoxAt(t, geminiCapturedRelease, geminiCarryStandIn(t))
	_, res := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "carry on", Resume: ran.SessionID,
		Carried: &sandbox.CarriedSession{Archive: described, Body: bytes.NewReader(raw)}})
	argv := argvOf(t, b)
	if after(argv, "--resume") != ran.SessionID || slices.Contains(argv, "--session-id") {
		t.Fatalf("the carried session was not resumed: argv %q\n%s", argv, boxFile(t, b, paths(b).Err()))
	}
	if !res.Continued || res.SessionID != ran.SessionID || res.FellBack != "" {
		t.Errorf("the run reports continued %v in session %q (%q), want %s continued", res.Continued,
			res.SessionID, res.FellBack, ran.SessionID)
	}
	carried := members["chats/"+file]
	if got := boxFile(t, b, paths(b).GeminiChats()+"/"+file); !strings.HasPrefix(got, carried) ||
		strings.Count(got, "\n") != strings.Count(carried, "\n")+1 {
		t.Errorf("the restored chat file holds %d lines, want the carried %d and the resumed run's",
			strings.Count(got, "\n"), strings.Count(carried, "\n"))
	}
	if left := boxFile(t, b, paths(b).SessionCarried()); left != "" {
		t.Errorf("the carried archive was left in the box after its restore (%d bytes)", len(left))
	}
}

// A GEMINI CLI RELEASE NOBODY MEASURED CARRYING A SESSION STARTS ONE OF ITS
// OWN. A session 0.70.0 wrote, carried into a box running 0.71.0, a minor never
// measured resuming another's session, is not restored: the run starts the
// session minted for it (`--session-id`), removes the archive, and says why
// in a person's words, naming the release and the minors measured.
//
// Mutations, each red: hold the box's release to no measured minor in the
// script's gate; read the release after the gate; name the session asked for
// on the fallback.
func TestAGeminiReleaseNobodyMeasuredCarryingStartsASessionOfItsOwn(t *testing.T) {
	t.Parallel()
	ran, described, raw := geminiCarried(t, geminiCapturedRelease)
	runner, b := geminiBoxAt(t, "0.71.0", geminiCarryStandIn(t))
	_, res := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "carry on", Resume: ran.SessionID,
		Carried: &sandbox.CarriedSession{Archive: described, Body: bytes.NewReader(raw)}})
	argv := argvOf(t, b)
	fresh := after(argv, "--session-id")
	if fresh == "" || fresh == ran.SessionID || slices.Contains(argv, "--resume") {
		t.Fatalf("the run on 0.71.0 ran argv %q, want a session of its own", argv)
	}
	const why = "this box runs Gemini CLI 0.71.0, and a carried session was measured continuing only " +
		"between Gemini CLI 0.65.x and 0.70.x"
	if res.Continued || res.SessionID != fresh || res.FellBack != why {
		t.Errorf("the run reports continued %v in session %q because %q, want session %s because %q",
			res.Continued, res.SessionID, res.FellBack, fresh, why)
	}
	if left := boxFile(t, b, paths(b).SessionCarried()); left != "" {
		t.Errorf("the refused archive was left in the box (%d bytes)", len(left))
	}
}

// A QWEN CODE SESSION CARRIED INTO A FRESH BOX IS LOADED UNDER ITS OWN KEY.
// One run's capture is its chat file (Qwen Code 0.28.3's chat-note.jsonl),
// named under the packing box's key. Handed to a run in another box, whose
// working directory has another realpath and so another key, the session is
// filed under the new key with its first line's cwd that realpath, which the
// stand-in, like Qwen Code, holds a load to; the helper is told to load it
// (session/load, never a new session); the archive is removed; the run
// reports it continued.
//
// Mutations, each red: restore under the archive's own key; leave the first
// line's cwd as carried; start a new session on a run that continues; leave
// the carried archive in place.
func TestAQwenSessionCarriedIntoAFreshBoxIsLoadedUnderItsOwnKey(t *testing.T) {
	t.Parallel()
	chat, err := filepath.Abs(filepath.Join("testdata", "qwen", "0.28.3", "chat-note.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	carried, err := os.ReadFile(chat)
	if err != nil {
		t.Fatal(err)
	}
	// The stand-in's run: argv is the helper, `run`, its config, the session
	// and the mode; `restore` is the real helper's.
	standIn := `case "$2" in restore) exec ` + shQuote(nodeForQwen(t)) + ` "$@";; run) ;; *) exit 2;; esac
s=$4 m=$5 w=$(pwd -P); cat > /dev/null
printf '%s\n' "$m" > "$HOME/mode"; printf '%s\n' "$w" > "$HOME/cwd"
c="$HOME/.qwen/projects/$(printf '%s' "$w" | sed 's/[^A-Za-z0-9]/-/g')/chats"; mkdir -p "$c"
if [ "$m" = new ]; then
  sed -e "s/` + qwenCapturedSession + `/$s/" -e "1s|\"cwd\":\"/work/proj\"|\"cwd\":\"$w\"|" ` + shQuote(chat) + ` > "$c/$s.jsonl"
else
  head -n 1 "$c/$s.jsonl" 2> /dev/null | grep -qF "\"cwd\":\"$w\"" || { echo "no session $s recorded here" >&2; exit 1; }
  tail -n 1 "$c/$s.jsonl" >> "$c/$s.jsonl"
fi
printf '{"type":"crewlet_qwen_session","session_id":"%s"}\n' "$s"
echo '{"type":"crewlet_qwen_end","stop_reason":"end_turn"}'
`
	first, a := qwenBox(t, standIn)
	_, ran := runToEnd(t, first, a, sandbox.RunRequest{Brief: "begin"})
	described, body, err := first.OpenCapture(t.Context(), a)
	if err != nil || body == nil {
		t.Fatalf("OpenCapture = %v, %v\n%s", body, err, boxFile(t, a, paths(a).Err()))
	}
	defer func() { _ = body.Close() }()
	raw, err := io.ReadAll(body)
	if err != nil {
		t.Fatal(err)
	}
	keyA := qwenKey(strings.TrimSpace(boxFile(t, a, a.Home()+"/cwd")))
	members := archiveMembers(t, bytes.NewReader(raw))
	member := "projects/" + keyA + "/chats/" + ran.SessionID + ".jsonl"
	if len(members) != 1 || strings.Count(members[member], "\n") != strings.Count(string(carried), "\n") {
		t.Fatalf("the capture holds %v, want %s alone", slices.Sorted(maps.Keys(members)), member)
	}
	if described.Harness != executor.QwenCodeName || described.Format != executor.QwenArchiveFormat ||
		described.SessionID != ran.SessionID {
		t.Errorf("the capture is described as %+v, want session %s of Qwen Code", described, ran.SessionID)
	}

	second, b := qwenBox(t, standIn)
	_, res := runToEnd(t, second, b, sandbox.RunRequest{Brief: "carry on", Resume: ran.SessionID,
		Carried: &sandbox.CarriedSession{Archive: described, Body: bytes.NewReader(raw)}})
	if mode := strings.TrimSpace(boxFile(t, b, b.Home()+"/mode")); mode != "load" {
		t.Fatalf("the helper was told %q, want load\n%s", mode, boxFile(t, b, paths(b).Err()))
	}
	if !res.Continued || res.SessionID != ran.SessionID || res.FellBack != "" {
		t.Errorf("the run reports continued %v in session %q (%q), want %s continued", res.Continued,
			res.SessionID, res.FellBack, ran.SessionID)
	}
	cwdB := strings.TrimSpace(boxFile(t, b, b.Home()+"/cwd"))
	if qwenKey(cwdB) == keyA {
		t.Fatalf("both boxes run in a directory keyed %s, so the test proves no move", keyA)
	}
	keys, err := os.ReadDir(paths(b).QwenDir() + "/projects")
	if err != nil || len(keys) != 1 || keys[0].Name() != qwenKey(cwdB) {
		t.Errorf("the box's sessions lie under %v (%v), want %s alone", keys, err, qwenKey(cwdB))
	}
	got := boxFile(t, b, paths(b).QwenDir()+"/projects/"+qwenKey(cwdB)+"/chats/"+ran.SessionID+".jsonl")
	head, _, _ := strings.Cut(got, "\n")
	if !strings.Contains(head, `"cwd":"`+cwdB+`"`) ||
		strings.Count(got, "\n") != strings.Count(members[member], "\n")+1 {
		t.Errorf("the loaded session holds %d lines, first %s; want the carried %d and the resumed run's, "+
			"its cwd %s", strings.Count(got, "\n"), head, strings.Count(members[member], "\n"), cwdB)
	}
	if left := boxFile(t, b, paths(b).SessionCarried()); left != "" {
		t.Errorf("the carried archive was left in the box after its restore (%d bytes)", len(left))
	}
}
