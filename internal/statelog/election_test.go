package statelog_test

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/statelog"
)

// longestElectionTimeout is the longest a follower of the pinned nats-server
// waits before it campaigns (maxElectionTimeoutDefault, v2.15.3 server/raft.go:329):
// the longest a log's group goes with no leader, short of the new leader's
// first heartbeat.
const longestElectionTimeout = 9 * time.Second

// electingAppender is a log whose group has no leader until its election ends:
// every append before then is heard by nobody, as a publish to a stream nobody
// leads is, and every one after lands at seq.
type electingAppender struct {
	// elected says whether the election has ended: a literal of the
	// bubble's, which reads its clock.
	elected func() bool
	asked   *atomic.Int32
}

// electedSeq is where the first append after the election lands.
const electedSeq = 7

func electing(elected func() bool) electingAppender {
	return electingAppender{elected: elected, asked: &atomic.Int32{}}
}

// untilTheCaseEnds is an election that ends only once the case's checks have:
// then its barrier, still waiting detached from the read that left, is let
// commit inside the bubble, which ends only once every goroutine of its has.
func untilTheCaseEnds(t *testing.T, index func() *statelog.ReadIndex) func() bool {
	var over atomic.Bool
	t.Cleanup(func() {
		over.Store(true)
		_, _ = index().Read(context.WithoutCancel(t.Context()))
	})
	return over.Load
}

func (a electingAppender) Append(context.Context, string, string, *uint64, []byte) (uint64, bool, error) {
	a.asked.Add(1)
	if !a.elected() {
		return 0, false, jetstream.ErrNoStreamResponse
	}
	return electedSeq, false, nil
}

func (electingAppender) LastSeq(context.Context, string) (uint64, bool, error) { return 0, false, nil }

// A BARRIER ON A LOG SEVERAL MEMBERS STORE WAITS OUT AN ELECTION OF ITS LEADER,
// for the longest one the pinned broker's raft settings allow, and commits once
// a leader is heard: a barrier refused on the first leaderless answer refused
// every linearizable read on the node for an election's length
// (crewlet/crewlet#2096). In a bubble, so the election's length is exact.
//
// Mutants: the barrier returns its first leaderless answer — refused at once;
// its budget below the election (the old five seconds) — refused at five.
func TestABarrierWaitsOutAnElectionOfItsLogsLeader(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name string
		// log is the appender and the gate reserve's usage reading, given
		// whether the election has ended.
		log func(t *testing.T, elected func() bool) (statelog.Appender, *statelog.Reserve)
	}{
		{name: "its append heard by nobody", log: func(t *testing.T, elected func() bool) (statelog.Appender, *statelog.Reserve) {
			return electing(elected), noCeiling(t)
		}},
		{name: "its admission's usage reading answered 10008", log: func(t *testing.T, elected func() bool) (statelog.Appender, *statelog.Reserve) {
			reserve, err := statelog.NewReserve(probeDomain{}.Stream(),
				func(context.Context) (statelog.Usage, error) {
					if !elected() {
						return statelog.Usage{}, &jetstream.APIError{Code: 503, ErrorCode: 10008,
							Description: "JetStream system temporarily unavailable"}
					}
					return statelog.Usage{}, nil
				})
			if err != nil {
				t.Fatalf("NewReserve: %v", err)
			}
			return electing(func() bool { return true }), reserve
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			synctest.Test(t, func(t *testing.T) {
				ends := time.Now().Add(longestElectionTimeout)
				log, reserve := tc.log(t, func() bool { return !time.Now().Before(ends) })
				index, err := statelog.NewReadIndex(probeDomain{}, specOf(probeDomain{}), log, members(3),
					testSigner(t, probeDomain{}), reserve, probeEncode, func() uint32 { return 1 }, nil)
				if err != nil {
					t.Fatalf("NewReadIndex: %v", err)
				}
				at, err := index.Read(t.Context())
				if err != nil {
					t.Fatalf("a barrier on a log whose leader was being elected for %v was refused "+
						"rather than waiting the election out: %v", longestElectionTimeout, err)
				}
				if at.Seq != electedSeq {
					t.Errorf("the barrier is at %s, want the sequence the new leader stored, %d", at,
						electedSeq)
				}
			})
		})
	}
}

// A LINEARIZABLE READ WHOSE ASKER STOPS LISTENING MID-ELECTION IS REFUSED
// SAYING THE LOG HAD NO LEADER: still bounded by its caller, a reply's margin
// before the asker's deadline, and naming the stream, so whoever reads the
// refusal looks for an election rather than a majority that disagreed.
//
// Mutant: the barrier returns its first leaderless answer — refused at once,
// naming a broker that did not answer.
func TestALinearizableReadWhoseAskerStopsListeningMidElectionSaysTheLogHadNoLeader(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var index *statelog.ReadIndex
		log := electing(untilTheCaseEnds(t, func() *statelog.ReadIndex { return index }))
		index, err := statelog.NewReadIndex(probeDomain{}, specOf(probeDomain{}),
			log, members(3), testSigner(t, probeDomain{}), noCeiling(t), probeEncode,
			func() uint32 { return 1 }, nil)
		if err != nil {
			t.Fatalf("NewReadIndex: %v", err)
		}
		r := newReader(t, unreadStore{t}, healthy, &stubWaiter{at: healthy().Position}, index)
		ctx, deadline := asked(t)
		_, err = r.Read(ctx, pointQuery(statelog.ReadLinearizable), func(*sql.Tx) error { return nil })
		answeredAt(t, "a read whose log had no leader", deadline)
		var refusal *statelog.Refused
		if !errors.As(err, &refusal) || refusal.Code != statelog.RefuseNoQuorum {
			t.Fatalf("a read whose log had no leader answered %v, want refused %q", err,
				statelog.RefuseNoQuorum)
		}
		if want := "the " + probeStream + " stream had no leader"; !strings.Contains(refusal.Detail, want) {
			t.Errorf("the refusal reads %q, which does not say %q", refusal.Detail, want)
		}
	})
}

// A BARRIER WHOSE CALLER'S DEADLINE ENDS MID-ELECTION FAILS SAYING THE LOG HAD
// NO LEADER, with the deadline's own error beneath it.
//
// Mutant: the reader leaves with its context's bare error — no stream named.
func TestABarrierWhoseCallerRunsOutMidElectionSaysTheLogHadNoLeader(t *testing.T) {
	t.Parallel()
	synctest.Test(t, func(t *testing.T) {
		var index *statelog.ReadIndex
		log := electing(untilTheCaseEnds(t, func() *statelog.ReadIndex { return index }))
		index, err := statelog.NewReadIndex(probeDomain{}, specOf(probeDomain{}), log, members(3),
			testSigner(t, probeDomain{}), noCeiling(t), probeEncode, func() uint32 { return 1 }, nil)
		if err != nil {
			t.Fatalf("NewReadIndex: %v", err)
		}
		ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
		defer cancel()
		_, err = index.Read(ctx)
		if want := "the " + probeStream + " stream had no leader"; err == nil ||
			!strings.Contains(err.Error(), want) {
			t.Fatalf("a barrier whose caller ran out mid-election answered %v, want an error "+
				"saying %q", err, want)
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("the error %v is not its caller's deadline", err)
		}
		if n := log.asked.Load(); n < 2 {
			t.Errorf("the barrier was appended %d times, want it appended again while it waited", n)
		}
	})
}
