package mirror

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/crewlet/crewlet/internal/events"
	"github.com/crewlet/crewlet/internal/events/types"
	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitletfault"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// share is whose transfer an attempt's fetch is in the transfer lane: the
// engine's own (gitproc's machine class), one principal for every import and
// mirror of the node, so they take one turn of the lane however many run.
var share = gitproc.Share{Class: gitproc.ShareMachine, Principal: "crewlet:mirror"}

// cutConfig is the cut's pack-objects configuration: every output in the
// stored bundle format (index version 2 and its reverse index, which
// gitlet.CheckBundle requires), and the delta window, which is the build's
// memory, held to twice max_blob_bytes, as a compaction's build is.
func cutConfig(maxBlob int64) []gitproc.KV {
	return []gitproc.KV{
		{Key: "pack.indexVersion", Value: "2"},
		{Key: "pack.writeReverseIndex", Value: "true"},
		{Key: "pack.windowMemory", Value: strconv.FormatInt(2*maxBlob, 10)},
	}
}

// fetchConfig is the fetch's configuration beside the remote's keys
// (gitproc.Cmd.Remote): every object checked as receive-pack checks a push's
// (transfer.fsckObjects), and kept as the pack it arrived in
// (fetch.unpackLimit 1: below the limit git explodes a small fetch into loose
// objects, which no `--stdin-packs` cut names).
var fetchConfig = []gitproc.KV{
	{Key: "transfer.fsckObjects", Value: "true"},
	{Key: "fetch.unpackLimit", Value: "1"},
}

// errPermanent is an import's end: a refusal another attempt would only meet
// again, which the import ends with as its import_error.
type errPermanent struct{ why string }

func (e *errPermanent) Error() string { return e.why }

