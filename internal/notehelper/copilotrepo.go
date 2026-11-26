package main

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// copilotInstructionsDir is the directory below a checkout's root a Copilot
// 1.0.114 session started there loads `*.instructions.md` from, and which
// COPILOT_CUSTOM_INSTRUCTIONS_DIRS hands exactly when it names that
// directory: its unscoped files inline, a row for each scoped one, nothing
// else of the checkout (measured, row Nb; naming the root instead loads
// stray `*.instructions.md` from anywhere below it, row N7).
const copilotInstructionsDir = ".github/instructions"

// copilotPointer ends the line naming a checkout whose launch set a run
// dropped to fit Copilot's static budget: Copilot loads its instructions
// itself on the first view of a file under it (measured, section 10).
const copilotPointer = ": its instructions arrive when you first view a file in it"

// Files under a run's checkout state copilot-launch writes for the run's
// script: the directories it hands Copilot, joined by `,`, the names of the
// checkouts whose launch sets it kept, and of every checkout with one, a name
// a line.
const (
	copilotDirsFile = "dirs"
	copilotKeptFile = "kept"
	copilotSetsFile = "sets"
)

// copilotSet is one checkout's launch set: its section of the home
// instruction file and the instruction directory Copilot is pointed at, either
// "" where it has none.
type copilotSet struct {
	name, root, section, dir string
}

// runCopilotLaunch composes a Copilot run's launch from what checkout-launch
// handed ([launchedFile]): -keep the first N launch sets (-1, all), or -kept
// those a file names, the rest each a line pointing at Copilot's own lazy load.
// It prints how many checkouts have a launch set. Exit 0 once written, 1
// where it could not write them.
func runCopilotLaunch(_ context.Context, f flags, _ io.Reader, stdout, stderr io.Writer, _ func(string) string) int {
	n, err := copilotLaunch(f.config, f.keep, f.kept, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "notehelper: composing the run's launch: %v\n", err)
		return exitError
	}
	fmt.Fprintln(stdout, n)
	return exitDone
}

// copilotLaunch is copilot-launch under the launch config at config: it
// writes the kept sets' sections and the dropped sets' pointers over the
// config's instruction file (removed where there is neither), the kept
// directories and names under its state, and is how many sets there are.
func copilotLaunch(config string, keep int, keptFrom string, stderr io.Writer) (int, error) {
	raw, err := readBounded(config, maxConfig)
	if err != nil {
		return 0, err
	}
	var c launchConfig
	if err = json.Unmarshal(raw, &c); err != nil {
		return 0, fmt.Errorf("the launch config: %w", err)
	}
	if c.State == "" || c.Agents == "" || (keptFrom != "" && keep >= 0) {
		return 0, errors.New("the launch config names no state or instruction file, or both -keep and -kept are given")
	}
	handed, err := readLaunched(c.State)
	if err != nil {
		return 0, err
	}
	// SAID ONCE A RUN: by its first attempt, which keeps every set, or its
	// resume; a relaunch says nothing again.
	sets := copilotSets(handed, keep < 0, stderr)
	kept, err := copilotKept(sets, keep, keptFrom)
	if err != nil {
		return 0, err
	}
	var sections, dirs, names, all []string
	for _, s := range sets {
		all = append(all, s.name)
		switch {
		case !kept[s.name]:
			sections = append(sections, checkoutHeader(s.name, s.root)+copilotPointer)
		case s.section != "":
			sections = append(sections, checkoutHeader(s.name, s.root)+"\n\n"+s.section)
		}
		if kept[s.name] {
			names = append(names, s.name)
			if s.dir != "" {
				dirs = append(dirs, s.dir)
			}
		}
	}
	if err = os.Remove(c.Agents); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return 0, err
	}
	if len(sections) > 0 {
		if err = os.WriteFile(c.Agents, []byte(strings.Join(sections, "\n\n")), 0o600); err != nil {
			return 0, err
		}
	}
	if err = os.WriteFile(filepath.Join(c.State, copilotDirsFile), []byte(strings.Join(dirs, ",")), 0o600); err != nil {
		return 0, err
	}
	if err = writeNames(filepath.Join(c.State, copilotSetsFile), all); err != nil {
		return 0, err
	}
	return len(sets), writeNames(filepath.Join(c.State, copilotKeptFile), names)
}

