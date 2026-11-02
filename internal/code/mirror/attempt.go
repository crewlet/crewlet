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

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/cache"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/gitref"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
	"golang.org/x/sync/errgroup"
)

// share is whose transfer an attempt's fetch is in the transfer lane: the
// engine's own, a credential acting as nobody — gitproc's machine class, "a
// mirror" among its examples — under one principal for every import and
// mirror of the node, so they take one turn of the lane between them however
// many run.
var share = gitproc.Share{Class: gitproc.ShareMachine, Principal: "crewlet:mirror"}

// cutConfig is the configuration the cut's pack-objects runs under: every
// output a bundle in the stored format — index version 2 and its reverse index
// beside it, which the bundle's seal holds it to (code.CheckBundle) — and the
// delta window held to twice the epoch's max_blob_bytes, the bound a
// compaction's build is held to for the same reason (the window is the
// build's memory).
func cutConfig(maxBlob int64) []gitproc.KV {
	return []gitproc.KV{
		{Key: "pack.indexVersion", Value: "2"},
		{Key: "pack.writeReverseIndex", Value: "true"},
		{Key: "pack.windowMemory", Value: strconv.FormatInt(2*maxBlob, 10)},
	}
}

// fetchConfig is what the fetch runs under, beside the remote's own keys,
// which the pool writes (gitproc.Cmd.Remote): every object it receives checked
// as a push's receive-pack checks one (transfer.fsckObjects), and everything
// it receives kept as the pack it arrived in (fetch.unpackLimit 1: below the
// limit git would explode a small fetch into loose objects, which no
// `--stdin-packs` cut names).
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
// ends. A nil error is an attempt that landed what it fetched, or found
// nothing to land. A sync that reached its source is counted
// (crewlet.code.mirror.runs{dir=pull}) by what it came to.
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
	case l.kind == kindImport && s.Repo.Status != code.RepoStatusImporting:
		return nil
	case l.kind == kindSync && (s.Repo.Status != code.RepoStatusActive || !s.Repo.Mirrored):
		return nil
	}
	if l.kind == kindImport && l.source.URL == "" {
		// A row importing from no source — one no apply of this build writes,
		// which the listing names with none (internal/code's importsDue) — is
		// ended rather than dialed: there is nothing to fetch, now or later.
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

	// FETCH what the source holds that the copy's refs do not reach: at most
	// what the repository may still take, for an import, and one push's worth
	// for a sync.
	bring := limits.MaxPushBytes
	if l.kind == kindImport {
		bring = max(limits.RepoMaxBytes-s.Repo.LiveBytes, 0)
	}
	if res, err := m.o.Git.Run(ctx, gitproc.Cmd{
		Dir: sc.Dir, Alternate: sc.Alternate, Class: gitproc.ClassTransfer, Share: share, Remote: &remote,
		Args: []string{"fetch", "--quiet", "--no-tags", "--no-write-fetch-head", "--no-auto-gc",
			"--no-auto-maintenance", remote.URL(), "+refs/heads/*:refs/heads/*", "+refs/tags/*:refs/tags/*"},
		Config: fetchConfig, Deadline: gitproc.RemoteDeadline(bring, s.Repo.LiveObjects),
	}); err != nil {
		return fmt.Errorf("mirror: fetch %s from %s: %w\n%s", repoID, remote, err, res.Stderr)
	}
	if loose, err := looseIn(sc.Dir); err != nil {
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
	for _, st := range steps(outs, cmds) {
		applied, err := m.land(ctx, repoID, l, s, sc, st, l.seq+1, limits)
		var perm *errPermanent
		if errors.As(err, &perm) {
			// Ended at the chunks landed so far, this attempt's among them:
			// the end's Seq is the repository's import_seq.
			return m.end(ctx, repoID, l, s, sc, perm)
		}
		if err != nil || !applied {
			return err
		}
		l.seq++
		landed++
	}
	if l.kind == kindImport {
		return m.end(ctx, repoID, l, s, sc, nil)
	}
	return nil
}

// syncFresh is what an attempt syncs its copy at: the further of the turn's
// floor — the record that nudged it — and the version its listing read the
// row at (both listings carry it), the floor zero on a backstop's turn. NEVER
// STALE: a stale sync answers the copy as this node holds it, and a pull
// mirror's copy can be the one its import's last chunk left, still
// importing, which the attempt would take for a repository owed nothing.
func syncFresh(floor, at statelog.Position) statelog.Freshness {
	if before, err := at.Before(floor); err == nil && before {
		at = floor
	}
	return statelog.FloorAt(statelog.ReadSession, at)
}

// looseIn reports whether a fetch left an object LOOSE in the repository at
// dir — what only git's dumb HTTP protocol writes, since a smart fetch under
// fetch.unpackLimit 1 keeps whatever it receives as the pack it arrived in.
// Such a source is refused: its objects reach no `--stdin-packs` cut, so a
// ref landed onto them would name objects no stored pack holds, and the
// dumb walker writes them without transfer.fsckObjects' check.
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

// step is one record an attempt lands: the outputs it stores and the commands
// it carries.
type step struct {
	outs []string
	cmds []code.Command
}

// steps orders what an attempt lands PACKS FIRST: every output in records of at
// most code.MaxImportPacks before any command, then the commands in records of
// at most code.MaxImportRefs, the first of them riding with the last outputs.
// None for an attempt that brought nothing.
func steps(outs []string, cmds []code.Command) []step {
	var out []step
	for len(outs) > code.MaxImportPacks {
		out = append(out, step{outs: outs[:code.MaxImportPacks]})
		outs = outs[code.MaxImportPacks:]
	}
	first := step{outs: outs}
	n := min(len(cmds), code.MaxImportRefs)
	first.cmds, cmds = cmds[:n], cmds[n:]
	if len(first.outs) > 0 || len(first.cmds) > 0 {
		out = append(out, first)
	}
	for len(cmds) > 0 {
		n := min(len(cmds), code.MaxImportRefs)
		out = append(out, step{cmds: cmds[:n]})
		cmds = cmds[n:]
	}
	return out
}

// commands is every ref the source moved against the copy: each of the
// scratch's refs — what the fetch wrote, the source's branches and tags —
// that the copy holds at another value or not at all, and each of the copy's
// branches and tags the source no longer has, deleted. A ref in the seats'
// namespace is none of a source's to set, and one the code host's grammar
// refuses no record carries: both are left out. In the order of their names.
func (m *Mirrors) commands(ctx context.Context, s cache.Synced, sc *cache.Scratch) ([]code.Command, error) {
	want, err := m.refs(ctx, sc.Dir, sc.Alternate)
	if err != nil {
		return nil, err
	}
	have, err := m.refs(ctx, s.Dir, "")
	if err != nil {
		return nil, err
	}
	var out []code.Command
	for ref, oid := range want {
		if old, ok := have[ref]; !ok || old != oid {
			c := code.Command{Ref: ref, Old: cmp(old, ok), New: oid}
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
			out = append(out, code.Command{Ref: ref, Old: old, New: code.ZeroOID})
		}
	}
	slices.SortFunc(out, func(a, b code.Command) int { return strings.Compare(a.Ref, b.Ref) })
	return out, nil
}

// cmp is an old value: the ref's, or the zero id for a ref the copy lacks.
func cmp(old string, ok bool) string {
	if !ok {
		return code.ZeroOID
	}
	return old
}

// refs is every branch and tag of the repository at dir and the object each
// names, less the seats' namespace and any name the code host's grammar
// refuses.
func (m *Mirrors) refs(ctx context.Context, dir, alternate string) (map[string]string, error) {
	res, err := m.o.Git.Run(ctx, gitproc.Cmd{
		Dir: dir, Alternate: alternate, Class: gitproc.ClassRead, Deadline: gitproc.ReadDeadline,
		Args: []string{"for-each-ref", "--format=%(objectname) %(refname)", "refs/heads/", "refs/tags/"},
	})
	if err != nil {
		return nil, fmt.Errorf("mirror: list the refs of %s: %w", dir, err)
	}
	out := map[string]string{}
	lines := bufio.NewScanner(bytes.NewReader(res.Stdout))
	for lines.Scan() {
		oid, ref, ok := strings.Cut(lines.Text(), " ")
		if !ok || !gitref.ValidOID(oid) {
			return nil, fmt.Errorf("mirror: for-each-ref printed %q", lines.Text())
		}
		if strings.HasPrefix(ref, gitref.SeatsRoot) || !gitref.ValidRef(ref) {
			continue
		}
		out[ref] = oid
	}
	return out, lines.Err()
}

// forced reports whether moving a ref from old to new leaves old behind: old is
// not an ancestor of new — a commit's history rewritten, or a tag moved —
// read in the scratch, which holds both through its alternate.
func (m *Mirrors) forced(ctx context.Context, sc *cache.Scratch, old, new string) (bool, error) {
	res, err := m.o.Git.Run(ctx, gitproc.Cmd{
		Dir: sc.Dir, Alternate: sc.Alternate, Class: gitproc.ClassRead, Answers: []int{1},
		Deadline: gitproc.ReadDeadline, Args: []string{"merge-base", "--is-ancestor", old, new},
	})
	switch {
	case err != nil:
		// An old value that is no commit — a tag of a tree, a blob — has no
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
// `pack-objects --stdin-packs` in the scratch over every pack the fetch wrote,
// every live pack of the copy excluded, cut at code.CompactPackBytes; and
// answers the checksums of its outputs, in the order it wrote them. None where
// the fetch brought nothing the copy lacks — `--non-empty`, since without it
// git writes and names a pack of no objects, which no record may carry
// (code.PackRef.Check), so the attempt would fail on it every time: the
// restart after a chunk of packs alone, whose fetch brings again only what
// those packs hold, would never land its refs.
func (m *Mirrors) cut(ctx context.Context, s cache.Synced, sc *cache.Scratch, fetched []cache.ScratchPack,
	fetchedBytes int64, limits code.Limits) ([]string, error) {

	if len(fetched) == 0 {
		return nil, nil
	}
	var list strings.Builder
	for _, p := range fetched {
		list.WriteString("pack-" + p.Checksum + ".pack\n")
	}
	for _, p := range m.o.Cache.Packs(s) {
		list.WriteString("^pack-" + p.GitChecksum + ".pack\n")
	}
	res, err := m.o.Git.Run(ctx, gitproc.Cmd{
		Dir: sc.Dir, Alternate: sc.Alternate, Class: gitproc.ClassMaintenance,
		Args: []string{"pack-objects", "--stdin-packs", "--non-empty",
			"--max-pack-size=" + strconv.FormatInt(code.CompactPackBytes, 10), "--delta-base-offset", "--quiet",
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

// largestBlob is why an output may not be stored — its first blob past limit,
// in one sentence — or "": ONE `cat-file --batch-check` over each output's ids,
// streamed from its index, ending at the first offender.
func (m *Mirrors) largestBlob(ctx context.Context, sc *cache.Scratch, outs []string, fetchedBytes int64,
	limit int64) (string, error) {

	for _, cs := range outs {
		ids, err := openIdxOIDs(filepath.Join(sc.Dir, "objects", "pack", "pack-"+cs+".idx"))
		if err != nil {
			return "", err
		}
		var found string
		lines := gitproc.NewLines(func(line []byte) error {
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

// permanent reports whether a chunk refused r is refused for good: a limit
// the source's content is past — repo_size, repo_refs, a ref the repository
// cannot hold beside another (ref_conflict), a pack past the stored bound —
// which every attempt would meet again, since each fetches the same source.
// The import ends on it (internal/code's decide_remote.go: "a limit's refusal
// is the home's to end the import on"). Every other refusal — stale, busy,
// expired, unavailable — says nothing of the source and is tried again.
func permanent(r code.Refusal) bool {
	switch r {
	case code.RefusalRepoSize, code.RefusalRepoRefs, code.RefusalRefConflict, code.RefusalPackTooLarge:
		return true
	}
	return false
}

// afterBuild, afterUpload and afterPropose are an attempt's fault points, an
// import's or a sync's, each spelled where it is hit — the form the codefault
// package's hit-site gate reads.
func (m *Mirrors) afterBuild(ctx context.Context, k kind) error {
	if k == kindImport {
		return m.fault(ctx, codefault.ImportAfterBuild)
	}
	return m.fault(ctx, codefault.MirrorAfterBuild)
}

func (m *Mirrors) afterUpload(ctx context.Context, k kind) error {
	if k == kindImport {
		return m.fault(ctx, codefault.ImportAfterUpload)
	}
	return m.fault(ctx, codefault.MirrorAfterUpload)
}

func (m *Mirrors) afterPropose(ctx context.Context, k kind) error {
	if k == kindImport {
		return m.fault(ctx, codefault.ImportAfterPropose)
	}
	return m.fault(ctx, codefault.MirrorAfterPropose)
}

// land stores one step's outputs, each laid out under the id its record
// derives, all at once, and proposes its record: an import's chunk at seq, or a
// sync at the copy's drop epoch. landed is that it applied — and its outputs
// then adopted into this node's copy; false with a nil error is a repository
// deleted meanwhile, owed nothing more. A chunk refused a limit ([permanent])
// is an *errPermanent; any other refusal, and a record not seen applied, is
// the attempt's error, after which the next attempt reads the rows again.
func (m *Mirrors) land(ctx context.Context, repoID string, l listed, s cache.Synced, sc *cache.Scratch, st step,
	seq int64, limits code.Limits) (landed bool, err error) {

	id := statelog.NewOpID(m.now(), "")
	refs := make([]code.PackRef, len(st.outs))
	// Every output uploads AT ONCE, as a compaction's do (internal/code/compact's
	// uploadAll gives the arithmetic): one after another, a step of
	// code.MaxImportPacks outputs would take that many paced uploads end to
	// end. The first failure cancels the rest, whose objects no row will name.
	g, gctx := errgroup.WithContext(ctx)
	for n, cs := range st.outs {
		path := filepath.Join(sc.Dir, "objects", "pack", "pack-"+cs+".pack")
		ref, err := code.LayBundle(code.DeriveOutputPackID(id, n), path, cs)
		if err != nil {
			return false, fmt.Errorf("mirror: lay out output %d: %w", n, err)
		}
		refs[n] = *ref
		g.Go(func() error {
			if err := code.StoreBundle(gctx, m.o.Objects, path, &refs[n], nil); err != nil {
				return fmt.Errorf("mirror: store output %d of %s: %w", n, repoID, err)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return false, err
	}
	if err := m.afterUpload(ctx, l.kind); err != nil {
		return false, err
	}
	var res code.SyncResult
	if l.kind == kindImport {
		res, err = m.o.Estate.Import(ctx, code.ImportProposal{Chunk: &code.ImportChunk{RepoID: repoID, ChunkID: id,
			Node: m.o.NodeID, Seq: seq, Packs: refs, Commands: st.cmds}})
	} else {
		res, err = m.o.Estate.MirrorSync(ctx, code.MirrorSyncProposal{Sync: code.MirrorSync{RepoID: repoID,
			SyncID: id, Node: m.o.NodeID, DropEpoch: s.DropEpoch, Packs: refs, Commands: st.cmds,
			FetchedAt: m.now()}})
	}
	if err != nil {
		return false, fmt.Errorf("mirror: land a step of %s: %w", repoID, err)
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
		return false, ferr
	}
	switch {
	case res.Refusal == code.RefusalRepoDeleted:
		// Nothing is owed a repository deleted meanwhile.
		return false, nil
	case l.kind == kindImport && permanent(res.Refusal):
		return false, &errPermanent{why: res.Detail}
	case res.Refusal != "":
		return false, fmt.Errorf("mirror: a step of %s refused %s: %s", repoID, res.Refusal, res.Detail)
	case res.Outcome != statelog.OutcomeApplied:
		// Appended, and not seen applied within the wait: a failure, so the
		// next attempt reads the rows at RetryAfter's first minute rather
		// than a sync's interval later.
		return false, fmt.Errorf("mirror: a step of %s is %s, not seen applied", repoID, res.Outcome)
	}
	if len(refs) > 0 {
		if err := m.o.Cache.Adopt(ctx, repoID, sc, refs...); err != nil {
			m.log.Info("code_mirror_adopt_failed", "repo", repoID, "error", err,
				"detail", "the packs are on their rows; the next sync downloads them")
		}
	}
	return true, nil
}

// end ends an import at l.seq — with why as its import_error where why is set
// — naming the source's HEAD branch where the branch the repository declares
// did not arrive in sc, the attempt's scratch (nil for an attempt that made
// none, which names no branch). A sync ends nothing: why is its attempt's
// error.
func (m *Mirrors) end(ctx context.Context, repoID string, l listed, s cache.Synced, sc *cache.Scratch,
	why *errPermanent) error {

	if l.kind != kindImport {
		if why != nil {
			return why
		}
		return nil
	}
	done := code.ImportDone{RepoID: repoID, Node: m.o.NodeID, Seq: l.seq}
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
	res, err := m.o.Estate.Import(ctx, code.ImportProposal{Done: &done})
	switch {
	case err != nil:
		return fmt.Errorf("mirror: end the import of %s: %w", repoID, err)
	case res.Refusal != "":
		return fmt.Errorf("mirror: the end of %s's import refused %s: %s", repoID, res.Refusal, res.Detail)
	}
	return nil
}

// sourceHead is the branch the source's HEAD names, by name ("main"), or ""
// for a source whose HEAD names none: `ls-remote --symref`.
func (m *Mirrors) sourceHead(ctx context.Context, src code.ImportSource) (string, error) {
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
