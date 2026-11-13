package codescan

import (
	"maps"
	"sync"
)

// MaxReadingBytes bounds the readings one node keeps in memory: 32 MiB, by
// an estimate of what each holds. A repository of ten files with five claims
// each is some twenty kibibytes, so the bound holds over a thousand of them;
// past it the repositories read longest ago are forgotten whole, and read
// once more at their next move.
const MaxReadingBytes = 32 << 20

// reading is what one blob at one path declared, kept so a later tip that
// holds the same blob there reads nothing.
type reading struct {
	blob     string
	parsed   Parsed
	oversize bool
}

// Rough sizes of what a reading holds, for the bound: a claim is two keys, a
// facet and a source, and a reading its path, its blob id and its counts.
const (
	claimBytes   = 384
	readingBytes = 320
)

func (r reading) bytes() int { return readingBytes + claimBytes*len(r.parsed.Claims) }

// repoReadings is one repository's readings, made under one name: a rename
// changes every key and every voice, so they are no reading of the new name.
type repoReadings struct {
	name  string
	files map[string]reading
	bytes int
	used  uint64
}

// readings is every repository's readings on this node, bounded by bytes.
type readings struct {
	mu    sync.Mutex
	max   int
	total int
	clock uint64
	repos map[string]*repoReadings
}

func newReadings(maxBytes int) *readings {
	return &readings{max: maxBytes, repos: map[string]*repoReadings{}}
}

// of is a copy of the readings held for the repository under name; none
// where they were made under another name.
func (r *readings) of(repo, name string) map[string]reading {
	r.mu.Lock()
	defer r.mu.Unlock()
	held, ok := r.repos[repo]
	if !ok || held.name != name {
		return map[string]reading{}
	}
	return maps.Clone(held.files)
}

// keep replaces the repository's readings with files, then forgets the
// repositories used longest ago until the whole is within its bound.
func (r *readings) keep(repo, name string, files map[string]reading) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.forget(repo)
	held := &repoReadings{name: name, files: maps.Clone(files)}
	for _, f := range files {
		held.bytes += f.bytes()
	}
	r.clock++
	held.used = r.clock
	r.repos[repo] = held
	r.total += held.bytes
	for r.total > r.max && len(r.repos) > 0 {
		oldest := ""
		for id, h := range r.repos {
			if oldest == "" || h.used < r.repos[oldest].used {
				oldest = id
			}
		}
		r.forget(oldest)
	}
}

// drop forgets the repository's readings.
func (r *readings) drop(repo string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.forget(repo)
}

func (r *readings) forget(repo string) {
	if held, ok := r.repos[repo]; ok {
		r.total -= held.bytes
		delete(r.repos, repo)
	}
}
