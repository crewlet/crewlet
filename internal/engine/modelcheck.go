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

// THE DOCTOR AND THE RUN COUNT, as the Models page asks them of a node.

// CheckModel diagnoses a model entry on this node, as `crewlet llm doctor`
// does ([harness.Provider.Diagnose]): the active entry, or, with draft, the
// entry as a create or an edit would write it. values are candidate credential
// values by the variable the CLI is handed each under, tested and never
// stored.
//
// THE ACTIVE ENTRY LEARNS FROM IT, a draft never does. A diagnosis of the
// entry as it runs is a real call on its sign-in, recorded on its plan as any
// call is; a draft is built here with no plan registry of the node's, so what
// a candidate met says nothing about the sign-in the entry runs on.
//
// A DRAFT OR A CANDIDATE NEVER WRITES THE LIVE ENTRY'S DISK but as a seat on
// it does. A candidate runs in a throwaway home made here and removed on every
// way out, an answer, a refusal, a cancelled context or a panic alike. A draft
// that would sign in with a login the active revision keeps on this node runs
// on that login as the entry's seats do, and any other draft runs in the
// throwaway home with only its own bundle ([Engine.draftAgent]). Built on the
// live entry's directory with a bundle of its own, a draft replaced the login
// every seat on the entry was signed in with, before anything was stored or
// applied.
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
		// THE DRAFT IN THE COMPANY IT WOULD JOIN: which seats launch on
		// it and which cell it runs in are read off that document.
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
// A DRAFT ON A LIVE LOGIN is built on that login's own state directory: it is
// on the directory and the CLI of an active entry, and names no bundle other
// than that entry's, so it would sign in exactly as that entry's seats do. Its
// calls check the login out of the shared directory under the shared login's
// lock, and carry a login the CLI refreshed back into it
// ([harness.Checkout.Release]); nothing is restored, the bundle being the one
// the apply already restored there. A COPY of the live login in a throwaway
// home was the wrong move: a CLI that refreshed its token during the check
// rotated the vendor's refresh token inside the copy, the copy was removed, and
// the live directory kept the retired token, which signs every seat out at the
// next expiry; and the copy was read without the lock.
//
// ANY OTHER DRAFT runs in home, the check's throwaway home: one that names a
// bundle of its own, restored into home and never over the live login, and
// one on a directory no active entry keeps, which it would otherwise make on
// the node's disk. Nothing of a live login is copied into home.
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

// loginKeptIn is the active entry, on agent's CLI, whose state directory is
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
