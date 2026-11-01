package harness

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
	"github.com/crewlet/crewlet/internal/words"
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
	// that reads one ([harnessprofile.Profile.EnvSignIn]), whose ${VAR}
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
	case p.signsInWithKey():
		// Not for a call that signs in with a key, which runs where no login
		// is seeded ([Workspace.AcquireForKey]): neither the login nor one
		// that cannot be read is any part of its sign-in.
	case loginErr != nil:
		s.unreadable = loginErr
	case len(p.ws.loginIn(login)) > 0:
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

	if p.auth.Mode == AuthEnv {
		// The route an env-mode entry signs in by, read off the child's map
		// like every other: what the operator's env sets of the names the
		// profile lists, as the CLI is handed them.
		for _, name := range p.profile.SignsInWithEnv(env) {
			s.sources = append(s.sources, signInSource{state: "env", line: "env sets " + name})
		}
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
		if name == p.profile.TokenEnv || name == p.profile.APIKeyEnv || !harnessprofile.IsCredentialName(name) {
			continue
		}
		if strings.TrimSpace(env[name]) == "" {
			s.unresolved = append(s.unresolved, name)
			continue
		}
		// ONE WORD PER MODE: under auth.mode env the entry's env is the
		// sign-in whatever CLI reads it, so it reads env as Claude Code's
		// does; under any other mode it is the provider key the CLI reads
		// from its environment.
		state := "environment"
		if p.auth.Mode == AuthEnv {
			state = "env"
		}
		s.sources = append(s.sources, signInSource{state: state, line: "env sets " + name})
	}
	return s
}

// SignInRoute is how an entry's CLI signs in, in the words a status read
// publishes: a closed set, so a reader that draws one (the Models screen) can
// be held to it.
type SignInRoute string

// The routes, in the order [Provider.signIn] reads them.
const (
	// RouteCredentials is a login the CLI keeps in credential files.
	RouteCredentials SignInRoute = "credentials"
	// RouteToken is a headless subscription token.
	RouteToken SignInRoute = "token"
	// RouteAPIKey is a metered API key (auth.mode api-key).
	RouteAPIKey SignInRoute = "api-key"
	// RouteInheritEnv is a token or key forwarded from the engine's own
	// environment (auth.mode inherit-env).
	RouteInheritEnv SignInRoute = "inherit-env"
	// RouteEnv is the entry's env alone, a cloud provider's switch or a
	// gateway's token (auth.mode env).
	RouteEnv SignInRoute = "env"
	// RouteEnvironment is a provider key in the entry's env, on a CLI that
	// reads one ([harnessprofile.Profile.EnvSignIn]).
	RouteEnvironment SignInRoute = "environment"
	// RouteNone is nothing this node can see signing the CLI in.
	RouteNone SignInRoute = "none"
)

// SignInRoutes is the closed set.
var SignInRoutes = []SignInRoute{RouteCredentials, RouteToken, RouteAPIKey, RouteInheritEnv,
	RouteEnv, RouteEnvironment, RouteNone}

// Valid reports whether r is one of [SignInRoutes].
func (r SignInRoute) Valid() bool { return slices.Contains(SignInRoutes, r) }

// SignInRoute is how this entry's CLI signs in as THIS node resolves it, and
// whether anything does. The first route of [Provider.signIn], the reading
// `crewlet llm list` prints, so the page and the command cannot disagree; a
// shared login that cannot be read reads as [RouteNone], with resolves false.
func (p *Provider) SignInRoute(ctx context.Context) (route SignInRoute, resolves bool) {
	s := p.signIn(ctx)
	if len(s.sources) == 0 {
		return RouteNone, false
	}
	switch s.sources[0].state {
	case "token":
		return RouteToken, true
	case "credentials":
		return RouteCredentials, true
	case "api key":
		return RouteAPIKey, true
	case "inherited token", "inherited key":
		return RouteInheritEnv, true
	case "env":
		return RouteEnv, true
	default:
		return RouteEnvironment, true
	}
}

// SignsInWithLogin reports whether this entry signs its CLI in with a login
// kept in credential files, which only the CLI can say the state of: no key
// and no env sign-in, no token reaching the CLI, and a profile that can ask the
// CLI who it is logged in as ([harnessprofile.Profile.StatusArgs]).
func (p *Provider) SignsInWithLogin() bool {
	if p.signsInWithKey() || len(p.profile.StatusArgs) == 0 {
		return false
	}
	return len(p.handedCredentials()) == 0
}

// signsInWithKey reports whether this entry's calls sign the CLI in with
// something other than a login: every call under api-key and env, neither of
// which ever signs in with one, and a call under inherit-env whose engine
// environment holds the key variable. Such a call runs in the seat's key home
// ([Workspace.AcquireForKey]), where no login is seeded: a login there would
// compete with the key or the cloud provider the entry chose.
//
// Read off [childEnv], as [Provider.signIn] is, so the home a call runs in and
// the doctor's account of its sign-in are one reading of one environment.
func (p *Provider) signsInWithKey() bool {
	if p.auth.Mode == AuthAPIKey || p.auth.Mode == AuthEnv {
		return true
	}
	if p.auth.Mode != AuthInheritEnv || p.profile.APIKeyEnv == "" {
		return false
	}
	env := childEnv(p.profile, &Checkout{}, p.env, p.auth)
	return strings.TrimSpace(env[p.profile.APIKeyEnv]) != ""
}

