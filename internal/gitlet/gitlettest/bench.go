package gitlettest

import (
	"log/slog"
	"testing"

	"github.com/crewlet/crewlet/internal/logging"
)

// Quiet keeps a benchmark's output to its figures: a node's info line in
// benchmark output is a line benchstat cannot read. Warn and above still
// print, because one during a measurement is a finding.
func Quiet(b *testing.B) {
	logging.SetVerbosity(slog.LevelWarn, logging.FormatConsole)
	b.Cleanup(func() { logging.SetVerbosity(slog.LevelInfo, logging.FormatConsole) })
}
