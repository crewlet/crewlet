package cache

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/gitproc"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
)

// VERIFY: an operator's question — does this node's cache of a repository
// hold exactly what its rows say? — answered by reading every byte of it
// against the rows, and, when asked, repaired by a rebuild that is checked the
// same way. Not a hot path: it hashes every pack, one at a time, and runs git's
// connectivity check over the repository.
//
// # Admitted before it reads anything, and only of a copy this node holds
//
// A check is ADMITTED ([Cache.AdmitVerify]) before a byte of it is read: a
// slot of the maintenance lane taken — a free one or none, the lane's rule —
// and the connectivity check's memory charged against the budget the
// background work shares, both held until the check is let go. A check
// admitted only at its fsck read up to the largest repository the defaults
// admit (twenty gibibytes) before a busy node told it to run again, and the
// retry read it all again for as long as a bitmap held the lane; and nothing
// bounded how many such reads ran at once, beside the clones the node serves
// from the same disk. Admitted first, a refusal costs nothing, and the lane
// bounds the checks.
//
// And only of a copy this node HOLDS ([ErrNotHeld]): a check of a repository
// with no incarnation here would sync it from nothing — a GET for every push
// the repository ever took — into a cache that then keeps it and evicts warm
// repositories to do so, undoing the routing that keeps each repository warm
// on one or two git nodes. Verifying every git node of a fleet would cost a
// whole download a node. The node that holds no copy has nothing to check, and
// says so for free.

// VerifyReport is what Verify found in one incarnation of a repository.
type VerifyReport struct {
	// Repo is the repository's id, and Name its name.
	Repo string `json:"repo"`
	Name string `json:"name"`
	// Position is the one the incarnation was checked at: every row up to
	// here is what it was held to.
	Position statelog.Position `json:"position"`
	// Packs is how many packs were checked against their rows, and Refs how
	// many refs packed-refs holds.
	Packs int `json:"packs"`
	Refs  int `json:"refs"`
	// Problems is everything the incarnation holds that its rows do not say,
	// each a sentence naming the file and both values; none is a cache
	// exactly its rows.
	Problems []string `json:"problems,omitempty"`
	// Rebuilt: the problems sent the repository to a rebuild, and Problems
	// is what the incarnation that replaced the damaged one holds — a
	// problem a rebuild left is in the object store or the rows, and its own
	// words say which. Found is what the check before the rebuild found.
	Rebuilt bool     `json:"rebuilt,omitempty"`
	Found   []string `json:"found,omitempty"`
}

// Verification is one check of a repository's cache this node holds:
// admitted by [Cache.AdmitVerify] before anything of it is read, run once by
// [Verification.Run], and let go by [Verification.Release], which its holder
// defers. From admission to release it holds a slot of the maintenance lane,
// the connectivity check's charge against the node's memory budget, a pin of
// the incarnation it was admitted over — so the budget never evicts the copy
// being checked — and the maintainer off the repository (holdMaintainer).
type Verification struct {
	c     *Cache
	e     *entry
	label string

	// held is the pin taken at admission, slot the maintenance slot, and
	// unhold what lets the maintainer back on the repository.
	held   Synced
	slot   *gitproc.Reserved
	unhold func()

	// mu guards mem, ran and released. mem is the memory charged so far:
	// the admitted incarnation's figure, raised when a sync or a rebuild
	// brings the repository more objects (charge).
	mu       sync.Mutex
	mem      int64
	ran      bool
	released bool
}

