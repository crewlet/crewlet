package gitlet

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// AN IMPORT AND A PULL MIRROR, LANDED: what a repository's home fetched from
// another host lands as records on the repository's subject — an import's
// chunks ([ImportChunk]) and end ([ImportDone]), a pull mirror's syncs
// ([MirrorSync]) — each proposed to its committer ([Committer.ProposeImport],
// [Committer.ProposeMirrorSync]) and decided alone in one snapshot through the
// repository's one queue, as a compaction's and an expunge's are (S27), never
// gathered with pushes: a record carries one op. A chunk or a sync lands as ONE
// push through the apply every push takes ([Applier.landPushes]): its push row
// `via import` or `via mirror`, by [PrincipalSystem], its push id the record's;
// its packs (kind import or mirror, several to a record) named by no push row; a
// code_ref_drops row per command leaving an object (M6-D2). A sync that moves
// nothing lands no push. The rules are [decideRemote]'s.

// remoteWrite is one import chunk, import end or mirror sync as the committer
// queues it: exactly one set, which [ImportProposal.Validate] and
// [MirrorSyncProposal.Validate] hold it to.
type remoteWrite struct {
	chunk  *ImportChunk
	done   *ImportDone
	mirror *MirrorSync
	// doneID is an end's operation id, minted by the committer that queues
	// it: an end carries no id of its own, and its idempotence is its Seq's
	// precondition — a second end of one import is refused stale.
	doneID string
}

// Validate refuses, before it is queued and naming the field
// ([ErrInvalidProposal]), a proposal no committer could decide: anything but
// exactly one of a chunk and an end; a repository id not in its canonical
// spelling; a chunk whose id is no bare UUIDv7, whose node config does not
// admit, whose Seq is below one, that carries neither a pack nor a command (packs
// alone land what a later chunk's refs need), whose packs pass MaxImportPacks or
// are not each what [PackRef.Check] admits under [DeriveOutputPackID]'s id, or
// whose commands pass MaxImportRefs, are not what a receiver may send
// ([validCommands]) undecided, or name a ref no push may ([StatelessRefusal]) or
// a seat's namespace (only a seat's own push may); an end whose node config does
// not admit, whose Seq is below zero, whose DefaultBranch is no branch's name or
// whose Error is past one sentence; and a proposal past what one record may carry.
func (p ImportProposal) Validate() error {
	_, err := p.write().measure()
	return err
}

// Validate refuses a sync no committer could decide ([ErrInvalidProposal]):
// a chunk's rules over its repository, id, node, packs and commands — but a
// sync carrying neither a pack nor a command is one ([MirrorSync.Current]),
// and one carrying packs and no command is not — a drop epoch below zero and
// a zero FetchedAt.
func (p MirrorSyncProposal) Validate() error {
	_, err := p.write().measure()
	return err
}

// RepoID is the repository p is about, "" for one that carries neither — the
// ONE answer the router keys p by and the committer queues it under.
func (p ImportProposal) RepoID() string {
	switch {
	case p.Chunk != nil:
		return p.Chunk.RepoID
	case p.Done != nil:
		return p.Done.RepoID
	}
	return ""
}

// RepoID is the repository p syncs.
func (p MirrorSyncProposal) RepoID() string { return p.Sync.RepoID }

// write is p as the committer queues it.
func (p ImportProposal) write() remoteWrite { return remoteWrite{chunk: p.Chunk, done: p.Done} }

// write is p as the committer queues it.
func (p MirrorSyncProposal) write() remoteWrite {
	s := p.Sync
	return remoteWrite{mirror: &s}
}

// repoID is the repository w is about.
func (w remoteWrite) repoID() string {
	switch {
	case w.chunk != nil:
		return w.chunk.RepoID
	case w.done != nil:
		return w.done.RepoID
	case w.mirror != nil:
		return w.mirror.RepoID
	}
	return ""
}

// kind is what w is, as the busy count names it.
func (w remoteWrite) kind() string {
	if w.mirror != nil {
		return kindMirrorSync
	}
	return kindImport
}

// again is what w's asker does after a refusal that decided nothing: the
// repository's home reads the rows and fetches again at its next round.
func (remoteWrite) again() string { return "its home reads the repository again at its next round" }

