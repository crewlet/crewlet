package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// THE SKILL COPY: a checkout's skill is the checkout's own content, copied
// whole into the run's skills directory within one firing's bound
// ([copyRoom]), never through a link that leads out of the checkout, and
// crash-safe: its name is listed for removal (spec.Copied) before anything is
// written, the copy is built in spec.Staging, which no harness loads, its
// SKILL.md renamed last, and the whole moved into place by one rename. A
// firing killed at any point leaves the final name absent or complete, and
// nothing the next launch does not remove (its gate removes every listed name
// and the staging directory).

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
// [maxCopyFiles], the latter counting entries).
type copyRoom struct {
	bytes int64
	files int
}

func newCopyRoom() *copyRoom { return &copyRoom{bytes: maxCopyBytes, files: maxCopyFiles} }

// skillName is the name a checkout's skill is listed under: `<tag>:<skill>`,
// the skill's directory name lower-cased with every byte outside [a-z0-9_.] a
// hyphen; past [maxSkillName], its first 55 bytes, a hyphen and the first 8
// hex digits of the full name's SHA-256, so it fits, stays the same for the
// same skill and differs from every other.
func skillName(tag, dir string) string {
	name := tag + ":" + slug(dir, "_.")
	if len(name) <= maxSkillName {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	return name[:maxSkillName-9] + "-" + hex.EncodeToString(sum[:4])
}

// copyCheckoutSkills copies each skill under root's skill directories to
// spec.Skills as [skillName], never over a skill already there, its SKILL.md
// renamed ([renameSkill]), within room; a directory without a SKILL.md, one
// whose SKILL.md opens with no frontmatter (no harness lists it), one that
// leads out of the checkout, and one past room are left out, the last two
// said on stderr.
func copyCheckoutSkills(root, tag string, spec checkoutSpec, room *copyRoom, stderr io.Writer) []handedSkill {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil
	}
	var skills []handedSkill
	for _, dir := range spec.SkillDirs {
		entries, err := os.ReadDir(filepath.Join(root, dir))
		if err != nil {
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
			if _, err := os.Lstat(filepath.Join(spec.Skills, name)); err == nil {
				continue
			}
			skill, err := copySkill(src, realRoot, name, spec, room, stderr)
			if err != nil {
				if !errors.Is(err, errNoFrontmatter) {
					fmt.Fprintf(stderr, "notehelper: the skill %s is left out: %v\n", src, err)
				}
				continue
			}
			skills = append(skills, skill)
		}
	}
	return skills
}

// errNoFrontmatter is a SKILL.md that opens with no frontmatter.
var errNoFrontmatter = errors.New("its SKILL.md opens with no frontmatter")

// copyItem is one entry of a skill's copy: its path relative to the skill,
// the file or directory it is read from, and a file's size and permission bits.
type copyItem struct {
	rel, src string
	dir      bool
	size     int64
	perm     os.FileMode
}

