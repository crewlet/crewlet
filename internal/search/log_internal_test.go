package search

import (
	"context"
	"log/slog"
	"testing"

	"github.com/crewlet/crewlet/internal/providers/embeddings"
	"github.com/crewlet/crewlet/internal/statelog"
	"github.com/crewlet/crewlet/internal/store"
)

// AN EMBED DUTY GIVEN NO LOGGER WRITES THROUGH THE SEARCH PACKAGE'S OWN.
//
// A batch failure is only ever LOGGED — the tick's other batches and the tick
// after it are the retry — so a duty whose logger went nowhere fails every
// batch with no trace at all. It read an absent logger as a discarding one,
// and the engine now hands it none. The tree-wide guard in internal/logging
// refuses a discarding handler; what it cannot see is a default that still
// writes but loses `component=search` (the process-wide `slog.Default()`) or
// drops a level, and that is what this pins — through the real constructor,
// because the default is decided there.
func TestAnEmbedDutyGivenNoLoggerWritesThroughThePackagesOwn(t *testing.T) {
	t.Parallel()
	valid := EmbedDeps{
		Publisher: &statelog.Publisher{},
		Estate:    (&store.DB{}).PartitionHandle("estate.000").Reader(),
		Log:       statelog.EstateStream(Domain{}).Name,
		Standing: func(context.Context) (LogStanding, error) {
			return LogStanding{}, nil
		},
		Embedder: widthOnly{},
		Model:    "probe-model",
		Corpora:  []Corpus{struct{ Corpus }{}},
		Budget:   noBound{},
	}
	duty, err := NewEmbedder(valid)
	if err != nil {
		t.Fatalf("NewEmbedder: %v", err)
	}
	if duty.deps.Logger != log {
		t.Errorf("the embed duty given no logger holds %p, not the search "+
			"package's own component logger %p", duty.deps.Logger, log)
	}

	// AND ONE THAT IS HANDED IN IS THE ONE USED.
	mine := slog.New(slog.DiscardHandler)
	valid.Logger = mine
	if duty, err = NewEmbedder(valid); err != nil {
		t.Fatalf("NewEmbedder: %v", err)
	}
	if duty.deps.Logger != mine {
		t.Errorf("the embed duty replaced a logger it was handed with %p", duty.deps.Logger)
	}
}

// widthOnly is the least an embedder can be and pass the constructor: a
// width. Nothing here embeds anything.
type widthOnly struct{ embeddings.BatchEmbedder }

func (widthOnly) Width() int { return 4 }

// noBound is a tick nothing bounds.
type noBound struct{}

func (noBound) Advanced()      {}
func (noBound) Exempt() func() { return func() {} }