// record is the record w is: its op, its operation id and its payload as
// proposed — a chunk's and a sync's commands still undecided.
func (w remoteWrite) record() (OpKind, string, any) {
	switch {
	case w.chunk != nil:
		return OpImportChunk, w.chunk.ChunkID, *w.chunk
	case w.done != nil:
		return OpImportDone, w.doneID, *w.done
	case w.mirror != nil:
		return OpMirrorSync, w.mirror.SyncID, *w.mirror
	}
	return "", "", nil
}

// moves is w's packs and commands: a chunk's or a sync's, none for an end.
func (w remoteWrite) moves() ([]PackRef, []Command) {
	switch {
	case w.chunk != nil:
		return w.chunk.Packs, w.chunk.Commands
	case w.mirror != nil:
		return w.mirror.Packs, w.mirror.Commands
	}
	return nil, nil
}

// decidedRows is the most rows w's record makes its apply touch before the
// changes its commands move, which the decide counts from the rows: an end's
// and a sync's that moves nothing one, the repository's row; a chunk's or a
// sync's its push entry's ([entryRows] of every command ok), its packs, the
// repository's row, and [recordRows]' share of the horizons.
func (w remoteWrite) decidedRows() int {
	packs, cmds := w.moves()
	if w.done != nil || w.mirror != nil && w.mirror.Current() {
		return 1
	}
	entry := PushEntry{Commands: slices.Clone(cmds)}
	for i := range entry.Commands {
		entry.Commands[i].Result = ResultOK
	}
	return recordRows(entryRows(entry) + len(packs) + 1)
}

// measure validates w and answers its queued byte charge: the payload its
// record carries, encoded at its widest — every command decided ok — held
// under [EntryRoom].
func (w remoteWrite) measure() (int, error) {
	if err := w.validFields(); err != nil {
		return 0, err
	}
	n, err := w.encoded()
	if err != nil {
		return 0, err
	}
	if n > EntryRoom {
		op, _, _ := w.record()
		return 0, fmt.Errorf("%w: %s: %d bytes, past the %d one proposal may take of its record",
			ErrInvalidProposal, op, n, EntryRoom)
	}
	return n, nil
}

// encoded is the bytes of the payload w's record carries, every command
// decided ok — the one measure [EntryRoom] holds a proposal to, which
// [remoteWrite.fitted] cuts a record's commands by.
func (w remoteWrite) encoded() (int, error) {
	op, _, payload := w.record()
	switch p := payload.(type) {
	case ImportChunk:
		p.Commands = decidedOK(p.Commands)
		payload = p
	case MirrorSync:
		p.Commands = decidedOK(p.Commands)
		payload = p
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return 0, fmt.Errorf("%w: %s: encode it: %w", ErrInvalidProposal, op, err)
	}
	return len(encoded), nil
}

// Fitted is p's chunk as ONE record carries it — its packs, and the most of its
// commands, from the first, that fit beside them — held to every rule of
// [ImportProposal.Validate] with each pack measured AS STORED though it may only
// be laid out ([LayBundle]): a home cuts and checks a step before uploading a
// byte, and the committer's Validate after the upload measures the same record.
// An end, or a proposal carrying no chunk, is p as Validate judges it.
func (p ImportProposal) Fitted() (ImportProposal, error) {
	if p.Chunk == nil || p.Done != nil {
		return p, p.Validate()
	}
	n, err := p.write().fitted()
	if err != nil {
		return ImportProposal{}, err
	}
	c := *p.Chunk
	c.Commands = c.Commands[:n]
	return ImportProposal{Chunk: &c}, nil
}

// Fitted is p's sync as ONE record carries it, on [ImportProposal.Fitted]'s
// terms.
func (p MirrorSyncProposal) Fitted() (MirrorSyncProposal, error) {
	n, err := p.write().fitted()
	if err != nil {
		return MirrorSyncProposal{}, err
	}
	p.Sync.Commands = p.Sync.Commands[:n]
	return p, nil
}

