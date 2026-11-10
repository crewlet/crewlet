package ledgerstore

import (
	"context"
	"sync"

	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/statelog"
)

// SeenThrough is how far a seat's completed turns read each conversation
// (ADR-0120), moved forward only.
type SeenThrough struct{ marks coord.SeenMarks }

// NewSeenThrough keeps the marks on marks.
func NewSeenThrough(marks coord.SeenMarks) *SeenThrough { return &SeenThrough{marks: marks} }

// NewMemorySeenThrough is the in-process twin.
func NewMemorySeenThrough() *SeenThrough {
	return &SeenThrough{marks: &memorySeen{rows: map[string]seen{}}}
}

// Mark is the position handle's turns read conversation through, zero where none.
// FAILS OPEN: an unreadable mark is none, so nothing is absorbed.
func (s *SeenThrough) Mark(ctx context.Context, handle, conversation string) statelog.Position {
	raw, _, err := s.marks.SeenThrough(ctx, handle, conversation)
	if err != nil {
		log.WarnContext(ctx, "seen_through_unreadable", "seat", handle, "error", err,
			"detail", "absorbing nothing, so a wake already read may start a turn")
		return statelog.Position{}
	}
	return parsedMark(raw)
}

// Advance moves the mark to at where at is past it; a mark another node moved
// further is kept. Best effort, as the completion ledger's writes are.
func (s *SeenThrough) Advance(ctx context.Context, handle, conversation string, at statelog.Position) error {
	if handle == "" || conversation == "" || at.Valid() != nil {
		return nil
	}
	for range 3 {
		raw, revision, err := s.marks.SeenThrough(ctx, handle, conversation)
		if err != nil {
			return err
		}
		if !Past(at, parsedMark(raw)) {
			return nil
		}
		moved, err := s.marks.MarkSeenThrough(ctx, handle, conversation, at.String(), revision)
		if err != nil || moved {
			return err
		}
	}
	return nil
}

// Past reports whether at lies after mark: a later record on the same stream
// and generation, or a mark at, or a later generation of, another log.
func Past(at, mark statelog.Position) bool {
	switch {
	case mark.IsZero() || mark.Stream != at.Stream:
		return true
	case at.Generation != mark.Generation:
		return at.Generation > mark.Generation
	}
	return at.Seq > mark.Seq
}

// parsedMark is a stored mark, zero where it does not parse.
func parsedMark(raw string) statelog.Position {
	if raw == "" {
		return statelog.Position{}
	}
	at, err := statelog.ParsePosition(raw)
	if err != nil {
		return statelog.Position{}
	}
	return at
}

type seen struct {
	mark     string
	revision uint64
}

// memorySeen holds the twin's marks.
type memorySeen struct {
	mu       sync.Mutex
	rows     map[string]seen
	revision uint64
}

func (m *memorySeen) SeenThrough(_ context.Context, scope, conversation string) (string, uint64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row := m.rows[scope+"\x00"+conversation]
	return row.mark, row.revision, nil
}

func (m *memorySeen) MarkSeenThrough(_ context.Context, scope, conversation, mark string,
	revision uint64) (bool, error) {

	m.mu.Lock()
	defer m.mu.Unlock()
	key := scope + "\x00" + conversation
	if m.rows[key].revision != revision {
		return false, nil
	}
	m.revision++
	m.rows[key] = seen{mark: mark, revision: m.revision}
	return true, nil
}
