package kv

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/jsprovision"
	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/queue/jetstream/jetstreamtest"
)

// bindBucket binds bucket through js as the product does
// ([jsprovision.LookupBucket]): re-asked while nobody answers. A cluster case
// that bound it with one bare request failed whenever the stream's group was
// between leaders when it asked, which on a cluster seconds old and under
// load it sometimes is.
func bindBucket(ctx context.Context, t *testing.T, js jetstream.JetStream, bucket string) jetstream.KeyValue {
	t.Helper()
	kv, err := jsprovision.LookupBucket(ctx, js, bucket)
	if err != nil {
		t.Fatalf("bind %s: %v", bucket, err)
	}
	return kv
}

// streamOf is the info of the stream named name as its leader answered it
// ([jsprovision.LookupStream]).
func streamOf(ctx context.Context, t *testing.T, js jetstream.JetStream, name string) *jetstream.StreamInfo {
	t.Helper()
	info, err := jsprovision.LookupStream(ctx, js, name)
	if err != nil || info.Cluster == nil {
		t.Fatalf("stream %s's info: (%v, %v)", name, info, err)
	}
	return info
}

// A BUCKET IS FOUND ACROSS A STEP-DOWN OF ITS LEADER: a smoke test of the
// product's lookup on a real cluster. The stream's leader is asked to step
// down, once, and the bucket is then bound through another member and its
// leader read back, each looked up as the product looks one up.
//
// NOT THE PROOF OF THE RE-ASK: leadership usually moves in milliseconds, so a
// bare request would mostly pass here too. That the lookup asks again while
// nobody answers, and gives up within its budget, is held deterministically
// in internal/jsprovision (TestALookupIsAskedAgainUntilItIsAnswered and its
// neighbours).
func TestABucketIsFoundAcrossAStepDownOfItsLeader(t *testing.T) {
	t.Parallel()
	c := jetstreamtest.StartCluster(t, 3, js.Config{})
	ctx := t.Context()
	client := func(i int) (*nats.Conn, jetstream.JetStream) {
		t.Helper()
		nc, err := c.Servers[i].Conn()
		if err != nil {
			t.Fatalf("member %d: connect: %v", i, err)
		}
		t.Cleanup(nc.Close)
		client, err := jsapi.Embedded().Client(nc)
		if err != nil {
			t.Fatalf("member %d: client: %v", i, err)
		}
		return nc, client
	}
	const bucket = "lookup_across_leaders"
	stream := "KV_" + bucket
	nc, first := client(0)
	if _, _, err := openBucket(ctx, first, true, jetstream.KeyValueConfig{Bucket: bucket, Replicas: 3}); err != nil {
		t.Fatalf("open a three-replica bucket: %v", err)
	}
	old := streamOf(ctx, t, first, stream).Cluster.Leader
	other := -1
	for i, cfg := range c.Configs {
		if cfg.ServerName != old {
			other = i
			break
		}
	}

	// THE STEP-DOWN, SENT ONCE. Whatever it answers (stepped down, a leader
	// that already moved, or nothing within the term) the lookups below
	// must find the bucket and a leader; a re-sent step-down could only
	// move leadership again.
	stepCtx, cancel := context.WithTimeout(ctx, jsprovision.AskTerm(true))
	defer cancel()
	if msg, err := nc.RequestWithContext(stepCtx, "$JS.API.STREAM.LEADER.STEPDOWN."+stream, nil); err != nil {
		t.Logf("the step-down of %s was not answered: %v", stream, err)
	} else {
		var resp struct {
			Success bool            `json:"success"`
			Error   json.RawMessage `json:"error"`
		}
		if err := json.Unmarshal(msg.Data, &resp); err != nil || !resp.Success {
			t.Logf("the step-down of %s answered %s", stream, msg.Data)
		}
	}

	_, through := client(other)
	if kv := bindBucket(ctx, t, through, bucket); kv.Bucket() != bucket {
		t.Fatalf("bound %q, want %q", kv.Bucket(), bucket)
	}
	if leader := streamOf(ctx, t, through, stream).Cluster.Leader; leader == "" {
		t.Error("the bucket's stream info names no leader after the step-down")
	}
}
