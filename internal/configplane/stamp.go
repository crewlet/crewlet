package configplane

import "time"

// ActivationStamp is what a row DERIVED FROM A CONFIGURATION is stamped with,
// so that a node applying an older configuration late cannot overwrite what a
// newer one wrote: the instant that configuration was activated, in Unix
// MILLISECONDS.
//
// Two writers stamp with it — the tracker's projects ([tracker.Writer.ApplyChart])
// and the knowledge base's containers ([pages.Store.EnsureContainer]) — and
// each compares a stamp only against its own kind's. One definition anyway,
// because the rule is the control plane's rather than either writer's, and the
// first copy of it that drifted (a seconds resolution beside a milliseconds
// one) would make "which configuration wrote this" a question with two
// answers.
//
// # The ACTIVATION's instant, never an apply's
//
// Every node applies one activation separately — at its reconcile tick, and
// again at every boot — so an instant read off the applying node's clock
// differs on every one of them. A node that boots on a stale revision would
// stamp NOW and walk a newer configuration's values back to its own; and no
// second apply would ever carry the stamp the first wrote, so every apply on
// every node would be a record per row for a value nobody changed. The
// activation's instant is the fleet's: stamped once by the node that
// activated, carried on the pointer every node reads ([coord.Activation.At]),
// and kept as a node's active revision's `activated_at`, so a boot reads the
// same instant the reconcile applied.
//
// # An instant rather than a counter
//
// Because the value has to survive the coordination store being recreated:
// the activation pointer's own revision is the one counter every node shares,
// and it restarts at 1 with a new store — after which every configuration
// would be older than every row's stamp and none would ever be applied again.
// An instant carries on from where the last one was.
//
// # Milliseconds, not seconds
//
// Two activations inside one second are an ordinary thing for a script to do,
// and at a second's resolution they share a stamp: a node still applying the
// first can land after the second's record and walk its values back at an
// EQUAL stamp, which every guard lets through. Both sources of the instant
// keep at least that much — the pointer carries nanoseconds and the store
// microseconds — so the pointer's instant and a boot's read of the store agree
// on it.
//
// The instants come from different nodes' clocks — each activating node's own
// — but the activation pointer publishes each one LATER, at this resolution,
// than the one it replaces (coord.ActivationAt, decided inside the
// compare-and-set that moves the pointer), so a later activation always
// carries a later stamp whichever node's clock made it. The work-tracker guide
// still asks for synchronised clocks, because the stamps are read off them.
func ActivationStamp(activatedAt time.Time) int64 { return activatedAt.UTC().UnixMilli() }
