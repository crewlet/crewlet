package main

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
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
// written, the copy is built outside the skills directory ([stagingFor]), its
// SKILL.md renamed last, and the whole moved into place by one rename that
// replaces nothing ([renameNoReplace]); one that never lands comes off the
// list. A firing killed at any point leaves the final name absent or
// complete, and nothing the next launch does not remove (its gate removes
// every listed name, the state and the sibling staging). A checkout's skills
// are planned before it is claimed, and one whose skills do not fit what is
// left waits for a firing with room ([handCheckouts]).

// stagingDir is the directory under a run's checkout state a copy is built
// in, outside every skills directory and every harness's own: Grok Build
// 1.0.92 and Hermes Agent 0.23.4 list a copy under `<skills>/.crewlet-staging`,
// and none of Codex 0.171.0, Hermes 0.23.3 and Grok 1.0.92 (nor Codex 0.172.0
// or Hermes 0.23.4) lists or loads one here (measured). The state and the
// skills directory are both under the box's one home (a host directory, or one
// bind mount in a container), so the one rename stays on one filesystem;
// where they are not, [siblingStaging].
const stagingDir = "staging"

// siblingStaging is the directory beside the skills directory's real path a
// copy is built in where the run's state is on another filesystem than the
// skills (a skills directory, or its harness's home, mounted or linked onto
// another one): outside the skills directory, so on its filesystem but where
// none of Codex 0.171.0, Grok Build 1.0.92, Hermes Agent 0.23.3 and Claude
// Code 2.1.310 lists or loads it (measured). Only a skills directory that is
// a mount point of its own is then on another filesystem than its parent:
// [otherFilesystemError].
const siblingStaging = ".crewlet-staging-skills"

// onOneFilesystem reports whether a and b are on one filesystem, by their
// devices; a test stands in for it to build in [siblingStaging].
var onOneFilesystem = func(a, b os.FileInfo) bool {
	as, aok := a.Sys().(*syscall.Stat_t)
	bs, bok := b.Sys().(*syscall.Stat_t)
	return aok && bok && as.Dev == bs.Dev
}

// stagingFor is where a firing under state builds its copies for the skills
// directory skills, made where the home has none: state/[stagingDir], or
// [siblingStaging] where the state is on another filesystem than the skills.
func stagingFor(state, skills string) string {
	inState := filepath.Join(state, stagingDir)
	if err := os.MkdirAll(skills, 0o700); err != nil {
		return inState
	}
	real, err := filepath.EvalSymlinks(skills)
	if err != nil {
		return inState
	}
	ri, rerr := os.Stat(real)
	si, serr := os.Stat(state)
	if rerr != nil || serr != nil || onOneFilesystem(ri, si) {
		return inState
	}
	return filepath.Join(filepath.Dir(real), siblingStaging)
}

// otherFilesystemError is a copy no one rename can move into place: the
// run's skills directory is on another filesystem than its staging directory
// (a skills directory that is a mount point of its own). The skill is left out,
// never copied there in parts, which a harness could list half written.
type otherFilesystemError struct{ skills, staging string }

func (e *otherFilesystemError) Error() string {
	return fmt.Sprintf("the run's skills directory %s is on another filesystem than %s, where a copy is built, "+
		"so no one rename moves it into place", e.skills, e.staging)
}

// Unwrap is the rename's own error, EXDEV.
func (e *otherFilesystemError) Unwrap() error { return syscall.EXDEV }

// renameCopy moves a built copy into place, never over an entry there
// ([renameNoReplace]); a test stands in for it to see the skills directory as
// the copy lands, or a rename across filesystems.
var renameCopy = renameNoReplace

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

