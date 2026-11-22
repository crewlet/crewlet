package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// nestedBox is a run's directories for the claude-nested adapter: a
// workspace holding a repository, both roots, and the run's own index and
// claims beside them.
type nestedBox struct {
	t                *testing.T
	work, app, index string
}

func newNestedBox(t *testing.T, files map[string]string) nestedBox {
	t.Helper()
	dir := t.TempDir()
	b := nestedBox{t: t, work: filepath.Join(dir, "workspace"), index: filepath.Join(dir, "run", "roots")}
	b.app = filepath.Join(b.work, "app")
	for name, body := range files {
		writeNested(t, filepath.Join(dir, filepath.FromSlash(name)), body)
	}
	writeNested(t, b.index, b.work+"\x00"+b.app+"\x00")
	if err := os.MkdirAll(filepath.Join(dir, "run", "seen"), 0o700); err != nil {
		t.Fatal(err)
	}
	return b
}

func writeNested(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// fire is one hook firing on a Read of file, and the context it hands.
func (b nestedBox) fire(file string) string {
	b.t.Helper()
	in, _ := json.Marshal(map[string]any{"hook_event_name": "PostToolUse", "tool_name": "Read", "cwd": b.work,
		"tool_input": map[string]string{"file_path": file}, "tool_response": map[string]string{"file_path": "/x"}})
	var stdout, stderr bytes.Buffer
	code, ok := runOnce(b.t.Context(), []string{"-adapter", "claude-nested", "-config", b.index}, os.Getenv,
		bytes.NewReader(in), &stdout, &stderr)
	if !ok || code != exitDone {
		b.t.Fatalf("the firing exited %d (one-shot %v): %s", code, ok, stderr.String())
	}
	if stdout.Len() == 0 {
		return ""
	}
	var answer claudeHookAnswer
	if err := json.Unmarshal(stdout.Bytes(), &answer); err != nil || answer.Output.Event != "PostToolUse" {
		b.t.Fatalf("the answer is no PostToolUse context (%v): %s", err, stdout.String())
	}
	return answer.Output.Context
}

// order reports the words' places in got, failing on a word not there once.
func order(t *testing.T, got string, words ...string) []int {
	t.Helper()
	var at []int
	for _, w := range words {
		if n := strings.Count(got, w); n != 1 {
			t.Errorf("%s was handed %d times, want once:\n%s", w, n, got)
		}
		at = append(at, strings.Index(got, w))
	}
	return at
}

// A NESTED FILE IS HANDED ONCE A RUN, AS A NATIVE SESSION LOADS IT: on the
// first call under a directory, each directory's CLAUDE.md,
// .claude/CLAUDE.md and CLAUDE.local.md from the outermost root down, each
// before its imports, its rules unscoped or matching (relative to that
// directory), and a root's matching rules (relative to the root, the
// workspace's for a file in its repository too); never what `--add-dir`
// handed at the start (a root's own CLAUDE.md and unscoped rules), a rule that
// does not match, or anything a second time.
//
// Mutation: drop the claim, the walk's stop at the outermost root, a file
// name, the root check, or the base a nested rule matches from, and this
// goes red.
func TestANestedFileIsHandedOnceARunAsANativeSessionLoadsIt(t *testing.T) {
	t.Parallel()
	b := newNestedBox(t, map[string]string{
		"workspace/CLAUDE.md":                     "WS-ROOT\n",
		"workspace/.claude/rules/ws-app.md":       "---\npaths: app/src/**\n---\nWS-SCOPED\n",
		"workspace/app/CLAUDE.md":                 "APP-ROOT\n",
		"workspace/app/.claude/rules/always.md":   "APP-UNSCOPED\n",
		"workspace/app/.claude/rules/src.md":      "---\npaths:\n  - \"src/**\"\n---\nAPP-SRC\n",
		"workspace/app/.claude/rules/sub/docs.md": "---\npaths: [\"docs/**\"]\n---\nAPP-DOCS\n",
		"workspace/app/src/CLAUDE.md":             "SRC-MD\n@notes.md\n",
		"workspace/app/src/notes.md":              "SRC-IMPORT\n",
		"workspace/app/src/.claude/CLAUDE.md":     "SRC-DOTCLAUDE\n",
		"workspace/app/src/CLAUDE.local.md":       "SRC-LOCAL\n",
		"workspace/app/src/.claude/rules/u.md":    "SRC-UNSCOPED\n",
		"workspace/app/src/.claude/rules/d.md":    "---\npaths: deep/*.txt\n---\nSRC-DEEP-RULE\n",
		"workspace/app/src/.claude/rules/n.md":    "---\npaths: src/**\n---\nSRC-WRONG-BASE\n",
		"workspace/app/src/deep/CLAUDE.md":        "DEEP-MD\n",
		"workspace/app/src/deep/b.txt":            "x\n",
		"workspace/app/other/CLAUDE.md":           "OTHER-MD\n",
	})
	got := b.fire(b.app + "/src/deep/b.txt")
	at := order(t, got, "SRC-MD", "SRC-IMPORT", "SRC-DOTCLAUDE", "SRC-LOCAL", "SRC-UNSCOPED", "SRC-DEEP-RULE",
		"DEEP-MD", "APP-SRC", "WS-SCOPED")
	if !slices.IsSorted(at[:5]) || at[6] < at[0] {
		t.Errorf("the files are not handed outermost first, each before its imports:\n%s", got)
	}
	for _, w := range []string{"WS-ROOT", "APP-ROOT", "APP-UNSCOPED", "APP-DOCS", "SRC-WRONG-BASE", "OTHER-MD"} {
		if strings.Contains(got, w) {
			t.Errorf("the first read was handed %s:\n%s", w, got)
		}
	}
	if !strings.Contains(got, "Contents of "+b.app+"/src/CLAUDE.md:\n\nSRC-MD\n@notes.md\n") {
		t.Errorf("a file is not named as a native session names it:\n%s", got)
	}
	if again := b.fire(b.app + "/src/deep/b.txt"); again != "" {
		t.Errorf("a second read was handed the same files again:\n%s", again)
	}
	if got := b.fire(b.app + "/other/x.go"); !strings.Contains(got, "OTHER-MD") || strings.Contains(got, "SRC-") {
		t.Errorf("a read under other/ was handed %q, want other's CLAUDE.md alone", got)
	}
}

// ONLY WHAT LIES UNDER A ROOT IS HANDED, its links resolved: a repository's
// CLAUDE.md that links out of it, or an import that climbs out, hands nothing,
// as a native session follows no import out of its project; and a file under
// no root hands nothing at all.
//
// Mutation: drop the resolved-path check, and the outside file is handed.
func TestOnlyWhatLiesUnderARootIsHanded(t *testing.T) {
	t.Parallel()
	b := newNestedBox(t, map[string]string{
		"secret.md":                 "OUTSIDE-SECRET\n",
		"workspace/app/a/CLAUDE.md": "A-MD\n@../../../secret.md\n@/etc/hostname\n",
		"workspace/app/a/x.txt":     "x\n",
		"workspace/app/b/x.txt":     "x\n",
		"elsewhere/CLAUDE.md":       "ELSEWHERE\n",
		"elsewhere/sub/CLAUDE.md":   "ELSEWHERE-SUB\n",
	})
	if err := os.Symlink(filepath.Join(filepath.Dir(b.work), "secret.md"), b.app+"/b/CLAUDE.md"); err != nil {
		t.Fatal(err)
	}
	if got := b.fire(b.app + "/a/x.txt"); !strings.Contains(got, "A-MD") || strings.Count(got, "Contents of ") != 1 {
		t.Errorf("a read under a/ was handed %q, want a's CLAUDE.md and no import out of the root", got)
	}
	if got := b.fire(b.app + "/b/x.txt"); got != "" {
		t.Errorf("a CLAUDE.md linking out of its repository was handed:\n%s", got)
	}
	if got := b.fire(filepath.Dir(b.work) + "/elsewhere/sub/x"); got != "" {
		t.Errorf("a read under no root was handed:\n%s", got)
	}
}

// IMPORTS ARE READ AS CLAUDE CODE READS THEM: `@path` at a line's start or
// after a space, a fragment cut and an escaped space kept; none in a code
// block, a code span or a comment, none of an e-mail address's `@`, none to a
// file of an extension Claude Code does not import. The frontmatter and a
// block comment are cut from what is handed.
//
// Mutation: scan code or comments, drop the extension list, or hand the file
// raw, and this goes red.
func TestImportsAreReadAsClaudeCodeReadsThem(t *testing.T) {
	t.Parallel()
	b := newNestedBox(t, map[string]string{
		"workspace/app/d/CLAUDE.md": "---\nowner: FRONT-KEY\n---\n<!-- HIDDEN-COMMENT @hidden.md -->\n" +
			"Read @one.md#part and @two\\ words.md and mail me@three.md.\n`@span.md`\n```\n@fenced.md\n```\n" +
			"@image.png\n@./four\n",
		"workspace/app/d/one.md":       "IMP-ONE\n",
		"workspace/app/d/two words.md": "IMP-TWO\n",
		"workspace/app/d/four":         "IMP-FOUR\n",
		"workspace/app/d/three.md":     "IMP-THREE\n",
		"workspace/app/d/hidden.md":    "IMP-HIDDEN\n",
		"workspace/app/d/span.md":      "IMP-SPAN\n",
		"workspace/app/d/fenced.md":    "IMP-FENCED\n",
		"workspace/app/d/image.png":    "IMP-IMAGE\n",
	})
	got := b.fire(b.app + "/d/x")
	order(t, got, "IMP-ONE", "IMP-TWO", "IMP-FOUR")
	for _, w := range []string{"IMP-THREE", "IMP-HIDDEN", "IMP-SPAN", "IMP-FENCED", "IMP-IMAGE", "FRONT-KEY",
		"HIDDEN-COMMENT"} {
		if strings.Contains(got, w) {
			t.Errorf("the read was handed %s:\n%s", w, got)
		}
	}
}

// A RULE'S `paths` ARE READ AS YAML READS THEM: a scalar, quoted or plain
// with its comment cut, a flow list and a block list; a plain value YAML
// refuses (an alias's `*`) leaves the rule unscoped, as Claude Code leaves a
// rule whose frontmatter does not parse.
//
// Mutation: read the plain `**/*.go` as a pattern, or keep a comment, and
// this goes red.
func TestARulesPathsAreReadAsYAMLReadsThem(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		front []string
		want  []string
	}{
		{[]string{"paths: src/**"}, []string{"src/**"}},
		{[]string{"paths: 'it''s/*.md'  "}, []string{"it's/*.md"}},
		{[]string{`paths: "a/**" `}, []string{"a/**"}},
		{[]string{"paths: lib/** # the library"}, []string{"lib/**"}},
		{[]string{`paths: ["a/*.go", 'b/*.go', c/**]`}, []string{"a/*.go", "b/*.go", "c/**"}},
		{[]string{"name: x", "paths:", "  - \"a/**\"", "  # a comment", "  - b/**", "other: y"},
			[]string{"a/**", "b/**"}},
		{[]string{"paths:", "- a/**", "- b/**"}, []string{"a/**", "b/**"}},
		{[]string{"paths: **/*.go"}, nil},
		{[]string{"paths:", "  - **/*.go"}, nil},
		{[]string{"description: no paths"}, nil},
	} {
		if got := pathsOf(tc.front); !slices.Equal(got, tc.want) {
			t.Errorf("%q: paths %q, want %q", tc.front, got, tc.want)
		}
	}
}
