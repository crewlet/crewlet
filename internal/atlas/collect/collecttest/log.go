// Package collecttest holds what the tests of the atlas's readers share: the
// atlas log as a reader writes it, kept in memory. One copy, so a vendor's
// test and the reader's own judge a record the same way.
package collecttest

import (
	"context"
	"sort"
	"sync"

	"github.com/crewlet/crewlet/internal/atlas"
	"github.com/crewlet/crewlet/internal/atlasdomain"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/statelog"
)

// Log is the atlas log as a reader writes it ([collect.Log]): every voice's
// live claims, and every read recorded. A claim it is handed is held to the
// vocabulary's own validation, as the domain holds it.
type Log struct {
	mu   sync.Mutex
	live map[atlas.Source]map[atlas.Ref]map[string]atlas.Claim

	// Reads is every read recorded, in order, and Callers who made each.
	Reads   []atlasdomain.ReadInput
	Callers []iam.Actor
	// Purged is the entries a read of which answers [atlasdomain.ErrPurged].
	Purged map[atlas.Ref]bool
	// Refuse, where set, is what every read answers.
	Refuse error
}

// NewLog is an empty log.
func NewLog() *Log {
	return &Log{live: map[atlas.Source]map[atlas.Ref]map[string]atlas.Claim{}, Purged: map[atlas.Ref]bool{}}
}

// LiveOf is every live claim of one voice.
func (l *Log) LiveOf(_ context.Context, src atlas.Source, _ statelog.Freshness) (atlasdomain.Live, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := atlasdomain.Live{Complete: true}
	for _, slots := range l.live[src] {
		for _, c := range slots {
			out.Claims = append(out.Claims, c)
		}
	}
	return out, nil
}

// Read records one entry's read in a voice.
func (l *Log) Read(_ context.Context, c atlasdomain.Call, in atlasdomain.ReadInput) (statelog.Result, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case l.Refuse != nil:
		return statelog.Result{}, l.Refuse
	case l.Purged[in.Entry]:
		return statelog.Result{}, atlasdomain.ErrPurged
	}
	l.Callers = append(l.Callers, c.By)
	l.Reads = append(l.Reads, in)
	voice := l.live[in.Source]
	if voice == nil {
		voice = map[atlas.Ref]map[string]atlas.Claim{}
		l.live[in.Source] = voice
	}
	slots := voice[in.Entry]
	if slots == nil {
		slots = map[string]atlas.Claim{}
		voice[in.Entry] = slots
	}
	for _, claim := range in.Claims {
		claim.Source, claim.ObservedAt = in.Source, in.ObservedAt
		if err := claim.Validate(); err != nil {
			return statelog.Result{}, err
		}
		slots[claim.Slot()] = claim
	}
	for _, slot := range in.Withdrawn {
		delete(slots, slot)
	}
	if len(slots) == 0 {
		delete(voice, in.Entry)
	}
	return statelog.Result{}, nil
}

// Entries is the entries a voice holds, as text, in order.
func (l *Log) Entries(src atlas.Source) []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	var out []string
	for r := range l.live[src] {
		out = append(out, r.String())
	}
	sort.Strings(out)
	return out
}

// Written is how many reads the log has recorded.
func (l *Log) Written() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.Reads)
}
