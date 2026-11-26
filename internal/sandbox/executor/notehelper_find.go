package executor

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/crewlet/crewlet/internal/notehelper/helperbuild"
	"github.com/crewlet/crewlet/internal/words"
)

// sharedHelperDir is where the engine image installs the note helper's builds,
// beside the speech files (Dockerfile).
const sharedHelperDir = "/usr/local/share/crewlet"

// helperPlace is one directory the engine looks in for the note helper's
// builds, and how the refusal names it.
type helperPlace struct {
	dir, says string
}

// noteHelperPlaces is where this engine looks, in order: beside its own
// executable (a release archive), sharedHelperDir (the image), then the
// directory helperbuild.DirEnv names (a checkout's `make notehelper`, a test
// binary). Read at each launch, so a variable set after start counts.
func noteHelperPlaces() []helperPlace {
	beside, besideSays := "", "beside the engine (its path unread)"
	if exe, err := os.Executable(); err == nil {
		if exe, err = filepath.EvalSymlinks(exe); err == nil {
			beside = filepath.Dir(exe)
			besideSays = "beside the engine (" + beside + ")"
		}
	}
	named, namedSays := os.Getenv(helperbuild.DirEnv), "unset"
	if named != "" {
		namedSays = named
	}
	return []helperPlace{
		{dir: beside, says: besideSays},
		{dir: sharedHelperDir, says: "in " + sharedHelperDir},
		{dir: named, says: "in " + helperbuild.DirEnv + " (" + namedSays + ")"},
	}
}

// noteHelperBinary is the note helper's executable for a linux box of arch,
// by GOARCH's names, read from the first of [noteHelperPlaces] holding it, at
// each launch. An arch with no build is [errNoHelperBuild]; a build missing
// from this install is a refusal that is NOT, so no harness that starts
// without the helper on an arch it has no build for starts without it here.
func noteHelperBinary(arch string) ([]byte, error) {
	if !slices.Contains(helperbuild.Archs, arch) {
		return nil, fmt.Errorf("executor: no note helper is built for a box of arch %q; the builds are %s: %w",
			arch, words.And(helperbuild.Archs), errNoHelperBuild)
	}
	return findNoteHelper(arch, noteHelperPlaces())
}

// findNoteHelper reads arch's build from the first of places holding it.
func findNoteHelper(arch string, places []helperPlace) ([]byte, error) {
	name := helperbuild.File(arch)
	said := make([]words.Said, 0, len(places))
	for _, p := range places {
		said = append(said, words.Said(p.says))
		if p.dir == "" {
			continue
		}
		path := filepath.Join(p.dir, name)
		helper, err := os.ReadFile(path) //nolint:gosec // the engine's own install directories
		if err == nil {
			return helper, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("executor: reading the note helper %s: %w", path, err)
		}
	}
	return nil, fmt.Errorf("executor: no note helper for a linux/%s box: %s is not %s. Take both "+
		"notehelper-linux-* files from the release archive of this engine's version and put them beside it, "+
		"or run `make notehelper` in a checkout of its source and set %s to its build/notehelper",
		arch, name, words.Or(said), helperbuild.DirEnv)
}