// skillName is the name a checkout's skill is listed under: `<tag><sep><skill>`,
// the skill's directory name lower-cased with every byte outside [a-z0-9_.] a
// hyphen. Past [maxSkillName] it keeps that shape: the skill part cut (and the
// tag, to half the room, where the skill leaves too little), then a hyphen and
// the first 8 hex digits of the full name's SHA-256, so it fits, stays the
// same for the same skill and differs from every other.
func skillName(tag, dir string, sep skillSep) string {
	skill := slug(dir, "_.")
	name := tag + sep.String() + skill
	if len(name) <= maxSkillName {
		return name
	}
	sum := sha256.Sum256([]byte(name))
	room := maxSkillName - len(sep.String()) - len("-") - 8
	tagRoom := max(room/2, room-len(skill))
	tag = tag[:min(len(tag), tagRoom)]
	skill = skill[:min(len(skill), room-len(tag))]
	return tag + sep.String() + skill + "-" + hex.EncodeToString(sum[:4])
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

// skillDir is a directory under one of a checkout's skill directories that
// holds a SKILL.md, or a command file ([commandSkills]): its path, its name,
// which of the spec's skill directories it is under, whether it is a
// command, and, listed as Grok lists ([grokSessionSkills]), its listed name.
type skillDir struct {
	src, name, listed string
	slot              int
	command           bool
}

// checkoutSkillDirs is the skill directories under root's skill directories,
// in order, each skill directory read within the entry bound, a recursive one
// walked ([skillWalk]), a commands one read for its files ([commandSkills]),
// a gated checkout's those its gate names ([gatedSkillDirs]); one past a
// bound is left out whole, over saying why.
func checkoutSkillDirs(root string, spec checkoutSpec) (dirs []skillDir, over []string) {
	if spec.gate != nil {
		return gatedSkillDirs(spec.gate.Checkouts[root]), nil
	}
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, nil
	}
	reached := map[string]bool{}
	for i, slot := range spec.SkillDirs {
		dir := filepath.Join(root, slot.Dir)
		if slot.Recursive || slot.Commands {
			var found []skillDir
			if slot.Commands {
				found, err = commandSkills(realRoot, dir)
			} else {
				found, err = walkSkills(realRoot, dir, reached, slot.skillWalkOptions)
			}
			if err != nil {
				over = append(over, fmt.Sprintf("the skills of %s are left out: %v", dir, err))
				continue
			}
			for _, d := range found {
				d.slot = i
				dirs = append(dirs, d)
			}
			continue
		}
		entries, past, err := readDirBounded(dir)
		if err != nil {
			continue
		}
		if past {
			over = append(over, fmt.Sprintf("the skills of %s are left out: it holds more than %d entries", dir, maxCopyFiles))
			continue
		}
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), ".") {
				continue
			}
			src := filepath.Join(dir, e.Name())
			if info, err := os.Stat(src); err != nil || !info.IsDir() || !holdsSkill(src) {
				continue
			}
			dirs = append(dirs, skillDir{src: src, name: e.Name(), slot: i})
		}
	}
	return dirs, over
}

// holdsSkill reports whether the directory dir holds a SKILL.md, a regular
// file or a link to one.
func holdsSkill(dir string) bool {
	info, err := os.Stat(filepath.Join(dir, "SKILL.md"))
	return err == nil && info.Mode().IsRegular()
}

// planCheckoutSkills plans each skill under root's skill directories
// ([planSkill]) until what they read passes a fresh room's capacity, which no
// firing could copy more of, and reports whether it did: a plan is charged its
// entries and bytes, and a refused one what it read (the entries its walk
// looked at, its SKILL.md), so refused skills cost a firing no more than ones
// it could copy. A directory without a SKILL.md, one whose SKILL.md opens with
// no frontmatter (no harness lists it), and one whose name an earlier skill
// directory's skill took, as the harness's own order has it, are left out;
// one whose name the home's skills already hold, one that cannot be planned,
// and one whose name a skill of its own skill directory took (a walk reaching
// two directories of one name, which no order of the harness's decides), are
// left out said.
func planCheckoutSkills(root, tag string, spec checkoutSpec, said *skips) ([]plannedSkill, bool) {
	realRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		return nil, false
	}
	dirs, over := checkoutSkillDirs(root, spec)
	said.sayAll(over)
	grok := spec.ListedAs == listedGrok
	if grok {
		dirs = grokSessionSkills(dirs)
	}
	rule, ruled := listingRules[spec.ListedAs]
	var plans []plannedSkill
	planned := map[string]skillDir{}
	files, bytes := 0, int64(0)
	for _, d := range dirs {
		name := skillName(tag, d.name, spec.SkillSep)
		if grok { // NAMED AS GROK LISTS IT: two skills of one directory name, apart in the walk, both kept
			name = skillName(tag, d.listed, spec.SkillSep)
		}
		key := name
		if ruled {
			key = rule.key(name)
		}
		if first, ok := planned[key]; ok {
			if first.slot == d.slot || grok {
				said.say("the skill %s is left out: %s, found first, takes its name %s", d.src, first.src, key)
			}
			continue
		}
		if why := homeHolds(spec, name); why != "" {
			said.say("the skill %s is left out: %s", d.src, why)
			continue
		}
		plan, cost, err := planEntry(d, realRoot, name, grok)
		if err != nil {
			if !errors.Is(err, errNoFrontmatter) {
				said.say("the skill %s is left out: %v", d.src, err)
			}
			if files, bytes = files+cost.entries, bytes+cost.bytes; files > maxCopyFiles || bytes > maxCopyBytes {
				return plans, true
			}
			continue
		}
		if files+len(plan.items) > maxCopyFiles || bytes+plan.bytes > maxCopyBytes {
			return plans, true
		}
		files, bytes = files+len(plan.items), bytes+plan.bytes
		planned[key] = d
		plans = append(plans, plan)
	}
	return plans, false
}

