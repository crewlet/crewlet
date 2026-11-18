package advisor

import (
	"cmp"
	"fmt"
	"maps"
	"slices"

	"github.com/crewlet/crewlet/internal/events/types"
)

// ClassSandboxOutOfMemory is an agent whose runs the kernel stopped at its
// box's memory ceiling, more than once in the window: the owner's to size.
const ClassSandboxOutOfMemory Class = "sandbox.out_of_memory"

// minOutOfMemory is how many runs stopped for memory make a finding: one is a
// spike, two in a week is a box too small for the work.
const minOutOfMemory = 2

// EndedRun is one executor run of an agent that ended for memory or on its
// round cap ([types.ExecutorErrorOutOfMemory], [types.ExecutorErrorMaxTurns]),
// and for memory its box's ceiling where the caller read one.
type EndedRun struct {
	Seat, Kind, Turn string
	// Tokens is what it spent, and Rounds the model calls its harness made.
	Tokens, Rounds int64
	MemoryBytes    int64
}

// sandboxOutOfMemory is one finding per agent with [minOutOfMemory] runs or
// more stopped for memory, in words every such agent's finding shares: how
// many runs and the ceiling they hit are its Item.
func sandboxOutOfMemory(ev Evidence, _ func(string) SeatKind) []Finding {
	runs := map[string]int{}
	ceiling := map[string]int64{}
	for _, k := range ev.Runs {
		if k.Kind != types.ExecutorErrorOutOfMemory {
			continue
		}
		runs[k.Seat]++
		ceiling[k.Seat] = max(ceiling[k.Seat], k.MemoryBytes)
	}
	var out []Finding
	for _, seat := range slices.Sorted(maps.Keys(runs)) {
		n := runs[seat]
		if n < minOutOfMemory {
			continue
		}
		item := fmt.Sprintf("%d runs stopped", n)
		if c := ceiling[seat]; c > 0 {
			item += " at its " + gigabytes(c) + " ceiling"
		}
		out = append(out, Finding{
			Class: ClassSandboxOutOfMemory, Severity: SeverityCosts, Actor: ActorOwner,
			Source: SourceEvidence, Area: AreaSetup, Subject: Subject{Kind: SubjectSeat, Seat: seat},
			Title: "Agent sandboxes ran out of memory this week.",
			Why: "In the last 7 days the kernel stopped the agent's runs at its sandbox's memory " +
				"ceiling, so their work was lost and started again. A second one in a row on a " +
				"conversation holds it until the sandbox changes.",
			Item:     item,
			Evidence: []Fact{{Key: "runs", Value: int64(n)}, {Key: "memory_bytes", Value: ceiling[seat]}},
			Fix: Fix{Kind: FixOpenSetting, Setting: "providers.sandbox", Options: []Option{},
				Sentence: "Give the sandbox more memory, such as a larger --memory in its run " +
					"arguments. Saving the change releases a conversation the engine held for it.",
				Unchanged: "The agent's work and its other settings stay as they are."},
		})
	}
	return out
}

// gigabytes is a byte count in whole or tenth gigabytes, as a ceiling is said.
func gigabytes(b int64) string {
	gb := float64(b) / (1 << 30)
	return cmp.Or(trimmed(fmt.Sprintf("%.1f", gb)), "0") + " GB"
}

func trimmed(s string) string {
	if len(s) > 2 && s[len(s)-2:] == ".0" {
		return s[:len(s)-2]
	}
	return s
}
