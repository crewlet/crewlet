package cliagent

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
)

// signIn is how one entry's CLI authenticates, as far as this engine can see:
// the credential files on disk, and what the environment it hands the child
// carries.
//
// ONE ANSWER for `crewlet llm doctor` and `crewlet llm list`. Each used to
// decide "logged in" for itself from the configuration — the doctor from the
// files and a resolved token, the list from those plus the auth mode — and
// neither from what the child is actually given, so both drifted, in both
// directions: a hermes key in env and a keyed api-key entry were reported
// as having no login, and a claude-code entry in api-key mode with no key was
// reported healthy on a token that [applyAuth] removes from the child.
type signIn struct {
	// sources is every route that authenticates the child, in the order
	// the report names them; empty means nothing does.
	sources []signInSource

	// unresolved names the credential-shaped env entries, on a profile
	// that reads one ([cliprofile.Profile.EnvSignIn]), whose ${VAR}
	// resolved to nothing — the case where the operator did configure a
	// sign-in and this process cannot see it.
	unresolved []string

	// unreadable is why the shared login could not be read — a write of it
	// that stopped part way and cannot be finished, a file that is not one —
	// so whether credential files sign the CLI in is UNKNOWN rather than no:
	// read as no, an entry with a whole login waiting to be put in place was
	// reported as one nobody had signed in.
	unreadable error
}

// signInSource is one route that authenticates the child.
type signInSource struct {
	// state is the one-word summary `crewlet llm list` prints.
	state string
	// line is what the doctor's sign-in line says about it.
	line string
}

// signIn reads this entry's sign-in off the environment [childEnv] builds.
//
// The checkout is empty because the isolation paths carry no credential:
// whether HOME points here or there does not change which key reaches the
// CLI, and a real checkout would claim a seat for a question about none.
func (p *Provider) signIn(ctx context.Context) signIn {
	login, err := p.ws.Login(ctx)
	return p.signInFrom(login, err)
}

// signInFrom is [Provider.signIn] over a reading of the shared login already
// taken, so the doctor's credentials line and its sign-in line describe ONE
// reading of it rather than two that a write between them could tell apart.
func (p *Provider) signInFrom(login map[string][]byte, loginErr error) signIn {
	var s signIn
	switch {
	case loginErr != nil:
		s.unreadable = loginErr
	case len(login) > 0:
		s.sources = append(s.sources, signInSource{
			state: "credentials",
			line:  "credential files in " + p.ws.CredentialsDir(),
		})
	}

	env := childEnv(p.profile, &Checkout{}, p.env, p.auth)
	inherited := p.auth.Mode == AuthInheritEnv
	if name := p.profile.TokenEnv; name != "" && env[name] != "" {
		source := signInSource{state: "token", line: "headless token in " + name}
		if inherited {
			source = signInSource{state: "inherited token",
				line: name + " from this process's environment (auth.mode inherit-env)"}
		}
		s.sources = append(s.sources, source)
	}
	if name := p.profile.APIKeyEnv; name != "" && env[name] != "" {
		source := signInSource{state: "api key", line: "API key in " + name + " (auth.mode api-key)"}
		if inherited {
			source = signInSource{state: "inherited key",
				line: name + " from this process's environment (auth.mode inherit-env)"}
		}
		s.sources = append(s.sources, source)
	}

	if !p.profile.EnvSignIn {
		return s
	}
	// Only the operator's env, and only a name shaped like a
	// credential: HTTPS_PROXY or a tuning variable there is configuration,
	// not a sign-in. The same predicate passthrough_env and a profile's env
	// are refused by, so this package has one notion of "this name carries
	// a secret". Checked on the child's map rather than on env itself,
	// so a value [applyAuth] removed is never counted.
	for _, name := range slices.Sorted(maps.Keys(p.env)) {
		if name == p.profile.TokenEnv || name == p.profile.APIKeyEnv || !cliprofile.IsCredentialName(name) {
			continue
		}
		if strings.TrimSpace(env[name]) == "" {
			s.unresolved = append(s.unresolved, name)
			continue
		}
		s.sources = append(s.sources, signInSource{state: "environment", line: "env sets " + name})
	}
	return s
}