// ModeSignIn is the credential an entry's auth mode hands its CLI from the
// environment, for an entry under api-key or inherit-env: what the mode reads,
// and how an operator supplies it.
//
// WORDED ONCE, for every message that tells an operator how to sign such an
// entry in — the doctor's "no sign-in", a remote launch's refusal,
// `crewlet llm login`'s refusal of a token the mode never reads, and its
// `-username` refusal of a CLI with no credential login
// ([Provider.CredentialLogin]). Each used to choose its remedy for itself, and
// three of them chose it by the CLI's profile alone: an api-key or inherit-env
// Claude Code entry was told to mint a headless token, which api-key removes
// from the CLI's environment and inherit-env never looks for in the secret
// store the token is stored in — and the command that mints one stored it
// anyway and printed an auth.token snippet that validation refuses under
// either mode.
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
	// where the profile names none for the mode, or none in the reference
	// grammar.
	Variables []string
	// Misnamed is the overrides field, in full, of each variable the mode
	// hands the CLI that is outside the reference grammar. Such a variable
	// is left out of Variables and of Supply, since no store, ${VAR} or
	// shell export can hold a credential under it ([misnamedVariable]);
	// where it is all the mode has, Supply names the field instead.
	Misnamed []string
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
//
// A VARIABLE OUTSIDE THE REFERENCE GRAMMAR IS NO ROUTE ([misnamedVariable]): no
// secret store or ${VAR} can hold the name and no shell can export it, so a key
// stored with `crewlet secrets set` or exported never reaches the CLI under it.
// It is left out of what the operator is told to supply, and where it is the
// only variable the mode has, the remedy is the overrides field that names it —
// never a seat's role.sandbox.env: every write of the company document refuses
// such a name, so the field is the change to make either way, and a name
// carrying `=` sets a different variable wherever it is placed.
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
	case AuthEnv:
		names := p.profile.SignInEnv
		if len(names) == 0 {
			return ModeSignIn{
				Mode: AuthEnv, Reads: "hands the CLI only its env's credential-named variables",
				Unset:  "and its env sets none that resolves",
				Supply: fmt.Sprintf("set the provider's key in providers.llm.%s.env", p.key),
			}, true
		}
		return ModeSignIn{
			Mode: AuthEnv, Variables: slices.Clone(names),
			Reads: fmt.Sprintf("signs the CLI in with its env alone (one of %s)",
				words.Or(names)),
			Unset: "and its env sets none of them",
			Supply: fmt.Sprintf("set one in providers.llm.%s.env, with the cloud provider's "+
				"credentials or the gateway's address beside it", p.key),
		}, true
	case AuthAPIKey:
		name := p.profile.APIKeyEnv
		switch {
		case name == "":
			return misconfigured(AuthAPIKey, fmt.Sprintf(
				"puts its key in no variable, since the %q profile names no api_key_env", p.harness)), true
		case !envref.ValidName(name):
			// The key's one variable is a name no store, ${VAR} or export
			// can hold, so the remedy is the name, and no route to a key is
			// offered: each would hand it over under that variable.
			path := p.overridePath("api_key_env")
			return ModeSignIn{
				Mode: AuthAPIKey, Misnamed: []string{path},
				Reads: fmt.Sprintf("hands the CLI only its key, in %q (%s)", name, path),
				Unset: "a name outside the reference grammar that no secret store, ${VAR} or " +
					"shell export can hold",
				Supply: fmt.Sprintf("name the variable your build of the CLI reads in %s", path),
			}, true
		}
		if p.profile.APIKeyEnvShared {
			// No "else": other vendors' keys are kept under this
			// variable too, so the engine never reads it in the entry's
			// place ([harnessprofile.Profile.APIKeyEnvShared]).
			return ModeSignIn{
				Mode: AuthAPIKey, Variables: []string{name},
				Reads: fmt.Sprintf("hands the CLI only its key, as %s (providers.llm.%s.auth.api_key)",
					name, p.key),
				Unset:  "and that key resolved to nothing",
				Supply: "store it with `crewlet secrets set` under the name auth.api_key references",
			}, true
		}
		return ModeSignIn{
			Mode: AuthAPIKey, Variables: []string{name},
			Reads: fmt.Sprintf("hands the CLI only its key (providers.llm.%s.auth.api_key, else %s)",
				p.key, name),
			Unset:  "and that key resolved to nothing",
			Supply: "store it with `crewlet secrets set`, or export it in the engine's environment",
		}, true
	case AuthInheritEnv:
		var forwarded, held, misnamed, misnamedNames []string
		for _, variable := range []struct{ field, name string }{
			{"token_env", p.profile.TokenEnv}, {"api_key_env", p.profile.APIKeyEnv},
		} {
			switch {
			case variable.name == "":
				continue
			case envref.ValidName(variable.name):
				held = append(held, variable.name)
			default:
				misnamed = append(misnamed, p.overridePath(variable.field))
				misnamedNames = append(misnamedNames, strconv.Quote(variable.name))
			}
			forwarded = append(forwarded, variable.name)
		}
		if len(forwarded) == 0 {
			return misconfigured(AuthInheritEnv, fmt.Sprintf(
				"forwards nothing, since the %q profile names neither a token_env nor an api_key_env", p.harness)), true
		}
		named := make([]words.Said, len(forwarded))
		for i, name := range forwarded {
			named[i] = words.Said(name)
		}
		sign := ModeSignIn{
			Mode: AuthInheritEnv, Variables: held, Misnamed: misnamed,
			Reads: fmt.Sprintf("hands the CLI only %s from the engine's own process environment, "+
				"never from the secret store", words.Or(named)),
			Unset:  "and it is not set there",
			Supply: "export it in the engine's environment",
		}
		if len(forwarded) > 1 {
			sign.Unset, sign.Supply = "and neither is set there", "export one in the engine's environment"
		}
		// Forwarded all the same — the engine's process environment is not
		// a shell's, and the mode hands over what it finds — but never
		// offered: no shell can export the name.
		switch {
		case len(misnamed) == 0:
		case len(held) == 0:
			what := "the variable"
			if len(misnamed) > 1 {
				what = "the variables"
			}
			fields, names := make([]words.Said, len(misnamed)), make([]words.Said, len(misnamed))
			for i := range misnamed {
				fields[i], names[i] = words.Said(misnamed[i]), words.Said(misnamedNames[i])
			}
			sign.Supply = fmt.Sprintf("name %s your build of the CLI reads in %s, since no shell "+
				"can export %s", what, words.And(fields), words.Or(names))
		default:
			sign.Supply = fmt.Sprintf("export %s in the engine's environment, since no shell can "+
				"export %s (%s)", held[0], misnamedNames[0], misnamed[0])
		}
		return sign, true
	}
	return ModeSignIn{}, false
}

