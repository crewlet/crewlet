package harness

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/providers/llm/harness/harnessprofile"
)

// SignInID names which account an entry's calls are made as, without saying it:
// an HMAC, under its [Plans]' key, of the harness, auth mode and credentials
// handed it, or of the login its files hold.
//
//	HMAC-SHA256(k, agent 0 mode 0 sorted(name=value 0 ...) 0 login)
//
// KEYED BY A SECRET THE FLEET SHARES, not hashed, not per process: an id must
// compare across nodes and restarts. A plain hash could be tested against a
// guessed or leaked credential; the keyring-derived key ([SignInKeyInfo])
// cannot. Rotation changes every id, so every sit-out starts clean once
// (ADR-0069). NEVER LOGGED AND NEVER PUBLISHED WHOLE ([SignInID.Short] may be).
// Empty means nothing signs the entry in: it shares its [Plan] with nobody.
type SignInID string

// Short is the id's first twelve hex characters: enough to tell the sign-ins
// of one company apart, and too few to stand for the credential.
func (id SignInID) Short() string {
	if len(id) < 12 {
		return string(id)
	}
	return string(id[:12])
}

// SignInID is this entry's sign-in identity, read now. It moves only when what
// signs the harness in moves: a credential value the harness is handed, the auth mode,
// or, for a login kept in files, a new login ([LoginInstall]). A token the harness
// refreshes into those files is the same login, so it keeps the id, and the
// sit-out with it.
//
// THE CREDENTIALS ARE READ OFF [childEnv], the environment a call is handed,
// so the id and what the harness signs in with cannot disagree: a token the mode
// removes from the child is no part of the id, and a gateway token set in the
// entry's env is.
func (p *Provider) SignInID() SignInID {
	credentials := p.handedCredentials()
	var login string
	if len(credentials) == 0 && !p.signsInWithKey() && p.ws != nil {
		login = p.loginIdentity()
	}
	if len(credentials) == 0 && login == "" {
		return ""
	}
	mac := hmac.New(sha256.New, p.registry().key)
	write := func(field string) {
		mac.Write([]byte(field))
		mac.Write([]byte{0})
	}
	write(p.harness)
	write(string(p.AuthMode()))
	for _, name := range slices.Sorted(maps.Keys(credentials)) {
		write(name + "=" + credentials[name])
	}
	// WHERE THOSE CREDENTIALS REACH ([harnessprofile.Profile.IdentityEnv]): a
	// cloud switch, or ambient cloud credentials the box resolves itself,
	// is one value for every region, project and gateway, and the limit is
	// each one's own.
	env := childEnv(p.profile, &Checkout{}, p.env, p.auth)
	for _, name := range slices.Sorted(slices.Values(p.profile.IdentityEnv)) {
		if value := strings.TrimSpace(env[name]); value != "" {
			write(name + "~" + value)
		}
	}
	write(login)
	return SignInID(hex.EncodeToString(mac.Sum(nil)))
}

// handedCredentials is every credential the harness is handed by name: the
// profile's token and key variables, every name the profile signs in by from
// an entry's env ([harnessprofile.Profile.SignInEnv]), and every credential-named
// variable of the entry's env ([harnessprofile.IsCredentialName]), as [childEnv]
// leaves them. A blank value hands nothing.
func (p *Provider) handedCredentials() map[string]string {
	env := childEnv(p.profile, &Checkout{}, p.env, p.auth)
	names := []string{p.profile.TokenEnv, p.profile.APIKeyEnv}
	names = append(names, p.profile.SignInEnv...)
	for name := range p.env {
		if harnessprofile.IsCredentialName(name) {
			names = append(names, name)
		}
	}
	out := map[string]string{}
	for _, name := range names {
		if name == "" {
			continue
		}
		if value := strings.TrimSpace(env[name]); value != "" {
			out[name] = value
		}
	}
	return out
}

