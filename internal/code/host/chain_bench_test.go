package host_test

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/code/host"
	"github.com/crewlet/crewlet/internal/objstore/memobj"
)

// BenchmarkBuildChain measures what a chain's bounds are read against
// (chain.go's head; code.MaxChainObjectBytes, code.ChainPackDeadline): one
// chain of code.MaxChainEntries entries, each a merge commit onto the tip the
// entry before it made, built by the Gitlet node's own BuildChain — its merges,
// its one pack and that pack's `index-pack --strict`, and its bundle stored
// in the in-memory store — in three arms:
//
//   - merges=small: each entry adds a file of its own, onto a main that moved
//     by one commit, over a history of chainHistory commits and a tree of
//     1 000 files — the queue's common chain, every merge a commit and the
//     trees above one path;
//   - pack=at-bound: each entry changes the last line of a file of its own of
//     random base64, every one of which main changed the first line of, so
//     each merge writes a new blob no other entry's deltas against, sized so
//     the chain's objects hold just under code.MaxChainObjectBytes — the
//     largest pack a chain makes, of the most objects;
//   - pack=past-bound (CREWLET_BENCH_LARGE=1, gittest.LargeEnv): the same
//     with each file chainPastLines lines, 4.9 MiB, its loose objects just
//     under code.MaxWritePackBytes and the chain's 314 MiB, the chain's
//     merges, objects and pack bounds lifted to build what no chain may — the
//     rate a pack is made at, which code.MaxChainObjectBytes is set from.
//
// ms/chain is the whole BuildChain, ms/pack its final pack-objects and
// index-pack together, the figure code.ChainPackDeadline bounds, each of the
// two on its own beside it, and MiB/pack the pack's size.
//
// MEASURED: doc.go's WHAT A CHAIN COSTS.
func BenchmarkBuildChain(b *testing.B) {
	codetest.Quiet(b)
	for _, arm := range []struct {
		name  string
		lines int
		lift  bool
	}{
		{name: "merges=small"},
		{name: "pack=at-bound", lines: chainAtBoundLines},
		{name: "pack=past-bound", lines: chainPastLines, lift: true},
	} {
		b.Run(fmt.Sprintf("entries=%d/%s", code.MaxChainEntries, arm.name), func(b *testing.B) {
			if arm.lift {
				gittest.Large(b, fmt.Sprintf("%d files of %d lines of random base64, three versions of each "+
					"pushed", code.MaxChainEntries, arm.lines))
			}
			r := newChainRig(b, arm.lines)
			s := r.f.Nodes()[0].Host()
			if arm.lift {
				host.SetChainMerges(s, time.Hour)
				host.SetChainObjects(s, 1<<40)
				host.SetChainPack(s, time.Hour)
			}
			packed := host.GitTook(s, "pack-objects")
			checked := host.GitTook(s, "index-pack", "--strict")
			var chains, packs, objects, checks []time.Duration
			var size int64
			for b.Loop() {
				packedBefore, _ := packed()
				checkedBefore, _ := checked()
				began := time.Now()
				res, err := s.BuildChain(b.Context(), r.chain())
				took := time.Since(began)
				b.StopTimer()
				if err != nil || res.Settle == nil {
					b.Fatalf("the chain was not built: %+v %v", res, err)
				}
				if built := len(res.Commits); built != code.MaxChainEntries {
					b.Fatalf("the chain reached %d of its %d entries", built, code.MaxChainEntries)
				}
				for _, c := range res.Commits {
					if c.Commit == "" {
						b.Fatalf("the chain skipped an entry: %+v", c)
					}
				}
				packedAfter, _ := packed()
				checkedAfter, _ := checked()
				chains = append(chains, took)
				packs = append(packs, packedAfter-packedBefore+checkedAfter-checkedBefore)
				objects = append(objects, packedAfter-packedBefore)
				checks = append(checks, checkedAfter-checkedBefore)
				if res.Pack != nil {
					size = res.Pack.Size
				}
				if err := res.Settle(b.Context(), false); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
			}
			b.ReportMetric(medianMillis(chains), "ms/chain")
			b.ReportMetric(medianMillis(packs), "ms/pack")
			b.ReportMetric(medianMillis(objects), "ms/pack-objects")
			b.ReportMetric(medianMillis(checks), "ms/index-pack")
			b.ReportMetric(float64(size)/(1<<20), "MiB/pack")
		})
	}
}

