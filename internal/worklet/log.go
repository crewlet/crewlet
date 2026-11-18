package worklet

import "github.com/crewlet/crewlet/internal/logging"

// log is the tracker's own component logger, and what a [DutyDeps] or a
// [ParserOptions] given no Logger writes through: the engine passes neither, and
// a discarding default would drop `tracker_one_sided_repair_failed` and
// `tracker_merge_marker_without_target`, the lines that say a repair is NOT
// happening.
var log = logging.Get("worklet")
