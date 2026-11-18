package jetstream

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// BenchmarkADomainLogReadOfItsLargestRecord measures [DomainLog.At] on a
// member for a record of [statelog.MaxTransportRecordBytes], the code log's
// ceiling: the reply, base64 in JSON and a third larger, is built and decoded
// in this process. It is the handling internal/jsapi's HandlingPace gives a
// read beyond the read term, without which the read is re-asked while its
// first reply is still being built. A link's crossing at the object store's
// floor ([Queue.recordArrival]) it does not measure.
func BenchmarkADomainLogReadOfItsLargestRecord(b *testing.B) {
	q, err := Open(b.Context(), Config{})
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	b.Cleanup(func() { _ = q.Stop(context.WithoutCancel(b.Context())) })
	if err := q.EnsureDomainStream(b.Context(), DomainStream{
		Name: "CREWLET_LARGE_BENCH", Subjects: []string{"crewlet.largebench.>"},
		MaxBytes: 64 << 20, Duplicates: time.Minute,
	}); err != nil {
		b.Fatalf("stream: %v", err)
	}
	log, err := q.DomainLog(b.Context(), "CREWLET_LARGE_BENCH")
	if err != nil {
		b.Fatalf("open the log: %v", err)
	}
	record := bytes.Repeat([]byte("r"), statelog.MaxTransportRecordBytes)
	seq, _, err := log.Append(b.Context(), "crewlet.largebench.a", "", nil, record)
	if err != nil {
		b.Fatalf("append the largest record: %v", err)
	}
	b.SetBytes(int64(len(record)))
	for b.Loop() {
		_, payload, _, ok, err := log.At(b.Context(), seq)
		if err != nil || !ok || len(payload) != len(record) {
			b.Fatalf("At = (%d bytes, %v, %v), want the record back whole", len(payload), ok, err)
		}
	}
}
