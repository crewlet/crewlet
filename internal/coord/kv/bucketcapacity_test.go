package kv

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsprovision"
)

// refusingKV is a broker whose bucket CREATE is refused, and which counts how
// many times it was asked.
//
// IT WRAPS A REAL ONE, so every other call on the path is a genuine JetStream:
// only the create's answer and the lookup's are substituted, and the lookup
// answers not-found on purpose — that is what makes a read-back visible. An arm
// that stops being terminal falls through to [jsprovision.Settle], spends its
// window asking after an object nobody made, and appends "it is not there" to
// a refusal that had already said what was wrong.
type refusingKV struct {
	jetstream.JetStream
	refusal error
	creates atomic.Int64
}

func (f *refusingKV) KeyValue(context.Context, string) (jetstream.KeyValue, error) {
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
// # What this arm is for
//
// `stream.store_max_bytes` made the broker's storage limit a declared number,
// and a create it cannot back is refused with text naming no limit, no usage
// and no field. [openBucket] classifies that refusal as terminal — nothing was
// placed, and nothing frees capacity by being waited for — and the classification
// arrived with no case behind it: deleting the arm left the refusal falling
// through to the read-back, which spends [jsprovision.ReadBack] asking after an
// object nobody made and then reports a bucket that is "not there". An operator
// reading that goes looking for a missing bucket on a broker that is full.
//
// # Why the create is substituted and the broker is not
//
// Because everything else on this path has to be real for the assertions to
// mean anything — the lookup, the budgets, the read-back this must not reach.
// The wire shapes are the server's own.
//
// # Which shape a bucket actually gets, and why the others are here anyway
//
// A BUCKET DECLARES NO CEILING, so the refusals it can receive are not the ones
// a stream can. A standalone create is checked against the account's limit and
// the server's, and answers 10047 (or 10028 where its streams are in memory).
// A clustered create runs the account half alone, and the metadata leader's
// peer selection — where a clustered STREAM's server limit surfaces — skips its
// storage check entirely for an object with no ceiling
// (`maxBytes > 0 && maxBytes > available`, server/jetstream_cluster.go,
// selectPeerGroup). So the "no suitable peers, insufficient storage" shape
// cannot reach a bucket today.
//
// And where it could, it would not be this arm anyway: that refusal is about
// ANOTHER member's disk, which this node cannot read, so it stays
// [jsprovision.Unplaceable] and is waited out rather than reported. Its case is
// below, with the rest of placement.
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
			// what separates this arm from the placement one below.
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

// AND A PLACEMENT REFUSAL IS THE OPPOSITE ARM: IT IS WAITED OUT, AND THEN ALSO
// REPORTED WITHOUT A READ-BACK.
//
// # Why the two arms are two
//
// They are told apart by the RETRY and by nothing else — both end in the
// broker's own error, unwrapped, with no read-back — so merged into one
// condition neither could be exercised without the other. Separated, each has
// an observable of its own: a storage refusal is attempted once, and a cluster
// that has not gathered enough members is re-asked every
// [jsprovision.PlacementRetry] until the create's deadline.
//
// That difference is the whole of b5e4481: keyed on the placement code alone,
// a capacity refusal was claimed by this arm, waited out for the entire
// provisioning budget, and then reported as the broker's bare text.
//
// THE PARENT CONTEXT IS THE BUDGET HERE. [jsprovision.Clustered.Budget] is
// minutes, and `WithTimeout` only ever shortens — so a caller with a deadline
// of its own bounds both the lookup and the create, which is what makes the
// retry loop observable in a test rather than in a boot.
func TestAPlacementRefusalIsRetriedAndThenReportedWithoutAReadBack(t *testing.T) {
	t.Parallel()
	// A STORAGE CLAUSE INSIDE THE PLACEMENT CODE IS STILL PLACEMENT, which
	// is the reading the split turns on: the room that refused is another
	// member's disk, this node cannot read it, and a peer that is offline
	// or about to be given room clears by being waited for. A bucket
	// cannot receive this shape today — peer selection skips its storage
	// check for an object with no ceiling — but the predicate is shared
	// with the stream path, which can, and a bucket path reading it as
	// terminal would be that drift invisibly.
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
// # A third refusal, and it was the one nothing classified
//
// `no JetStream default or applicable tiered limit present` (10120) is the
// broker answering that the account's limits are TIERED and carry none for the
// replica class `stream.replicas` puts this node in. It is decided before a
// byte is compared, so it is not the arm above wearing another code: a bucket
// declares no ceiling at all, which makes it the clearest case of the two being
// different — there is nothing here to make smaller.
//
// Unclassified it matched neither [jsprovision.OutOfCapacity] nor
// [jsprovision.Unplaceable] and fell through to [jsprovision.Settle], which
// spent its window asking after a bucket nobody made and reported one that is
// "not there". Of everything a boot path can say about the store that holds the
// fleet's leases and this company's secrets, that is the sentence most likely
// to be read as corruption.
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