// attempt is one import or sync of repoID: synced, fetched into a scratch, cut,
// checked, stored and landed step by step, the scratch removed however it
// ends. A nil error landed what it fetched or found nothing to land; a sync
// then lands a record of nothing, which moves the mirror's last sync
// (gitlet.MirrorSync.Current), so every node's listing holds it back an
// interval, not only this worker's memory. A sync that reached its source is
// counted (crewlet.code.mirror.runs{dir=pull}) by what it came to.
func (m *Mirrors) attempt(ctx context.Context, repoID string, l listed, floor statelog.Position) (err error) {
	what := map[kind]string{kindImport: "import", kindSync: "sync"}[l.kind]
	var dialed bool
	landed := 0
	defer func() {
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			m.log.Warn("code_mirror_attempt_failed", "repo", repoID, "attempt", what, "error", err,
				"detail", "the repository keeps what landed; its next attempt reads the rows again")
		}
		if l.kind != kindSync || !dialed {
			return
		}
		outcome := "current"
		switch {
		case err != nil:
			outcome = "failed"
		case landed > 0:
			outcome = "synced"
		}
		m.count(metrics.CodeMirrorRuns, metrics.Attrs{"dir": "pull", "outcome": outcome})
	}()
	s, err := m.o.Cache.Sync(ctx, repoID, syncFresh(floor, l.at))
	if err != nil {
		return fmt.Errorf("mirror: sync %s: %w", repoID, err)
	}
	defer s.Release()
	switch {
	case l.kind == kindImport && s.Repo.Status != gitlet.RepoStatusImporting:
		return nil
	case l.kind == kindSync && (s.Repo.Status != gitlet.RepoStatusActive || !s.Repo.Mirrored):
		return nil
	}
	if l.kind == kindImport && l.source.URL == "" {
		// A row importing from no source (no apply of this build writes one;
		// internal/gitlet's importsDue lists it) is ended, not dialed: there
		// is nothing to fetch, now or later.
		return m.end(ctx, repoID, l, s, nil, &errPermanent{why: "the repository's row names no source to " +
			"import from"})
	}
	limits := m.o.Limits()
	dialed = true
	remote, err := m.o.Dial(ctx, l.source.URL, l.source.Credential)
	if err != nil {
		return fmt.Errorf("mirror: dial %s's source: %w", repoID, err)
	}
	sc, err := m.o.Cache.NewScratch(ctx, s, statelog.NewOpID(m.now(), ""))
	if err != nil {
		return fmt.Errorf("mirror: a scratch for %s: %w", repoID, err)
	}
	defer sc.Remove()

	// FETCH what the copy's refs do not reach: at most what the repository
	// may still take for an import, one push's worth for a sync.
	bring := limits.MaxPushBytes
	if l.kind == kindImport {
		bring = max(limits.RepoMaxBytes-s.Repo.LiveBytes, 0)
	}
	if res, err := m.o.Git.Run(ctx, gitproc.Cmd{ //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		Dir: sc.Dir, Alternate: sc.Alternate, Class: gitproc.ClassTransfer, Share: share, Remote: &remote,
		Args: []string{"fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--no-auto-gc",
			"--no-auto-maintenance", remote.URL(), "+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"},
		Config: fetchConfig, Deadline: gitproc.RemoteDeadline(bring, s.Repo.LiveObjects),
	}); err != nil {
		return fmt.Errorf("mirror: fetch %s from %s: %w\n%s", repoID, remote, err, res.Stderr)
	}
	if loose, err := looseIn(sc.Dir); err != nil { //nolint:govet // shadow: `x, err := f()` declares x too; see .golangci.yml
		return err
	} else if loose {
		return m.end(ctx, repoID, l, s, sc, &errPermanent{why: "the source answered over git's dumb HTTP " +
			"protocol, an object at a time, which is neither checked as it arrives nor kept as a pack the " +
			"repository can store: serve it over smart HTTP"})
	}
	fetched, err := sc.Packs()
	if err != nil {
		return err
	}
	cmds, err := m.commands(ctx, s, sc)
	if err != nil {
		return err
	}
	var fetchedBytes int64
	for _, p := range fetched {
		fetchedBytes += fileSize(p.Path)
	}
	outs, err := m.cut(ctx, s, sc, fetched, fetchedBytes, limits)
	if err != nil {
		return err
	}
	if blob, err := m.largestBlob(ctx, sc, outs, fetchedBytes, limits.MaxBlobBytes); err != nil {
		return err
	} else if blob != "" {
		return m.end(ctx, repoID, l, s, sc, &errPermanent{why: blob})
	}
	if err := m.afterBuild(ctx, l.kind); err != nil {
		return err
	}
	todo := &steps{outs: outs, cmds: cmds}
	if l.kind == kindSync && todo.done() {
		// LEVEL WITH ITS SOURCE: one record of nothing, which moves only the
		// mirror's last sync.
		_, _, err := m.land(ctx, repoID, l, s, sc, nil, nil, 0)
		return err
	}
	for !todo.done() {
		outs, offered := todo.next()
		applied, took, err := m.land(ctx, repoID, l, s, sc, outs, offered, l.seq+1)
		var perm *errPermanent
		if errors.As(err, &perm) {
			// Ended at the chunks landed so far: the end's Seq is the
			// repository's import_seq.
			return m.end(ctx, repoID, l, s, sc, perm)
		}
		if err != nil || !applied {
			return err
		}
		todo.landed(len(outs), took)
		l.seq++
		landed++
	}
	if l.kind == kindImport {
		return m.end(ctx, repoID, l, s, sc, nil)
	}
	return nil
}

// syncFresh is what an attempt syncs its copy at: the further of the turn's
// floor (the record that nudged it; zero on a backstop's turn) and the version
// its listing read the row at. NEVER STALE: a pull mirror's copy as this node
// holds it can be its import's last chunk, still importing, which the attempt
// would take for a repository owed nothing.
func syncFresh(floor, at statelog.Position) statelog.Freshness {
	if before, err := at.Before(floor); err == nil && before {
		at = floor
	}
	return statelog.FloorAt(statelog.ReadSession, at)
}