// ModeSignIn is the credential an entry's auth mode hands its CLI from the
// environment, for an entry under api-key or inherit-env: what the mode reads,
// and how an operator supplies it.
//
// WORDED ONCE, for every message that tells an operator how to sign such an
// entry in — the doctor's "no sign-in", a remote coding run's refusal and
// `crewlet llm login`'s refusal of a token the mode never reads. Each used to
// choose its remedy for itself, and two of them chose it by the CLI's profile
// alone: an api-key or inherit-env Claude Code entry was told to mint a
// headless token, which api-key removes from the CLI's environment and
// inherit-env never looks for in the secret store the token is stored in — and
// the command that mints one stored it anyway and printed an auth.token snippet
// that validation refuses under either mode.
//
// A subscription entry has none: its credentials are a login, which the login
// commands write, and a headless token, which the mode reads from the secret
// store — each message words those for its own setting, since what travels to
// a remote box differs from what a host-side call can read.
type ModeSignIn struct {
	// Mode is the entry's auth mode, api-key or inherit-env.
	Mode AuthMode
	// Variables are the ones the mode hands the CLI — api-key's key
	// variable, the one or two inherit-env forwards — for a message that
	// names where else one may be set (a seat's role.sandbox.env). Empty
	// where the profile names none for the mode.
	Variables []string
	// Reads completes "its auth.mode is <Mode>, which …": what the mode
	// hands the CLI, and from where.
	Reads string
	// Unset completes the same sentence after Reads, for an entry where
	// none of that resolved.
	Unset string
	// Supply is how to provide it, as an imperative.
	Supply string
}

// Missing is the clause for an entry whose mode's credential resolved to
// nothing, naming the mode, what it reads and how to supply it.
func (m ModeSignIn) Missing() string {
	return fmt.Sprintf("its auth.mode is %s, which %s, %s: %s", m.Mode, m.Reads, m.Unset, m.Supply)
}

// ModeSignIn is this entry's [ModeSignIn], or false under subscription.
//
// THE VARIABLES ARE THE PROFILE'S, as [applyAuth] uses them. An entry whose
// profile names none for its mode — api-key on a CLI with no api_key_env,
// inherit-env on one with neither variable — is refused on every write of the
// configuration and only warned about where a revision is applied, so it can
// run; its mode then hands the CLI nothing at all, and the remedy is the mode.
func (p *Provider) ModeSignIn() (ModeSignIn, bool) {
	// No Variables: none would reach the CLI.
	misconfigured := func(mode AuthMode, reads string) ModeSignIn {
		return ModeSignIn{
			Mode: mode, Reads: reads,
			Unset:  "a pairing every write of the configuration refuses",
			Supply: "set auth.mode: subscription, and sign the CLI in with a login",
		}
	}
	switch p.AuthMode() {
	case AuthAPIKey:
		if p.profile.APIKeyEnv == "" {
			return misconfigured(AuthAPIKey, fmt.Sprintf(
				"puts its key in no variable, since the %q profile names no api_key_env", p.agent)), true
		}
		return ModeSignIn{
			Mode: AuthAPIKey, Variables: []string{p.profile.APIKeyEnv},
			Reads: fmt.Sprintf("hands the CLI only its key (providers.llm.%s.auth.api_key, else %s)",
				p.key, p.profile.APIKeyEnv),
			Unset:  "and that key resolved to nothing",
			Supply: "store it with `crewlet secrets set`, or export it in the engine's environment",
		}, true
	case AuthInheritEnv:
		forwarded := make([]string, 0, 2)
		for _, name := range []string{p.profile.TokenEnv, p.profile.APIKeyEnv} {
			if name != "" {
				forwarded = append(forwarded, name)
			}
		}
		if len(forwarded) == 0 {
			return misconfigured(AuthInheritEnv, fmt.Sprintf(
				"forwards nothing, since the %q profile names neither a token_env nor an api_key_env", p.agent)), true
		}
		sign := ModeSignIn{
			Mode: AuthInheritEnv, Variables: forwarded,
			Reads: fmt.Sprintf("hands the CLI only %s from the engine's own process environment, "+
				"never from the secret store", strings.Join(forwarded, " or ")),
			Unset:  "and it is not set there",
			Supply: "export it in the engine's environment",
		}
		if len(forwarded) > 1 {
			sign.Unset, sign.Supply = "and neither is set there", "export one in the engine's environment"
		}
		return sign, true
	}
	return ModeSignIn{}, false
}

// line is the doctor's sign-in line.
func (s signIn) line() string {
	if len(s.sources) == 0 {
		if s.unreadable != nil {
			return "unknown — the shared login could not be read"
		}
		return "none"
	}
	lines := make([]string, len(s.sources))
	for i, source := range s.sources {
		lines[i] = source.line
	}
	return strings.Join(lines, "; ")
}

// state is the one-word summary of the first route, or "none" — "unknown"
// where nothing else signs the CLI in and the shared login could not be read.
func (s signIn) state() string {
	if len(s.sources) == 0 {
		if s.unreadable != nil {
			return "unknown"
		}
		return "none"
	}
	return s.sources[0].state
}
