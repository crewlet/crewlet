package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/crewlet/crewlet/internal/assistant"
	"github.com/crewlet/crewlet/internal/assistant/evals"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
	"github.com/crewlet/crewlet/internal/words"
)

// `crewlet assistant eval`: what the Dashboard Assistant does with the tools
// it is offered, case by case.
//
// # Why it runs in this process
//
// A case describes the company it is asked in as the answers its requests
// receive, and runs through the assistant's real orchestrator and curated
// catalogue over the same harness the engine's suites use
// (internal/assistant/evals, production code the suites drive through the
// test-only assistanttest). So nothing is read from or written to a
// node, a case is repeatable, and the one thing that varies is the model:
// each case's own script by default, which calls none and is how the engine's
// tests run the shipped suite, or a `providers.llm` entry with -model, which
// is how an operator measures a candidate before naming it in
// `assistant.llm`. Permissions are not measured here: that a person's
// assistant reads and writes exactly what the person may is held by the
// deterministic suites in internal/api on every change.
func runAssistant(environ config.Source, args []string, stdout, stderr io.Writer) error {
	sub, rest := splitSubject(args)
	switch sub {
	case "eval":
		return runAssistantEval(environ, rest, stdout, stderr)
	case "":
		fmt.Fprintln(stderr, assistantUsage)
		return errors.New("name an assistant subcommand")
	default:
		fmt.Fprintln(stderr, assistantUsage)
		return fmt.Errorf("unknown assistant command %q", sub)
	}
}

const assistantUsage = `usage: crewlet assistant eval -suite DIR [-model KEY -paid] [-trials N] [-company PATH] [-config PATH] [-api URL]

  eval   Run a directory of evaluation cases through Ask Crewlet's
         own orchestrator and tool catalogue, in this process. Each case plays
         its own scripted rounds (no model is called) unless -model names a
         providers.llm entry to ask instead, which calls a paid model once per
         round of every trial and so also needs -paid. A case passes only when
         every trial does; the command exits non-zero when any case fails.`

func runAssistantEval(environ config.Source, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("assistant eval", flag.ContinueOnError)
	fs.SetOutput(stderr)
	suite := fs.String("suite", "", "the directory of cases: every *.yaml file in it")
	model := fs.String("model", "",
		"the providers.llm entry to ask; empty plays each case's own script")
	paid := fs.Bool("paid", false,
		"confirm that -model calls a paid model, once per round of every trial")
	trials := fs.Int("trials", 1, "how many times each case runs; it passes only if every run does")
	companyPath := fs.String("company", defaultCompanyPath,
		"Tier B, read with -model for the entry to ask")
	storeAt := addStoreFlags(fs)
	if err := fs.Parse(args); err != nil {
		return err
	}
	switch {
	case strings.TrimSpace(*suite) == "":
		fmt.Fprintln(stderr, assistantUsage)
		return errors.New("crewlet assistant eval: -suite names no directory")
	case *trials < 1:
		return fmt.Errorf("crewlet assistant eval: -trials %d runs nothing; pass 1 or more", *trials)
	case fs.NArg() > 0:
		return fmt.Errorf("crewlet assistant eval takes no arguments, got %s", words.And(fs.Args()))
	case *model != "" && !*paid:
		// A MODEL IS NEVER CALLED BY ACCIDENT: a suite run with -model is
		// one paid completion per round of every trial, so the spend is
		// confirmed on the command line rather than discovered on a bill.
		return fmt.Errorf("crewlet assistant eval: -model %s calls a paid model once per "+
			"round of every trial; pass -paid to confirm, or drop -model to play each "+
			"case's script", *model)
	case *model == "" && *paid:
		return errors.New("crewlet assistant eval: -paid confirms a -model run, and no " +
			"-model is named")
	}
	cases, err := evals.LoadSuite(*suite)
	if err != nil {
		return err
	}
	ctx := context.Background()
	provider, closeModel, err := evalModel(ctx, environ, *companyPath, storeAt, *model, stderr)
	if err != nil {
		return err
	}
	defer closeModel()
	opts := evals.Options{Trials: *trials}
	if provider != nil {
		fmt.Fprintf(stderr, "crewlet assistant eval: asking %s (%s), a paid model: %d "+
			"cases, %d trials each\n", *model, provider.Model(), len(cases), *trials)
		opts.Model, opts.ModelKey = provider, *model
	}
	results, err := evals.RunSuite(ctx, cases, opts)
	if err != nil {
		return err
	}
	if err := reportEval(stdout, results, opts); err != nil {
		return err
	}
	failed := 0
	for _, r := range results {
		if !r.Passed() {
			failed++
		}
	}
	if failed > 0 {
		return fmt.Errorf("crewlet assistant eval: %d of %d cases failed", failed, len(results))
	}
	return nil
}

