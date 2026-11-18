package engine

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/queue/jetstream"
)

// silentCeilingsHost is a broker that answers none of the sizing's reads of
// what each log's stream holds: each read is held for its lookup ceiling and
// then fails as an unanswered one does ([jetstream.Queue.DomainStreamCeiling]),
// unless as many reads as there are logs are in flight together, which lets
// them all go at once. So a sizing that asks in turn is caught by how many it
// ever had in flight — a count, not a stopwatch a loaded runner could trip.
//
// The embedded nil [domainHost] makes any other call panic rather than meet a
// broker nobody modelled.
type silentCeilingsHost struct {
	domainHost

	// ceiling is how long one read is held: the lookup ceiling, scaled down
	// from thirty seconds but long enough that every read of a sizing that
	// asks together starts inside it, however loaded the runner.
	ceiling time.Duration
	// logs is how many reads have to be in flight at once to be let go.
	logs int

	mu       sync.Mutex
	inFlight int
	most     int
	together chan struct{}
	released bool
}

func newSilentCeilingsHost(ceiling time.Duration, logs int) *silentCeilingsHost {
	return &silentCeilingsHost{ceiling: ceiling, logs: logs, together: make(chan struct{})}
}

// StreamBudget answers, so the streams' holdings are all this broker withholds.
func (h *silentCeilingsHost) StreamBudget(context.Context) (jetstream.StorageBudget, error) {
	return jetstream.StorageBudget{Limit: int64(48) << 30, Source: jetstream.BudgetServerStore}, nil
}

func (h *silentCeilingsHost) DomainStreamCeiling(ctx context.Context, stream string) (int64, bool, error) {
	h.mu.Lock()
	h.inFlight++
	h.most = max(h.most, h.inFlight)
	if h.inFlight == h.logs && !h.released {
		h.released = true
		close(h.together)
	}
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		h.inFlight--
		h.mu.Unlock()
	}()

	ceiling := time.NewTimer(h.ceiling)
	defer ceiling.Stop()
	select {
	case <-h.together:
	case <-ceiling.C:
	case <-ctx.Done():
	}
	return 0, false, fmt.Errorf("jetstream: read %q's ceiling: %w", stream, context.DeadlineExceeded)
}

// mostInFlight is how many reads were ever in flight together.
func (h *silentCeilingsHost) mostInFlight() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.most
}

// THE SIZING ASKS EVERY LOG'S STREAM AT ONCE, so a broker that answers none of
// the reads costs the boot one lookup ceiling rather than one per log: the
// boot's context carries no deadline, so nothing else bounds the sum, and the
// provision after the sizing starts its own ceiling only once it ends.
//
// Each read is silent for its ceiling unless all are in flight together
// ([silentCeilingsHost]). Either way the sizing must carry every unread log
// as absent and size it, rather than fail a boot the provision after it may
// still complete.
func TestTheSizingAsksEveryLogsCeilingAtOnce(t *testing.T) {
	t.Parallel()
	logs := len(registeredDomains())
	host := newSilentCeilingsHost(5*time.Second, logs)

	began := time.Now()
	sized, err := sizeCeilings(t.Context(), host, config.Stream{}, int64(64)<<30,
		"/var/lib/crewlet/stream")
	took := time.Since(began)
	if err != nil {
		t.Fatalf("a broker that answered none of the sizing's reads failed the "+
			"sizing: %v — an unread ceiling is carried as an absent stream", err)
	}
	if most := host.mostInFlight(); most != logs {
		t.Errorf("at most %d of the %d logs' reads were in flight together, so a "+
			"broker answering none of them cost the boot %v — a lookup ceiling per "+
			"read in turn, where asked together one ceiling bounds the pass",
			most, logs, took.Round(time.Millisecond))
	}
	if took >= host.ceiling {
		t.Errorf("the sizing took %v against a broker that answered none of its "+
			"reads, past the %v one read is held for", took.Round(time.Millisecond),
			host.ceiling)
	}
	for _, domain := range registeredDomains() {
		if ceiling := sized[domain.Name()]; ceiling.Bytes <= 0 {
			t.Errorf("%s's log was sized at %d bytes after its read went unanswered, "+
				"want the ceiling an absent stream is created with",
				domain.Name(), ceiling.Bytes)
		}
	}
}
