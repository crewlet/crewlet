package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"
)

// THE INSTRUCTION TREE: a slot that walks a directory of the checkout for the
// files whose names end in a suffix, each handed whole in walk order unless
// its frontmatter scopes it to the paths its harness loads it for itself
// (Copilot 1.0.114's and Claude Code 2.1.310's .claude/rules), walked and read
// as the slot's form, its harness, does. The package doc's "Checkouts below
// the working directory" has each form's rules and the measurements they match.

// instructionSlot is one slot of [checkoutSpec.Instructions]: Names, of which
// it hands the first regular file, or Tree, walked for its files. On the wire
// a slot of names is a list of strings, or {"names": [...]} with its options
// ([slotOptions]), and a tree slot an object.
type instructionSlot struct {
	Names []string
	Tree  *instructionTree
	slotOptions
}

// slotOptions is how a slot reads past what Codex and Copilot do (both false
// for them): Inside, a name's file followed through a link only while it
// stays in the checkout, as a tree's is; Once, a file an earlier slot handed,
// by its real path, not handed again (Grok Build 1.0.92 for both, measured).
type slotOptions struct {
	Inside bool `json:"inside,omitempty"`
	Once   bool `json:"once,omitempty"`
}

// instructionTree is a tree slot: Tree, the directory below the checkout's
// root; Suffix, the end of a handed file's name; Form, the harness whose walk
// and frontmatter reading it follows; Scoped, the frontmatter that leaves a
// file out, nil where none does; Once, as [slotOptions]'.
type instructionTree struct {
	Tree   string     `json:"tree"`
	Suffix string     `json:"suffix"`
	Form   treeForm   `json:"form"`
	Scoped *treeScope `json:"scoped,omitempty"`
	Once   bool       `json:"once,omitempty"`
}

// treeForm is the harness a tree slot walks and reads a file's scope as.
type treeForm string

const (
	// formCopilot is Copilot 1.0.114's walk and line reader.
	formCopilot treeForm = "copilot"
	// formClaude is Claude Code 2.1.310's walk and YAML reader.
	formClaude treeForm = "claude"
	// formGrok is Grok Build 1.0.92's read of .grok/rules and .claude/rules:
	// the files directly in it, dot files too, the suffix in any case, every
	// one (measured, rows g4r, g4x), so it takes no scope.
	formGrok treeForm = "grok"
)

// Valid reports whether f is a form the helper walks.
func (f treeForm) Valid() bool { return f == formCopilot || f == formClaude || f == formGrok }

// treeRules is how a form walks a tree and reads a file's scope (measured):
// whether the suffix matches in its own case only, dot entries are walked, a
// directory link is followed while it stays in the checkout, an empty file is
// left out, and no directory below the tree is entered (flat).
type treeRules struct {
	caseExact, dots, dirLinks, skipEmpty, flat bool
	scopedAway                                 func(raw []byte, s treeScope) bool
}

// rules is f's [treeRules].
func (f treeForm) rules() treeRules {
	switch f {
	case formClaude:
		return treeRules{caseExact: true, dots: true, dirLinks: true, skipEmpty: true, scopedAway: claudeScopedAway}
	case formGrok:
		return treeRules{dots: true, flat: true}
	}
	return treeRules{scopedAway: copilotScopedAway}
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
	switch {
	case s.Tree != nil:
		return json.Marshal(s.Tree)
	case s.Inside || s.Once:
		return json.Marshal(struct {
			Names []string `json:"names"`
			slotOptions
		}{s.Names, s.slotOptions})
	}
	return json.Marshal(s.Names)
}

