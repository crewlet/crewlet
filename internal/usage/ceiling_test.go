package usage_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	js "github.com/crewlet/crewlet/internal/queue/jetstream"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/usage"
)

// A FULL USAGE LOG REFUSES THE APPEND AND RAISES THE HEADROOM ALARM.
//
// Crossing the ceiling must never drop a day: the stream is created to refuse
// rather than discard, the publisher reports the refusal as a full log rather
// than as a transient failure, and the alarm that tells an operator names THIS
// domain — a fleet with four logs is told which one is full, not that
// something is.
func TestAFullUsageLogRefusesTheAppendAndAlarms(t *testing.T) {
	t.Parallel()
	q, err := js.Open(t.Context(), js.Config{StoreDir: t.TempDir()})
	if err != nil {
		t.Fatalf("open a broker: %v", err)
	}
	t.Cleanup(func() {
		if err := q.Stop(context.WithoutCancel(t.Context())); err != nil {
			t.Errorf("stop the broker: %v", err)
		}
	})
	spec := usage.Domain{}.Stream()
	const ceiling = 16 << 10
	if err := q.EnsureDomainStream(t.Context(), js.DomainStream{
		Name: spec.Name, Subjects: spec.Subjects, MaxBytes: ceiling,
		MaxPerSubject: spec.MaxPerSubject, MaxAge: spec.MaxAge,
		Duplicates: spec.Duplicates,
	}); err != nil {
		t.Fatalf("provision the log: %v", err)
	}
	now := time.Date(2026, 9, 23, 17, 0, 0, 0, santiago)
	node := joinFleet(t, q, "node-a", now)

	// FAR MORE SEAT-DAYS THAN THE CEILING HOLDS.
	ev := &events{t: t, db: node.db}
	for i := range 120 {
		ev.phase(now.Add(-time.Duration(i+1)*time.Minute), fmt.Sprintf("seat-%03d", i),
			fmt.Sprintf("t%03d", i), "execute", "", 100+i, 10)
	}
	err = node.publisher.Flush(t.Context())
	var refused *statelog.Unavailable
	if !errors.As(err, &refused) || refused.Reason != statelog.ReasonLogFull {
		t.Fatalf("a flush past the ceiling answered %v, want a %s refusal — a "+
			"full log that dropped the oldest day instead would lose history "+
			"with nothing to say so", err, statelog.ReasonLogFull)
	}

	log, err := q.DomainLog(t.Context(), spec.Name)
	if err != nil {
		t.Fatal(err)
	}
	stats, err := log.Stats(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if stats.FirstSeq > 1 {
		t.Fatalf("the log's first record is %d — a full log discarded its oldest "+
			"records rather than refusing the new one", stats.FirstSeq)
	}
	report := statelog.NewReport(statelog.ReportInputs{
		NodeID: "node-a", At: now, RegisterReadable: true,
		Domains: []statelog.DomainInputs{{
			Domain: usage.Domain{}.Name(), Stream: spec.Name, Replay: spec.Replay,
			FirstSeq: stats.FirstSeq, LastSeq: stats.LastSeq,
			Bytes: stats.Bytes, MaxBytes: stats.MaxBytes, StreamReadable: true,
		}},
	})
	var named bool
	for _, a := range report.Alarms {
		if a.Kind == statelog.KindLogHeadroom && strings.HasPrefix(a.Detail, "usage: ") {
			named = true
		}
	}
	if !named {
		t.Fatalf("a usage log at %d of %d bytes raised %+v — the headroom alarm "+
			"must fire and name the usage domain", stats.Bytes, stats.MaxBytes,
			report.Alarms)
	}
}
