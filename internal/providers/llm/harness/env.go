package harness

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/crewlet/crewlet/internal/hostbox"
	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// AuthMode is how a call authenticates. It mirrors config.HarnessAuthMode,
// declared here so this package does not import config — the backend is
// constructed from resolved values, not from a config document.
type AuthMode string

// The four modes. Subscription is the default and the point of the backend.
const (
	AuthSubscription AuthMode = "subscription"
	AuthAPIKey       AuthMode = "api-key"
	AuthInheritEnv   AuthMode = "inherit-env"
	// AuthEnv is an entry whose own env signs the harness in: a cloud
	// provider's switch and credentials, or a gateway's address and token
	// ([harnessprofile.Profile.SignInEnv]). Neither the token nor the key
	// reaches the harness, and no login is seeded.
	AuthEnv AuthMode = "env"
)

// Auth is one provider's resolved credential material.
type Auth struct {
	Mode AuthMode
	// Token is a resolved long-lived subscription token, or empty when
	// the login lives in the credential files instead.
	Token string
	// TokenConfigured reports that the entry NAMED a token (auth.token),
	// whatever it resolved to. Only the doctor reads it: an empty Token is
	// either "this entry signs in some other way" or "the reference it
	// wrote resolved to nothing", and only the second is worth telling the
	// operator about by name.
	TokenConfigured bool
	// APIKey is a resolved metered key, used only under AuthAPIKey.
	APIKey string
}

// buildEnv is the complete environment for one call, as KEY=VALUE.
//
// Rendered by [hostbox.Environ], the one renderer every child the engine
// starts goes through: sorted, so a failing invocation is reproducible from a
// log line and tests compare an environment rather than a map iteration
// order, and NEVER nil, because os/exec reads a nil Cmd.Env as the engine's
// own environment.
func buildEnv(p harnessprofile.Profile, c *Checkout, extra map[string]string, auth Auth) []string {
	return hostbox.Environ(childEnv(p, c, extra, auth))
}

// childEnv is the environment a child is given, by name.
//
// Layered lowest-precedence first, so the reason each layer can override the
// one below is visible in the order: the host allowlist every child gets
// ([hostbox.Inherit]), then the isolation variables, then the profile's own
// fixed environment, then the profile's passthrough, then the operator's
// env, then auth. The isolation is NOT protected by its place in that
// order — every layer above it could replace it — but by validation: a
// profile whose env or passthrough_env names one of
// [harnessprofile.Profile.IsolationVariables] does not load
// ([harnessprofile.Profile.Validate]), and an entry whose env names one is
// refused on every write of the configuration and warned about on every node
// that applies it (config's admission rules, which never take a node off the
// fleet's revision over an entry that still runs).
//
// A map of its own, apart from [buildEnv]'s rendering, because it is also
// what decides whether an entry is signed in ([Provider.signIn]): whether a
// key reaches the harness is a fact about this map after [applyAuth] has run,
// and a second reading of the configuration beside it is what the doctor
// had, and what drifted from it in both directions.
func childEnv(p harnessprofile.Profile, c *Checkout, extra map[string]string, auth Auth) map[string]string {
	// THE HOST ALLOWLIST IS [hostbox]'s, the one every child the engine
	// starts is handed, and not a copy kept here. An allowlist rather than
	// os.Environ() because the engine's environment holds its keyring, its
	// Tier A tokens, the org's chat token, its collector credential and every
	// provider key: a child that inherited it would hand each of those to a vendor's
	// harness, and would silently bill a metered ANTHROPIC_API_KEY that happened
	// to be exported while the operator believed they were on a flat-rate
	// plan. This package kept its own list once, and it had drifted from the
	// sandbox's by five names. Anything else a harness needs is declared — in the
	// profile's passthrough_env, or the operator's env.
	env := hostbox.Inherit()

	// The isolation. Every path a harness could keep state under is pointed
	// inside the seat's own home; XDG_CACHE_HOME is the one exception,
	// living beside the home rather than in it, because a cache is warm
	// across calls and holds no conversation.
	env["HOME"] = c.Home
	env["XDG_CONFIG_HOME"] = filepath.Join(c.Home, ".config")
	env["XDG_DATA_HOME"] = filepath.Join(c.Home, ".local", "share")
	env["XDG_STATE_HOME"] = filepath.Join(c.Home, ".local", "state")
	env["XDG_CACHE_HOME"] = c.Cache
	env["TMPDIR"] = tempDir(c.Home)
	for name, rel := range p.ConfigEnv {
		env[name] = filepath.Join(c.Home, rel)
	}
	// ONLY FOR A CALL, which is the one kind of command with a working
	// directory of its own — see [harnessprofile.Profile.WorkConfigEnv]. A
	// command run in a home (a credential store's import and export, a login,
	// the version probe) is handed no Work, and pointing the variable at the
	// home instead would put per-call configuration where every concurrent
	// call of the seat reads it.
	if c.Work != "" {
		for name, rel := range p.WorkConfigEnv {
			env[name] = filepath.Join(c.Work, rel)
		}
	}
	// ON EVERY COMMAND, a call's or not — see
	// [harnessprofile.Profile.IsolationEnv]. A credential store's import, a login
	// and the version probe start the same harness, and a switch that kept a call
	// from loading an ancestor's plugin but not an import would run that
	// plugin's code anyway.
	for name, value := range p.IsolationEnv {
		env[name] = value
	}

	for name, value := range p.Env {
		env[name] = value
	}

	// Forwarded BEFORE auth is consulted, which is exactly why a profile
	// may not name a credential here — see
	// harnessprofile.Profile.ValidateCredentials.
	for _, name := range p.PassthroughEnv {
		if value, ok := os.LookupEnv(name); ok {
			env[name] = value
		}
	}

	for name, value := range extra {
		env[name] = value
	}

	// THE ZONE IS THE ENGINE'S, ALWAYS STATED, and over the entry's env: a
	// harness writes the instant its limit resets as a time of day in its own
	// zone, Codex with no zone at all ("try again at 4:31 PM"), and the
	// engine reads that time in the zone it handed the harness ([resetAt]).
	// Inherited only where the host had one, as TZ used to be, the zone of a
	// zone-less sentence was whatever the host or a box's image defaulted
	// to, which the engine could only guess. An entry's env may not set it
	// (config refuses one), so this replaces nothing an operator wrote.
	env[harnessprofile.ZoneVariable], _ = childZone()

	applyAuth(env, p, auth)
	return env
}

