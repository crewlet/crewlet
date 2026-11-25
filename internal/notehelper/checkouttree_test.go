package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// copilotRules is the tree slot of Copilot 1.0.114's .claude/rules, scoped as
// its line reader scopes a file.
func copilotRules() instructionSlot {
	return instructionSlot{Tree: &instructionTree{Tree: ".claude/rules", Suffix: ".md", Form: formCopilot,
		Scoped: &treeScope{Keys: []string{"applyTo", "paths"}, Whole: []string{"**", "**/*", "*"}}}}
}

// claudeRules is the tree slot of Claude Code 2.1.310's .claude/rules, scoped
// as it reads a rule's frontmatter, as internal/sandbox/executor writes it.
func claudeRules() instructionSlot {
	return instructionSlot{Tree: &instructionTree{Tree: ".claude/rules", Suffix: ".md", Form: formClaude,
		Scoped: &treeScope{Keys: []string{"paths"}, Whole: []string{"**"}}}}
}

// writeTree writes files under root, by path relative to it.
func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// A TREE HANDS ITS FILES IN COPILOT'S WALK ORDER, after the slots before it:
// depth first, a directory's entries by name in byte order (row i1: the
// directory `a` before the file `a.md`, which a sort of whole paths
// reverses), the suffix in any case (`R.MD`), dot files and dot directories
// skipped, a file link followed while it stays in the checkout, a directory
// link never, a scoped file left out and the rest whole, frontmatter kept,
// apart by a blank line.
//
// Mutation: sort whole paths, match the suffix's case, walk dot entries,
// follow a directory link or a link out of the checkout, or drop the scope,
// and the text goes red.
func TestATreeHandsItsFilesInCopilotsWalkOrder(t *testing.T) {
	t.Parallel()
	root, outside := t.TempDir(), filepath.Join(t.TempDir(), "out.md")
	writeTree(t, root, map[string]string{
		"AGENTS.md":                 "ROOT\n",
		".claude/rules/b.md":        "B\n",
		".claude/rules/a.md":        "A\n",
		".claude/rules/a/z.md":      "AZ\n",
		".claude/rules/R.MD":        "UPPER\n",
		".claude/rules/x.txt":       "TXT\n",
		".claude/rules/.hidden.md":  "HIDDEN\n",
		".claude/rules/.dot/e.md":   "DOT\n",
		".claude/rules/sub/c.md":    "C\n",
		".claude/rules/sub/deep/d":  "NO-SUFFIX\n",
		".claude/rules/whole.md":    "---\napplyTo: \"**\"\n---\nWHOLE\n",
		".claude/rules/scoped.md":   "---\napplyTo: \"**/*.py\"\n---\nSCOPED\n",
		"shared/target.txt":         "LINKED\n",
		".claude/rules/sub/deep/.x": "",
	})
	if err := os.WriteFile(outside, []byte("OUTSIDE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{".claude/rules/rl.md": filepath.Join(root, "shared/target.txt"),
		".claude/rules/out.md": outside, ".claude/rules/dl": filepath.Join(root, ".claude/rules/sub")} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	var said bytes.Buffer
	slots := append(names([]string{"AGENTS.md"}), copilotRules())
	text, n := rootText(root, 1<<20, slots, &skips{stderr: &said})
	want := "ROOT\n\n\nUPPER\n\n\nAZ\n\n\nA\n\n\nB\n\n\nLINKED\n\n\nC\n\n\n---\napplyTo: \"**\"\n---\nWHOLE\n"
	if text != want || said.Len() > 0 {
		t.Errorf("the slots hand\n%q\nwant\n%q\nsaying %q", text, want, said.String())
	}
	if n != len("ROOT\nUPPER\nAZ\nA\nB\nLINKED\nC\n---\napplyTo: \"**\"\n---\nWHOLE\n") {
		t.Errorf("the slots took %d bytes, want each handed file's", n)
	}
	// The limit cuts the tree as it cuts a slot's file: the cut file spends it.
	if text, n = rootText(root, 12, slots, &skips{stderr: io.Discard}); text != "ROOT\n\n\nUPPER\n\n\nA" || n != 12 {
		t.Errorf("within 12 bytes the slots hand %q for %d, want AZ cut and nothing after it", text, n)
	}
}

// A TREE PAST A BOUND IS LEFT OUT WHOLE, said with the checkout's tree and
// the bound: deeper than 8 directories, or more than 1,024 entries in all,
// though every directory holds fewer; the slots beside it are handed, and a
// tree 8 directories deep is walked to its bottom.
//
// Mutation: drop the depth or the entry bound, or hand the part walked before
// the bound, and a case goes red.
func TestATreePastABoundIsLeftOutWhole(t *testing.T) {
	t.Parallel()
	deep := ".claude/rules/1/2/3/4/5/6/7/8"
	wide := map[string]string{}
	for i := range maxCopyFiles + 1 {
		wide[fmt.Sprintf(".claude/rules/d%d/f%d.md", i%3, i)] = "W\n"
	}
	cases := []struct {
		name, want, said string
		files            map[string]string
	}{
		{"8 directories deep", "ROOT\n\n\nTOP\n\n\nBOTTOM\n", "",
			map[string]string{".claude/rules/0.md": "TOP\n", deep + "/y.md": "BOTTOM\n"}},
		{"9 directories deep", "ROOT\n", "runs past the bound of 8 directories deep",
			map[string]string{".claude/rules/0.md": "TOP\n", deep + "/9/y.md": "BOTTOM\n"}},
		{"1,025 entries in all", "ROOT\n", "passes the bound of 1024 entries", wide},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			writeTree(t, root, c.files)
			writeTree(t, root, map[string]string{"AGENTS.md": "ROOT\n"})
			var said bytes.Buffer
			text, _ := rootText(root, 1<<30, append(names([]string{"AGENTS.md"}), copilotRules()), &skips{stderr: &said})
			if text != c.want {
				t.Errorf("the slots hand %q, want %q", text, c.want)
			}
			prefix := "notehelper: the instructions under " + filepath.Join(root, ".claude/rules") + " are left out: "
			if (c.said == "" && said.Len() > 0) || (c.said != "" &&
				(!strings.HasPrefix(said.String(), prefix) || !strings.Contains(said.String(), c.said))) {
				t.Errorf("the walk said %q, want %q", said.String(), c.said)
			}
		})
	}
}

