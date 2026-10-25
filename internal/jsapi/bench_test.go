package jsapi_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/jsapi"
)

// BenchmarkASubjectListing measures what a stream's LEADER takes to answer a
// listing of the subjects it holds a message on, a page of a hundred thousand
// at a time ([jsapi.Leader.Subjects]) — the object store's piece listing,
// which names every object the bucket holds, and a certified coordination
// listing's index. The leader sorts EVERY subject under the filter to cut each
// page, so a page costs what the whole set does and a listing of n subjects
// costs a sort of n for each of its pages — (n−1)/99,999 of them rounded up,
// one subject of overlap a page: reported are the pages a listing took, the
// slowest page and the whole listing, on one solo member's file store. A
// million subjects runs only under CREWLET_BENCH_LARGE=1.
func BenchmarkASubjectListing(b *testing.B) {
	for _, n := range []int{100_000, 300_000, 1_000_000} {
		b.Run(fmt.Sprintf("subjects=%d", n), func(b *testing.B) {
			if n > 300_000 {
				gittest.Large(b, "a stream holding a message on each of a million subjects")
			}
			leader := subjectStream(b, n)
			var pages int
			var slowest time.Duration
			for b.Loop() {
				pages = 0
				last := time.Now()
				err := leader.Subjects(b.Context(), "s.>", func(page map[string]uint64) error {
					pages++
					now := time.Now()
					slowest = max(slowest, now.Sub(last))
					last = now
					return nil
				})
				if err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(pages), "pages/op")
			b.ReportMetric(slowest.Seconds(), "slowest-page-s")
		})
	}
}

// subjectStream is a leader reader of a stream, on a solo member of its own,
// holding one message on each of n subjects.
func subjectStream(b *testing.B, n int) *jsapi.Leader {
	b.Helper()
	srv, err := server.NewServer(&server.Options{
		ServerName: "jsapi-bench", JetStream: true, DontListen: true,
		StoreDir: b.TempDir(), NoLog: true, NoSigs: true,
	})
	if err != nil {
		b.Fatal(err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(30 * time.Second) {
		b.Fatal("the broker did not become ready")
	}
	b.Cleanup(func() {
		srv.Shutdown()
		srv.WaitForShutdown()
	})
	nc, err := nats.Connect("", nats.InProcessServer(srv))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(nc.Close)
	js, err := jsapi.Account().Client(nc)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := js.CreateStream(b.Context(), jetstream.StreamConfig{
		Name: "S", Subjects: []string{"s.>"}, Storage: jetstream.FileStorage,
	}); err != nil {
		b.Fatal(err)
	}
	// Published in windows of a few thousand, each acknowledged before the
	// next, so a million fit the client's own pending bound.
	const window = 4096
	for from := 0; from < n; from += window {
		acks := make([]jetstream.PubAckFuture, 0, window)
		for i := from; i < min(from+window, n); i++ {
			ack, err := js.PublishAsync(fmt.Sprintf("s.%08d", i), nil)
			if err != nil {
				b.Fatal(err)
			}
			acks = append(acks, ack)
		}
		for _, ack := range acks {
			select {
			case <-ack.Ok():
			case err := <-ack.Err():
				b.Fatal(err)
			}
		}
	}
	leader, err := jsapi.NewLeader(js, "S")
	if err != nil {
		b.Fatal(err)
	}
	return leader
}
