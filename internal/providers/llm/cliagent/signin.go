package cliagent

import (
	"context"
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
// directions: a hermes key in cli.env and a keyed api-key entry were reported
// as having no login, and a claude-code entry in api-key mode with no key was
// reported healthy on a token that [applyAuth] removes from the child.
type signIn struct {
	// sources is every route that authenticates the child, in the order
	// the report names them; empty means nothing does.
	sources []signInSource

	// unresolved names the credential-shaped cli.env entries, on a profile
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
	// Only the operator's cli.env, and only a name shaped like a
	// credential: HTTPS_PROXY or a tuning variable there is configuration,
	// not a sign-in. The same predicate passthrough_env and a profile's env
	// are refused by, so this package has one notion of "this name carries
	// a secret". Checked on the child's map rather than on cli.env itself,
	// so a value [applyAuth] removed is never counted.
	for _, name := range slices.Sorted(maps.Keys(p.env)) {
		if name == p.profile.TokenEnv || name == p.profile.APIKeyEnv || !cliprofile.IsCredentialName(name) {
			continue
		}
		if strings.TrimSpace(env[name]) == "" {
			s.unresolved = append(s.unresolved, name)
			continue
		}
		s.sources = append(s.sources, signInSource{state: "environment", line: "cli.env sets " + name})
	}
	return s
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
