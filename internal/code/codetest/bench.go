package codetest

import (
	"log/slog"
	"testing"

	"github.com/crewlet/crewlet/internal/logging"
)

// Quiet keeps a benchmark's output to its figures: every node a topology
// starts logs at info, and an info line in the middle of benchmark output is a
// line benchstat cannot read. Warn and above still print, because one of those
// during a measurement is a finding.
func Quiet(b *testing.B) {
	logging.SetVerbosity(slog.LevelWarn, logging.FormatConsole)
	b.Cleanup(func() { logging.SetVerbosity(slog.LevelInfo, logging.FormatConsole) })
}