// A SCOPED FILE IS LEFT OUT AS COPILOT'S LINE READER SCOPES IT, each case a
// measured row (F1, i1 on Copilot 1.0.114): left out where line 1, after a
// BOM, is `---`, a `---` line closes it, and the first case-exact key of
// applyTo, paths has an entry other than `**`, `**/*` and `*`.
//
// Mutation: parse YAML, drop the BOM or CR trim, read keys in any case, let
// paths decide beside applyTo, or inline a list holding one match-all, and a
// row goes red.
func TestAScopedFileIsLeftOutAsCopilotReadsItsFrontmatter(t *testing.T) {
	t.Parallel()
	type row struct {
		name, file string
		out        bool
	}
	var rows []row
	for _, key := range []string{"applyTo", "paths"} {
		for _, v := range []string{`"**"`, `"**/*"`, `"*"`, `""`, ``, `["**"]`, `["*"]`, `[]`, `"**,**"`} {
			rows = append(rows, row{"F1 " + key + " " + v + " inlined", "---\n" + key + ": " + v + "\n---\nX\n", false})
		}
	}
	rows = append(rows,
		row{"F1 applyTo ** beside paths **/*.py inlined", "---\napplyTo: \"**\"\npaths: \"**/*.py\"\n---\n", false},
		row{"F1 applyTo **/*.py beside paths ** left out", "---\napplyTo: \"**/*.py\"\npaths: \"**\"\n---\n", true},
		row{"F1 other keys only inlined", "---\ndescription: x\n---\nX\n", false},
		row{"F1 empty frontmatter inlined", "---\n---\nX\n", false},
		row{"F1 unclosed list left out", "---\napplyTo: [**/*.py\n---\n", true},
		row{"F1 tab-indented key left out", "---\n\tapplyTo: \"**/*.py\"\n---\n", true},
		row{"F1 BOM left out", "\uFEFF---\napplyTo: \"**/*.py\"\n---\n", true},
		row{"F1 CRLF left out", "---\r\napplyTo: \"**/*.py\"\r\n---\r\nX\r\n", true},
		row{"F1 unclosed frontmatter inlined", "---\napplyTo: \"**/*.py\"\nX\n", false},
		row{"F1 blank first line inlined", "\n---\napplyTo: \"**/*.py\"\n---\n", false},
		row{"F1 ApplyTo inlined", "---\nApplyTo: \"**/*.py\"\n---\n", false},
		row{"F1 Paths inlined", "---\nPaths: \"**/*.py\"\n---\n", false},
		row{"F1 control left out", "---\napplyTo: \"**/*.py\"\n---\n", true},
		row{"i1 list with a match-all left out", "---\napplyTo: [\"**/*.go\",\"**\"]\n---\n", true},
		row{"i1 comma value with a match-all left out", "---\napplyTo: \"**/*.go, **\"\n---\n", true},
		row{"i1 no frontmatter inlined", "X\n", false},
	)
	scope := *copilotRules().Tree.Scoped
	for _, r := range rows {
		if got := copilotScopedAway([]byte(r.file), scope); got != r.out {
			t.Errorf("%s: %q left out %v, want %v", r.name, r.file, got, r.out)
		}
	}
}

