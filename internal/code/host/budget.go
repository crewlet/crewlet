package host

import (
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/objstore"
)

// EVERY CONSTANT THE HOST WAITS ON OR BOUNDS BY, each with its reason.

const (
	// RequestMemoryBytes is the most of ONE fetch request's DECODED body
	// held in memory before the rest is spooled to disk: one mebibyte. A
	// request is held as large as it is — a clone's or an ordinary fetch's
	// negotiation is a few kilobytes, a want and a have a commit at fifty
	// bytes each, and costs that — so nearly every request lives in memory
	// and only a partial clone's lazy fetch, which names every blob a
	// checkout lacks in one request, reaches the spool. What every request
	// holds together is RequestMemoryAllowance's to bound.
	RequestMemoryBytes = 1 << 20

	// WantBytes is one `want <id>` on the wire: a pkt-line's four hex digits,
	// "want ", forty hex digits and a newline. The most wants a request to a
	// repository can legitimately name is one for each object it holds and
	// each ref, so this is what a request's bound is counted in
	// (RequestBound); a have line is the same size.
	WantBytes = 4 + len("want ") + 40 + 1

	// SpoolPerTransfer is how much spooled request each slot of the transfer
	// lane is given in the node's SPOOL ALLOWANCE (SpoolAllowance): 64 MiB, a
	// want list of about 1.3 million objects — a blobless checkout of a tree
	// that size, past the largest public monorepos' working trees. Per SLOT,
	// because a spooled request becomes exactly one upload-pack in the
	// transfer lane, which admits that many at once: the lane is what bounds
	// how many spooled requests the node can put to use, and a spool past it
	// would only wait for a slot.
	SpoolPerTransfer = 64 << 20

	// SpoolCredentialShare is the share of the spool allowance one
	// credential's spooled requests may hold at once, IN BYTES and in count:
	// a quarter of the allowance (SpoolCredentialBytes) and spools for a
	// quarter of the transfer lane (SpoolsPerCredential). A spool is held
	// from its request's first byte past the in-memory part until
	// upload-pack has read it and started answering — at the per-mebibyte
	// floor (objstore.MiBPace) half a minute a mebibyte of the client's
	// sending — so without a cap one credential could open a few slow
	// requests to a large repository and hold the node's whole allowance at
	// almost no cost to itself. A quarter still lets a CI fleet behind one
	// token run a quarter of the node's fetches through the spool at once,
	// leaves three quarters to everybody else, and is also the largest one
	// request this node will spool: 1 GiB on a transfer lane of 64 slots
	// (sixteen cores with the 32 GiB that back them), some 21 million wants.
	SpoolCredentialShare = 4

	// MaxCommandSectionBytes bounds a push's command section — the ref
	// updates before the pack: one mebibyte. code.MaxCommandsPerPush
	// commands naming refs the engine takes — at most code.MaxRefNameBytes,
	// each line its two ids beside the ref, the first its capability list —
	// come to about 340 KiB. So a section past a mebibyte necessarily holds
	// refs the engine refuses bad_ref (a pkt-line carries one of up to
	// 64 KiB, and seventeen such are enough) or lines that parse as no
	// command, and refusing it whole costs no push the engine would have
	// taken.
	MaxCommandSectionBytes = 1 << 20

	// HookDialTimeout is how long the hook gives the engine's socket to
	// accept: the engine is a process on the same host, and a socket that
	// has not accepted in five seconds belongs to an engine that is gone.
	HookDialTimeout = 5 * time.Second

	// hookFrameMax is the largest frame the relay sends the hook: one pipe's
	// capacity, so a frame is copied to the hook's stdout or stderr in one
	// write.
	hookFrameMax = 64 << 10

	// maxHandshake is the longest handshake line a hook connection may send
	// — "CREWLET-HOOK 1 " and a 64-digit token, with room.
	maxHandshake = 128

	// maxSocketPath is the longest relay socket path: under darwin's 104
	// bytes and Linux's 108, with room for the terminating NUL.
	maxSocketPath = 100

	// ProposeBudget is the most a push's proposal is waited on: two of the
	// estate router's write attempts (60 s each), the committer and its
	// runner-up — so a push survives the death of the node committing it.
	ProposeBudget = 2 * time.Minute

	// StoppingRetryAfter is what a stopping code host answers every request
	// with: api.DrainRetryAfter, what takes a stopping node out of a
	// balancer's rotation — held equal by the API's
	// TestTheGitStopRetryIsTheDrainRetry, since this package imports no api.
	StoppingRetryAfter = 30 * time.Second

	// BusyRetryAfter is gitproc.SlotWait: a node whose slots stayed busy for
	// a whole read attempt is one a balancer should send the retry elsewhere
	// from, and so is a node whose spool allowance is spent.
	BusyRetryAfter = gitproc.SlotWait

	// UnavailableRetryAfter is what a request the code log or the object
	// store could not answer is told to wait: one read attempt, the same
	// figure, since what it waits out is an election or a store's blip.
	UnavailableRetryAfter = gitproc.SlotWait

	// maxSentence bounds one refusal sentence on the wire: a diagnostic, and
	// git prints it on one line beside the ref.
	maxSentence = 512
)

