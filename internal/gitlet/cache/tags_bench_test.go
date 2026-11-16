package cache_test

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/gitlet/cache"
	"github.com/crewlet/crewlet/internal/gitlet/gitproc/gittest"
)

// namedTagStream is n annotated tags, each dated a second after the one
// before, over commits each tagged perCommit times, every tag's name padded to
// 245 bytes where long.
func namedTagStream(n, perCommit int, long bool) func(*bufio.Writer) error {
	return func(w *bufio.Writer) error {
		for i := range n {
			if i%perCommit == 0 {
				msg := fmt.Sprintf("release %d\n", i)
				fmt.Fprintf(w, "commit refs/heads/main\nmark :%d\ncommitter %s %d +0000\ndata %d\n%s", i/perCommit+1,
					benchIdent, benchEpoch+i, len(msg), msg)
				fmt.Fprintf(w, "M 100644 inline t/f%06d\ndata %d\n%s\n", i, len(msg), msg)
			}
			name := fmt.Sprintf("t%06d", i)
			if long {
				name += "-" + strings.Repeat("x", 245-len(name)-1)
			}
			msg := fmt.Sprintf("tag %d\n", i)
			fmt.Fprintf(w, "tag %s\nfrom :%d\ntagger %s %d +0000\ndata %d\n%s\n", name, i/perCommit+1, benchIdent,
				benchEpoch+i, len(msg), msg)
		}
		return nil
	}
}

// BenchmarkTagIndex runs the step's code over n annotated tags: step=whole,
// one (a merge) and level (a boot's check), read=page and marks (100 commits),
// names=long every name at 245 bytes.
func BenchmarkTagIndex(b *testing.B) {
	quiet(b)
	w := newSyncWorld(b, onMemory)
	c := w.open(b)
	for _, n := range []int{1_000, 10_000, 100_000} {
		for _, long := range []bool{false, true} {
			names := "short"
			if long {
				names = "long"
			}
			b.Run(fmt.Sprintf("tags=%d/names=%s", n, names), func(b *testing.B) {
				if n > 10_000 {
					gittest.Large(b, "100 000 annotated tags")
				}
				dir := gittest.Empty(b)
				gittest.FastImport(b, w.client, dir, namedTagStream(n, max(1, n/1_000), long))
				var refs []gitlet.RefRow
				commits := map[string]string{}
				for line := range strings.Lines(w.client.Run(b, dir, "for-each-ref",
					"--format=%(refname) %(objectname) %(*objectname)", "refs/tags/")) {
					f := strings.Fields(line)
					refs = append(refs, gitlet.RefRow{Ref: f[0], OID: f[1]})
					commits[f[0]] = f[2]
				}
				t := cache.NewTagBench(c, dir)
				// The round's peel is its own cost (BenchmarkPeel), read once
				// here: each arm times the step alone.
				whole, err := t.Peel(b.Context(), refs, true)
				if err != nil {
					b.Fatal(err)
				}
				if err = t.StepPeeled(b.Context(), whole); err != nil {
					b.Fatalf("the first build: %v", err)
				}
				index := filepath.Join(dir, cache.TagsRel)
				b.Run("step=whole", func(b *testing.B) {
					for b.Loop() {
						b.StopTimer()
						if err := os.Remove(index); err != nil {
							b.Fatal(err)
						}
						b.StartTimer()
						if err := t.StepPeeled(b.Context(), whole); err != nil {
							b.Fatal(err)
						}
					}
				})
				b.Run("step=one", func(b *testing.B) {
					// The first tag moved, lightweight, to the second's commit and back.
					first := refs[0].Ref
					var moves [2]cache.BenchPeel
					for i, to := range []string{commits[refs[1].Ref], commits[refs[0].Ref]} {
						if moves[i], err = t.Peel(b.Context(), []gitlet.RefRow{{Ref: first, OID: to}}, false); err != nil {
							b.Fatal(err)
						}
					}
					i := 0
					for b.Loop() {
						if err := t.StepPeeled(b.Context(), moves[i%2]); err != nil {
							b.Fatal(err)
						}
						i++
					}
				})
				if err = t.StepPeeled(b.Context(), whole); err != nil {
					b.Fatal(err)
				}
				b.Run("step=level", func(b *testing.B) {
					for b.Loop() {
						if err := t.StepPeeled(b.Context(), whole); err != nil {
							b.Fatal(err)
						}
					}
				})
				x, err := t.Open()
				if err != nil {
					b.Fatal(err)
				}
				defer func() { _ = x.Close() }()
				mid := cache.TagKeyOf(int64(benchEpoch+n/2), strings.TrimPrefix(refs[n/2].Ref, "refs/tags/"))
				b.Run("read=page", func(b *testing.B) {
					for b.Loop() {
						rows := 0
						if err := x.Rows(mid, func(cache.TagEntry) bool { rows++; return rows < 25 }); err != nil ||
							rows != 25 {
							b.Fatalf("a page of %d rows: %v", rows, err)
						}
					}
				})
				b.Run("read=marks", func(b *testing.B) {
					for b.Loop() {
						for i := range 100 {
							got, _, err := x.Commit(commits[refs[i*n/100].Ref], gitlet.MaxCommitTags)
							if err != nil || len(got) == 0 {
								b.Fatalf("the tags of a commit: %v %v", got, err)
							}
						}
					}
				})
			})
		}
	}
}
