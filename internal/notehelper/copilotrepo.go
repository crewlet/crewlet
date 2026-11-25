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
// script: the directories it hands Copilot, joined by `,`, and the names of
// the checkouts whose launch sets it kept, a name a line.
const (
	copilotDirsFile = "dirs"
	copilotKeptFile = "kept"
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
	sets := copilotSets(handed, stderr)
	kept, err := copilotKept(sets, keep, keptFrom)
	if err != nil {
		return 0, err
	}
	var sections, dirs, names []string
	for _, s := range sets {
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
	return len(sets), writeNames(filepath.Join(c.State, copilotKeptFile), names)
}

// copilotSets is each handed checkout's launch set, in the launch's order,
// those with neither instructions nor an instruction directory left out. A
// root holding `,` cannot be named in Copilot's list of directories, so its
// directory is left to Copilot's lazy load, said on stderr.
func copilotSets(handed []launched, stderr io.Writer) []copilotSet {
	var sets []copilotSet
	for _, h := range handed {
		s := copilotSet{name: h.Name, root: h.Root, section: h.Text}
		dir := filepath.Join(h.Root, copilotInstructionsDir)
		// A REAL DIRECTORY ONLY: Copilot walks what it is named, so a link
		// would point it outside the checkout.
		if info, err := os.Lstat(dir); err == nil && info.IsDir() {
			if strings.ContainsRune(dir, ',') {
				fmt.Fprintf(stderr, "notehelper: %s is not handed at launch, since its path holds a comma, which "+
					"Copilot's list of instruction directories cannot carry; Copilot loads it on the first view of a file in %s\n",
					dir, h.Root)
			} else {
				s.dir = dir
			}
		}
		if s.section != "" || s.dir != "" {
			sets = append(sets, s)
		}
	}
	return sets
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
