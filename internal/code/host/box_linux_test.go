//go:build linux

package host_test

import (
	"bufio"
	"crypto/sha256"
	"fmt"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/code"
	"github.com/crewlet/crewlet/internal/code/codetest"
	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/code/host"
)

// A BOX PUSH'S COMMITS ARE READ ONE AT A TIME: no git process the landing of
// a coding box's push runs over its pack holds the pack's commits at once.
// What a box sends is a few bytes of pack for every megabyte of commit text,
// and its committers are read before anything is stored, so a scan that held
// every commit it was handed would let a pack of a megabyte hold gigabytes of
// the git node's memory. The box sends sixteen commits of 8 MiB messages —
// each a line of its own repeated, so nothing deltas against anything and the
// pack stays small — on 8 192 small ones, so a listing that prints only once
// it has read every commit still writes more than its pipe holds, and is alive
// when its output is first read. Every read the landing runs that writes
// output holds under half the 128 MiB of commit text at its peak — on the
// node pin the listing of what the push introduces held 23.6 MiB, and on git
// 2.53.0 the committers' cat-file 14.8 MB — and the push lands.
//
// Linux alone: the peak is read from /proc.
//
// Mutation: read the committers with `rev-list --no-walk=unsorted --stdin
// --no-commit-header --format=%H%x00%ce`, as the scan first did — it holds
// every commit's text before it prints one, 138.9 MiB on the node pin, red.
func TestABoxPushsCommitsAreReadOneAtATime(t *testing.T) {
	t.Parallel()
	c, head := boxCase(t, codetest.SoloOptions{})
	peaks := host.PeakGit(c.Fleet.Nodes()[0].Host())
	_, box := c.LaunchBox(codetest.FixtureAlice, code.AccessWrite)
	clone := box.Clone(c.BoxURL(0))
	const big, bigBytes, small = 16, 8 << 20, 8192
	gittest.FastImport(t, box.Git(), clone, func(w *bufio.Writer) error {
		for i := range big + small {
			msg := fmt.Sprintf("commit %d\n", i)
			if i < big {
				line := fmt.Sprintf("%x\n", sha256.Sum256([]byte(msg)))
				msg = strings.Repeat(line, bigBytes/len(line))
			}
			if _, err := fmt.Fprintf(w, "commit refs/heads/work\ncommitter A Box <box@example.com> %d +0000\n"+
				"data %d\n%s\n", 1767225600+i, len(msg), msg); err != nil {
				return err
			}
			if i == 0 {
				if _, err := fmt.Fprintf(w, "from %s\n", head); err != nil {
					return err
				}
			}
		}
		return nil
	})
	branch := code.SeatNamespace(codetest.FixtureAlice) + "work"
	from := len(peaks())

	if res := box.Push(c.Ctx, clone, c.BoxURL(0), "refs/heads/work:"+branch); !res.OK(branch) {
		t.Fatalf("a box's push of %d commits, %d of them of %d MiB messages: %s", big+small, big, bigBytes>>20,
			res)
	}
	const bound = big * bigBytes / 2
	var found int
	for _, p := range peaks()[from:] {
		switch {
		case !p.Found && p.Wrote > 256<<10:
			// More than the pipe and the pool's read of it hold: the
			// process was alive at its first output, and was not found.
			t.Errorf("git %s wrote %d bytes and was never found while they were read: it was not measured",
				strings.Join(p.Args, " "), p.Wrote)
			continue
		case !p.Found:
			continue
		}
		found++
		t.Logf("git %s wrote %d bytes and held %.1f MiB at most", strings.Join(p.Args, " "), p.Wrote,
			float64(p.Peak)/(1<<20))
		if p.Peak > bound {
			t.Errorf("git %s held %.1f MiB at once, past half the push's %d MiB of commit text: it "+
				"holds the pack's commits together, not one at a time", strings.Join(p.Args, " "),
				float64(p.Peak)/(1<<20), big*bigBytes>>20)
		}
	}
	if found == 0 {
		t.Fatalf("no read of the box's push was found running while its output was read: nothing was measured")
	}
}
