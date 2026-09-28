package main

import (
	"context"
	"database/sql"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/search"
	"github.com/crewlet/crewlet/internal/store"
)

// `crewlet search` — what the semantic half is actually doing on this
// company's own documents.
//
// # Why there is a command at all
//
// The quality gate in internal/search measures the ARITHMETIC on a seeded
// fixture and deliberately not recall on anybody's real corpus: a sign code
// keeps only each vector's orthant, and how much an orthant says about cosine
// rank is a property of the corpus's own distribution and of nothing else —
// over a family of embedding-shaped generators the same arithmetic spans 0.29
// to 0.98. There is therefore no number in this engine that answers "is
// semantic search working for us", and this is the command that asks.
//
// Run it monthly and after any change to providers.embeddings.model or
// .dimensions, which are the two inputs that move the answer.
//
// # It reads a FILE, not a running node
//
// The replicated estate is exclusively owned by the running engine, so this
// takes a path: the node's own file with the engine stopped, or — better, and
// what the runbook says — the copy inside a backup, which needs nothing
// stopped and measures the same rows.
func runSearch(args []string, stdout, stderr io.Writer) error {
	sub, rest := splitSubject(args)
	switch sub {
	case "eval":
		return runSearchEval(rest, stdout, stderr)
	case "":
		fmt.Fprintln(stderr, searchUsage)
		return errors.New("name a search subcommand")
	default:
		fmt.Fprintln(stderr, searchUsage)
		return fmt.Errorf("unknown search command %q", sub)
	}
}

const searchUsage = `usage: crewlet search eval [-store PATH] [-config PATH] [flags]

  eval   Measure the two-stage semantic search against the exact scan, on the
         vectors a store actually holds. Ground truth is the exact f32 scan's
         own top-K, so nobody authors a judgement. When the partition has a
         semantic index the search is measured through it — what searches
         run — and again with the full scan as its first stage, so the report
         says what the index costs. Every query is measured unfiltered,
         narrowed to each source and narrowed to its own container, and the
         evaluation passes only if every one of those shapes does.`

func runSearchEval(args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("search eval", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", defaultBootstrapPath,
		"Tier A config, read for the replicated store's path")
	storePath := fs.String("store", "",
		"the replicated database to measure; overrides -config, and is what a "+
			"backup's own copy is passed as")
	queries := fs.Int("queries", search.EvalQueries,
		"how many held-out documents to measure over")
	limit := fs.Int("limit", search.ReturnDepth,
		"the depth recall is measured at")
	candidates := fs.Int("candidates", search.Stage1Depth,
		"the stage-1 candidate depth")
	model := fs.String("model", "",
		"the embedding model to measure; empty takes the corpus's most populated")
	dim := fs.Int("dimensions", 0, "the width to measure; empty takes the model's")
	probes := fs.Int("probes", 0,
		"how many lists of the semantic index to probe; 0 takes the count the "+
			"index's own training measured, and a larger one shows what it would buy")
	fit := fs.Bool("fit", false,
		"print the corpus's own mean pairwise cosine, which is what the "+
			"seeded fixture's FixtureMeanPairCos is fitted from")
	metrics := fs.Bool("metrics", false,
		"print the report as one key=value line per metric, for a collector")
	if err := fs.Parse(args); err != nil {
		return err
	}

	path := strings.TrimSpace(*storePath)
	if path == "" {
		boot, err := config.LoadBootstrap(*configPath, config.EnvOnly())
		if err != nil {
			return err
		}
		if err := refuseScratchStore(boot, "crewlet search eval",
			"Run it on a node that holds data, or pass -store with a copy of "+
				"the replicated estate from a backup."); err != nil {
			return err
		}
		path = store.ReplicatedPath(boot.Store.Path, boot.Store.ReplicatedPath)
	}

	ctx := context.Background()
	// ONE ESTATE, opened as one. This file is a copy of the replicated
	// estate — the node's own or a backup's — and opening it as a node
	// would apply the OTHER estate's whole migration sequence into it and
	// open a second file beside it.
	db, err := store.OpenEstate(ctx, store.EstateReplicated, path, store.Options{})
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = db.Close() }()

	var report search.EvalReport
	var spaces []search.Space
	if err := db.Read(ctx, func(tx *sql.Tx) error {
		var err error
		if spaces, err = search.SpacesIn(ctx, tx); err != nil {
			return err
		}
		report, err = search.Eval(ctx, tx, search.EvalOptions{
			Model: *model, Dim: *dim, Queries: *queries,
			Limit: *limit, Candidates: *candidates, Probes: *probes,
		})
		return err
	}); err != nil {
		return err
	}

	if *metrics {
		printEvalMetrics(stdout, report)
	} else {
		printEvalReport(stdout, report, spaces, *fit)
	}
	if !report.Passed() {
		// A NON-ZERO EXIT, because this is a gate an operator can put in a
		// schedule. The remedy is decided in advance rather than
		// discovered, and which one depends on which first stage failed —
		// see printEvalReport's verdict.
		return fmt.Errorf("the two-stage search is below its floor for %d "+
			"sources in %s", report.Sources, strings.Join(failingShapes(report), ", "))
	}
	return nil
}