// copySkill copies the skill directory src, of the checkout whose real root
// is realRoot, to spec.Skills/name, crash-safe (the file's header), saying on
// stderr each entry left out ([planCopy]), and is it as listed. room is
// charged only for a skill copied.
func copySkill(src, realRoot, name string, spec checkoutSpec, room *copyRoom, stderr io.Writer) (handedSkill, error) {
	real, err := filepath.EvalSymlinks(src)
	if err != nil {
		return handedSkill{}, err
	}
	if !within(realRoot, real) {
		return handedSkill{}, fmt.Errorf("it leads out of its checkout, to %s", real)
	}
	md, err := filepath.EvalSymlinks(filepath.Join(src, "SKILL.md"))
	if err != nil || !within(realRoot, md) {
		return handedSkill{}, fmt.Errorf("its SKILL.md leads out of its checkout")
	}
	raw, err := readBounded(md, maxCopyBytes)
	if err != nil {
		return handedSkill{}, err
	}
	renamed, description, ok := renameSkill(raw, name)
	if !ok {
		return handedSkill{}, errNoFrontmatter
	}
	var notes []string
	items, bytes, err := planCopy(src, realRoot, "", 0, nil, &notes)
	if err != nil {
		return handedSkill{}, err
	}
	if bytes > room.bytes || len(items) > room.files {
		return handedSkill{}, fmt.Errorf("its %d files of %d bytes pass what is left of the firing's bound "+
			"(%d files, %d bytes)", len(items), bytes, room.files, room.bytes)
	}
	if spec.Copied != "" {
		if err = appendLines(spec.Copied, []string{name}); err != nil {
			return handedSkill{}, fmt.Errorf("listing it for removal: %w", err)
		}
	}
	staging := filepath.Join(spec.Staging, name)
	final := filepath.Join(spec.Skills, name)
	if err = buildCopy(staging, items, renamed); err == nil {
		err = os.Rename(staging, final)
	}
	if err != nil {
		_ = os.RemoveAll(staging)
		return handedSkill{}, err
	}
	_ = os.Remove(spec.Staging) // emptied, so it is not left among the skills; never one holding another's copy
	room.bytes -= bytes
	room.files -= len(items)
	for _, note := range notes {
		fmt.Fprintf(stderr, "notehelper: the skill %s is copied without %s\n", src, note)
	}
	return handedSkill{name: name, description: description, path: filepath.Join(final, "SKILL.md")}, nil
}

// within reports whether path is root or below it, both real paths.
func within(root, path string) bool {
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

// planCopy is the entries of the directory dir (rel below the skill), links
// followed while they stay in the checkout, and the bytes of their files; a
// link out of the checkout, one that leads nowhere and one back into a
// directory above it are left out, each noted. A skill deeper than
// [maxCopyDepth], or holding more than one firing's bound, is refused whole.
func planCopy(dir, realRoot, rel string, depth int, on []os.FileInfo, notes *[]string) ([]copyItem, int64, error) {
	if depth > maxCopyDepth {
		return nil, 0, fmt.Errorf("it runs more than %d directories deep", maxCopyDepth)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, 0, err
	}
	on = append(on, info)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, 0, err
	}
	var items []copyItem
	var total int64
	for _, e := range entries {
		path, at := filepath.Join(dir, e.Name()), filepath.Join(rel, e.Name())
		if e.Type()&os.ModeSymlink != 0 {
			real, err := filepath.EvalSymlinks(path)
			switch {
			case err != nil:
				*notes = append(*notes, at+" leads nowhere")
				continue
			case !within(realRoot, real):
				*notes = append(*notes, at+" leads out of the checkout")
				continue
			}
		}
		fi, err := os.Stat(path)
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
			items = append(append(items, copyItem{rel: at, src: path, dir: true, perm: fi.Mode().Perm()}), sub...)
			total += n
		case fi.Mode().IsRegular():
			items = append(items, copyItem{rel: at, src: path, size: fi.Size(), perm: fi.Mode().Perm()})
			total += fi.Size()
		}
		if total > maxCopyBytes || len(items) > maxCopyFiles {
			return nil, 0, fmt.Errorf("it holds more than one firing's bound (%d files, %d bytes)", maxCopyFiles, maxCopyBytes)
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

// buildCopy writes items under staging, made afresh, each file at most its
// planned size and keeping its permission bits with its owner's read and
// write (a directory's search) added, then skill as its SKILL.md, last.
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
		if err := copyFile(it.src, dst, it.size, it.perm|0o600); err != nil {
			return err
		}
	}
	return os.WriteFile(filepath.Join(staging, "SKILL.md"), skill, 0o600)
}

// copyFile copies at most size bytes of the file src to a new dst of
// permission bits perm.
func copyFile(src, dst string, size int64, perm os.FileMode) error {
	in, err := os.Open(src) //nolint:gosec // a checkout's file the harness itself would read
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, perm) //nolint:gosec // the run's own copy
	if err != nil {
		return err
	}
	_, err = io.Copy(out, io.LimitReader(in, size))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}