// RequestBound is the most decoded bytes a fetch request to repo may carry:
// fifty bytes for every object its live packs hold and every live ref — the
// most wants and haves a request can legitimately name — beside the
// mebibyte held in memory. Read from the row admission already holds
// (code.RepoRef.LiveObjects), before the request is read. On DECODED bytes, so
// a gzip-compressed request cannot expand past it.
func RequestBound(repo code.RepoRef) int64 {
	return int64(WantBytes)*(max(repo.LiveObjects, 0)+max(repo.Refs, 0)) + RequestMemoryBytes
}

// RequestMemoryAllowance is the memory every fetch request held on a node may
// take together, for a transfer lane of transfers slots: RequestMemoryBytes
// each — 64 MiB on a lane of 64 slots, the default at sixteen cores with the
// 32 GiB that back them (gitproc: four a core, and no more than one for every
// gitproc.TransferShare of the node's memory).
// Charged as each request's in-memory part grows and given back as soon as
// upload-pack has read it, so it bounds what requests WAITING for their sync
// and their transfer slot hold, however many arrive at once; a request that
// finds it spent has the rest spooled, never refused for it. Per slot for
// SpoolPerTransfer's reason: the lane is what puts a request to use.
func RequestMemoryAllowance(transfers int) int64 {
	return int64(max(transfers, 1)) * RequestMemoryBytes
}

// SpoolAllowance is the node's spool allowance for a transfer lane of
// transfers slots: SpoolPerTransfer each. On the cache's volume and never in
// its budget (cache.Spool): 4 GiB on a lane of 64 slots, the default at
// sixteen cores with the 32 GiB that back them.
func SpoolAllowance(transfers int) int64 {
	return int64(max(transfers, 1)) * SpoolPerTransfer
}

// SpoolCredentialBytes is the most one credential's spooled requests may hold
// at once on a transfer lane of transfers slots: SpoolCredentialShare of the
// allowance — which is also the largest one request the node spools.
func SpoolCredentialBytes(transfers int) int64 {
	return SpoolAllowance(transfers) / SpoolCredentialShare
}

// SpoolsPerCredential is how many spooled requests one credential may hold at
// once on a transfer lane of transfers slots: SpoolCredentialShare of them,
// and at least one.
func SpoolsPerCredential(transfers int) int {
	return max(transfers/SpoolCredentialShare, 1)
}

// HookBudget bounds what a push's hook session does inside one receive-pack —
// its landing (land) — as gitproc.ReceivePackDeadline's hook term: the three
// scans over the stored pack (force, blobs, introduced — each
// gitproc.PackWorkDeadline of the largest pack one push may store,
// code.MaxPackBytes), the bundle's put at
// the pace floor every leg is held to (objstore.ReadBase +
// objstore.PaceFor(code.MaxPackBytes)), ProposeBudget, and the pending wait —
// summed, though the introduced listing runs beside the put, because a bound
// on the two together is no less than the longer of them.
// Over code.MaxPackBytes rather than the epoch's push limit, because what the
// landing scans and stores is the client's input COMPLETED against the
// repository plus its indexes, which a thin push can grow past
// max_push_bytes. A BOUND on a trickling client, never a schedule: a push that
// stops moving is ended by its Timing.Stall long before it.
func HookBudget(t Timing) time.Duration {
	return 3*gitproc.PackWorkDeadline(code.MaxPackBytes) + objstore.ReadBase +
		objstore.PaceFor(code.MaxPackBytes) + ProposeBudget + t.PendingGrace
}

// receivePackDeadline bounds one push's receive-pack (gitproc.ReceivePackDeadline)
// of cmds into repo — the row the push is judged on, as its round synced it —
// under limits: the push's bytes at the floor pace; a walk of repo's history,
// which git's own connectivity check is for a tip dated before its parent;
// and the landing its hook runs inside the process, its own work (HookBudget)
// and the walks of the history it may make (hookWalks). Every history term
// reads repo's live objects HERE, so the one call site hands the row and
// never a count it could get wrong.
func receivePackDeadline(limits code.Limits, repo code.RepoRef, cmds []parsedCommand, t Timing) time.Duration {
	return gitproc.ReceivePackDeadline(limits.MaxPushBytes, repo.LiveObjects,
		HookBudget(t)+hookWalks(cmds, repo.LiveObjects))
}

// hookWalks bounds the walks of a repository's history a push's landing may
// make beside its HookBudget, over a repository of objects live objects: one
// for each force check (a branch update whose old and new are both commits,
// one merge-base each, in series on a loaded node), one for the listing of
// what the push introduces and one for the path of a blob over the limit —
// each a gitproc.HistoryWalk, the term that bounds the process itself. Sized
// by the repository, never by the push: a skewed push into a large history
// with no commit-graph walks every commit in each of them, and a hook budget
// of the push's own bytes killed receive-pack under its landing rather than
// let the push be slow.
func hookWalks(cmds []parsedCommand, objects int64) time.Duration {
	walks := 2
	for _, c := range cmds {
		if c.Old != code.ZeroOID && c.New != code.ZeroOID && strings.HasPrefix(c.Ref, "refs/heads/") {
			walks++
		}
	}
	return time.Duration(walks) * gitproc.HistoryWalk(objects)
}

// uploadDeadline is the bundle put's own bound: the pace floor over its bytes.
func uploadDeadline(size int64) time.Duration { return objstore.ReadBase + objstore.PaceFor(size) }