// AdmitVerify admits a check of repoID's cache on this node, BEFORE ANYTHING
// OF IT IS READ (see the file's doc), or refuses it at once, having read and
// downloaded nothing:
//
//   - [ErrNotHeld] when this node holds no copy of the repository — it never
//     served it here, or evicted it — since a check would build one;
//   - [gitproc.ErrBusy] when the maintenance lane has no free slot, or the
//     background work in flight leaves the check's memory no room: a moment's
//     state, answered when it is asked again;
//   - [ErrNoMemory] when the connectivity check's charge alone is past what
//     this node can give its background work: definite for this node, and a
//     node with more memory can run it;
//   - [ErrStopped] and [ErrInvalid] as for every request of a cache.
//
// The charge is the incarnation's objects at [FsckBytesPerObject], and room
// beside it for one transfer of the repository (charge): what the check's
// fsck holds, and the most of the node's memory any step of it holds — its
// hashing reads through one buffer. Every holder defers the answer's Release.
func (c *Cache) AdmitVerify(repoID string) (*Verification, error) {
	switch {
	case c.isStopped():
		return nil, ErrStopped
	case !canonicalID(repoID):
		return nil, fmt.Errorf("%w: repository id %q is not a canonical lowercase UUID", ErrInvalid, repoID)
	}
	c.mu.Lock()
	e := c.entries[repoID]
	c.mu.Unlock()
	if e == nil {
		return nil, fmt.Errorf("cache: verify %s: %w", repoID, ErrNotHeld)
	}
	e.mu.Lock()
	inc := e.current
	if e.dead || inc == nil || inc.retired {
		e.mu.Unlock()
		return nil, fmt.Errorf("cache: verify %s: %w", repoID, ErrNotHeld)
	}
	held := c.pin(e, inc, SyncNone)
	label := cmp.Or(inc.repo.Name, repoID)
	mem := charge(inc.objects, FsckBytesPerObject)
	e.mu.Unlock()

	slot, err := c.git.Reserve()
	if err != nil {
		held.Release()
		return nil, fmt.Errorf("cache: verify %s: %w — a check holds a maintenance slot from its first read "+
			"to its last, and none is free; run it again", label, err)
	}
	if err := c.admitCheck(label, mem, 0); err != nil {
		slot.Release()
		held.Release()
		return nil, err
	}
	return &Verification{c: c, e: e, label: label, held: held, slot: slot, unhold: c.holdMaintainer(e), mem: mem},
		nil
}

// admitCheck charges a check's memory against the node's budget: mem in all,
// of which already is charged. ErrNoMemory when mem alone is past the budget,
// gitproc.ErrBusy when the work in flight leaves the rest no room.
func (c *Cache) admitCheck(label string, mem, already int64) error {
	if budget := c.maint.budgetNow(); mem > budget {
		return fmt.Errorf("cache: verify %s: %w: its connectivity check needs about %d bytes, and this node "+
			"can give its background work %d of its memory — run it on a node with more memory", label,
			ErrNoMemory, mem, budget)
	}
	if mem <= already {
		return nil
	}
	if c.maint.admitOther(mem-already) != admitted {
		return fmt.Errorf("cache: verify %s: %w — its connectivity check needs about %d bytes, which the "+
			"background work in flight leaves no room for; run it again", label, gitproc.ErrBusy, mem)
	}
	return nil
}

// charge raises what v holds of the node's memory to what the objects of the
// incarnation s pins need now: a sync or a rebuild between the admission and
// the check may have brought the repository pushes of its own. Refused like
// the admission — before anything of the incarnation is read. Read under s's
// own entry, which is v's unless the repository was purged and declared again
// meanwhile.
func (v *Verification) charge(s Synced) error {
	s.entry.mu.Lock()
	mem := charge(s.inc.objects, FsckBytesPerObject)
	s.entry.mu.Unlock()
	v.mu.Lock()
	defer v.mu.Unlock()
	if err := v.c.admitCheck(v.label, mem, v.mem); err != nil {
		return err
	}
	v.mem = max(v.mem, mem)
	return nil
}

// Release lets go of everything the admission took — the slot, the charge,
// the pin and the hold on the maintainer, which it kicks so a repository that
// fell due meanwhile is taken up at once. Once; a later call does nothing.
func (v *Verification) Release() {
	v.mu.Lock()
	if v.released {
		v.mu.Unlock()
		return
	}
	v.released = true
	mem := v.mem
	v.mu.Unlock()
	v.slot.Release()
	v.c.maint.doneOther(mem)
	v.held.Release()
	v.unhold()
}

