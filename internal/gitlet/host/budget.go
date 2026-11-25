package host

import (
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
)

// EVERY CONSTANT THE HOST WAITS ON OR BOUNDS BY, each with its reason.

const (
	// RequestMemoryBytes is the most of ONE fetch request's DECODED body held in
	// memory before the rest is spooled to disk: one mebibyte. A clone's or a
	// fetch's negotiation is a few kilobytes, so only a partial clone's lazy
	// fetch, naming every blob a checkout lacks in one request, reaches the
	// spool. RequestMemoryAllowance bounds what every request holds together.
	RequestMemoryBytes = 1 << 20

	// WantBytes is one `want <id>` on the wire: a pkt-line's four hex digits,
	// "want ", forty hex digits and a newline; a have line is the same size. A
	// request legitimately names at most one want per object and ref the
	// repository holds, so RequestBound is counted in it.
	WantBytes = 4 + len("want ") + 40 + 1

	// SpoolPerTransfer is each transfer-lane slot's share of the node's SPOOL
	// ALLOWANCE (SpoolAllowance), and the most ONE request spools
	// (SpoolRequestBytes): 64 MiB, a want list of about 1.3 million objects, past
	// a blobless checkout of the largest public monorepos. Per SLOT, because a
	// spooled request becomes exactly one upload-pack in the transfer lane, so
	// the lane's width bounds how many spooled requests the node can put to use.
	SpoolPerTransfer = 64 << 20

	// SpoolCredentialShare is the share of the node's spooling one credential's
	// requests may hold at once: a quarter of the allowance's bytes
	// (SpoolCredentialBytes) and of the waiting room (SpoolWaitersPerCredential).
	// A spool is held from its first byte to the answer's first (its sync,
	// upload-pack's read, pack-objects' run; body.go), so uncapped, a few CI
	// runners or slow clients behind one credential could hold the whole
	// allowance. Requests past the quarter WAIT, never refused (spool.go); the
	// cost is that wait: behind a cold sync it reaches Timing.SpoolWait (503), so
	// N lazy fetches of B bytes behind one token fail on a cold node once N × B
	// passes the quarter, thirteen of 20 MB on a four-core node's 256 MiB
	// (TestACredentialsEndedSpoolsOutlastingTheWait). A token per runner moves
	// that to the node's whole room.
	SpoolCredentialShare = 4

	// MaxCommandSectionBytes bounds a push's command section (the ref updates
	// before the pack and the push options after them), which receive-pack holds
	// in memory with no bound of git's: one mebibyte. gitlet.MaxCommandsPerPush
	// commands at gitlet.MaxRefNameBytes come to about 340 KiB and a change's
	// longest options (gitlet.ParseChangeOptions) to some 33 KiB, so a larger
	// section holds refs the engine refuses bad_ref, lines that parse as no
	// command, or options a push to Gitlet has no reason to send: refusing it
	// whole costs no push the engine would have taken.
	MaxCommandSectionBytes = 1 << 20

	// maxEngineCommits is the most commits committed as the engine
	// (landing.EngineCommitter) one coding box's pack may hold for the host to
	// ask the repository about (forged). A box's git commits, amends and rebases
	// under the box's identity, so such a commit is a forgery or an engine write
	// the box brings back (a seat branch whose ref the repository lost). A
	// thousand is a seat's pushing for some thirty-three days at
	// config.DefaultCodePushesPerSeatDay. The scan keeps them (committedAs) and
	// asks in ONE cat-file (cache.Cache.Missing, some 14 ms a thousand), so this
	// bounds what one push makes the node keep and ask: past it the push is
	// refused forged_committer, whose remedy is pushing the history back in parts
	// (a landed part is reached by a ref, so git sends the next without it).
	maxEngineCommits = 1_000

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

	// StoppingRetryAfter is what Gitlet answers every request with while it
	// stops: api.DrainRetryAfter, what takes a stopping node out of a
	// balancer's rotation — held equal by the API's
	// TestTheGitStopRetryIsTheDrainRetry, since this package imports no api.
	StoppingRetryAfter = 30 * time.Second

	// BusyRetryAfter is gitproc.SlotWait: a node whose slots stayed busy that
	// long is one a balancer should send the retry elsewhere from, and so is a
	// node a fetch waited its whole turn at for room to spool.
	BusyRetryAfter = gitproc.SlotWait

	// UnavailableRetryAfter is what a request the code log or the object
	// store could not answer is told to wait: the same ten seconds, also the
	// longest a broker election leaves a log leaderless (jsprovision.Election),
	// which with a store's blip is what it waits out.
	UnavailableRetryAfter = gitproc.SlotWait

	// maxSentence bounds one refusal sentence on the wire: a diagnostic, and
	// git prints it on one line beside the ref.
	maxSentence = 512

	// ArchiveShare is how many read-lane slots (cores) each archive place
	// stands for (Archives): a large ZIP inflates and deflates every file, a
	// core for as long as its client reads fast.
	ArchiveShare = 4

	// ArchivesPerPrincipal is how many ZIPs one principal of the transfer
	// share makes at once on a node, from its measure to its last byte.
	ArchivesPerPrincipal = 1

	// ArchiveFreeBytes is the largest output bound (archiveBound) a ZIP may
	// have and take no archive place: about 4 s of one core at the measured
	// 0.06 s a MiB (BenchmarkDownload).
	ArchiveFreeBytes = 64 << 20

	// MaxArchiveTreeBytes is the most bytes of files a ZIP is made of; past
	// it `git clone --depth 1` serves the same files from compressed packs.
	MaxArchiveTreeBytes = 4 << 30

	// MaxArchiveEntries is the most entries (files, directories, submodules
	// and the prefix's own) a ZIP is made of: a repeated subtree names a
	// million from a few KiB of objects, at about 5 µs and 55 bytes of git's each.
	MaxArchiveEntries = 1_000_000

	// MaxArchiveNameBytes is the most bytes of entry names a ZIP is made of:
	// git holds every name in the ZIP's central directory until its end.
	MaxArchiveNameBytes = 64 << 20

	// ArchiveDeadline is the longest one archive runs: a large one holds a
	// scarce place, so a client slower than its ZIP's size an hour is cut.
	ArchiveDeadline = time.Hour

	// ArchiveEntryBytes is archiveBound's term for each entry beside its name
	// twice: local and central headers, timestamps, zip64 fields and a data
	// descriptor (measured 94 bytes for an ordinary entry, at most about 170).
	ArchiveEntryBytes = 256

	// ArchiveSlackBytes is archiveBound's constant: the end records and the
	// commit-id comment (138 bytes), with room.
	ArchiveSlackBytes = 64 << 10
)

