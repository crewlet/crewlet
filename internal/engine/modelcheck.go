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
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
	"github.com/crewlet/crewlet/internal/sandbox"
	"github.com/crewlet/crewlet/internal/sandbox/executor"
)

// The doctor and the run count, as the Models page asks them of a node.

// CheckModel diagnoses a model entry on this node, as `crewlet llm doctor`
// does ([harness.Provider.Diagnose]): the active entry, or, with draft, the
// entry as a create or an edit would write it. values are candidate credential
// values keyed by the variable the harness gets each under, tested and never stored.
//
// Only the active entry's diagnosis is recorded on its plan: a draft is built
// with no plan registry, so what it met says nothing about the live sign-in.
// Neither a draft nor a candidate writes the live entry's disk other than as a
// seat on it would: a candidate runs in a throwaway home removed on every way
// out (a panic included), and a draft is placed by [Engine.draftAgent].
//
// [ErrNoModel] for an existing entry the active revision does not have.
func (e *Engine) CheckModel(ctx context.Context, key string, draft *config.LLMProvider,
	values map[string]string, smoke bool) (harness.Diagnosis, error) {

	c := e.Company()
	if c == nil || c.Config == nil {
		return harness.Diagnosis{}, fmt.Errorf("%w: %q", ErrNoModel, key)
	}
	company := c.Config
	var home string
	if draft != nil || values != nil {
		var err error
		if home, err = os.MkdirTemp(e.checkHomeRoot(), "model-check-*"); err != nil {
			return harness.Diagnosis{}, fmt.Errorf("engine: a home for the model's check: %w", err)
		}
		defer func() {
			if err := os.RemoveAll(home); err != nil {
				log.WarnContext(ctx, "model_check_home_not_removed", "provider", key, "home", home,
					"error", err, "detail", "a check's throwaway home is left on this node's disk; "+
						"remove it by hand")
			}
		}()
	}
	var agent *harness.Provider
	if draft == nil {
		var ok bool
		if agent, ok = e.harnessEntry(key); !ok {
			return harness.Diagnosis{}, fmt.Errorf("%w: %q", ErrNoModel, key)
		}
	} else {
		// The draft in the company it would join: its seats and cell are read
		// off that document.
		spliced := *company
		spliced.Providers.LLM = maps.Clone(company.Providers.LLM)
		if spliced.Providers.LLM == nil {
			spliced.Providers.LLM = map[string]config.LLMProvider{}
		}
		spliced.Providers.LLM[key] = *draft
		company = &spliced
		built, err := e.draftAgent(ctx, c, key, *draft, home)
		if err != nil {
			return harness.Diagnosis{}, err
		}
		agent = built
	}
	opts := harness.DiagnoseOptions{
		Smoke:             smoke,
		ExecutorSeats:     company.ExecutorSeatsOn(key),
		ExecutorHarnesses: executor.Names(),
		Cell:              string(company.EntryCell(key)),
		RunsAsRoot:        processEUID() == 0 && (company.Providers.Sandbox == nil || !company.Providers.Sandbox.Fake),
		BridgeURL:         os.Getenv(mcpbridge.BaseURLVar),
		Values:            values,
	}
	if values != nil {
		opts.Home = home
	}
	return agent.Diagnose(ctx, opts), nil
}

// draftAgent builds draft for its check, writing the live entry's disk only as
// every seat on it does.
//
// A draft on an active entry's directory and harness, naming no other bundle,
// signs in as that entry's seats do, so it runs on that directory: its calls
// check the login out under the shared lock and carry a refreshed login back
// ([harness.Checkout.Release]). Never a copy in a throwaway home: a refresh
// there rotates the vendor's token in the copy, leaving the live directory a
// retired one that signs every seat out at expiry. Any other draft (its own
// bundle, or a directory no active entry keeps, which it would create on the
// node's disk) runs in home, its bundle restored there, copying in nothing live.
func (e *Engine) draftAgent(ctx context.Context, c *Company, key string, draft config.LLMProvider,
	home string) (*harness.Provider, error) {

	dir, err := draft.ResolvedStateDir(key)
	if err != nil {
		return nil, fmt.Errorf("engine: the draft's state_dir: %w", err)
	}
	if liveKey, live, ok := e.loginKeptIn(c, dir, draft.HarnessName()); ok {
		own := e.bundleOf(key, draft)
		if own == "" || own == e.bundleOf(liveKey, live) {
			return BuildHarness(key, draft, e.resolver(), nil)
		}
	}
	isolated := draft
	isolated.StateDir = home
	built, err := BuildHarness(key, isolated, e.resolver(), nil)
	if err != nil {
		return nil, err
	}
	if err := built.RestoreConfiguredBundle(ctx); err != nil && !errors.Is(err, harness.ErrLoggedOut) {
		return nil, err
	}
	return built, nil
}

// loginKeptIn is the active entry, on agent's harness, whose state directory is
// dir: the entry whose login a draft on that directory would sign in with.
func (e *Engine) loginKeptIn(c *Company, dir, agent string) (string, config.LLMProvider, bool) {
	if dir == "" || c.Config == nil {
		return "", config.LLMProvider{}, false
	}
	for _, key := range slices.Sorted(maps.Keys(c.Config.Providers.LLM)) {
		spec := c.Config.Providers.LLM[key]
		if got, err := spec.ResolvedStateDir(key); err != nil || got != dir || spec.HarnessName() != agent {
			continue
		}
		if _, ok := e.harnessEntry(key); ok {
			return key, spec, true
		}
	}
	return "", config.LLMProvider{}, false
}

// bundleOf is the credential bundle spec under key restores, read as
// [BuildHarness] reads it: "" for none.
func (e *Engine) bundleOf(key string, spec config.LLMProvider) string {
	source := spec.CredentialSources(key).Bundle
	return resolveOrConvention(e.resolver(), source.Value, source.Fallback)
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
// across the fleet: they keep their launch credential until their segment ends,
// which a credential write tells the person. Counted from the runs' shared rows
// by launch entry ([sandbox.LaunchRecord.ProviderKey]).
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