// UnmarshalJSON reads a slot: an object naming "names" is a slot of names
// with its options; any other object a tree, refused where it names no
// directory below the checkout's root, no form, or a scope its form does not
// read; anything else a list of names.
func (s *instructionSlot) UnmarshalJSON(raw []byte) error {
	*s = instructionSlot{}
	if trimmed := bytes.TrimLeft(raw, " \t\r\n"); len(trimmed) == 0 || trimmed[0] != '{' {
		return json.Unmarshal(raw, &s.Names)
	}
	var names struct {
		Names *[]string `json:"names"`
		slotOptions
	}
	if err := json.Unmarshal(raw, &names); err != nil {
		return err
	}
	if names.Names != nil {
		s.Names, s.slotOptions = *names.Names, names.slotOptions
		return nil
	}
	var tree instructionTree
	if err := json.Unmarshal(raw, &tree); err != nil {
		return err
	}
	if !filepath.IsLocal(tree.Tree) {
		return fmt.Errorf("the tree slot's tree %q is no directory below a checkout's root", tree.Tree)
	}
	if !tree.Form.Valid() {
		return fmt.Errorf("the tree slot's form %q is none of %q, %q and %q", tree.Form, formCopilot, formClaude, formGrok)
	}
	if tree.Scoped != nil && tree.Form.rules().scopedAway == nil {
		return fmt.Errorf("the tree slot's form %q takes every file, and no scope", tree.Form)
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

// treeText is a tree file to hand: the file the walk found, and its bytes.
type treeText struct {
	treeFile
	raw []byte
}

// treeWalk is a tree walk's bounds left: the entries it may still see and the
// bytes its files may still hold; and the real directories it has walked, so
// each is walked once however many links lead to it.
type treeWalk struct {
	realRoot string
	suffix   string
	rules    treeRules
	entries  int
	bytes    int64
	files    []treeFile
	walked   map[string]bool
}

// treeTexts is the files of root's tree t to hand, in walk order, each read
// once, whole: [treeWalk.walk]'s, less those t's scope leaves out as its form
// reads it ([treeRules]). A tree past the walk's bounds is left out whole, and
// a file that is no longer the file walked is left out, each said.
func treeTexts(root string, t instructionTree, said *skips) []treeText {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil
	}
	dir := filepath.Join(realRoot, t.Tree)
	w := treeWalk{realRoot: realRoot, suffix: t.Suffix, rules: t.Form.rules(), entries: maxCopyFiles,
		bytes: maxCopyBytes, walked: map[string]bool{}}
	if !w.rootDir(dir) {
		return nil
	}
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
		if t.Scoped == nil || !w.rules.scopedAway(raw, *t.Scoped) {
			texts = append(texts, treeText{treeFile: f, raw: raw})
		}
	}
	return texts
}

// walk finds the files below dir, depth directories below the tree, as its
// form walks one (measured): depth first, a directory's entries by name in
// byte order, a name ending in the suffix, a file link followed while it stays
// in the checkout, and dot entries, a directory link and an empty file as
// [treeRules] says. Past [maxCopyDepth] directories deep, [maxCopyFiles]
// entries in a directory or in all, or [maxCopyBytes] in its files, it is
// refused, the bound named.
func (w *treeWalk) walk(dir string, depth int) error {
	if depth > maxCopyDepth {
		return fmt.Errorf("its directory %s runs past the bound of %d directories deep", dir, maxCopyDepth)
	}
	if w.rules.dirLinks {
		real, err := filepath.EvalSymlinks(dir)
		if err != nil {
			return err
		}
		if w.walked[real] {
			return nil
		}
		w.walked[real] = true
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
		if strings.HasPrefix(name, ".") && !w.rules.dots {
			continue
		}
		if w.entries--; w.entries < 0 {
			return fmt.Errorf("it passes the bound of %d entries at %s", maxCopyFiles, filepath.Join(dir, name))
		}
		path := filepath.Join(dir, name)
		if e.IsDir() || w.linkedDir(path, e) {
			if w.rules.flat {
				continue
			}
			if err := w.walk(path, depth+1); err != nil {
				return err
			}
			continue
		}
		if !w.named(name) {
			continue
		}
		real, err := filepath.EvalSymlinks(path)
		if err != nil || !within(w.realRoot, real) {
			continue
		}
		info, err := os.Stat(real)
		if err != nil || !info.Mode().IsRegular() || (w.rules.skipEmpty && info.Size() == 0) {
			continue
		}
		if w.bytes -= info.Size(); w.bytes < 0 {
			return fmt.Errorf("its files pass the bound of %d bytes at %s", maxCopyBytes, path)
		}
		w.files = append(w.files, treeFile{path: path, real: real, info: info})
	}
	return nil
}

