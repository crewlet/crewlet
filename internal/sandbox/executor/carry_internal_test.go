package executor

import (
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/executordomain"
)

// A RELEASE PAST WHAT THE EXECUTOR LOG HOLDS IS NONE. The record of a run's
// session is a file in a box its own agent can write, read up to a box's whole
// file, and the release it names rides the run's row on every capture's stamp —
// twice once one lands. A harness prints its release in a dozen characters, so
// one longer than the log's bound (executordomain.HarnessVersionMaxBytes) is no
// harness's, and is read as a release not known rather than carried; one of the
// harnesses' own shapes is read as it is.
//
// Mutation: read the first MAJOR.MINOR.PATCH whatever its length, and a record
// naming a megabyte of digits puts that megabyte on the run's row.
func TestAReleasePastWhatTheExecutorLogHoldsIsNone(t *testing.T) {
	t.Parallel()
	for printed, want := range map[string]string{
		"2.1.310 (Claude Code)":             "2.1.310",
		"opencode v2.0.32":                  "2.0.32",
		strings.Repeat("9", 1<<20) + ".0.0": "",
		"1." + strings.Repeat("0", executordomain.HarnessVersionMaxBytes) + ".1": "",
	} {
		rec := parseSessionRecord("id s-1\ncontinued no\nrelease " + printed + "\n")
		if got := releaseOf(rec.release); got != want {
			t.Errorf("a record printing %.40q reads release %.40q, want %q", printed, got, want)
		}
	}
}
