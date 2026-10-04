package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/crewlet/crewlet/internal/search"
)

// THE EVALUATION NAMES WHICH FIRST STAGE FAILED, because the two failures have
// opposite remedies.
//
// An index below the floor while the full scan over the same queries meets it
// is the INDEX's recall — the corpus has moved since it was trained, and
// raising the quantization over-fetch would pay latency on every search for a
// loss the codes never had. A scan below the floor is the sign codes failing
// this corpus, and there the over-fetch is the remedy. Printing one verdict
// for both sends an operator to the wrong knob on exactly the partitions that
// have an index.
//
// And the first-stage line names WHY a partition scanned, because "no index"
// has four causes an operator acts on differently: too small, measured not
// worth installing, trained in another space, never trained.
func TestTheEvalVerdictNamesWhichFirstStageFailed(t *testing.T) {
	t.Parallel()
	measured := &search.Measurement{Sources: 20000, Recall: 0.9859, Floor: 0.98,
		Shape: search.ShapeSource, ShapeSource: search.SourcePage}
	probed := search.Stage1Report{Method: search.Stage1IVF, Lists: 256,
		Probed: 128, IVFGeneration: 4203}
	pages := func(recall, scan float64, head int) search.ShapeReport {
		return search.ShapeReport{Shape: search.ShapeSource, Source: search.SourcePage,
			Queries: 25, Recall: recall, Floor: 0.98, HeadMisses: head,
			ScanRecall: scan}
	}
	for _, tc := range []struct {
		name   string
		report search.EvalReport
		// want is every fragment the report must carry, and absent every
		// fragment it must not.
		want, absent []string
	}{
		{
			name: "the index fails and the scan passes",
			report: search.EvalReport{Stage1: probed, Recall: 0.90, Floor: 0.98,
				HeadMisses: 3, ScanRecall: 0.99},
			want: []string{"the semantic index: 128 of 256 lists probed",
				"BELOW THE FLOOR THROUGH THE INDEX in all", "-probes",
				"re-measures it every 24h0m0s and retrains it",
				"scan recall  0.9900 with 0 head miss(es)"},
			absent: []string{"raise BinaryOversample"},
		},
		{
			name: "the index fails and so does the scan",
			report: search.EvalReport{Stage1: probed, Recall: 0.90, Floor: 0.98,
				ScanRecall: 0.95},
			want:   []string{"raise BinaryOversample"},
			absent: []string{"THROUGH THE INDEX"},
		},
		{
			name: "the scan passes but drops a head document",
			report: search.EvalReport{Stage1: probed, Recall: 0.90, Floor: 0.98,
				ScanRecall: 0.99, ScanHeadMisses: 1},
			want:   []string{"raise BinaryOversample"},
			absent: []string{"THROUGH THE INDEX"},
		},
		{
			name: "only a narrowed shape fails through the index",
			report: search.EvalReport{Stage1: probed, Recall: 0.99, Floor: 0.98,
				ScanRecall: 0.99, Shapes: []search.ShapeReport{pages(0.95, 1, 2)}},
			want: []string{"BELOW THE FLOOR THROUGH THE INDEX in source:page",
				"narrowed     source:page"},
			absent: []string{"raise BinaryOversample", "recovers the exact"},
		},
		{
			name: "a narrowed shape the scan fails too",
			report: search.EvalReport{Stage1: probed, Recall: 0.99, Floor: 0.98,
				ScanRecall: 0.99, Shapes: []search.ShapeReport{pages(0.95, 0.96, 0)}},
			want:   []string{"BELOW THE FLOOR in source:page — raise BinaryOversample"},
			absent: []string{"THROUGH THE INDEX"},
		},
		{
			name: "a partition with no index fails",
			report: search.EvalReport{Stage1: search.Stage1Report{
				Method: search.Stage1Scan, Why: search.ScanUnindexed},
				Recall: 0.90, Floor: 0.98, ScanRecall: 0.90},
			want:   []string{"the full scan — the partition has no index yet", "raise BinaryOversample"},
			absent: []string{"THROUGH THE INDEX", "scan recall"},
		},
		{
			name: "an index that passes",
			report: search.EvalReport{Model: "m", Dim: 64, Stage1: probed,
				Recall: 0.99, Floor: 0.98, ScanRecall: 0.995,
				Shapes: []search.ShapeReport{pages(0.99, 1, 0)},
				Index: &search.IndexSummary{Model: "m", Dim: 64, Lists: 256,
					Probes: 128, TrainedOn: 20000, Measurement: measured}},
			want: []string{"recovers the exact ranking",
				"index        measured on 20000 sources: 0.9859 against a 0.9800 " +
					"floor in its worst shape (source:page), 0 head miss(es)"},
			absent: []string{"BELOW THE FLOOR"},
		},
		{
			name: "mid-rollout",
			report: search.EvalReport{Stage1: search.Stage1Report{
				Method: search.Stage1IVF, Lists: 64, Probed: 16, Stale: true},
				Recall: 0.99, Floor: 0.98},
			want: []string{"mid-rollout"},
		},
		{
			name: "a training that measured the index not worth installing",
			report: search.EvalReport{Model: "m", Dim: 64, Stage1: search.Stage1Report{
				Method: search.Stage1Scan, Why: search.ScanRetired},
				Index: &search.IndexSummary{Model: "m", Dim: 64,
					Why: search.VerdictNotWorthwhile, TrainedOn: 20000,
					Measurement: measured},
				Recall: 0.99, Floor: 0.98},
			want: []string{"no probe count within 1/2 of the lists", "index        measured"},
		},
		{
			name: "a partition below the index's minimum",
			report: search.EvalReport{Model: "m", Dim: 64, Stage1: search.Stage1Report{
				Method: search.Stage1Scan, Why: search.ScanRetired},
				Index:  &search.IndexSummary{Model: "m", Dim: 64, Why: search.VerdictTooSmall},
				Recall: 0.99, Floor: 0.98},
			want:   []string{"below the index's minimum corpus"},
			absent: []string{"index        measured"},
		},
		{
			// A VERDICT ABOUT A MODEL THE COMPANY HAS LEFT describes no
			// search measured here, so its training is not printed as if
			// it were this corpus's.
			name: "an index trained in another space",
			report: search.EvalReport{Model: "m", Dim: 64, Stage1: search.Stage1Report{
				Method: search.Stage1Scan, Why: search.ScanOtherSpace},
				Index: &search.IndexSummary{Model: "left", Dim: 64,
					Why: search.VerdictNotWorthwhile, Measurement: measured},
				Recall: 0.99, Floor: 0.98},
			want:   []string{"trained in another embedding space"},
			absent: []string{"index        measured", "no probe count"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var out bytes.Buffer
			printEvalReport(&out, tc.report, nil, false)
			for _, w := range tc.want {
				if !strings.Contains(out.String(), w) {
					t.Errorf("the report does not say %q:\n%s", w, out.String())
				}
			}
			for _, a := range tc.absent {
				if strings.Contains(out.String(), a) {
					t.Errorf("the report says %q, which is the other failure's "+
						"remedy or a line with nothing to report:\n%s", a, out.String())
				}
			}
		})
	}
}