// Archives is how many large ZIPs a node with readSlots read-lane slots makes
// at once: one for every ArchiveShare, and never fewer than two, so one slow
// client never holds a node's only place.
func Archives(readSlots int) int { return max(2, readSlots/ArchiveShare) }

// archiveBound is the most bytes a ZIP of a commit measured m can be: every
// file's bytes with deflate's worst-case growth, each entry's headers, each
// name twice (local and central header) and the end records.
func archiveBound(m treeMeasure) int64 {
	return m.bytes + m.bytes>>10 + m.entries*ArchiveEntryBytes + 2*m.nameBytes + ArchiveSlackBytes
}

// RequestBound is the most decoded bytes a fetch request to repo may carry:
// WantBytes for every object its live packs hold and every live ref (the most
// wants and haves a request can legitimately name), beside the mebibyte held
// in memory. Read from the row admission already holds
// (gitlet.RepoRef.LiveObjects) before the request is read; on DECODED bytes,
// so a gzip-compressed request cannot expand past it.
func RequestBound(repo gitlet.RepoRef) int64 {
	return int64(WantBytes)*(max(repo.LiveObjects, 0)+max(repo.Refs, 0)) + RequestMemoryBytes
}

// RequestMemoryAllowance is the memory every fetch request held on a node may
// take together, for a transfer lane of transfers slots: RequestMemoryBytes
// each, 64 MiB at sixteen cores' default 64 slots (gitproc's sizing). Charged
// as each request's in-memory part grows and given back at the answer's first
// byte (body.go), it bounds what requests WAITING for their sync and slot
// hold, however many arrive; a request that finds it spent spools, never
// refused. Per slot for SpoolPerTransfer's reason. A request bound for the
// spool is NOT charged here: its head goes with it (into its spool, or a
// SpoolWaiters place until then), since charged here a queue of large fetches
// behind one token would spend the allowance and send every other request,
// a small ls-refs among them, to the spool and its queue.
func RequestMemoryAllowance(transfers int) int64 {
	return int64(max(transfers, 1)) * RequestMemoryBytes
}

// SpoolAllowance is the node's spool allowance for a transfer lane of
// transfers slots: SpoolPerTransfer each, on the cache's volume and never in
// its budget (cache.Spool), 4 GiB at sixteen cores' default 64 slots.
func SpoolAllowance(transfers int) int64 {
	return int64(max(transfers, 1)) * SpoolPerTransfer
}

// SpoolCredentialBytes is the most one credential's spools may hold at once
// on a transfer lane of transfers slots: SpoolCredentialShare of the
// allowance.
func SpoolCredentialBytes(transfers int) int64 {
	return SpoolAllowance(transfers) / SpoolCredentialShare
}