// childZone is the zone every child the engine starts is handed, by its TZ
// name and as a location: the engine's own TZ where it is set to a zone the
// system knows, and UTC otherwise. Named rather than inherited so the engine
// and the harness agree on it, which is what lets a reset the harness names with no
// zone be read ([resetAt]).
//
// UTC, not the host's local zone, where TZ names none: Go reads the host's
// /etc/localtime when TZ is unset, and a box (or a container image) has its
// own, so "local" is the one value two processes cannot be relied on to share.
func childZone() (string, *time.Location) {
	name := strings.TrimPrefix(os.Getenv("TZ"), ":")
	if name == "" {
		return "UTC", time.UTC
	}
	zone, err := time.LoadLocation(name)
	if err != nil {
		return "UTC", time.UTC
	}
	return name, zone
}

// applyAuth puts the credential the mode asks for into the child environment,
// and removes the one it does not.
//
// The removal is the load-bearing half. A subscription call that left an
// ANTHROPIC_API_KEY in place — inherited from the profile's env, or written
// by an operator into env — would bill the metered account silently while
// the plan sat unused, which is the failure the default mode exists to
// prevent.
func applyAuth(env map[string]string, p harnessprofile.Profile, auth Auth) {
	switch auth.Mode {
	case AuthAPIKey:
		if p.APIKeyEnv != "" && auth.APIKey != "" {
			env[p.APIKeyEnv] = auth.APIKey
		}
		if p.TokenEnv != "" {
			delete(env, p.TokenEnv)
		}
	case AuthEnv:
		// THE ENTRY'S ENV IS THE SIGN-IN, and nothing else may compete
		// with it. Left in place, the plan token the convention resolves
		// for every subscription entry reached a Bedrock entry's harness, and
		// into its box, where the cloud switch outranks it and it was never
		// used: a credential handed to a process that does not need it. No
		// subscription_env either: that tells a harness to use the login a home
		// holds, and this entry has none.
		for _, name := range []string{p.TokenEnv, p.APIKeyEnv} {
			if name != "" {
				delete(env, name)
			}
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
		// Part of the mode, so over the operator's env: what points the
		// harness at the login it would otherwise not choose
		// ([harnessprofile.Profile.SubscriptionEnv]).
		for name, value := range p.SubscriptionEnv {
			env[name] = value
		}
	}
}

// tempDir is the TMPDIR [childEnv] hands every command run in a home: a
// directory inside that home, so what a harness unpacks or spools there stays with
// the seat it ran for rather than in the engine's own temporary directory.
func tempDir(home string) string { return filepath.Join(home, seatTempDir) }

// prepareHome creates a home, its cache and the [tempDir] its environment
// names, 0700, before a command runs in it.
//
// THE TEMPORARY DIRECTORY TOO, because it is the one directory a program
// expects to find rather than create: mktemp and every library like it fail
// on a TMPDIR that names nothing. Named and never made, it failed every codex
// call from 0.164 on, whose sandbox helper resolves TMPDIR before it reads
// the call's AGENTS.md ("failed to resolve synthetic mount registry temp
// directory"), and it would fail any harness that writes a temporary file.
func prepareHome(home, cache string) error {
	for _, dir := range []string{home, cache, tempDir(home)} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("harness: preparing %q: %w", dir, err)
		}
	}
	return nil
}

// probeEnv is the child environment for an invocation that belongs to no seat
// — today the version probe `crewlet llm doctor` runs.
//
// IT EXISTS BECAUSE A MISSING ENV IS NOT AN EMPTY ONE. [run] assigns whatever
// it is given to exec.Cmd.Env, and a nil there does not mean "no environment":
// os/exec then hands the child the ENGINE's own, which is the company's chat
// token, its collector credential and every provider key — precisely what the
// allowlist [childEnv] starts from exists to keep out of a vendor's harness. The
// probe was the one call in this package that skipped it, so `--version` ran
// with more access than any real completion ever gets — and [run] now refuses
// a nil environment outright, so the next caller that forgets cannot either.
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
	// Best effort: a harness that cannot write its config still prints its
	// version, and a probe that refused to run over a mkdir would report the
	// drift it exists to detect as an absence.
	_ = prepareHome(home, cache)
	return buildEnv(p.profile, &Checkout{Home: home, Cache: cache}, p.env, p.auth)
}
