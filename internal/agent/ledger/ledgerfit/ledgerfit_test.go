package ledgerfit_test

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/crewlet/crewlet/internal/agent/ledger"
	"github.com/crewlet/crewlet/internal/agent/ledger/ledgerfit"
	"github.com/crewlet/crewlet/internal/compact"
)

type fitter struct {
	err   error
	calls atomic.Int32
}

func (f *fitter) Fit(_ context.Context, kind compact.Kind, text string, _ int) (compact.Result, error) {
	f.calls.Add(1)
	if f.err != nil {
		return compact.Result{From: len(text)}, f.err
	}
	return compact.Result{Text: "rewrite of " + string(kind), Compacted: true, From: len(text)}, nil
}

func pieces() []ledger.Piece {
	long := strings.Repeat("payload ", 200)
	return []ledger.Piece{
		{Kind: ledger.PieceArgument, Text: long, Limit: ledger.ValueLimit},
		{Kind: ledger.PieceError, Text: long + "!", Limit: ledger.ValueLimit},
		{Kind: ledger.PieceProduced, Text: long + "?", Limit: ledger.RenderedArtifactLimit},
	}
}

// A REWRITE IS MARKED AS ONE, whatever kind it is.
func TestARewriteRendersMarked(t *testing.T) {
	t.Parallel()
	f := &fitter{}
	got := ledgerfit.Fit(context.Background(), f, pieces())
	ps := pieces()
	if got[ps[0]] != ledgerfit.Condensed+"rewrite of argument" {
		t.Errorf("argument = %q", got[ps[0]])
	}
	if got[ps[1]] != ledgerfit.Condensed+"rewrite of tool_error" {
		t.Errorf("error = %q", got[ps[1]])
	}
	if !strings.Contains(got[ps[2]], "condensed by a model") || !strings.HasSuffix(got[ps[2]], "rewrite of produced") {
		t.Errorf("produced = %q", got[ps[2]])
	}
	if f.calls.Load() != 3 {
		t.Errorf("%d calls for three pieces", f.calls.Load())
	}
}

// NO REWRITE IS NEVER A CUT: a payload is omitted by size and digest, and the
// round's own draft renders whole (absent from the map).
func TestNoRewriteOmitsAPayloadAndKeepsTheDraftWhole(t *testing.T) {
	t.Parallel()
	for name, f := range map[string]ledgerfit.Fitter{
		"failing": &fitter{err: compact.ErrUnavailable},
		"none":    nil,
	} {
		t.Run(name, func(t *testing.T) {
			got := ledgerfit.Fit(context.Background(), f, pieces())
			ps := pieces()
			for _, p := range ps[:2] {
				shown := got[p]
				if !strings.HasPrefix(shown, "(") || strings.Contains(shown, "payload") {
					t.Errorf("%s fallback = %q, want an omission with no fragment", p.Kind, shown)
				}
			}
			if _, present := got[ps[2]]; present {
				t.Error("an unrewritten draft must render whole, not be replaced")
			}
		})
	}
}
