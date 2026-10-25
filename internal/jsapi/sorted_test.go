package jsapi

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"
)

// EVERY ENTRY THERE THROUGHOUT A PAGED LISTING IS VISITED, AND NONE TWICE —
// whatever is added and removed between its pages.
//
// The leader cuts each page out of a sorted list it builds for that request
// alone, so a removal before a page boundary moves the entry after it back
// across the boundary, into a page already read: offset paging alone — the
// client's own listers', counting what they hold — skips it, and a listing
// of a hundred thousand subjects, the stream names or a stream's consumers
// that skips one names a key, a stream or a mailbox as absent that was there
// all along. Two removals before a boundary is the case that needs the step
// back as well as the overlap, so it is pinned by hand beside the seeds.
func TestEveryEntryThereThroughoutAListingIsVisitedOnce(t *testing.T) {
	t.Parallel()
	t.Run("two_removed_before_a_boundary", func(t *testing.T) {
		t.Parallel()
		list := names(10)
		page := 0
		visited := walkPages(t, 4, func() []string { return list }, func() {
			if page++; page == 1 {
				list = list[2:]
			}
		})
		wantOnce(t, visited, names(10))
	})
	// A QUIET LISTING ASKS EACH PAGE ONCE: the overlap is one entry, and a
	// page that lands on it is never asked again from further back.
	t.Run("quiet", func(t *testing.T) {
		t.Parallel()
		list := names(10)
		var offsets []int
		visited := map[string]int{}
		err := sorted(func(offset int) (map[string]struct{}, int, error) {
			offsets = append(offsets, offset)
			page := map[string]struct{}{}
			for _, name := range list[min(offset, len(list)):min(offset+4, len(list))] {
				page[name] = struct{}{}
			}
			return page, len(list), nil
		}, func(fresh map[string]struct{}) error {
			for name := range fresh {
				visited[name]++
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		wantOnce(t, visited, list)
		if want := []int{0, 3, 6}; !slices.Equal(offsets, want) {
			t.Errorf("a quiet listing asked pages from %v, want %v: one entry of "+
				"overlap a page, and none asked twice", offsets, want)
		}
	})
	t.Run("seeded", func(t *testing.T) {
		t.Parallel()
		for seed := range uint64(2000) {
			rng := rand.New(rand.NewPCG(seed, 0))
			size := 2 + rng.IntN(5)
			list := names(rng.IntN(40))
			original := slices.Clone(list)
			added, churn := 0, 30
			visited := walkPages(t, size, func() []string { return list }, func() {
				// Between pages, remove and add at random — a bounded
				// number of times, since a list that grows faster than a
				// listing reads it has no end to reach.
				for range rng.IntN(3) {
					if churn--; churn < 0 {
						return
					}
					if len(list) > 0 && rng.IntN(2) == 0 {
						i := rng.IntN(len(list))
						list = slices.Delete(list, i, i+1)
						continue
					}
					added++
					list = append(list, fmt.Sprintf("n%03d.%d", rng.IntN(40), added))
					slices.Sort(list)
				}
			})
			// What was there throughout is what was there at the start
			// and is there at the end: nothing is added back under a
			// name it once had.
			var throughout []string
			for _, name := range original {
				if slices.Contains(list, name) {
					throughout = append(throughout, name)
				}
			}
			for name, n := range visited {
				if n > 1 {
					t.Fatalf("seed %d: %q visited %d times", seed, name, n)
				}
			}
			for _, name := range throughout {
				if visited[name] != 1 {
					t.Fatalf("seed %d: %q, there throughout, visited %d times (page size %d)",
						seed, name, visited[name], size)
				}
			}
		}
	})
}

// walkPages runs [sorted] over the list current returns, a page of size
// entries cut from it at each request, calling between after every page it
// answers, and counts how often each entry was visited.
func walkPages(t *testing.T, size int, current func() []string, between func()) map[string]int {
	t.Helper()
	visited := map[string]int{}
	asks := 0
	err := sorted(func(offset int) (map[string]struct{}, int, error) {
		if asks++; asks > 10_000 {
			t.Fatalf("the listing asked %d pages of a list of %d", asks, len(current()))
		}
		list := current()
		page := map[string]struct{}{}
		for _, name := range list[min(offset, len(list)):min(offset+size, len(list))] {
			page[name] = struct{}{}
		}
		total := len(list)
		between()
		return page, total, nil
	}, func(fresh map[string]struct{}) error {
		for name := range fresh {
			visited[name]++
		}
		return nil
	})
	if err != nil {
		t.Fatalf("the listing failed: %v", err)
	}
	return visited
}

// names is n sorted entries.
func names(n int) []string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("n%03d", i)
	}
	return out
}

// wantOnce fails unless visited holds every one of want exactly once and
// nothing else more than once.
func wantOnce(t *testing.T, visited map[string]int, want []string) {
	t.Helper()
	for _, name := range want {
		if visited[name] != 1 {
			t.Errorf("%q visited %d times, want once", name, visited[name])
		}
	}
	for name, n := range visited {
		if n > 1 {
			t.Errorf("%q visited %d times", name, n)
		}
	}
}