// shapeName is how a report names a query shape: its kind, and the source it
// narrows to when it does.
func shapeName(shape search.QueryShape, source search.Source) string {
	if source == "" {
		return string(shape)
	}
	return string(shape) + ":" + string(source)
}

// failingShapes names every shape that missed its floor or its head, the
// unfiltered one first.
func failingShapes(r search.EvalReport) []string {
	var out []string
	if r.Recall < r.Floor || r.HeadMisses != 0 {
		out = append(out, string(search.ShapeAll))
	}
	for _, s := range r.Shapes {
		if !s.Passed() {
			out = append(out, shapeName(s.Shape, s.Source))
		}
	}
	return out
}

// describeStage1 says what the first stage the unfiltered searches ran was.
func describeStage1(r search.EvalReport) string {
	s := r.Stage1
	if s.Method == search.Stage1IVF {
		out := fmt.Sprintf("the semantic index: %d of %d lists probed "+
			"(generation %d)", s.Probed, s.Lists, s.IVFGeneration)
		if s.Stale {
			out += ", mid-rollout — rows not yet re-filed read in full"
		}
		return out
	}
	switch s.Why {
	case search.ScanRetired:
		why := "its training installed none"
		switch {
		case r.Index != nil && r.Index.Why == search.VerdictTooSmall:
			why = "the partition is below the index's minimum corpus"
		case r.Index != nil && r.Index.Why == search.VerdictNotWorthwhile:
			why = fmt.Sprintf("its training found no probe count within 1/%d "+
				"of the lists that met the floor", search.IVFProbeCeiling)
		}
		return "the full scan — " + why
	case search.ScanOtherSpace:
		return "the full scan — the index was trained in another embedding " +
			"space, and the embed duty retrains it"
	case search.ScanUnindexed:
		return "the full scan — the partition has no index yet"
	case search.ScanFiltered:
		return "the full scan — the search's filter keeps too few rows near " +
			"its query for the index to read"
	}
	return "the full scan"
}

func printEvalReport(w io.Writer, r search.EvalReport, spaces []search.Space, fit bool) {
	fmt.Fprintf(w, "corpus       %d sources, %s at %d dimensions\n",
		r.Sources, r.Model, r.Dim)
	if len(spaces) > 1 {
		// TWO SPACES IS A REFILL IN PROGRESS, and it is worth saying
		// plainly: until it finishes, a search over the other space
		// returns nothing at all, because the scan filters on the pair.
		fmt.Fprintln(w, "             a refill is in progress:")
		for _, s := range spaces {
			fmt.Fprintf(w, "               %-32s %5d dim  %8d sources\n",
				s.Model, s.Dim, s.Sources)
		}
	}
	fmt.Fprintf(w, "measured     %d queries at depth %d from %d candidates\n",
		r.Queries, r.Limit, r.Candidates)
	fmt.Fprintf(w, "first stage  %s\n", describeStage1(r))
	// THE INDEX'S OWN MEASUREMENT only when it is about the space measured
	// here: one of a model the company has left describes nothing these
	// searches ran.
	if x := r.Index; x != nil && x.InSpace(r) && x.Measurement != nil {
		m := x.Measurement
		fmt.Fprintf(w, "index        measured on %d sources: %.4f against a %.4f "+
			"floor in its worst shape (%s), %d head miss(es)\n", m.Sources,
			m.Recall, m.Floor, shapeName(m.Shape, m.ShapeSource), m.HeadMisses)
	}
	fmt.Fprintf(w, "recall       %.4f  (floor %.4f for this corpus size)\n",
		r.Recall, r.Floor)
	fmt.Fprintf(w, "worst query  %.4f\n", r.WorstQuery)
	fmt.Fprintf(w, "head misses  %d  (documents dropped from the exact top ten)\n",
		r.HeadMisses)
	indexed := r.Stage1.Method == search.Stage1IVF
	if indexed {
		fmt.Fprintf(w, "scan recall  %.4f with %d head miss(es) — the same "+
			"search with the full scan as its first stage\n",
			r.ScanRecall, r.ScanHeadMisses)
	}
	for _, s := range r.Shapes {
		line := fmt.Sprintf("narrowed     %-16s recall %.4f  floor %.4f  head "+
			"misses %d", shapeName(s.Shape, s.Source), s.Recall, s.Floor, s.HeadMisses)
		if s.Scanned > 0 {
			line += fmt.Sprintf("  (%d of %d scanned)", s.Scanned, s.Queries)
		}
		if indexed {
			line += fmt.Sprintf("  — scan %.4f, %d head miss(es)", s.ScanRecall,
				s.ScanHeadMisses)
		}
		fmt.Fprintln(w, line)
	}
	if fit {
		fmt.Fprintf(w, "mean cosine  %.4f  — FixtureMeanPairCos is fitted from "+
			"this; the gate's floor moves with it\n", r.MeanPairCos)
	}
	if r.Passed() {
		fmt.Fprintln(w, "verdict      the two-stage search recovers the exact "+
			"ranking at the shipped depth, in every shape")
		return
	}
	// WHICH FIRST STAGE FAILED decides the remedy. A shape that fails
	// through the index while its own full scan passes is the index's
	// recall: the embed duty re-measures the index every day and retrains
	// it the moment it misses the floor, and -probes shows what reading more
	// lists recovers meanwhile. A scan that fails too is the sign codes
	// failing this corpus, whose remedy is the over-fetch.
	if indexed && scansPass(r) {
		fmt.Fprintf(w, "verdict      BELOW THE FLOOR THROUGH THE INDEX in %s, and "+
			"the full scan meets it — the corpus has moved since the index was "+
			"last measured; the embed duty re-measures it every %s and retrains "+
			"it when it misses the floor, and -probes shows what probing more "+
			"lists recovers meanwhile\n", strings.Join(failingShapes(r), ", "),
			search.IVFMeasureInterval)
		return
	}
	fmt.Fprintf(w, "verdict      BELOW THE FLOOR in %s — raise BinaryOversample "+
		"first (measured free in latency), then an int8 first stage\n",
		strings.Join(failingShapes(r), ", "))
}