// Run checks the repository's cache against its rows, after a linearizable
// sync — of the copy this node holds, so a delta, or a full round reusing every
// pack it holds on an incarnation a previous process left — ONCE: a second
// Run, or one after Release, is refused ([ErrInvalid]).
//
//  1. its directory listed against the image grammar (sweep.go) — every
//     entry it does not admit is a problem — with the repository's rounds
//     held off, so no step half done reads as damage, and the files a
//     maintenance run in flight holds while it runs admitted: the run is
//     left to finish, and none starts until the check is let go
//     (holdMaintainer);
//  2. the repository's whole rows read at least as fresh as the listing,
//     AFTER it: a pack installed between the two has its row applied first,
//     so it is in the answer, and nothing removes a row-named pack;
//  3. every pack a row added at or before the listing's position held to its
//     row: installed, each section at its row's size, and the three files
//     back to back — .pack, .idx, .rev, the bundle the push stored — hashing
//     to the row's Hash; a pack no row names is a stray;
//  4. packed-refs hashed against the digest this cache wrote with it;
//  5. git's connectivity check, `fsck --connectivity-only`, in the
//     maintenance slot the admission took.
//
// With repair and any problem, the repository is rebuilt — a whole read at
// the log's head into the next incarnation (Rebuild), counted under reason
// verify and refused by none of a git process's windows, since an operator
// asked and the damage is named — SEEDED with every pack step 3 found whole,
// linked rather than downloaded (seedRebuild), so a repair asks the object
// store only for what it found damaged — and the rebuilt incarnation checked
// again:
// Rebuilt, Found the first check's problems, Problems the second's. A rebuild
// that fails answers the first report beside its failure, which names what
// it could not repair. A rebuild that left no problem has ENDED the damage it
// was for, so the incarnation it made is rebuilt at once on any later
// failure rather than held to the window a rebuild made at a git process's
// word is (rebuild.go): that window exists because the cache never sees a
// request succeed, and Verify has just seen the whole of it whole.
//
// EVERY ANSWER CARRIES THE REPORT SO FAR beside its error — what the check
// found before it failed, the repository named whenever it was known — so a
// check cut short by a stop or a failure still says what it saw.
func (v *Verification) Run(ctx context.Context, repair bool) (VerifyReport, error) {
	v.mu.Lock()
	if v.ran || v.released {
		v.mu.Unlock()
		return VerifyReport{Repo: v.e.id}, fmt.Errorf("%w: a verification runs once, and only before its Release",
			ErrInvalid)
	}
	v.ran = true
	v.mu.Unlock()
	c := v.c
	s, err := c.Sync(ctx, v.e.id, statelog.Freshness{Level: statelog.ReadLinearizable})
	if err != nil {
		return VerifyReport{Repo: v.e.id, Name: v.held.Repo.Name},
			fmt.Errorf("cache: verify %s: %w", v.label, err)
	}
	defer s.Release()
	report := VerifyReport{Repo: v.e.id, Name: cmp.Or(s.Repo.Name, v.held.Repo.Name), Position: s.Position}
	if err = v.charge(s); err != nil {
		return report, err
	}
	report, whole, err := c.check(ctx, s, v.slot)
	if err != nil || !repair || len(report.Problems) == 0 {
		return report, err
	}
	rebuilt, err := c.rebuild(ctx, s, RebuildVerify, false, whole)
	if err != nil {
		return report, fmt.Errorf("cache: verify %s: a rebuild could not repair it: %w", report.label(), err)
	}
	defer rebuilt.Release()
	if err = v.charge(rebuilt); err != nil {
		return report, err
	}
	second, _, err := c.check(ctx, rebuilt, v.slot)
	second.Rebuilt, second.Found = true, report.Problems
	if err == nil && len(second.Problems) == 0 {
		rebuilt.entry.mu.Lock()
		rebuilt.inc.rebuilds, rebuilt.inc.unmoved = 0, false
		rebuilt.entry.mu.Unlock()
	}
	return second, err
}

// holdMaintainer keeps the maintainer from starting a run on e until the
// function it answers is called, which lets it go and kicks the maintainer, so
// a repository that fell due meanwhile is taken up at once rather than at the
// next tick. A check holds it from its admission to its release, because its
// LISTING reads whether a run is in flight once, as it begins, and admits a
// running run's lock and temporary files only on that reading: a run started
// while the listing reads the directory would leave files the listing reports
// as damage, and one started between a repair's two checks the same in the
// second. A run ALREADY in flight is left to finish, never stopped: work
// thrown away for a check is work the maintainer does again at once, and its
// lock and temporary files are admitted by the listing instead
// (maintenanceWriting). The check's fsck no longer races it for a slot: the
// admission holds one.
func (c *Cache) holdMaintainer(e *entry) func() {
	e.mu.Lock()
	e.verifying++
	e.mu.Unlock()
	return func() {
		e.mu.Lock()
		e.verifying--
		e.mu.Unlock()
		c.kickMaintainer()
	}
}

