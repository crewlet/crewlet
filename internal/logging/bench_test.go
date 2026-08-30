package logging

import (
	"context"
	"io"
	"log/slog"
	"testing"
)

// These exist so the zap-versus-slog numbers are RUNNABLE rather than
// remembered.
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

// The same pair again in `json`, because the console numbers alone flatter
// the design: console's own encoder allocates, so [lazy]'s per-record
// handler rebuild is a smaller fraction of a bigger total. json is slog's
// zero-allocation handler and is what a log shipper is pointed at
// (docs/guides/deployment.md), so it is where the indirection is most
// visible — and a reader deciding whether this design is affordable should
// see its worst case, not its best.
func BenchmarkEmittedThroughLazyJSON(b *testing.B) {
	Configure(slog.LevelInfo, FormatJSON, io.Discard)
	b.Cleanup(func() { Configure(slog.LevelInfo, FormatConsole, io.Discard) })
	log := Get("bench.component").With("node", "n1")

	b.ReportAllocs()
	for b.Loop() {
		log.Info("an_event", "seat", "eng.alice", "epoch", 7)
	}
}

func BenchmarkEmittedDirectJSON(b *testing.B) {
	log := slog.New(slog.NewJSONHandler(io.Discard, &slog.HandlerOptions{Level: slog.LevelInfo})).
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

// The traced path, so the claim that correlation costs one allocation on
// traced lines ONLY is a number rather than an assertion.
//
// Read it against BenchmarkEmittedThroughLazy above, which is the same line
// with nothing bound: that gap is the whole cost of the injection, and it is
// paid only inside a span.
func BenchmarkEmittedWithTrace(b *testing.B) {
	Configure(slog.LevelInfo, FormatConsole, io.Discard)
	b.Cleanup(func() { Configure(slog.LevelInfo, FormatConsole, io.Discard) })
	log := Get("bench.component").With("node", "n1")
	ctx := WithTrace(context.Background(),
		"4bf92f3577b34da6a3ce929d0e0e4736", "00f067aa0ba902b7")

	b.ReportAllocs()
	for b.Loop() {
		log.InfoContext(ctx, "an_event", "seat", "eng.alice", "epoch", 7)
	}
}

// The UNtraced context path, which is what every line outside a span pays:
// one type assertion that finds nothing.
func BenchmarkEmittedWithoutTrace(b *testing.B) {
	Configure(slog.LevelInfo, FormatConsole, io.Discard)
	b.Cleanup(func() { Configure(slog.LevelInfo, FormatConsole, io.Discard) })
	log := Get("bench.component").With("node", "n1")
	ctx := context.Background()

	b.ReportAllocs()
	for b.Loop() {
		log.InfoContext(ctx, "an_event", "seat", "eng.alice", "epoch", 7)
	}
}
