package api

import (
	"cmp"
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/crewlet/crewlet/internal/api/configapi"
	"github.com/crewlet/crewlet/internal/api/llmapi"
	"github.com/crewlet/crewlet/internal/api/operator"
	"github.com/crewlet/crewlet/internal/api/queries"
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/coord"
	"github.com/crewlet/crewlet/internal/engine"
	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/fleetsecrets"
	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/modelcatalog"
	"github.com/crewlet/crewlet/internal/providers/llm/harness"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
	"github.com/crewlet/crewlet/internal/secrets"
	"github.com/crewlet/crewlet/internal/statelog"
)

// Settings › Models over a running engine: the model routes
// (internal/api/llmapi) and the model questions (internal/api/queries), each
// declaring the engine it needs as an interface of its own, adapted here from
// the engine's methods. ONE PLACE, which `crewlet run` and the end-to-end
// suite both call, so the two cannot wire Settings › Models differently.

// NewModelSurface builds the model routes over e, writing through the config
// surface and the fleet's secret store sealed under cipher, and auditing
// through audit.
func NewModelSurface(e *engine.Engine, config *configapi.Service, cipher secrets.Cipher,
	audit operator.AuditPublisher) (*llmapi.Service, error) {

	return llmapi.New(llmapi.Options{
		Engine:  modelEngine{e: e},
		Config:  config,
		Secrets: fleetsecrets.New(e.Backends().Fleet, cipher),
		Leases:  e.Backends().Coord,
		Audit:   audit,
		Node:    e.Node().ID(),
	})
}

// ModelSources fills the model half of the question sources over e: what this
// node resolved and its alarms, the fleet's secret rows, its device sign-ins,
// and the model catalogue.
func ModelSources(s *queries.Sources, e *engine.Engine, cipher secrets.Cipher) {
	s.Models = modelSource{e: e}
	s.Secrets = fleetsecrets.New(e.Backends().Fleet, cipher)
	s.DeviceSignIns = e.Backends().Fleet
	s.Catalogue = modelcatalog.New(modelcatalog.Options{})
}

// modelSource is the engine as `llm_entries` reads it.
type modelSource struct{ e *engine.Engine }

func (m modelSource) LocalModels() []queries.LocalModel {
	rows := m.e.LocalModels()
	out := make([]queries.LocalModel, 0, len(rows))
	for _, r := range rows {
		out = append(out, queries.LocalModel{Key: r.Key, Route: r.Route, Variable: r.Variable,
			Resolves: r.Resolves, Hint: r.Hint, TokenLifetime: r.TokenLifetime})
	}
	return out
}

func (m modelSource) LatestAlarms() []statelog.Alarm { return m.e.LatestAlarms() }

// modelEngine is the engine as the model routes call it, its refusals mapped
// onto llmapi's.
type modelEngine struct{ e *engine.Engine }

func (m modelEngine) CheckModel(ctx context.Context, key string, draft *config.LLMProvider,
	values map[string]string, smoke bool) (llmapi.Check, error) {

	spec := draft
	if spec == nil {
		c := m.e.Company()
		if c == nil || c.Config == nil {
			return llmapi.Check{}, llmapi.ErrNoSuchModel
		}
		entry, ok := c.Config.Providers.LLM[key]
		if !ok {
			return llmapi.Check{}, llmapi.ErrNoSuchModel
		}
		spec = &entry
	}
	out := llmapi.Check{At: time.Now().UTC(), Node: m.e.Node().ID()}
	budget := llmapi.CheckBudget
	var handed map[string]string
	if values != nil {
		// A CANDIDATE IS TESTED BY THE REAL CALL: the smoke run is what
		// signs the CLI in with it, so it always runs, within the entry's
		// own timeout capped at the credential budget.
		smoke = true
		budget = min(llmapi.CredentialBudget, time.Duration(spec.Timeout()*float64(time.Second)))
		profile, err := harnessprofile.Load(spec.HarnessName(), spec.Overrides)
		if err != nil {
			out.Failure, out.Said = "fatal", err.Error()
			return out, nil
		}
		handed = candidateVariables(key, spec, profile, values)
		out.Hints = candidateHints(key, spec, values)
	}
	ctx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()
	start := time.Now()
	diagnosis, err := m.e.CheckModel(ctx, key, draft, handed, smoke)
	out.Elapsed = time.Since(start)
	if err != nil {
		if errors.Is(err, engine.ErrNoModel) {
			return llmapi.Check{}, llmapi.ErrNoSuchModel
		}
		// AN ENTRY THAT DOES NOT BUILD is the entry's to fix, said at its
		// CLI, as the doctor reports it.
		out.Failure, out.Said = "fatal", err.Error()
		out.Lines = []harness.Check{{Name: harness.CheckBinary, Detail: err.Error(), Kind: "fatal"}}
		return out, nil
	}
	out.Lines = diagnosis.Checks
	for _, line := range out.Lines {
		if !line.OK {
			out.Failure, out.Said = cmp.Or(line.Kind, harness.CheckKindConfig), line.Detail
			break
		}
	}
	out.OK = out.Failure == ""
	if out.Failure == "rate_limit" {
		// A LIMIT PROVES THE CREDENTIAL AUTHENTICATED, which the route
		// passes and says.
		out.Limited = &coord.ModelSitOut{Window: "unknown", Said: out.Said}
	}
	return out, nil
}

