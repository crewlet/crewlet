package jetstream

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/changefeed"
)

// THE WAKE FEED'S DEFERRAL BACKS OFF NO FURTHER THAN THIS CONSUMER'S ACK
// WINDOW: a record handed back for a peer (changefeed.DeferDelay, #838) waits
// at most changefeed.DeferCap, which is domainConsumerAckWait, so it is never
// redelivered more slowly than a record nobody answered at all.
//
// Mutation: raise either constant alone, red.
func TestTheFeedsDeferralCapIsTheDomainAckWait(t *testing.T) {
	if changefeed.DeferCap != domainConsumerAckWait {
		t.Errorf("changefeed.DeferCap is %v and the domain consumer's AckWait %v: a deferred record would "+
			"come back on another schedule than an unanswered one", changefeed.DeferCap, domainConsumerAckWait)
	}
}

// A WAKE FEED'S DEAD LETTER IS KEPT IN CREWLET_DLQ FOR AS LONG AS THE EVENT
// STREAM KEEPS ANYTHING, and says when it goes.
//
// The entry carries the record's sealed frame, so it needs its log for
// nothing; what bounds it is the dead-letter stream's own age, which is the
// event stream's `stream.event_retention_hours` — a safety field every node of
// a fleet holds equal, so no node keeps an entry longer than another reads it
// as kept. The stream keeps by age and never by interest, so the entry is
// there whether or not anybody reads the subject; and the instant a listing
// says it expires is the one that bound gives, which the alarm counts down to.
//
// Mutation: route the feed's subject to another stream, or give the dead
// letters an age of their own, and the stream or its bound is wrong; compute
// the expiry from anything but the stream's bound, and the instant is.
func TestAFeedsDeadLetterIsKeptInTheDeadLetterStreamForTheEventRetention(t *testing.T) {
	t.Parallel()
	subject := changefeed.DeadLetterSubject(changefeed.Source{Name: "work", Group: "crewlet-tracker-feed"})
	for name, tc := range map[string]struct {
		cfg  Config
		keep time.Duration
	}{
		"the default retention":   {cfg: Config{}, keep: defaultEventRetention},
		"an operator's retention": {cfg: Config{EventRetention: 48 * time.Hour}, keep: 48 * time.Hour},
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			q := openForTest(t, tc.cfg)
			if err := q.KeepDeadLetter(t.Context(), subject, []byte("header\nframe")); err != nil {
				t.Fatalf("KeepDeadLetter: %v", err)
			}
			stream, err := q.streamFor(t.Context(), subject)
			if err != nil || stream != streamDeadLetter {
				t.Fatalf("the feed's dead letters land in %q (%v), want %s", stream, err, streamDeadLetter)
			}
			st, err := q.js.Stream(t.Context(), streamDeadLetter)
			if err != nil {
				t.Fatalf("read %s: %v", streamDeadLetter, err)
			}
			cfg := st.CachedInfo().Config
			if cfg.MaxAge != tc.keep || cfg.Retention != jetstream.LimitsPolicy {
				t.Errorf("%s keeps by %v for %v, want by limits for the event retention %v",
					streamDeadLetter, cfg.Retention, cfg.MaxAge, tc.keep)
			}
			if _, err := st.GetLastMsgForSubject(t.Context(), subject); err != nil {
				t.Errorf("the entry is not in %s: %v", streamDeadLetter, err)
			}
			kept, err := q.DeadLettersOn(t.Context(), subject)
			if err != nil || len(kept) != 1 {
				t.Fatalf("the subject lists %d entries (%v), want the one kept", len(kept), err)
			}
			if want := kept[0].StoredAt.Add(tc.keep); !kept[0].Expires.Equal(want) {
				t.Errorf("the entry kept at %v expires at %v, want %v: the stream's own %v later",
					kept[0].StoredAt, kept[0].Expires, want, tc.keep)
			}
		})
	}
}
