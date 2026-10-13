package gitproc

import (
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
)

// Every duration a call site picks, and the pool's own, each with the reason
// it has the value it has.
//
// # Why a deadline follows the work it bounds
//
// The bounds a git process is given are of three different kinds, and each is
// written once here so no call site restates one:
//
//   - A BOUNDED LOCAL READ (rev-parse, cat-file of one object, merge-base) is
//     one read attempt, [ReadDeadline].
//   - LOCAL WORK PROPORTIONAL TO A PACK OR A REPOSITORY — a scan of a push,
//     the maintainer's index, verify's fsck, receive-pack's own index-pack —
//     is [PackWorkDeadline]: it is CPU and disk, not a network leg, so it is
//     paced by what one core does per mebibyte. objstore.ReadBudget would
//     give a 1 GiB index-pack seventeen hours ("a minute and two paces a
//     mebibyte, for TWO NETWORK LEGS") where it needs two to three minutes;
//     a flat hour would refuse every rebuild above about 3.5 GiB while
//     code.limits.repo_max_bytes admits a tebibyte; and a read's ten seconds
//     kills the scan of any large push.
//   - A TRANSFER (upload-pack, receive-pack) is a client's leg at the floor
//     every transfer is held to, objstore.ReadBase + objstore.PaceFor, IN
//     SERIES with the local pack work over the same bytes. receive-pack adds
//     the engine's hook after both, since all three phases run inside one
//     process: bounded by the client's leg alone, a client at the floor pace
//     spends the whole budget uploading and the run is killed in index-pack.
//
// These are BOUNDS on a client that trickles. A client that stops moving is
// ended by [TransferStall] long before either transfer deadline.

// ReadDeadline bounds one local read: one read attempt, the bound the router
// gives a read anywhere in the engine (estate.Production().ReadAttempt). This
// package imports no engine package, so estate's
// TestGitprocsReadBoundsAreOneReadAttempt holds the equality, and [SlotWait]'s
// beside it.
const ReadDeadline = 10 * time.Second

// SlotWait is the longest a run waits for a slot before it is refused
// [ErrBusy]: one read attempt (estate.Production().ReadAttempt, held equal by
// estate's TestGitprocsReadBoundsAreOneReadAttempt). A node whose slots have
// been busy for a whole read attempt is a node a balancer should send the
// retry elsewhere from, which is what the host's 503 and its Retry-After are
// for.
const SlotWait = 10 * time.Second

// KillGrace is how long a process is given between SIGTERM and SIGKILL. git
// removes its lockfiles on SIGTERM and exits; a process that has not done so
// in five seconds is wedged in I/O, and waiting longer only holds its slot.
// It is also how long an empty pipe may be held open by a descendant that
// escaped the process group before the pool closes it (run.go's drain watch).
const KillGrace = 5 * time.Second

// TransferStall ends a transfer that has read and written nothing for a
// minute — objstore.ReadStall, the bound every object transfer is held to.
// uploadpack.keepAlive and receive.keepAlive are five seconds, so a healthy
// transfer moves at least that often; twelve keepalives of silence is a
// process that stopped.
const TransferStall = objstore.ReadStall

// PackWorkPerMiB is the time local git work is given per mebibyte of the pack
// or repository it works over.
//
// ONE SECOND: receive-pack's own index-pack at pack.threads=1 — the pack
// read from stdin and written out, every object checked (--strict) and
// looked up in the repository — was measured at 0.14 s a mebibyte by
// BenchmarkIndexPack's worst case, a push whose every object the repository
// already holds and so reads back and compares (an 18 MiB history whose
// deltas chain 32 deep; the package doc's "Measured"), and at 0.11 s when
// every object is new. A 97 MB pack was measured only in the lighter form,
// index-pack over a file outside any repository, which is the delta
// resolution alone: 0.125–0.18 s a mebibyte (12.1 s, and 16.7 s on the same
// four shared vCPUs under a resumed session's load). On the 18 MiB history
// that form is 1.3 times quicker than the worst case, so the 97 MB pack
// pushed would take about 0.16–0.24 s a mebibyte. One second is seven times
// the measured worst case and four times the slowest of those: room for a
// disk shared with the store and the broker and a node under load, without
// letting a wedged process hold its slot for hours.
const PackWorkPerMiB = time.Second

// CaptureMax is the most a run captures when its Cmd.Stdout is nil: one
// mebibyte, for output bounded by construction — a rev-parse, a version, one
// object's type. Output that grows with a push or a repository (show-index
// writes about 60 bytes an object, and a 1 GiB pack holds millions) streams
// through Cmd.Stdout ([Lines]) instead, because a capture that overflows is
// refused ([ErrCaptureFull]) rather than cut short.
const CaptureMax = 1 << 20

// CopyBuffer is the size of each read from git's stdout and stderr pipes:
// 64 KiB, the Linux pipe's default capacity, so one read takes everything a
// pipe can hold where io.Copy's 32 KiB would take half.
const CopyBuffer = 64 << 10

// StderrTail is how much of git's stderr a [Result] keeps: the last 16 KiB,
// which holds every line a failure ends with. Classification reads every line
// as it arrives, so a damage line pushed out of the tail still decides the
// kind.
const StderrTail = 16 << 10

// PackWorkDeadline bounds LOCAL git work proportional to a pack or a
// repository: [ReadDeadline] plus [PackWorkPerMiB] for every mebibyte begun.
// receive-pack's own index-pack (inside [ReceivePackDeadline]), the receive
// pipeline's scans of a push, the maintainer and verify's fsck (over the
// repository's live bytes).
func PackWorkDeadline(bytes int64) time.Duration {
	if bytes <= 0 {
		return ReadDeadline
	}
	mib := (bytes + objstore.MiB - 1) / objstore.MiB
	return ReadDeadline + time.Duration(mib)*PackWorkPerMiB
}

// transferDeadline is ONE formula for both directions of a transfer: a
// client's leg at the floor every transfer is held to (objstore.ReadBase +
// objstore.PaceFor) in series with local pack work over the same bytes.
// Unexported: a call site names its direction.
func transferDeadline(bytes int64) time.Duration {
	return objstore.ReadBase + objstore.PaceFor(bytes) + PackWorkDeadline(bytes)
}

// UploadPackDeadline bounds one upload-pack request over a repository holding
// liveBytes. ONE network leg — the cache is local disk, and the object store's
// leg is the sync before the request, bounded per pack by objstore.ReadBudget
// — beside pack-objects' counting and compression, which is local work
// proportional to what it sends. Not objstore.ReadBudget, whose second pace a
// mebibyte is for a second network leg this request does not have: for a
// 1 GiB repository that is eight and a half hours too many.
func UploadPackDeadline(liveBytes int64) time.Duration {
	return transferDeadline(liveBytes)
}

// ReceivePackDeadline bounds one receive-pack. Its three phases run in series
// in one process: the client's upload and index-pack's work over what arrived
// (together the transfer formula over maxPushBytes), and then the engine's
// proc-receive hook, whose own bound — the object-store put, the propose, the
// pending wait — the caller passes as hook.
func ReceivePackDeadline(maxPushBytes int64, hook time.Duration) time.Duration {
	return transferDeadline(maxPushBytes) + hook
}
