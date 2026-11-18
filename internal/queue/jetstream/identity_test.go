package jetstream

import (
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// A STREAM'S IDENTITY GOES WHERE ITS CONFIGURATION GOES.
//
// A snapshot restore makes a NEW stream dated to the restore, so the broker's
// creation instant would read every restored log as recreated. The create
// stamps the instant into the configuration instead: a stream made again from
// its own configuration, as a restore makes it, answers its first instant,
// while one the queue provisions afresh after it was lost answers a new one —
// the recreation the identity exists to detect.
func TestAStreamsIdentityGoesWhereItsConfigurationGoes(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	spec := probeDomain()
	q := domainQueue(t, spec)
	log, err := q.DomainLog(ctx, spec.Name)
	if err != nil {
		t.Fatalf("open the log: %v", err)
	}
	identity := func() time.Time {
		t.Helper()
		stats, err := log.Stats(ctx)
		if err != nil {
			t.Fatalf("read the log's state: %v", err)
		}
		return stats.CreatedAt
	}

	made, err := q.js.Stream(ctx, spec.Name)
	if err != nil {
		t.Fatalf("read the stream the queue made: %v", err)
	}
	original := made.CachedInfo()
	stamp := original.Config.Metadata[statelog.CreatedAtKey]
	if stamp == "" {
		t.Fatalf("the queue made %s with no %s in its configuration — its identity "+
			"is then the broker's instant, which a restore does not keep",
			spec.Name, statelog.CreatedAtKey)
	}
	first := identity()
	if first.Format(time.RFC3339Nano) != stamp {
		t.Fatalf("the log's identity is %s and its stamp %s", first.Format(time.RFC3339Nano), stamp)
	}

	// MADE AGAIN FROM ITS OWN CONFIGURATION, as a restore's request makes it.
	if err := q.js.DeleteStream(ctx, spec.Name); err != nil {
		t.Fatalf("delete the stream: %v", err)
	}
	if _, err := q.js.CreateStream(ctx, original.Config); err != nil {
		t.Fatalf("make the stream again from its configuration: %v", err)
	}
	again, err := q.js.Stream(ctx, spec.Name)
	if err != nil {
		t.Fatalf("read the stream made again: %v", err)
	}
	if statelog.IdentityOf(original.Created, again.CachedInfo().Created, true) != statelog.StreamRecreated {
		t.Fatalf("the broker dated the stream made again %v, the original %v — this "+
			"case proves nothing unless the broker's own instant moved",
			again.CachedInfo().Created, original.Created)
	}
	if got := identity(); statelog.IdentityOf(first, got, true) != statelog.StreamSame {
		t.Fatalf("a stream made again from its own configuration is identified as %v, "+
			"its original as %v — every node restored from a backup would refuse it "+
			"as recreated", got, first)
	}

	// PROVISIONED AFRESH after it was lost: a recreation, and must read as one.
	if err := q.js.DeleteStream(ctx, spec.Name); err != nil {
		t.Fatalf("delete the stream again: %v", err)
	}
	q.mu.Lock()
	delete(q.streams, spec.Name)
	q.mu.Unlock()
	if err := q.EnsureDomainStream(ctx, spec); err != nil {
		t.Fatalf("provision the stream afresh: %v", err)
	}
	if got := identity(); statelog.IdentityOf(first, got, true) != statelog.StreamRecreated {
		t.Fatalf("a stream the queue provisioned afresh is identified as %v, its "+
			"predecessor as %v — an empty stream would pass for the history it lost",
			got, first)
	}
}