// label is how an error names the repository: its name, else its id.
func (r VerifyReport) label() string {
	return cmp.Or(r.Name, r.Repo)
}

// check is one pass of Verify over the incarnation s pins, its fsck run in
// slot. Beside its report it answers the checksums of the packs it held WHOLE
// to their rows — every section at its row's size and the bundle hashing to
// the row's Hash — which a repair need not download again.
func (c *Cache) check(ctx context.Context, s Synced, slot *gitproc.Reserved) (VerifyReport, []string, error) {
	e, inc := s.entry, s.inc
	r := VerifyReport{Repo: e.id, Name: s.Repo.Name, Position: s.Position}
	l, err := c.list(ctx, e, inc)
	if err != nil {
		return r, nil, fmt.Errorf("cache: verify %s: listing %s: %w", r.label(), inc.dir, err)
	}
	defer l.close()
	r.Name, r.Position, r.Problems = cmp.Or(l.repo.Name, r.Name), l.at, l.problems

	snap, err := c.read(ctx, e.id, l.at, l.stream, true,
		statelog.Freshness{Level: statelog.ReadSession, MinPosition: l.at})
	switch {
	case err != nil:
		return r, nil, fmt.Errorf("cache: verify %s: reading its rows: %w", r.label(), err)
	case !snap.streamCreated.Equal(l.stream):
		return r, nil, fmt.Errorf("cache: verify %s: the code log under the cache was recreated while it was "+
			"checked (made %s, the cache read one made %s); run it again", r.label(),
			snap.streamCreated.Format(time.RFC3339Nano), l.stream.Format(time.RFC3339Nano))
	}

	packs, whole, problems, err := checkPacks(ctx, inc.dir, l, snap)
	r.Packs, r.Problems = packs, append(r.Problems, problems...)
	if err != nil {
		return r, whole, fmt.Errorf("cache: verify %s: %w", r.label(), err)
	}
	refs, problem, err := l.checkRefs()
	r.Refs = refs
	if problem != "" {
		r.Problems = append(r.Problems, problem)
	}
	if err != nil {
		return r, whole, fmt.Errorf("cache: verify %s: %w", r.label(), err)
	}
	problem, err = c.fsck(ctx, e, inc, r.label(), slot)
	if problem != "" {
		r.Problems = append(r.Problems, problem)
	}
	return r, whole, err
}

// listing is an incarnation as Verify's listing found it.
type listing struct {
	// at, stream and repo are what the directory reflects; digest is the
	// packed-refs this cache wrote, and refs that file, opened while nothing
	// could replace it.
	at     statelog.Position
	stream time.Time
	repo   code.RepoRef
	digest string
	refs   *os.File
	// packs is every pack file in objects/pack, by checksum: each
	// extension's size.
	packs map[string]map[string]int64
	// problems is what the grammar does not admit, and what it requires that
	// is missing.
	problems []string
}

// close lets go of the packed-refs the listing opened.
func (l *listing) close() {
	if l.refs != nil {
		_ = l.refs.Close() // read-only: what was read is the answer
	}
}

