package stream_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/crewlet/crewlet/internal/api/stream"
)

// BenchmarkHubBroadcast measures one push reaching N clients across M
// postures.
//
// # What it is for, and what it is not
//
// It is the JUSTIFICATION for encoding once per posture rather than once per
// client, and nothing gates on it: a benchmark is a measurement of one machine
// on one day, so a threshold here would fail on a loaded CI runner and pass on
// a fast laptop while saying nothing about either. The property is pinned by
// TestAFrameIsEncodedOncePerPosture, which COUNTS encodes.
//
// The shape it is built to expose is the one the rewrite removed: the cost per
// broadcast used to grow with the number of tabs open, because the envelope
// travelled to every writer and every writer marshalled it again. Run it with
// clients=1 against clients=64 — the per-push cost should now grow with the
// DELIVERIES (a channel send each) and not with the encodes (one per posture,
// whatever N is).
//
// It broadcasts KindHealth deliberately: it is the one fanned-out kind both
// postures deliver, so postures=1 and postures=2 differ in the number of
// ENCODES and in nothing else.
func BenchmarkHubBroadcast(b *testing.B) {
	payload := map[string]any{
		"role": "Engineering Lead", "state": "working", "handle": "lead",
		"task_id": "CRW-1421", "phase": "execute", "round": 3,
		"since": "2026-06-14T12:00:00Z",
	}
	now := time.Date(2026, 6, 14, 12, 0, 0, 0, time.UTC)

	for _, clients := range []int{1, 8, 64} {
		for _, postures := range []int{1, 2} {
			name := fmt.Sprintf("clients=%d/postures=%d", clients, postures)
			b.Run(name, func(b *testing.B) {
				h := stream.NewHub()
				done := make(chan struct{})
				for i := range clients {
					c := stream.NewClient(reader)
					h.Register(c)
					if postures == 2 && i%2 == 1 {
						c.SetPosture(stream.FrameDegraded)
					}
					// One drain goroutine per client, which is what
					// a socket's writer is: without it every queue
					// fills and the run measures the drop path
					// instead of the fan-out.
					go func() {
						for {
							select {
							case <-c.Out():
							case <-done:
								return
							}
						}
					}()
				}
				b.Cleanup(func() { close(done); h.Close() })

				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					h.Broadcast(stream.Push(stream.KindHealth, payload, now))
				}
			})
		}
	}
}
