package statelog

import (
	"log/slog"

	"github.com/crewlet/crewlet/internal/logging"
)

// log is the framework's own component logger, and every line this package
// writes reaches the process's destinations through it unless a caller hands
// in a logger of its own.
var log = logging.Get("statelog")

// loggerOr is the logger a constructor was handed, or the package's own when
// it was handed none.
//
// # Why an absent logger is this package's and never a discarding one
//
// It was a discarding one, in every constructor here, and the engine passed no
// logger to the applier, the write authority or the re-anchor — so every line
// those three write was dropped on every production node. That is
// `statelog_apply_faulted` (a node whose rows stopped moving),
// `statelog_record_gated` (a durable record that applied nowhere, which the
// replication guide tells an operator to look for), `statelog_write_gated`,
// `statelog_publish_unknown` and both re-anchor lines, all silent, while the
// three constructors the engine did hand a logger went on logging and made the
// silence look like a quiet log rather than a missing one.
//
// A NIL IS NOT REFUSED instead, because the tree's convention is the other
// one: a package logs through its own [logging.Get] logger, and an optional
// logger falls back to it — `mcp.NewBridge` is handed nil by the engine for
// exactly that. A refusal would make every caller pass a logger it has no
// reason to choose, and the one the engine had to choose was its OWN, which
// labels these lines `component=engine` when the deployment guide promises the
// component names the subsystem that emitted the line. The engine therefore
// passes none. A test that wants to read what was written passes one.
func loggerOr(given *slog.Logger) *slog.Logger {
	if given != nil {
		return given
	}
	return log
}
