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
//     A REMOTE run — a fetch, push or listing the engine makes of another
//     host ([RemoteDeadline]) — is the same transfer with the engine as the
//     client.
//   - A WALK OF A REPOSITORY'S HISTORY adds [HistoryWalk] to whatever else
//     bounds it — a push's force check and its listings, and receive-pack's
//     own connectivity check — because what such a walk costs is set by the
//     repository and not by the push: a commit dated before its parent walks
//     every commit there is, and a bound sized by the push alone refused a
//     small push into a large history rather than letting it be slow.
//
// These are BOUNDS on a client that trickles. A client that stops moving is
// ended by [TransferStall] long before either transfer deadline; a remote
// whose connection stops answering is ended by TCP's keepalive probes within
// the same minute, and one that answers and says nothing is given its whole
// [RemoteDeadline].

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
// minute — objstore.Stall, the bound every object transfer is held to.
// uploadpack.keepAlive and receive.keepAlive are five seconds, so a healthy
// transfer moves at least that often; twelve keepalives of silence is a
// process that stopped.
const TransferStall = objstore.Stall

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
// which holds every line a failure ends with — the human's message
// ([Result.LastLine]). Nothing classifies it: what a run's failure means is
// read from its trace2 events (trace.go), which carry no request text and are
// read whole however much stderr git writes.
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

// ReceivePackDeadline bounds one receive-pack into a repository holding
// liveObjects objects. Its phases run in series in one process: the client's
// upload and index-pack's work over what arrived (together the transfer
// formula over maxPushBytes); git's own connectivity check of the new tips
// against every ref, a walk of the history ([HistoryWalk]); and then the
// engine's proc-receive hook, whose own bound — its scans and walks, the
// object-store put, the propose, the pending wait — the caller passes as
// hook.
//
// THE CONNECTIVITY CHECK IS GIT'S, and it walks by date whatever the
// repository holds: `rev-list --objects --stdin --not --all`, with no flag
// the engine can add, so a tip dated before its parent walks the whole
// history with a commit-graph (which it reads in place of the commits, about
// a seventh of the cost on the node pin: 0.62 µs a commit against 4.6) and
// without one. That is the one step of a push whose cost follows the
// repository however its node is maintained, and this term is what lets it
// be slow rather than refused.
func ReceivePackDeadline(maxPushBytes, liveObjects int64, hook time.Duration) time.Duration {
	return transferDeadline(maxPushBytes) + HistoryWalk(liveObjects) + hook
}

// RemoteDeadline bounds one run that dials a remote ([Cmd.Remote]) and moves
// bytes over it — a fetch of a repository from another host, a push of one to
// it, or the listing of its refs — where bytes is what the run moves and
// objects the objects of the repository it fetches into or pushes from (0 for
// a listing, which walks nothing).
//
// IT IS A TRANSFER WITH THE ENGINE AS THE CLIENT, so it is a transfer's
// formula: the remote's leg at the floor every transfer is held to, in series
// with the local pack work over the same bytes — a fetch's index-pack of what
// arrives, a push's pack-objects of what it sends — and a walk of the history
// ([HistoryWalk]), since git's connectivity check after a fetch walks by date
// whatever the repository holds, as receive-pack's does
// ([ReceivePackDeadline]), and a push's counting walks from its tips to what
// the remote holds.
//
// IT IS ALL A REMOTE THAT ANSWERS IS GIVEN. One whose connection dies is ended
// long before it, by TCP's keepalive probes, a minute ([TransferStall]) after
// the last thing the connection heard (the package doc's "One outbound dial";
// a push whose peer vanishes with bytes still in flight, by TCP's
// retransmissions, in about fifteen minutes on Linux's defaults). One that
// stays connected is waited on to this deadline whatever it sends — a
// keepalive every five seconds, or nothing at all — because a git server
// counting or checking a large repository may be silent for minutes and no
// client tells it from one that has wedged; a run ended for its quiet would be
// ended on every attempt.
func RemoteDeadline(bytes, objects int64) time.Duration {
	return transferDeadline(bytes) + HistoryWalk(objects)
}

// WalkPerObject is what a walk of a repository's history is given for every
// object the repository holds: the term a push's walks carry beside the one
// sized by the push (HistoryWalk).
//
// git walks commits newest first BY DATE wherever it has no generation number
// to stop on — every walk where the repository has no commit-graph, and a
// walk that is not asked for in topological order everywhere — so a commit
// dated before its parent (a person's skewed clock, a GIT_COMMITTER_DATE) is
// a reason to walk every commit of the history. MEASURED on the node pin,
// git 2.56.0, on two E-cores of an Intel Core Ultra 7 265U, over the
// gigabyte history internal/code/host's BenchmarkPushSmall pushes (455 680
// commits, 3.27 million objects; its step= arms, a tip dated before the
// history began, one bounded run): with no commit-graph, 4.5–5.0 µs a
// commit — the force check's merge-base 5.0, the listing of what a push
// introduces 4.5, the search for a blob's path 4.6 and receive-pack's
// connectivity check 4.6, 0.63–0.69 µs an object of that history, 2.1–2.3 s
// a walk — and with one, the connectivity check 0.62 µs a commit, while the
// walks the engine asks for by generation take 3–7 ms whatever the tip's
// date. (git 2.47.3 on four vCPUs of a 2.1 GHz Xeon had put a walk by date at
// 5.0–5.5 µs a commit.)
//
// ELEVEN MICROSECONDS AN OBJECT: the term multiplies the repository's LIVE
// OBJECTS, which the rows count and which bound its commits from above, so
// its figure is the per-COMMIT cost, never the per-object one — a commit adds
// at least itself, a tree and a blob, so commits are at most a third of a
// history's objects and nearly all of them only in a history of commits that
// change nothing, and a bound read off the gigabyte's seventh would refuse a
// skewed push into a history of small commits. At least twice the dearest
// measured per-commit cost, for a node whose cores are shared with the store
// and the broker: twice the pin's dearest (the force check's 5.0 µs) is
// 10 µs, and eleven — twice git 2.47.3's 5.5 — is kept, a tenth over it,
// since one run's spread is that wide, and a clone pays 11.0 µs an object of
// a published chain's tail on the pin (internal/code/cache's BenchmarkMidx
// tail= arms), which compact.PublishTailObjects, a second's walk at this
// figure, is set from; the largest repository the defaults admit
// (some 60 million objects, code.DefaultRepoMaxBytes) is given eleven minutes
// a walk, against the hours a pack of that size is given (PackWorkDeadline).
const WalkPerObject = 11 * time.Microsecond

// HistoryWalk bounds one walk of the whole history of a repository holding
// objects live objects: objects × WalkPerObject. It is what any of a push's
// walks may cost however small the push, and it is added to their other
// bounds, never in place of them.
func HistoryWalk(objects int64) time.Duration {
	if objects <= 0 {
		return 0
	}
	return time.Duration(objects) * WalkPerObject
}
