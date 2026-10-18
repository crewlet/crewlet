package manifest

import (
	"fmt"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/artifacts"
	"github.com/crewlet/crewlet/internal/artifacts/mediatype"
	"github.com/crewlet/crewlet/internal/objstore"
)

// Mode is how a publish names its files.
type Mode string

const (
	// ModeWhole states the next version's complete file set: a path the
	// publish does not name is not in the version.
	ModeWhole Mode = "whole"

	// ModePatch adds, replaces and removes paths relative to the base
	// version, and keeps every path it does not name (carried, since the
	// entry names the base's object).
	ModePatch Mode = "patch"
)

// Modes are both modes.
var Modes = []Mode{ModeWhole, ModePatch}

// Valid reports whether m is a mode this build composes.
func (m Mode) Valid() bool { return slices.Contains(Modes, m) }

// Change is one path a publish names: a file to hold at the path, or, in patch
// mode, the path's removal.
type Change struct {
	Path string

	// Remove drops the path from the base version. Patch mode only.
	Remove bool

	// Entry is the file to hold at Path, its Path equal to it, when Remove is
	// false.
	Entry artifacts.Entry
}

// Next composes the next version's manifest from the base version and what a
// publish names, in canonical form. entry is the entry the publish names, or
// empty to keep the base version's.
//
// IT DECIDES NOTHING ABOUT BYTES: every entry a change carries already names
// its object (uploaded by this publish, or found held by [PlanCarry]). A path
// a patch keeps is marked carried, because it names the base's object rather
// than one this publish uploaded. The result is checked by [Validate] where
// the kind is known.
func Next(mode Mode, base artifacts.Manifest, entry string, changes []Change) (artifacts.Manifest, error) {
	if !mode.Valid() {
		return artifacts.Manifest{}, fmt.Errorf("%w: mode %q is not whole or patch",
			ErrInvalid, mode)
	}
	files := map[string]artifacts.Entry{}
	if mode == ModePatch {
		for _, e := range base.Files {
			e.Carried = true
			files[e.Path] = e
		}
	}
	named := map[string]bool{}
	for _, c := range changes {
		p, err := CleanPath(c.Path)
		if err != nil {
			return artifacts.Manifest{}, err
		}
		if named[p] {
			return artifacts.Manifest{}, fmt.Errorf("%w: %q is named twice in one "+
				"publish", ErrInvalid, p)
		}
		named[p] = true
		if c.Remove {
			if mode != ModePatch {
				return artifacts.Manifest{}, fmt.Errorf("%w: %q is removed in a whole "+
					"publish, which states the full set — leave it out instead",
					ErrInvalid, p)
			}
			if _, held := files[p]; !held {
				return artifacts.Manifest{}, fmt.Errorf("%w: %q is removed and the base "+
					"version holds no such file", ErrInvalid, p)
			}
			delete(files, p)
			continue
		}
		if c.Entry.Path != p {
			return artifacts.Manifest{}, fmt.Errorf("%w: the change for %q carries an "+
				"entry for %q", ErrInvalid, p, c.Entry.Path)
		}
		files[p] = c.Entry
	}
	if entry == "" {
		entry = base.Entry
	} else {
		cleaned, err := CleanPath(entry)
		if err != nil {
			return artifacts.Manifest{}, fmt.Errorf("the entry: %w", err)
		}
		entry = cleaned
	}
	out := artifacts.Manifest{Entry: entry, Files: make([]artifacts.Entry, 0, len(files))}
	for _, e := range files {
		out.Files = append(out.Files, e)
	}
	return Canonical(out), nil
}

// Declared is one file a client names by its content before any byte moves.
type Declared struct {
	Path        string
	SHA256      objstore.Hash
	Size        int64
	ContentType string
}

// CarryPlan is which declared files the artifact already holds, and which
// must be transferred.
type CarryPlan struct {
	// Carried are entries naming objects this artifact already holds: no
	// byte moves for them.
	Carried []artifacts.Entry

	// Upload are the files whose bytes must arrive, in path order.
	Upload []Declared
}

// Hashes is every distinct digest declared files name, sorted: the question a
// publish asks of its own artifact's held objects (the domain reader's
// HeldObjects), and nothing wider.
func Hashes(declared []Declared) []objstore.Hash {
	out := make([]objstore.Hash, 0, len(declared))
	for _, d := range declared {
		out = append(out, d.SHA256)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// PlanCarry decides, for each declared file, whether held (THIS artifact's
// objects, by digest) already holds its bytes.
//
// A CARRY NEVER CROSSES AN ARTIFACT: held is the answer the artifact's own
// reader gives for its own rows. A company-wide lookup would tell a writer
// whether any artifact, including one they cannot read, holds a file with a
// given digest. And a carry is matched on the digest AND the size, so a held
// object is never named for bytes it does not hold.
func PlanCarry(declared []Declared, held map[objstore.Hash]objstore.Object) (CarryPlan, error) {
	var plan CarryPlan
	seen := map[string]bool{}
	for _, d := range declared {
		p, err := CleanPath(d.Path)
		if err != nil {
			return CarryPlan{}, err
		}
		if seen[p] {
			return CarryPlan{}, fmt.Errorf("%w: %q is declared twice", ErrInvalid, p)
		}
		seen[p] = true
		if !d.SHA256.Valid() {
			return CarryPlan{}, fmt.Errorf("%w: %q declares digest %q, which is not a "+
				"SHA-256 in lowercase hex", ErrInvalid, p, d.SHA256)
		}
		if d.Size < 0 {
			return CarryPlan{}, fmt.Errorf("%w: %q declares %d bytes", ErrInvalid, p, d.Size)
		}
		typ, err := mediatype.Resolve(p, d.ContentType)
		if err != nil {
			return CarryPlan{}, err
		}
		d.Path, d.ContentType = p, typ.Served
		if obj, ok := held[d.SHA256]; ok && obj.Size == d.Size && obj.Hash == d.SHA256 {
			e, err := NewEntry(p, typ.Served, obj, true)
			if err != nil {
				return CarryPlan{}, err
			}
			plan.Carried = append(plan.Carried, e)
			continue
		}
		plan.Upload = append(plan.Upload, d)
	}
	slices.SortFunc(plan.Carried, func(a, b artifacts.Entry) int { return strings.Compare(a.Path, b.Path) })
	slices.SortFunc(plan.Upload, func(a, b Declared) int { return strings.Compare(a.Path, b.Path) })
	return plan, nil
}
