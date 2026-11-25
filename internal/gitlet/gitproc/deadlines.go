package gitproc

import (
	"time"

	"github.com/crewlet/crewlet/internal/objstore"
)

// Each deadline follows the kind of work it bounds, written once here so no
// call site restates one:
//   - A bounded local read (rev-parse, one object's cat-file, merge-base):
//     [ReadDeadline].
//   - Local work proportional to a pack or a repository (a push's scans, the
//     maintainer's index, verify's fsck, receive-pack's index-pack):
//     [PackWorkDeadline], paced by what one core does per mebibyte.
//   - A transfer ([UploadPackDeadline], [DownloadDeadline],
//     [ReceivePackDeadline], and [RemoteDeadline] with the engine as the
//     client): a client's leg at objstore's floor IN SERIES with the pack work
//     over the same bytes.
//   - A walk of a repository's history adds [HistoryWalk] to its other bounds.

// ReadDeadline bounds one local read, inside one read attempt
// (estate.Production().ReadAttempt): ten seconds, what that attempt holds for
// a log one member stores (2 + 5 + 2 + 1), since no local git waits out the
// election a replicated log's barrier may. This package imports no engine
// package, so estate's TestGitprocsReadBoundsFitInsideOneReadAttempt holds the
// bound, and [SlotWait]'s beside it.
const ReadDeadline = 10 * time.Second

// SlotWait is the longest a run waits for a slot before it is refused
// [ErrBusy]: as long as [ReadDeadline], inside one read attempt. A node whose
// slots stay busy that long is one a balancer should send the retry elsewhere
// from, which the host's 503 and its Retry-After are for.
const SlotWait = 10 * time.Second

// KillGrace is how long a process is given between SIGTERM and SIGKILL. git
// removes its lockfiles on SIGTERM and exits; one that has not in five seconds
// is wedged in I/O, and waiting longer only holds its slot. It is also how
// long a descendant that escaped the process group may hold an empty pipe
// open before the pool closes it (run.go's drain watch).
const KillGrace = 5 * time.Second

// TransferStall ends a transfer that has read and written nothing for a
// minute (objstore.Stall, every object transfer's bound): uploadpack.keepAlive
// and receive.keepAlive are five seconds, so twelve missed keepalives is a
// process that stopped.
const TransferStall = objstore.Stall

// PackWorkPerMiB is the time local git work is given per mebibyte of the pack
// or repository it works over.
//
// One second is seven times the measured worst case, receive-pack's own
// index-pack at one thread over a push the repository already holds (0.14 s a
// mebibyte, BenchmarkIndexPack; package doc, "Measured"), and four times the
// 0.24 s a mebibyte estimated for a pushed 97 MB pack under load: room for a
// disk shared with the store and the broker, without letting a wedged process
// hold its slot for hours.
const PackWorkPerMiB = time.Second

// CaptureMax is the most a run captures when its Cmd.Stdout is nil: one
// mebibyte, for output bounded by construction (a rev-parse, a version, one
// object's type). Output that grows with a push or a repository streams
// through Cmd.Stdout ([Lines]), because a capture that overflows is refused
// ([ErrCaptureFull]) rather than cut short.
const CaptureMax = 1 << 20

// CopyBuffer is the size of each read from git's stdout and stderr pipes:
// 64 KiB, the Linux pipe's default capacity, so one read takes everything a
// pipe can hold where io.Copy's 32 KiB would take half.
const CopyBuffer = 64 << 10

// StderrTail is how much of git's stderr a [Result] keeps: the last 16 KiB,
// which holds the lines a failure ends with ([Result.LastLine]). Nothing
// classifies it: a failure is read from its trace2 events (trace.go), which
// carry no request text and are read whole however much stderr git writes.
const StderrTail = 16 << 10

