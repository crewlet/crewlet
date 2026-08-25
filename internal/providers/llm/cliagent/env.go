package cliagent

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// hostAllowlist is the engine environment a child ALWAYS inherits.
//
// An allowlist rather than os.Environ() because the engine's environment
// holds the org's chat token, its database DSN and every provider key: a
// child that inherited it would hand each of those to a vendor's CLI, and
// would silently bill a metered ANTHROPIC_API_KEY that happened to be
// exported while the operator believed they were on a flat-rate plan.
//
// What survives is the four things a process genuinely cannot run without:
// where to find binaries, how to render text, whom to trust for TLS, and how
// to reach the network. Anything else a CLI needs is declared — in the
// profile's passthrough_env, or the operator's cli.env.
var hostAllowlist = []string{
	"PATH",
	"LANG", "LANGUAGE", "LC_ALL", "LC_CTYPE", "TZ",
	"SSL_CERT_FILE", "SSL_CERT_DIR", "CURL_CA_BUNDLE", "REQUESTS_CA_BUNDLE",
	"NODE_EXTRA_CA_CERTS",
	"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY",
	"http_proxy", "https_proxy", "no_proxy",
}

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
	// APIKey is a resolved metered key, used only under AuthAPIKey.
	APIKey string
}

// buildEnv is the complete environment for one call, as KEY=VALUE.
//
// Layered lowest-precedence first, so the reason each layer can override the
// one below is visible in the order: host allowlist, then the isolation
// variables (which nothing may override — they ARE the isolation), then the
// profile's own fixed environment, then the profile's passthrough, then the
// operator's cli.env, then auth.
func buildEnv(p Profile, c *Checkout, extra map[string]string, auth Auth) []string {
	env := map[string]string{}

	for _, name := range hostAllowlist {
		if value, ok := os.LookupEnv(name); ok {
			env[name] = value
		}
	}

	// The isolation. Every path a CLI could keep state under is pointed
	// inside the seat's own home; XDG_CACHE_HOME is the one exception,
	// living beside the home rather than in it, because a cache is warm
	// across calls and holds no conversation.
	env["HOME"] = c.Home
	env["XDG_CONFIG_HOME"] = filepath.Join(c.Home, ".config")
	env["XDG_DATA_HOME"] = filepath.Join(c.Home, ".local", "share")
	env["XDG_STATE_HOME"] = filepath.Join(c.Home, ".local", "state")
	env["XDG_CACHE_HOME"] = c.Cache
	env["TMPDIR"] = filepath.Join(c.Home, "tmp")
	for name, rel := range p.ConfigEnv {
		env[name] = filepath.Join(c.Home, rel)
	}

	for name, value := range p.Env {
		env[name] = value
	}

	// Forwarded BEFORE auth is consulted, which is exactly why a profile
	// may not name a credential here — see Profile.validate.
	for _, name := range p.PassthroughEnv {
		if value, ok := os.LookupEnv(name); ok {
			env[name] = value
		}
	}

	for name, value := range extra {
		env[name] = value
	}

	applyAuth(env, p, auth)

	out := make([]string, 0, len(env))
	for name, value := range env {
		out = append(out, name+"="+value)
	}
	// Sorted so a failing invocation is reproducible from a log line and
	// so tests compare an environment rather than a map iteration order.
	sort.Strings(out)
	return out
}

// applyAuth puts the credential the mode asks for into the child environment,
// and removes the one it does not.
//
// The removal is the load-bearing half. A subscription call that left an
// ANTHROPIC_API_KEY in place — inherited from the profile's env, or written
// by an operator into cli.env — would bill the metered account silently while
// the plan sat unused, which is the failure the default mode exists to
// prevent.
func applyAuth(env map[string]string, p Profile, auth Auth) {
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

// TokenVarName is the environment variable a profile's headless token lives
// in, for the messages `crewlet llm login` and `doctor` print.
func TokenVarName(p Profile) (string, error) {
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