// looseIn reports whether a fetch left an object LOOSE at dir, which only
// git's dumb HTTP protocol writes under fetchConfig. Such a source is refused:
// its objects reach no `--stdin-packs` cut, so a ref landed onto them would
// name objects no stored pack holds, and the dumb walker skips
// transfer.fsckObjects' check.
func looseIn(dir string) (bool, error) {
	entries, err := os.ReadDir(filepath.Join(dir, "objects"))
	if err != nil {
		return false, fmt.Errorf("mirror: read the scratch's objects: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() || len(e.Name()) != 2 || !gitref.ValidOID(e.Name()+strings.Repeat("0", 38)) {
			continue
		}
		inner, err := os.ReadDir(filepath.Join(dir, "objects", e.Name()))
		if err != nil {
			return false, fmt.Errorf("mirror: read the scratch's objects: %w", err)
		}
		if len(inner) > 0 {
			return true, nil
		}
	}
	return false, nil
}

// fileSize is the bytes of the file at path, 0 where it cannot be read: a
// bound's input, never a check.
func fileSize(path string) int64 {
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// steps is what an attempt has left to land, PACKS FIRST: every output in
// records of at most gitlet.MaxImportPacks, then the commands, the first riding
// with the last outputs. HOW MANY COMMANDS A RECORD TAKES IS THE RECORD'S TO
// SAY, never a count's: each is offered every command left and takes the most
// that fit its bytes and gitlet.MaxImportRefs (gitlet.ImportProposal.Fitted),
// so refs whose names JSON spells long (`&`, `<`, `>`, six bytes each) land in
// more records rather than in one the committer refuses at every attempt.
type steps struct {
	outs []string
	cmds []gitlet.Command
}

// done reports whether nothing is left to land.
func (s *steps) done() bool { return len(s.outs) == 0 && len(s.cmds) == 0 }

// next is the next record's outputs and the commands offered to it: outputs
// alone while more than one record's are left, then the last of them with
// every command left, then the commands left alone.
func (s *steps) next() ([]string, []gitlet.Command) {
	if len(s.outs) > gitlet.MaxImportPacks {
		return s.outs[:gitlet.MaxImportPacks], nil
	}
	return s.outs, s.cmds
}

// landed moves past a record that landed outs outputs and the first cmds of
// the commands offered to it.
func (s *steps) landed(outs, cmds int) { s.outs, s.cmds = s.outs[outs:], s.cmds[cmds:] }

// commands is every ref the source moved against the copy, by name: each of
// the scratch's refs (what the fetch wrote) the copy holds at another value or
// not at all, and each of the copy's the source no longer has, deleted. Refs
// in the seats' namespace (none of a source's to set) and names Gitlet's
// grammar refuses are left out ([Mirrors.refs]).
func (m *Mirrors) commands(ctx context.Context, s cache.Synced, sc *cache.Scratch) ([]gitlet.Command, error) {
	want, err := m.refs(ctx, sc.Dir, sc.Alternate)
	if err != nil {
		return nil, err
	}
	have, err := m.refs(ctx, s.Dir, "")
	if err != nil {
		return nil, err
	}
	var out []gitlet.Command
	for ref, oid := range want {
		if old, ok := have[ref]; !ok || old != oid {
			c := gitlet.Command{Ref: ref, Old: cmp(old, ok), New: oid}
			if ok {
				forced, err := m.forced(ctx, sc, old, oid)
				if err != nil {
					return nil, err
				}
				c.Forced = forced
			}
			out = append(out, c)
		}
	}
	for ref, old := range have {
		if _, ok := want[ref]; !ok {
			out = append(out, gitlet.Command{Ref: ref, Old: old, New: gitlet.ZeroOID})
		}
	}
	slices.SortFunc(out, func(a, b gitlet.Command) int { return strings.Compare(a.Ref, b.Ref) })
	return out, nil
}

// cmp is an old value: the ref's, or the zero id for a ref the copy lacks.
func cmp(old string, ok bool) string {
	if !ok {
		return gitlet.ZeroOID
	}
	return old
}

// refs is every branch and tag of the repository at dir and the object each
// names, less any name Gitlet's grammar refuses and the seats' namespace,
// which `--exclude` keeps from being listed at all. STREAMED (gitproc.Lines),
// never captured: gitlet.MaxRefsPerRepo refs list to some 6 MB, past
// gitproc.CaptureMax (1 MiB), which would fail every attempt on such a
// repository.
func (m *Mirrors) refs(ctx context.Context, dir, alternate string) (map[string]string, error) {
	out := map[string]string{}
	lines := gitproc.NewLines(func(line []byte) error {
		oid, ref, ok := strings.Cut(string(line), " ")
		if !ok || !gitref.ValidOID(oid) {
			return fmt.Errorf("mirror: for-each-ref printed %q", line)
		}
		if gitref.ValidRef(ref) {
			out[ref] = oid
		}
		return nil
	})
	_, err := m.o.Git.Run(ctx, gitproc.Cmd{
		Dir: dir, Alternate: alternate, Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline, Stdout: lines,
		Args: []string{"for-each-ref", "--format=%(objectname) %(refname)", "--exclude=" + gitref.SeatsRoot,
			"refs/heads/", "refs/tags/"},
	})
	if err == nil {
		err = lines.Close()
	}
	if err != nil {
		return nil, fmt.Errorf("mirror: list the refs of %s: %w", dir, err)
	}
	return out, nil
}

// forced reports whether moving a ref from old to new leaves old behind (old
// is not an ancestor of new), read in the scratch, which holds both through
// its alternate.
func (m *Mirrors) forced(ctx context.Context, sc *cache.Scratch, old, new string) (bool, error) {
	res, err := m.o.Git.Run(ctx, gitproc.Cmd{
		Dir: sc.Dir, Alternate: sc.Alternate, Class: gitproc.ClassRead, Answers: []int{1},
		Deadline: gitproc.ReadDeadline, Args: []string{"merge-base", "--is-ancestor", old, new},
	})
	switch {
	case err != nil:
		// An old value that is no commit (a tag of a tree, a blob) has no
		// history to be an ancestor in: moving it leaves it.
		var d *gitproc.Damage
		if errors.As(err, &d) {
			return false, err
		}
		return true, nil
	case res.Exit == 1:
		return true, nil
	}
	return false, nil
}

// cut writes what the fetch brought into the packs the repository stores: ONE
// `pack-objects --stdin-packs` over every pack the fetch wrote, every live pack
// of the copy excluded, cut at gitlet.CompactPackBytes; it answers its outputs'
// checksums in the order written, none where nothing is new. `--non-empty`:
// without it git writes a pack of no objects, which no record may carry
// (gitlet.PackRef.Check), so the restart after a chunk of packs alone, whose
// fetch brings only what those packs hold, would never land its refs. It runs
// in a maintenance slot the attempt WAITS FOR ([Mirrors.maintenanceSlot]), the
// fetch kept meanwhile.
func (m *Mirrors) cut(ctx context.Context, s cache.Synced, sc *cache.Scratch, fetched []cache.ScratchPack,
	fetchedBytes int64, limits gitlet.Limits) ([]string, error) {

	if len(fetched) == 0 {
		return nil, nil
	}
	var list strings.Builder
	for _, p := range fetched {
		list.WriteString("pack-" + p.Checksum + ".pack\n")
	}
	// THE PACKS THE REPOSITORY HOLDS, which the cut leaves out. A copy a swap
	// has replaced would read as none and the cut would repack everything, so
	// that read is refused (cache.ErrSuperseded) and the attempt ends, the
	// next one syncing past the swap.
	held, err := m.o.Cache.Packs(s)
	if err != nil {
		return nil, fmt.Errorf("mirror: list the packs the repository holds: %w", err)
	}
	for _, p := range held {
		list.WriteString("^pack-" + p.GitChecksum + ".pack\n")
	}
	slot, err := m.maintenanceSlot(ctx, s.Repo.ID)
	if err != nil {
		return nil, err
	}
	defer slot.Release()
	res, err := slot.Run(ctx, gitproc.Cmd{
		Dir: sc.Dir, Alternate: sc.Alternate, Class: gitproc.ClassMaintenance,
		Args: []string{"pack-objects", "--stdin-packs", "--non-empty",
			"--max-pack-size=" + strconv.FormatInt(gitlet.CompactPackBytes, 10), "--delta-base-offset", "--quiet",
			"objects/pack/pack"},
		Stdin: strings.NewReader(list.String()), Config: cutConfig(limits.MaxBlobBytes),
		Deadline: gitproc.PackWorkDeadline(fetchedBytes),
	})
	if err != nil {
		return nil, fmt.Errorf("mirror: cut what the fetch brought: %w\n%s", err, res.Stderr)
	}
	var outs []string
	lines := bufio.NewScanner(bytes.NewReader(res.Stdout))
	for lines.Scan() {
		cs := strings.TrimSpace(lines.Text())
		if !gitref.ValidOID(cs) {
			return nil, fmt.Errorf("mirror: pack-objects printed %q where it names a pack it wrote", cs)
		}
		outs = append(outs, cs)
	}
	return outs, lines.Err()
}

// The cut polls for a maintenance slot, since the lane never queues a caller
// (gitproc's Reserved refuses at once when full): a quarter of a second first,
// doubling to five seconds. A poll is one lock, so it costs nothing; the cap
// bounds how long a freed slot sits unasked, small against a maintenance
// process's seconds to minutes and the home's once-a-minute passes.
const (
	slotPollFirst = 250 * time.Millisecond
	slotPollMax   = 5 * time.Second
)

// maintenanceSlot is a maintenance-lane slot for repoID's cut, WAITED FOR under
// ctx alone. The cut follows a fetch of up to repo_max_bytes; refused, the
// attempt would remove it with its scratch and the next would download it all
// again for as long as the lane stayed busy (the hazard gitproc's Reserved
// names). NO DEADLINE OF ITS OWN: giving up is that same refetch, every
// process holding the lane ends by its own deadline, and Stop ends the wait.
func (m *Mirrors) maintenanceSlot(ctx context.Context, repoID string) (*gitproc.Reserved, error) {
	wait := slotPollFirst
	for logged := false; ; {
		slot, err := m.o.Git.Reserve()
		switch {
		case err == nil:
			return slot, nil
		case !errors.Is(err, gitproc.ErrBusy):
			return nil, fmt.Errorf("mirror: a maintenance slot for the cut of %s: %w", repoID, err)
		case !logged:
			logged = true
			m.log.Info("code_mirror_cut_waiting", "repo", repoID, "error", err,
				"detail", "the fetch is kept; the cut starts once a maintenance slot frees")
		}
		t := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, fmt.Errorf("mirror: waiting for a maintenance slot for the cut of %s: %w", repoID,
				context.Cause(ctx))
		case <-t.C:
		}
		wait = min(2*wait, slotPollMax)
	}
}

