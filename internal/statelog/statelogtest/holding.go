package statelogtest

import (
	"sync"

	"github.com/crewlet/crewlet/internal/statelog"
)

// Holding is a [statelog.Holding] a test moves while its node runs: which
// partitions the node serves, changed the way a join that finished or a leave
// that began changes them, or an answer it cannot give.
//
// THE ONE A TEST HANDS A PUBLISHER WHOSE NODE'S SERVING CHANGES, for the
// reason the write authority asks at all: a write asked of a node that has
// stopped serving its partition is refused, and one already deciding when its
// node stopped is not appended — neither of which a fixed answer
// ([statelog.ServesOnly]) can show.
type Holding struct {
	mu      sync.Mutex
	serving map[statelog.PartitionID]bool
	err     error
	asked   int
}

// NewHolding is a node serving exactly the partitions given.
func NewHolding(serving ...statelog.PartitionID) *Holding {
	h := &Holding{serving: map[statelog.PartitionID]bool{}}
	for _, p := range serving {
		h.serving[p] = true
	}
	return h
}

// Serving answers as [statelog.Holding] does, counting every question.
func (h *Holding) Serving(p statelog.PartitionID) (bool, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.asked++
	if h.err != nil {
		return false, h.err
	}
	return h.serving[p], nil
}

// Serve makes the node serve p: its join has established the partition.
func (h *Holding) Serve(p statelog.PartitionID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.serving[p] = true
}

// Stop makes the node stop serving p: its leave has stopped deciding there.
func (h *Holding) Stop(p statelog.PartitionID) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.serving, p)
}

// Fail makes every answer "cannot tell", carrying err; nil answers again.
func (h *Holding) Fail(err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.err = err
}

// Asked is how many times the node was asked whether it serves a partition.
func (h *Holding) Asked() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.asked
}