// list reads inc's directory against the image grammar with no round writing
// it (entry.write, waited for under ctx). A maintenance run may be: one in
// flight on inc when the listing begins is left to finish — the check holds
// the maintainer off (holdMaintainer), so none starts — and the files it holds
// only while it runs are admitted (maintenanceWriting), since a writer's own
// lock and temporary files are no damage. With no run in flight they are,
// and named as such.
func (c *Cache) list(ctx context.Context, e *entry, inc *incarnation) (*listing, error) {
	if c.knobs != nil && c.knobs.verifyWaits != nil {
		c.knobs.verifyWaits(inc.dir)
	}
	if err := c.acquireWrite(ctx, e); err != nil {
		return nil, fmt.Errorf("waiting for the repository's round: %w", context.Cause(ctx))
	}
	defer c.releaseWrite(e, writeExclusive)
	if c.knobs != nil && c.knobs.verifyListing != nil {
		c.knobs.verifyListing(inc.dir)
	}

	l := &listing{packs: map[string]map[string]int64{}}
	e.mu.Lock()
	l.at, l.stream, l.repo, l.digest = inc.at, inc.stream, inc.repo, inc.refsDigest
	running := e.maint != nil && e.maint.inc == inc
	e.mu.Unlock()
	writing := func(rel, name string) bool { return running && maintenanceWriting(rel, name) }
	unexpected := func(rel string) { l.problems = append(l.problems, "unexpected "+filepath.ToSlash(rel)) }
	missing := func(rel string) { l.problems = append(l.problems, "missing "+filepath.ToSlash(rel)) }

	// admitted is every entry the grammar admitted, by its path: a
	// directory is listed only when its parent admitted it as one, and one
	// the grammar requires that is not there is missing.
	admitted := map[string]bool{".": true}
	dir := func(rel string, admit func(name string, dir bool) bool) ([]fs.DirEntry, error) {
		if !admitted[rel] {
			missing(rel)
			return nil, nil
		}
		list, err := os.ReadDir(filepath.Join(inc.dir, rel))
		if err != nil {
			return nil, err
		}
		kept := list[:0]
		for _, d := range list {
			if !admit(d.Name(), d.IsDir()) {
				unexpected(filepath.Join(rel, d.Name()))
				continue
			}
			kept = append(kept, d)
			admitted[filepath.Join(rel, d.Name())] = true
		}
		return kept, nil
	}
	if _, err := dir(".", inRoot); err != nil {
		return nil, err
	}
	// The state is not required: a cache serves without it, and its first
	// round of a process writes it.
	for _, name := range []string{"HEAD", "config", "packed-refs"} {
		if !admitted[name] {
			missing(name)
		}
	}
	for _, f := range []struct {
		name string
		ok   func([]byte) bool
		says string
	}{
		{"HEAD", func(b []byte) bool { return string(b) == string(headFor(l.repo.DefaultBranch)) },
			"is not a symbolic ref to the default branch " + l.repo.DefaultBranch},
		{"config", func(b []byte) bool { return string(b) == gitproc.RepoConfig },
			"is not the one the engine writes"},
	} {
		if !admitted[f.name] {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(inc.dir, f.name))
		if err != nil {
			return nil, err
		}
		if !f.ok(raw) {
			l.problems = append(l.problems, fmt.Sprintf("%s %s: it reads %q", f.name, f.says, raw))
		}
	}
	for _, sub := range []struct {
		rel   string
		admit func(string, bool) bool
	}{
		{"refs", inRefs},
		{filepath.Join("refs", "heads"), func(string, bool) bool { return false }},
		{filepath.Join("refs", "tags"), func(string, bool) bool { return false }},
		{"objects", inObjects},
		{infoRel, func(name string, isDir bool) bool {
			return inInfo(name, isDir) || (!isDir && writing(infoRel, name))
		}},
	} {
		if _, err := dir(sub.rel, sub.admit); err != nil {
			return nil, err
		}
	}
	files, err := dir(packRel, func(name string, isDir bool) bool {
		return !isDir && (packFileName.MatchString(name) || midxFile.MatchString(name) || writing(packRel, name))
	})
	if err != nil {
		return nil, err
	}
	for _, d := range files {
		if !d.Type().IsRegular() {
			unexpected(filepath.Join(packRel, d.Name()))
			continue
		}
		m := packFileName.FindStringSubmatch(d.Name())
		if m == nil {
			continue // the maintainer's
		}
		info, ierr := d.Info()
		if errors.Is(ierr, fs.ErrNotExist) {
			continue
		}
		if ierr != nil {
			return nil, ierr
		}
		if l.packs[m[1]] == nil {
			l.packs[m[1]] = map[string]int64{}
		}
		l.packs[m[1]]["."+m[2]] = info.Size()
	}
	// packed-refs, opened while no round can rename another over it: the
	// file and the digest this cache wrote with it are read together.
	if admitted["packed-refs"] {
		if l.refs, err = os.Open(filepath.Join(inc.dir, "packed-refs")); err != nil {
			return nil, err
		}
	}
	return l, nil
}

