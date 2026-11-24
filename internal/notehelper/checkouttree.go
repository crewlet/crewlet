package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// THE INSTRUCTION TREE: a slot that walks a directory of the checkout for the
// files whose names end in a suffix, each handed whole in walk order unless
// its frontmatter scopes it to the paths its harness loads it for itself
// (Copilot 1.0.114's .claude/rules). The package doc's "Checkouts below the
// working directory" has the walk's rules and the measurements they match.

// instructionSlot is one slot of [checkoutSpec.Instructions]: Names, of which
// it hands the first regular file, or Tree, walked for its files. On the wire
// a slot of names is a list of strings and a tree slot an object.
type instructionSlot struct {
	Names []string
	Tree  *instructionTree
}

// instructionTree is a tree slot: Tree, the directory below the checkout's
// root; Suffix, the end of a handed file's name, in any case; Scoped, the
// frontmatter that leaves a file out, nil where none does.
type instructionTree struct {
	Tree   string     `json:"tree"`
	Suffix string     `json:"suffix"`
	Scoped *treeScope `json:"scoped,omitempty"`
}

// treeScope is the frontmatter keys that scope a file to paths, the first
// present deciding, and the patterns that match every path.
type treeScope struct {
	Keys  []string `json:"keys"`
	Whole []string `json:"whole"`
}

// MarshalJSON writes a slot as the helper reads it: its names as a list, a
// tree as an object.
func (s instructionSlot) MarshalJSON() ([]byte, error) {
	if s.Tree != nil {
		return json.Marshal(s.Tree)
	}
	return json.Marshal(s.Names)
}

// UnmarshalJSON reads a slot: an object is a tree, refused where it names no
// directory below the checkout's root; anything else a list of names.
func (s *instructionSlot) UnmarshalJSON(raw []byte) error {
	*s = instructionSlot{}
	if trimmed := bytes.TrimLeft(raw, " \t\r\n"); len(trimmed) == 0 || trimmed[0] != '{' {
		return json.Unmarshal(raw, &s.Names)
	}
	var tree instructionTree
	if err := json.Unmarshal(raw, &tree); err != nil {
		return err
	}
	if !filepath.IsLocal(tree.Tree) {
		return fmt.Errorf("the tree slot's tree %q is no directory below a checkout's root", tree.Tree)
	}
	s.Tree = &tree
	return nil
}

// treeFile is a file a tree walk found: its path, its real path, and the file
// as the walk saw it.
type treeFile struct {
	path, real string
	info       os.FileInfo
}

// treeText is a tree file to hand: its path and its bytes.
type treeText struct {
	path string
	raw  []byte
}

// treeWalk is a tree walk's bounds left: the entries it may still see and the
// bytes its files may still hold.
type treeWalk struct {
	realRoot string
	suffix   string
	entries  int
	bytes    int64
	files    []treeFile
}

// treeTexts is the files of root's tree t to hand, in walk order, each its
// path and its bytes read whole: [treeWalk.walk]'s, less those t's scope
// leaves out ([scopedAway]). A tree past the walk's bounds is left out whole,
// and a file that is no longer the file walked is left out, each said.
func treeTexts(root string, t instructionTree, said *skips) []treeText {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil
	}
	dir := filepath.Join(realRoot, t.Tree)
	info, err := os.Lstat(dir)
	if err != nil || !info.IsDir() {
		return nil
	}
	w := treeWalk{realRoot: realRoot, suffix: strings.ToLower(t.Suffix), entries: maxCopyFiles, bytes: maxCopyBytes}
	if err = w.walk(dir, 0); err != nil {
		said.say("the instructions under %s are left out: %v", filepath.Join(root, t.Tree), err)
		return nil
	}
	var texts []treeText
	for _, f := range w.files {
		raw, err := readWalked(f)
		if err != nil {
			said.say("the instruction file %s is left out: %v", f.path, err)
			continue
		}
		if t.Scoped == nil || !scopedAway(raw, *t.Scoped) {
			texts = append(texts, treeText{path: f.path, raw: raw})
		}
	}
	return texts
}

