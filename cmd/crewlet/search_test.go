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
// for both sends an operator to the wrong knob exactly when there is an index.
//
// And the first-stage line names WHY a corpus scanned, because "no index"
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
			name: "a corpus with no index fails",
			report: search.EvalReport{Stage1: search.Stage1Report{
				Method: search.Stage1Scan, Why: search.ScanUnindexed},
				Recall: 0.90, Floor: 0.98, ScanRecall: 0.90},
			want:   []string{"the full scan — the corpus has no index yet", "raise BinaryOversample"},
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
			name: "a corpus below the index's minimum",
			report: search.EvalReport{Model: "m", Dim: 64, Stage1: search.Stage1Report{
				Method: search.Stage1Scan, Why: search.ScanRetired},
				Index:  &search.IndexSummary{Model: "m", Dim: 64, Why: search.VerdictTooSmall},
				Recall: 0.99, Floor: 0.98},
			want:   []string{"the corpus is below the index's minimum"},
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

// THE WINDOW THE REPORT MEASURES AT IS THE ONE THE DUTY EMBEDDED AT.
//
// The duty sends each source's opening up to the smaller of 8 KiB and the
// model's own per-input bound, and the report of what lies past that window is
// only true at the same number: measured at 8 KiB for a model that took 2 032
// bytes, it would call three quarters of every long page embedded. The store
// carries no configuration, so a model this build knows is resolved from its
// table — its request limits documented or not — and an operator's -window
// wins, for a bound their configuration lowered.
func TestTheWindowIsTheOneTheDutyEmbeddedAt(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, model string
		stated      int
		want        int
		basis       string
	}{
		{"OpenAI's window is the corpus's opening", "text-embedding-3-large", 0,
			search.EmbedInputBytes, "the corpus's opening"},
		{"a narrower documented window cuts it", "gemini-embedding-001", 0,
			2_032, "gemini-embedding-001's own per-input bound"},
		{"a model this build does not know takes the opening", "my-own-model", 0,
			search.EmbedInputBytes, "the corpus's opening"},
		{"a stated window wins", "text-embedding-3-large", 1_000,
			1_000, "as -window states"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, basis := embeddedWindow(tc.stated, tc.model)
			if got != tc.want || basis != tc.basis {
				t.Fatalf("the window for %s is %d (%s), want %d (%s)", tc.model,
					got, basis, tc.want, tc.basis)
			}
		})
	}
}

// THE REPORT AND ITS METRICS SAY HOW MUCH OF EACH CORPUS A SEARCH BY MEANING
// CANNOT SEE — the sources and the bytes past the window, beside the whole — so
// an operator can read the one number that decides whether a source needs more
// than one vector, and a schedule can record it.
func TestTheEvalReportsWhatLiesPastTheWindow(t *testing.T) {
	t.Parallel()
	past := windowReport{bytes: 8192, basis: "the corpus's opening",
		corpora: []search.WindowReport{
			{Source: search.SourceTask, Sources: 200, Beyond: 3, Bytes: 400_000, BeyondBytes: 20_000},
			{Source: search.SourcePage, Sources: 40, Beyond: 30, Bytes: 4_000_000, BeyondBytes: 3_000_000},
		}}
	var out bytes.Buffer
	printWindowReport(&out, past)
	for _, w := range []string{
		"window       8192 bytes a source (the corpus's opening)",
		"past window  task  3 of 200 sources (1.5%), 19.5 KiB of 390.6 KiB of text (5.0%)",
		"past window  page  30 of 40 sources (75.0%), 2.9 MiB of 3.8 MiB of text (75.0%)",
	} {
		if !strings.Contains(out.String(), w) {
			t.Errorf("the report does not say %q:\n%s", w, out.String())
		}
	}
	out.Reset()
	printWindowMetrics(&out, past)
	for _, w := range []string{"search_eval_window_bytes 8192\n",
		`search_eval_window_sources{source="page"} 40` + "\n",
		`search_eval_window_beyond_sources{source="page"} 30` + "\n",
		`search_eval_window_text_bytes{source="task"} 400000` + "\n",
		`search_eval_window_beyond_bytes{source="task"} 20000` + "\n"} {
		if !strings.Contains(out.String(), w) {
			t.Errorf("the metrics do not carry %q:\n%s", w, out.String())
		}
	}
}

// A NEGATIVE WINDOW IS REFUSED before anything is opened, rather than read as
// the default — a report printed under a flag that asked for another window
// would describe a window nobody chose.
func TestANegativeWindowIsRefused(t *testing.T) {
	t.Parallel()
	var out, errs bytes.Buffer
	err := runSearchEval([]string{"-store", t.TempDir() + "/absent.db", "-window", "-1"}, &out, &errs)
	if err == nil || !strings.Contains(err.Error(), "-window -1") {
		t.Fatalf("a negative window answered %v, want a refusal naming -window", err)
	}
}
