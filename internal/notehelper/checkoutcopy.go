package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
)

// THE SKILL COPY: a checkout's skill is the checkout's own content, copied
// whole into the run's skills directory within one firing's bound
// ([copyRoom]), never through a link that leads out of the checkout, and
// crash-safe: its name is listed for removal (spec.Copied) before anything is
// written, the copy is built in spec.Staging, which no harness loads, its
// SKILL.md renamed last, and the whole moved into place by one rename. A
// firing killed at any point leaves the final name absent or complete, and
// nothing the next launch does not remove (its gate removes every listed name
// and the staging directory). A checkout's skills are planned before it is
// claimed, and one whose skills do not fit what is left waits for a firing
// with room ([handCheckouts]).

// The bounds of one firing's copies (a launch is one firing): bytes, entries
// (files and directories) and depth, well past the largest skill directory
// measured among the harnesses' bundled skills (Qwen Code 0.28.4's qc-helper:
// 2.6 MB in 112 files, 3 levels below its SKILL.md; Codex 0.171.0's largest,
// imagegen, 127 KB in 12 files). Copying to the bound took 0.12 s on the
// build VM, so no copy holds a run's start or its 10 s hook.
const (
	maxCopyBytes = 32 << 20
	maxCopyFiles = 1024
	maxCopyDepth = 8
)

// maxSkillName is the longest name Codex 0.171.0 lists a skill under: a
// longer frontmatter name is refused at load, "invalid name: exceeds maximum
// length of 64 characters" (measured), as the Agent Skills specification
// bounds it.
const maxSkillName = 64

// copyRoom is what is left of one firing's bound ([maxCopyBytes],
// [maxCopyFiles], the latter counting entries), and whether any of it was
// spent.
type copyRoom struct {
	bytes int64
	files int
	used  bool
}

func newCopyRoom() *copyRoom { return &copyRoom{bytes: maxCopyBytes, files: maxCopyFiles} }

// fits reports whether plans fit what is left of r.
func (r *copyRoom) fits(plans []plannedSkill) bool {
	files, bytes := 0, int64(0)
	for _, p := range plans {
		files, bytes = files+len(p.items), bytes+p.bytes
	}
	return files <= r.files && bytes <= r.bytes
}

// take charges p to r, false where p does not fit what is left.
func (r *copyRoom) take(p plannedSkill) bool {
	if len(p.items) > r.files || p.bytes > r.bytes {
		return false
	}
	r.files, r.bytes, r.used = r.files-len(p.items), r.bytes-p.bytes, true
	return true
}

// spent reports whether nothing is left of r.
func (r *copyRoom) spent() bool { return r.files <= 0 || r.bytes <= 0 }

// skillName is the name a checkout's skill is listed under: `<tag>:<skill>`,
// the skill's directory name lower-cased with every byte outside [a-z0-9_.] a
// hyphen. Past [maxSkillName] it keeps that shape: the skill part cut (and the
// tag, to half the room, where the skill leaves too little), then a hyphen and
// the first 8 hex digits of the full name's SHA-256, so it fits, stays the
// same for the same skill and differs from every other.
func skillName(tag, dir string) string {
	skill := slug(dir, "_.")
	name := tag + ":" + skill
	if len(name) <= maxSkillName {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	room := maxSkillName - len(":") - len("-") - 8
	tagRoom := max(room/2, room-len(skill))
	tag = tag[:min(len(tag), tagRoom)]
	skill = skill[:min(len(skill), room-len(tag))]
	return tag + ":" + skill + "-" + hex.EncodeToString(sum[:4])
}

// plannedSkill is a checkout's skill as planned: its directory, the name it is
// listed under, its renamed SKILL.md and description, its entries and their
// bytes, and the entries left out of it.
type plannedSkill struct {
	src, name, description string
	renamed                []byte
	items                  []copyItem
	bytes                  int64
	notes                  []string
}

// planCheckoutSkills plans each skill under root's skill directories
// ([planSkill]); a directory without a SKILL.md, one whose SKILL.md opens with
// no frontmatter (no harness lists it), and one whose name a skill already
// holds, or an earlier skill directory's skill took, are left out, one that
// cannot be planned said on stderr.
func planCheckoutSkills(root, tag string, spec checkoutSpec, stderr io.Writer) []plannedSkill {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil
	}
	var plans []plannedSkill
	planned := map[string]bool{}
	for _, dir := range spec.SkillDirs {
		entries, over, err := readDirBounded(filepath.Join(root, dir), maxCopyFiles)
		if err != nil {
			continue
		}
		if over {
			fmt.Fprintf(stderr, "notehelper: the skills of %s are left out: it holds more than %d entries\n",
				filepath.Join(root, dir), maxCopyFiles)
			continue
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}
			src := filepath.Join(root, dir, e.Name())
			if info, err := os.Stat(src); err != nil || !info.IsDir() {
				continue
			}
			if info, err := os.Stat(filepath.Join(src, "SKILL.md")); err != nil || !info.Mode().IsRegular() {
				continue
			}
			name := skillName(tag, e.Name())
			if _, err := os.Lstat(filepath.Join(spec.Skills, name)); err == nil || planned[name] {
				continue
			}
			plan, err := planSkill(src, realRoot, name)
			if err != nil {
				if !errors.Is(err, errNoFrontmatter) {
					fmt.Fprintf(stderr, "notehelper: the skill %s is left out: %v\n", src, err)
				}
				continue
			}
			planned[name] = true
			plans = append(plans, plan)
		}
	}
	return plans
}

