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
// IT WRAPS A REAL ONE, so every other call on the path is a genuine JetStream:
// only the create's answer and the lookup's are substituted, and the lookup
// answers not-found on purpose — that is what makes a read-back visible. An arm
// that stops being terminal falls through to [jsprovision.Settle], spends its
// window asking after an object nobody made, and appends "it is not there" to
// a refusal that had already said what was wrong.
//
// UNLESS A PEER WON: with peerWon set, a lookup made after the create was
// refused is answered from the real broker, where the case has made the bucket
// itself — the shape a node that lost a create race to a peer meets.
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
			bucket, err := openBucket(t.Context(), js, tc.clustered,
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
// # Why the two are told apart by the retry alone
//
// Both end in the broker's own error, unwrapped, with no read-back, which is
// why [openBucket] asks one predicate of both ([jsprovision.Refused]). What
// separates them is the RETRY, and that is [jsprovision.Place]'s: a storage
// refusal is attempted once, and a cluster that has not gathered enough
// members is re-asked every [jsprovision.PlacementRetry] until the create's
// deadline. Each has an observable of its own, so each has a case.
//
// That difference is the whole of b5e4481: keyed on the placement code alone,
// a capacity refusal was waited out for the entire provisioning budget, and
// then reported as the broker's bare text.
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

	bucket, err := openBucket(ctx, js, true, jetstream.KeyValueConfig{
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
// byte is compared, so it is not the storage refusal above wearing another
// code — and it keeps an arm of its own in [openBucket], ahead of the general
// refusal, because it is the one worded differently: a bucket
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
	bucket, err := openBucket(t.Context(), js, true, jetstream.KeyValueConfig{
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

// EVERY REFUSAL NO PEER COULD HAVE CAUSED IS TERMINAL, NOT ONLY THE THREE THIS
// SITE ONCE LISTED.
//
// The bucket create gated its read-back on its own three refusals — capacity,
// no applicable limit, still forming — so every other answer the broker gives
// a create it refused on the request alone fell through to
// [jsprovision.Settle]: a bucket whose subjects another stream already holds,
// or an account at its stream count, spent the read-back window asking after a
// bucket nobody made and was reported as one that is "not there". The site now
// asks [jsprovision.Refused], the one list every create site shares, and these
// are the codes on it the old arms did not name.
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
			bucket, err := openBucket(t.Context(), js, true, jetstream.KeyValueConfig{
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
// The other direction of the same list, and the one whose failure is worse: a
// code wrongly taken as a refusal turns the node that lost a create race into
// a node that refuses to boot over a bucket that exists. "Stream name already
// in use" is the tidy shape of that race — the peer's create committed first —
// so it must reach the read-back, find the peer's bucket and carry on.
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

	bucket, err := openBucket(t.Context(), js, true, cfg)
	if err != nil {
		t.Fatalf("a node that lost the create race to a peer refused to boot "+
			"over the peer's bucket: %v", err)
	}
	if bucket == nil || bucket.Bucket() != cfg.Bucket {
		t.Fatalf("opened %v, want the peer's bucket %q", bucket, cfg.Bucket)
	}
}
