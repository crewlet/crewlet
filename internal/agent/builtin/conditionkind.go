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
//
// THE STATE LOG HAS TWO. [KindStateLogUnavailable] is the log, the node or the
// broker refusing a read or a write (the node evicted, behind or deferring,
// the log full or wrong, a broker that refused it), which another node, a
// later attempt or an operator gets past. [KindStateLogWriteRefused] is the
// log refusing a write for what the write itself is or names
// ([statelog.Reason.RefusesTheWrite]: a record too large for the broker, an
// object deleted for good, an operation id that names another write, an
// operation a later record undid), which every node and every attempt meets
// alike. One kind for both put a write the work asked for and no node could
// make beside an outage, on the node's side.
const (
	KindStateLogUnavailable     = "state_log_unavailable"
	KindStateLogWriteRefused    = "state_log_write_refused"
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

// InfrastructureKind reports whether kind ([ConditionKind]) is a condition of
// the infrastructure the engine runs on: a store, a log, a broker, a disk, the
// fleet's nodes and what they hold. A failure on one is counted as the failing
// node's, never the work's: a seat's mail that failed on one is handed back as
// the node's own (internal/engine's nakFailure), and a delivery it spends is
// kept with its group's unhandled deliveries rather than with its poison.
//
// FALSE FOR THE KINDS LEFT TO THE WORK, each one what was asked can cause: a
// write the state log refused for what the write itself is
// ([KindStateLogWriteRefused]); a coding sandbox's class ([SandboxKindPrefix]),
// one of which does not say whether the box failed for what was asked of it
// or for its provider (a command the box timed out or killed, a provider that
// did not answer: [sandbox.WhyProviderUnavailable]); and a deadline (`timeout`)
// and an abandoned wait (`canceled`), the provider layer's words for a call
// that ran past its own wall clock or that its caller gave up on, which the
// length of what was asked decides as much as the node does. False for
// anything that is no kind of a condition.
//
// EVERY KIND THIS PACKAGE DECLARES IS NAMED HERE, on one side, and
// TestEveryConditionKindIsTheNodesOrTheWorks holds each to it: a new kind
// lands on the side its author chose rather than on a default.
func InfrastructureKind(kind string) bool {
	switch kind {
	case KindStateLogUnavailable, KindFileStoreFull, KindFileStoreUnavailable,
		KindCoordinationUnavailable, KindNoDataNode, KindEstateClosed,
		KindStoreBusy, KindQueueNotLive, KindQueueUnavailable,
		KindExecutorUnanswered, KindFleetKeysDiffer, KindFleetBuildsDiffer,
		KindDiskFull, KindDiskReadOnly, KindDiskFailed, KindOpenFilesExhausted:
		return true
	case KindStateLogWriteRefused:
		// THE WORK'S: no node can make the write it asked for, and no wait
		// lets one; a different write (a split change, a fresh operation, an
		// object not deleted) is what gets past it.
		return false
	}
	return false
}

// ConditionKind is the kind a failed phase or turn records err under when err
// is a condition of the environment ([Condition]), and false for a fault. The
// same classification the tools and the API answer a condition by, so a
// record's kind and a refusal's words cannot disagree about what failed; and
// the one a seat's mail that failed is told the failing node's or the work's
// by ([InfrastructureKind]). That reads the failure itself, and a record's
// kind does not always: a provider's failure that wraps a condition (a
// harness whose prompt a full disk could not take) is recorded under the
// provider's own kind (internal/agent/runner's ErrorKind), while its mail is
// handed back as the node's.
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
