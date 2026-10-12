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
// what each log's stream holds: every read is held until its lookup ceiling
// ends and then comes back as the silence it is, the way
// [jetstream.Queue.DomainStreamCeiling] fails one nobody answered.
//
// Except that every read is LET GO AT ONCE when as many are in flight together
// as there are logs. A sizing that asks them together therefore costs the case
// nothing, and one that asks them in turn waits out every ceiling and is
// caught by how many it ever had in flight — a count, rather than a stopwatch
// a loaded runner could trip.
//
// The rest of [domainHost] is the nil interface it embeds, so a sizing that
// reached for anything else panics rather than meeting a broker nobody
// modelled.
type silentCeilingsHost struct {
	domainHost

	// ceiling is how long one read is held: the lookup ceiling, scaled
	// down from thirty seconds to a span every read of a sizing that asks
	// them together starts inside, however loaded the runner.
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

// StreamBudget answers, with a limit, so the one thing this broker withholds
// is what the logs' streams hold.
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
// the reads costs the boot one lookup ceiling rather than one per log.
//
// # What it was
//
// One read after another on the boot's context, which carries no deadline.
// While each was a single request on nats.go's five-second default, a silent
// broker cost twenty seconds here; once each was asked again for a whole
// lookup ceiling — so that a read the broker dropped is answered rather than
// sized as an absent stream — it cost four ceilings, two minutes over sixty
// requests on either topology, before the provision after it began its own
// sequence ceiling. Nothing bounded the product.
//
// # The staging
//
// Every read silent for its ceiling, and let go early only once all of them
// are in flight together ([silentCeilingsHost]). Asked in turn, the reads only
// ever have one in flight and wait out a ceiling each; asked together, the
// pass ends as soon as the last has started. Either way the sizing must carry
// every log as absent and size it, rather than fail a boot the provision
// after it may still complete.
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