// errNoFrontmatter is a SKILL.md that opens with no frontmatter.
var errNoFrontmatter = errors.New("its SKILL.md opens with no frontmatter")

// planRead is what planning a skill read: the entries its walk looked at and
// the bytes of its SKILL.md.
type planRead struct {
	entries int
	bytes   int64
}

// planSkill plans the skill directory src, of the checkout whose real root is
// realRoot, to be listed as name ([copyWalk.plan]), and is what it read,
// whether the plan was refused or not.
func planSkill(src, realRoot, name string) (plannedSkill, planRead, error) {
	return planEntry(skillDir{src: src}, realRoot, name, false)
}

// planEntry is [planSkill] for d, a skill directory or a command file, whose
// copy is its SKILL.md alone; bare, a SKILL.md or command opening with no
// frontmatter is planned with one naming it ([bareSkill]) rather than
// refused, as Grok Build lists one (measured).
func planEntry(d skillDir, realRoot, name string, bare bool) (plannedSkill, planRead, error) {
	src := d.src
	real, err := filepath.EvalSymlinks(src)
	if err != nil {
		return plannedSkill{}, planRead{}, err
	}
	if !within(realRoot, real) {
		return plannedSkill{}, planRead{}, fmt.Errorf("it leads out of its checkout, to %s", real)
	}
	md := real
	if !d.command {
		md, err = filepath.EvalSymlinks(filepath.Join(src, "SKILL.md"))
		if err != nil || !within(realRoot, md) {
			return plannedSkill{}, planRead{}, fmt.Errorf("its SKILL.md leads out of its checkout")
		}
	}
	raw, err := readBounded(md, maxCopyBytes)
	cost := planRead{bytes: int64(len(raw))}
	if err != nil {
		return plannedSkill{}, cost, err
	}
	renamed, description, ok := renameSkill(raw, name)
	if !ok && bare {
		renamed, ok = bareSkill(raw, name), true
	}
	if !ok {
		return plannedSkill{}, cost, errNoFrontmatter
	}
	p := plannedSkill{src: src, name: name, description: description, renamed: renamed}
	if d.command {
		p.bytes, cost.entries = int64(len(raw)), 1
		return p, cost, nil
	}
	walk := copyWalk{realRoot: realRoot}
	p.items, p.bytes, err = walk.plan(src, "", 0, nil)
	p.notes, cost.entries = walk.notes, walk.entries
	return p, cost, err
}

