package host

import (
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc"
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
	// lane is given in the node's SPOOL ALLOWANCE (SpoolAllowance), and the
	// most ONE request spools (SpoolRequestBytes): 64 MiB, a want list of
	// about 1.3 million objects — a blobless checkout of a tree that size,
	// past the largest public monorepos' working trees. Per SLOT, because a
	// spooled request becomes exactly one upload-pack in the transfer lane,
	// which admits that many at once: the lane is what bounds how many
	// spooled requests the node can put to use, so a lane's width of the
	// largest requests is exactly the allowance.
	SpoolPerTransfer = 64 << 20

	// SpoolCredentialShare is the share of the node's spooling one
	// credential's requests may hold at once: a quarter of the allowance's
	// bytes (SpoolCredentialBytes) and of the waiting room
	// (SpoolWaitersPerCredential). A spool's bytes are held from its first
	// byte until the answer's first byte is written — through its sync (a cold
	// one included), upload-pack's read of it, and, for a fetch, pack-objects'
	// run before its pack begins (at most a progress line or one keepalive, 5
	// s, for a side-band client; body.go) — so without a cap one credential
	// could hold the node's whole allowance in the requests of a few CI
	// runners or a few slow clients at almost no cost to itself. A quarter
	// still lets a CI fleet behind one token spool a quarter of the node's
	// room at once — the rest WAIT THEIR TURN, never refused for it (spool.go)
	// — and leaves three quarters to everybody else. WHAT IT COSTS is that
	// wait: the sync of the requests ahead plus each one's pack-objects before
	// its pack — behind a cold sync of a minute or two the waits past the
	// quarter reach Timing.SpoolWait first and are answered 503, so N lazy
	// fetches of B bytes behind one token fail on a cold node once N × B passes
	// the quarter — thirteen of 20 MB on a four-core node's 256 MiB
	// (TestACredentialsEndedSpoolsOutlastingTheWait); a token per runner moves
	// that to the node's whole room.
	SpoolCredentialShare = 4

	// MaxCommandSectionBytes bounds a push's command section — the ref
	// updates before the pack, and the push's options after them, which
	// receive-pack holds in its memory with no bound of git's: one
	// mebibyte. code.MaxCommandsPerPush commands naming refs the engine takes
	// — at most code.MaxRefNameBytes, each line its two ids beside the ref,
	// the first its capability list — come to about 340 KiB, and a change's
	// options at their longest (code.ParseChangeOptions) to some 33 KiB. So a
	// section past a mebibyte necessarily holds refs the engine refuses
	// bad_ref (a pkt-line carries one of up to 64 KiB, and seventeen such are
	// enough), lines that parse as no command, or options of a forge's that a
	// push to the code host has no reason to send, and refusing it whole
	// costs no push the engine would have taken.
	MaxCommandSectionBytes = 1 << 20

	// maxEngineCommits is the most commits committed as the engine
	// (landing.EngineCommitter) one coding box's pack may hold for the host to
	// ask the repository about (forged): a thousand. A box's git commits under
	// the identity it was given, and every amend, rebase and cherry-pick
	// rewrites the committer to it, so a commit its pack holds under the
	// engine's name is either a forgery or one of the engine's own writes the
	// box brings back — a seat branch the repository lost its ref to
	// (deleted, or expired) that the box's old checkout still holds, one
	// commit a tool write. A thousand is a seat's whole pushing for some
	// thirty-three days at the reference company's thirty a day
	// (config.DefaultCodePushesPerSeatDay's reckoning), every push of it a
	// tool write on that one branch. They are asked in ONE cat-file whether
	// the repository holds them (cache.Cache.Missing, some 14 ms a thousand
	// ids by gitproc's measured section) and kept by the scan until then
	// (committedAs), forty hex digits each, so the bound is what one push may
	// make the node keep and ask, never what a box sends: the scan ends at the
	// first such commit past it, and the push is refused forged_committer with
	// its remedy — push the history back in parts, since a part once landed is
	// reached by a ref, and the box's git sends the next part without it.
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

	// StoppingRetryAfter is what a stopping code host answers every request
	// with: api.DrainRetryAfter, what takes a stopping node out of a
	// balancer's rotation — held equal by the API's
	// TestTheGitStopRetryIsTheDrainRetry, since this package imports no api.
	StoppingRetryAfter = 30 * time.Second

	// BusyRetryAfter is gitproc.SlotWait: a node whose slots stayed busy for
	// a whole read attempt is one a balancer should send the retry elsewhere
	// from, and so is a node a fetch waited its whole turn at for room to
	// spool.
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
// Charged as each request's in-memory part grows and given back at the
// answer's first byte (a fetch's is its pack's, after pack-objects; body.go),
// so it bounds what requests WAITING for their sync
// and their transfer slot hold, however many arrive at once; a request that
// finds it spent has the rest spooled, never refused for it. Per slot for
// SpoolPerTransfer's reason: the lane is what puts a request to use. ONLY the
// requests held in memory are charged here: a request that goes to the spool
// takes its head there with it — written into its spool once it starts, held
// in a place of the spool's waiting room (SpoolWaiters) until then — because
// charged here, a queue of large fetches behind one token spent the allowance
// and sent every other request on the node, an ls-refs of a few hundred bytes
// among them, to the spool and its queue.
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

// SpoolCredentialBytes is the most one credential's spools may hold at once
// on a transfer lane of transfers slots: SpoolCredentialShare of the
// allowance.
func SpoolCredentialBytes(transfers int) int64 {
	return SpoolAllowance(transfers) / SpoolCredentialShare
}

// SpoolRequestBytes is the most ONE request spools on a transfer lane of
// transfers slots: SpoolPerTransfer, and never past a credential's bytes —
// 64 MiB, about 1.3 million wants, on every lane of four slots or more, and a
// quarter of the allowance on a smaller one. A request past it is refused 413
// at once. It is also what a body whose client declared no length — every
// spooled request git sends — CLAIMS while it arrives (spool.go): the room
// the bodies behind the oldest one arriving keep free, so the bodies arriving
// may hold all but this much of the allowance, and of a credential's bytes,
// before one waits on another — 960 MiB of a four-core node's gibibyte, and
// 192 MiB of a credential's 256. Held below the credential's bytes on every
// lane of four slots or more for that reason: a claim of the credential's
// whole share leaves it no room for a second body while the first arrives,
// which is what a lane narrower than four slots gives a credential.
func SpoolRequestBytes(transfers int) int64 {
	return min(SpoolPerTransfer, SpoolCredentialBytes(transfers))
}

// SpoolWaiters is the node's WAITING ROOM on a transfer lane of transfers
// slots: how many fetch requests may wait at once to start spooling,
// SpoolCredentialShare places a slot. A place holds what its request read
// into memory before it was known to need the spool — up to
// RequestMemoryBytes — so the room is a memory bound, 4 MiB a transfer slot,
// beside RequestMemoryAllowance and never in it: 64 MiB on a four-core node's
// sixteen slots and 256 MiB on sixteen cores' sixty-four, out of the 896 MiB
// a core a git node keeps for serving (cache.ServingMemoryPerCore). Past it a
// request that must wait is answered 503 at once; one that need not wait
// never takes a place (spool.go).
func SpoolWaiters(transfers int) int {
	return SpoolCredentialShare * max(transfers, 1)
}

// SpoolWaitersPerCredential is how many of one credential's requests may wait
// at once to start spooling, on a transfer lane of transfers slots:
// SpoolCredentialShare of the waiting room, the lane's own width — so a burst
// behind one token as wide as the node runs transfers at once waits whole
// beside the requests it has spooling. WHAT IT COSTS: a token with more large
// fetches waiting to start at once than that — 16 on a four-core node, 64 on
// sixteen cores — has the excess refused 503 at once, and git does not retry
// a lazy fetch, so those checkouts fail: a token for each CI runner ends it.
func SpoolWaitersPerCredential(transfers int) int {
	return SpoolWaiters(transfers) / SpoolCredentialShare
}

// HookBudget bounds what a push's hook session does inside one receive-pack —
// its landing (land) — as gitproc.ReceivePackDeadline's hook term: the three
// scans over the stored pack (force, blobs, introduced — each
// gitproc.PackWorkDeadline of the largest pack one push may store,
// code.MaxPackBytes), the bundle's put under its own bound at that size
// (code.UploadDeadline: objstore.ReadBase and the pace floor every leg is
// held to), ProposeBudget, and the pending wait — summed, though the
// introduced listing runs beside the put, because a bound on the two together
// is no less than the longer of them. The put's term IS the bound the put is
// given (code.StoreBundle), read from the one function, so the hook never
// kills a receive-pack whose upload is still inside its own deadline.
// Over code.MaxPackBytes rather than the epoch's push limit, because what the
// landing scans and stores is the client's input COMPLETED against the
// repository plus its indexes, which a thin push can grow past
// max_push_bytes. A BOUND on a trickling client, never a schedule: a push that
// stops moving is ended by its Timing.Stall long before it.
func HookBudget(t Timing) time.Duration {
	return 3*gitproc.PackWorkDeadline(code.MaxPackBytes) + code.UploadDeadline(code.MaxPackBytes) +
		ProposeBudget + t.PendingGrace
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
