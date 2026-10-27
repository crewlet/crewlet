package cache

import (
	"cmp"
	"math"
	"math/bits"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// HEAT: what this node served of each repository over the last hour and the
// last day — the git operations clients ran against it here and the bytes git
// sent them — and what its cache spent on its own account over the same two
// windows: the copies it evicted and the repairs it made. The host counts a
// repository's use into it as it answers (internal/code/host); the host's
// status reads it back (host.Server.Status), and the engine answers its
// peers' status scatter with that (topics.CodeStatus).
//
// IT DECIDES NOTHING. The budget evicts by last use, the maintainer indexes by
// what a copy holds, and neither reads it: heat is reported, for a person or
// a granted seat to judge (a hot repository whose copy is not in service here
// is a budget too small for what this node is asked), and the health tools
// set no thresholds of their own over it.
//
// IN MEMORY, AND THIS PROCESS'S ALONE. It answers a question about one node,
// which only that node can answer, so no other node has to agree on it — and
// it is what an instrument is: counted as requests are answered, and lost
// with the process. Persisting it would be a write per fetch to say what the
// last day looked like; a restarted node says instead since when it counts
// ([Status.Since]), so a "day" of ten minutes reads as one. It is not a
// metric because a repository's id is no closed set: an attribute per
// repository would grow the collector's series with every repository the
// company ever made, where this holds what was used within the day and
// reports the HotRepos hottest of it.
//
// TWO WINDOWS OF heatBuckets BUCKETS EACH. A bucket counts one slot of time —
// a heatBuckets-th of its window — and names the slot it counts, so a bucket a
// later slot comes round to starts again from nothing, and a window's sum
// reads only the buckets of its last heatBuckets slots. Slots are counted on
// the clock's monotonic reading from the instant the cache opened, so a wall
// clock NTP steps moves no count from one bucket to another. A repository
// nothing used within the day holds no bucket a sum reads, and is dropped: by
// the first count of each slot of the day's window, or by a read that finds
// it so.
//
// EACH WINDOW'S NOW IS READ UNDER ITS LOCK, AND NEVER RUNS BACK (heatNow): a
// count, a sweep and a read act at the slots the clock reads once they hold
// the lock, or at the latest slots a holder of that lock already acted at
// where the clock reads earlier. So no window is counted, summed or judged at
// a slot older than one it was counted at, and no caller whose reading went
// stale while it waited for the lock judges a repository idle that a later
// caller had just counted — which deleted the repository with every count it
// held, at an hour's turn under contention.

// heatBuckets is how many buckets each of heat's windows is kept in.
//
// TWENTY-FOUR, so a window's sum reaches back between 23/24 of the window and
// all of it — a fetch leaves the hour's count between 57.5 and 60 minutes
// after it, and the day's between 23 and 24 hours — at 1.5 KiB a repository for
// both windows (twenty-four buckets of 32 bytes, twice). One number for both
// keeps one rule a reader can hold: a count is short of its window by at most
// a twenty-fourth of it. Finer buckets would buy an accuracy nobody reading
// "the last hour" needs, paid by every repository used within the day.
const heatBuckets = 24

// The two windows heat is counted over.
//
// THE HOUR is what is hot now: the window a person looking at a busy node is
// asking about. THE DAY is what was hot today, a working day's traffic and a
// nightly job's alike — a CI pipeline that clones a repository once an hour
// shows there and never in a single hour. Both fixed: a window is what makes
// two nodes' counts comparable, and a reader comparing a node's hour with
// another's must be comparing the same span.
const (
	// HeatHour is the shorter window.
	HeatHour = time.Hour
	// HeatDay is the longer window.
	HeatDay = 24 * time.Hour
)

// HotRepos is how many repositories of each window a node's status reports:
// the hottest of the hour and the hottest of the day, at most twice this many
// in one list.
//
// TWENTY, the figure the status was specified with, held to two bounds. What a
// person reads at a glance: a hot list longer than a screen is a table nobody
// reads to its end. And what one node's answer carries to whoever asks: forty
// repositories with both windows' counts and their copies is about sixteen
// kilobytes (some 400 bytes each), far inside a broker message
// (queue.MaxPayloadBytes, 8 MiB), and the twenty a fleet's merged list keeps
// leave it room inside the 64 KiB a seat's tool answer is held to
// (builtin.ToolAnswerBytes). A repository below the twentieth of both windows
// is still reported by id to a status that asks for it ([Heat.Of]).
const HotRepos = 20

// Use is how much of a repository a node served: one request's, as the host
// counts it into the heat ([Heat.Add]), or a window's sum, as the heat
// reports it ([RepoHeat]).
type Use struct {
	// Fetches is git processes that read the repository here. Every clone,
	// fetch, pull and ls-remote — and every fetch a partial clone makes of
	// what it lacks — is one git process, and every one opens with an
	// advertisement of upload-pack; each advertisement served is one fetch,
	// however many negotiation rounds follow it, and whether or not a pack
	// does: a fetch that finds its client up to date costs this node its
	// advertisement and its ls-refs, and is counted.
	Fetches int64 `json:"fetches"`
	// Clones is the fetches whose transfer named wants and no `have`, served
	// to their end: a client offering nothing to subtract is sent each
	// want's whole history, the costliest answer a node gives. That is every
	// fresh clone — full, shallow or partial — and a partial clone's fetch of
	// what it lacks, which asks for its objects the same way (git negotiates
	// nothing for one): a repository read through blobless clones counts a
	// clone for each such fetch, each light, and its bytes out say how light.
	Clones int64 `json:"clones"`
	// Pushes is the pushes this node received for the repository, whatever
	// became of each — applied, refused or failed — since each is work the
	// node did: every push is one record of the node that received it.
	Pushes int64 `json:"pushes"`
	// BytesOut is what git's processes here wrote to their clients for the
	// repository: every advertisement, negotiation, pack and push report.
	BytesOut int64 `json:"bytes_out"`
}

// heat is a window's heat: how many git operations it served — fetches and
// pushes, each a git process this node ran for the repository. What a
// hot list is ordered by; its bytes out break a tie.
func (u Use) heat() int64 { return u.Fetches + u.Pushes }

// zero reports a use that counts nothing.
func (u Use) zero() bool { return u == Use{} }

// served is one bucket of a repository's heat: uint32 counts — past four
// billion in a bucket, over a million a second for a whole hour on one
// repository, a count saturates rather than wraps — so a bucket and the slot
// it names are 32 bytes.
type served struct {
	fetches, clones, pushes uint32
	bytesOut                uint64
}

// plus is s and t counted together.
func (s served) plus(t served) served {
	return served{fetches: add32(s.fetches, t.fetches), clones: add32(s.clones, t.clones),
		pushes: add32(s.pushes, t.pushes), bytesOut: add64(s.bytesOut, t.bytesOut)}
}

// servedOf is u as a bucket counts it: a negative field counts nothing, and
// one past a bucket's range counts as many as a bucket holds.
func servedOf(u Use) served {
	return served{fetches: clamp32(u.Fetches), clones: clamp32(u.Clones), pushes: clamp32(u.Pushes),
		bytesOut: uint64(max(u.BytesOut, 0))}
}

// widen adds what one bucket counted into a window's sum.
func (u *Use) widen(s served) {
	u.Fetches += int64(s.fetches)
	u.Clones += int64(s.clones)
	u.Pushes += int64(s.pushes)
	u.BytesOut = int64(min(add64(uint64(u.BytesOut), s.bytesOut), math.MaxInt64))
}

// churned is one bucket of what the cache spent on its own account: copies
// evicted, and repairs counted — saturating as served's counts do.
type churned struct{ evictions, rebuilds uint32 }

// plus is c and d counted together.
func (c churned) plus(d churned) churned {
	return churned{evictions: add32(c.evictions, d.evictions), rebuilds: add32(c.rebuilds, d.rebuilds)}
}

// Churn is what the cache spent on its own account over a window: copies its
// budget evicted, each a cold sync the next time anybody asks for it, and
// repairs it counted — every count of crewlet.code.cache.rebuilds, whatever
// its reason, an in-place repair or a whole rebuild.
type Churn struct {
	Evictions int64 `json:"evictions"`
	Rebuilds  int64 `json:"rebuilds"`
}

// ring is the slot each of a window's heatBuckets buckets counts, zero for a
// bucket never written: the window's arithmetic, kept once, beside which each
// kind of window keeps its counts (servedWindow, churnWindow). Concrete types
// rather than one generic window: the cache's writers gate types every value
// a call in this package is handed (internal/sourcetree/decltype), and a type
// parameter is a value it cannot type.
type ring [heatBuckets]int64

// claim is the bucket slot is counted in, and whether that bucket last counted
// an older slot — and so starts again from nothing.
func (r *ring) claim(slot int64) (int, bool) {
	i := int(slot % heatBuckets)
	if r[i] == slot {
		return i, false
	}
	r[i] = slot
	return i, true
}

// live reports whether bucket i counts in the span that ends at slot.
func (r *ring) live(i int, slot int64) bool {
	s := r[i]
	return s != 0 && s <= slot && s > slot-heatBuckets
}

// idle reports a window with no bucket in the span that ends at slot.
func (r *ring) idle(slot int64) bool {
	for i := range r {
		if r.live(i, slot) {
			return false
		}
	}
	return true
}

// servedWindow is what a node served of a repository over one window.
type servedWindow struct {
	ring   ring
	counts [heatBuckets]served
}

// add counts s in slot.
func (w *servedWindow) add(slot int64, s served) {
	i, fresh := w.ring.claim(slot)
	if fresh {
		w.counts[i] = served{}
	}
	w.counts[i] = w.counts[i].plus(s)
}

// sum is what the window counts over the span that ends at slot.
func (w *servedWindow) sum(slot int64) Use {
	var u Use
	for i := range w.counts {
		if w.ring.live(i, slot) {
			u.widen(w.counts[i])
		}
	}
	return u
}

// churnWindow is what the cache spent on its own account over one window.
type churnWindow struct {
	ring   ring
	counts [heatBuckets]churned
}

// add counts c in slot.
func (w *churnWindow) add(slot int64, c churned) {
	i, fresh := w.ring.claim(slot)
	if fresh {
		w.counts[i] = churned{}
	}
	w.counts[i] = w.counts[i].plus(c)
}

// sum is what the window counts over the span that ends at slot.
func (w *churnWindow) sum(slot int64) Churn {
	var c Churn
	for i := range w.counts {
		if w.ring.live(i, slot) {
			c.Evictions += int64(w.counts[i].evictions)
			c.Rebuilds += int64(w.counts[i].rebuilds)
		}
	}
	return c
}

// slots is the slot an instant falls in, in each window.
type slots struct{ hour, day int64 }

// heatClock is where heat's slots are counted from: the instant the cache
// opened, on the monotonic reading of its clock where it has one.
type heatClock struct {
	now    func() time.Time
	origin time.Time
}

// newHeatClock is a clock counting from now.
func newHeatClock(now func() time.Time) heatClock { return heatClock{now: now, origin: now()} }

// at is the slots now falls in. Slot zero is no slot — a bucket never written
// names it, and a sum passes it over — so the first slot is one, and an
// instant before the origin (a clock with no monotonic reading, set back)
// falls in it. A window reads it only through its heatNow.
func (k heatClock) at() slots {
	since := max(k.now().Sub(k.origin), 0)
	return slots{hour: int64(since/(HeatHour/heatBuckets)) + 1, day: int64(since/(HeatDay/heatBuckets)) + 1}
}

// heatNow is the now one lock's holder counts, sums and judges its windows
// at: the slots heat's clock reads, or the latest slots a holder of the same
// lock acted at where the clock reads earlier. Each of heat's two structures
// keeps one beside its windows, under the lock they are counted and read
// under (Heat.mu, cacheChurn.mu), and reads it only there.
//
// READ UNDER THE LOCK, so the slots a window is acted at follow the order its
// callers took the lock in. Read before the lock, a caller that read the
// clock just before a slot's turn and took the lock only after another had
// counted a repository in the new slot judged that repository at the older
// slot — nothing in the day's span that ends there — and deleted it with all
// its counts; a count's sweep moved the slot it had swept in back.
//
// NEVER EARLIER THAN THE LATEST, so the same holds whatever the clock reads:
// a clock with no monotonic reading, set back, or a case's own. No bucket is
// ever newer than the slot that sums or judges it, so the span a sum reads
// (ring.live) is the one rule a judgement of idleness needs too, and a sweep
// only ever moves forward.
type heatNow struct {
	clock  heatClock
	latest slots
}

// at is the slots its lock's holder acts at now; the lock is held.
func (n *heatNow) at() slots {
	at := n.clock.at()
	n.latest = slots{hour: max(n.latest.hour, at.hour), day: max(n.latest.day, at.day)}
	return n.latest
}

// RepoHeat is one repository's heat on this node.
type RepoHeat struct {
	// ID is the repository's id: heat is kept by it, so a repository renamed
	// keeps its heat.
	ID string `json:"id"`
	// Name is the name the last request counted for it named it by; "" for a
	// repository nothing used within the day.
	Name string `json:"name,omitempty"`
	// Hour and Day are what the node served of it in each window.
	Hour Use `json:"hour"`
	Day  Use `json:"day"`
}

// Heat is what this node served of each repository, over the last hour and
// the last day. Safe for concurrent use: one lock, held for a reading of the
// clock and a few additions a count, and for one walk of the repositories a
// read.
type Heat struct {
	mu sync.Mutex
	// now is the slots the heat is counted, read and swept at, read under
	// mu (heatNow).
	now   heatNow
	repos map[string]*repoHeat
	// swept is the day's slot the last sweep of repositories nothing used
	// within the day ran in: one sweep a slot of the day's window, an hour,
	// on the first count of the slot, and never one at a slot older than
	// the last.
	swept int64
}

// repoHeat is one repository's buckets, and the name it was last counted by.
type repoHeat struct {
	name      string
	hour, day servedWindow
}

// newHeat is a Heat counting on clock.
func newHeat(clock heatClock) *Heat {
	return &Heat{now: heatNow{clock: clock}, repos: map[string]*repoHeat{}}
}

// Add counts u, one request's use of the repository whose id is repo and
// which the request named name, into its heat now. A use that counts nothing
// adds nothing — no repository is kept for a request that served none of it —
// and neither does a repository with no id.
func (h *Heat) Add(repo, name string, u Use) {
	s := servedOf(u)
	if repo == "" || s == (served{}) {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	at := h.now.at()
	if at.day > h.swept {
		h.sweep(at.day)
		h.swept = at.day
	}
	r := h.repos[repo]
	if r == nil {
		r = &repoHeat{}
		h.repos[repo] = r
	}
	if name != "" {
		r.name = name
	}
	r.hour.add(at.hour, s)
	r.day.add(at.day, s)
}

// sweep drops every repository nothing used within the day's span that ends
// at day; h.mu is held.
func (h *Heat) sweep(day int64) {
	for id, r := range h.repos {
		if r.day.ring.idle(day) {
			delete(h.repos, id)
		}
	}
}

// Hot is the n hottest repositories of the hour and the n hottest of the day,
// in one list of at most 2n: those hottest in the hour first, each window's
// heat its count of git operations, its bytes out breaking a tie and its id
// the next ([Hottest]). A repository with nothing in a window is never one of
// that window's hottest. So a reader may order the list by either window and
// cut it at n, and read that window's n hottest exactly.
func (h *Heat) Hot(n int) []RepoHeat {
	if n <= 0 {
		return nil
	}
	h.mu.Lock()
	at := h.now.at()
	all := make([]RepoHeat, 0, len(h.repos))
	for id, r := range h.repos {
		if r.day.ring.idle(at.day) {
			delete(h.repos, id)
			continue
		}
		all = append(all, r.heat(id, at))
	}
	h.mu.Unlock()
	keep := Hottest(all, n)
	out := make([]RepoHeat, len(keep))
	for i, k := range keep {
		out[i] = all[k]
	}
	return out
}

// Hottest is which of entries a hot list keeps, as their indices in entries,
// in the list's order: the n hottest of the hour and the n hottest of the day,
// at most 2n, those hottest in the hour first — each window's heat its count
// of git operations, its bytes out breaking a tie, then its id, then its place
// in entries. An entry with nothing in a window is never one of that window's
// hottest. So a reader may order the list by either window and cut it at n,
// and read that window's n hottest exactly.
//
// ONE RULE for one node's list ([Heat.Hot]) and for a list several nodes'
// lists make — a fleet's, where one repository stands once for every node that
// served it, which is why the answer is indices rather than ids: two entries
// may name one repository, and the caller knows which node each came from.
func Hottest(entries []RepoHeat, n int) []int {
	if n <= 0 {
		return nil
	}
	order := make([]int, len(entries))
	for i := range order {
		order[i] = i
	}
	hour := func(r RepoHeat) Use { return r.Hour }
	day := func(r RepoHeat) Use { return r.Day }
	keep := make(map[int]bool, 2*n)
	for _, in := range []func(RepoHeat) Use{hour, day} {
		slices.SortFunc(order, func(i, j int) int {
			return cmp.Or(hotter(in(entries[i]), in(entries[j]), entries[i].ID, entries[j].ID),
				cmp.Compare(i, j))
		})
		for _, i := range order[:min(n, len(order))] {
			if !in(entries[i]).zero() {
				keep[i] = true
			}
		}
	}
	out := slices.DeleteFunc(order, func(i int) bool { return !keep[i] })
	slices.SortFunc(out, func(i, j int) int {
		a, b := entries[i], entries[j]
		return cmp.Or(hotter(a.Hour, b.Hour, "", ""), hotter(a.Day, b.Day, a.ID, b.ID), cmp.Compare(i, j))
	})
	return out
}

// hotter orders a's use before b's when it is hotter: more git operations,
// then more bytes out, then the smaller id.
func hotter(a, b Use, aID, bID string) int {
	return cmp.Or(cmp.Compare(b.heat(), a.heat()), cmp.Compare(b.BytesOut, a.BytesOut), strings.Compare(aID, bID))
}

// Of is the heat of the repository whose id is repo: what the node served of
// it in each window, nothing at all where nothing used it within the day.
func (h *Heat) Of(repo string) RepoHeat {
	h.mu.Lock()
	defer h.mu.Unlock()
	at := h.now.at()
	r := h.repos[repo]
	if r == nil || r.day.ring.idle(at.day) {
		return RepoHeat{ID: repo}
	}
	return r.heat(repo, at)
}

// heat is r's two windows summed at at; h.mu is held.
func (r *repoHeat) heat(id string, at slots) RepoHeat {
	return RepoHeat{ID: id, Name: r.name, Hour: r.hour.sum(at.hour), Day: r.day.sum(at.day)}
}

// cacheChurn is what the cache spent on its own account, over both windows.
type cacheChurn struct {
	mu sync.Mutex
	// now is the slots the windows are counted and summed at, read under mu
	// (heatNow): a status that takes the lock after an eviction was counted
	// reads that eviction, whichever of the two read the clock first.
	now       heatNow
	hour, day churnWindow
}

// churned counts d into what the cache spent, now.
func (c *Cache) churned(d churned) {
	c.churn.mu.Lock()
	defer c.churn.mu.Unlock()
	at := c.churn.now.at()
	c.churn.hour.add(at.hour, d)
	c.churn.day.add(at.day, d)
}

// spent is what the cache spent over each window.
func (c *Cache) spent() (hour, day Churn) {
	c.churn.mu.Lock()
	defer c.churn.mu.Unlock()
	at := c.churn.now.at()
	return c.churn.hour.sum(at.hour), c.churn.day.sum(at.day)
}

// Heat is what this node served of each repository, which the host counts
// into as it answers.
func (c *Cache) Heat() *Heat { return c.heat }

// Status is this node's cache as it stands: what it holds against its budget,
// how many repositories it holds a copy of, and what it spent on its own
// account in each window.
type Status struct {
	// Since is when the cache opened, on its clock: both windows count from
	// it, so a node that started ten minutes ago reports a day of ten
	// minutes, and its heat likewise.
	Since time.Time `json:"since"`
	// Bytes is what the cache holds against its budget: every copy, current
	// and retiring, the downloads in flight and the pushes' scratch
	// repositories.
	Bytes int64 `json:"bytes"`
	// Budget is what it is held to: git.cache_max_bytes, as the node resolved
	// it.
	Budget int64 `json:"budget"`
	// Warm is how many repositories it holds a copy of in service — each
	// answered from the disk at its next request, rather than downloaded.
	Warm int64 `json:"warm"`
	// Hour and Day are what it spent on its own account in each window.
	Hour Churn `json:"hour"`
	Day  Churn `json:"day"`
}

// Status is the cache as it stands now: running totals and the two windows,
// read with no walk of the repositories and no disk.
func (c *Cache) Status() Status {
	hour, day := c.spent()
	return Status{Since: c.clock.origin.UTC(), Bytes: c.total(), Budget: c.maxBytes, Warm: c.heldRepos.Load(),
		Hour: hour, Day: day}
}

// Copy is a repository's copy in service on this node — the incarnation its
// readers are handed — as the cache holds it now.
type Copy struct {
	// Incarnation is the copy's number (repos/<id>.<n>.git): every rebuild
	// moves it on, so a number that climbs between two looks is a copy being
	// rebuilt.
	Incarnation int `json:"incarnation"`
	// Position is how far into the code log the copy holds every row.
	Position statelog.Position `json:"position"`
	// Refs and Packs are what it holds as its rows count them.
	Refs  int `json:"refs"`
	Packs int `json:"packs"`
	// Bytes is what it holds on disk: its packs, its refs and its derived
	// files.
	Bytes int64 `json:"bytes"`
	// LastUsed is when a reader last took or let go of it: what the budget
	// evicts by, least recent first.
	LastUsed time.Time `json:"last_used"`
	// Readers is the requests and runs holding it now.
	Readers int `json:"readers"`
	// Retiring is the copies a rebuild replaced that a reader still holds,
	// each removed with its last reader.
	Retiring int `json:"retiring"`
}

// Copy is the copy in service of the repository whose id is repo, and false
// where this node holds none: never synced here, evicted, purged, or between
// a rebuild's retiring of one copy and its promotion of the next.
func (c *Cache) Copy(repo string) (Copy, bool) {
	c.mu.Lock()
	e := c.entries[repo]
	c.mu.Unlock()
	if e == nil {
		return Copy{}, false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	inc := e.current
	if e.dead || inc == nil {
		return Copy{}, false
	}
	return Copy{Incarnation: inc.n, Position: inc.at, Refs: inc.refs, Packs: inc.packs,
		Bytes: inc.diskBytes + inc.metaBytes, LastUsed: inc.lastUsed.UTC(), Readers: inc.pins,
		Retiring: len(e.retiring)}, true
}

// add32 is a+b, saturating at a bucket's range.
func add32(a, b uint32) uint32 {
	s, carry := bits.Add32(a, b, 0)
	if carry != 0 {
		return math.MaxUint32
	}
	return s
}

// add64 is a+b, saturating at a bucket's range.
func add64(a, b uint64) uint64 {
	s, carry := bits.Add64(a, b, 0)
	if carry != 0 {
		return math.MaxUint64
	}
	return s
}

// clamp32 is n as a bucket counts it: nothing for a negative n, and a
// bucket's range for one past it.
func clamp32(n int64) uint32 { return uint32(min(max(n, 0), math.MaxUint32)) }
