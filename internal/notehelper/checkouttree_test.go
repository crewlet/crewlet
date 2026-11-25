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

// qwenRules is the tree slot of Qwen Code 0.28.4's .qwen/rules, scoped as it
// reads a rule's frontmatter, as internal/sandbox/executor writes it.
func qwenRules() instructionSlot {
	return instructionSlot{Tree: &instructionTree{Tree: ".qwen/rules", Suffix: ".md", Form: formQwen,
		Scoped: &treeScope{Keys: []string{"paths"}}}}
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
// measured row of 2.1.310 and 2.1.330 (B2a's C4 rows; the mixed list, `[5]`
// and `[""]` on 2.1.310): handed where it loads at a session's start, left out
// where it loads only on a matching path or never (invalid YAML).
//
// Mutation: read the frontmatter line by line, take `Paths` for paths, read a
// number, a bool or a map as a pattern, drop a list's strings beside another
// entry, count `**/*` as matching every path, skip the `,` split or the trim,
// or hand a file whose YAML is invalid, and a row goes red.
func TestAScopedRuleIsLeftOutAsClaudeCodeReadsItsFrontmatter(t *testing.T) {
	t.Parallel()
	handed := []string{"X\n", "---\ndescription: only\n---\nX\n", "---\nglobs: src/**\n---\n",
		"---\napplyTo: \"src/**\"\n---\n", "---\nPaths: [\"src/**\"]\n---\n", "---\npaths: []\n---\n",
		"---\npaths:\n---\n", "---\npaths: \"\"\n---\n", "---\npaths: [\"\"]\n---\n", "---\npaths: 5\n---\n",
		"---\npaths: true\n---\n", "---\npaths: {a: 1}\n---\n", "---\npaths: [\"**\"]\n---\n",
		"---\npaths: \"**\"\n---\n", "---\npaths: [\"src/**\"]\nX\n",
		"---\npaths: [5]\n---\nX\n", "---\npaths: [\"\"]\n---\nX\n"}
	left := []string{"---\npaths: [\"src/**/*.ts\"]\n---\n", "---\npaths:\n  - \"src/**/*.ts\"\n---\n",
		"---\npaths: \"src/**/*.ts\"\n---\n", "---\npaths: [\"**/*\"]\n---\n", "---\npaths: [\"*\"]\n---\n",
		"---\npaths: [\"**/\"]\n---\n", "---\npaths: [\"**\", \"docs/**\"]\n---\n",
		"---\npaths: \" src/**/*.ts \"\n---\n", "---\npaths: \"src/**/*.ts, docs/**\"\n---\n",
		"---\npaths: [unclosed\n---\nX\n", "---\npaths: [\"src/**\", 5]\n---\nMIXED\n"}
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

// A LINKED RULES DIRECTORY IS WALKED AS CLAUDE CODE WALKS IT: a .claude/rules
// that is a link to a directory inside the checkout is the tree (2.1.310 loads
// its rules, measured, row linkroot of review P2-B); one leading out of the
// checkout hands nothing, not even a file it links back in.
//
// Mutation: check the tree's root without following the link, or follow it
// out of the checkout, and a row goes red.
func TestALinkedRulesDirectoryIsWalkedAsClaudeCodeWalksIt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name   string
		inside bool
	}{{"inside the checkout", true}, {"out of the checkout", false}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			root, outside := t.TempDir(), t.TempDir()
			target := outside
			if tc.inside {
				target = filepath.Join(root, "shared-rules")
			}
			writeTree(t, target, map[string]string{"root.md": "ROOTLINK\n"})
			writeTree(t, root, map[string]string{"elsewhere/back.md": "BACK\n"})
			if !tc.inside {
				if err := os.Symlink(filepath.Join(root, "elsewhere/back.md"), filepath.Join(outside, "back.md")); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.MkdirAll(filepath.Join(root, ".claude"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, filepath.Join(root, ".claude/rules")); err != nil {
				t.Fatal(err)
			}
			real, err := filepath.EvalSymlinks(target)
			if err != nil {
				t.Fatal(err)
			}
			want := ""
			if tc.inside {
				want = "@" + filepath.Join(real, "root.md")
			}
			var said bytes.Buffer
			if got := rootImports(root, []instructionSlot{claudeRules()}, &skips{stderr: &said}); got != want {
				t.Errorf("the linked tree imports %q, want %q (saying %q)", got, want, said.String())
			}
		})
	}
}

