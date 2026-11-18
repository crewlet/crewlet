package kv

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsprovision"
)

// refusingKV is a broker whose bucket CREATE is refused, and which counts how
// many times it was asked.
//
// IT WRAPS A REAL ONE: only the create's answer and the lookup's are
// substituted, and the lookup answers not-found on purpose so that a read-back
// shows: an arm that stops being terminal falls through to [jsprovision.Settle]
// and appends "it is not there" to a refusal that already said what was wrong.
//
// UNLESS A PEER WON: with peerWon set, a lookup after the refused create is
// answered from the real broker, where the case made the bucket itself.
type refusingKV struct {
	jetstream.JetStream
	refusal error
	creates atomic.Int64
	peerWon bool
}

func (f *refusingKV) KeyValue(ctx context.Context, bucket string) (jetstream.KeyValue, error) {
	if f.peerWon && f.creates.Load() > 0 {
		return f.JetStream.KeyValue(ctx, bucket)
	}
	return nil, jetstream.ErrBucketNotFound
}

func (f *refusingKV) CreateKeyValue(context.Context, jetstream.KeyValueConfig) (
	jetstream.KeyValue, error) {

	f.creates.Add(1)
	return nil, f.refusal
}

func refusingBroker(t *testing.T, refusal error) *refusingKV {
	t.Helper()
	js, err := jetstream.New(embeddedNATS(t))
	if err != nil {
		t.Fatalf("jetstream context: %v", err)
	}
	return &refusingKV{JetStream: js, refusal: refusal}
}

// A STORAGE REFUSAL ON A BUCKET IS TERMINAL, AND IS NEVER REPORTED AS A BUCKET
// THAT IS NOT THERE.
//
// [openBucket] classifies a create the broker cannot back as terminal: nothing
// was placed, and waiting frees no capacity. Falling through to the read-back
// spends [jsprovision.ReadBack] asking after an object nobody made, then sends
// an operator looking for a missing bucket on a broker that is full. Only the
// create is substituted, so the lookup, the budgets and the read-back are real.
//
// With no ceiling, a bucket never meets "no suitable peers, insufficient
// storage" (selectPeerGroup skips its storage check), a placement refusal
// anyway (the case below).
func TestAStorageRefusalOnABucketIsTerminalAndNotAMissingBucket(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		refusal   *jetstream.APIError
		clustered bool
	}{
		"a standalone server's file-store limit": {&jetstream.APIError{
			ErrorCode: 10047, Code: 500,
			Description: "insufficient storage resources available",
		}, false},
		"a memory-backed server's, the silent half of the same event": {
			&jetstream.APIError{
				ErrorCode: 10028, Code: 500,
				Description: "insufficient memory resources available",
			}, false},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			js := refusingBroker(t, tc.refusal)

			started := time.Now()
			bucket, _, err := openBucket(t.Context(), js, tc.clustered,
				jetstream.KeyValueConfig{
					Bucket: "t_capacity", TTL: time.Minute, Replicas: 1,
				})
			elapsed := time.Since(started)

			if err == nil {
				t.Fatal("a bucket the broker refused for want of room was " +
					"reported as opened")
			}
			if bucket != nil {
				t.Error("a handle was returned beside the refusal, and nothing " +
					"was placed for it to name")
			}
			// THE BROKER'S OWN ANSWER, reported as itself.
			if !errors.Is(err, tc.refusal) {
				t.Errorf("the refusal is not the broker's own:\n%v", err)
			}
			// AND NOT READ BACK. This is the arm's whole observable
			// effect, and the one that tells a full broker from a
			// missing bucket.
			if strings.Contains(err.Error(), "it is not there") {
				t.Errorf("the refusal was read back, so a broker with no room "+
					"is reported as a bucket that does not exist:\n%v", err)
			}
			// ATTEMPTED ONCE. A limit nobody is going to raise inside
			// the provisioning budget must not be waited out — which is
			// what separates this refusal from the placement one below.
			if got := js.creates.Load(); got != 1 {
				t.Errorf("the create was issued %d times: a storage limit was "+
					"waited out as though the cluster were still forming", got)
			}
			if elapsed >= jsprovision.ReadBack {
				t.Errorf("the refusal took %v, which is at least the read-back "+
					"window — a terminal answer spent time asking after an "+
					"object nobody made", elapsed)
			}
		})
	}
}

