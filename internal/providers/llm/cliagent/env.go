package cliagent

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/providers/llm/cliagent/cliprofile"
)

// AuthMode is how a call authenticates. It mirrors config.CLIAgentAuthMode,
// declared here so this package does not import config — the backend is
// constructed from resolved values, not from a config document.
type AuthMode string

// The three modes. Subscription is the default and the point of the backend.
const (
	AuthSubscription AuthMode = "subscription"
	AuthAPIKey       AuthMode = "api-key"
	AuthInheritEnv   AuthMode = "inherit-env"
)

// Auth is one provider's resolved credential material.
type Auth struct {
	Mode AuthMode
	// Token is a resolved long-lived subscription token, or empty when
	// the login lives in the credential files instead.
	Token string
	// TokenConfigured reports that the entry NAMED a token (cli.auth.token),
	// whatever it resolved to. Only the doctor reads it: an empty Token is
	// either "this entry signs in some other way" or "the reference it
	// wrote resolved to nothing", and only the second is worth telling the
	// operator about by name.
	TokenConfigured bool
	// APIKey is a resolved metered key, used only under AuthAPIKey.
	APIKey string
}

// buildEnv is the complete environment for one call, as KEY=VALUE.
func buildEnv(p cliprofile.Profile, c *Checkout, extra map[string]string, auth Auth) []string {
	env := childEnv(p, c, extra, auth)
	out := make([]string, 0, len(env))
	for name, value := range env {
		out = append(out, name+"="+value)
	}
	// Sorted so a failing invocation is reproducible from a log line and
	// so tests compare an environment rather than a map iteration order.
	slices.Sort(out)
	return out
}

// childEnv is the environment a child is given, by name.
//
// IT STARTS FROM AN ALLOWLIST, never os.Environ(): the engine's environment
// holds the org's chat token, the keyring that decrypts its secret store and
// every provider key, and a child that inherited it would hand each of those
// to a vendor's CLI — and would silently bill a metered ANTHROPIC_API_KEY that
// happened to be exported while the operator believed they were on a
// flat-rate plan. The list is [hostbox.PassthroughEnv], the one a local
// sandbox box starts from too: this package kept a copy of its own, and the
// two drifted (see that list's doc). Anything else a CLI needs is declared —
// in the profile's passthrough_env, or the operator's cli.env.
//
// Layered lowest-precedence first, so the reason each layer can override the
// one below is visible in the order: host allowlist, then the profile's own
// fixed environment, then the profile's passthrough, then the operator's
// cli.env, then auth — and THE ISOLATION LAST.
//
// Last because it is the one layer nothing may override: it IS the isolation
// ([cliprofile.Profile.SeatEnv]). It used to come second, under the comment
// that nothing may override it, with three layers an operator writes applied
// after it — so a cli.env HOME, or a passthrough_env forwarding the host's,
// put every seat on the entry in one home and each read the others'
// transcripts. Every variable the engine owns ([cliprofile.Profile.EngineEnv])
// is first REMOVED from what the layers below assembled, and the isolation
// then set: a call's per-call variables — the system prompt's path — are
// added by [Provider.Complete] on the call that has one, so a value a lower
// layer wrote never reaches a call that has none. Config refuses a document
// naming one in any layer, so this is the guarantee behind the refusal rather
// than the only thing standing between a typo and a shared home.
//
// A map of its own, apart from [buildEnv]'s rendering, because it is also
// what decides whether an entry is signed in ([Provider.signIn]): whether a
// key reaches the CLI is a fact about this map after [applyAuth] has run,
// and a second reading of the configuration beside it is what the doctor
// had, and what drifted from it in both directions.
func childEnv(p cliprofile.Profile, c *Checkout, extra map[string]string, auth Auth) map[string]string {
	env := hostbox.Inherit()

	for name, value := range p.Env {
		env[name] = value
	}

	// Forwarded BEFORE auth is consulted, which is exactly why a profile
	// may not name a credential here — see
	// cliprofile.Profile.ValidateCredentials.
	for _, name := range p.PassthroughEnv {
		if value, ok := os.LookupEnv(name); ok {
			env[name] = value
		}
	}

	for name, value := range extra {
		env[name] = value
	}

	applyAuth(env, p, auth)

	for _, name := range p.EngineEnv() {
		delete(env, name)
	}
	maps.Copy(env, p.SeatEnv(c.Home, c.Cache))
	return env
}

