package kv

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsprovision"
)

// droppingLookups is a broker that DROPS (never answers) the first bucket
// lookup, as every member but one drops a read of a bucket still in flight,
// and answers every later one from the real broker beneath it.
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
// together drops reads routinely: a just-assigned bucket has no leader yet, and
// only its leader-to-be answers. A WRITE's reply is held until a leader exists,
// so a create waits [jsprovision.AskTerm] (fifteen seconds clustered); a lookup
// asked at that term stalls the boot waiting for a reply that never comes.
//
// CLUSTERED, because that topology's write term is the long one: on a solo
// broker the two terms would not tell a lookup asked at either apart.
func TestADroppedBucketLookupIsAskedAgainAfterAReadsTerm(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	cfg := jetstream.KeyValueConfig{Bucket: "t_dropped", TTL: time.Minute, Replicas: 1}
	if _, err := jsOf(nc).CreateKeyValue(t.Context(), cfg); err != nil {
		t.Fatalf("create the bucket the lookup is to find: %v", err)
	}
	js := &droppingLookups{JetStream: jsOf(nc)}

	started := time.Now()
	bucket, _, err := openBucket(t.Context(), js, true, cfg)
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

// droppingStreamReads is a broker that DROPS the first stream-info read (what
// a bucket's status is read through) and answers every later one for real.
type droppingStreamReads struct {
	jetstream.JetStream
	reads atomic.Int64
}

func (d *droppingStreamReads) Stream(ctx context.Context, name string) (jetstream.Stream, error) {
	if d.reads.Add(1) == 1 {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return d.JetStream.Stream(ctx, name)
}

// A REPLICA-COUNT READ NOBODY ANSWERED IS ASKED AGAIN RATHER THAN FAILING THE
// BOOT.
//
// The check refusing a bucket replicated below this node's configuration reads
// its status, a metadata read dropped like the lookup above. Asked once with
// the whole [jsprovision.ReadBack] window as deadline, a dropped request fails
// the boot over a bucket just found, so it goes through the one status read on
// the path, which re-asks an unanswered request.
//
// THE ANSWER IS THE PROOF: the bucket has one replica and the node asks for
// three, so an answered read is the replication refusal naming both counts, and
// one never re-asked a status failure naming neither.
func TestADroppedReplicaReadIsAskedAgainRatherThanFailingTheBoot(t *testing.T) {
	t.Parallel()
	nc := embeddedNATS(t)
	cfg := jetstream.KeyValueConfig{Bucket: "t_status", TTL: time.Minute, Replicas: 1}
	made, err := jsOf(nc).CreateKeyValue(t.Context(), cfg)
	if err != nil {
		t.Fatalf("create the bucket whose replicas are read: %v", err)
	}
	js := &droppingStreamReads{JetStream: jsOf(nc)}
	cfg.Replicas = 3

	_, _, err = observeBucket(t.Context(), js, made, cfg)
	if err == nil {
		t.Fatal("a bucket replicated 1x was accepted by a node configured for 3x")
	}
	if !strings.Contains(err.Error(), "replicated 1x") {
		t.Fatalf("the replica count was never read — the dropped status read "+
			"was not asked again, and the boot failed on the silence:\n%v", err)
	}
	if got := js.reads.Load(); got != 2 {
		t.Errorf("the status was read %d times, want 2: the dropped request "+
			"once, and once more to be answered", got)
	}
}
