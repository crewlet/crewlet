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
	return instructionSlot{Tree: &instructionTree{Tree: ".claude/rules", Suffix: ".md",
		Scoped: &treeScope{Keys: []string{"applyTo", "paths"}, Whole: []string{"**", "**/*", "*"}}}}
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
	text, n := rootInstructions(root, 1<<20, slots, &skips{stderr: &said})
	want := "ROOT\n\n\nUPPER\n\n\nAZ\n\n\nA\n\n\nB\n\n\nLINKED\n\n\nC\n\n\n---\napplyTo: \"**\"\n---\nWHOLE\n"
	if text != want || said.Len() > 0 {
		t.Errorf("the slots hand\n%q\nwant\n%q\nsaying %q", text, want, said.String())
	}
	if n != len("ROOT\nUPPER\nAZ\nA\nB\nLINKED\nC\n---\napplyTo: \"**\"\n---\nWHOLE\n") {
		t.Errorf("the slots took %d bytes, want each handed file's", n)
	}
	// The limit cuts the tree as it cuts a slot's file: the cut file spends it.
	if text, n = rootInstructions(root, 12, slots, &skips{stderr: io.Discard}); text != "ROOT\n\n\nUPPER\n\n\nA" || n != 12 {
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
			text, _ := rootInstructions(root, 1<<30, append(names([]string{"AGENTS.md"}), copilotRules()), &skips{stderr: &said})
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
		if got := scopedAway([]byte(r.file), scope); got != r.out {
			t.Errorf("%s: %q left out %v, want %v", r.name, r.file, got, r.out)
		}
	}
}

// A SLOT KEEPS ITS SHAPE ON THE WIRE: a slot of names is the list it always
// was, byte for byte, and a tree slot the object the runner wrote; a tree
// naming no directory below the checkout's root is refused.
//
// Mutation: write a slot of names as an object, or take `..` as a tree, and a
// check goes red.
func TestASlotKeepsItsShapeOnTheWire(t *testing.T) {
	t.Parallel()
	wire := `[["AGENTS.override.md","AGENTS.md"],{"tree":".claude/rules","suffix":".md",` +
		`"scoped":{"keys":["applyTo","paths"],"whole":["**","**/*","*"]}}]`
	var slots []instructionSlot
	if err := json.Unmarshal([]byte(wire), &slots); err != nil {
		t.Fatal(err)
	}
	if again, err := json.Marshal(slots); err != nil || string(again) != wire {
		t.Errorf("the slots read back as %s (%v), want %s", again, err, wire)
	}
	for _, bad := range []string{`[{"tree":"../up","suffix":".md"}]`, `[{"tree":"","suffix":".md"}]`,
		`[{"tree":"/abs","suffix":".md"}]`} {
		if err := json.Unmarshal([]byte(bad), &slots); err == nil {
			t.Errorf("the slots %s were read, want them refused", bad)
		}
	}
}
