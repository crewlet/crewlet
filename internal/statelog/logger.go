package statelog

import (
	"log/slog"

	"github.com/crewlet/crewlet/internal/logging"
)

// log is the package's component logger, which every line reaches the
// process's destinations through unless a caller hands in its own.
var log = logging.Get("statelog")

// loggerOr is the logger a constructor was handed, or the package's own.
//
// Never a discarding one: it would silence every warning the applier, the write
// authority and the re-anchor write (`statelog_apply_faulted`,
// `statelog_record_gated`, ...). Nil is not refused either: the tree's
// convention is that an optional logger falls back to the package's
// [logging.Get] logger, and a caller made to choose would pass its OWN,
// labelling these lines with the wrong `component`. So the engine passes none;
// a test that reads the lines passes one.
func loggerOr(given *slog.Logger) *slog.Logger {
	if given != nil {
		return given
	}
	return log
}
