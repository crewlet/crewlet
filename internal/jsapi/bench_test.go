package jsapi_test

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/nats-io/nuid"

	"github.com/crewlet/crewlet/internal/code/gitproc/gittest"
	"github.com/crewlet/crewlet/internal/jsapi"
	"github.com/crewlet/crewlet/internal/objstore/natsobj"
)

// BenchmarkASubjectListing measures what a stream's LEADER takes to answer a
// listing of the subjects it holds a message on, a page of a hundred thousand
// at a time ([jsapi.Leader.Subjects]) — the object store's piece listing,
// which names every object the bucket holds, and a certified coordination
// listing's index. The leader sorts EVERY subject under the filter to cut each
// page, so a page costs what the whole set does and a listing of n subjects
// costs a sort of n for each of its pages — (n−1)/99,999 of them rounded up,
// one subject of overlap a page: reported are the pages a listing took, the
// slowest page and the whole listing, on one solo member's file store.
//
// THE SUBJECTS ARE THE OBJECT STORE'S OWN PIECE SUBJECTS,
// `$O.crewlet_files.C.<nuid>`, one per object, because the sort is of the
// subjects' bytes and a shorter name sorts faster — the figure the object
// store's doc gives for its objects is this one.
//
// A LISTING IS ASKED BEFORE THE TIMED ONES, and every page under an hour's
// deadline: a reader's first page ever is given the read term alone and
// asked again while it is still sorting, which the timed listings, given what
// the last page took, are not; and a page that outgrows the five seconds a
// caller with no deadline keeps is measured rather than cut off — the ceiling
// every read is held to is thirty seconds a page. A million subjects and
// three million run only under CREWLET_BENCH_LARGE=1.
func BenchmarkASubjectListing(b *testing.B) {
	for _, n := range []int{100_000, 300_000, 1_000_000, 3_000_000} {
		b.Run(fmt.Sprintf("subjects=%d", n), func(b *testing.B) {
			if n > 300_000 {
				gittest.Large(b, fmt.Sprintf("a stream holding a message on each of %d subjects", n))
			}
			leader := subjectStream(b, n)
			ctx, cancel := context.WithTimeout(b.Context(), time.Hour)
			defer cancel()
			var pages int
			var slowest time.Duration
			list := func() error {
				pages = 0
				last := time.Now()
				return leader.Subjects(ctx, pieceSubjects, func(map[string]uint64) error {
					pages++
					now := time.Now()
					slowest = max(slowest, now.Sub(last))
					last = now
					return nil
				})
			}
			if err := list(); err != nil {
				b.Fatal(err)
			}
			slowest = 0
			for b.Loop() {
				if err := list(); err != nil {
					b.Fatal(err)
				}
			}
			b.ReportMetric(float64(pages), "pages/op")
			b.ReportMetric(slowest.Seconds(), "slowest-page-s")
		})
	}
}

// pieceSubjects is every subject the object store keeps an object's pieces
// on, and pieceSubject one object's.
const pieceSubjects = "$O." + natsobj.Bucket + ".C.>"

func pieceSubject(id string) string { return "$O." + natsobj.Bucket + ".C." + id }

// subjectStream is a leader reader of a stream, on a solo member of its own,
// holding one message on each of n piece subjects.
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
		Name: "S", Subjects: []string{pieceSubjects}, Storage: jetstream.FileStorage,
	}); err != nil {
		b.Fatal(err)
	}
	// Published in windows of a few thousand, each acknowledged before the
	// next, so millions fit the client's own pending bound.
	const window = 4096
	for from := 0; from < n; from += window {
		acks := make([]jetstream.PubAckFuture, 0, window)
		for range min(window, n-from) {
			ack, err := js.PublishAsync(pieceSubject(nuid.Next()), nil)
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