// walk finds the files below dir, depth directories below the tree, as
// Copilot 1.0.114 walks one (measured): depth first, a directory's entries by
// name in byte order, dot entries skipped, a name ending in the suffix in any
// case, a file link followed while it stays in the checkout and a directory
// link never. Past [maxCopyDepth] directories deep, [maxCopyFiles] entries in
// a directory or in all, or [maxCopyBytes] in its files, it is refused, the
// bound named.
func (w *treeWalk) walk(dir string, depth int) error {
	if depth > maxCopyDepth {
		return fmt.Errorf("its directory %s runs past the bound of %d directories deep", dir, maxCopyDepth)
	}
	entries, over, err := readDirBounded(dir)
	if err != nil {
		return err
	}
	if over {
		return fmt.Errorf("its directory %s passes the bound of %d entries", dir, maxCopyFiles)
	}
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		if w.entries--; w.entries < 0 {
			return fmt.Errorf("it passes the bound of %d entries at %s", maxCopyFiles, filepath.Join(dir, name))
		}
		path := filepath.Join(dir, name)
		if e.IsDir() {
			if err := w.walk(path, depth+1); err != nil {
				return err
			}
			continue
		}
		if !strings.HasSuffix(strings.ToLower(name), w.suffix) {
			continue
		}
		real, err := filepath.EvalSymlinks(path)
		if err != nil || !within(w.realRoot, real) {
			continue
		}
		info, err := os.Stat(real)
		if err != nil || !info.Mode().IsRegular() {
			continue
		}
		if w.bytes -= info.Size(); w.bytes < 0 {
			return fmt.Errorf("its files pass the bound of %d bytes at %s", maxCopyBytes, path)
		}
		w.files = append(w.files, treeFile{path: path, real: real, info: info})
	}
	return nil
}

// readWalked is the walked file f whole, opened as a skill's file is copied
// ([openPlanned]), so nothing swapped in since the walk is read through or
// waited on.
func readWalked(f treeFile) ([]byte, error) {
	in, err := openPlanned(f.real, f.info)
	if err != nil {
		return nil, err
	}
	defer func() { _ = in.Close() }()
	return io.ReadAll(io.LimitReader(in, f.info.Size()))
}

// scopedAway reports whether raw's frontmatter scopes it to paths, read as
// Copilot 1.0.114 reads a rules or instructions file, line by line (measured,
// rows F1 and i1): line 1, after an optional BOM, is `---`, a `---` line
// closes it (either line LF or CRLF), and between them the first of s's keys
// with a `<key>:` line (case-exact, its indentation trimmed) has a value with
// an entry outside s.Whole ([scopePatterns]).
func scopedAway(raw []byte, s treeScope) bool {
	lines := strings.Split(strings.TrimPrefix(string(raw), "\uFEFF"), "\n")
	if strings.TrimSuffix(lines[0], "\r") != "---" {
		return false
	}
	end := slices.IndexFunc(lines[1:], func(l string) bool { return strings.TrimSuffix(l, "\r") == "---" })
	if end < 0 {
		return false
	}
	front := lines[1 : 1+end]
	for _, key := range s.Keys {
		for _, l := range front {
			value, ok := strings.CutPrefix(strings.TrimLeft(strings.TrimSuffix(l, "\r"), " \t"), key+":")
			if !ok {
				continue
			}
			return slices.ContainsFunc(scopePatterns(value), func(p string) bool { return !slices.Contains(s.Whole, p) })
		}
	}
	return false
}

// scopePatterns is a scope's value as patterns: a `[a, b]` list's entries or
// a value's `,`-separated parts, each with its spaces and quotes trimmed, the
// empty ones dropped; an unclosed `[` stays in its pattern, as Copilot keeps
// it (measured: `[**/*.py` is a pattern of its own).
func scopePatterns(value string) []string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && value[0] == '[' && value[len(value)-1] == ']' {
		value = value[1 : len(value)-1]
	}
	var patterns []string
	for _, part := range strings.Split(value, ",") {
		if p := strings.Trim(strings.TrimSpace(part), `"'`); p != "" {
			patterns = append(patterns, p)
		}
	}
	return patterns
}
