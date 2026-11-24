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

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/executorkeep"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
	"github.com/crewlet/crewlet/internal/shellword"
)

// geminiCarryStandIn is the run of a stand-in Gemini CLI that keeps a session
// as 0.70.0 does, from its own captures: a new session (`--session-id`) is
// session-tools.jsonl under the pinned slug, renamed to the id; a resumed one
// (`--resume`) must already lie there, and gets the capture's last response
// appended under an id of its own, as a resumed run's request. It prints
// stream-tools.jsonl, renamed, and records its argv.
func geminiCarryStandIn(t *testing.T) string {
	t.Helper()
	session, err := filepath.Abs(filepath.Join("testdata", "gemini", "0.70.0", "session-tools.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return geminiCarryStandInAppending(t, `grep '"type":"gemini"' `+shellword.Quote(session)+
		` | tail -n 1 | sed "s/881853cd-0513-4cbd-b8bd-5bc81ae51ac1/resumed-response-1/"`)
}

// geminiCarryStandInAppending is [geminiCarryStandIn] whose resumed run
// appends what the shell command appended prints, as its own responses.
func geminiCarryStandInAppending(t *testing.T, appended string) string {
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
  ` + appended + ` >> "$f";;
*) sed "s/842120d9-2536-4808-8ba9-437d34456f75/$s/" ` + shellword.Quote(session) + ` > "$d/session-2026-11-21T18-48-$i8.jsonl";;
esac
sed "s/6b8d5e13-304f-44a6-aa06-9f3ccff25600/$s/" ` + shellword.Quote(stream) + "\n"
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
// run's capture is its chat file alone, under chats/ of the pinned slug, at
// the release that wrote it. Handed to a run in another box, it is restored
// under that box's pinned slug before the harness starts and removed; the
// harness resumes it (`--resume`) and appends to it; the run reports it
// continued; its record of takes opens at the carried 19 lines, as its script
// counted them.
//
// Mutations, each red: pack the slug's directory rather than the chat file;
// restore the file anywhere but the pinned slug's chats; start
// `--session-id` on a run that continues; leave the carried archive in place;
// open the record of takes at 0.
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
	endpoint := probeBridge(t, &geminiProbeInbox{}, &probeLedger{})
	_, res := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "carry on", Resume: ran.SessionID,
		Carried:    &sandbox.CarriedSession{Archive: described, Body: bytes.NewReader(raw)},
		MCPServers: map[string]sandbox.MCPServer{"crewlet": {Transport: sandbox.TransportHTTP, URL: endpoint}},
		Steer: sandbox.SteerWiring{Server: "crewlet", Tool: mcpbridge.SteerTool,
			URL: mcpbridge.SteerURL(endpoint)}})
	argv := argvOf(t, b)
	if after(argv, "--resume") != ran.SessionID || slices.Contains(argv, "--session-id") {
		t.Fatalf("the carried session was not resumed: argv %q\n%s", argv, boxFile(t, b, paths(b).Err()))
	}
	if got := boxFile(t, b, paths(b).GeminiTakes(ran.SessionID)); got != `{"from":19}`+"\n" {
		t.Errorf("the continued run's record of takes opens %q, want at the carried session's 19 lines", got)
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

// A NEWER GEMINI CLI RELEASE RUNS ON A SESSION OF ITS OWN. A session 0.70.0
// wrote, carried into a box running a newer minor (0.71.0) or major (1.0.0),
// whose resume this build never read, is not restored: the run passes the
// start gate, starts the session minted for it (`--session-id`) and runs,
// removes the archive, and says why in a person's words, naming the one
// release a session continues on.
//
// Mutations, each red: continue on any release past the start gate; read the
// release after the gate; name the session asked for on the fallback; judge
// the minor whatever the major (1.0.0 refused at the start).
func TestANewerGeminiReleaseRunsOnASessionOfItsOwn(t *testing.T) {
	t.Parallel()
	for name, release := range map[string]string{"a newer minor": "0.71.0", "a newer major": "1.0.0"} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ran, described, raw := geminiCarried(t, geminiCapturedRelease)
			runner, b := geminiBoxAt(t, release, geminiCarryStandIn(t))
			_, res := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "carry on", Resume: ran.SessionID,
				Carried: &sandbox.CarriedSession{Archive: described, Body: bytes.NewReader(raw)}})
			argv := argvOf(t, b)
			fresh := after(argv, "--session-id")
			if fresh == "" || fresh == ran.SessionID || slices.Contains(argv, "--resume") {
				t.Fatalf("the run on %s ran argv %q, want a session of its own: %q", release, argv, res.Error)
			}
			why := "this build continues Gemini CLI sessions only on 0.70.0, and this box runs " + release
			if !res.Success || res.Continued || res.SessionID != fresh || res.FellBack != why {
				t.Errorf("the run reports success %v, continued %v in session %q because %q; want a successful "+
					"run in session %s because %q", res.Success, res.Continued, res.SessionID, res.FellBack, fresh, why)
			}
			if left := boxFile(t, b, paths(b).SessionCarried()); left != "" {
				t.Errorf("the refused archive was left in the box (%d bytes)", len(left))
			}
		})
	}
}

// A RELEASE OLDER THAN THE ONE THIS BUILD DRIVES IS REFUSED AT THE START, every
// run, carried or not: the script ends before the harness starts, and the run
// fails saying the release it found and the one it needs. One that does not
// read as a release, or prints none, is refused too, since it cannot be shown
// not to be older, and so is a pre-release of the supported one, which comes
// before it.
//
// Mutations, each red: drop the start gate; compare only the major and minor;
// let a release that does not read run; cut a pre-release's suffix off.
func TestAReleaseOlderThanTheOneThisBuildDrivesIsRefusedAtTheStart(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		box     func(t *testing.T) (*executor.Runner, sandbox.Sandbox)
		started string
		says    string
	}{
		"gemini, an older minor": {func(t *testing.T) (*executor.Runner, sandbox.Sandbox) {
			return geminiBoxAt(t, "0.69.9", geminiCarryStandIn(t))
		}, "argv", "this box runs Gemini CLI 0.69.9, and this build runs Gemini CLI 0.70.0 or newer"},
		"gemini, an older minor that sorts after it as text": {func(t *testing.T) (*executor.Runner, sandbox.Sandbox) {
			return geminiBoxAt(t, "0.9.99", geminiCarryStandIn(t))
		}, "argv", "this box runs Gemini CLI 0.9.99"},
		"gemini, a release that does not read": {func(t *testing.T) (*executor.Runner, sandbox.Sandbox) {
			return geminiBoxAt(t, "gemini nightly", geminiCarryStandIn(t))
		}, "argv", "this box runs Gemini CLI gemini nightly"},
		"gemini, no release printed": {func(t *testing.T) (*executor.Runner, sandbox.Sandbox) {
			return geminiBoxAt(t, "", geminiCarryStandIn(t))
		}, "argv", "this box runs Gemini CLI that prints no release"},
		"qwen, an older patch": {func(t *testing.T) (*executor.Runner, sandbox.Sandbox) {
			return qwenBoxAt(t, "0.28.3", qwenCarryStandIn(t))
		}, "mode", "this box runs Qwen Code 0.28.3, and this build runs Qwen Code 0.28.4 or newer"},
		"qwen, a release that does not read": {func(t *testing.T) (*executor.Runner, sandbox.Sandbox) {
			return qwenBoxAt(t, "0.28", qwenCarryStandIn(t))
		}, "mode", "this box runs Qwen Code 0.28,"},
		"gemini, a pre-release of its release": {func(t *testing.T) (*executor.Runner, sandbox.Sandbox) {
			return geminiBoxAt(t, "0.70.0-nightly", geminiCarryStandIn(t))
		}, "argv", "this box runs Gemini CLI 0.70.0-nightly, and this build runs Gemini CLI 0.70.0 or newer"},
		"qwen, a pre-release of its release": {func(t *testing.T) (*executor.Runner, sandbox.Sandbox) {
			return qwenBoxAt(t, "0.28.4-preview.1", qwenCarryStandIn(t))
		}, "mode", "this box runs Qwen Code 0.28.4-preview.1, and this build runs Qwen Code 0.28.4 or newer"},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			runner, b := tc.box(t)
			_, res := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "begin"})
			if started := boxFile(t, b, b.Home()+"/"+tc.started); started != "" {
				t.Errorf("the harness started (%q) under a release this build does not run", started)
			}
			if res.Success || !strings.Contains(res.Error+"\n"+res.Transcript, tc.says) {
				t.Errorf("the run reports success %v, %q / %q; want a failure saying %q", res.Success, res.Error,
					res.Transcript, tc.says)
			}
		})
	}
}

// qwenCarryStandIn is the run of a stand-in for the Qwen Code helper that
// keeps a session as 0.28.4 does, from its own capture: argv is the helper,
// `run`, its config, the session and the mode, and `restore` is the real
// helper's. A new session is chat-note.jsonl under the working directory's
// key, its first line's cwd that realpath; a load must find the session there,
// its cwd the realpath (the check Qwen Code makes), and gets one more record.
// It records the mode and the working directory.
func qwenCarryStandIn(t *testing.T) string {
	t.Helper()
	chat, err := filepath.Abs(filepath.Join("testdata", "qwen", "0.28.4", "chat-note.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	return `case "$2" in restore) exec ` + shellword.Quote(nodeForQwen(t)) + ` "$@";; run) ;; *) exit 2;; esac