// A SLOT KEEPS ITS SHAPE ON THE WIRE: a slot of names is the list it always
// was, byte for byte, and a tree slot the object the runner wrote; a tree
// naming no directory below the checkout's root, or no form the helper walks,
// is refused.
//
// Mutation: write a slot of names as an object, take `..` as a tree, or read
// a tree with no form or an unknown one, and a check goes red.
func TestASlotKeepsItsShapeOnTheWire(t *testing.T) {
	t.Parallel()
	wire := `[["AGENTS.override.md","AGENTS.md"],{"tree":".claude/rules","suffix":".md","form":"copilot",` +
		`"scoped":{"keys":["applyTo","paths"],"whole":["**","**/*","*"]}},` +
		`{"tree":".claude/rules","suffix":".md","form":"claude","scoped":{"keys":["paths"],"whole":["**"]}}]`
	var slots []instructionSlot
	if err := json.Unmarshal([]byte(wire), &slots); err != nil {
		t.Fatal(err)
	}
	if again, err := json.Marshal(slots); err != nil || string(again) != wire {
		t.Errorf("the slots read back as %s (%v), want %s", again, err, wire)
	}
	for _, bad := range []string{`[{"tree":"../up","suffix":".md","form":"claude"}]`,
		`[{"tree":"","suffix":".md","form":"claude"}]`, `[{"tree":"/abs","suffix":".md","form":"claude"}]`,
		`[{"tree":"r","suffix":".md"}]`, `[{"tree":"r","suffix":".md","form":"Claude"}]`} {
		if err := json.Unmarshal([]byte(bad), &slots); err == nil {
			t.Errorf("the slots %s were read, want them refused", bad)
		}
	}
}

