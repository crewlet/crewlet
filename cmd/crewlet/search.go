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
         own top-K, so nobody authors a judgement.`

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
			Limit: *limit, Candidates: *candidates,
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
		// discovered: raise the quantization over-fetch first, which
		// measured free in latency, and fall back to an int8 first stage
		// in the same change that moves the model default.
		return fmt.Errorf("recall %.4f is below the %.4f floor for %d sources, "+
			"or %d document(s) were dropped from the first ten ranks",
			report.Recall, report.Floor, report.Sources, report.HeadMisses)
	}
	return nil
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
	fmt.Fprintf(w, "recall       %.4f  (floor %.4f for this corpus size)\n",
		r.Recall, r.Floor)
	fmt.Fprintf(w, "worst query  %.4f\n", r.WorstQuery)
	fmt.Fprintf(w, "head misses  %d  (documents dropped from the exact top ten)\n",
		r.HeadMisses)
	if fit {
		fmt.Fprintf(w, "mean cosine  %.4f  — FixtureMeanPairCos is fitted from "+
			"this; the gate's floor moves with it\n", r.MeanPairCos)
	}
	if r.Passed() {
		fmt.Fprintln(w, "verdict      the two-stage search recovers the exact "+
			"ranking at the shipped depth")
		return
	}
	fmt.Fprintln(w, "verdict      BELOW THE FLOOR — raise BinaryOversample "+
		"first (measured free in latency), then an int8 first stage")
}

// printEvalMetrics is the same report as one key=value line per metric.
//
// FLAT AND UNPREFIXED, because the consumer is a scrape or a shell rather than
// a person: a run in a schedule wants to record the recall it saw, and a
// reader that has to parse a formatted report is a reader that stops.
func printEvalMetrics(w io.Writer, r search.EvalReport) {
	fmt.Fprintf(w, "search_eval_sources %d\n", r.Sources)
	fmt.Fprintf(w, "search_eval_queries %d\n", r.Queries)
	fmt.Fprintf(w, "search_eval_limit %d\n", r.Limit)
	fmt.Fprintf(w, "search_eval_candidates %d\n", r.Candidates)
	fmt.Fprintf(w, "search_eval_recall %.6f\n", r.Recall)
	fmt.Fprintf(w, "search_eval_recall_floor %.6f\n", r.Floor)
	fmt.Fprintf(w, "search_eval_recall_worst %.6f\n", r.WorstQuery)
	fmt.Fprintf(w, "search_eval_head_misses %d\n", r.HeadMisses)
	fmt.Fprintf(w, "search_eval_mean_pair_cosine %.6f\n", r.MeanPairCos)
	fmt.Fprintf(w, "search_eval_passed %d\n", boolMetric(r.Passed()))
}

func boolMetric(v bool) int {
	if v {
		return 1
	}
	return 0
}
