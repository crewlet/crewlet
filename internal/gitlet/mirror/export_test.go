package mirror

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"time"

	"github.com/crewlet/crewlet/internal/gitlet"
	"github.com/crewlet/crewlet/internal/statelog"
)

// Wait waits for every attempt this worker started: a case's way of reading
// the rows an attempt left once it has ended.
func (m *Mirrors) Wait() { m.wg.Wait() }

// Steps is what [steps] lands of outs and cmds where each record takes take of
// the commands offered to it, as an attempt's land takes what its record
// carries: each record's outputs and commands, in its order. A record that
// would take nothing and land no output ends the walk, as land's refusal
// ends an attempt.
func Steps(outs []string, cmds []gitlet.Command, take func(outs []string, offered []gitlet.Command) int) (
	packs [][]string, refs [][]gitlet.Command) {

	s := &steps{outs: outs, cmds: cmds}
	for !s.done() {
		o, offered := s.next()
		n := take(o, offered)
		if n == 0 && len(o) == 0 {
			return packs, refs
		}
		packs, refs = append(packs, o), append(refs, offered[:n])
		s.landed(len(o), n)
	}
	return packs, refs
}

// CutOfWhatTheCopyHolds is what [Mirrors.cut] answers of a fetch that brought
// only objects repoID's copy holds already — the restart after a chunk of
// packs alone, whose fetch asks again for everything those packs hold, since
// no ref of the copy reaches them: a scratch over the copy synced at fresh,
// holding a link of every live pack of it, cut as an attempt cuts.
func (m *Mirrors) CutOfWhatTheCopyHolds(ctx context.Context, repoID string, fresh statelog.Freshness) ([]string,
	error) {

	s, err := m.o.Cache.Sync(ctx, repoID, fresh)
	if err != nil {
		return nil, err
	}
	defer s.Release()
	sc, err := m.o.Cache.NewScratch(ctx, s, statelog.NewOpID(m.now(), ""))
	if err != nil {
		return nil, err
	}
	defer sc.Remove()
	held, err := m.o.Cache.Packs(s)
	if err != nil {
		return nil, err
	}
	if len(held) == 0 {
		return nil, fmt.Errorf("the copy of %s holds no pack to fetch again", repoID)
	}
	for _, p := range held {
		for _, ext := range []string{".pack", ".idx"} {
			name := "pack-" + p.GitChecksum + ext
			if err := os.Link(filepath.Join(sc.Alternate, "objects", "pack", name),
				filepath.Join(sc.Dir, "objects", "pack", name)); err != nil {
				return nil, err
			}
		}
	}
	fetched, err := sc.Packs()
	if err != nil {
		return nil, err
	}
	var bytes int64
	for _, p := range fetched {
		bytes += fileSize(p.Path)
	}
	return m.cut(ctx, s, sc, fetched, bytes, m.o.Limits())
}

// StartOrder is the order [Mirrors.startQueued] starts the attempts queued
// while MaxAttempts ran: one import of each repository in queued, queued in
// that order, the last attempt of each in ended having ended then (none
// attempted, where it names none).
func StartOrder(queued []string, ended map[string]time.Time) []string {
	m := &Mirrors{memory: map[mirrorKey]memory{}, queued: map[mirrorKey]queuedAttempt{}}
	for _, repo := range queued {
		key := mirrorKey{repo: repo}
		if at, ok := ended[repo]; ok {
			m.memory[key] = memory{ended: at}
		}
		m.arrivals++
		m.queued[key] = queuedAttempt{arrived: m.arrivals}
	}
	var out []string
	for {
		key, _, ok := m.nextQueued()
		if !ok {
			return out
		}
		delete(m.queued, key)
		out = append(out, key.repo)
	}
}

// FailingPastThePromise is what [Mirrors.AlarmReading] counts a second after
// the last of turns, where MaxAttempts push mirrors' attempts started at the
// first of them and run through every one — each of their repositories
// worked at every turn, as the listing names them — and a pull mirror is
// listed and worked at each turn while they hold every slot.
func FailingPastThePromise(turns []time.Time) int {
	now := turns[0]
	m := &Mirrors{now: func() time.Time { return now }, known: map[mirrorKey]listed{},
		running: map[mirrorKey]bool{}, memory: map[mirrorKey]memory{}, health: map[mirrorKey]health{},
		queued: map[mirrorKey]queuedAttempt{}}
	var repos []string
	for i := range MaxAttempts {
		key := mirrorKey{repo: fmt.Sprintf("busy-%d", i), out: true}
		l := listed{kind: kindPush, name: key.repo}
		m.known[key], m.running[key] = l, true
		m.claim(key, l, now)
		repos = append(repos, key.repo)
	}
	late := mirrorKey{repo: "late"}
	for _, at := range turns {
		now = at
		m.known[late] = listed{kind: kindSync, name: late.repo}
		for _, repo := range append(slices.Clone(repos), late.repo) {
			if err := m.Work(context.Background(), repo, statelog.Position{}); err != nil {
				panic(err)
			}
		}
	}
	now = now.Add(time.Second)
	var r statelog.Reading
	m.AlarmReading(&r)
	return r.CodeMirrorFailing
}

// RefsOf is the branches and tags of the repository at dir as every attempt
// lists a copy's or a scratch's ([Mirrors.refs]).
func (m *Mirrors) RefsOf(ctx context.Context, dir string) (map[string]string, error) {
	return m.refs(ctx, dir, "")
}

// Remembered is how many entries the worker holds of the repositories its
// listings named: what they said, its attempts' memory, its health, what its
// pushes left level and its queued attempts, together.
func (m *Mirrors) Remembered() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.known) + len(m.memory) + len(m.health) + len(m.levels) + len(m.queued)
}

// FailingWhileQueued is what [Mirrors.AlarmReading] counts a second after the
// last of turns, where a pull mirror is listed and worked at each of them
// while MaxAttempts other attempts hold every slot, so that it is queued at
// every one.
func FailingWhileQueued(turns []time.Time) int {
	var now time.Time
	m := &Mirrors{now: func() time.Time { return now }, known: map[mirrorKey]listed{},
		running: map[mirrorKey]bool{}, memory: map[mirrorKey]memory{}, health: map[mirrorKey]health{},
		queued: map[mirrorKey]queuedAttempt{}}
	for i := range MaxAttempts {
		m.running[mirrorKey{repo: fmt.Sprintf("busy-%d", i)}] = true
	}
	key := mirrorKey{repo: "late"}
	for _, at := range turns {
		now = at
		m.known[key] = listed{kind: kindSync, name: "late"}
		if err := m.Work(context.Background(), key.repo, statelog.Position{}); err != nil {
			panic(err)
		}
	}
	now = now.Add(time.Second)
	var r statelog.Reading
	m.AlarmReading(&r)
	return r.CodeMirrorFailing
}
