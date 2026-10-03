package usage

import "github.com/crewlet/crewlet/internal/logging"

// log is this domain's own component logger: what a [Publisher] handed no
// logger writes through, so its lines carry `component=usage` — the subsystem
// that wrote them, which is what the deployment guide promises the component
// names.
//
// NEVER A DISCARDING ONE. That was the default here, and it is the default the
// state-log framework removed from every constructor it had it in (statelog's
// loggerOr says what it cost): the engine hands this publisher no logger, so
// `usage_publish_unresolved` and `usage_flush_failed` — the two lines that say
// a node's day is not reaching the fleet — were dropped on every node.
var log = logging.Get("usage")
