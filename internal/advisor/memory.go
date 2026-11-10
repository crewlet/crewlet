package advisor

import (
	"cmp"
	"fmt"
	"maps"
	"slices"
)

// ClassSandboxOutOfMemory is an agent whose runs the kernel stopped at its
// box's memory ceiling, more than once in the window: the owner's to size.
const ClassSandboxOutOfMemory Class = "sandbox.out_of_memory"

// minOutOfMemory is how many runs stopped for memory make a finding: one is a
// spike, two in a week is a box too small for the work.
const minOutOfMemory = 2

// MemoryKill is one run of an agent the kernel stopped for memory, with its
// box's ceiling where the run named one.
type MemoryKill struct {
	Seat        string
	MemoryBytes int64
}

// sandboxOutOfMemory is one finding per agent with [minOutOfMemory] runs or
// more stopped for memory, naming the ceiling they hit.
func sandboxOutOfMemory(ev Evidence, _ func(string) SeatKind) []Finding {
	runs := map[string]int{}
	ceiling := map[string]int64{}
	for _, k := range ev.MemoryKills {
		runs[k.Seat]++
		ceiling[k.Seat] = max(ceiling[k.Seat], k.MemoryBytes)
	}
	var out []Finding
	for _, seat := range slices.Sorted(maps.Keys(runs)) {
		n := runs[seat]
		if n < minOutOfMemory {
			continue
		}
		at := ""
		if c := ceiling[seat]; c > 0 {
			at = " at its " + gigabytes(c) + " ceiling"
		}
		out = append(out, Finding{
			Class: ClassSandboxOutOfMemory, Severity: SeverityCosts, Actor: ActorOwner,
			Source: SourceEvidence, Area: AreaSetup, Subject: Subject{Kind: SubjectSeat, Seat: seat},
			Title: fmt.Sprintf("%s's sandbox ran out of memory %d times this week.", seat, n),
			Why: fmt.Sprintf("In the last 7 days the kernel stopped %d of %s's runs%s, so their "+
				"work was lost and started again. A second one in a row on a conversation holds "+
				"it until the sandbox changes.", n, seat, at),
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