// SpoolRequestBytes is the most ONE request spools on a transfer lane of
// transfers slots, refused 413 past it: SpoolPerTransfer, never past a
// credential's bytes, so 64 MiB (about 1.3 million wants) on a lane of four
// slots or more and a quarter of the allowance on a smaller one. It is also
// what a body of undeclared length (every spooled request git sends) CLAIMS
// while it arrives (spool.go): arriving bodies may hold all but this much of
// the allowance, and of a credential's bytes, before one waits on another
// (960 MiB of a four-core node's GiB, 192 MiB of a credential's 256). Hence
// below the credential's bytes on lanes of four or more: a claim of the whole
// share leaves no room for a second body while the first arrives.
func SpoolRequestBytes(transfers int) int64 {
	return min(SpoolPerTransfer, SpoolCredentialBytes(transfers))
}

// SpoolWaiters is the node's WAITING ROOM on a transfer lane of transfers
// slots: how many fetch requests may wait at once to start spooling,
// SpoolCredentialShare places a slot. A place holds up to RequestMemoryBytes
// read before the request was known to need the spool, so the room is a
// memory bound beside RequestMemoryAllowance, never in it: 4 MiB a slot,
// 256 MiB at sixteen cores, out of the 896 MiB a core a node keeps for
// serving (cache.ServingMemoryPerCore). Past it a request that must wait is
// answered 503 at once; one that need not wait takes no place (spool.go).
func SpoolWaiters(transfers int) int {
	return SpoolCredentialShare * max(transfers, 1)
}

// SpoolWaitersPerCredential is how many of one credential's requests may wait
// at once to start spooling, on a transfer lane of transfers slots:
// SpoolCredentialShare of the waiting room, the lane's own width, so a burst
// behind one token as wide as the node's transfers waits whole. The cost: a
// token's excess past it (16 on four cores, 64 on sixteen) is refused 503 at
// once, and git does not retry a lazy fetch, so those checkouts fail; a token
// for each CI runner ends it.
func SpoolWaitersPerCredential(transfers int) int {
	return SpoolWaiters(transfers) / SpoolCredentialShare
}

// HookBudget bounds a push's hook session inside one receive-pack (land), as
// gitproc.ReceivePackDeadline's hook term: three scans of the stored pack
// (force, blobs, introduced; each gitproc.PackWorkDeadline of
// gitlet.MaxPackBytes), the bundle's put at gitlet.UploadDeadline (the bound
// gitlet.StoreBundle gives it, so the hook never kills an upload inside its
// deadline), ProposeBudget and the pending wait, summed though the listing
// runs beside the put. A coding box's extra scan before the put (committedAs,
// one more PackWorkDeadline) fits that slack: its listing is far shorter than
// the put. Over gitlet.MaxPackBytes, not the epoch's push limit, because a
// thin push grows past max_push_bytes once completed and indexed. A BOUND on
// a trickling client, never a schedule: a push that stops moving is ended by
// Timing.Stall long before it.
func HookBudget(t Timing) time.Duration {
	return 3*gitproc.PackWorkDeadline(gitlet.MaxPackBytes) + gitlet.UploadDeadline(gitlet.MaxPackBytes) +
		ProposeBudget + t.PendingGrace
}

// receivePackDeadline bounds one push's receive-pack (gitproc.ReceivePackDeadline)
// of cmds into repo, the row the push is judged on, under limits: the push's
// bytes at the floor pace; a walk of repo's history (git's connectivity check
// of a tip dated before its parent); and its hook's landing, HookBudget plus
// hookWalks. Every history term reads repo's live objects HERE, so the one
// call site hands the row and never a count it could get wrong.
func receivePackDeadline(limits gitlet.Limits, repo gitlet.RepoRef, cmds []parsedCommand, t Timing) time.Duration {
	return gitproc.ReceivePackDeadline(limits.MaxPushBytes, repo.LiveObjects,
		HookBudget(t)+hookWalks(cmds, repo.LiveObjects))
}

// hookWalks bounds the history walks a push's landing may make beside
// HookBudget, over a repository of objects live objects: one per force check
// (a branch update from commit to commit, a merge-base each, in series on a
// loaded node), one for listing what the push introduces and one for an
// oversized blob's path, each a gitproc.HistoryWalk. Sized by the repository,
// never the push: a skewed push into a large history with no commit-graph
// walks every commit in each, and a budget of the push's own bytes would kill
// receive-pack under its landing rather than let the push be slow.
func hookWalks(cmds []parsedCommand, objects int64) time.Duration {
	walks := 2
	for _, c := range cmds {
		if c.Old != gitlet.ZeroOID && c.New != gitlet.ZeroOID && strings.HasPrefix(c.Ref, "refs/heads/") {
			walks++
		}
	}
	return time.Duration(walks) * gitproc.HistoryWalk(objects)
}