// evalModel is the providers.llm entry key names, resolved for the assistant
// through the run's route to the company's secret store ([companyStore], the
// one `crewlet llm` reads through), and the closer of that route. An empty key
// is no model (each case plays its own script): a nil provider and a closer
// with nothing to close.
func evalModel(ctx context.Context, environ config.Source, companyPath string, storeAt storeFlags,
	key string, stderr io.Writer) (*harness.Provider, func(), error) {

	if key == "" {
		return nil, func() {}, nil
	}
	companySecrets, err := openCompanyStore(environ, storeAt, "crewlet assistant eval")
	if err != nil {
		return nil, nil, err
	}
	company, keys, resolver, err := loadLLMConfig(ctx, companyPath, companySecrets, "eval", key,
		stderr)
	if err != nil {
		companySecrets.Close()
		return nil, nil, err
	}
	provider, err := assistantProvider(company, keys, resolver, key)
	if err != nil {
		companySecrets.Close()
		return nil, nil, err
	}
	restoreConfiguredBundle(ctx, provider, stderr)
	return provider, companySecrets.Close, nil
}

// assistantProvider is the harness agent entry key names, refused when the
// company may not run the assistant on it.
func assistantProvider(company *config.Company, keys []string, resolver *config.Resolver,
	key string) (*harness.Provider, error) {

	if problem := company.AssistantEntryProblems(key); problem != nil {
		return nil, fmt.Errorf("crewlet assistant eval: %s may not run Ask Crewlet: %w",
			key, problem)
	}
	providers, err := harnessProviders(company, keys, resolver)
	if err != nil {
		return nil, err
	}
	return oneProvider(providers, key)
}

// reportEval prints one line per case (its passes, and the mean rounds,
// tokens and median round latency across its trials), then every failure,
// and a footer naming the prompt version and the model.
func reportEval(w io.Writer, results []evals.Result, opts evals.Options) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "case\tpass\trounds\ttokens\tp50 round")
	passed := 0
	var failures []string
	for _, r := range results {
		if r.Passed() {
			passed++
		}
		var rounds, tokens, ok int
		var latencies []time.Duration
		for _, t := range r.Trials {
			rounds += t.Rounds
			tokens += t.Tokens
			latencies = append(latencies, t.Latencies...)
			if len(t.Failures) == 0 {
				ok++
			}
		}
		n := len(r.Trials)
		fmt.Fprintf(tw, "%s\t%d/%d\t%d\t%d\t%s\n", r.Case, ok, n, rounds/n, tokens/n,
			median(latencies).Round(100*time.Millisecond))
		for i, t := range r.Trials {
			for _, f := range t.Failures {
				failures = append(failures, fmt.Sprintf("%s, trial %d: %s", r.Case, i+1, f))
			}
		}
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	// BELOW THE TABLE, so a long failure never widens its columns.
	for _, f := range failures {
		fmt.Fprintln(w, f)
	}
	model := "scripted"
	if opts.Model != nil {
		model = fmt.Sprintf("%s (%s)", opts.ModelKey, opts.Model.Model())
	}
	_, err := fmt.Fprintf(w, "prompt %s, model %s, pass^%d %d/%d cases\n",
		assistant.PromptVersion, model, max(opts.Trials, 1), passed, len(results))
	return err
}

// median is the middle duration of ds, zero for none.
func median(ds []time.Duration) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	sorted := slices.Clone(ds)
	slices.Sort(sorted)
	return sorted[len(sorted)/2]
}
