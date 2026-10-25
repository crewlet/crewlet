package jetstream

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/statelog"
)

// BenchmarkADomainLogReadOfItsLargestRecord is what reading back by sequence
// ([DomainLog.At]) the largest record any log may hold costs on a member — a
// record of [statelog.MaxTransportRecordBytes], the ceiling the code log
// declares — which is the figure [DomainLog.At]'s term is judged against: the
// reply has to be in before the one-second read term, or the read is asked
// again with the first reply still crossing.
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
