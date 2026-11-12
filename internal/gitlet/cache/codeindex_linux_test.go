package cache_test

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"unsafe"

	"github.com/crewlet/crewlet/internal/gitlet/cache"
)

// mappingOf is the file the mapping of this process covering addr maps, as
// /proc/self/maps names it, and whether any mapping covers it — "" for an
// anonymous one, which the Go heap's are.
func mappingOf(t *testing.T, addr uintptr) (string, bool) {
	t.Helper()
	f, err := os.Open("/proc/self/maps")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	lines := bufio.NewScanner(f)
	for lines.Scan() {
		// start-end perms offset dev inode [path]
		fields := strings.Fields(lines.Text())
		if len(fields) < 5 {
			continue
		}
		lo, hi, ok := strings.Cut(fields[0], "-")
		if !ok {
			continue
		}
		start, err := strconv.ParseUint(lo, 16, 64)
		if err != nil {
			continue
		}
		end, err := strconv.ParseUint(hi, 16, 64)
		if err != nil {
			continue
		}
		if uint64(addr) < start || uint64(addr) >= end {
			continue
		}
		if len(fields) < 6 {
			return "", true
		}
		return strings.Join(fields[5:], " "), true
	}
	if err := lines.Err(); err != nil {
		t.Fatal(err)
	}
	return "", false
}

// AN INDEX BUILD HOLDS NO SEGMENT IN ITS HEAP. A build reads back every
// segment it writes — to know which blobs it holds, and as both inputs of
// every merge — and reads it through a MAPPING of the file it placed, never
// from the bytes it wrote, so what its heap holds is one builder's postings
// and one merge's lists however large the repository: held as written, a
// first build kept every fresh segment resident at once and buffered each
// merge's whole output beside its inputs. Four files at a segment a file
// write four segments and merge two of them twice; every one is read back
// through a mapping of the very file it is, and the build leaves no mapping
// behind.
//
// Mutation: open a written segment from the file read into the heap
// (codeindex.go's openBuilt, os.ReadFile in place of the mapping) — the
// bytes are no mapping of the file.
func TestAnIndexBuildReadsItsSegmentsThroughMappings(t *testing.T) {
	t.Parallel()
	w := newSyncWorld(t, onMemory)
	repo := w.repo(t, 1)
	w.push(t, repo, "refs/heads/main", fourSegmentFiles())
	var (
		mu     sync.Mutex
		opened int
		wrong  []string
	)
	c := w.open(t, cache.IndexSegmentBytesWith(1), cache.IndexMappedWith(func(path string, data []byte) {
		got, ok := mappingOf(t, uintptr(unsafe.Pointer(unsafe.SliceData(data))))
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			path = resolved
		}
		mu.Lock()
		defer mu.Unlock()
		opened++
		if got != path {
			wrong = append(wrong, fmt.Sprintf("%s read back from %q (mapped %t)", path, got, ok))
		}
	}))
	s := w.sync(t, c, repo)
	before, _ := cache.MappedRuns(c)
	indexOf(t, c, s, cache.IndexBuilt)
	mu.Lock()
	defer mu.Unlock()
	// Four written a file, and the merges of the first two and of that with
	// the third.
	if opened != 6 {
		t.Errorf("the build read back %d segments, want 6: four written and two merged", opened)
	}
	for _, line := range wrong {
		t.Errorf("a segment the build wrote is no mapping of its file: %s", line)
	}
	if after, _ := cache.MappedRuns(c); after != before {
		t.Errorf("the node maps %d files after the build, want the %d before it: the build left mappings", after,
			before)
	}
}