// PackWorkDeadline bounds LOCAL git work proportional to a pack or a
// repository: [ReadDeadline] plus [PackWorkPerMiB] for every mebibyte begun.
// It is CPU and disk, not a network leg: objstore.ReadBudget would give a
// 1 GiB index-pack seventeen hours, a flat hour would refuse rebuilds above
// about 3.5 GiB that code.limits.repo_max_bytes admits, and a read's ten
// seconds kills the scan of any large push.
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
// liveBytes: ONE network leg (the cache is local disk; the object store's leg
// is the sync before the request, bounded by objstore.ReadBudget) beside
// pack-objects' local work. Not objstore.ReadBudget, whose second network leg
// this request does not have: for 1 GiB, eight and a half hours too many.
func UploadPackDeadline(liveBytes int64) time.Duration {
	return transferDeadline(liveBytes)
}

// DownloadDeadline bounds one download of bytes to a client: a file's bytes or
// an archive, the client's leg in series with local work over the same bytes.
func DownloadDeadline(bytes int64) time.Duration { return transferDeadline(bytes) }

// ReceivePackDeadline bounds one receive-pack into a repository holding
// liveObjects objects. Its phases run in series in one process, so their
// bounds add: the client's upload and index-pack (the transfer formula over
// maxPushBytes; the upload's bound alone lets a client at the floor pace be
// killed in index-pack), git's connectivity check ([HistoryWalk]), and the
// engine's proc-receive hook, whose bound the caller passes as hook.
//
// The connectivity check is git's (`rev-list --objects --stdin --not --all`,
// no flag the engine can add) and walks by date, so a tip dated before its
// parent walks the whole history, with a commit-graph or without: the one
// step of a push whose cost follows the repository, which this term lets be
// slow rather than refused.
func ReceivePackDeadline(maxPushBytes, liveObjects int64, hook time.Duration) time.Duration {
	return transferDeadline(maxPushBytes) + HistoryWalk(liveObjects) + hook
}

// RemoteDeadline bounds one run that dials a remote ([Cmd.Remote]): a fetch,
// a push or a listing of refs (objects 0: it walks nothing). It is a transfer
// of bytes with the engine as the client, plus [HistoryWalk] of the local
// repository's objects: a fetch's connectivity check walks by date as
// receive-pack's does, and a push counts from its tips to what the remote holds.
//
// It is all a remote that answers is given. A dead connection is ended by
// TCP's keepalive a minute ([TransferStall]) after it last heard anything
// (package doc, "One outbound dial"), or with bytes in flight by TCP's
// retransmissions, in about fifteen minutes. A live one is waited on whatever
// it sends: a server counting a large repository may be silent for minutes,
// like a wedged one, and a run ended for its quiet would end on every attempt.
func RemoteDeadline(bytes, objects int64) time.Duration {
	return transferDeadline(bytes) + HistoryWalk(objects)
}

// WalkPerObject is what a walk of a repository's history is given per live
// object ([HistoryWalk]). Without a generation number to stop on, git walks
// by date, so a commit dated before its parent walks the whole history.
//
// Eleven microseconds is a per-COMMIT cost applied to live objects, which
// bound commits from above (a per-object figure would refuse a skewed push
// into a history of small commits): twice git 2.47.3's 5.5 µs a commit, over
// twice the node pin's dearest walk by date (5.0 µs; git 2.56.0, no
// commit-graph, internal/gitlet/host's BenchmarkPushSmall history), for cores
// shared with the store and the broker. compact.PublishTailObjects, a second's
// walk at this figure, is set from it; gitlet.DefaultRepoMaxBytes (some 60
// million objects) gets eleven minutes a walk.
const WalkPerObject = 11 * time.Microsecond

// HistoryWalk bounds one walk of the whole history of a repository holding
// objects live objects: objects × WalkPerObject. It is what any of a push's
// walks may cost however small the push, so it is added to their other
// bounds, never in place of them.
func HistoryWalk(objects int64) time.Duration {
	if objects <= 0 {
		return 0
	}
	return time.Duration(objects) * WalkPerObject
}
