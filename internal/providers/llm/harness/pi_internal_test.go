package harness

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/providers/llm"
)

// The invariants the built-in pi profile keeps, each measured against pi
// 0.85.1 and 1.4.0 through this package's own Complete (see the profile's
// comments): a seat's system prompt and a side pass's each reach the CLI as a
// file the engine wrote before it started, and the transcript reaches it on
// stdin. Driven with the SHIPPED profile, this test binary standing in for the
// CLI, so what is checked is what a call is handed.

// shippedPi is the built-in pi profile with this test binary as its CLI.
func shippedPi(t *testing.T, env map[string]string) *Provider {
	t.Helper()
	dir := t.TempDir()
	t.Cleanup(func() { forgetWorkspace(dir) })
	p, err := New(Config{
		Key: "pi", Model: "anthropic/claude-sonnet-4-5", Harness: "pi", StateDir: dir,
		Overrides: map[string]any{"binary": os.Args[0]},
		Timeout:   20 * time.Second, MaxConcurrent: 1,
		Env:  fakeChildEnv(env),
		Auth: Auth{Mode: AuthSubscription},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return p
}

// EACH SYSTEM PROMPT REACHES PI AS A FILE THAT EXISTS WHEN PI STARTS.
//
// pi reads `--system-prompt` and `--append-system-prompt` as a path when a
// file exists there — and when none does, it sends the value itself as the
// prompt, silently: exit 0, nothing on stderr. So a call that handed over a
// path before writing the file, or a path pi could not open, would give every
// seat the path's text as its whole identity, with nothing to say so; and one
// that handed over the text would put the seat's org chart and memory on
// argv. Asked from inside the child, for each kind of call: the flag is the
// channel the call's kind takes, its value is an absolute path (pi resolves a
// relative one against its own directory), the file is private, and it holds
// exactly the engine's text.
func TestPiReadsEachSystemPromptFromAFileThatExistsWhenItStarts(t *testing.T) {
	t.Parallel()
	const seat, side = "You are Agent PM at Nimbus.", "Classify the message."
	for name, tc := range map[string]struct {
		agent        bool
		system       string
		flag, absent string
		want         string
	}{
		"a seat at work": {
			agent: true, system: seat,
			flag: "--append-system-prompt", absent: "--system-prompt",
			want: harnessNote + "\n\n" + seat,
		},
		"a side pass": {
			agent: false, system: side,
			flag: "--system-prompt", absent: "--append-system-prompt",
			want: side,
		},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			p := shippedPi(t, map[string]string{"FAKE_PROMPT_FILE": tc.flag})
			got, err := ask(t, p, llm.Request{Agent: tc.agent, Messages: []llm.Message{
				{Role: llm.RoleSystem, Content: tc.system},
				{Role: llm.RoleUser, Content: "the build is red"},
			}})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if strings.HasPrefix(got.Content, "UNREADABLE") || strings.HasPrefix(got.Content, "UNSTATABLE") {
				t.Fatalf("pi was handed %s with no file behind it, which it would have sent "+
					"as the prompt itself: %s", tc.flag, got.Content)
			}
			argv, mode, body := splitFileProbe(t, got.Content)
			fields := strings.Fields(argv)
			if slices.Contains(fields, tc.absent) {
				t.Errorf("argv = %q also carries %s — one channel per call", argv, tc.absent)
			}
			if path := fileArgAfter(argv, tc.flag); !filepath.IsAbs(path) {
				t.Errorf("%s carried %q, want an absolute path", tc.flag, path)
			}
			if strings.Contains(argv, tc.system) {
				t.Errorf("the system prompt is on argv: %q", argv)
			}
			if mode != "600" {
				t.Errorf("the file's mode = %s, want 600", mode)
			}
			if body != tc.want {
				t.Errorf("pi read %q, want %q", body, tc.want)
			}
		})
	}
}

// THE TRANSCRIPT REACHES PI ON STDIN, AND NOTHING OF IT ON ARGV. pi's print
// mode reads stdin to its end as the message when stdin is not a terminal, so
// the conversation — every tool result in it — stays off /proc/<pid>/cmdline,
// where every account on the machine reads it, and out of ARG_MAX's reach.
func TestPiTakesTheTranscriptOnStdin(t *testing.T) {
	t.Parallel()
	rec := t.TempDir()
	p := shippedPi(t, map[string]string{"FAKE_ECHO_STDIN": "1", "FAKE_RECORD_ARGV": rec})
	const ask1 = "what changed in the launch plan?"
	got, err := ask(t, p, llm.Request{Agent: true, Messages: []llm.Message{
		{Role: llm.RoleSystem, Content: "You are Agent PM at Nimbus."},
		{Role: llm.RoleUser, Content: ask1},
	}})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	stdin, err := base64.StdEncoding.DecodeString(strings.TrimSpace(got.Content))
	if err != nil {
		t.Fatalf("decoding what the child read on stdin: %v (%q)", err, got.Content)
	}
	if !strings.Contains(string(stdin), ask1) {
		t.Errorf("stdin = %q, want the transcript", stdin)
	}
	argv := completionArgv(t, rec)
	for _, arg := range argv {
		if strings.Contains(arg, ask1) || strings.Contains(arg, "## user") {
			t.Errorf("argv = %q carries the transcript", argv)
		}
	}
}