s=$4 m=$5 w=$(pwd -P); cat > /dev/null
printf '%s\n' "$m" > "$HOME/mode"; printf '%s\n' "$w" > "$HOME/cwd"
c="$HOME/.qwen/projects/$(printf '%s' "$w" | sed 's/[^A-Za-z0-9]/-/g')/chats"; mkdir -p "$c"
if [ "$m" = new ]; then
  sed -e "s/` + qwenCapturedSession + `/$s/" -e "1s|\"cwd\":\"/work/proj\"|\"cwd\":\"$w\"|" ` + shellword.Quote(chat) + ` > "$c/$s.jsonl"
else
  head -n 1 "$c/$s.jsonl" 2> /dev/null | grep -qF "\"cwd\":\"$w\"" || { echo "no session $s recorded here" >&2; exit 1; }
  tail -n 1 "$c/$s.jsonl" >> "$c/$s.jsonl"
fi
printf '{"type":"crewlet_qwen_session","session_id":"%s"}\n' "$s"
echo '{"type":"crewlet_qwen_end","stop_reason":"end_turn"}'
`
}

// A QWEN CODE SESSION CARRIED INTO A FRESH BOX IS LOADED UNDER ITS OWN KEY.
// One run's capture is its chat file (0.28.4's chat-note.jsonl) under the
// packing box's key. Handed to a run in a box of another realpath, so another
// key, it is filed under the new key, its first line's cwd that realpath,
// which the stand-in, like Qwen Code, holds a load to; the helper is told to
// load it; the archive is removed; the run reports it continued, its start in
// the session the carried 26 lines, as its script counted them.
//
// Mutations, each red: restore under the archive's own key; leave the first
// line's cwd as carried; start a new session on a run that continues; leave
// the carried archive in place; record the run's start as 0.
func TestAQwenSessionCarriedIntoAFreshBoxIsLoadedUnderItsOwnKey(t *testing.T) {
	t.Parallel()
	chat, err := filepath.Abs(filepath.Join("testdata", "qwen", "0.28.4", "chat-note.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	carried, err := os.ReadFile(chat)
	if err != nil {
		t.Fatal(err)
	}
	standIn := qwenCarryStandIn(t)
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
	if got := boxFile(t, b, paths(b).QwenRunStart()); got != ran.SessionID+" 26\n" {
		t.Errorf("the continued run's start reads %q, want its session at the carried 26 lines", got)
	}
}

// A NEWER QWEN CODE RELEASE LOADS NO CARRIED SESSION. A session 0.28.4 wrote,
// carried into a box running 0.29.0, is not restored: the helper starts the
// session minted for it (`new`), the archive is removed, and the run says why,
// naming the one release a session continues on.
//
// Mutations, each red: continue on any release past the start gate; name the
// session asked for on the fallback.
func TestANewerQwenReleaseLoadsNoCarriedSession(t *testing.T) {
	t.Parallel()
	standIn := qwenCarryStandIn(t)
	first, a := qwenBox(t, standIn)
	_, ran := runToEnd(t, first, a, sandbox.RunRequest{Brief: "begin"})
	described, body, err := first.OpenCapture(t.Context(), a)
	if err != nil || body == nil {
		t.Fatalf("OpenCapture = %v, %v\n%s", body, err, boxFile(t, a, paths(a).Err()))
	}
	defer func() { _ = body.Close() }()
	second, b := qwenBoxAt(t, "0.29.0", standIn)
	_, res := runToEnd(t, second, b, sandbox.RunRequest{Brief: "carry on", Resume: ran.SessionID,
		Carried: &sandbox.CarriedSession{Archive: described, Body: body}})
	if mode := strings.TrimSpace(boxFile(t, b, b.Home()+"/mode")); mode != "new" {
		t.Fatalf("the helper was told %q on 0.29.0, want new\n%s", mode, boxFile(t, b, paths(b).Err()))
	}
	const why = "this build continues Qwen Code sessions only on 0.28.4, and this box runs 0.29.0"
	if !res.Success || res.Continued || res.SessionID == ran.SessionID || res.FellBack != why {
		t.Errorf("the run reports success %v, continued %v in session %q because %q; want a successful run "+
			"in a session of its own because %q", res.Success, res.Continued, res.SessionID, res.FellBack, why)
	}
	if left := boxFile(t, b, paths(b).SessionCarried()); left != "" {
		t.Errorf("the refused archive was left in the box (%d bytes)", len(left))
	}
}

// A CONTINUED GEMINI RUN IS CHARGED ITS OWN REQUESTS, through its script: the
// responses the carried session holds (three, 0.70.0's session-tools.jsonl)
// are listed before the harness starts, and the resumed run's two, of tokens
// of their own ([geminiOwnResponses]), are its requests, their tool prompts
// (11 and 13) moved to its input: 3,024 in (1,200 cached), 237 out of the
// stream's 3,261. Where the list fails, the run is charged the stream's totals
// unsplit (3,000 and 261) on no requests read, never the whole session's sums,
// and its transcript says why.
//
// Mutations, each red: write an empty list where the list failed; sum the
// session whatever the list names; charge the file's last response alone;
// pass a failed list over unsaid.
func TestAContinuedGeminiRunIsChargedItsOwnRequestsThroughItsScript(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		failList                      bool
		input, output, requests, note int
	}{
		"its list read":   {false, 3024, 237, 2, 0},
		"its list failed": {true, 3000, 261, 0, 1},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			own := filepath.Join(t.TempDir(), "own.jsonl")
			if err := os.WriteFile(own, []byte(geminiOwnResponses(t)), 0o600); err != nil {
				t.Fatal(err)
			}
			ran, described, raw := geminiCarried(t, geminiCapturedRelease)
			runner, b := geminiBoxAt(t, geminiCapturedRelease, geminiCarryStandInAppending(t, "cat "+shellword.Quote(own)))
			if tc.failList {
				writeBoxStandIn(t, b, paths(b).BinDir()+"/node", "#!/bin/sh\ncase \"$2\" in ids) exit 3;; esac\nexec "+
					shellword.Quote(geminiNode(t))+` "$@"`+"\n")
			}
			_, res := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "carry on", Resume: ran.SessionID,
				Carried: &sandbox.CarriedSession{Archive: described, Body: bytes.NewReader(raw)}})
			if !res.Continued {
				t.Fatalf("the run did not continue its session:\n%s", boxFile(t, b, paths(b).Err()))
			}
			note := strings.Count(res.Transcript, "was not read off its chat file")
			if res.InputTokens != tc.input || res.CacheReadTokens != 1200 || res.OutputTokens != tc.output ||
				res.Turns != tc.requests || note != tc.note {
				t.Errorf("the run is charged in %d (cached %d), out %d over %d requests, its transcript saying "+
					"the list failed %d times; want %d (1200), %d over %d, %d", res.InputTokens,
					res.CacheReadTokens, res.OutputTokens, res.Turns, note, tc.input, tc.output, tc.requests, tc.note)
			}
		})
	}
}

// A NEWER RELEASE RUNS ITS BOX'S OWN COPY OF A SESSION FRESH, as it runs a
// carried one: a box on Gemini CLI 0.71.0 or Qwen Code 0.29.0 that holds the
// session it is asked to continue, its own run's, starts the session minted
// for it instead and says why, naming the one release a session continues on.
//
// Mutation, red: hold only a carried archive to the release, so the box's own
// copy is resumed (`--resume`, `load`).
func TestANewerReleaseRunsItsBoxsOwnCopyOfASessionFresh(t *testing.T) {
	t.Parallel()
	t.Run("gemini", func(t *testing.T) {
		t.Parallel()
		runner, b := geminiBoxAt(t, "0.71.0", geminiCarryStandIn(t))
		_, ran := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "begin"})
		_, res := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "carry on", Resume: ran.SessionID})
		argv := argvOf(t, b)
		fresh := after(argv, "--session-id")
		const why = "this build continues Gemini CLI sessions only on 0.70.0, and this box runs 0.71.0"
		if fresh == "" || fresh == ran.SessionID || slices.Contains(argv, "--resume") || res.Continued ||
			res.FellBack != why {
			t.Errorf("the second run ran %q, continued %v because %q; want a session of its own because %q",
				argv, res.Continued, res.FellBack, why)
		}
	})
	t.Run("qwen", func(t *testing.T) {
		t.Parallel()
		runner, b := qwenBoxAt(t, "0.29.0", qwenCarryStandIn(t))
		_, ran := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "begin"})
		_, res := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "carry on", Resume: ran.SessionID})
		mode := strings.TrimSpace(boxFile(t, b, b.Home()+"/mode"))
		const why = "this build continues Qwen Code sessions only on 0.28.4, and this box runs 0.29.0"
		if mode != "new" || res.Continued || res.SessionID == ran.SessionID || res.FellBack != why {
			t.Errorf("the second run was told %q, continued %v in %q because %q; want a session of its own "+
				"because %q", mode, res.Continued, res.SessionID, res.FellBack, why)
		}
	})
}

// A SESSION A PRE-RELEASE WROTE IS NEVER CONTINUED. Its record names the
// release as the box printed it, 0.70.0-nightly, whose numbers are the
// supported release's but which comes before it: its capture is described as
// written by no release, and no box restores it.
//
// Mutation, red: read the release's numbers off the record, as Claude Code's
// is read, so the archive is 0.70.0's.
func TestASessionAPreReleaseWroteIsNeverContinued(t *testing.T) {
	t.Parallel()
	runner, a := geminiBoxAt(t, geminiCapturedRelease, geminiCarryStandIn(t))
	runToEnd(t, runner, a, sandbox.RunRequest{Brief: "begin"})
	record := boxFile(t, a, paths(a).SessionRecord())
	nightly := strings.Replace(record, "release "+geminiCapturedRelease+"\n", "release 0.70.0-nightly\n", 1)
	if nightly == record {
		t.Fatalf("the run's record names no release %s:\n%s", geminiCapturedRelease, record)
	}
	if err := a.WriteFile(t.Context(), paths(a).SessionRecord(), []byte(nightly)); err != nil {
		t.Fatal(err)
	}
	described, body, err := runner.OpenCapture(t.Context(), a)
	if err != nil || body == nil {
		t.Fatalf("OpenCapture = %v, %v", body, err)
	}
	_ = body.Close()
	restored := runner.RestoresUnder(described, "")
	restores, why := restored.Continues, restored.Why
	const said = "the session was written by a Gemini CLI that named no release this build reads"
	if described.Version != "" || restores || !strings.Contains(why, said) ||
		restored.Loss != executorkeep.LossReleaseFloor {
		t.Errorf("a 0.70.0-nightly session is described at %q and restores %v (%q); want no release and "+
			"refused", described.Version, restores, why)
	}
}

// A RELEASE PRINTED ON A CRLF LINE IS THE RELEASE. A box whose `--version`
// ends its line with a carriage return runs as the release it names, not a
// suffixed one the start gate refuses, and its session is recorded as written
// by that release.
//
// Mutation, red: keep the carriage return in the release the script reads.
func TestAReleasePrintedOnACRLFLineIsTheRelease(t *testing.T) {
	t.Parallel()
	t.Run("gemini", func(t *testing.T) {
		t.Parallel()
		runner, b := geminiBoxAt(t, geminiCapturedRelease+"\r", geminiCarryStandIn(t))
		_, res := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "begin"})
		if !res.Success || boxFile(t, b, b.Home()+"/argv") == "" {
			t.Fatalf("a CRLF release line was not run: %q / %q", res.Error, res.Transcript)
		}
		described, body, err := runner.OpenCapture(t.Context(), b)
		if err != nil || body == nil {
			t.Fatalf("OpenCapture = %v, %v", body, err)
		}
		_ = body.Close()
		if described.Version != geminiCapturedRelease {
			t.Errorf("the session is recorded as written by %q, want %s", described.Version, geminiCapturedRelease)
		}
	})
	t.Run("qwen", func(t *testing.T) {
		t.Parallel()
		runner, b := qwenBoxAt(t, qwenCapturedRelease+"\r", qwenCarryStandIn(t))
		_, res := runToEnd(t, runner, b, sandbox.RunRequest{Brief: "begin"})
		if !res.Success || strings.TrimSpace(boxFile(t, b, b.Home()+"/mode")) != "new" {
			t.Errorf("a CRLF release line was not run: %q / %q", res.Error, res.Transcript)
		}
	})
}
