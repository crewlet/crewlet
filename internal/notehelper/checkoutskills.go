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
// its walk reaches ([walkSkills]). On the wire a plain one is a string and a
// recursive one {"dir": ..., "recursive": true}.
type skillDirSlot struct {
	Dir       string
	Recursive bool
}

// MarshalJSON writes s as the helper reads it.
func (s skillDirSlot) MarshalJSON() ([]byte, error) {
	if !s.Recursive {
		return json.Marshal(s.Dir)
	}
	return json.Marshal(map[string]any{"dir": s.Dir, "recursive": true})
}

// UnmarshalJSON reads s: a string, or an object.
func (s *skillDirSlot) UnmarshalJSON(raw []byte) error {
	*s = skillDirSlot{}
	if trimmed := bytes.TrimLeft(raw, " \t\r\n"); len(trimmed) == 0 || trimmed[0] != '{' {
		return json.Unmarshal(raw, &s.Dir)
	}
	var slot struct {
		Dir       string `json:"dir"`
		Recursive bool   `json:"recursive"`
	}
	if err := json.Unmarshal(raw, &slot); err != nil {
		return err
	}
	s.Dir, s.Recursive = slot.Dir, slot.Recursive
	return nil
}

// skillWalk is a recursive skill directory's walk: the checkout's real root,
// the real paths of the skills earlier walks of the checkout reached (seen)
// and this one has (reached), and the entries it may still see.
type skillWalk struct {
	realRoot      string
	seen, reached map[string]bool
	entries       int
	found         []skillDir
}

// walkSkills is the skills below dir as Pi 1.6.0 finds them (measured, rows
// walk): a directory holding SKILL.md is a skill and ends the descent, dot
// directories and node_modules are skipped, and a skill reached twice, by
// real path in reached, is one, the first reached. A walk refused adds
// nothing to reached: its skills are copied by none, so a later link to one
// is its first. A link is followed while
// it stays in the checkout (realRoot), never back into a directory above it.
// Past [maxCopyDepth] directories deep or [maxCopyFiles] entries in a
// directory or in all it is refused, the bound named.
func walkSkills(realRoot, dir string, reached map[string]bool) ([]skillDir, error) {
	info, err := os.Stat(dir)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return nil, nil
	case err != nil:
		return nil, err
	case !info.IsDir():
		return nil, nil
	}
	w := skillWalk{realRoot: realRoot, seen: reached, reached: map[string]bool{}, entries: maxCopyFiles}
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
		if strings.HasPrefix(name, ".") || name == "node_modules" {
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
		if !holdsSkill(path) {
			if err := w.walk(path, depth+1, append(on, info)); err != nil {
				return err
			}
			continue
		}
		if !w.seen[real] && !w.reached[real] {
			w.reached[real] = true
			w.found = append(w.found, skillDir{src: path, name: name})
		}
	}
	return nil
}