// largestBlob is why an output may not be stored (its first blob past limit,
// in one sentence) or "": ONE `cat-file --batch-check` per output over its ids,
// streamed from its index, ending at the first offender.
func (m *Mirrors) largestBlob(ctx context.Context, sc *cache.Scratch, outs []string, fetchedBytes int64,
	limit int64) (string, error) {

	for _, cs := range outs {
		ids, err := openIdxOIDs(filepath.Join(sc.Dir, "objects", "pack", "pack-"+cs+".idx"))
		if err != nil {
			return "", err
		}
		var found string
		lines := gitproc.NewLines(func(line []byte) (err error) {
			f := bytes.Fields(line)
			if len(f) != 3 || string(f[1]) != "blob" {
				return nil
			}
			n, err := strconv.ParseInt(string(f[2]), 10, 64)
			if err != nil {
				return fmt.Errorf("mirror: git printed %q as the size of %s: %w", f[2], f[0], err)
			}
			if n > limit {
				found = fmt.Sprintf("blob %s is %d bytes, past code.limits.max_blob_bytes (%d)", f[0], n, limit)
				return errFound
			}
			return nil
		})
		_, err = m.o.Git.Run(ctx, gitproc.Cmd{
			Dir: sc.Dir, Alternate: sc.Alternate, Class: gitproc.ClassRead, Stdin: ids, Stdout: lines,
			Args:     []string{"cat-file", "--batch-check=%(objectname) %(objecttype) %(objectsize)", "--buffer"},
			Deadline: gitproc.PackWorkDeadline(fetchedBytes),
		})
		_ = ids.Close()
		if errors.Is(err, errFound) {
			return found, nil
		}
		if err == nil {
			err = lines.Close()
		}
		if err != nil {
			return "", fmt.Errorf("mirror: scan the blobs of what the fetch brought: %w", err)
		}
	}
	return "", nil
}

