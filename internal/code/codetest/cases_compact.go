package codetest

import (
	"bufio"
	"context"
	"crypto/sha1" //nolint:gosec // git's own object ids, which a case computes for the blobs it writes.
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codefault"
	"github.com/crewlet/crewlet/internal/code/compact"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/objstore"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/statelog/metrics"
)

// THE COMPACTION CASES (C1–C8): a repository's packs merged by its home git
// node and decided by its committer (ADR-0060), the merged packs retired for
// a day and swept, and every git node's cache following by a swap — through
// the production compactor every git node of a topology is built with
// (Node.Compactor), never started, so each case drives its passes itself.

// pushEach makes n pushes to main through the first git node, each one commit
// adding a file of size random bytes, and answers the last commit.
func pushEach(c *Case, p *Person, work string, n, size int) string {
	c.T.Helper()
	var head string
	for i := range n {
		head = p.Commit(work, map[string]string{fmt.Sprintf("c%03d.bin", i): randomFile(size)},
			fmt.Sprintf("push %d", i))
		c.MustPush(p, work, "main")
	}
	return head
}

// candidateOf is RepoName as a due listing through node n names it — at the
// floor of one live pack, whatever its plan makes of it.
func candidateOf(c *Case, n Node) code.CompactionCandidate {
	c.T.Helper()
	a, err := n.Read(c.Ctx, code.ReadQuery{Kind: code.ReadCompactionDue, MinPacks: 1, Fresh: Linearizable})
	if err != nil {
		c.T.Fatalf("the due listing through %s: %v", n.ID(), err)
	}
	for _, cand := range a.Due {
		if cand.Name == RepoName {
			return cand
		}
	}
	c.T.Fatalf("the due listing through %s names no %s: %+v", n.ID(), RepoName, a.Due)
	return code.CompactionCandidate{}
}

// compactOn is node n's compaction of RepoName — as its compactor makes one
// for a repository it is home for — asked again while it answers busy: the
// node's maintainer keeps the one slot a compaction may take for a moment at
// a time, and a case judging something else waits that out, a minute at most.
func compactOn(c *Case, n Node) compact.Outcome {
	c.T.Helper()
	out, err := tryCompactOn(c.Ctx, c, n)
	if err != nil && out == compact.OutcomeFailed {
		c.T.Logf("%s's compaction failed: %v", n.ID(), err)
	}
	return out
}

