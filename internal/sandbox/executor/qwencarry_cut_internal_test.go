package executor

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/prereqtest"
	"github.com/crewlet/crewlet/internal/sandbox"
)

// A NOTE QWEN CODE RECORDED WITH NO REPLY AFTER IT IS CUT BEFORE THE LOAD,
// and nothing else is. The session gate ([chatSessionGate]) runs with sh over
// 0.28.4's own chat files as the box's copy of the continued session: a
// request failed on the note and a run killed once it was recorded keep their
// first 12 lines; an answered note, a compacted one and a file with no note
// stay whole, as does a note before a record whose type does not read or a
// brief nesting the subtype. The cut runs before the start mark is counted.
//
// Mutations, each red: cut through the note; cut at the last note; let an
// assistant record keep no note; let an unread record keep none; read the
// subtype past the record's fields; cut with no note; no cut in the gate;
// count the start mark before the gate.
func TestAQwenNoteNoReplyFollowedIsCutBeforeTheLoad(t *testing.T) {
	t.Parallel()
	sh := prereqtest.OnPath(t, "sh", "the case runs the box's session gate", "install a POSIX shell")
	prereqtest.OnPath(t, "awk", "the gate's cut reads the chat file's records with awk", "install any POSIX awk")
	const session, fresh = "11111111-2222-4333-8444-0000000000c7", "11111111-2222-4333-8444-0000000000c8"
	fixture := func(name string) string {
		raw, err := os.ReadFile(filepath.Join("testdata", "qwen", "0.28.4", name))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	failed, killed := fixture("chat-note-failed.jsonl"), fixture("chat-note-killed.jsonl")
	lines := strings.SplitAfter(failed, "\n")
	head := func(s string, n int) string { return strings.Join(strings.SplitAfter(s, "\n")[:n], "") }
	// Each capture's 13th line is its note, its 12th the tool result before it.
	for name, f := range map[string]string{"failed": failed, "killed": killed} {
		if l := strings.SplitAfter(f, "\n")[12]; !strings.Contains(l, `"subtype":"mid_turn_user_message"`) {
			t.Fatalf("the %s capture's 13th line is no note: %s", name, l)
		}
	}
	note, after := lines[12], strings.Join(lines[13:], "")
	second := strings.Replace(note, "MARK-NF-START", "MARK-NG-START", 1)
	brief := strings.Replace(lines[1], `"parts":[`, `"parts":[{"x":1,"subtype":"mid_turn_user_message"},`, 1)
	if brief == lines[1] {
		t.Fatalf("the failed capture's 2nd line is no brief: %s", lines[1])
	}
	unread := `{"type":"assistant","message":{"role":"model","parts":[{"text":"another release's reply"}]}}` + "\n"
	for _, tc := range []struct{ name, held, want string }{
		{"a request that failed on the note", failed, head(failed, 12)},
		{"a run killed once the note was recorded", killed, head(killed, 12)},
		{"two notes no reply followed", head(failed, 13) + second + after, head(failed, 12)},
		{"an answered note", fixture("chat-note.jsonl"), fixture("chat-note.jsonl")},
		{"a note answered after its compaction", fixture("chat-compact.jsonl"), fixture("chat-compact.jsonl")},
		{"no note", fixture("chat-quota.jsonl"), fixture("chat-quota.jsonl")},
		{"a note before a record whose type does not read", failed + unread, failed + unread},
		{"a brief nesting the subtype", head(failed, 12) + brief + after, head(failed, 12) + brief + after},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			paths := Paths{Home: filepath.Join(dir, "home")}
			work := filepath.Join(dir, "work")
			if err := os.MkdirAll(work, 0o700); err != nil {
				t.Fatal(err)
			}
			real, err := filepath.EvalSymlinks(work)
			if err != nil {
				t.Fatal(err)
			}
			key := regexp.MustCompile(`[^A-Za-z0-9]`).ReplaceAllString(real, "-")
			file := filepath.Join(paths.QwenDir(), "projects", key, "chats", session+".jsonl")
			if err := os.MkdirAll(filepath.Dir(file), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(file, []byte(tc.held), 0o600); err != nil {
				t.Fatal(err)
			}
			script := "v=" + qwenRelease + "\n" +
				chatSessionGate(Session{ID: session, Continues: true, Fresh: fresh}, paths, qwenGate(paths, nil)) +
				`printf '%s %s' "$k" "$cf"`
			cmd := exec.CommandContext(t.Context(), sh, "-c", script) //nolint:gosec // the script under test
			cmd.Dir = work
			var stderr strings.Builder
			cmd.Stderr = &stderr
			out, err := cmd.Output()
			if err != nil {
				t.Fatalf("the gate: %v\n%s", err, stderr.String())
			}
			if string(out) != "yes "+file {
				t.Fatalf("the gate continues %q, want the box's copy %s\n%s", out, file, stderr.String())
			}
			kept, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if string(kept) != tc.want {
				t.Errorf("the gate kept %d lines (%d bytes), want %d (%d bytes)", strings.Count(string(kept), "\n"),
					len(kept), strings.Count(tc.want, "\n"), len(tc.want))
			}
		})
	}
	t.Run("before the start mark", func(t *testing.T) {
		t.Parallel()
		script := qwenScript(sandbox.RunRequest{Brief: "x", Resume: session},
			Session{ID: session, Continues: true, Fresh: fresh}, Paths{Home: "/home/crewlet"}, time.Second)
		cut, mark := strings.Index(script, qwenUnansweredCut), strings.Index(script, `n=$(wc -l < "$cf"`)
		if cut < 0 || mark < 0 || cut > mark {
			t.Errorf("the cut is at %d of the script and the start mark at %d; want the cut first", cut, mark)
		}
	})
}