// fitted is how many of w's commands, from the first, its record carries: at
// most MaxImportRefs, and the most whose encoding ([remoteWrite.encoded]) stays
// within EntryRoom beside every pack, w cut to them valid by every rule of
// [remoteWrite.measure], its packs as stored. THE COUNT ALONE DOES NOT BOUND THE
// BYTES: JSON spells `&`, `<` and `>` in six bytes each, so some six hundred refs
// at MaxRefNameBytes pass EntryRoom, and a chunk cut by count would be refused
// at every attempt, after its upload. The encoding grows with every command, so
// the cut is found by halving (about ten encodings). A packless record whose
// first command alone does not fit is an error, never a record of none, which
// would land nothing for ever.
func (w remoteWrite) fitted() (int, error) {
	w = w.asStored()
	packs, cmds := w.moves()
	hi := min(len(cmds), MaxImportRefs)
	past := sort.Search(hi+1, func(n int) bool {
		size, err := w.first(n).encoded()
		return err != nil || size > EntryRoom
	})
	n := max(past-1, 0)
	if n == 0 && len(cmds) > 0 && len(packs) == 0 {
		op, _, _ := w.record()
		return 0, fmt.Errorf("%w: %s: its first command, %q, alone is past the %d bytes one proposal may take "+
			"of its record", ErrInvalidProposal, op, cmds[0].Ref, EntryRoom)
	}
	if _, err := w.first(n).measure(); err != nil {
		return 0, err
	}
	return n, nil
}

// first is w with only its first n commands.
func (w remoteWrite) first(n int) remoteWrite {
	switch {
	case w.chunk != nil:
		c := *w.chunk
		c.Commands = c.Commands[:n]
		w.chunk = &c
	case w.mirror != nil:
		m := *w.mirror
		m.Commands = m.Commands[:n]
		w.mirror = &m
	}
	return w
}

// storedStandIn is the object a pack not yet stored is measured and checked
// as: a key and a digest at the widths every stored one has — a version 7
// key's canonical 36 characters, a SHA-256's 64 hex digits — which are the
// only fields [StoreBundle] fills in, so a record carrying it encodes to
// exactly the bytes the record carrying the stored pack will.
var storedStandIn = objstore.Object{Key: objstore.KeyAt(time.UnixMilli(0)), Hash: objstore.HashOf(nil)}

// asStored is w with every pack that names no object yet given
// [storedStandIn]'s.
func (w remoteWrite) asStored() remoteWrite {
	stored := func(packs []PackRef) []PackRef {
		out := slices.Clone(packs)
		for i := range out {
			if out[i].Object.IsZero() {
				out[i].Object, out[i].Hash = storedStandIn.Key, storedStandIn.Hash
			}
		}
		return out
	}
	switch {
	case w.chunk != nil:
		c := *w.chunk
		c.Packs = stored(c.Packs)
		w.chunk = &c
	case w.mirror != nil:
		m := *w.mirror
		m.Packs = stored(m.Packs)
		w.mirror = &m
	}
	return w
}

// decidedOK is cmds each decided ok, as a record that lands carries them.
func decidedOK(cmds []Command) []Command {
	out := slices.Clone(cmds)
	for i := range out {
		out[i].Result = ResultOK
	}
	return out
}

// validFields is every rule of the proposals' Validate but the record's bytes.
func (w remoteWrite) validFields() error {
	set := 0
	for _, isSet := range []bool{w.chunk != nil, w.done != nil, w.mirror != nil} {
		if isSet {
			set++
		}
	}
	if set != 1 {
		return fmt.Errorf("%w: an import proposal is exactly one of a chunk and an end, and this one sets %d",
			ErrInvalidProposal, set)
	}
	switch {
	case w.chunk != nil:
		c := w.chunk
		if c.Seq < 1 {
			return badField("chunk.seq", "%d; an import's first chunk is 1", c.Seq)
		}
		return validMoves("chunk", c.RepoID, "chunk_id", c.ChunkID, c.Node, c.Packs, c.Commands)
	case w.mirror != nil:
		m := w.mirror
		switch {
		case m.DropEpoch < 0:
			return badField("sync.drop_epoch", "%d; a repository's drop epoch is never below zero", m.DropEpoch)
		case m.FetchedAt.IsZero():
			return badField("sync.fetched_at", "a sync names when its home fetched the source")
		}
		return validMoves("sync", m.RepoID, "sync_id", m.SyncID, m.Node, m.Packs, m.Commands)
	}
	d := w.done
	if err := validRepoID("done.repo_id", d.RepoID); err != nil {
		return err
	}
	switch {
	case !config.ValidNodeID(d.Node):
		return badField("done.node", "%q is not a node id", d.Node)
	case d.Seq < 0:
		return badField("done.seq", "%d; an import lands no chunk at the least", d.Seq)
	case d.DefaultBranch != "" && !ValidRef(branchPrefix+d.DefaultBranch):
		return badField("done.default_branch", "%q is not a branch's name", d.DefaultBranch)
	case len(d.Error) > maxDetailBytes:
		return badField("done.error", "%d bytes, past the %d of one sentence", len(d.Error), maxDetailBytes)
	case !utf8.ValidString(d.Error):
		return badField("done.error", "not valid UTF-8")
	}
	return nil
}