// checkPacks holds every pack a row added at or before the listing's position
// to its row, one at a time, and names every pack file no row names; it
// answers how many packs it checked, and the checksums of those it found
// whole. A row added after the listing is not asked about: what the listing
// saw of it says nothing.
func checkPacks(ctx context.Context, dir string, l *listing, snap snapshot) (int, []string, []string, error) {
	rows := make([]code.PackRow, 0, len(snap.packs))
	for _, p := range snap.packs {
		rows = append(rows, p)
	}
	slices.SortFunc(rows, func(a, b code.PackRow) int {
		return cmp.Or(cmp.Compare(a.PackID, b.PackID), cmp.Compare(a.Part, b.Part))
	})
	var problems, whole []string
	named := map[string]bool{}
	checked := 0
	buf := make([]byte, copyBuffer)
	for _, row := range rows {
		named[row.GitChecksum] = true
		if !reached(l.at, row.Added) {
			continue
		}
		checked++
		label := "pack " + row.PackID + " (pack-" + row.GitChecksum + ")"
		files := l.packs[row.GitChecksum]
		if _, ok := files[".idx"]; !ok {
			problems = append(problems, label+" is missing")
			continue
		}
		if _, ok := files[".pack"]; !ok {
			problems = append(problems, label+" is missing")
			continue
		}
		ref, sized := row.Ref(), true
		for _, section := range []struct {
			ext  string
			want int64
		}{{".pack", ref.PackBytes()}, {".idx", ref.IndexBytes}, {".rev", ref.ReverseBytes}} {
			got, ok := files[section.ext]
			switch {
			case !ok:
				problems = append(problems, fmt.Sprintf("%s: its %s is missing", label, section.ext))
				sized = false
			case got != section.want:
				problems = append(problems, fmt.Sprintf("%s: its %s is %d bytes; its row says %d",
					label, section.ext, got, section.want))
				sized = false
			}
		}
		if !sized {
			continue
		}
		sum, err := hashBundle(ctx, filepath.Join(dir, "objects", "pack", "pack-"+row.GitChecksum), buf)
		switch {
		case errors.Is(err, fs.ErrNotExist):
			problems = append(problems, label+" is missing")
		case err != nil:
			return checked, whole, problems, fmt.Errorf("hashing %s: %w", label, err)
		case sum != row.Hash:
			problems = append(problems, fmt.Sprintf("%s hashes to %s; its row says %s", label, sum, row.Hash))
		default:
			whole = append(whole, row.GitChecksum)
		}
	}
	strays := make([]string, 0)
	for cs := range l.packs {
		if !named[cs] {
			strays = append(strays, cs)
		}
	}
	slices.Sort(strays)
	for _, cs := range strays {
		problems = append(problems, "stray pack pack-"+cs+" (no row names it)")
	}
	return checked, whole, problems, nil
}

// hashBundle is the SHA-256 of the pack at stem as the push stored it: its
// .pack, .idx and .rev back to back. Streamed through buf, ctx checked
// between reads — an operator's request over files up to code.MaxPackBytes.
func hashBundle(ctx context.Context, stem string, buf []byte) (objstore.Hash, error) {
	sum := sha256.New()
	for _, ext := range []string{".pack", ".idx", ".rev"} {
		f, err := os.Open(stem + ext)
		if err != nil {
			return "", err
		}
		err = copyChecked(ctx, sum, f, buf)
		_ = f.Close() // read-only: what was read is the answer
		if err != nil {
			return "", err
		}
	}
	return objstore.Hash(hex.EncodeToString(sum.Sum(nil))), nil
}

// copyChecked copies r to w through buf, checking ctx before every read.
func copyChecked(ctx context.Context, w io.Writer, r io.Reader, buf []byte) error {
	for {
		if err := ctx.Err(); err != nil {
			return context.Cause(ctx)
		}
		n, err := r.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		switch {
		case errors.Is(err, io.EOF):
			return nil
		case err != nil:
			return err
		}
	}
}