// A TREE HANDS ITS FILES IN QWEN CODE'S WALK, each a measured row of 0.28.4's
// start in a checkout (B2b's rules rows): `.md` in its own case (`UP.MD`,
// `a.txt` not), dot files and dot directories walked, nested directories
// walked, a file link and a directory link never followed, whatever they lead
// to, a scoped rule left out, and each rule as Qwen Code loads it: its body,
// the frontmatter and HTML comments dropped, trimmed, none where that is
// empty; apart by a blank line.
//
// Mutation: match the suffix in any case, skip dot entries, follow a file or
// a directory link, drop the scope, hand the frontmatter or a comment, or
// hand a rule with no body, and the text goes red.
func TestATreeHandsItsFilesInQwenCodesWalk(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"QWEN.md":                   "ROOT\n",
		".qwen/rules/plain.md":      "PLAIN\n",
		".qwen/rules/UP.MD":         "UPPER\n",
		".qwen/rules/a.txt":         "TXT\n",
		".qwen/rules/.dot.md":       "DOTFILE\n",
		".qwen/rules/.hid/in.md":    "DOTDIR\n",
		".qwen/rules/sub/nested.md": "NESTED <!-- note --> TEXT\n",
		".qwen/rules/paths.md":      "---\npaths: [\"src/**/*.ts\"]\n---\nSCOPED\n",
		".qwen/rules/desc.md":       "\uFEFF---\r\ndescription: FRONT\r\n---\r\n\r\nDESC\r\n",
		".qwen/rules/note.md":       "---\ndescription: d\n---\n<!-- only a note -->\n",
		".qwen/rules/blank.md":      "---\ndescription: d\n---\n   \n",
		"elsewhere/l.md":            "FILELINK\n",
		"elsewhere/dir/v.md":        "DIRLINK\n",
	})
	for link, target := range map[string]string{".qwen/rules/fl.md": "elsewhere/l.md", ".qwen/rules/dl": "elsewhere/dir"} {
		if err := os.Symlink(filepath.Join(root, target), filepath.Join(root, link)); err != nil {
			t.Fatal(err)
		}
	}
	var said bytes.Buffer
	text, _ := rootText(root, 1<<20, append(names([]string{"QWEN.md"}), qwenRules()), &skips{stderr: &said})
	want := strings.Join([]string{"ROOT\n", "DOTFILE", "DOTDIR", "DESC", "PLAIN", "NESTED  TEXT"}, "\n\n")
	if text != want || said.Len() > 0 {
		t.Errorf("the slots hand\n%q\nwant\n%q\nsaying %q", text, want, said.String())
	}
}