// validMoves is the rules a chunk and a sync share, the record's own id named
// by idField: a repository id, the id a bare UUIDv7, a node config admits,
// packs derived from the id that a bundle check admits, and commands a
// receiver could send, undecided, in a namespace an import may set — a sync
// landing no pack without a ref, a chunk landing a pack or setting a ref.
func validMoves(what, repoID, idField, id, node string, packs []PackRef, cmds []Command) error {
	if err := validRepoID(what+".repo_id", repoID); err != nil {
		return err
	}
	if err := bareUUIDv7(what+"."+idField, id); err != nil {
		return err
	}
	switch {
	case !config.ValidNodeID(node):
		return badField(what+".node", "%q is not a node id", node)
	case len(packs) > MaxImportPacks:
		return badField(what+".packs", "%d packs, past the %d one record lands (MaxImportPacks)", len(packs),
			MaxImportPacks)
	case len(cmds) == 0 && len(packs) > 0 && what == "sync":
		// A SYNC'S PACKS RIDE WITH THE REFS THAT REACH THEM: one that
		// moves no ref found its source level, and carries nothing at all
		// ([MirrorSync.Current]).
		return badField(what+".packs", "a sync that moves no ref lands no pack")
	case len(cmds) == 0 && len(packs) == 0 && what == "chunk":
		// A CHUNK OF PACKS ALONE lands what the refs of a later chunk
		// need, but one that lands nothing and sets nothing would only
		// write a push row of nothing: an import with no refs at all is
		// ended at Seq 0, with no chunk.
		return badField(what+".commands", "a chunk lands a pack or sets a ref at the least: one that does "+
			"neither is no record")
	case len(cmds) > MaxImportRefs:
		return badField(what+".commands", "%d refs, past the %d one record sets (MaxImportRefs)", len(cmds),
			MaxImportRefs)
	}
	if err := derivedOutputs(what+".packs", id, 0, packs); err != nil {
		return err
	}
	if err := validCommands(cmds); err != nil {
		return err
	}
	for i, c := range cmds {
		field := fmt.Sprintf("%s.commands[%d]", what, i)
		switch {
		case c.Result != "":
			return badField(field+".result", "%q — its committer decides every command", c.Result)
		case StatelessRefusal(c) != "":
			return badField(field+".ref", "%q: %s — an import sets only what a push may", c.Ref,
				StatelessRefusal(c))
		case c.Ref == SeatNamespaceRoot || strings.HasPrefix(c.Ref, SeatNamespaceRoot+"/"):
			// A SEAT'S BRANCH IS ITS OWN: its row names the seat that set
			// it, and an import is nobody's push.
			return badField(field+".ref", "%q is in the seats' namespace (%s/), which only a seat's own push "+
				"sets", c.Ref, SeatNamespaceRoot)
		}
	}
	return nil
}

// ProposeImport decides one import record — a chunk or the import's end — as a
// batch of its own in the repository's queue, on the queue's terms
// ([Committer.ProposeExpunge]); ctx bounds only this caller's wait. A chunk's
// operation id is its ChunkID, minted for the attempt, and an end's one this
// committer mints: each is idempotent by its Seq's precondition, so its home may
// ask a runner-up the same proposal after this node went silent, and a record
// landed meanwhile answers stale. A chunk's applied is answered from its push
// row, never from the decision.
func (c *Committer) ProposeImport(ctx context.Context, p ImportProposal) (SyncResult, error) {
	w := p.write()
	if w.done != nil {
		w.doneID = statelog.NewOpID(c.clock(), "")
	}
	return c.proposeRemote(ctx, w)
}

// ProposeMirrorSync decides one pull mirror's sync as a batch of its own in
// the repository's queue, on [Committer.ProposeImport]'s terms; its operation
// id is its SyncID, and its applied is answered from its push row.
func (c *Committer) ProposeMirrorSync(ctx context.Context, p MirrorSyncProposal) (SyncResult, error) {
	return c.proposeRemote(ctx, p.write())
}

// proposeRemote queues w and waits for its answer.
func (c *Committer) proposeRemote(ctx context.Context, w remoteWrite) (SyncResult, error) {
	bytes, err := w.measure()
	if err != nil {
		return SyncResult{}, err
	}
	deadline, _ := ctx.Deadline()
	a := c.queue(ctx, &queued{remote: &w, bytes: bytes, rows: w.decidedRows(), arrived: c.clock(),
		deadline: deadline, gone: ctx.Done(), answer: make(chan answered, 1)})
	return a.remote, a.err
}