// scansPass reports whether every shape's FULL SCAN met its floor with no
// head miss — the scan the index approximates.
func scansPass(r search.EvalReport) bool {
	if r.ScanRecall < r.Floor || r.ScanHeadMisses != 0 {
		return false
	}
	for _, s := range r.Shapes {
		if s.ScanRecall < s.Floor || s.ScanHeadMisses != 0 {
			return false
		}
	}
	return true
}

// printEvalMetrics is the same report as one key=value line per metric.
//
// FLAT AND UNPREFIXED, because the consumer is a scrape or a shell rather than
// a person: a run in a schedule wants to record the recall it saw, and a
// reader that has to parse a formatted report is a reader that stops. The
// narrowed shapes carry a `shape` label, the one form a scrape reads a family
// of series from.
func printEvalMetrics(w io.Writer, r search.EvalReport) {
	fmt.Fprintf(w, "search_eval_sources %d\n", r.Sources)
	fmt.Fprintf(w, "search_eval_queries %d\n", r.Queries)
	fmt.Fprintf(w, "search_eval_limit %d\n", r.Limit)
	fmt.Fprintf(w, "search_eval_candidates %d\n", r.Candidates)
	fmt.Fprintf(w, "search_eval_recall %.6f\n", r.Recall)
	fmt.Fprintf(w, "search_eval_recall_floor %.6f\n", r.Floor)
	fmt.Fprintf(w, "search_eval_recall_worst %.6f\n", r.WorstQuery)
	fmt.Fprintf(w, "search_eval_head_misses %d\n", r.HeadMisses)
	fmt.Fprintf(w, "search_eval_ivf %d\n", boolMetric(r.Stage1.Method == search.Stage1IVF))
	fmt.Fprintf(w, "search_eval_ivf_lists %d\n", r.Stage1.Lists)
	fmt.Fprintf(w, "search_eval_ivf_probed %d\n", r.Stage1.Probed)
	fmt.Fprintf(w, "search_eval_scan_recall %.6f\n", r.ScanRecall)
	fmt.Fprintf(w, "search_eval_scan_head_misses %d\n", r.ScanHeadMisses)
	for _, s := range r.Shapes {
		label := fmt.Sprintf(`{shape=%q}`, shapeName(s.Shape, s.Source))
		fmt.Fprintf(w, "search_eval_shape_recall%s %.6f\n", label, s.Recall)
		fmt.Fprintf(w, "search_eval_shape_recall_floor%s %.6f\n", label, s.Floor)
		fmt.Fprintf(w, "search_eval_shape_head_misses%s %d\n", label, s.HeadMisses)
		fmt.Fprintf(w, "search_eval_shape_scanned%s %d\n", label, s.Scanned)
		fmt.Fprintf(w, "search_eval_shape_passed%s %d\n", label, boolMetric(s.Passed()))
	}
	fmt.Fprintf(w, "search_eval_mean_pair_cosine %.6f\n", r.MeanPairCos)
	fmt.Fprintf(w, "search_eval_passed %d\n", boolMetric(r.Passed()))
}

func boolMetric(v bool) int {
	if v {
		return 1
	}
	return 0
}