// errNoFrontmatter is a SKILL.md that opens with no frontmatter.
var errNoFrontmatter = errors.New("its SKILL.md opens with no frontmatter")

// planSkill plans the skill directory src, of the checkout whose real root is
// realRoot, to be listed as name ([planCopy]).
func planSkill(src, realRoot, name string) (plannedSkill, error) {
	real, err := filepath.EvalSymlinks(src)
	if err != nil {
		return plannedSkill{}, err
	}
	if !within(realRoot, real) {
		return plannedSkill{}, fmt.Errorf("it leads out of its checkout, to %s", real)
	}
	md, err := filepath.EvalSymlinks(filepath.Join(src, "SKILL.md"))
	if err != nil || !within(realRoot, md) {
		return plannedSkill{}, fmt.Errorf("its SKILL.md leads out of its checkout")
	}
	raw, err := readBounded(md, maxCopyBytes)
	if err != nil {
		return plannedSkill{}, err
	}
	renamed, description, ok := renameSkill(raw, name)
	if !ok {
		return plannedSkill{}, errNoFrontmatter
	}
	p := plannedSkill{src: src, name: name, description: description, renamed: renamed}
	p.items, p.bytes, err = planCopy(src, realRoot, "", 0, nil, &p.notes)
	return p, err
}

// copyPlanned copies p to spec.Skills, crash-safe (the file's header), saying
// on stderr each entry left out of it, and is it as listed.
func copyPlanned(p plannedSkill, spec checkoutSpec, stderr io.Writer) (handedSkill, error) {
	if spec.Copied != "" {
		if err := appendLines(spec.Copied, []string{p.name}); err != nil {
			return handedSkill{}, fmt.Errorf("listing it for removal: %w", err)
		}
	}
	staging := filepath.Join(spec.Staging, p.name)
	final := filepath.Join(spec.Skills, p.name)
	err := buildCopy(staging, p.items, p.renamed)
	if err == nil {
		err = os.Rename(staging, final)
	}
	if err != nil {
		_ = os.RemoveAll(staging)
		return handedSkill{}, err
	}
	_ = os.Remove(spec.Staging) // emptied, so it is not left among the skills; never one holding another's copy
	for _, note := range p.notes {
		fmt.Fprintf(stderr, "notehelper: the skill %s is copied without %s\n", p.src, note)
	}
	return handedSkill{name: p.name, description: p.description, path: filepath.Join(final, "SKILL.md")}, nil
}

// copyItem is one entry of a skill's copy: its path relative to the skill,
// the real path a file is read from with the file as planned, and a file's
// size and permission bits.
type copyItem struct {
	rel, real string
	dir       bool
	info      os.FileInfo
	size      int64
	perm      os.FileMode
}

// within reports whether path is root or below it, both real paths.
func within(root, path string) bool {
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

// readDirBounded is dir's entries by name, at most n read, and whether it
// holds more, so no directory costs more than the bound to list.
func readDirBounded(dir string, n int) ([]os.DirEntry, bool, error) {
	f, err := os.Open(dir) //nolint:gosec // a checkout's directory the harness itself would read
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = f.Close() }()
	entries, err := f.ReadDir(n + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, err
	}
	if len(entries) > n {
		return nil, true, nil
	}
	slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return entries, false, nil
}

