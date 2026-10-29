package engine

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"

	"github.com/crewlet/crewlet/internal/api/mcpbridge"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/codingagent"
)

// THE DOCTOR AND THE RUN COUNT, as the Models page asks them of a node.

// CheckModel diagnoses a model entry on this node, as `crewlet llm doctor`
// does ([cliagent.Provider.Diagnose]): the active entry, or, with draft, the
// entry as a create or an edit would write it. values are candidate credential
// values by the variable the CLI is handed each under, tested in a home of
// their own and never stored.
//
// THE ACTIVE ENTRY LEARNS FROM IT, a draft never does. A diagnosis of the
// entry as it runs is a real call on its sign-in, recorded on its plan as any
// call is; a draft is built here with no plan registry of the node's, so what
// a candidate met says nothing about the sign-in the entry runs on.
//
// [ErrNoModel] for an existing entry the active revision does not have.
func (e *Engine) CheckModel(ctx context.Context, key string, draft *config.LLMProvider,
	values map[string]string, smoke bool) (cliagent.Diagnosis, error) {

	c := e.Company()
	if c == nil || c.Config == nil {
		return cliagent.Diagnosis{}, fmt.Errorf("%w: %q", ErrNoModel, key)
	}
	company := c.Config
	var agent *cliagent.Provider
	if draft == nil {
		var ok bool
		if agent, ok = e.cliEntry(key); !ok {
			return cliagent.Diagnosis{}, fmt.Errorf("%w: %q", ErrNoModel, key)
		}
	} else {
		// THE DRAFT IN THE COMPANY IT WOULD JOIN: which seats launch on
		// it and which cell it runs in are read off that document.
		spliced := *company
		spliced.Providers.LLM = maps.Clone(company.Providers.LLM)
		if spliced.Providers.LLM == nil {
			spliced.Providers.LLM = map[string]config.LLMProvider{}
		}
		spliced.Providers.LLM[key] = *draft
		company = &spliced
		built, err := BuildCLIAgent(key, *draft, e.resolver(), nil)
		if err != nil {
			return cliagent.Diagnosis{}, err
		}
		if err := built.RestoreConfiguredBundle(ctx); err != nil && !errors.Is(err, cliagent.ErrLoggedOut) {
			return cliagent.Diagnosis{}, err
		}
		agent = built
	}
	opts := cliagent.DiagnoseOptions{
		Smoke:         smoke,
		ExecutorSeats: company.ExecutorSeatsOn(key),
		AgentRunners:  codingagent.Names(),
		Cell:          string(company.EntryCell(key)),
		BridgeURL:     os.Getenv(mcpbridge.BaseURLVar),
		Values:        values,
	}
	if values != nil {
		// A CANDIDATE RUNS IN A HOME OF ITS OWN, made here and removed
		// here on every way out, an answer, a refusal, a cancelled
		// context or a panic alike: the CLI may write what it was handed
		// into its home (a refreshed token, a session), and a candidate
		// nobody stored must not outlive its test on the node's disk.
		home, err := os.MkdirTemp(e.checkHomeRoot(), "model-check-*")
		if err != nil {
			return cliagent.Diagnosis{}, fmt.Errorf("engine: a home for the candidate's test: %w", err)
		}
		defer func() { _ = os.RemoveAll(home) }()
		opts.Home = home
	}
	return agent.Diagnose(ctx, opts), nil
}

// checkHomeRoot is where a candidate's throwaway home is made: the process's
// temporary directory, or the one a test names.
func (e *Engine) checkHomeRoot() string {
	if e.checkHomes != "" {
		return e.checkHomes
	}
	return os.TempDir()
}

// RunningRunsOn counts the executor runs out in a box on any of keys now,
// across the fleet: the runs that keep the credential they were launched with
// until their segment ends, which a credential write tells the person about.
//
// FROM THE RUNS' OWN ROWS, which every node shares, by the entry each job was
// launched on ([sandbox.LaunchRecord.ProviderKey]).
func (e *Engine) RunningRunsOn(ctx context.Context, keys []string) (int, error) {
	rt := e.sandbox.Load()
	if rt == nil || rt.pending == nil || len(keys) == 0 {
		return 0, nil
	}
	runs, err := rt.pending.ListActive(ctx)
	if err != nil {
		return 0, err
	}
	n := 0
	for _, run := range runs {
		if slices.Contains(sandbox.Stoppable, run.Status) && slices.Contains(keys, run.Launch.ProviderKey) {
			n++
		}
	}
	return n, nil
}
