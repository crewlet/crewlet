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

// Verify checks repoID's cache against its rows, after a linearizable sync:
//
//  1. its directory listed against the image grammar (sweep.go) — every
//     entry it does not admit is a problem — with the repository's rounds
//     held off, so no step half done reads as damage, and the files a
//     maintenance run in flight holds while it runs admitted: the run is
//     left to finish, and none starts from here to the fsck
//     (holdMaintainer);
//  2. the repository's whole rows read at least as fresh as the listing,
//     AFTER it: a pack installed between the two has its row applied first,
//     so it is in the answer, and nothing removes a row-named pack;
//  3. every pack a row added at or before the listing's position held to its
//     row: installed, each section at its row's size, and the three files
//     back to back — .pack, .idx, .rev, the bundle the push stored — hashing
//     to the row's Hash; a pack no row names is a stray;
//  4. packed-refs hashed against the digest this cache wrote with it;
//  5. git's connectivity check, `fsck --connectivity-only`, in a FREE
//     maintenance slot: refused when the lane is full rather than queued
//     ([gitproc.ErrBusy], answered beside the report so far), because the
//     lane never waits — the maintainer keeps one of its slots for exactly
//     this — and held to the node's memory budget like the background work
//     beside it: busy too when what is in flight leaves it no room, and
//     refused ([ErrNoMemory]) when its charge alone is past the budget.
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
func (c *Cache) Verify(ctx context.Context, repoID string, repair bool) (VerifyReport, error) {
	s, err := c.Sync(ctx, repoID, statelog.Freshness{Level: statelog.ReadLinearizable})
	if err != nil {
		return VerifyReport{Repo: repoID}, fmt.Errorf("cache: verify %s: %w", repoID, err)
	}
	defer s.Release()
	defer c.holdMaintainer(s.entry)()
	report, whole, err := c.check(ctx, s)
	if err != nil || !repair || len(report.Problems) == 0 {
		return report, err
	}
	rebuilt, err := c.rebuild(ctx, s, RebuildVerify, false, whole)
	if err != nil {
		return report, fmt.Errorf("cache: verify %s: a rebuild could not repair it: %w", report.label(), err)
	}
	defer rebuilt.Release()
	second, _, err := c.check(ctx, rebuilt)
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
// next tick. Verify holds it from its first listing to its last fsck: the fsck
// takes a maintenance slot or is refused, and a run started between the two
// would take the lane's last free slot — on a node of one or two cores the
// lane's only one — from the person who asked, who would be told to run it
// again into the same race. A run ALREADY in flight is left to finish, never
// stopped: work thrown away for a check is work the maintainer does again at
// once, and its lock and temporary files are admitted by the listing
// instead (maintenanceWriting). On a one-slot lane such a run makes the fsck
// busy until it ends, and the next verify finds the repository no longer
// due.
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

// check is one pass of Verify over the incarnation s pins. Beside its report
// it answers the checksums of the packs it held WHOLE to their rows — every
// section at its row's size and the bundle hashing to the row's Hash — which
// a repair need not download again.
func (c *Cache) check(ctx context.Context, s Synced) (VerifyReport, []string, error) {
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
	problem, err = c.fsck(ctx, e, inc, r.label())
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
// flight on inc when the listing begins is left to finish — Verify holds the
// maintainer off (holdMaintainer), so none starts — and the files it holds
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

// fsck runs git's connectivity check in inc, in a free maintenance slot, and
// answers a problem when git found one. A slot refused is Verify's error,
// never a problem: nothing was checked.
//
// It is held to the node's memory like every whole-repository process the
// cache starts (memory.go): a connectivity check holds FsckBytesPerObject an
// object of its repository — some 11 GB over the largest the defaults admit —
// and is charged that and room beside it for one transfer of the repository
// against the budget the maintenance in flight shares. A check whose charge alone is past the
// budget is refused ([ErrNoMemory]) — this node cannot check the repository
// at its memory, and one with more can — and one the work in flight leaves
// no room for is busy ([gitproc.ErrBusy]), as a full lane is: refused rather
// than queued, and answered when it is asked again.
func (c *Cache) fsck(ctx context.Context, e *entry, inc *incarnation, label string) (string, error) {
	e.mu.Lock()
	held := inc.diskBytes + inc.metaBytes
	mem := charge(inc.objects, FsckBytesPerObject)
	e.mu.Unlock()
	switch c.maint.admitOther(mem) {
	case pastBudget:
		return "", fmt.Errorf("cache: verify %s: %w: its connectivity check needs about %d bytes, and this node "+
			"can give its background work %d of its memory — run it on a node with more memory", label,
			ErrNoMemory, mem, c.maint.budgetNow())
	case heldBack:
		return "", fmt.Errorf("cache: verify %s: %w — its connectivity check needs about %d bytes, which the "+
			"background work in flight leaves no room for; run it again", label, gitproc.ErrBusy, mem)
	}
	defer c.maint.doneOther(mem)
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
	_, err := c.git.Run(ctx, gitproc.Cmd{
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
	case errors.Is(err, gitproc.ErrBusy):
		return "", fmt.Errorf("cache: verify %s: %w — fsck runs only in a free maintenance slot "+
			"(half this node's git process slots); run it again", label, err)
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
