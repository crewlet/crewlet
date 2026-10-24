package artifactupload

import (
	"errors"
	"fmt"
	"io/fs"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/artifacts"
)

// releaseName is one embedded release's directory name: an exact semantic
// version, as a runtime contract and a kit release are both named.
var releaseName = regexp.MustCompile(`^(\d+)\.(\d+)\.(\d+)$`)

// LatestContract asks for the newest contract the build serves, resolved to
// that exact version at publish.
const LatestContract = "latest"

// ReleaseSet is the releases one embedded tree holds: the runtime contracts a
// page may pin, or the kit versions a page may link, each with the files it
// ships.
type ReleaseSet struct {
	versions []string

	// files is every file each release ships, by its path inside the
	// release's directory (`kit.css`, `fonts/geist-sans.woff2`): read from
	// the tree itself, so a file a release added or never had is answered
	// from what the origin serves rather than from a list kept by hand.
	files map[string]map[string]bool
}

// ReleasesIn is the releases a tree holds: one directory per exact version
// (`1.0.0/`), as the build embeds them and the artifact origin serves them. A
// tree with no such directory, or none at all, is a build serving no release,
// which is a set rather than an error: a page that declares no capability needs
// no runtime, and one that links no kit needs no kit.
func ReleasesIn(tree fs.FS) (ReleaseSet, error) {
	entries, err := fs.ReadDir(tree, ".")
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return ReleaseSet{}, nil
	case err != nil:
		return ReleaseSet{}, fmt.Errorf("artifactupload: read the release tree: %w", err)
	}
	set := ReleaseSet{files: map[string]map[string]bool{}}
	for _, e := range entries {
		if !e.IsDir() || !releaseName.MatchString(e.Name()) {
			continue
		}
		files, err := filesIn(tree, e.Name())
		if err != nil {
			return ReleaseSet{}, err
		}
		set.versions = append(set.versions, e.Name())
		set.files[e.Name()] = files
	}
	slices.SortFunc(set.versions, compareReleases)
	return set, nil
}

// filesIn is every regular file under one release's directory, by its path
// inside it.
func filesIn(tree fs.FS, release string) (map[string]bool, error) {
	files := map[string]bool{}
	err := fs.WalkDir(tree, release, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			files[strings.TrimPrefix(p, release+"/")] = true
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("artifactupload: read release %s: %w", release, err)
	}
	return files, nil
}

// Latest is the newest release, empty for none.
func (r ReleaseSet) Latest() string {
	if len(r.versions) == 0 {
		return ""
	}
	return r.versions[len(r.versions)-1]
}

// Serves reports whether version is one of the set.
func (r ReleaseSet) Serves(version string) bool { return slices.Contains(r.versions, version) }

// Ships reports whether release version ships file, a path inside its
// directory. A release the set does not hold ships nothing.
func (r ReleaseSet) Ships(version, file string) bool { return r.files[version][file] }

// Versions is every release served, oldest first.
func (r ReleaseSet) Versions() []string { return slices.Clone(r.versions) }

// compareReleases orders two exact versions numerically.
func compareReleases(a, b string) int {
	pa, pb := releaseName.FindStringSubmatch(a), releaseName.FindStringSubmatch(b)
	for i := 1; i <= 3; i++ {
		x, _ := strconv.Atoi(pa[i])
		y, _ := strconv.Atoi(pb[i])
		if x != y {
			return x - y
		}
	}
	return 0
}

// resolveContract is the exact contract a version of kind pins, from what the
// publish asked for and what the artifact already pins (empty for a create):
//
//   - an empty request keeps the current pin, and a create takes the newest;
//   - `latest` is the newest this build serves, resolved now and stored exact;
//   - an exact version must be one this build serves.
//
// Only a framed kind runs the runtime; a native kind is drawn by the dashboard,
// pins none, and refuses one. A version declaring a capability needs a runtime
// to reach it through.
func (s *Service) resolveContract(kind artifacts.Kind, asked, current string, caps []artifacts.Capability) (string, error) {
	if !kind.Framed() {
		// A NATIVE KIND HAS NO PAGE to grant a capability to or run a
		// runtime in, so either is refused naming the kind, never stored as
		// a declaration nothing honours.
		switch {
		case asked != "":
			return "", fmt.Errorf("%w: a %s artifact runs no page runtime; publish it with no "+
				"contract", ErrContractUnknown, kind)
		case len(caps) > 0:
			return "", fmt.Errorf("%w: a %s artifact is drawn by the dashboard and declares no "+
				"capabilities, and the version declares %s", ErrInvalid, kind, capabilityList(caps))
		}
		return "", nil
	}
	contract := asked
	switch asked {
	case "":
		contract = current
		if contract == "" {
			contract = s.runtimes.Latest()
		}
	case LatestContract:
		contract = s.runtimes.Latest()
	}
	if contract != "" && !s.runtimes.Serves(contract) {
		return "", fmt.Errorf("%w: %q is not a runtime contract this build serves (latest "+
			"is %q)", ErrContractUnknown, contract, s.runtimes.Latest())
	}
	if contract == "" && len(caps) > 0 {
		return "", fmt.Errorf("%w: the version declares %s and this build serves no page "+
			"runtime to reach them through", ErrContractUnknown, capabilityList(caps))
	}
	return contract, nil
}

// checkCapabilities refuses a capability this build does not serve.
func checkCapabilities(caps []artifacts.Capability) error {
	for _, c := range caps {
		if !c.Valid() {
			return fmt.Errorf("%w: %q (this build serves %s)", ErrCapabilityUnknown, c,
				capabilityList(artifacts.Capabilities))
		}
	}
	return nil
}

func capabilityList(caps []artifacts.Capability) string {
	names := make([]string, len(caps))
	for i, c := range caps {
		names[i] = string(c)
	}
	return strings.Join(names, ", ")
}
