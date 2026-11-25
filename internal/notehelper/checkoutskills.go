package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"strings"
)

// skillDirSlot is one of [checkoutSpec.SkillDirs]: Dir, below the checkout's
// root, its skills the directories directly in it, or, Recursive, every one
// its walk reaches ([walkSkills]), or, Commands, the command files directly
// in it ([skillWalkOptions]). On the wire a plain one is a string and any
// other {"dir": ..., "recursive": true, ...}.
type skillDirSlot struct {
	Dir       string
	Recursive bool
	skillWalkOptions
}

// skillWalkOptions is how a skill directory is read past Pi's walk (all zero
// for Codex and Pi), each as Grok Build 1.0.92 reads its skills and commands
// directories (measured): Through, a recursive walk going on below a skill;
// Every, dot directories and node_modules walked too; Depth, the deepest a
// skill lies below the directory, deeper ones not walked, nothing refused (0:
// the walk's own bound); Commands, each `*.md` file directly in it, dot files
// too, a skill of its own, its name the file's less `.md` ([commandSkills]).
type skillWalkOptions struct {
	Through  bool `json:"through,omitempty"`
	Every    bool `json:"every,omitempty"`
	Depth    int  `json:"depth,omitempty"`
	Commands bool `json:"commands,omitempty"`
}

// skillDirWire is a skill directory's object form.
type skillDirWire struct {
	Dir       string `json:"dir"`
	Recursive bool   `json:"recursive,omitempty"`
	skillWalkOptions
}

// MarshalJSON writes s as the helper reads it.
func (s skillDirSlot) MarshalJSON() ([]byte, error) {
	if !s.Recursive && s.skillWalkOptions == (skillWalkOptions{}) {
		return json.Marshal(s.Dir)
	}
	return json.Marshal(skillDirWire(s))
}

// UnmarshalJSON reads s: a string, or an object.
func (s *skillDirSlot) UnmarshalJSON(raw []byte) error {
	*s = skillDirSlot{}
	if trimmed := bytes.TrimLeft(raw, " \t\r\n"); len(trimmed) == 0 || trimmed[0] != '{' {
		return json.Unmarshal(raw, &s.Dir)
	}
	var slot skillDirWire
	if err := json.Unmarshal(raw, &slot); err != nil {
		return err
	}
	if slot.Depth < 0 {
		return fmt.Errorf("the skill directory %q has a negative depth", slot.Dir)
	}
	s.Dir, s.Recursive, s.skillWalkOptions = slot.Dir, slot.Recursive, slot.skillWalkOptions
	return nil
}

// skillWalk is a recursive skill directory's walk: the checkout's real root,
// the real paths of the skills earlier walks of the checkout reached (seen)
// and this one has (reached), the entries it may still see, and its options.
type skillWalk struct {
	realRoot      string
	seen, reached map[string]bool
	entries       int
	found         []skillDir
	opts          skillWalkOptions
}

// walkSkills is the skills below dir as Pi 1.6.0 finds them (measured, rows
// walk): a directory holding SKILL.md is a skill and ends the descent, dot
// directories and node_modules are skipped, and a skill reached twice, by
// real path in reached, is one, the first reached. A walk refused adds
// nothing to reached: its skills are copied by none, so a later link to one
// is its first. A link is followed while
// it stays in the checkout (realRoot), never back into a directory above it.
// Past [maxCopyDepth] directories deep or [maxCopyFiles] entries in a
// directory or in all it is refused, the bound named. opts walks as Grok
// Build does where set ([skillWalkOptions]).
func walkSkills(realRoot, dir string, reached map[string]bool, opts skillWalkOptions) ([]skillDir, error) {
	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, err
	case !info.IsDir():
		return nil, nil
	}
	w := skillWalk{realRoot: realRoot, seen: reached, reached: map[string]bool{}, entries: maxCopyFiles, opts: opts}
	if err = w.walk(dir, 0, []os.FileInfo{info}); err != nil {
		return nil, err
	}
	maps.Copy(reached, w.reached)
	return w.found, nil
}

// walk walks dir, depth directories below the skill directory, with the
// directories on its path in on ([walkSkills]).
func (w *skillWalk) walk(dir string, depth int, on []os.FileInfo) error {
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
		if !w.opts.Every && (strings.HasPrefix(name, ".") || name == "node_modules") {
			continue
		}
		if w.entries--; w.entries < 0 {
			return fmt.Errorf("it passes the bound of %d entries at %s", maxCopyFiles, filepath.Join(dir, name))
		}
		path := filepath.Join(dir, name)
		real, err := filepath.EvalSymlinks(path)
		if err != nil || !within(w.realRoot, real) {
			continue
		}
		info, err := os.Stat(real)
		if err != nil || !info.IsDir() || aboveOn(on, info) {
			continue
		}
		skill := holdsSkill(path)
		if skill && !w.seen[real] && !w.reached[real] {
			w.reached[real] = true
			w.found = append(w.found, skillDir{src: path, name: name})
		}
		if (!skill || w.opts.Through) && (w.opts.Depth == 0 || depth+1 < w.opts.Depth) {
			if err := w.walk(path, depth+1, append(on, info)); err != nil {
				return err
			}
		}
	}
	return nil
}

// commandSkills is the command files directly in dir, of the checkout whose
// real root is realRoot, as Grok Build 1.0.92 lists them as skills
// (measured): each entry whose name ends in `.md`, case-exact, a dot file
// too, that is a regular file or a link to one, named its name less `.md`.
// A link out of the checkout is left out, as a skill's is; past
// [maxCopyFiles] entries the directory is refused, the bound named.
func commandSkills(realRoot, dir string) ([]skillDir, error) {
	entries, over, err := readDirBounded(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, err
	case over:
		return nil, fmt.Errorf("it holds more than %d entries", maxCopyFiles)
	}
	var found []skillDir
	for _, e := range entries {
		stem, ok := strings.CutSuffix(e.Name(), ".md")
		if !ok {
			continue
		}
		path := filepath.Join(dir, e.Name())
		real, err := filepath.EvalSymlinks(path)
		if err != nil || !within(realRoot, real) {
			continue
		}
		if info, err := os.Stat(real); err == nil && info.Mode().IsRegular() {
			found = append(found, skillDir{src: path, name: stem, command: true})
		}
	}
	return found, nil
}