// errFound ends a scan at its answer: not a failure.
var errFound = errors.New("mirror: found")

// permanent reports whether a chunk refused r is refused for good: a limit the
// source's content is past, which every attempt fetching it would meet again.
// The import ends on it (internal/gitlet's decide_remote.go decides so). Every
// other refusal (stale, busy, expired, unavailable) says nothing of the source
// and is tried again.
func permanent(r gitlet.Refusal) bool {
	switch r {
	case gitlet.RefusalRepoSize, gitlet.RefusalRepoRefs, gitlet.RefusalRefConflict, gitlet.RefusalPackTooLarge:
		return true
	}
	return false
}

// afterBuild, afterUpload and afterPropose are an attempt's fault points, each
// spelled where it is hit, the form gitletfault's hit-site gate reads.
func (m *Mirrors) afterBuild(ctx context.Context, k kind) error {
	if k == kindImport {
		return m.fault(ctx, gitletfault.ImportAfterBuild)
	}
	return m.fault(ctx, gitletfault.MirrorAfterBuild)
}

func (m *Mirrors) afterUpload(ctx context.Context, k kind) error {
	if k == kindImport {
		return m.fault(ctx, gitletfault.ImportAfterUpload)
	}
	return m.fault(ctx, gitletfault.MirrorAfterUpload)
}