// The fixtures BenchmarkBuildChain builds.
const (
	// chainHistory is how many commits main's history holds below the
	// commit the chain's branches leave it at.
	chainHistory = 20_000
	// chainLineBytes is a big file's line: 76 characters of base64 and a
	// newline.
	chainLineBytes = 77
	// chainAtBoundLines is how many lines each of the at-bound arm's files
	// holds: code.MaxChainObjectBytes shared by code.MaxChainEntries merges,
	// less half a mebibyte for their commits and trees (a commit of some
	// 300 bytes, the root tree and big/'s tree of 64 entries, 2.6 KB, each
	// merge), so the chain's objects hold just under the bound.
	chainAtBoundLines = (code.MaxChainObjectBytes - 512<<10) / code.MaxChainEntries / chainLineBytes
	// chainPastLines is how many lines each of the past-bound arm's files
	// holds: 4.9 MiB a file, whose random base64 deflates to some 3.8 MiB —
	// one merge's objects just under code.MaxWritePackBytes.
	chainPastLines = 66_700
)

// chainRig is one arm's fixture: one Gitlet node holding a repository whose main
// moved by one commit since code.MaxChainEntries branches left it.
type chainRig struct {
	f     *codetest.SoloFleet
	repo  string
	seen  codetest.Rows
	tip   string
	heads []string
}

// newChainRig builds the fixture with ONE fast-import and one push: main's
// root commit, then its history (the small arm, lines 0) or its big files of
// lines lines (the others), then each entry's branch off it, then main's
// move.
func newChainRig(b *testing.B, lines int) *chainRig {
	b.Helper()
	r := &chainRig{f: writeFleet(b, memobj.New(), newWriteFigures())}
	r.f.Declare(b, code.Declared{Name: codetest.RepoName, Settings: codetest.Settings()})
	p := codetest.NewPerson(b, r.f, codetest.FixturePat)
	work := p.Init()
	now := time.Now().Add(-time.Hour).Unix()
	big := lines > 0
	gittest.FastImport(b, p.Git(), work, func(w *bufio.Writer) error {
		if _, err := fmt.Fprintf(w, "commit refs/heads/main\nmark :1\ncommitter A <a@example.com> %d +0000\n"+
			"data 5\nroot\n", now); err != nil {
			return err
		}
		if big {
			for k := range code.MaxChainEntries {
				if err := bigFile(w, k, lines, -1); err != nil {
					return err
				}
			}
		} else if err := commitTree(w, 1_000); err != nil {
			return err
		}
		if _, err := fmt.Fprintf(w, "\n"); err != nil {
			return err
		}
		base := ":1"
		if !big {
			for h := range chainHistory {
				if _, err := fmt.Fprintf(w, "commit refs/heads/main\nmark :%d\ncommitter A <a@example.com> %d "+
					"+0000\ndata 8\nhistory\n", h+2, now+int64(h)); err != nil {
					return err
				}
				content := fmt.Sprintf("version %05d\n", h)
				if _, err := fmt.Fprintf(w, "M 100644 inline history/f%03d.txt\ndata %d\n%s\n", h%100,
					len(content), content); err != nil {
					return err
				}
			}
			base = fmt.Sprintf(":%d", chainHistory+1)
		}
		for k := range code.MaxChainEntries {
			if _, err := fmt.Fprintf(w, "commit refs/heads/e%02d\ncommitter A <a@example.com> %d +0000\n"+
				"data 6\nentry\nfrom %s\n", k, now+chainHistory+int64(k), base); err != nil {
				return err
			}
			if big {
				if err := bigFile(w, k, lines, lines-1); err != nil {
					return err
				}
			} else if err := inlineFile(w, fmt.Sprintf("chain/e%02d.txt", k)); err != nil {
				return err
			}
			if _, err := fmt.Fprintf(w, "\n"); err != nil {
				return err
			}
		}
		if _, err := fmt.Fprintf(w, "commit refs/heads/main\ncommitter A <a@example.com> %d +0000\n"+
			"data 6\nmoved\nfrom %s\n", now+chainHistory+code.MaxChainEntries, base); err != nil {
			return err
		}
		if big {
			for k := range code.MaxChainEntries {
				if err := bigFile(w, k, lines, 0); err != nil {
					return err
				}
			}
		} else if err := inlineFile(w, "moved.txt"); err != nil {
			return err
		}
		_, err := fmt.Fprintf(w, "\n")
		return err
	})
	refspecs := []string{"main"}
	for k := range code.MaxChainEntries {
		refspecs = append(refspecs, fmt.Sprintf("e%02d", k))
	}
	mustPush(b, p, work, repoURL(r.f.Nodes()[0]), refspecs...)
	for k := range code.MaxChainEntries {
		r.heads = append(r.heads, strings.TrimSpace(p.Git().Run(b, work, "rev-parse", fmt.Sprintf("e%02d", k))))
	}
	r.tip = strings.TrimSpace(p.Git().Run(b, work, "rev-parse", "main"))
	rows, err := r.f.Rows(b.Context(), codetest.RepoName, codetest.Linearizable)
	if err != nil {
		b.Fatal(err)
	}
	r.repo, r.seen = rows.Repo.ID, rows
	return r
}