// checkRefs hashes the packed-refs the listing opened against the digest
// this cache wrote with it, and counts its refs (every line but the header
// and the peel lines).
func (l *listing) checkRefs() (int, string, error) {
	if l.refs == nil {
		return 0, "", nil
	}
	sum := sha256.New()
	count := &refLines{start: true}
	if _, err := io.Copy(io.MultiWriter(sum, count), l.refs); err != nil {
		return 0, "", fmt.Errorf("reading packed-refs: %w", err)
	}
	got := hex.EncodeToString(sum.Sum(nil))
	if l.digest != "" && got != l.digest {
		return count.refs, fmt.Sprintf("packed-refs hashes to %s; this cache wrote %s", got, l.digest), nil
	}
	return count.refs, "", nil
}

// refLines counts the ref lines of a packed-refs passing through it.
type refLines struct {
	refs  int
	start bool
}

func (r *refLines) Write(p []byte) (int, error) {
	for _, b := range p {
		if r.start && b != '#' && b != '^' {
			r.refs++
		}
		r.start = b == '\n'
	}
	return len(p), nil
}

// fsck runs git's connectivity check in inc, in the maintenance slot the
// check's admission holds, and answers a problem when git found one.
//
// Its memory was charged at the admission, and raised to the incarnation it
// runs over (Verification.charge): a connectivity check holds
// FsckBytesPerObject an object of its repository — some 11 GB over the
// largest the defaults admit — and is charged that and room beside it for one
// transfer of the repository against the budget the maintenance in flight
// shares (memory.go).
func (c *Cache) fsck(ctx context.Context, e *entry, inc *incarnation, label string,
	slot *gitproc.Reserved) (string, error) {

	e.mu.Lock()
	held := inc.diskBytes + inc.metaBytes
	e.mu.Unlock()
	var firstOut string
	findings := 0
	out := gitproc.NewLines(func(line []byte) error {
		if findings == 0 {
			firstOut = string(line)
		}
		findings++
		return nil
	})
	if c.knobs != nil && c.knobs.verifyFsck != nil {
		c.knobs.verifyFsck(inc.dir)
	}
	_, err := slot.Run(ctx, gitproc.Cmd{
		Dir: inc.dir, Args: []string{"fsck", "--connectivity-only", "--no-dangling", "--no-progress"},
		Config: c.ServeConfig(), Stdout: out, Class: gitproc.ClassMaintenance,
		Deadline: gitproc.PackWorkDeadline(held),
	})
	var exit *gitproc.ExitError
	switch {
	case err == nil:
		return "", nil
	case errors.As(err, &exit):
		return fsckProblem(exit.Result, firstOut, findings), nil
	case errors.Is(err, gitproc.ErrRepoShape):
		return "fsck did not run, the repository being no shape the engine writes: " + err.Error(), nil
	}
	return "", fmt.Errorf("cache: verify %s: fsck: %w", label, err)
}

// fsckProblem is the sentence a failed fsck is reported as: its FIRST
// finding — an error it printed, or an object it named on its output — and
// how many more there were. Not its last line: fsck ends a damaged
// repository's report with a notice ("No default references") that names
// nothing. A stderr cut to its tail (gitproc.StderrTail) loses its first,
// partial, line, and the count is then a floor.
func fsckProblem(res gitproc.Result, firstOut string, outFindings int) string {
	stderr := res.Stderr
	if len(stderr) >= gitproc.StderrTail {
		if i := strings.IndexByte(stderr, '\n'); i >= 0 {
			stderr = stderr[i+1:]
		}
	}
	var findings []string
	for _, line := range strings.Split(stderr, "\n") {
		if line = strings.TrimSpace(line); line != "" && !strings.HasPrefix(line, "notice:") {
			findings = append(findings, line)
		}
	}
	if firstOut != "" {
		findings = append(findings, firstOut)
	}
	n := len(findings) + max(outFindings-1, 0)
	switch n {
	case 0:
		return fmt.Sprintf("fsck: exited %d naming nothing", res.Exit)
	case 1:
		return "fsck: " + findings[0]
	}
	return fmt.Sprintf("fsck: %s (and %d more)", findings[0], n-1)
}