// loginIdentity names the login the entry's credential files hold, for an
// entry whose calls are handed no credential: the login's install record, which
// moves only on a new login ([LoginInstall]), or, for a login installed before
// records existed or one with no record, the directory it is kept in, which is
// the one login every entry on that directory and harness signs in with.
//
// A record that cannot be read names the directory too: the entry still signs
// in with whatever login is there, and sharing a plan with the other entries on
// it is what is true of that login.
func (p *Provider) loginIdentity() string {
	if record, err := p.ws.LoginInstall(); err == nil && !record.IsZero() {
		return "login:" + record.ID
	}
	return "dir:" + p.ws.CredentialsDir()
}

// Plan is the record of what this entry's sign-in has met on this node
// ([Plans]), found anew whenever the sign-in moves: a credential stored under
// the same name, a login written by another process. A provider never keeps a
// Plan its sign-in has left.
func (p *Provider) Plan() *Plan {
	id := p.SignInID()
	plans := p.registry()
	p.planMu.Lock()
	defer p.planMu.Unlock()
	if p.plan != nil && p.planOf == id {
		return p.plan
	}
	p.plan, p.planOf = plans.For(id), id
	return p.plan
}

// registry is the provider's [Plans]: the one [New] was handed, or, for a
// Provider built without New, a registry of its own made on first use, so the
// zero Provider sits out and remembers as one built alone would.
func (p *Provider) registry() *Plans {
	p.planMu.Lock()
	defer p.planMu.Unlock()
	if p.plans == nil {
		p.plans = NewPlans(nil)
	}
	return p.plans
}

// EntryID names the providers.llm entry a launch runs under and where its
// calls go, built from its route and sign-in mode only: 32 hex characters of an
// HMAC, under its [Plans]' entry key ([FleetPlans]), of the entry's key,
// harness, auth mode, the provider its model names where the harness reads one
// there ([harnessprofile.Profile.ModelNamesProvider]) and its routes as [New]
// read them ([readRoutes]). A carried session continues only under the entry
// it ran under unless its harness resumes on its launch's route
// (internal/sandbox's EntryContinues). An allowlist, since the env is secret
// whole, and keyed, so a credential in a route is never a plain digest. EMPTY,
// which continues nothing, under Plans with no entry key (nil Plans, the
// doctor's, [NewPlans]): only the fleet's makes an id another node can match.
func (p *Provider) EntryID() string {
	plans := p.registry()
	if len(plans.entryKey) == 0 {
		return ""
	}
	mac := hmac.New(sha256.New, plans.entryKey)
	write := func(field string) {
		mac.Write([]byte(field))
		mac.Write([]byte{0})
	}
	write(p.key)
	write(p.harness)
	write(string(p.AuthMode()))
	if p.profile.ModelNamesProvider {
		provider, _, _ := strings.Cut(p.model, "/")
		write("model-provider=" + provider)
	}
	for _, name := range slices.Sorted(maps.Keys(p.routes)) {
		write(name + "=" + p.routes[name])
	}
	return hex.EncodeToString(mac.Sum(nil))[:entryIDChars]
}

// entryIDChars is how much of the digest an [Provider.EntryID] keeps: 128 bits,
// which no two entries of one fleet collide in, within
// executorkeep.EntryMaxBytes.
const entryIDChars = 32

// readRoutes is the routes of an entry's env, as resolved: each variable its
// profile declares a route ([harnessprofile.Profile.RouteEnv]) the env sets,
// as [harnessprofile.Profile.RouteValue] reads it, and an error for one that is
// no route. Read once, by [New], so [Provider.EntryID] never reads a value
// again. The error names the entry and the variable, never the value.
func readRoutes(key string, profile harnessprofile.Profile, env map[string]string) (map[string]string, error) {
	routes := map[string]string{}
	for _, name := range profile.RouteEnv() {
		value := strings.TrimSpace(env[name])
		if value == "" {
			continue
		}
		route, err := profile.RouteValue(value)
		if err != nil {
			return nil, fmt.Errorf("harness %q: env %s: %w", key, name, err)
		}
		routes[name] = route
	}
	return routes, nil
}