// remoteDecision is one round's decision of an import or mirror record: its
// refusal and sentence where it is refused, the payload it forms otherwise —
// a chunk's and a sync's commands decided ok — and the version the
// repository's row was read at either way.
type remoteDecision struct {
	refusal Refusal
	detail  string
	payload any
	version int64
}

// decideRemote is one round's decision of w over the rows tx holds, pure over
// (the rows, w, retention, limits, now): the first rule that fails is the
// answer, and none writes. Every record first: an absent, deleted or purging
// repository is repo_deleted ([RepoGone]). A CHUNK lands only in its turn — the
// repository importing and its Seq import_seq plus one, else stale: one landed
// twice or out of order would set refs onto objects whose packs have not landed
// — then [decideMoves]. An END is [decideImportDone]'s. A SYNC needs the
// repository active (importing is repo_importing, archived repo_archived) and a
// pull mirror, else stale; then its DropEpoch the repository's, and
// [decideMoves].
func decideRemote(ctx context.Context, tx *sql.Tx, w remoteWrite, retention time.Duration, limits Limits,
	now time.Time) (remoteDecision, error) {

	repoID := w.repoID()
	repo, found, err := pushRepoIn(ctx, tx, repoID)
	if err != nil {
		return remoteDecision{}, err
	}
	d := remoteDecision{version: repo.version}
	if !found || repo.status == RepoStatusDeleted || repo.status == RepoStatusPurging {
		if d.detail, err = goneDetail(ctx, tx, repoID, repo, found, retention); err != nil {
			return remoteDecision{}, err
		}
		d.refusal = RefusalRepoDeleted
		return d, nil
	}
	stale := func(format string, args ...any) (remoteDecision, error) {
		d.refusal, d.detail = RefusalStale, fmt.Sprintf(format, args...)+"; "+w.again()
		return d, nil
	}
	switch {
	case w.done != nil:
		return decideImportDone(ctx, tx, *w.done, repo, d)
	case w.chunk != nil:
		c := *w.chunk
		switch {
		case repo.status != RepoStatusImporting:
			return stale("%s is %s, not being imported: its import ended", repo.name, repo.status)
		case c.Seq != repo.importSeq+1:
			return stale("%s's import has landed %d chunks, and this is chunk %d: a chunk lands only in its turn",
				repo.name, repo.importSeq, c.Seq)
		}
		//nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		cmds, refusal, detail, err := decideMoves(ctx, tx, repoID, repo, c.Packs, c.Commands, limits, now)
		if err != nil || refusal != "" {
			d.refusal, d.detail = refusal, detail
			return d, err
		}
		c.Commands = cmds
		d.payload = c
		return d, nil
	}
	m := *w.mirror
	switch {
	case repo.status == RepoStatusImporting:
		d.refusal, d.detail = RefusalRepoImporting, ImportingDetail(repo.name)
		return d, nil
	case repo.status == RepoStatusArchived:
		d.refusal, d.detail = RefusalRepoArchived, ArchivedDetail(repo.name, repo.managedBy)
		return d, nil
	case !repo.mirrored:
		return stale("%s follows no source: a settings change took its pull mirror away", repo.name)
	case m.Current():
		// LEVEL WITH ITS SOURCE: no object for an epoch to judge and no ref
		// to be stale, so it records only mirror_synced_at. Without it a quiet
		// source's mirror would head `mirrors_due` for ever, past
		// MirrorInterval's stop, and every Gitlet node would page and fetch it
		// again on every backstop.
		d.payload = m
		return d, nil
	case m.DropEpoch != repo.dropEpoch:
		// M6-D4: ITS PACKS WERE CUT against objects an expunge may since have
		// left dropping, so it is recorded nowhere and its home builds again
		// over a copy at the new epoch.
		d.refusal, d.detail = RefusalExpunging, fmt.Sprintf("an expunge of %s's unreachable objects moved "+
			"its drop epoch to %d after this sync's packs were cut at %d: nothing was decided; %s", repo.name,
			repo.dropEpoch, m.DropEpoch, w.again())
		return d, nil
	}
	cmds, refusal, detail, err := decideMoves(ctx, tx, repoID, repo, m.Packs, m.Commands, limits, now)
	if err != nil || refusal != "" {
		d.refusal, d.detail = refusal, detail
		return d, err
	}
	m.Commands = cmds
	d.payload = m
	return d, nil
}