// chain is the arm's chain under a new id: every branch merged onto main's
// tip, in order, each entry an hour old.
func (r *chainRig) chain() code.ChainBuild {
	at := time.Now().Add(-time.Hour)
	c := code.ChainBuild{RepoID: r.repo, ChainID: opNow(), TargetRef: "refs/heads/main", Base: r.tip,
		Seen: r.seen.Position}
	for k, head := range r.heads {
		c.Entries = append(c.Entries, code.ChainEntry{EntryID: code.QueueEntryID(opAt(at.Add(time.Duration(k) *
			time.Second))), Head: head, Method: code.MergeMethodMerge, Merger: pat(),
			Change: code.ChainChange{ID: fmt.Sprintf("00000000-0000-7000-8000-%012d", k+1), Number: int64(k + 1),
				Title: fmt.Sprintf("Land e%02d", k), SourceRef: fmt.Sprintf("refs/heads/e%02d", k),
				Owner: codetest.FixturePat, Authors: []string{codetest.FixturePat}}})
	}
	return c
}

// bigFile writes file k of a big arm as one fast-import file line: lines
// lines of base64 of random bytes seeded by k — the same lines in every
// version — but for line changed (none where it is -1), which reads
// "changed".
func bigFile(w *bufio.Writer, k, lines, changed int) error {
	rnd := rand.New(rand.NewPCG(uint64(k), 0x636861696e))
	var body bytes.Buffer
	raw := make([]byte, 57)
	for i := range lines {
		for j := range raw {
			raw[j] = byte(rnd.Uint32())
		}
		if i == changed {
			body.WriteString("changed\n")
			continue
		}
		body.WriteString(base64.StdEncoding.EncodeToString(raw))
		body.WriteByte('\n')
	}
	if _, err := fmt.Fprintf(w, "M 100644 inline big/f%02d.txt\ndata %d\n", k, body.Len()); err != nil {
		return err
	}
	if _, err := w.Write(body.Bytes()); err != nil {
		return err
	}
	_, err := fmt.Fprintf(w, "\n")
	return err
}