// AND A PLACEMENT REFUSAL IS THE OPPOSITE CASE: IT IS WAITED OUT, AND THEN ALSO
// REPORTED WITHOUT A READ-BACK.
//
// Both end in the broker's own error with no read-back ([jsprovision.Refused]);
// only the RETRY, [jsprovision.Place]'s, tells them apart: a storage refusal is
// attempted once, a cluster still gathering members is re-asked every
// [jsprovision.PlacementRetry] until the deadline.
//
// THE PARENT CONTEXT IS THE BUDGET: [jsprovision.Clustered.Budget] is minutes
// and `WithTimeout` only shortens, so the case's own deadline bounds the loop.
func TestAPlacementRefusalIsRetriedAndThenReportedWithoutAReadBack(t *testing.T) {
	t.Parallel()
	// A STORAGE CLAUSE INSIDE THE PLACEMENT CODE IS STILL PLACEMENT: the room
	// that refused is another member's disk, which this node cannot read and
	// waiting can clear. A bucket cannot meet this shape today, but the predicate
	// is shared with the stream path, which can.
	refusal := &jetstream.APIError{
		ErrorCode: 10005, Code: 400,
		Description: "no suitable peers for placement, insufficient storage",
	}
	js := refusingBroker(t, refusal)

	// FOUR RETRY INTERVALS, so a loop that runs is unmistakable from one
	// that does not and the case still costs about a second.
	ctx, cancel := context.WithTimeout(t.Context(), 4*jsprovision.PlacementRetry)
	defer cancel()

	bucket, _, err := openBucket(ctx, js, true, jetstream.KeyValueConfig{
		Bucket: "t_unplaceable", TTL: time.Minute, Replicas: 3,
	})
	if err == nil {
		t.Fatal("a bucket the cluster refused to place was reported as opened")
	}
	if bucket != nil {
		t.Error("a handle was returned beside the refusal")
	}
	if !errors.Is(err, refusal) {
		t.Errorf("the refusal is not the broker's own:\n%v", err)
	}
	if strings.Contains(err.Error(), "it is not there") {
		t.Errorf("the refusal was read back, which appends a not-found for an "+
			"object the metadata leader declined to place:\n%v", err)
	}
	if got := js.creates.Load(); got < 2 {
		t.Errorf("the create was issued %d times: a cluster that has not yet "+
			"gathered enough members is the one condition worth waiting on, "+
			"and it was not waited on at all", got)
	}
}

// A BUCKET THE ACCOUNT HAS NO LIMIT FOR NAMES THE CLASS, AND IS NOT A MISSING
// BUCKET EITHER.
//
// 10120 means the account's limits are TIERED with none for the replica class
// `stream.replicas` puts this node in. It is decided before a byte is compared,
// so it is no storage refusal, and keeps its own arm in [openBucket], ahead of
// the general refusal, worded to name the class: a bucket declares no ceiling,
// so there is nothing to make smaller. Read back, it reports the bucket holding
// the fleet's leases and secrets as "not there": the sentence most likely to
// be read as corruption.
func TestABucketWithNoApplicableLimitNamesTheClassAndIsNotAMissingBucket(t *testing.T) {
	t.Parallel()
	refusal := &jetstream.APIError{ErrorCode: 10120, Code: 400,
		Description: "no JetStream default or applicable tiered limit present"}
	js := refusingBroker(t, refusal)

	started := time.Now()
	bucket, _, err := openBucket(t.Context(), js, true, jetstream.KeyValueConfig{
		Bucket: "t_nolimit", TTL: time.Minute, Replicas: 3,
	})
	elapsed := time.Since(started)

	if err == nil {
		t.Fatal("a bucket the account carries no limit for was reported as " +
			"opened")
	}
	if bucket != nil {
		t.Error("a handle was returned beside the refusal, and nothing was " +
			"placed for it to name")
	}
	if !errors.Is(err, refusal) {
		t.Errorf("the refusal is not the broker's own:\n%v", err)
	}
	for _, needle := range []string{"R3", "stream.replicas"} {
		if !strings.Contains(err.Error(), needle) {
			t.Errorf("the refusal does not mention %q, so an operator gets "+
				"the broker's bare text and nothing to move:\n%v", needle, err)
		}
	}
	if strings.Contains(err.Error(), "it is not there") {
		t.Errorf("the refusal was read back, so an account with no applicable "+
			"limit is reported as a bucket that does not exist:\n%v", err)
	}
	// ATTEMPTED ONCE AND ANSWERED AT ONCE. A limit table is not changed by
	// a member arriving, so neither the placement retry nor the capacity
	// grace has anything to wait for here.
	if got := js.creates.Load(); got != 1 {
		t.Errorf("the create was issued %d times", got)
	}
	if elapsed >= jsprovision.ReadBack {
		t.Errorf("the refusal took %v, which is at least the read-back "+
			"window — a terminal answer spent time asking after an object "+
			"nobody made", elapsed)
	}
}