// decideImportDone is an end's rules over the repository's row as repo holds
// it: the repository importing at import_seq Seq, and its DefaultBranch, where
// set, a branch the rows hold while the default branch the repository declares
// is not — else stale: the declared one is kept wherever the import brought it.
func decideImportDone(ctx context.Context, tx *sql.Tx, e ImportDone, repo pushRepo,
	d remoteDecision) (remoteDecision, error) {

	again := remoteWrite{}.again()
	switch {
	case repo.status != RepoStatusImporting:
		d.refusal, d.detail = RefusalStale, fmt.Sprintf("%s is %s, not being imported: its import ended "+
			"already; %s", repo.name, repo.status, again)
		return d, nil
	case e.Seq != repo.importSeq:
		d.refusal, d.detail = RefusalStale, fmt.Sprintf("%s's import has landed %d chunks, and its end "+
			"names %d; %s", repo.name, repo.importSeq, e.Seq, again)
		return d, nil
	}
	if e.DefaultBranch != "" {
		refs, err := refsNamed(ctx, tx, e.RepoID, []string{branchPrefix + e.DefaultBranch,
			branchPrefix + repo.defaultBranch})
		if err != nil {
			return remoteDecision{}, fmt.Errorf("gitlet: read the default branches of %s's import: %w", repo.name, err)
		}
		switch {
		case refTip(refs, branchPrefix+e.DefaultBranch) == ZeroOID:
			d.refusal, d.detail = RefusalStale, fmt.Sprintf("%s holds no branch %s, which the import's end "+
				"makes its default; %s", repo.name, e.DefaultBranch, again)
			return d, nil
		case e.DefaultBranch != repo.defaultBranch && refTip(refs, branchPrefix+repo.defaultBranch) != ZeroOID:
			d.refusal, d.detail = RefusalStale, fmt.Sprintf("%s holds %s, the default branch it declares, "+
				"which the import keeps; %s", repo.name, repo.defaultBranch, again)
			return d, nil
		}
	}
	d.payload = e
	return d, nil
}

// decideMoves is a chunk's and a sync's rules after the repository's: its
// commands decided ok, or the whole record refused, unrecorded. Every command's
// Old must be the ref as the rows hold it, else stale (a chunk: no write but the
// import's moves an importing repository; a sync: its next round reads both
// again). Then the push rules, by the push decide's own working state
// ([working]): ref_conflict, repo_refs, every pack [PackRef.Check] admits with
// its key minted within objstore.RecordWithin (pack_too_large, upload_expired),
// and repo_size (code.limits.repo_max_bytes). A blob past
// code.limits.max_blob_bytes is the fetching home's to judge, ending the import
// with [ImportDone.Error]. A record past MaxRecordRows is the builder's bug,
// refused before any record ([ErrRecordCap]): MaxImportRefs and MaxImportPacks
// keep one from forming.
func decideMoves(ctx context.Context, tx *sql.Tx, repoID string, repo pushRepo, packs []PackRef,
	cmds []Command, limits Limits, now time.Time) ([]Command, Refusal, string, error) {

	again := remoteWrite{}.again()
	w, err := newWorking(ctx, tx, repoID, repo, []Proposal{{RepoID: repoID, Commands: cmds}}, []int{0}, now)
	if err != nil {
		return nil, "", "", err
	}
	out := slices.Clone(cmds)
	made, gone := map[string]bool{}, map[string]bool{}
	var creating []int
	deletes := 0
	for i := range out {
		c := &out[i]
		if current := w.current(c.Ref); current != c.Old {
			return nil, RefusalStale, fmt.Sprintf("%s is at %s, and the record moves it from %s; %s", c.Ref,
				short(current), short(c.Old), again), nil
		}
		c.Result = ResultOK
		switch {
		case c.Old == ZeroOID:
			creating = append(creating, i)
		case c.New == ZeroOID:
			deletes++
			gone[c.Ref] = true
		}
	}
	// THE PUSH'S BOUNDS, in its order: a create beside a live ref, then the
	// live refs — the record's own deletions freeing room first.
	creates := 0
	for _, i := range creating {
		c := out[i]
		if other, ok := w.beside(c.Ref, made, gone); ok {
			return nil, RefusalRefConflict, conflictDetail(other), nil
		}
		if held := w.live - deletes + creates; held >= MaxRefsPerRepo {
			return nil, RefusalRepoRefs, refsDetail(repo.name, held, MaxRefsPerRepo, Grant{}), nil
		}
		creates++
		made[c.Ref] = true
	}
	_, refusal, detail := decideOutputs(packs, now)
	if refusal != "" {
		return nil, refusal, detail, nil
	}
	var size int64
	for _, p := range packs {
		size += p.Size
	}
	if w.bytes+size > limits.RepoMaxBytes {
		return nil, RefusalRepoSize, sizeDetail(repo.name, w.bytes+size, limits.RepoMaxBytes, limits, Grant{}), nil
	}
	// THE RECORD'S ROWS, with the changes its commands move — counted by the
	// model its apply writes them from.
	entry := PushEntry{PushID: "", Principal: Principal{Kind: PrincipalSystem}, Commands: out}
	book, err := readChangeBook(ctx, tx, repoID, []PushEntry{entry}, 0, 0)
	if err != nil {
		return nil, "", "", fmt.Errorf("gitlet: the record's changes on %s: %w", repo.name, err)
	}
	changeRows, _ := book.alone(entry)
	if rows := recordRows(entryRows(entry) + len(packs) + 1 + changeRows); rows > MaxRecordRows {
		return nil, "", "", fmt.Errorf("%w: up to %d rows, past MaxRecordRows (%d)", ErrRecordCap, rows,
			MaxRecordRows)
	}
	return out, "", "", nil
}