// A TREE IMPORTS ITS FILES IN CLAUDE CODE'S WALK, each a measured row of
// 2.1.310's start in a checkout (B2a's walk rows): `.md` in its own case
// (`UP.MD`, `mixed.Md` not), dot files and dot directories walked, a file or
// directory link followed while it stays in the checkout and not out of it
// (not even to a link back in), an empty file skipped, a scoped rule left
// out; a link back to a directory the walk is in ends there, and a link to a
// sibling imports its files once.
//
// Mutation: match the suffix in any case, skip dot entries, follow no
// directory link or one out of the checkout, import the empty file, or walk a
// directory twice, and the imports go red.
func TestATreeHandsItsFilesInClaudeCodesWalk(t *testing.T) {
	t.Parallel()
	root, outside := t.TempDir(), t.TempDir()
	writeTree(t, root, map[string]string{
		"CLAUDE.md":                     "ROOT\n",
		".claude/rules/plain.md":        "PLAIN\n",
		".claude/rules/UP.MD":           "UPPER\n",
		".claude/rules/mixed.Md":        "MIXED\n",
		".claude/rules/.dot.md":         "DOTFILE\n",
		".claude/rules/.hidden/in.md":   "DOTDIR\n",
		".claude/rules/notes.txt":       "TXT\n",
		".claude/rules/empty.md":        "",
		".claude/rules/a/b/c/deep.md":   "DEEP\n",
		".claude/rules/scoped.md":       "---\npaths: [\"src/**/*.ts\"]\n---\nSCOPED\n",
		".claude/rules/whole.md":        "---\npaths: \"**\"\n---\nWHOLE\n",
		"elsewhere/linked.md":           "FILELINK\n",
		"elsewhere/dir/viadir.md":       "DIRLINK\n",
		".claude/rules/zsib/sibling.md": "SIBLING\n",
	})
	writeTree(t, outside, map[string]string{"out.md": "FILE-OUT\n", "dir/outdir.md": "DIR-OUT\n"})
	if err := os.Symlink(filepath.Join(root, "elsewhere/linked.md"), filepath.Join(outside, "dir/back.md")); err != nil {
		t.Fatal(err)
	}
	for link, target := range map[string]string{
		".claude/rules/filelink.md": filepath.Join(root, "elsewhere/linked.md"),
		".claude/rules/dirlink":     filepath.Join(root, "elsewhere/dir"),
		".claude/rules/outlink.md":  filepath.Join(outside, "out.md"),
		".claude/rules/outdir":      filepath.Join(outside, "dir"),
		".claude/rules/a/b/loop":    filepath.Join(root, ".claude/rules/a"),
		".claude/rules/sib":         filepath.Join(root, ".claude/rules/zsib"),
	} {
		if err := os.Symlink(target, filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	var said bytes.Buffer
	got := rootImports(root, append(names([]string{"CLAUDE.md"}), claudeRules()), &skips{stderr: &said})
	var want []string
	for _, rel := range []string{"CLAUDE.md", ".claude/rules/.dot.md", ".claude/rules/.hidden/in.md",
		".claude/rules/a/b/c/deep.md", "elsewhere/dir/viadir.md", "elsewhere/linked.md", ".claude/rules/plain.md",
		".claude/rules/zsib/sibling.md", ".claude/rules/whole.md"} {
		want = append(want, "@"+filepath.Join(real, rel))
	}
	if got != strings.Join(want, "\n") || said.Len() > 0 {
		t.Errorf("the slots import\n%s\nwant\n%s\nsaying %q", got, strings.Join(want, "\n"), said.String())
	}
}

// A SCOPED RULE IS LEFT OUT AS CLAUDE CODE READS ITS FRONTMATTER, each case a
// measured row of 2.1.310 and 2.1.330 (B2a's C4 rows): handed where it loads
// at a session's start, left out where it loads only on a matching path or
// never (frontmatter that is not valid YAML).
//
// Mutation: read the frontmatter line by line, take `Paths` for paths, read a
// number, a bool or a map as a pattern, count `**/*` as matching every path,
// skip the `,` split or the trim, or hand a file whose YAML is invalid, and a
// row goes red.
func TestAScopedRuleIsLeftOutAsClaudeCodeReadsItsFrontmatter(t *testing.T) {
	t.Parallel()
	handed := []string{"X\n", "---\ndescription: only\n---\nX\n", "---\nglobs: src/**\n---\n",
		"---\napplyTo: \"src/**\"\n---\n", "---\nPaths: [\"src/**\"]\n---\n", "---\npaths: []\n---\n",
		"---\npaths:\n---\n", "---\npaths: \"\"\n---\n", "---\npaths: [\"\"]\n---\n", "---\npaths: 5\n---\n",
		"---\npaths: true\n---\n", "---\npaths: {a: 1}\n---\n", "---\npaths: [\"**\"]\n---\n",
		"---\npaths: \"**\"\n---\n", "---\npaths: [\"src/**\"]\nX\n"}
	left := []string{"---\npaths: [\"src/**/*.ts\"]\n---\n", "---\npaths:\n  - \"src/**/*.ts\"\n---\n",
		"---\npaths: \"src/**/*.ts\"\n---\n", "---\npaths: [\"**/*\"]\n---\n", "---\npaths: [\"*\"]\n---\n",
		"---\npaths: [\"**/\"]\n---\n", "---\npaths: [\"**\", \"docs/**\"]\n---\n",
		"---\npaths: \" src/**/*.ts \"\n---\n", "---\npaths: \"src/**/*.ts, docs/**\"\n---\n",
		"---\npaths: [unclosed\n---\nX\n"}
	scope := *claudeRules().Tree.Scoped
	for _, file := range handed {
		if claudeScopedAway([]byte(file), scope) {
			t.Errorf("%q is left out, want it handed: Claude Code loads it at a session's start", file)
		}
	}
	for _, file := range left {
		if !claudeScopedAway([]byte(file), scope) {
			t.Errorf("%q is handed, want it left out: Claude Code loads it only on a matching path, or never", file)
		}
	}
}