func (m *Mirrors) afterPropose(ctx context.Context, k kind) error {
	if k == kindImport {
		return m.fault(ctx, gitletfault.ImportAfterPropose)
	}
	return m.fault(ctx, gitletfault.MirrorAfterPropose)
}

// land lands one record: outs and the most of offered, from the first, it
// carries, fitted as the committer will check it (gitlet.ImportProposal.Fitted,
// gitlet.MirrorSyncProposal.Fitted) BEFORE ANY UPLOAD, so a step no committer
// would take costs no PUT; then the outputs stored at once and the record
// proposed (an import's chunk at seq, or a sync at the copy's drop epoch).
// landed is that it applied, its outputs then adopted into this node's copy;
// took is how many of offered it carried. False with a nil error is a
// repository deleted meanwhile. A chunk refused a limit ([permanent]) is an
// *errPermanent; any other refusal, or a record not seen applied, is an error.
func (m *Mirrors) land(ctx context.Context, repoID string, l listed, s cache.Synced, sc *cache.Scratch,
	outs []string, offered []gitlet.Command, seq int64) (landed bool, took int, err error) {

	id := statelog.NewOpID(m.now(), "")
	refs := make([]gitlet.PackRef, len(outs))
	paths := make([]string, len(outs))
	for n, cs := range outs {
		paths[n] = filepath.Join(sc.Dir, "objects", "pack", "pack-"+cs+".pack")
		ref, layErr := gitlet.LayBundle(gitlet.DeriveOutputPackID(id, n), paths[n], cs)
		if layErr != nil {
			return false, 0, fmt.Errorf("mirror: lay out output %d: %w", n, layErr)
		}
		refs[n] = *ref
	}
	var (
		chunk *gitlet.ImportChunk
		sync  *gitlet.MirrorSync
	)
	if l.kind == kindImport {
		var p gitlet.ImportProposal
		p, err = gitlet.ImportProposal{Chunk: &gitlet.ImportChunk{RepoID: repoID, ChunkID: id, Node: m.o.NodeID,
			Seq: seq, Packs: refs, Commands: offered}}.Fitted()
		if err != nil {
			return false, 0, fmt.Errorf("mirror: cut a step of %s: %w", repoID, err)
		}
		chunk, took = p.Chunk, len(p.Chunk.Commands)
	} else {
		var p gitlet.MirrorSyncProposal
		p, err = gitlet.MirrorSyncProposal{Sync: gitlet.MirrorSync{RepoID: repoID, SyncID: id, Node: m.o.NodeID,
			DropEpoch: s.DropEpoch, Packs: refs, Commands: offered, FetchedAt: m.now()}}.Fitted()
		if err != nil {
			return false, 0, fmt.Errorf("mirror: cut a step of %s: %w", repoID, err)
		}
		sync, took = &p.Sync, len(p.Sync.Commands)
	}
	// Every output uploads AT ONCE, as a compaction's do (internal/gitlet/compact's
	// uploadAll gives the arithmetic). The first failure cancels the rest,
	// whose objects no row will name.
	g, gctx := errgroup.WithContext(ctx)
	for n := range outs {
		g.Go(func() (err error) {
			if err = gitlet.StoreBundle(gctx, m.o.Objects, paths[n], &refs[n], nil); err != nil {
				return fmt.Errorf("mirror: store output %d of %s: %w", n, repoID, err)
			}
			return nil
		})
	}
	if err = g.Wait(); err != nil {
		return false, 0, err
	}
	if err = m.afterUpload(ctx, l.kind); err != nil {
		return false, 0, err
	}
	// THE RECORD NAMES THE STORED OBJECTS, which only the uploads minted.
	var res gitlet.SyncResult
	if chunk != nil {
		chunk.Packs = refs
		res, err = m.o.Estate.Import(ctx, gitlet.ImportProposal{Chunk: chunk})
	} else {
		sync.Packs = refs
		res, err = m.o.Estate.MirrorSync(ctx, gitlet.MirrorSyncProposal{Sync: *sync})
	}
	if err != nil {
		return false, 0, fmt.Errorf("mirror: land a step of %s: %w", repoID, err)
	}
	if l.kind == kindImport {
		outcome := "failed"
		switch {
		case res.Refusal == "" && res.Outcome == statelog.OutcomeApplied:
			outcome = "applied"
		case permanent(res.Refusal):
			outcome = "ended"
		}
		m.count(metrics.CodeImportChunks, metrics.Attrs{"outcome": outcome})
	}
	if ferr := m.afterPropose(ctx, l.kind); ferr != nil {
		return false, 0, ferr
	}
	switch {
	case res.Refusal == gitlet.RefusalRepoDeleted:
		// Nothing is owed a repository deleted meanwhile.
		return false, 0, nil
	case l.kind == kindImport && permanent(res.Refusal):
		return false, 0, &errPermanent{why: res.Detail}
	case res.Refusal != "":
		return false, 0, fmt.Errorf("mirror: a step of %s refused %s: %s", repoID, res.Refusal, res.Detail)
	case res.Outcome != statelog.OutcomeApplied:
		// Not seen applied within the wait: a failure, so the next attempt
		// comes at RetryAfter's first minute, not a sync's interval later.
		return false, 0, fmt.Errorf("mirror: a step of %s is %s, not seen applied", repoID, res.Outcome)
	}
	if len(refs) > 0 {
		if err := m.o.Cache.Adopt(ctx, repoID, sc, refs...); err != nil {
			m.log.Info("code_mirror_adopt_failed", "repo", repoID, "error", err,
				"detail", "the packs are on their rows; the next sync downloads them")
		}
	}
	return true, took, nil
}

