package tracker

import "github.com/crewlet/crewlet/internal/logging"

// log is the tracker's own component logger, and what a [DutyDeps] or a
// [ParserOptions] given no Logger writes through.
//
// Both used to read an absent logger as a discarding one, and the engine
// passes neither a logger — so every line the housekeeping duty wrote was
// dropped, `tracker_one_sided_repair_failed` and
// `tracker_merge_marker_without_target` included, which are the two that say
// a repair is NOT happening.
var log = logging.Get("tracker")
