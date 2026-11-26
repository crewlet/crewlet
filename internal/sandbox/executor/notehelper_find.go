package executor

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"

	"github.com/crewlet/crewlet/internal/notehelper/helperbuild"
	"github.com/crewlet/crewlet/internal/notehelper/helpersource"
	"github.com/crewlet/crewlet/internal/words"
)

// sharedHelperDir is where the engine image installs the note helper's builds,
// beside the speech files (Dockerfile).
const sharedHelperDir = "/usr/local/share/crewlet"

// helperSearch is where this engine looks for the note helper's builds, in
// order, and the refusal's words for where it looked and how to get them.
type helperSearch struct {
	dirs        []string
	looked, fix string
}

// noteHelperSearch is where this engine looks, read at each launch: only the
// directory helperbuild.DirEnv names while it is set, since a person who names
// one means that build; else beside its own executable (a release archive,
// `make notehelper`), then sharedHelperDir (the image).
func noteHelperSearch() helperSearch {
	if named := os.Getenv(helperbuild.DirEnv); named != "" {
		return helperSearch{dirs: []string{named},
			looked: fmt.Sprintf("in %s, which %s names, and while it is set the engine looks nowhere else",
				named, helperbuild.DirEnv),
			fix: fmt.Sprintf("Put both notehelper-linux-* files there, or unset %s for the engine to look "+
				"beside itself, then in %s", helperbuild.DirEnv, sharedHelperDir)}
	}
	beside, besideSays := "", "beside the engine (whose path could not be read)"
	if exe, err := os.Executable(); err == nil {
		if exe, err = filepath.EvalSymlinks(exe); err == nil {
			beside = filepath.Dir(exe)
			besideSays = "beside the engine (" + beside + ")"
		}
	}
	return helperSearch{dirs: []string{beside, sharedHelperDir},
		looked: fmt.Sprintf("%s, nor then in %s (%s is unset)", besideSays, sharedHelperDir, helperbuild.DirEnv),
		fix: fmt.Sprintf("Put both notehelper-linux-* files from the release archive of this engine's version "+
			"beside it, run `make crewlet` in a checkout of its source, which builds them beside the engine, "+
			"or name a directory holding them in %s", helperbuild.DirEnv)}
}

// noteHelperBinary is the note helper's executable for a linux box of arch,
// by GOARCH's names, read from the first of [noteHelperSearch]'s directories
// holding it, at each launch. An arch with no build is [errNoHelperBuild]; a
// build missing from this install is a refusal that is NOT, so no harness that
// starts without the helper on an arch it has no build for starts without it
// here.
func noteHelperBinary(arch string) ([]byte, error) {
	if !slices.Contains(helperbuild.Archs, arch) {
		return nil, fmt.Errorf("executor: no note helper is built for a box of arch %q; the builds are %s: %w",
			arch, words.And(helperbuild.Archs), errNoHelperBuild)
	}
	return findNoteHelper(arch, noteHelperSearch())
}

// findNoteHelper reads arch's build from the first of search's directories
// holding it.
func findNoteHelper(arch string, search helperSearch) ([]byte, error) {
	name := helperbuild.File(arch)
	for _, dir := range search.dirs {
		if dir == "" {
			continue
		}
		path := filepath.Join(dir, name)
		helper, err := os.ReadFile(path) //nolint:gosec // the engine's own install directories
		if err == nil {
			if err = sameSource(path, helper); err != nil {
				return nil, err
			}
			return helper, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("executor: reading the note helper %s: %w", path, err)
		}
	}
	return nil, fmt.Errorf("executor: no note helper for a linux/%s box: %s is not %s. %s",
		arch, name, search.looked, search.fix)
}

// sameSource refuses a helper built from other source than this engine's: the
// flags, configs and routes between them are one commit's contract, and a
// helper found on disk is not tied to the engine by anything else
// (internal/notehelper/helpersource). The stamp is read from the file's bytes,
// since a darwin engine cannot run a linux helper to ask it.
func sameSource(path string, helper []byte) error {
	from := "no source it names"
	if i := bytes.Index(helper, []byte(helpersource.Prefix)); i >= 0 && i+len(helpersource.Digest) <= len(helper) {
		from = string(helper[i : i+len(helpersource.Digest)])
	}
	if from == helpersource.Digest {
		return nil
	}
	return fmt.Errorf("executor: the note helper %s was built from %s, and this engine from %s: put the "+
		"notehelper-linux-* files of this engine's own release or image beside it, or build both with "+
		"`make crewlet` in one checkout", path, from, helpersource.Digest)
}