// end ends an import at l.seq, with why (if set) as its import_error, naming
// the source's HEAD branch where the declared default branch did not arrive in
// sc (nil for an attempt that made no scratch, which names none). A sync ends
// nothing: why is its error.
func (m *Mirrors) end(ctx context.Context, repoID string, l listed, s cache.Synced, sc *cache.Scratch,
	why *errPermanent) error {

	if l.kind != kindImport {
		if why != nil {
			return why
		}
		return nil
	}
	done := gitlet.ImportDone{RepoID: repoID, Node: m.o.NodeID, Seq: l.seq}
	if why != nil {
		done.Error = why.why
	}
	if sc != nil {
		held, err := m.refs(ctx, sc.Dir, sc.Alternate)
		if err != nil {
			return err
		}
		if _, ok := held["refs/heads/"+s.Repo.DefaultBranch]; !ok {
			if head, err := m.sourceHead(ctx, l.source); err == nil && head != "" {
				if _, ok := held["refs/heads/"+head]; ok {
					done.DefaultBranch = head
				}
			}
		}
	}
	res, err := m.o.Estate.Import(ctx, gitlet.ImportProposal{Done: &done})
	switch {
	case err != nil:
		return fmt.Errorf("mirror: end the import of %s: %w", repoID, err)
	case res.Refusal != "":
		return fmt.Errorf("mirror: the end of %s's import refused %s: %s", repoID, res.Refusal, res.Detail)
	case res.Outcome == statelog.OutcomeApplied:
		m.announceImported(ctx, repoID, l.source, done, res.Position)
	}
	return nil
}

