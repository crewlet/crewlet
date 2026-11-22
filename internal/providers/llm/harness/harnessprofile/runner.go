package harnessprofile

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

// Executor is a built-in profile's `executor` block: present only on a harness
// this build runs a seat's executor on in a box, with the facts the layers
// below its runner read (config, the sandbox, the doctor, the issue report),
// which cannot import the runner. Everything else about the harness stays in
// its runner's code. BUILT-IN ONLY: the block is a section of the table, not a
// profile field, so no `overrides` reaches it ([ErrExecutorOverride]).
type Executor struct {
	// Runner says the harness has an in-box runner; a block must set it.
	Runner bool `yaml:"runner"`
	// RefusesRoot is the flag the runner starts the harness with that the
	// harness refuses for uid 0, named in the refusal of an engine running as
	// root; "" for a harness that refuses none.
	RefusesRoot string `yaml:"refuses_root,omitempty"`
	// E2BTemplate is E2B's prebuilt image holding the harness, which a
	// company naming no template of its own gets.
	E2BTemplate string `yaml:"e2b_template"`
	// TempEnv is the variable the harness roots its own temporary directory
	// at beside the standard ones, pointed at a host box's own; "" for none.
	TempEnv string `yaml:"temp_env,omitempty"`
	// Floor is the oldest release of the harness its runner drives, as
	// MAJOR.MINOR.PATCH; "" for a runner that states none here.
	Floor string `yaml:"floor,omitempty"`
	// ProbeArgs is a command whose success shows the harness has what its
	// runner starts beyond its release; none for a runner that needs no probe.
	ProbeArgs []string `yaml:"probe_args,omitempty"`
}

// ErrExecutorOverride marks an `overrides` that sets `executor`: which
// harnesses run a seat's executor is this build's own list, never an entry's.
var ErrExecutorOverride = errors.New("overrides may not set executor: which harnesses run a seat's executor, " +
	"and the facts their runners need, are this build's own (the built-in profile's executor block)")

// executorEntry is one built-in profile's executor block, by name.
type executorEntry struct {
	name  string
	block Executor
}

// executors is the built-in table's executor blocks IN THE FILE'S ORDER, the
// one order every list of the runners follows ([ExecutorRunners]).
var executors = sync.OnceValue(func() []executorEntry {
	entries, err := decodeExecutors(profilesYAML)
	if err != nil {
		panic(fmt.Sprintf("harnessprofile: profiles.yaml: %v", err))
	}
	return entries
})

// decodeExecutors reads raw's executor blocks in its order, refusing a block
// that does not say it is a runner or names no template.
func decodeExecutors(raw []byte) ([]executorEntry, error) {
	var doc yaml.Node
	if err := yaml.NewDecoder(bytes.NewReader(raw)).Decode(&doc); err != nil {
		return nil, fmt.Errorf("does not decode: %w", err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return nil, errors.New("is not a mapping of profiles")
	}
	table := doc.Content[0].Content
	var out []executorEntry
	for i := 0; i+1 < len(table); i += 2 {
		var entry struct {
			Executor *Executor `yaml:"executor"`
		}
		if err := table[i+1].Decode(&entry); err != nil {
			return nil, fmt.Errorf("%s: executor: %w", table[i].Value, err)
		}
		switch e := entry.Executor; {
		case e == nil:
			continue
		case !e.Runner:
			return nil, fmt.Errorf("%s: executor: runner must be true where the block is set", table[i].Value)
		case e.E2BTemplate == "":
			return nil, fmt.Errorf("%s: executor: e2b_template is unset", table[i].Value)
		}
		out = append(out, executorEntry{name: table[i].Value, block: *entry.Executor})
	}
	return out, nil
}

// ExecutorRunners is every harness this build runs a seat's executor on, in
// the built-in table's order.
func ExecutorRunners() []string {
	entries := executors()
	out := make([]string, len(entries))
	for i, e := range entries {
		out[i] = e.name
	}
	return out
}

// ExecutorOf is the executor block of the built-in profile named name, false
// for a harness that runs no executor.
func ExecutorOf(name string) (Executor, bool) {
	for _, e := range executors() {
		if e.name == name {
			return e.block, true
		}
	}
	return Executor{}, false
}

// Release is the release a harness's `--version` printed, as a bare
// MAJOR.MINOR.PATCH, or "" where it printed none: the first word that starts
// with a digit (after an optional "v", `opencode v2.0.32`) must be three runs
// of digits joined by dots and nothing else. A pre-release
// (`codex-cli 0.171.0-alpha.1`) names no release, so a floor or an exact
// release is never met by a build cut before it; words around the release
// (`2.1.310 (Claude Code)`) are passed over. The one reading the runners, the
// sandbox and the doctor share.
func Release(printed string) string {
	for _, word := range strings.Fields(printed) {
		word = strings.TrimPrefix(word, "v")
		if word == "" || word[0] < '0' || word[0] > '9' {
			continue
		}
		if !bareRelease(word) {
			return ""
		}
		return word
	}
	return ""
}

// bareRelease reports whether word is exactly three non-empty runs of digits
// joined by dots.
func bareRelease(word string) bool {
	parts := 0
	for part := range strings.SplitSeq(word, ".") {
		parts++
		if part == "" || strings.ContainsFunc(part, func(r rune) bool { return r < '0' || r > '9' }) {
			return false
		}
	}
	return parts == 3
}