// remote publishes one import or mirror record and answers it — the record's
// batch of its own, under the committer's own lifetime and one CommitBudget,
// as an expunge's ([Committer.expunge]).
func (c *Committer) remote(repoID string, q *queued) {
	ctx, cancel := context.WithTimeout(c.life, CommitBudget)
	defer cancel()
	w := *q.remote
	op, opID, _ := w.record()
	retention, limits := c.retention(), c.limits()
	subject := RepoSubject(repoID)
	scope := []string{RepoPath(repoID)}
	var (
		last    remoteDecision
		decided bool
	)
	res, err := c.pub.Publish(ctx, statelog.Request{
		Subject: subject.statelog(), Scope: statelog.ScopeSet{Paths: scope},
		OpID: opID, Pattern: statelog.PatternArbitrated,
		Decide: func(tx *sql.Tx, stamp statelog.Stamp) (statelog.Decision, error) {
			// RESET AT THE TOP OF EVERY ROUND: only the last round says
			// what the publish decided.
			last, decided = remoteDecision{}, false
			now := c.now()
			d, err := decideRemote(ctx, tx, w, retention, limits, now)
			if err != nil {
				return statelog.Decision{}, err
			}
			last, decided = d, true
			if d.refusal != "" {
				return statelog.Decision{Version: d.version}, nil
			}
			decision, err := recordFor(stamp, subject, op, scope, opID, Actor{ID: c.nodeID, Kind: ActorSystem},
				d.payload, now)
			if err != nil {
				return statelog.Decision{}, err
			}
			if len(decision.Payload) > MaxRecordBytes {
				return statelog.Decision{}, fmt.Errorf("%w: %d bytes, past MaxRecordBytes (%d)", ErrRecordCap,
					len(decision.Payload), MaxRecordBytes)
			}
			decision.Version = d.version
			return decision, nil
		},
	})
	out, answerErr := c.remoteAnswer(ctx, w, opID, last, decided, res, err)
	q.answer <- answered{remote: out, err: answerErr}
}

// remoteAnswer is an import or mirror record's answer from the publish's
// result and the last round's decision.
func (c *Committer) remoteAnswer(ctx context.Context, w remoteWrite, opID string, last remoteDecision,
	decided bool, res statelog.Result, err error) (SyncResult, error) {

	if err != nil {
		return c.refusedRemote(ctx, w, opID, err)
	}
	switch res.Outcome {
	case statelog.OutcomeApplied:
		if !res.Position.IsZero() {
			return c.appliedRemote(ctx, w, opID, res.Position)
		}
		// NOTHING WAS APPENDED: the decide refused it, and its last round
		// says why.
		if !decided || last.refusal == "" {
			return SyncResult{}, fmt.Errorf("gitlet: the publish of operation %s answered applied with no "+
				"record and decided no refusal", opID)
		}
		return SyncResult{Refusal: last.refusal, Detail: last.detail}, nil
	case statelog.OutcomePending:
		return SyncResult{Outcome: statelog.OutcomePending, Position: res.Position}, nil
	}
	return SyncResult{Outcome: statelog.OutcomeUnknown}, nil
}