// pinnedRefusal is the pinned server's own answer for id, in the shape the
// client hands a create — so a case asserts against the broker's table rather
// than against a number copied out of it.
func pinnedRefusal(t *testing.T, id server.ErrorIdentifier) *jetstream.APIError {
	t.Helper()
	e, ok := server.ApiErrors[id]
	if !ok {
		t.Fatalf("the pinned server has no error %d", id)
	}
	return &jetstream.APIError{ErrorCode: jetstream.ErrorCode(e.ErrCode),
		Code: e.Code, Description: e.Description}
}

// EVERY REFUSAL NO PEER COULD HAVE CAUSED IS TERMINAL, NOT ONLY CAPACITY, NO
// APPLICABLE LIMIT AND STILL FORMING.
//
// [openBucket] asks [jsprovision.Refused], the one list every create site
// shares; these are codes on it beyond those three, each of which a bucket
// create must not read back and report as a bucket "not there".
func TestEveryRefusalNoPeerCouldHaveCausedIsNotReadBack(t *testing.T) {
	t.Parallel()
	for name, id := range map[string]server.ErrorIdentifier{
		"subjects another stream already holds": server.JSStreamSubjectOverlapErr,
		"the account's stream count":            server.JSMaximumStreamsLimitErr,
		"a configuration the server rejects":    server.JSStreamInvalidConfigF,
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			refusal := pinnedRefusal(t, id)
			js := refusingBroker(t, refusal)

			started := time.Now()
			bucket, _, err := openBucket(t.Context(), js, true, jetstream.KeyValueConfig{
				Bucket: "t_refused", TTL: time.Minute, Replicas: 3,
			})
			elapsed := time.Since(started)

			if err == nil || bucket != nil {
				t.Fatalf("a bucket the broker refused to make came back as (%v, %v)",
					bucket, err)
			}
			if !errors.Is(err, refusal) {
				t.Errorf("the refusal is not the broker's own:\n%v", err)
			}
			if strings.Contains(err.Error(), "it is not there") {
				t.Errorf("the refusal was read back, so a create the broker "+
					"declined is reported as a bucket that does not exist:\n%v", err)
			}
			if got := js.creates.Load(); got != 1 {
				t.Errorf("the create was issued %d times: an answer no member "+
					"arriving can change was waited out", got)
			}
			if elapsed >= jsprovision.ReadBack {
				t.Errorf("the refusal took %v, which is at least the read-back "+
					"window — it was spent asking after a bucket nobody made",
					elapsed)
			}
		})
	}
}

// AND A CREATE A PEER WON IS STILL READ BACK, AND ITS BUCKET OPENED.
//
// The worse failure of the same list: a code wrongly taken as a refusal makes
// the node that lost a create race refuse to boot over a bucket that exists.
// "Stream name already in use" is that race's shape, so it must reach the
// read-back and open the peer's bucket.
func TestABucketAPeerMadeFirstIsReadBackAndOpened(t *testing.T) {
	t.Parallel()
	refusal := pinnedRefusal(t, server.JSStreamNameExistErr)
	js := refusingBroker(t, refusal)
	js.peerWon = true
	cfg := jetstream.KeyValueConfig{Bucket: "t_raced", TTL: time.Minute, Replicas: 1}
	// THE PEER'S BUCKET, made on the real broker beneath the fake one.
	if _, err := js.JetStream.CreateKeyValue(t.Context(), cfg); err != nil {
		t.Fatalf("make the peer's bucket: %v", err)
	}

	bucket, _, err := openBucket(t.Context(), js, true, cfg)
	if err != nil {
		t.Fatalf("a node that lost the create race to a peer refused to boot "+
			"over the peer's bucket: %v", err)
	}
	if bucket == nil || bucket.Bucket() != cfg.Bucket {
		t.Fatalf("opened %v, want the peer's bucket %q", bucket, cfg.Bucket)
	}
}