// copilotSets is each handed checkout's launch set, in the launch's order,
// those with neither instructions nor an instruction directory left out. The
// directory is handed by its real path, and only while that stays in the
// checkout: Copilot walks what it is named, so a link out would hand another
// tree. A real path holding `,` cannot be named in Copilot's list of
// directories, so it is left to Copilot's lazy load, said on stderr where say.
func copilotSets(handed []launched, say bool, stderr io.Writer) []copilotSet {
	var sets []copilotSet
	for _, h := range handed {
		s := copilotSet{name: h.Name, root: h.Root, section: h.Text}
		if dir, ok := checkoutDir(h.Root, copilotInstructionsDir); ok {
			if !strings.ContainsRune(dir, ',') {
				s.dir = dir
			} else if say {
				fmt.Fprintf(stderr, "notehelper: %s is not handed at launch, since its path holds a comma, which "+
					"Copilot's list of instruction directories cannot carry; Copilot loads it on the first view of a file in %s\n",
					dir, h.Root)
			}
		}
		if s.section != "" || s.dir != "" {
			sets = append(sets, s)
		}
	}
	return sets
}

// checkoutDir is rel below root as its real path, true where that is a
// directory inside root's own real path.
func checkoutDir(root, rel string) (string, bool) {
	real, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", false
	}
	dir, err := filepath.EvalSymlinks(filepath.Join(root, rel))
	if err != nil {
		return "", false
	}
	inside, err := filepath.Rel(real, dir)
	if err != nil || !filepath.IsLocal(inside) {
		return "", false
	}
	info, err := os.Stat(dir)
	return dir, err == nil && info.IsDir()
}

// copilotKept is the names of sets kept: those keptFrom names (none where the
// file is missing: a session whose kept sets were never stored), else the
// first keep, all where keep is negative.
func copilotKept(sets []copilotSet, keep int, keptFrom string) (map[string]bool, error) {
	kept := map[string]bool{}
	if keptFrom != "" {
		named, err := readNames(keptFrom)
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		for _, s := range sets {
			kept[s.name] = slices.Contains(named, s.name)
		}
		return kept, nil
	}
	for i, s := range sets {
		kept[s.name] = keep < 0 || i < keep
	}
	return kept, nil
}

// readNames is the file at path, a name a line.
func readNames(path string) ([]string, error) {
	file, err := os.Open(path) //nolint:gosec // the run's own state, or the session's stored launch
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()
	var names []string
	scan := bufio.NewScanner(io.LimitReader(file, maxConfig))
	for scan.Scan() {
		if name := scan.Text(); name != "" {
			names = append(names, name)
		}
	}
	return names, scan.Err()
}

// writeNames writes names to path, a name a line.
func writeNames(path string, names []string) error {
	var b strings.Builder
	for _, name := range names {
		b.WriteString(name + "\n")
	}
	return os.WriteFile(path, []byte(b.String()), 0o600)
}

// runCopilotNested is one firing of a Copilot run's extension after a tool of
// its main loop (internal/sandbox/executor's copilot_steer.js): it hands each
// checkout cloned since, once ([handNested]), as its skills' listing and what
// it left out, plain text the extension appends to the tool's result. No
// instructions: Copilot loads a checkout's itself on the first view of a file
// in it (measured, section 10). It reads no stdin and exits 0 whatever
// happened, its trouble on stderr.
func runCopilotNested(_ context.Context, f flags, _ io.Reader, stdout, stderr io.Writer, _ func(string) string) int {
	c, err := readNested(f.config)
	if err != nil {
		fmt.Fprintf(stderr, "notehelper: the nested config: %v\n", err)
		return exitDone
	}
	handed, skipped, err := handNested(c, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "notehelper: handing a checkout's skills: %v\n", err)
	}
	_, _ = io.WriteString(stdout, copilotNestedListing(handed, skipped))
	return exitDone
}

// copilotNestedListing is the checkouts handed as a Copilot firing lists them:
// each one's skills ([skillsListing]), since Copilot lists no skill copied
// after its launch (measured, row m8), then what the firing left out, within
// one firing's bound ([listingBudget.listings], the left out first).
func copilotNestedListing(handed []handedCheckout, skipped []string) string {
	skills, left := newListingBudget().listings(handed, skipped)
	return strings.Join(appendNonEmpty(appendNonEmpty(nil, skills...), left), "\n\n")
}