// copyPlanned copies p to spec.Skills, built in staging, crash-safe (the
// file's header), saying each entry left out of it, and is it as listed.
func copyPlanned(p plannedSkill, spec checkoutSpec, staging string, said *skips) (handedSkill, error) {
	// ONE FIRING PLANS EVERY CHECKOUT BEFORE IT COPIES ANY, so another
	// checkout's copy this firing made can take the name only now; and
	// NOTHING THE RUN DID NOT MAKE IS LISTED AS ITS COPY, as the run's end
	// removes every name listed: an entry at its place leaves it out.
	if why := homeHolds(spec, p.name); why != "" {
		return handedSkill{}, errors.New(why)
	}
	listed, key := p.name, p.name
	if rule, ok := listingRules[spec.ListedAs]; ok {
		listed, key = rule.shown(p.name), rule.key(p.name)
	}
	if spec.Copied != "" {
		if err := appendLines(spec.Copied, []string{p.name}); err != nil {
			return handedSkill{}, fmt.Errorf("listing it for removal: %w", err)
		}
	}
	built, final := filepath.Join(staging, p.name), filepath.Join(spec.Skills, p.name)
	err := buildCopy(built, p.items, p.renamed)
	if err == nil {
		err = os.MkdirAll(spec.Skills, 0o700)
	}
	if err == nil {
		err = renameCopy(built, final)
		switch {
		case errors.Is(err, syscall.EXDEV):
			err = &otherFilesystemError{skills: spec.Skills, staging: staging}
		case errors.Is(err, fs.ErrExist):
			err = errors.New("the home's skills hold " + p.name + " already")
		}
	}
	if err != nil {
		_ = os.RemoveAll(built)
		// A COPY THAT NEVER LANDED IS NOT THE RUN'S: its name comes off the
		// list, or the run's end would remove whatever is put there later.
		if spec.Copied != "" {
			if uerr := unlistCopy(spec.Copied, p.name); uerr != nil {
				err = fmt.Errorf("%w; and its name could not be taken off %s, whose entry the run's end removes: %w",
					err, spec.Copied, uerr)
			}
		}
		return handedSkill{}, err
	}
	_ = os.Remove(staging) // emptied; never one holding another firing's copy
	if spec.held != nil {
		spec.held.add(key, final)
	}
	for _, note := range p.notes {
		said.say("the skill %s is copied without %s", p.src, note)
	}
	return handedSkill{name: listed, description: p.description, path: filepath.Join(final, "SKILL.md")}, nil
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

// within reports whether path is root or below it, both real paths; a root
// of "" holds every absolute path (the home's own skills, [homeHolds]).
func within(root, path string) bool {
	return path == root || strings.HasPrefix(path, root+string(filepath.Separator))
}

// readDirBounded is dir's entries by name, at most [maxCopyFiles] read, and
// whether it holds more, so no directory costs more than the bound to list.
func readDirBounded(dir string) ([]os.DirEntry, bool, error) {
	f, err := os.Open(dir) //nolint:gosec // a checkout's directory the harness itself would read
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = f.Close() }()
	entries, err := f.ReadDir(maxCopyFiles + 1)
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, false, err
	}
	if len(entries) > maxCopyFiles {
		return nil, true, nil
	}
	slices.SortFunc(entries, func(a, b os.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return entries, false, nil
}

// copyWalk is one skill's plan as it walks the checkout whose real root is
// realRoot: the entries it left out, noted, and how many it looked at.
type copyWalk struct {
	realRoot string
	notes    []string
	entries  int
}

// plan is the entries of the directory dir (rel below the skill), links
// followed while they stay in the checkout, each file by its real path, and
// the bytes of their files; a link out of the checkout, one that leads nowhere
// and one back into a directory above it are left out, each noted. A skill
// with an entry deeper than [maxCopyDepth], or past one firing's bound, is
// refused whole, the entry and the bound named: a skill missing a part is a
// broken one.
func (w *copyWalk) plan(dir, rel string, depth int, on []os.FileInfo) ([]copyItem, int64, error) {
	if depth > maxCopyDepth {
		return nil, 0, fmt.Errorf("its entry %s runs past the bound of %d directories deep", rel, maxCopyDepth)
	}
	info, err := os.Stat(dir)
	if err != nil {
		return nil, 0, err
	}
	on = append(on, info)
	entries, over, err := readDirBounded(dir)
	if err != nil {
		return nil, 0, err
	}
	if over {
		return nil, 0, fmt.Errorf("its directory %s passes the bound of %d entries", filepath.Join(".", rel), maxCopyFiles)
	}
	var items []copyItem
	var total int64
	for _, e := range entries {
		w.entries++
		path, at := filepath.Join(dir, e.Name()), filepath.Join(rel, e.Name())
		real, err := filepath.EvalSymlinks(path)
		switch {
		case err != nil:
			w.notes = append(w.notes, at+" leads nowhere")
			continue
		case !within(w.realRoot, real):
			w.notes = append(w.notes, at+" leads out of the checkout")
			continue
		}
		fi, err := os.Stat(real)
		if err != nil {
			continue
		}
		switch {
		case fi.IsDir():
			if aboveOn(on, fi) {
				w.notes = append(w.notes, at+" leads back into a directory above it")
				continue
			}
			sub, n, err := w.plan(path, at, depth+1, on)
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

// openPlanned is the planned file info at its real path, opened without
// following a link and without waiting (a FIFO swapped in opens at once), and
// refused where it is no longer the regular file planned, so nothing swapped
// in after the plan is read through or waited on.
func openPlanned(real string, info os.FileInfo) (*os.File, error) {
	in, err := os.OpenFile(real, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	if now, statErr := in.Stat(); statErr != nil || !now.Mode().IsRegular() || !os.SameFile(now, info) {
		_ = in.Close()
		return nil, errChanged
	}
	return in, nil
}

// copyFile copies the planned file it to a new dst ([openPlanned]).
func copyFile(it copyItem, dst string) error {
	in, err := openPlanned(it.real, it.info)
	if errors.Is(err, errChanged) {
		return fmt.Errorf("%s %w", it.rel, err)
	}
	if err != nil {
		return fmt.Errorf("%s: %w", it.rel, err)
	}
	defer func() { _ = in.Close() }()
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

// unlistCopy takes name back off the list of copies at path, one that never
// landed: the list is rewritten without it and moved over the old by one
// rename, under the state's lock the firing holds.
func unlistCopy(path, name string) error {
	raw, err := os.ReadFile(path) //nolint:gosec // the run's own state
	if err != nil {
		return err
	}
	var kept strings.Builder
	for line := range strings.SplitSeq(string(raw), "\n") {
		if line != "" && line != name {
			kept.WriteString(line + "\n")
		}
	}
	tmp := path + ".unlist"
	if err = os.WriteFile(tmp, []byte(kept.String()), 0o600); err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
	}
	return err
}