// planCopy is the entries of the directory dir (rel below the skill), links
// followed while they stay in the checkout, each file by its real path, and
// the bytes of their files; a link out of the checkout, one that leads nowhere
// and one back into a directory above it are left out, each noted. A skill
// with an entry deeper than [maxCopyDepth], or past one firing's bound, is
// refused whole, the entry and the bound named: a skill missing a part is a
// broken one.
func planCopy(dir, realRoot, rel string, depth int, on []os.FileInfo, notes *[]string) ([]copyItem, int64, error) {
	if depth > maxCopyDepth {
		return nil, 0, fmt.Errorf("its entry %s runs past the bound of %d directories deep", rel, maxCopyDepth)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, 0, err
	}
	on = append(on, info)
	entries, over, err := readDirBounded(dir, maxCopyFiles)
	if err != nil {
		return nil, 0, err
	}
	if over {
		return nil, 0, fmt.Errorf("its directory %s passes the bound of %d entries", filepath.Join(".", rel), maxCopyFiles)
	}
	var items []copyItem
	var total int64
	for _, e := range entries {
		path, at := filepath.Join(dir, e.Name()), filepath.Join(rel, e.Name())
		real, err := filepath.EvalSymlinks(path)
		switch {
		case err != nil:
			*notes = append(*notes, at+" leads nowhere")
			continue
		case !within(realRoot, real):
			*notes = append(*notes, at+" leads out of the checkout")
			continue
		}
		fi, err := os.Stat(real)
		if err != nil {
			continue
		}
		switch {
		case fi.IsDir():
			if aboveOn(on, fi) {
				*notes = append(*notes, at+" leads back into a directory above it")
				continue
			}
			sub, n, err := planCopy(path, realRoot, at, depth+1, on, notes)
			if err != nil {
				return nil, 0, err
			}
			items = append(append(items, copyItem{rel: at, dir: true, perm: fi.Mode().Perm()}), sub...)
			total += n
		case fi.Mode().IsRegular():
			items = append(items, copyItem{rel: at, real: real, info: fi, size: fi.Size(), perm: fi.Mode().Perm()})
			total += fi.Size()
		}
		if len(items) > maxCopyFiles {
			return nil, 0, fmt.Errorf("its entry %s passes one firing's bound of %d entries", at, maxCopyFiles)
		}
		if total > maxCopyBytes {
			return nil, 0, fmt.Errorf("its entry %s passes one firing's bound of %d bytes", at, maxCopyBytes)
		}
	}
	return items, total, nil
}

// aboveOn reports whether fi is one of the directories on the path being
// planned.
func aboveOn(on []os.FileInfo, fi os.FileInfo) bool {
	for _, d := range on {
		if os.SameFile(d, fi) {
			return true
		}
	}
	return false
}

// buildCopy writes items under staging, made afresh, each file read whole up
// to its planned size and keeping its permission bits with its owner's read
// and write (a directory's search) added, then skill as its SKILL.md, last.
func buildCopy(staging string, items []copyItem, skill []byte) error {
	if err := os.RemoveAll(staging); err != nil {
		return err
	}
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return err
	}
	for _, it := range items {
		dst := filepath.Join(staging, it.rel)
		if it.dir {
			if err := os.Mkdir(dst, it.perm|0o700); err != nil {
				return err
			}
			continue
		}
		if it.rel == "SKILL.md" {
			continue
		}
		if err := copyFile(it, dst); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(staging, "SKILL.md"), skill, 0o600)
}

// errChanged is a planned file that is no longer the file planned: replaced,
// or turned into a link, since.
var errChanged = errors.New("changed since it was planned")

// copyFile copies the planned file it to a new dst, opened by its real path
// without following a link and refused where it is no longer the file
// planned, so nothing swapped in after the plan is read through.
func copyFile(it copyItem, dst string) error {
	in, err := os.OpenFile(it.real, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if err != nil {
		return fmt.Errorf("%s: %w", it.rel, err)
	}
	defer func() { _ = in.Close() }()
	if now, statErr := in.Stat(); statErr != nil || !os.SameFile(now, it.info) {
		return fmt.Errorf("%s %w", it.rel, errChanged)
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, it.perm|0o600) //nolint:gosec // the run's own copy
	if err != nil {
		return err
	}
	_, err = io.Copy(out, io.LimitReader(in, it.size))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}
