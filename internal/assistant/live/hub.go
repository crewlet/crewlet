package live

import (
	"sync"
	"time"

	"github.com/google/uuid"
)

// Hub is every frame log this node keeps: one per turn running here, and for
// [EndedRetention] after each segment ends.
//
// A TURN'S NUMBERING OUTLIVES ITS FRAMES. A segment that parks on a proposal
// ends its log, and the approval that resumes it may come minutes later, when
// the frames are long gone; the frames after it still continue the numbering
// a client drew (the wire's seq rule), so the hub remembers the last seq of
// every turn it has numbered, for as long as a turn may wait, and a resumed
// segment's log starts after it.
type Hub struct {
	now func() time.Time
	// remember is how long a turn's last seq is kept after its log is
	// dropped: the longest a parked turn may wait on an approval.
	remember time.Duration

	mu   sync.Mutex
	logs map[uuid.UUID]*Log
	last map[uuid.UUID]lastSeq
}

// lastSeq is the newest seq a dropped log wrote, and when it was dropped.
type lastSeq struct {
	seq uint64
	at  time.Time
}

// NewHub builds the node's hub. remember is how long a parked turn may wait
// (the company's longest proposal lifetime): its numbering is kept that long.
func NewHub(now func() time.Time, remember time.Duration) *Hub {
	if now == nil {
		now = time.Now
	}
	return &Hub{now: now, remember: remember, logs: map[uuid.UUID]*Log{},
		last: map[uuid.UUID]lastSeq{}}
}

// Open starts the log of a turn segment: a new turn's from seq 1, and a
// resumed turn's after the last seq this node numbered for it. A log still
// open for the turn is returned as it is.
func (h *Hub) Open(conversation, turn uuid.UUID) *Log {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sweepLocked()
	if l, ok := h.logs[turn]; ok {
		if !l.Ended() {
			return l
		}
		h.last[turn] = lastSeq{seq: l.Last(), at: h.now()}
		delete(h.logs, turn)
	}
	first := uint64(1)
	if prior, ok := h.last[turn]; ok {
		first = prior.seq + 1
	}
	l := newLog(conversation, turn, first, h.now)
	h.logs[turn] = l
	return l
}

// Get is the log this node holds for a turn, running or recently ended.
func (h *Hub) Get(turn uuid.UUID) (*Log, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.sweepLocked()
	l, ok := h.logs[turn]
	return l, ok
}

// Forget drops everything the hub holds for a turn: a conversation purged.
func (h *Hub) Forget(turn uuid.UUID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if l, ok := h.logs[turn]; ok {
		l.End()
		delete(h.logs, turn)
	}
	delete(h.last, turn)
}

// sweepLocked drops logs past their retention, keeping their last seq, and
// numbering past its own.
func (h *Hub) sweepLocked() {
	now := h.now()
	for turn, l := range h.logs {
		if l.expired(now) {
			h.last[turn] = lastSeq{seq: l.Last(), at: now}
			delete(h.logs, turn)
		}
	}
	for turn, seq := range h.last {
		if now.Sub(seq.at) > h.remember {
			delete(h.last, turn)
		}
	}
}