// candidateVariables re-keys candidate values from the store's names, which a
// person types, to the variables the entry's CLI is handed each under, which
// the doctor tests: the token's and the key's slots by the profile's
// variables, and an env entry by its own name. A name none of them reads is
// kept, so the doctor reports it rather than vouching for a value nothing
// tried.
func candidateVariables(key string, entry *config.LLMProvider, profile harnessprofile.Profile,
	values map[string]string) map[string]string {

	slot := map[string]string{}
	sources := entry.CredentialSources(key)
	for src, cli := range map[config.CredentialSource]string{
		sources.Token: profile.TokenEnv, sources.APIKey: profile.APIKeyEnv,
	} {
		if name := slotName(src); name != "" && cli != "" {
			slot[name] = cli
		}
	}
	for cli, value := range entry.Env {
		if name, ok := envref.Whole(value); ok {
			slot[name] = cli
		}
	}
	out := make(map[string]string, len(values))
	for name, value := range values {
		out[cmp.Or(slot[name], name)] = value
	}
	return out
}

// slotName is the store name a credential slot reads: its one ${VAR}, or its
// convention.
func slotName(src config.CredentialSource) string {
	if name, ok := envref.Whole(src.Value); ok {
		return name
	}
	if src.Value == "" {
		return src.Fallback
	}
	return ""
}

// candidateHints is each candidate value's hint by the prefixes of the entry
// it is for ([harness.Provider.CredentialHintOf]); none for an entry that
// does not build, whose check says why.
func candidateHints(key string, entry *config.LLMProvider, values map[string]string) map[string]string {
	out := map[string]string{}
	built, err := engine.BuildHarness(key, *entry, config.NewResolver(config.MapSource(values)), nil)
	if err != nil {
		return out
	}
	for name, value := range values {
		out[name] = built.CredentialHintOf(value)
	}
	return out
}

func (m modelEngine) RetryModel(ctx context.Context, key string, mode coord.ModelRetryMode,
	by iam.Actor) (llmapi.Retried, error) {

	r, err := m.e.RetryModel(ctx, key, mode, by)
	if err != nil {
		return llmapi.Retried{}, modelErr(err)
	}
	return llmapi.Retried{Cleared: r.Cleared, Node: r.Node, Still: r.Still, Failed: r.Failed}, nil
}

func (m modelEngine) CheckHostLogin(ctx context.Context, key string) (coord.HostLogin, error) {
	login, err := m.e.CheckHostLogin(ctx, key)
	return login, modelErr(err)
}

func (m modelEngine) StartDeviceSignIn(ctx context.Context, key string,
	by iam.Actor) (coord.DeviceSignIn, error) {

	started, err := m.e.StartDeviceSignIn(ctx, key, by)
	var running *engine.SignInInProgressError
	switch {
	case err == nil:
		return started, nil
	case errors.As(err, &running):
		return coord.DeviceSignIn{}, &llmapi.SignInInProgressError{Running: running.Running}
	case errors.Is(err, engine.ErrNoDeviceSignIn):
		return coord.DeviceSignIn{}, llmapi.ErrNoDeviceSignIn
	case errors.Is(err, engine.ErrSignsInWithoutLogin):
		mode := config.AuthAPIKey
		if c := m.e.Company(); c != nil && c.Config != nil {
			if entry, ok := c.Config.Providers.LLM[key]; ok {
				mode = entry.ResolvedAuthMode()
			}
		}
		return coord.DeviceSignIn{}, &llmapi.SignInModeError{Mode: mode}
	case errors.Is(err, engine.ErrDeviceSignInUnavailable):
		line := strings.TrimPrefix(err.Error(), engine.ErrDeviceSignInUnavailable.Error())
		return coord.DeviceSignIn{}, &llmapi.DeviceSignInUnavailableError{
			Line: strings.TrimSpace(strings.TrimPrefix(line, ":"))}
	}
	return coord.DeviceSignIn{}, modelErr(err)
}

func (m modelEngine) CancelDeviceSignIn(ctx context.Context, id uuid.UUID,
	by iam.Actor) (coord.DeviceSignIn, error) {

	cancelled, err := m.e.CancelDeviceSignIn(ctx, id, by)
	if errors.Is(err, engine.ErrNoSuchSignIn) {
		return coord.DeviceSignIn{}, llmapi.ErrNoSuchSignIn
	}
	return cancelled, modelErr(err)
}

func (m modelEngine) RunningRunsOn(ctx context.Context, keys []string) (int, error) {
	return m.e.RunningRunsOn(ctx, keys)
}

// modelErr maps the engine's refusals that every model route shares.
func modelErr(err error) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, engine.ErrNoModel):
		return llmapi.ErrNoSuchModel
	case errors.Is(err, engine.ErrNotSittingOut):
		return llmapi.ErrNotSittingOut
	case errors.Is(err, engine.ErrNoHostLogin):
		return llmapi.ErrNoHostLogin
	}
	return err
}
