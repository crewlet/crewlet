package builtin

import (
	"errors"
	"syscall"

	"github.com/crewlet/crewlet/internal/store"
)

// The kinds a failed phase or turn records a condition under ([ConditionKind]):
// the environment's, never the engine's own defect. A coding sandbox's class is
// [SandboxKindPrefix] and the class, a deadline `timeout` and an abandoned wait
// `canceled`, the provider layer's own words for the two.
const (
	KindStateLogUnavailable     = "state_log_unavailable"
	KindFileStoreFull           = "file_store_full"
	KindFileStoreUnavailable    = "file_store_unavailable"
	KindCoordinationUnavailable = "coordination_unavailable"
	KindNoDataNode              = "no_data_node"
	KindEstateClosed            = "estate_closed"
	KindStoreBusy               = "store_busy"
	KindQueueNotLive            = "queue_not_live"
	KindQueueUnavailable        = "queue_unavailable"
	KindExecutorUnanswered      = "executor_unanswered"
	KindFleetKeysDiffer         = "fleet_keys_differ"
	KindFleetBuildsDiffer       = "fleet_builds_differ"
	KindDiskFull                = "disk_full"
	KindDiskReadOnly            = "disk_read_only"
	KindDiskFailed              = "disk_failed"
	KindOpenFilesExhausted      = "open_files_exhausted"
)

// SandboxKindPrefix opens the kind of a failure a coding sandbox's backend
// classed: `sandbox_` and the class ([sandbox.Why]).
const SandboxKindPrefix = "sandbox_"

// ConditionKind is the kind a failed phase or turn records err under when err
// is a condition of the environment ([Condition]), and false for a fault. The
// same classification the tools and the API answer a condition by, so a
// record's kind and a refusal's words cannot disagree about what failed.
func ConditionKind(err error) (string, bool) {
	said, ok := condition(err)
	return said.kind, ok
}

// hostCondition is a failure the host's operating system or the store's
// driver marked: a disk that is full, read-only or failing, or a process out of
// file descriptors. Read by the error number the system call returned, never
// by its words, and no wait clears any of them: somebody has to act on the host.
func hostCondition(err error) (conditionSaid, bool) {
	switch {
	case errors.Is(err, syscall.ENOSPC), errors.Is(err, syscall.EDQUOT),
		errors.Is(err, store.ErrFull):
		return conditionSaid{kind: KindDiskFull, clause: "this node's disk is full",
			person: "This node's disk is full. An administrator needs to make room on it.",
			stuck:  true}, true
	case errors.Is(err, syscall.EROFS):
		return conditionSaid{kind: KindDiskReadOnly,
			clause: "this node's disk is mounted read-only",
			person: "This node's disk is read-only. An administrator needs to check it.",
			stuck:  true}, true
	case errors.Is(err, syscall.EIO):
		return conditionSaid{kind: KindDiskFailed,
			clause: "this node's disk failed to read or write",
			person: "This node's disk failed to read or write. An administrator needs " +
				"to check it.", stuck: true}, true
	case errors.Is(err, syscall.EMFILE), errors.Is(err, syscall.ENFILE):
		return conditionSaid{kind: KindOpenFilesExhausted,
			clause: "this node has as many files open as it is allowed",
			person: "This node has reached its limit of open files. An administrator " +
				"needs to raise it.", stuck: true}, true
	}
	return conditionSaid{}, false
}