// applyAuth puts the credential the mode asks for into the child environment,
// and removes the one it does not.
//
// The removal is the load-bearing half. A subscription call that left an
// ANTHROPIC_API_KEY in place — inherited from the profile's env, or written
// by an operator into cli.env — would bill the metered account silently while
// the plan sat unused, which is the failure the default mode exists to
// prevent.
func applyAuth(env map[string]string, p cliprofile.Profile, auth Auth) {
	switch auth.Mode {
	case AuthAPIKey:
		if p.APIKeyEnv != "" && auth.APIKey != "" {
			env[p.APIKeyEnv] = auth.APIKey
		}
		if p.TokenEnv != "" {
			delete(env, p.TokenEnv)
		}
	case AuthInheritEnv:
		// The deliberate escape hatch: whichever of the two the ENGINE's
		// own environment holds is forwarded. The only mode where a host
		// credential reaches a child, and it has to be asked for by name.
		for _, name := range []string{p.TokenEnv, p.APIKeyEnv} {
			if name == "" {
				continue
			}
			if value, ok := os.LookupEnv(name); ok {
				env[name] = value
			}
		}
	default:
		if p.TokenEnv != "" && auth.Token != "" {
			env[p.TokenEnv] = auth.Token
		}
		if p.APIKeyEnv != "" {
			delete(env, p.APIKeyEnv)
		}
	}
}

// probeEnv is the child environment for an invocation that belongs to no seat
// — today the version probe `crewlet llm doctor` runs.
//
// IT EXISTS BECAUSE A MISSING ENV IS NOT AN EMPTY ONE. [run] assigns whatever
// it is given to exec.Cmd.Env, and a nil there does not mean "no environment":
// os/exec then hands the child the ENGINE's own, which is the company's chat
// token, its secret store's keyring and every provider key — precisely what
// the allowlist in [buildEnv] exists to keep out of a vendor's CLI. The probe was
// the one call in this package that skipped it, so `--version` ran with more
// access than any real completion ever gets.
//
// It also makes the profile's own env apply, which the muse-code profile
// depends on: its binary is a LAUNCHER that checks for an update every hour
// and forks the download, and MUSE_NO_AUTO_UPDATE=1 is only a setting if the
// child that would act on it receives it.
//
// A home of its own, beside the login home rather than inside a seat's: a
// seat home is pruned between calls, and a probe has no seat.
func (p *Provider) probeEnv() []string {
	home := filepath.Join(p.ws.Root(), "probe-home")
	cache := filepath.Join(p.ws.Root(), "probe-cache")
	for _, dir := range []string{home, cache} {
		// Best effort: a CLI that cannot write its config still prints
		// its version, and a probe that refused to run over a mkdir
		// would report the drift it exists to detect as an absence.
		_ = os.MkdirAll(dir, 0o700)
	}
	return buildEnv(p.profile, &Checkout{Home: home, Cache: cache}, p.env, p.auth)
}

// TokenVarName is the environment variable a profile's headless token lives
// in, for the messages `crewlet llm login` and `doctor` print.
func TokenVarName(p cliprofile.Profile) (string, error) {
	if p.TokenEnv == "" {
		return "", fmt.Errorf("this CLI mints no headless token")
	}
	return p.TokenEnv, nil
}

// BundleVarName is the conventional secret name a provider's exported
// credential bundle lives under.
//
// A CONVENTION rather than a required setting, so `crewlet llm export -secret-store`
// and the engine's boot-time restore agree without the operator wiring
// cli.auth.credential_bundle by hand — and one function, so the writer and
// the reader cannot derive it differently.
func BundleVarName(key string) string {
	var b strings.Builder
	b.WriteString("CREWLET_LLM_CLI_")
	for _, r := range strings.ToUpper(key) {
		switch {
		case r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	b.WriteString("_CREDENTIALS")
	return b.String()
}
