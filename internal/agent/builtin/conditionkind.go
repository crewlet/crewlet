package builtin

import (
	"errors"
	"strings"
	"syscall"

	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/store"
)

// The kinds a failed phase or turn records a condition under ([ConditionKind]):
// the environment's, never the engine's own defect. A coding sandbox's class is
// [SandboxKindPrefix] and the class, a deadline `timeout` and an abandoned wait
// `canceled`, the provider layer's own words for the two.
//
// THE STATE LOG HAS TWO: [KindStateLogUnavailable] for the log, the node or the
// broker refusing, which another node or a later attempt gets past, and
// [KindStateLogWriteRefused] for a write refused for what it is or names
// ([statelog.Reason.RefusesTheWrite]), which every node meets alike; one kind
// for both put a write no node could make beside an outage (the package doc's
// "Whose failure a condition is").
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
// the infrastructure the engine runs on, counted as the failing node's and
// never the work's: a seat's mail that failed on one is handed back as the
// node's own (internal/engine's nakFailure). False for the kinds a piece of
// work can cause (a write refused for what it is, `timeout`, `canceled`) and
// for anything that is no kind. A coding sandbox's class is sided class by
// class ([sandboxClassIsTheNodes]), never as a block. Every declared kind and
// every sandbox class is named here on one side, so a new one lands where its
// author chose rather than on a default, as
// TestEveryConditionKindIsTheNodesOrTheWorks holds; the argument is the
// package doc's "Whose failure a condition is".
func InfrastructureKind(kind string) bool {
	if class, ok := strings.CutPrefix(kind, SandboxKindPrefix); ok {
		return sandboxClassIsTheNodes(sandbox.Why(class))
	}
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

// OperatorKind reports whether kind ([ConditionKind]) is a condition of the
// infrastructure that only an operator clears: this node's disk full, mounted
// read-only or failing to read or write, its limit of open files reached, or
// the fleet's nodes holding different keys or running different builds. Each
// is [InfrastructureKind] besides, and no wait clears one. A finished job
// whose resume fails on one waits for the operator, and its node says so for
// `executor_resume_blocked` (the package doc's "Whose failure a condition
// is"). False for every other kind, one that clears when a store, a log or a
// broker answers again included, and for anything that is no kind.
func OperatorKind(kind string) bool {
	switch kind {
	case KindDiskFull, KindDiskReadOnly, KindDiskFailed, KindOpenFilesExhausted,
		KindFleetKeysDiffer, KindFleetBuildsDiffer:
		return true
	}
	return false
}

// sandboxClassIsTheNodes reports whether a coding sandbox's class is a
// condition of the infrastructure rather than something a mail can cause.
// Every class a turn or a resume fails on is met where the engine starts a
// box for the seat (the runtime's create, the company's setup recipe, the
// engine's probes of the box, a relaunch): every request and command there is
// the deployment's or the engine's own, and a command the seat's executor runs
// in its box ends with its own exit status (a timeout's 124, a signal's), never
// with a class (internal/sandbox's process_unix.go). False for a class this
// build does not name.
func sandboxClassIsTheNodes(why sandbox.Why) bool {
	switch why {
	case sandbox.WhyProviderUnavailable:
		// THE NODE'S: the box, or the runtime that runs it, did not answer
		// or failed on its side, or the way to it did; it clears when it
		// answers again, whatever the mail asked.
		return true
	case sandbox.WhySetupUnavailable:
		// THE NODE'S: a download the company's setup recipe makes is failing
		// on the far side (its command exited 75); the recipe is the
		// deployment's, and it clears when that service answers again.
		return true
	case sandbox.WhyConfiguration:
		// THE NODE'S: the deployment's configuration cannot start the run
		// (this node with no sandbox runtime for the company or no tool
		// bridge its box can dial, a cell no backend serves, an executor
		// chain with no member a box runs, the company's setup step exiting,
		// an image the runtime refuses), which a person's edit changes and
		// nothing in a mail reaches.
		return true
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