// A SCOPED RULE IS LEFT OUT AS QWEN CODE READS ITS FRONTMATTER, each case a
// measured row of 0.28.4 (B2b's rules rows): handed where it loads at start
// (no or unclosed frontmatter, no `paths`, `Paths`, a `paths` naming nothing:
// empty, a core-schema number or bool, null, a mapping, a list of empties or
// nulls, or one holding a mapping; refused YAML whose line reading holds
// none), left out where `paths` names anything (spaces, `**`, `1_000`, `0b1`,
// a date, a listed number, an alias's list, a key given twice, a line-read
// empty single-quoted string), past a BOM or with CRLF.
//
// Mutation: count `**` as every path, read `Paths`, trim, take yaml.v3's tags,
// read a null or number as a path, scope a list with a mapping, skip
// duplicateKeys, deref or the line reader, or miss a BOM or CRLF: red.
func TestAScopedRuleIsLeftOutAsQwenCodeReadsItsFrontmatter(t *testing.T) {
	t.Parallel()
	handed := []string{"X\n", "---\ndescription: d\n---\nX\n", "---\nglobs: [\"src/**\"]\n---\nX\n",
		"---\napplyTo: \"src/**\"\n---\nX\n", "---\nPaths: [\"src/**\"]\n---\nX\n", "---\npaths: []\n---\nX\n",
		"---\npaths: \"\"\n---\nX\n", "---\npaths:\n---\nX\n", "---\npaths: 5\n---\nX\n",
		"---\npaths: [\"src/**\"]\nX\n", "---\npaths: [\"\"]\n---\nX\n", "---\npaths: [null]\n---\nX\n",
		"---\npaths: [~]\n---\nX\n", "---\npaths: [[]]\n---\nX\n", "---\npaths: [{}]\n---\nX\n",
		"---\npaths: [\"src/**\", {}]\n---\nX\n", "---\ndescription: [x\n---\nX\n",
		"---\ndescription: [x\npaths: null\n---\nX\n", "---\npaths: 0o7\n---\nX\n", "---\npaths: 0x1F\n---\nX\n",
		"---\npaths: True\n---\nX\n", "---\npaths: {a: b}\n---\nX\n", "---\npaths: .inf\n---\nX\n",
		"---\npaths: !!int 5\n---\nX\n"}
	left := []string{"---\npaths: [\"src/**/*.ts\"]\n---\nX\n", "---\npaths: \"**\"\n---\nX\n",
		"---\npaths: [5]\n---\nX\n", "---\npaths: [\"src/**\", 5]\n---\nX\n", "---\npaths: [true]\n---\nX\n",
		"---\npaths: \"  \"\n---\nX\n", "---\npaths: [bad\n---\nX\n",
		"---\ndescription: [x\npaths:\n  - \"src/**\"\n---\nX\n", "\uFEFF---\npaths: [\"src/**\"]\n---\nX\n",
		"---\r\npaths: [\"src/**\"]\r\n---\r\nX\r\n", "---\npaths: 1_000\n---\nX\n", "---\npaths: 0b1\n---\nX\n",
		"---\npaths: 2024-01-01\n---\nX\n", "---\npaths: []\npaths: [\"src/**\"]\n---\nX\n",
		"---\nx: &a [\"src/**\"]\npaths: *a\n---\nX\n", "---\ndescription: [x\npaths: ''\n---\nX\n",
		"---\ndescription: [x\npaths:\n  - ''\n---\nX\n", "---\npaths: !!timestamp 2020-01-01\n---\nX\n",
		"---\npaths: !x a\n---\nX\n", "---\npaths: !!binary aGk=\n---\nX\n"}
	scope := qwenRules().Tree.Scoped
	for _, file := range handed {
		if _, ok := qwenRule([]byte(file), scope); !ok {
			t.Errorf("%q is left out, want it handed: Qwen Code loads it at a session's start", file)
		}
	}
	for _, file := range left {
		if _, ok := qwenRule([]byte(file), scope); ok {
			t.Errorf("%q is handed, want it left out: Qwen Code does not load it at a session's start", file)
		}
	}
}

// QWEN CODE'S LINE READER IS READ AS IT READS: where its YAML parser refuses a
// rule's frontmatter, a `paths` value's text decides as its parseValue reads
// it (0.28.4's source; the list, `[bad` and empty single-quoted rows
// measured): a string or a list item other than `null`, empty or `""` names a
// path, an empty single-quoted one included, and `true`, `null`, `""`, a
// number as JavaScript's Number reads one (0x, 0o and 0b included) or an
// object do not.
//
// Mutation: read a number, `true` or `""` as a string, an empty single-quoted
// one as empty, `Infinity` or `1_0` as a number, an object as a string, or a
// `null` item as a path, and a row goes red.
func TestQwenCodesLineReaderDecidesAsItReads(t *testing.T) {
	t.Parallel()
	for value, want := range map[string]bool{
		"paths: src/**": true, "paths: Infinity": true, "paths: 1_0": true, "paths: 0x1p3": true,
		`paths: "x"`: true, "paths: 'x'": true, "paths:\n  - null\n  - a": true,
		"paths: true": false, "paths: null": false, `paths: ""`: false, "paths: 5": false, "paths: -1.5e3": false,
		"paths: 0x1F": false, "paths: 0o7": false, "paths: 0b1": false, "paths:\n  a: b": false,
		"paths:\n  - null\n  - \"\"": false, "# c\npaths:": false, "paths: ''": true, "paths:\n  - ''": true,
	} {
		if got := qwenLineScoped(value, *qwenRules().Tree.Scoped); got != want {
			t.Errorf("%q names a path: %v, want %v", value, got, want)
		}
	}
}
