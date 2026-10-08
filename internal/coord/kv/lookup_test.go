package kv

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsprovision"
)

// droppingLookups is a broker that DROPS the first bucket lookup it is sent —
// it never answers it, the way every member but one drops a read of a bucket
// that is still in flight — and answers every later one from the real broker
// beneath it.
type droppingLookups struct {
	jetstream.JetStream
	lookups atomic.Int64
}

func (d *droppingLookups) KeyValue(ctx context.Context, bucket string) (jetstream.KeyValue, error) {
	if d.lookups.Add(1) == 1 {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return d.JetStream.KeyValue(ctx, bucket)
}

// A BUCKET LOOKUP NOBODY ANSWERED IS ASKED AGAIN AFTER A READ'S TERM, NOT A
// WRITE'S.
//
// The broker answers a read when it processes it or never, and a fleet booting
// together drops reads as a matter of course: a bucket a peer has just asked
// for is assigned with no leader yet, and only the member chosen to lead it
// answers for it. A WRITE's reply, by contrast, is held until the object has a
// leader, which is why a create waits [jsprovision.AskTerm] — fifteen seconds
// on a cluster — before asking again. The lookup used to be asked at that same
// term, so every lookup dropped at boot stalled the node for sixteen seconds
// waiting for a reply that did not exist.
//
// CLUSTERED, because that is the topology whose write term is the long one: on
// a solo broker the two terms would not tell the bug from the fix.
func TestADroppedBucketLookupIsAskedAgainAfterAReadsTerm(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	cfg := jetstream.KeyValueConfig{Bucket: "t_dropped", TTL: time.Minute, Replicas: 1}
	if _, err := jsOf(nc).CreateKeyValue(t.Context(), cfg); err != nil {
		t.Fatalf("create the bucket the lookup is to find: %v", err)
	}
	js := &droppingLookups{JetStream: jsOf(nc)}

	started := time.Now()
	bucket, err := openBucket(t.Context(), js, true, cfg)
	elapsed := time.Since(started)

	if err != nil {
		t.Fatalf("a bucket that exists, behind one dropped lookup, was not opened: %v", err)
	}
	if bucket == nil || bucket.Bucket() != cfg.Bucket {
		t.Fatalf("opened %v, want the bucket %q", bucket, cfg.Bucket)
	}
	if got := js.lookups.Load(); got != 2 {
		t.Errorf("the lookup was sent %d times, want 2: the dropped request "+
			"once, and once more to be answered", got)
	}
	if write := jsprovision.AskTerm(true); elapsed >= write {
		t.Errorf("the bucket took %v to open behind one dropped lookup: the "+
			"lookup waited a write's %v term for a reply the broker never "+
			"holds, where a read is asked again after %v", elapsed, write,
			jsprovision.ReadTerm)
	}
}