// announceImported publishes code_imported for an import whose end, done,
// applied at at: its source, chunks and error, and the repository's refs, live
// bytes and default branch read from this node's copy synced at the end, never
// tallied from this attempt's landings, which of a resumed import are only its
// last part. Published ONCE: the end is the import's last record. A copy that
// cannot be synced there publishes nothing, logged; the row still names how
// the import ended.
func (m *Mirrors) announceImported(ctx context.Context, repoID string, src gitlet.ImportSource, done gitlet.ImportDone,
	at statelog.Position) {

	const unpublished = "code_imported_unpublished"
	const detail = "the event log does not hold this import's end; the repository's row names how it ended"
	if m.o.Events == nil {
		return
	}
	s, err := m.o.Cache.Sync(ctx, repoID, syncFresh(at, at))
	if err != nil {
		m.log.Warn(unpublished, "repo", repoID, "error", err.Error(), "detail", detail)
		return
	}
	defer s.Release()
	m.announce(ctx, repoID, events.New(types.CodeImported{RepoID: repoID, RepoName: s.Repo.Name, URL: src.URL,
		Mirrored: s.Repo.Mirrored, Chunks: done.Seq, Refs: s.Repo.Refs, Bytes: s.Repo.LiveBytes,
		DefaultBranch: s.Repo.DefaultBranch, Error: done.Error, Position: at.String()}, events.TraceContext{}),
		unpublished, detail)
}

// sourceHead is the branch the source's HEAD names, by name ("main"), or ""
// for a source whose HEAD names none: `ls-remote --symref`.
func (m *Mirrors) sourceHead(ctx context.Context, src gitlet.ImportSource) (string, error) {
	remote, err := m.o.Dial(ctx, src.URL, src.Credential)
	if err != nil {
		return "", err
	}
	res, err := m.o.Git.Run(ctx, gitproc.Cmd{
		Args: []string{"ls-remote", "--symref", remote.URL(), "HEAD"}, Remote: &remote,
		Class: gitproc.ClassTransfer, Share: share, Deadline: gitproc.RemoteDeadline(0, 0),
	})
	if err != nil {
		return "", err
	}
	lines := bufio.NewScanner(bytes.NewReader(res.Stdout))
	for lines.Scan() {
		if target, ok := strings.CutPrefix(lines.Text(), "ref: refs/heads/"); ok {
			branch, _, _ := strings.Cut(target, "\t")
			if gitref.ValidBranch(branch) {
				return branch, nil
			}
		}
	}
	return "", lines.Err()
}