// rootDir reports whether dir, the tree's own directory, is one the walk
// takes: a directory, or, for a form that follows directory links, a link to
// a directory inside the checkout, as Claude Code 2.1.310 follows a linked
// .claude/rules (measured).
func (w *treeWalk) rootDir(dir string) bool {
	if !w.rules.dirLinks {
		info, err := os.Lstat(dir)
		return err == nil && info.IsDir()
	}
	real, err := filepath.EvalSymlinks(dir)
	if err != nil || !within(w.realRoot, real) {
		return false
	}
	info, err := os.Stat(real)
	return err == nil && info.IsDir()
}

// named reports whether a file's name ends in the walk's suffix, in its own
// case where the form's rules say so, else in any.
func (w *treeWalk) named(name string) bool {
	if w.rules.caseExact {
		return strings.HasSuffix(name, w.suffix)
	}
	return strings.HasSuffix(strings.ToLower(name), strings.ToLower(w.suffix))
}

// linkedDir reports whether the entry e at path is a link the walk follows
// into a directory: the form follows directory links and it stays in the
// checkout. [treeWalk.walk] walks each real directory once, so a link to one
// above it ends rather than loops, and one to a sibling hands no file twice.
func (w *treeWalk) linkedDir(path string, e fs.DirEntry) bool {
	if !w.rules.dirLinks || e.Type()&fs.ModeSymlink == 0 {
		return false
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil || !within(w.realRoot, real) {
		return false
	}
	info, err := os.Stat(real)
	return err == nil && info.IsDir()
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

// frontmatter is raw's frontmatter lines, and whether it has one: line 1,
// after an optional BOM, is `---`, and a later `---` line closes it, either
// line LF or CRLF (Copilot 1.0.114, measured, rows F1 and i1; Claude Code
// 2.1.310 reads an unclosed one as none, measured).
func frontmatter(raw []byte) ([]string, bool) {
	lines := strings.Split(strings.TrimPrefix(string(raw), "\uFEFF"), "\n")
	if strings.TrimSuffix(lines[0], "\r") != "---" {
		return nil, false
	}
	end := slices.IndexFunc(lines[1:], func(l string) bool { return strings.TrimSuffix(l, "\r") == "---" })
	if end < 0 {
		return nil, false
	}
	return lines[1 : 1+end], true
}

// copilotScopedAway reports whether raw's frontmatter scopes it to paths,
// read as Copilot 1.0.114 reads a rules or instructions file, line by line
// (measured, rows F1 and i1): the first of s's keys with a `<key>:` line
// (case-exact, its indentation trimmed) has a value with an entry outside
// s.Whole ([scopePatterns]).
func copilotScopedAway(raw []byte, s treeScope) bool {
	front, ok := frontmatter(raw)
	if !ok {
		return false
	}
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

// claudeScopedAway reports whether raw is a file Claude Code 2.1.310 does not
// load at a session's start (measured, B2a's C4 rows): its frontmatter is not
// valid YAML, which it never loads, or the first of s's keys present in it
// (case-exact) holds patterns ([claudePatterns]) with an entry outside
// s.Whole, which it loads only on a path that entry matches.
func claudeScopedAway(raw []byte, s treeScope) bool {
	front, ok := frontmatter(raw)
	if !ok {
		return false
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(strings.Join(front, "\n")), &doc); err != nil {
		return true
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return false
	}
	fields := doc.Content[0].Content
	for _, key := range s.Keys {
		for i := 0; i+1 < len(fields); i += 2 {
			if fields[i].Value == key {
				return slices.ContainsFunc(claudePatterns(fields[i+1]),
					func(p string) bool { return !slices.Contains(s.Whole, p) })
			}
		}
	}
	return false
}

// claudePatterns is a frontmatter value as Claude Code's patterns: a
// string's, or each string's of a list, `,`-separated parts, trimmed, the
// empty ones dropped; a list's other entries are dropped (measured: `["src/**",
// 5]` scopes the rule to src/**), and a value of another kind holds none.
func claudePatterns(v *yaml.Node) []string {
	strs := []*yaml.Node{v}
	if v.Kind == yaml.SequenceNode {
		strs = v.Content
	}
	var patterns []string
	for _, n := range strs {
		if n.Kind != yaml.ScalarNode || n.Tag != "!!str" {
			continue
		}
		for _, part := range strings.Split(n.Value, ",") {
			if p := strings.TrimSpace(part); p != "" {
				patterns = append(patterns, p)
			}
		}
	}
	return patterns
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
