package configplane

import "time"

// ActivationStamp is what a row DERIVED FROM A CONFIGURATION is stamped with,
// so a node applying an older configuration late cannot overwrite a newer
// one's: the instant that configuration was ACTIVATED, never an apply's (every
// node applies separately, at each reconcile and boot, so its own clock would
// differ and a stale boot would walk newer values back), in Unix MILLISECONDS
// (two activations in one second would share a stamp every guard lets
// through). It is carried on the pointer ([coord.Activation.At]) and kept as
// the boot row's `served_at` ([store.Revision.ServedAt]), NOT `activated_at`,
// which a refused re-activation raises without an apply. An instant, not the
// pointer's revision counter, which restarts at 1 with a recreated store. The
// pointer publishes each instant later than the one it replaces
// (coord.ActivationAt), so clocks cannot reorder it.
func ActivationStamp(activatedAt time.Time) int64 { return activatedAt.UTC().UnixMilli() }