// misnamedVariable is a credential variable an entry's merged profile names
// outside the reference grammar: `MY-TOKEN`, `MY=KEY`.
//
// NOTHING THAT KEEPS A CREDENTIAL CAN HOLD ONE. The secret store refuses the
// name, so `crewlet llm login -capture-token` would have nowhere to put a token
// the vendor had just minted; a ${VAR} cannot spell it; no shell exports one;
// and a name carrying `=` sets a different variable in the CLI's environment.
// Only overrides can name one — every shipped profile names its variables in
// the grammar — and every write of the company document refuses it, but that
// is an admission rule: an entry loaded to run, a revision being applied or the
// file `crewlet llm` reads, still carries it. So every route this package words
// for an operator leaves such a variable out, and the doctor names its field:
// a route through it is advice nobody can follow. The grammar is envref's, the
// one the store and the resolver hold every name to.
type misnamedVariable struct {
	// field is the overrides key that names it: token_env or api_key_env.
	field string
	// name is the variable as the field writes it.
	name string
	// credential is what the CLI is handed in it.
	credential string
}

// misnamedVariables is each credential variable this entry's merged profile
// names outside the reference grammar, the token's first.
func (p *Provider) misnamedVariables() []misnamedVariable {
	var out []misnamedVariable
	for _, variable := range []misnamedVariable{
		{field: "token_env", name: p.profile.TokenEnv, credential: "headless token"},
		{field: "api_key_env", name: p.profile.APIKeyEnv, credential: "API key"},
	} {
		if variable.name != "" && !envref.ValidName(variable.name) {
			out = append(out, variable)
		}
	}
	return out
}

// misnamedProblem is the doctor's problem for one [misnamedVariable], naming
// the field to change.
func (p *Provider) misnamedProblem(variable misnamedVariable) string {
	problem := fmt.Sprintf("%s is %q, which is not an environment variable name (letters, "+
		"digits and underscores, not starting with a digit): the CLI is handed its %s under "+
		"it, and no secret store, ${VAR} or shell export can hold a name outside that grammar, "+
		"so neither a stored value nor an export can be read under it", p.overridePath(variable.field),
		variable.name, variable.credential)
	if variable.field == "token_env" {
		problem += ", and a token `crewlet llm login -capture-token` minted for it could be " +
			"neither stored nor referenced"
	}
	return problem + ". Every write of the company document refuses it: name the variable " +
		"your build of the CLI reads"
}

// overridePath is the full path of one of this entry's overrides fields.
func (p *Provider) overridePath(field string) string {
	return fmt.Sprintf("providers.llm.%s.overrides.%s", p.key, field)
}

// TokenCanBeStored reports whether this CLI names a headless token variable a
// token can be kept under: one in the reference grammar, which a secret store,
// a ${VAR} and a shell export can all hold ([misnamedVariable]). A CLI that
// names none takes no headless token, and one that names a variable outside
// the grammar can be handed none, so no message this package words offers
// `crewlet llm login -capture-token` for either ([Provider.MintsHeadlessToken]).
func (p *Provider) TokenCanBeStored() bool {
	return p.profile.TokenEnv != "" && envref.ValidName(p.profile.TokenEnv)
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
