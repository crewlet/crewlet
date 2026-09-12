package search_test

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/queue"
	"github.com/crewlet/crewlet/internal/queue/memory"
	"github.com/crewlet/crewlet/internal/search"
)

// THE FAN-OUT REACHES A PEER PROCESS AND COMES BACK RANKED.
//
// The coordinator's own tests drive the seam directly; this one drives the
// wire — encode, scatter, decode, merge — because a payload that round-trips
// wrong is a ranking nobody can reproduce, and nothing above this layer can
// tell that from a corpus that changed.
func TestASlicedSearchTravelsAndComesBackRanked(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	coordinator := startClient(t, broker)
	peer := startClient(t, broker)

	// The peer answers for its own row and nothing else.
	stop, err := search.ServeSlices(t.Context(), peer, "n2", cannedScan{
		lexical: []search.Scored{{Key: "page:remote", Score: 0.7}},
	})
	if err != nil {
		t.Fatalf("serve slices: %v", err)
	}
	t.Cleanup(func() { _ = stop(context.WithoutCancel(t.Context())) })

	fan := &search.FanOut{
		Self: "n1",
		Local: fixed{search.Slice{
			Node:    "n1",
			Lexical: []search.Scored{{Key: "page:local", Score: 0.9}},
		}},
		Peers:  search.Broker{Queue: coordinator},
		Roster: func(context.Context) ([]string, error) { return []string{"n1", "n2"}, nil },
		Corpus: func(context.Context) (int, error) { return search.FanOutFloor * 10, nil },
		Budget: 5 * time.Second,
	}
	answer, err := fan.Search(t.Context(), search.FanQuery{Text: "anything", Limit: 10})
	if err != nil {
		t.Fatalf("fan-out search: %v", err)
	}
	if want := []string{"page:local", "page:remote"}; !slices.Equal(answer.Hits, want) {
		t.Fatalf("the fleet answered %v, want %v", answer.Hits, want)
	}
	if answer.Partial() {
		t.Fatalf("both assignments answered and %d buckets are reported missing",
			answer.BucketsMissing)
	}
}

// A NODE THE TABLE DOES NOT NAME STAYS SILENT.
//
// A node that joined after the coordinator read its roster receives the
// scattered request like every other node. If it volunteered an answer, its
// range would overlap somebody's: those buckets would be counted twice and the
// documents in them merged against themselves.
func TestANodeOutsideTheTableDoesNotVolunteer(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	coordinator := startClient(t, broker)

	// THE ONLY SERVER IS ONE THE TABLE DOES NOT NAME, which is what makes
	// this deterministic: an ask that stops at its first reply could
	// otherwise collect a legitimate answerer and never see the volunteer.
	// A silent node leaves the scatter to time out with nothing.
	stop, err := search.ServeSlices(t.Context(), startClient(t, broker), "n99",
		cannedScan{lexical: []search.Scored{{Key: "page:n99", Score: 0.5}}})
	if err != nil {
		t.Fatalf("serve slices: %v", err)
	}
	t.Cleanup(func() { _ = stop(context.WithoutCancel(t.Context())) })

	table := []search.Assigned{{Node: "n2", Shards: search.Assignment{From: 32, To: 64}}}
	deadline, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	answers, err := search.Broker{Queue: coordinator}.Scatter(deadline,
		search.FanQuery{Text: "anything"}, table)
	if err != nil {
		t.Fatalf("scatter: %v", err)
	}
	if len(answers) != 0 {
		got := make([]string, 0, len(answers))
		for _, s := range answers {
			got = append(got, s.Node)
		}
		t.Fatalf("a table naming only n2 was answered by %v — a node answering "+
			"a range it was not given overlaps one somebody else holds, so "+
			"those buckets are counted twice and their documents merged "+
			"against themselves", got)
	}
}

// A PEER SPEAKING ANOTHER PROTOCOL VERSION IS A MISSING SLICE, NOT A BROKEN
// SEARCH.
//
// A rolling upgrade puts two builds on one broker. A peer that cannot read the
// request answers nothing, which the coordinator already reports as a missing
// assignment — so an upgrade degrades a ranking rather than breaking a search.
func TestAPeerThatCannotReadTheRequestIsSimplyAbsent(t *testing.T) {
	t.Parallel()
	broker := memory.NewBroker()
	coordinator := startClient(t, broker)
	peer := startClient(t, broker)

	// A server that answers with something this build does not speak, which
	// is what an older or newer peer looks like from here.
	stop, err := peer.Serve(t.Context(), search.SliceSubject,
		func(context.Context, []byte) ([]byte, error) {
			return []byte(`{"version":99,"slice":{"node":"n2"}}`), nil
		})
	if err != nil {
		t.Fatalf("serve: %v", err)
	}
	t.Cleanup(func() { _ = stop(context.WithoutCancel(t.Context())) })

	fan := &search.FanOut{
		Self: "n1",
		Local: fixed{search.Slice{
			Node: "n1", Lexical: []search.Scored{{Key: "page:local", Score: 0.9}},
		}},
		Peers:  search.Broker{Queue: coordinator},
		Roster: func(context.Context) ([]string, error) { return []string{"n1", "n2"}, nil },
		Corpus: func(context.Context) (int, error) { return search.FanOutFloor * 10, nil },
		Budget: 2 * time.Second,
	}
	answer, err := fan.Search(t.Context(), search.FanQuery{Text: "anything", Limit: 10})
	if err != nil {
		t.Fatalf("a peer on another protocol version failed the search: %v", err)
	}
	if !slices.Equal(answer.Absent, []string{"n2"}) {
		t.Fatalf("the unreadable peer is reported as %v", answer.Absent)
	}
	if !slices.Equal(answer.Hits, []string{"page:local"}) {
		t.Fatalf("the local half of the answer is %v", answer.Hits)
	}
}

// cannedScan answers with a fixed slice, so the wire is what is under test.
type cannedScan struct{ lexical []search.Scored }

func (c cannedScan) Scan(context.Context, search.FanQuery, search.Assignment) (search.Slice, error) {
	return search.Slice{Lexical: c.lexical}, nil
}

// startClient mints one node on a shared broker and starts it.
func startClient(t *testing.T, b *memory.Broker) queue.EventQueue {
	t.Helper()
	q := b.Client()
	if err := q.Start(t.Context()); err != nil {
		t.Fatalf("start a queue client: %v", err)
	}
	t.Cleanup(func() { _ = q.Stop(context.WithoutCancel(t.Context())) })
	return q
}
