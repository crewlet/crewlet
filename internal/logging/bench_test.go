package logging

import (
	"io"
	"log/slog"
	"testing"
)

// These exist so decisions/001's numbers are RUNNABLE rather than remembered.
// The question they settle is "should this be zap?", and the honest answer
// depends on what the current design actually costs — which nobody could
// check without them.
//
// Read them as a pair. Emitted lines pay for the per-record handler rebuild
// ([lazy.resolve]); suppressed ones pay almost nothing, because [lazy.Enabled]
// answers from the root without replaying the ops. A debug call in a loop is
// therefore cheap while debug is off, which is the case that governs whether
// the guard clause has to be hand-written at each call site.

func BenchmarkEmittedThroughLazy(b *testing.B) {
	Configure(slog.LevelInfo, FormatConsole, io.Discard)
	b.Cleanup(func() { Configure(slog.LevelInfo, FormatConsole, io.Discard) })
	log := Get("bench.component").With("node", "n1")

	b.ReportAllocs()
	for b.Loop() {
		log.Info("an_event", "seat", "eng.alice", "epoch", 7)
	}
}

// The same line, on the same handler, with the lazy indirection removed. The
// gap between this and the benchmark above is what [lazy] costs to make a
// late Configure reach a logger bound at package init.
func BenchmarkEmittedDirect(b *testing.B) {
	log := slog.New(newConsoleHandler(io.Discard, slog.LevelInfo, ColorNever)).
		With("component", "bench.component").With("node", "n1")

	b.ReportAllocs()
	for b.Loop() {
		log.Info("an_event", "seat", "eng.alice", "epoch", 7)
	}
}

// THE ONE THAT DECIDES WHETHER A DEBUG CALL NEEDS A GUARD. It must stay
// allocation-free: [lazy.Enabled] deliberately does not replay the recorded
// ops, and a handler whose Enabled consulted its attributes would show up
// here before it showed up anywhere else.
func BenchmarkSuppressedThroughLazy(b *testing.B) {
	Configure(slog.LevelInfo, FormatConsole, io.Discard)
	b.Cleanup(func() { Configure(slog.LevelInfo, FormatConsole, io.Discard) })
	log := Get("bench.component").With("node", "n1")

	b.ReportAllocs()
	for b.Loop() {
		log.Debug("an_event", "seat", "eng.alice", "epoch", 7)
	}
}