// tryCompactOn is compactOn reporting through nothing but its answer, for a
// goroutine a crash holds.
func tryCompactOn(ctx context.Context, c *Case, n Node) (compact.Outcome, error) {
	cand := candidateOf(c, n)
	deadline := time.Now().Add(time.Minute)
	for {
		out, err := n.Compactor().Compact(ctx, cand)
		if out != compact.OutcomeBusy || time.Now().After(deadline) {
			return out, err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// compactAtHome is a pass of every git node's compactor, in turn, until the
// repository's home has compacted it — a node that is not its home skips it —
// asked again while a pass finds it busy; it answers the home.
func compactAtHome(c *Case) Node {
	c.T.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		for _, n := range c.Fleet.Nodes() {
			report, err := n.Compactor().Poll(c.Ctx)
			switch {
			case err != nil:
				c.T.Fatalf("%s's pass: %v", n.ID(), err)
			case report.Compacted > 0:
				return n
			case report.Refused+report.Failed > 0:
				c.T.Fatalf("%s's pass refused or failed the compaction: %+v", n.ID(), report)
			}
		}
		if time.Now().After(deadline) {
			c.T.Fatalf("no git node's pass compacted %s in a minute", RepoName)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// retiredSince is every retirement repoID's rows serve above since, every
// page — of a delta from since, or of the full answer that stands in for it,
// asked for the retirements above since as a cache asks for those above its
// own position ([code.StateQuery.RetiredSince]); either way served above since
// itself, or the case cannot say which retirements the rows hold.
func retiredSince(c *Case, repoID string, since statelog.Position) []code.RetiredRow {
	c.T.Helper()
	out, served := retiredOf(c, repoID, since)
	if !served {
		c.T.Fatalf("the snapshot of %s serves no retirement above %s", RepoName, since)
	}
	return out
}

// retiredOf is every retirement repoID's rows serve above since, and whether
// the snapshot served them from since at all: a full answer serves none below
// the repository's retired mark, which a sweep raises.
func retiredOf(c *Case, repoID string, since statelog.Position) ([]code.RetiredRow, bool) {
	c.T.Helper()
	var out []code.RetiredRow
	q := code.StateQuery{RepoID: repoID, Since: since, RetiredSince: since, Fresh: Linearizable}
	for first := true; ; first = false {
		page, err := c.Fleet.State(c.Ctx, q)
		if err != nil {
			c.T.Fatalf("the snapshot of %s since %s: %v", RepoName, since, err)
		}
		if first && page.RetiredFrom != since {
			return nil, false
		}
		out = append(out, page.Retired...)
		if page.Next == nil {
			return out, true
		}
		q.Cursor = page.Next
	}
}

// counterOf is a counter's total on node n over the series whose attribute
// attr is value.
func counterOf(n Node, name, attr, value string) uint64 {
	var total uint64
	for _, s := range n.Metrics().Read() {
		if s.Name == name && s.Attrs[attr] == value {
			total += s.Total
		}
	}
	return total
}

// graphOf is the path of the commit-graph in the copy of repoID that node n's
// cache serves now, synced linearizable — and whether one is there.
func graphOf(c *Case, n Node, repoID string) (string, bool) {
	c.T.Helper()
	s, err := n.Cache().Sync(c.Ctx, repoID, Linearizable)
	if err != nil {
		c.T.Fatalf("%s's sync of %s: %v", n.ID(), RepoName, err)
	}
	defer s.Release()
	path := filepath.Join(s.Dir, "objects", "info", "commit-graph")
	_, err = os.Stat(path)
	return path, err == nil
}

// awaitGraph waits for node n's copy of repoID to hold its commit-graph — the
// file its background indexing writes first, at once, over a copy it has just
// read whole — a minute at most, and answers its path.
func awaitGraph(c *Case, n Node, repoID string) string {
	c.T.Helper()
	var path string
	if !within(time.Minute, func() bool {
		var ok bool
		path, ok = graphOf(c, n, repoID)
		return ok
	}) {
		c.T.Fatalf("%s's copy of %s has no commit-graph a minute after it read it whole", n.ID(), RepoName)
	}
	return path
}

// fsckCaches runs git's connectivity check over every node's copy of repoID.
func fsckCaches(c *Case, p *Person, repoID string) {
	c.T.Helper()
	for _, n := range c.Fleet.Nodes() {
		s, err := n.Cache().Sync(c.Ctx, repoID, Linearizable)
		if err != nil {
			c.T.Fatalf("%s's sync of %s: %v", n.ID(), RepoName, err)
		}
		if _, stderr, err := p.Git().Try(c.T, s.Dir, nil, "fsck", "--connectivity-only", "--no-dangling"); err != nil {
			c.T.Errorf("git fsck --connectivity-only over %s's copy: %v\n%s", n.ID(), err, stderr)
		}
		s.Release()
	}
}

// C1: a compaction merges a repository's packs on its home, its committer
// retires them in one record, and every git node's cache follows — the home by
// adopting the output it built, the other by its next fetch's swap, keeping
// the commit-graph — while every clone checks out every commit.
func aCompactionMergesPacksAndEveryCacheFollows(t *testing.T, newFleet NewFleet) {
	c := newCase(t, newFleet, Shape{GitNodes: 2}, Settings())
	p := NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	// Past the floor, so the home's first pass is due.
	const pushes = code.CompactMinMerge + 4
	head := pushEach(c, p, work, pushes, 1<<10)
	before := c.Rows()
	if len(before.Packs) != pushes {
		t.Fatalf("%d pushes left %d live packs", pushes, len(before.Packs))
	}
	// EVERY COPY HELD, ITS COMMIT-GRAPH WRITTEN: what the other node's swap
	// carries across.
	for _, n := range c.Fleet.Nodes() {
		awaitGraph(c, n, before.Repo.ID)
	}
	adopted := map[string]uint64{}
	kept := map[string]uint64{}
	for _, n := range c.Fleet.Nodes() {
		adopted[n.ID()] = counterOf(n, metrics.CodeCacheAdoptions, "outcome", "adopted")
		kept[n.ID()] = counterOf(n, metrics.CodeCacheSwaps, "graph", "kept")
	}

	home := compactAtHome(c)
	after := c.Rows()
	if after.Repo.LivePacks != 1 || len(after.Packs) != 1 {
		t.Fatalf("after the compaction the rows hold %d live packs (%d rows), want the one output",
			after.Repo.LivePacks, len(after.Packs))
	}
	retired := retiredSince(c, before.Repo.ID, before.Position)
	if len(retired) != pushes {
		t.Errorf("the compaction retired %d rows, want every push's %d", len(retired), pushes)
	}
	for _, r := range retired[1:] {
		if r.Position != retired[0].Position {
			t.Errorf("the retirements landed at %s and %s, want one record's position", retired[0].Position,
				r.Position)
			break
		}
	}
	if got := counterOf(home, metrics.CodeCacheAdoptions, "outcome", "adopted"); got != adopted[home.ID()]+1 {
		t.Errorf("the home adopted %d packs, want its one output linked from its build, never downloaded",
			got-adopted[home.ID()])
	}
	for i, n := range c.Fleet.Nodes() {
		if n.ID() == home.ID() {
			continue
		}
		clone := p.Clone(c.URL(i))
		if got := p.Head(clone); got != head {
			t.Errorf("a clone through %s checked out %s, want %s", n.ID(), got, head)
		}
		if got := counterOf(n, metrics.CodeCacheSwaps, "graph", "kept"); got != kept[n.ID()]+1 {
			t.Errorf("%s's fetch after the compaction swapped %d times keeping the graph, want once",
				n.ID(), got-kept[n.ID()])
		}
	}
	for i := range c.Fleet.Nodes() {
		clone := p.Clone(c.URL(i))
		if n := strings.TrimSpace(p.Git().Run(t, clone, "rev-list", "--count", "HEAD")); n != strconv.Itoa(pushes) {
			t.Errorf("a clone through node %d holds %s commits, want %d", i, n, pushes)
		}
		p.Git().Run(t, clone, "fsck", "--no-dangling")
	}
	fsckCaches(c, p, before.Repo.ID)
	assertConverged(t, c.Fleet)
}

// C2: a compaction decided while a push is in flight merges only the packs it
// planned — a merge never retires a pack it did not plan — and the push lands
// beside its output.
func aCompactionDuringAPush(t *testing.T, newFleet NewFleet) {
	c := newCase(t, newFleet, Shape{}, Settings())
	p := NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	pushEach(c, p, work, code.CompactMinMerge, 1<<10)
	head := p.Commit(work, map[string]string{"held.bin": randomFile(1 << 10)}, "held")
	held, reached := make(chan struct{}), make(chan struct{})
	c.Fleet.Faults().Arm(codefault.HostAfterScratch, codefault.Action{Once: true, Block: held, Reached: reached})
	release := sync.OnceFunc(func() { close(held) })
	defer release()
	done := make(chan Pushed, 1)
	go func() { done <- p.Push(c.Ctx, work, c.URL(0), "main") }()
	select {
	case <-reached:
	case res := <-done:
		t.Fatalf("the push ended before its scratch: %s", res)
	}
	before := c.Rows()
	if out := compactOn(c, c.Fleet.Nodes()[0]); out != compact.OutcomeApplied {
		t.Fatalf("the compaction beside a push in flight: %s, want applied", out)
	}
	release()
	if res := <-done; !res.OK("refs/heads/main") {
		t.Fatalf("the push held across the compaction: %s", res)
	}
	after := c.Rows()
	if got := after.Refs["refs/heads/main"]; got != head {
		t.Errorf("main is at %s, want the push's %s", got, head)
	}
	var fresh []string
	for _, r := range after.Packs {
		if !slices.ContainsFunc(before.Packs, func(b code.PackRow) bool { return b.PackID == r.PackID }) {
			fresh = append(fresh, r.PackID)
		}
	}
	if len(after.Packs) != 2 || len(fresh) != 2 {
		t.Errorf("the rows hold %v, want the compaction's output and the push's pack, both new", after.Packs)
	}
	assertConverged(t, c.Fleet)
}

// fetchBody is a protocol v2 fetch of want, done at once, with no progress.
func fetchBody(want string) string {
	pkt := func(s string) string { return fmt.Sprintf("%04x%s", len(s)+4, s) }
	return pkt("command=fetch\n") + "0001" + pkt("no-progress\n") + pkt("want "+want+"\n") + pkt("done\n") + "0000"
}

// packOfFetch is the pack a protocol v2 fetch's answer carries: its packfile
// section's band-1 payloads, in order.
func packOfFetch(answer []byte) ([]byte, error) {
	var pack []byte
	inPack := false
	for len(answer) > 0 {
		if len(answer) < 4 {
			return nil, fmt.Errorf("a pkt-line cut short: %q", answer)
		}
		n, err := strconv.ParseUint(string(answer[:4]), 16, 16)
		if err != nil {
			return nil, fmt.Errorf("a pkt-line length %q: %w", answer[:4], err)
		}
		if n < 4 {
			answer = answer[4:]
			if n == 0 && inPack {
				return pack, nil
			}
			continue
		}
		if int(n) > len(answer) {
			return nil, fmt.Errorf("a pkt-line of %d bytes where %d are left", n, len(answer))
		}
		payload := answer[4:n]
		answer = answer[n:]
		switch {
		case !inPack && string(payload) == "packfile\n":
			inPack = true
		case inPack && len(payload) > 0 && payload[0] == 1:
			pack = append(pack, payload[1:]...)
		case inPack && len(payload) > 0 && payload[0] == 3:
			return nil, fmt.Errorf("the fetch's answer ended in an error: %s", payload[1:])
		}
	}
	return nil, errors.New("the fetch's answer carries no complete packfile section")
}

// C3: a fetch reading a copy while a compaction is applied and a sync swaps
// the copy into its next directory reads to its end, whole: the old directory
// stays until the fetch lets it go, and goes into trash/ after.
func aCompactionDuringAFetch(t *testing.T, newFleet NewFleet) {
	c := newCase(t, newFleet, Shape{}, Settings())
	p := NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	pushEach(c, p, work, code.CompactMinMerge-1, 1<<10)
	// A PACK PAST WHAT THE CONNECTION BUFFERS, so the fetch's git is still
	// writing it while the case holds its reader still.
	head := p.Commit(work, map[string]string{"large.bin": randomFile(8 << 20)}, "large")
	c.MustPush(p, work, "main")
	node := c.Fleet.Nodes()[0]
	repoID := c.Rows().Repo.ID
	s, err := node.Cache().Sync(c.Ctx, repoID, Linearizable)
	if err != nil {
		t.Fatal(err)
	}
	old := s.Dir
	s.Release()

	// THE HOST'S OWN NAMES for where a fetch posts and what it sends: a
	// service spelled here is a second classifier to the gate that keeps
	// GitAccess the only one (internal/api/auth).
	req, err := http.NewRequestWithContext(c.Ctx, http.MethodPost, c.URL(0)+host.UploadPackSuffix,
		strings.NewReader(fetchBody(head)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", host.UploadPackRequestType)
	req.Header.Set("Git-Protocol", "version=2")
	req.SetBasicAuth("crewlet", c.Fleet.Token(t, "ceo"))
	resp, err := node.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("the fetch answered %s", resp.Status)
	}
	// 64 KiB, then nothing until the swap is done: a client this slow holds
	// its git — and the copy it reads — for as long as the case waits.
	first := make([]byte, 64<<10)
	if _, err := io.ReadFull(resp.Body, first); err != nil {
		t.Fatalf("the fetch's first 64 KiB: %v", err)
	}

	if out := compactOn(c, node); out != compact.OutcomeApplied {
		t.Fatalf("the compaction under a fetch in flight: %s, want applied", out)
	}
	s, err = node.Cache().Sync(c.Ctx, repoID, Linearizable)
	if err != nil {
		t.Fatal(err)
	}
	next := s.Dir
	s.Release()
	if next == old {
		t.Fatalf("the sync after the compaction serves %s still, want the next directory", old)
	}
	if _, err := os.Stat(old); err != nil {
		t.Errorf("the directory the fetch reads is gone while it reads: %v", err)
	}

	rest, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("the rest of the fetch: %v", err)
	}
	_ = resp.Body.Close()
	pack, err := packOfFetch(append(first, rest...))
	if err != nil {
		t.Fatal(err)
	}
	fetched := gittest.Empty(t)
	if _, stderr, err := p.Git().Try(t, fetched, strings.NewReader(string(pack)), "index-pack", "--stdin",
		"--strict"); err != nil {
		t.Fatalf("index-pack of the fetched pack: %v\n%s", err, stderr)
	}
	gittest.SetRefs(t, fetched, map[string]string{"refs/heads/main": head})
	if _, stderr, err := p.Git().Try(t, fetched, nil, "fsck", "--connectivity-only", "--no-dangling"); err != nil {
		t.Errorf("the fetched history is not whole: %v\n%s", err, stderr)
	}
	if !within(10*time.Second, func() bool {
		_, err := os.Stat(old)
		return errors.Is(err, os.ErrNotExist)
	}) {
		t.Errorf("the old directory %s stayed after the fetch let it go", old)
	}
	assertConverged(t, c.Fleet)
}

// ARetiredPackOutlivesItsLongestRead is C4 as a check a topology that lies can
// be run through, nil when the topology behaves: a compaction's inputs keep
// their objects from the collector for code.RetireGrace — a first collection
// past collect.PendingGrace of every input, where only the retired rows shield
// them, deletes none — and once the code-retention duty's sweep has removed
// the rows past the grace, the next collection deletes them all, the outputs
// untouched throughout.
func ARetiredPackOutlivesItsLongestRead(t *testing.T, newFleet NewFleet) error {
	t.Helper()
	c := newCase(t, newFleet, Shape{}, Settings())
	p := NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	pushEach(c, p, work, code.CompactMinMerge, 1<<10)
	before := c.Rows()
	if out := compactOn(c, c.Fleet.Nodes()[0]); out != compact.OutcomeApplied {
		return fmt.Errorf("the compaction: %s, want applied", out)
	}
	now := time.Now()
	after := c.Rows()
	inputs := map[string]bool{}
	for _, r := range before.Packs {
		inputs[r.Object.Name()] = true
	}
	outputs := map[string]bool{}
	for _, r := range after.Packs {
		outputs[r.Object.Name()] = true
	}
	held := func(want map[string]bool) []string {
		stored := map[string]bool{}
		for _, n := range objectNames(t, c.Fleet.Objects()) {
			stored[n] = true
		}
		var missing []string
		for n := range want {
			if !stored[n] {
				missing = append(missing, n)
			}
		}
		return missing
	}

	if r := c.Fleet.Retain(t, now.Add(23*time.Hour)); r.SweptRows != 0 {
		return fmt.Errorf("a retention pass 23 h after the compaction swept %d retired rows, inside the grace",
			r.SweptRows)
	}
	c.Fleet.CollectAt(t, now.Add(24*time.Hour+30*time.Minute))
	if missing := held(inputs); len(missing) > 0 {
		return fmt.Errorf("a collection 24 h 30 min after the compaction deleted %d retired objects inside "+
			"their grace (%v)", len(missing), missing)
	}
	if retired, served := retiredOf(c, before.Repo.ID, before.Position); !served || len(retired) != len(before.Packs) {
		return fmt.Errorf("inside the grace the rows hold %d retirements (served %v), want the %d the compaction "+
			"made", len(retired), served, len(before.Packs))
	}
	if missing := held(outputs); len(missing) > 0 {
		return fmt.Errorf("the collection deleted the compaction's outputs %v", missing)
	}
	if r := c.Fleet.Retain(t, now.Add(24*time.Hour+31*time.Minute)); r.SweptRows != len(before.Packs) {
		return fmt.Errorf("a retention pass past the grace swept %d retired rows, want the %d the compaction "+
			"retired", r.SweptRows, len(before.Packs))
	}
	c.Fleet.CollectAt(t, now.Add(25*time.Hour+32*time.Minute))
	if missing := held(inputs); len(missing) != len(inputs) {
		return fmt.Errorf("once swept, %d retired objects of %d stayed past the collection", len(inputs)-len(missing),
			len(inputs))
	}
	if missing := held(outputs); len(missing) > 0 {
		return fmt.Errorf("the second collection deleted the compaction's outputs %v", missing)
	}
	if retired, served := retiredOf(c, before.Repo.ID, before.Position); served && len(retired) > 0 {
		return fmt.Errorf("once swept, the rows still serve %d retirements from below the sweep", len(retired))
	}
	return converged(c.Ctx, c.Fleet)
}

// C4 (ARetiredPackOutlivesItsLongestRead).
func aRetiredPackOutlivesItsLongestRead(t *testing.T, newFleet NewFleet) {
	if err := ARetiredPackOutlivesItsLongestRead(t, newFleet); err != nil {
		t.Error(err)
	}
}

// blobID is git's object id of a blob holding body.
func blobID(body string) string {
	h := sha1.New() //nolint:gosec // see the import.
	fmt.Fprintf(h, "blob %d\x00%s", len(body), body)
	return hex.EncodeToString(h.Sum(nil))
}

// bulkPacks proposes n pushes through the data nodes, each creating a ref of
// its own at tip and carrying a pack of one blob of its own: n live packs, no
// person's git paying for any — packed eight at a time out of one source
// repository, each stored as one bundle, as a receiving node stores a push.
func bulkPacks(c *Case, p *Person, repoID, tip string, n int) {
	c.T.Helper()
	src := gittest.Empty(c.T)
	bodies := make([]string, n)
	gittest.FastImport(c.T, p.Git(), src, func(w *bufio.Writer) error {
		for i := range bodies {
			bodies[i] = fmt.Sprintf("bulk %d\n", i)
			if _, err := fmt.Fprintf(w, "blob\ndata %d\n%s\n", len(bodies[i]), bodies[i]); err != nil {
				return err
			}
		}
		return nil
	})
	objects, err := objstore.NewStore(c.Fleet.Objects())
	if err != nil {
		c.T.Fatal(err)
	}
	scratch := c.T.TempDir()
	principal := code.Principal{Kind: code.PrincipalOperator, OperatorID: "codetest-bulk", Seat: "ceo"}
	g, ctx := errgroup.WithContext(c.Ctx)
	g.SetLimit(8)
	for i, body := range bodies {
		g.Go(func() error {
			dir := filepath.Join(scratch, strconv.Itoa(i))
			if err := os.MkdirAll(filepath.Join(dir, "objects", "pack"), 0o700); err != nil {
				return err
			}
			out, stderr, err := p.Git().TryContext(ctx, src, strings.NewReader(blobID(body)+"\n"),
				"-c", "pack.writeReverseIndex=true", "-c", "pack.indexVersion=2", "pack-objects", "-q",
				filepath.Join(dir, "objects", "pack", "pack"))
			if err != nil {
				return fmt.Errorf("pack bulk blob %d: %w\n%s", i, err, stderr)
			}
			checksum := strings.TrimSpace(out)
			pushID := uuid.Must(uuid.NewV7()).String()
			path := filepath.Join(dir, "objects", "pack", "pack-"+checksum+".pack")
			ref, err := code.LayBundle(pushID, path, checksum)
			if err != nil {
				return err
			}
			if err := code.StoreBundle(ctx, objects, path, ref, nil); err != nil {
				return err
			}
			res, err := c.Fleet.Propose(ctx, code.Proposal{
				PushID: pushID, RepoID: repoID, Principal: principal, Via: code.ViaGit,
				Node: c.Fleet.Nodes()[0].ID(), ReceivedAt: time.Now().UTC(), Pack: ref,
				Commands: []code.Command{{Ref: fmt.Sprintf("refs/heads/bulk/%04d", i), Old: code.ZeroOID, New: tip}},
			})
			switch {
			case err != nil:
				return fmt.Errorf("propose bulk pack %d: %w", i, err)
			case res.Outcome != statelog.OutcomeApplied || len(res.Answer.Commands) != 1 ||
				res.Answer.Commands[0].Result != code.ResultOK:
				return fmt.Errorf("bulk pack %d was answered %s: %+v", i, res.Outcome, res.Answer.Commands)
			}
			return os.RemoveAll(dir)
		})
	}
	if err := g.Wait(); err != nil {
		c.T.Fatalf("the bulk packs: %v", err)
	}
}

// C5: a repository at code.MaxPacksPerRepo live packs refuses a push carrying
// one more `compaction_behind`, BEFORE its bundle is stored, while a push that
// carries none lands; its home names it in its alarm reading while it is so;
// and once a compaction has merged its packs the push lands and the reading
// is clear.
func compactionBehindRefusesAPack(t *testing.T, newFleet NewFleet) {
	c := newCase(t, newFleet, Shape{}, Settings())
	p := NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	tip := pushEach(c, p, work, 1, 1<<10)
	repoID := c.Rows().Repo.ID
	bulkPacks(c, p, repoID, tip, code.MaxPacksPerRepo-2)
	pushEach(c, p, work, 1, 1<<10) // the bound's last: lands
	if live := c.Rows().Repo.LivePacks; live != code.MaxPacksPerRepo {
		t.Fatalf("the repository holds %d live packs, want the bound's %d", live, code.MaxPacksPerRepo)
	}

	p.Commit(work, map[string]string{"one-more.bin": randomFile(1 << 10)}, "one more")
	stored := len(objectNames(t, c.Fleet.Objects()))
	res := p.Push(c.Ctx, work, c.URL(0), "main")
	if res.Err == nil {
		t.Fatalf("a push past the pack bound exited 0: %s", res)
	}
	wantRefused(t, res, "refs/heads/main", string(code.RefusalCompactionBehind))
	if got := len(objectNames(t, c.Fleet.Objects())); got != stored {
		t.Errorf("the refused push stored %d objects, want none: the receiver refuses before the upload",
			got-stored)
	}
	if del := p.Push(c.Ctx, work, c.URL(0), ":refs/heads/bulk/0000"); !del.OK("refs/heads/bulk/0000") {
		t.Errorf("a push of a deletion alone at the bound: %s, want ok", del)
	}

	home := compactAtHome(c)
	var r statelog.Reading
	home.Compactor().AlarmReading(&r)
	if r.CodeCompactionBehind != 1 || r.CodeCompactionBehindRepo != RepoName {
		t.Errorf("the home's reading after the pass that found %s at the bound names %d repositories, the "+
			"most %q; want it", RepoName, r.CodeCompactionBehind, r.CodeCompactionBehindRepo)
	}
	if live := c.Rows().Repo.LivePacks; live >= code.MaxPacksPerRepo {
		t.Fatalf("after one compaction the repository holds %d live packs, still at the bound", live)
	}
	if again := p.Push(c.Ctx, work, c.URL(0), "main"); !again.OK("refs/heads/main") {
		t.Errorf("the push once compacted below the bound: %s, want ok", again)
	}
	if _, err := home.Compactor().Poll(c.Ctx); err != nil {
		t.Fatal(err)
	}
	r = statelog.Reading{}
	home.Compactor().AlarmReading(&r)
	if r.CodeCompactionBehind != 0 {
		t.Errorf("the home's reading once the repository is below the bound names %d repositories, want none",
			r.CodeCompactionBehind)
	}
	assertConverged(t, c.Fleet)
}

// compactCrash is one point C6 crashes the home at, and the offset an upload's
// point is hit at.
type compactCrash struct {
	point codefault.Point
	at    int64
}

// compactCrashes is every point of codefault.CompactPath(), the upload's at
// its first byte and mid-bundle.
func compactCrashes() []compactCrash {
	var out []compactCrash
	for _, p := range codefault.CompactPath() {
		out = append(out, compactCrash{point: p})
		if p == codefault.CompactUpload {
			out = append(out, compactCrash{point: p, at: 128 << 10})
		}
	}
	return out
}

// C6: a crash of the home at every step of a compaction leaves a truthful
// state: nothing applied — the outputs named by no row, which the collector
// deletes and nothing else — or applied once; a compaction again is applied or
// not due; every cache converges; a push lands.
func aCrashAtEveryCompactionStepLeavesATruthfulState(t *testing.T, newFleet NewFleet) {
	for _, crash := range compactCrashes() {
		name := string(crash.point)
		if crash.at > 0 {
			name += "@" + strconv.FormatInt(crash.at, 10)
		}
		t.Run(name, func(t *testing.T) { compactCrashAt(t, newFleet, crash) })
	}
}

// compactCrashAt is C6 at one point.
func compactCrashAt(t *testing.T, newFleet NewFleet, crash compactCrash) {
	c := newCase(t, newFleet, Shape{GitNodes: 2}, Settings())
	p := NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	// A PUSH OF 64 KiB EACH, so an output is past one read of its upload and
	// a crash mid-bundle is one.
	pushEach(c, p, work, code.CompactMinMerge, 64<<10)
	before := c.Rows()
	retiredObjects := map[string]string{}
	for _, r := range before.Packs {
		retiredObjects[r.PackID] = r.Object.Name()
	}

	reached := make(chan struct{})
	c.Fleet.Faults().Arm(crash.point, codefault.Action{Once: true, Crash: true, Reached: reached, At: crash.at})
	home := c.Fleet.Nodes()[0]
	done := make(chan compact.Outcome, 1)
	go func() {
		out, _ := tryCompactOn(context.WithoutCancel(c.Ctx), c, home)
		done <- out
	}()
	select {
	case <-reached:
	case out := <-done:
		t.Fatalf("the compaction ended %s before it reached %s", out, crash.point)
	}
	c.Fleet.Kill(t, home.ID())
	c.Fleet.Restart(t, home.ID())

	rows := c.Rows()
	switch {
	case len(rows.Packs) == len(before.Packs):
		// Nothing applied: every input live still.
	case len(rows.Packs) == 1 && rows.Repo.LivePacks == 1:
		// Applied once.
		if retired := retiredSince(c, before.Repo.ID, before.Position); len(retired) != len(before.Packs) {
			t.Errorf("the compaction applied retired %d rows, want %d", len(retired), len(before.Packs))
		}
	default:
		t.Fatalf("after a crash at %s the rows hold %d packs: neither nothing applied nor applied once",
			crash.point, len(rows.Packs))
	}
	if out := compactOn(c, c.Fleet.Nodes()[0]); out != compact.OutcomeApplied && out != compact.OutcomeNotDue {
		t.Errorf("the compaction again after a crash at %s: %s, want applied or not due", crash.point, out)
	}
	after := c.Rows()
	if len(after.Packs) != 1 {
		t.Errorf("the rows hold %d packs after the compaction again, want one", len(after.Packs))
	}
	// THE COLLECTOR DELETES WHAT NO ROW NAMES AND NOTHING ELSE: the outputs of a
	// compaction that never applied, and none of a retired pack's objects —
	// shielded by its row for the grace — nor a live one's.
	named := map[string]bool{}
	for _, r := range after.Packs {
		named[r.Object.Name()] = true
	}
	for _, r := range retiredSince(c, before.Repo.ID, before.Position) {
		named[retiredObjects[r.PackID]] = true
	}
	c.Fleet.Collect(t)
	stored := map[string]bool{}
	for _, n := range objectNames(t, c.Fleet.Objects()) {
		stored[n] = true
		if !named[n] {
			t.Errorf("%s is named by no row, live or retired, and the collector left it", n)
		}
	}
	for n := range named {
		if !stored[n] {
			t.Errorf("the collector deleted %s, which a row names", n)
		}
	}
	assertConverged(t, c.Fleet)
	pushEach(c, p, work, 1, 1<<10)
	assertConverged(t, c.Fleet)
}

// C7: a sync that crashed at the swap's one irreversible step leaves the old
// directory serving, the highest in repos/, with its state, and the scratch it
// staged the next one in emptied at boot; the first round after the restart
// reads the merged packs as the retirements they are (the state's
// LeavingSince), so it sheds them in place — a boot's first round is an
// incarnation nobody holds yet (cache's doc, "A swap") — keeps the
// commit-graph, the same file, and converges.
func aCrashMidSwapLeavesTheOldIncarnation(t *testing.T, newFleet NewFleet) {
	c := newCase(t, newFleet, Shape{}, Settings())
	p := NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	pushEach(c, p, work, code.CompactMinMerge, 1<<10)
	node := c.Fleet.Nodes()[0]
	repoID := c.Rows().Repo.ID
	awaitGraph(c, node, repoID)
	if out := compactOn(c, node); out != compact.OutcomeApplied {
		t.Fatalf("the compaction: %s, want applied", out)
	}

	reached := make(chan struct{})
	c.Fleet.Faults().Arm(codefault.SyncSwapBeforeRename, codefault.Action{Once: true, Crash: true, Reached: reached})
	done := make(chan error, 1)
	go func() {
		s, err := node.Cache().Sync(context.WithoutCancel(c.Ctx), repoID, Linearizable)
		if err == nil {
			s.Release()
		}
		done <- err
	}()
	select {
	case <-reached:
	case err := <-done:
		t.Fatalf("the sync after the compaction ended (%v) before it reached the swap's rename", err)
	}
	repos := filepath.Join(node.CacheDir(), "repos")
	highest := func() string {
		var top string
		var n int
		for _, e := range entries(repos) {
			rest, ok := strings.CutPrefix(e, repoID+".")
			if !ok {
				continue
			}
			if k, err := strconv.Atoi(strings.TrimSuffix(rest, ".git")); err == nil && k >= n {
				top, n = e, k
			}
		}
		return top
	}
	crashedAt := highest()
	dir := node.CacheDir()
	c.Fleet.Kill(t, node.ID())
	diskAsTheCrashLeftIt(t, dir)
	graph := gittest.FileIdentity(t, filepath.Join(repos, crashedAt, "objects", "info", "commit-graph"))
	c.Fleet.Restart(t, node.ID())
	node = c.Fleet.Nodes()[0]
	if got := highest(); got != crashedAt {
		t.Errorf("after the restart the highest directory is %s, want %s, which the crash left serving", got,
			crashedAt)
	}
	if got := entries(filepath.Join(node.CacheDir(), "scratch")); len(got) > 0 {
		t.Errorf("after the restart scratch/ holds %v, want it emptied at boot", got)
	}
	switch path, ok := graphOf(c, node, repoID); {
	case !ok:
		t.Error("the first round after the restart dropped the commit-graph of a copy a compaction retired " +
			"packs from: it read them as packs no row accounts for")
	case gittest.FileIdentity(t, path) != graph:
		t.Error("the first round after the restart wrote the commit-graph again rather than keeping it")
	}
	assertConverged(t, c.Fleet)
}

// diskAsTheCrashLeftIt puts in dir's place a copy of what it holds now, while
// a crash holds a step of the cache over it: the disk as a process that died
// at that step leaves it. A process that dies gives its claim on the
// directory back with it, and a killed node in this process cannot — its
// held step is work of the cache's still running, which keeps the claim
// (cache.Cache.Stop) — so the restart boots over the copy, whose claim file
// is a new one, and the old directory, held, is moved aside; the held step,
// released at the case's end, finds nothing of its own to finish.
func diskAsTheCrashLeftIt(t *testing.T, dir string) {
	t.Helper()
	copied := dir + ".copy"
	if err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		to := filepath.Join(copied, rel)
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.IsDir():
			return os.MkdirAll(to, info.Mode().Perm())
		case !info.Mode().IsRegular():
			return nil
		}
		src, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = src.Close() }()
		dst, err := os.OpenFile(to, os.O_CREATE|os.O_EXCL|os.O_WRONLY, info.Mode().Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(dst, src); err != nil {
			_ = dst.Close()
			return err
		}
		return dst.Close()
	}); err != nil {
		t.Fatalf("codetest: copy %s as the crash left it: %v", dir, err)
	}
	if err := os.Rename(dir, dir+".crashed"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(copied, dir); err != nil {
		t.Fatal(err)
	}
}

// C8: a git node that was down while a compaction applied reads the
// repository whole at its first round — and the retirements above its own
// position with it — so it sheds the merged packs keeping the commit-graph it
// had, the same file; without those retirements (the control) it cannot tell
// the merged packs from strays, and drops it.
func aRestartedNodeKeepsTheCommitGraphOfACompactedRepository(t *testing.T, newFleet NewFleet) {
	for _, control := range []bool{false, true} {
		name := "the retirements served"
		if control {
			name = "the retirements withheld"
		}
		t.Run(name, func(t *testing.T) { restartedNodeKeepsGraph(t, newFleet, control) })
	}
}

// retirementsWithheld is an estate whose snapshots never serve a full
// answer's retirements: the control of C8.
type retirementsWithheld struct{ host.Estate }

func (e retirementsWithheld) RepoState(ctx context.Context, q code.StateQuery) (code.RepoSnapshot, error) {
	q.RetiredSince = statelog.Position{}
	return e.Estate.RepoState(ctx, q)
}

// restartedNodeKeepsGraph is C8, its retirements withheld where control.
func restartedNodeKeepsGraph(t *testing.T, newFleet NewFleet, control bool) {
	shape := Shape{GitNodes: 2}
	if control {
		shape.WrapEstate = func(e host.Estate) host.Estate { return retirementsWithheld{e} }
	}
	c := newCase(t, newFleet, shape, Settings())
	p := NewPerson(t, c.Fleet, "ceo")
	work := p.Init()
	pushEach(c, p, work, code.CompactMinMerge, 1<<10)
	repoID := c.Rows().Repo.ID
	a, b := c.Fleet.Nodes()[0], c.Fleet.Nodes()[1]
	graph := awaitGraph(c, b, repoID)
	before := gittest.FileIdentity(t, graph)
	bID := b.ID()
	c.Fleet.StopGit(t, bID)
	if out := compactOn(c, a); out != compact.OutcomeApplied {
		t.Fatalf("the compaction through %s with %s down: %s, want applied", a.ID(), bID, out)
	}
	c.Fleet.StartGit(t, bID)
	var restarted Node
	for _, n := range c.Fleet.Nodes() {
		if n.ID() == bID {
			restarted = n
		}
	}
	path, ok := graphOf(c, restarted, repoID)
	switch {
	case control && ok && gittest.FileIdentity(t, path) == before:
		t.Errorf("with the retirements withheld the first round kept the commit-graph: the case cannot tell " +
			"the rule it holds from its absence")
	case !control && !ok:
		t.Errorf("%s's first round after its restart dropped the commit-graph of a merged copy", bID)
	case !control && gittest.FileIdentity(t, path) != before:
		t.Errorf("%s's first round after its restart wrote the commit-graph again rather than linking it", bID)
	}
	assertConverged(t, c.Fleet)
}
