package engine

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
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
// values by the variable the CLI is handed each under, tested and never
// stored.
//
// THE ACTIVE ENTRY LEARNS FROM IT, a draft never does. A diagnosis of the
// entry as it runs is a real call on its sign-in, recorded on its plan as any
// call is; a draft is built here with no plan registry of the node's, so what
// a candidate met says nothing about the sign-in the entry runs on.
//
// A DRAFT OR A CANDIDATE NEVER TOUCHES THE LIVE ENTRY'S DISK. Both run in a
// throwaway home made here and removed on every way out, an answer, a
// refusal, a cancelled context or a panic alike. A draft is built with that
// home as its state directory, so the bundle it names is restored there and
// nowhere else, and where the draft would sign in with a login the active
// revision keeps on this node (its state directory is an active entry's), the
// home is seeded with a COPY of that login: read, never written. Built on the
// live entry's own directory instead, a draft's bundle replaced the login every
// seat on the entry was signed in with, before anything was stored or applied.
//
// [ErrNoModel] for an existing entry the active revision does not have.
func (e *Engine) CheckModel(ctx context.Context, key string, draft *config.LLMProvider,
	values map[string]string, smoke bool) (cliagent.Diagnosis, error) {

	c := e.Company()
	if c == nil || c.Config == nil {
		return cliagent.Diagnosis{}, fmt.Errorf("%w: %q", ErrNoModel, key)
	}
	company := c.Config
	var home string
	if draft != nil || values != nil {
		var err error
		if home, err = os.MkdirTemp(e.checkHomeRoot(), "model-check-*"); err != nil {
			return cliagent.Diagnosis{}, fmt.Errorf("engine: a home for the model's check: %w", err)
		}
		defer func() {
			if err := os.RemoveAll(home); err != nil {
				log.WarnContext(ctx, "model_check_home_not_removed", "provider", key, "home", home,
					"error", err, "detail", "a check's throwaway home is left on this node's disk; "+
						"remove it by hand")
			}
		}()
	}
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
		built, err := e.isolatedDraft(ctx, c, key, *draft, home)
		if err != nil {
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
		opts.Home = home
	}
	return agent.Diagnose(ctx, opts), nil
}

// isolatedDraft builds draft with home as its state directory, seeded with a
// copy of the login an active entry keeps where the draft would share that
// entry's state directory, and restores the bundle the draft names there.
func (e *Engine) isolatedDraft(ctx context.Context, c *Company, key string, draft config.LLMProvider,
	home string) (*cliagent.Provider, error) {

	wouldShare, _ := draft.ResolvedStateDir(key)
	isolated := draft
	isolated.StateDir = home
	built, err := BuildCLIAgent(key, isolated, e.resolver(), nil)
	if err != nil {
		return nil, err
	}
	if live := e.loginKeptIn(c, wouldShare, draft.AgentName()); live != nil {
		if err := copyLogin(live.Workspace().CredentialsDir(), built.Workspace().CredentialsDir()); err != nil {
			return nil, fmt.Errorf("engine: copy the login the draft signs in with: %w", err)
		}
	}
	if err := built.RestoreConfiguredBundle(ctx); err != nil && !errors.Is(err, cliagent.ErrLoggedOut) {
		return nil, err
	}
	return built, nil
}

// loginKeptIn is the active entry, on agent's CLI, whose state directory is
// dir, nil where none is: the entry whose login a draft on that directory
// would sign in with.
func (e *Engine) loginKeptIn(c *Company, dir, agent string) *cliagent.Provider {
	if dir == "" || c.Config == nil {
		return nil
	}
	for _, key := range slices.Sorted(maps.Keys(c.Config.Providers.LLM)) {
		spec := c.Config.Providers.LLM[key]
		if got, err := spec.ResolvedStateDir(key); err != nil || got != dir || spec.AgentName() != agent {
			continue
		}
		if live, ok := e.cliEntry(key); ok {
			return live
		}
	}
	return nil
}

// copyLogin copies the regular files under src into dst, which it makes,
// keeping their relative paths and modes: a read of a login, never a move. A
// src that does not exist is a login of nothing.
func copyLogin(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		switch {
		case errors.Is(err, fs.ErrNotExist) && path == src:
			return nil
		case err != nil:
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o700)
		}
		if !d.Type().IsRegular() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, raw, info.Mode().Perm())
	})
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
