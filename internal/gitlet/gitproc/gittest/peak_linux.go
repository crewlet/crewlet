package gittest

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// peakAnonSample is how often PeakAnon reads its children's memory: a
// millisecond, against git runs of a tenth of a second and more, each of whose
// peak lasts from its last allocation to its exit — so a sample a millisecond
// apart lands inside it.
const peakAnonSample = time.Millisecond

// PeakAnon runs fn and answers the most ANONYMOUS memory (RssAnon, sampled
// from /proc) any child process of this one held while it ran, and true.
// Anonymous pages are what a node's memory must hold: the pack and index pages
// a git maps count toward its resident size, but the kernel reclaims them
// under pressure rather than killing anybody. Every git a pool runs is a child
// of this process, and each maintenance step, clone's pack-objects, fsck and
// compaction build is one process.
//
// ONE SAMPLER for every benchmark a node's memory budget charges by, so their
// figures are read the same way.
func PeakAnon(fn func()) (int64, bool) {
	var (
		mu   sync.Mutex
		peak int64
	)
	stop, done := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(done)
		tick := time.NewTicker(peakAnonSample) //fixedwait:liveness
		defer tick.Stop()
		for {
			n := childrenAnon()
			mu.Lock()
			peak = max(peak, n)
			mu.Unlock()
			select {
			case <-stop:
				return
			case <-tick.C:
			}
		}
	}()
	fn()
	close(stop)
	<-done
	mu.Lock()
	defer mu.Unlock()
	return peak, true
}

// childrenAnon is the largest RssAnon, in bytes, of this process's children
// now: every thread's children file names the processes it forked.
func childrenAnon() int64 {
	files, _ := filepath.Glob("/proc/self/task/*/children")
	var most int64
	for _, f := range files {
		list, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for _, pid := range strings.Fields(string(list)) {
			most = max(most, rssAnon(pid))
		}
	}
	return most
}

// rssAnon is the RssAnon line of /proc/<pid>/status in bytes, 0 for a
// process gone or a line missing.
func rssAnon(pid string) int64 {
	status, err := os.ReadFile(filepath.Join("/proc", pid, "status"))
	if err != nil {
		return 0
	}
	sc := bufio.NewScanner(bytes.NewReader(status))
	for sc.Scan() {
		field, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok || field != "RssAnon" {
			continue
		}
		kb, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimSpace(rest), " kB"), 10, 64)
		if err != nil {
			return 0
		}
		return kb << 10
	}
	return 0
}