// The metrics form carries the first stage and every narrowed shape too, so a
// scheduled evaluation's scrape can tell an index's regression from the codes'
// and a narrowed search's from an unfiltered one's.
func TestTheEvalMetricsNameTheFirstStage(t *testing.T) {
	t.Parallel()
	var out bytes.Buffer
	printEvalMetrics(&out, search.EvalReport{
		Stage1:     search.Stage1Report{Method: search.Stage1IVF, Lists: 256, Probed: 128},
		ScanRecall: 0.99, ScanHeadMisses: 2,
		Shapes: []search.ShapeReport{{Shape: search.ShapeContainer,
			Source: search.SourceTask, Queries: 20, Scanned: 20, Recall: 0.97,
			Floor: 0.98}},
	})
	for _, w := range []string{"search_eval_ivf 1\n", "search_eval_ivf_lists 256\n",
		"search_eval_ivf_probed 128\n", "search_eval_scan_recall 0.990000\n",
		"search_eval_scan_head_misses 2\n",
		`search_eval_shape_recall{shape="container:task"} 0.970000` + "\n",
		`search_eval_shape_scanned{shape="container:task"} 20` + "\n",
		`search_eval_shape_passed{shape="container:task"} 0` + "\n",
		"search_eval_passed 0\n"} {
		if !strings.Contains(out.String(), w) {
			t.Errorf("the metrics do not carry %q:\n%s", w, out.String())
		}
	}
}