// appliedRemote answers an import or mirror record that applied at at. A
// CHUNK AND A SYNC are answered FROM THE ROWS: their push row, which their
// apply writes or — met where the repository was no longer as its decide
// read it, which no correct log carries — does not: a record that applied and
// wrote no push row is logged at ERROR and answered unknown, never applied,
// which would tell its home the refs moved. An END, and a SYNC THAT MOVES
// NOTHING, are answered at their record's position: each writes the
// repository's row alone and no push row, and that row a later record may
// move on from.
func (c *Committer) appliedRemote(ctx context.Context, w remoteWrite, opID string,
	at statelog.Position) (SyncResult, error) {

	if w.done != nil || w.mirror != nil && w.mirror.Current() {
		return SyncResult{Outcome: statelog.OutcomeApplied, Position: at}, nil
	}
	var landed map[string]bool
	if err := c.db.Read(ctx, func(tx *sql.Tx) error {
		var err error
		landed, err = pushesApplied(ctx, tx, []string{opID})
		return err
	}); err != nil {
		failed := fmt.Errorf("gitlet: the record of operation %s applied at %s and its push row could not be "+
			"read: %w", opID, at, err)
		c.log.WarnContext(ctx, "code_commit_failed", "kind", w.kind(), "repo", w.repoID(), "op_id", opID,
			"error", failed)
		return SyncResult{}, failed
	}
	if !landed[opID] {
		c.log.ErrorContext(ctx, "code_remote_unanswerable", "kind", w.kind(), "repo", w.repoID(), "op_id", opID,
			"position", at.String())
		return SyncResult{Outcome: statelog.OutcomeUnknown, Detail: fmt.Sprintf("the record applied at %s "+
			"and the rows this node holds name no push %s", at, opID)}, nil
	}
	return SyncResult{Outcome: statelog.OutcomeApplied, Position: at}, nil
}

// refusedRemote answers an import or mirror record whose publish failed — the
// push's mapping in its words, as an expunge's is ([Committer.refusedExpunge]).
func (c *Committer) refusedRemote(ctx context.Context, w remoteWrite, opID string,
	err error) (SyncResult, error) {

	var refusal *statelog.Unavailable
	switch {
	case errors.Is(err, ErrInvalidProposal), errors.Is(err, ErrRecordCap):
		// THE BUILDER'S BUG: refused before any record, and said where an
		// operator reads the node that decided.
		c.log.ErrorContext(ctx, "code_remote_invalid", "kind", w.kind(), "repo", w.repoID(), "op_id", opID,
			"error", err)
		return SyncResult{}, err
	case errors.Is(err, statelog.ErrConflict):
		c.countBusy(w.repoID(), w.kind(), busyContended, 1)
		return SyncResult{Refusal: RefusalBusy, Detail: "the repository kept changing for every round the " +
			"write may take; " + w.again()}, nil
	case errors.As(err, &refusal) && refusal.Reason == statelog.ReasonDeleted:
		var at statelog.Position
		if refusal.Landed() {
			at = refusal.Position
		}
		return SyncResult{Refusal: RefusalRepoDeleted, Position: at,
			Detail: c.purgedDetail(ctx, w.repoID(), opID)}, nil
	case errors.As(err, &refusal) && refusal.Landed():
		return SyncResult{Refusal: RefusalNotApplied, Position: refusal.Position,
			Detail: fmt.Sprintf("the record applies nowhere: it was durable at %s and the code log's %s gate "+
				"drops it on every node; %s", refusal.Position, refusal.Reason, w.again())}, nil
	case errors.As(err, &refusal) && nodeRefusals[refusal.Reason]:
		return SyncResult{}, fmt.Errorf("%w: %w", ErrNotServing, refusal)
	case errors.As(err, &refusal):
		return SyncResult{Refusal: RefusalUnavailable,
			Detail: "the code log refused the write: " + strings.TrimPrefix(refusal.Error(), "statelog: ")}, nil
	}
	c.log.WarnContext(ctx, "code_commit_failed", "kind", w.kind(), "repo", w.repoID(), "op_id", opID,
		"error", err)
	return SyncResult{}, fmt.Errorf("gitlet: %s operation %s: %w", w.kind(), opID, err)
}
