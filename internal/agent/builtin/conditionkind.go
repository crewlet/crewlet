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
// ([KindStateLogWriteRefused]); and a deadline (`timeout`) and an abandoned
// wait (`canceled`), the provider layer's words for a call that ran past its
// own wall clock or that its caller gave up on, which the length of what was
// asked decides as much as the node does. False for anything that is no kind
// of a condition.
//
// A CODING SANDBOX'S CLASS ([SandboxKindPrefix]) IS PUT ON A SIDE BY WHETHER A
// MAIL CAN CAUSE IT, class by class ([sandboxClassIsTheNodes]), never as a
// block: a provider that did not answer or is at its account's limit spent a
// seat's backlog into its poison when every class was the work's. No class
// the sandbox names today is one a mail can cause, so each is the node's, and
// a class added there is decided on its own.
//
// EVERY KIND THIS PACKAGE DECLARES, AND EVERY CLASS THE SANDBOX NAMES, IS
// NAMED HERE, on one side, and TestEveryConditionKindIsTheNodesOrTheWorks
// holds each to it: a new kind or class lands on the side its author chose
// rather than on a default.
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
// is [InfrastructureKind] besides, and no wait clears one: somebody has to act
// on the host, on the keys or on the upgrade.
//
// A FINISHED JOB WHOSE RESUME FAILS ON ONE WAITS FOR THE OPERATOR: the resume
// give-up counts no failure of the node's (internal/sandbox's resumeGivenUp),
// so the run and its seat wait for as long as the condition lasts, and
// nothing ends it but a turn's deadline. So it is said: the node holding the
// seat raises executor_resume_blocked while one such run waits
// (internal/sandbox's Coordinator.ResumesBlocked). False for every other kind,
// one that clears when a store, a log or a broker answers again included,
// and for anything that is no kind of a condition.
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
// box for the seat (the provider's create, the company's setup recipe, the
// engine's probes of the box, a relaunch): every request and command there is
// the deployment's or the engine's own, and a command the seat's executor runs
// in its box ends with its own exit status (a timeout's 124, a signal's), never
// with a class (internal/sandbox's e2b.go). False for a class this build does
// not name.
func sandboxClassIsTheNodes(why sandbox.Why) bool {
	switch why {
	case sandbox.WhyProviderUnavailable:
		// THE NODE'S: the provider, or the box it runs, did not answer or
		// failed on its side, or the way to it did; it clears when the
		// provider answers again, whatever the mail asked.
		return true
	case sandbox.WhySetupUnavailable:
		// THE NODE'S: a download the company's setup recipe makes is failing
		// on the far side (its command exited 75); the recipe is the
		// deployment's, and it clears when that service answers again.
		return true
	case sandbox.WhyProviderLimit:
		// THE NODE'S: the company's account with the provider is at a limit
		// (too many boxes, too many requests), set by the account and spent
		// by every seat alike; it clears when a box ends or the window passes.
		return true
	case sandbox.WhyProviderBilling:
		// THE NODE'S: the provider wants the company's account paid for
		// before it starts anything, a billing state a person settles.
		return true
	case sandbox.WhyConfiguration:
		// THE NODE'S: the deployment's configuration cannot start the run
		// (this node with no sandbox runtime for the company or no tool
		// bridge its box can dial, a cell no backend serves, an executor
		// chain with no member a box runs, the company's setup step exiting,
		// a template or image the provider or the runtime refuses), which a
		// person's edit changes and nothing in a mail reaches.
		return true
	case sandbox.WhyProviderRefused:
		// THE NODE'S: the provider's control plane refused the company's
		// credentials (a 401 or 403 from it, whose requests name the
		// deployment's template, timeout and run environment or a box, and
		// carry nothing of a mail's); a box's own agent answering one gets
		// no class at all. A person changes the key, as with billing and a
		// configuration. Counted as the mail's, its first failure marked a
		// healthy message failed, so an outage that spent the message's
		// budget later sent it to the poison.
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
