package cliprofile

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"
)

// seatBase is the part of the isolation every profile gets, whatever it
// declares: HOME, the XDG base directories and TMPDIR, each pointed inside the
// seat's home — except XDG_CACHE_HOME, which lives BESIDE the home at cache,
// because a cache is warm across calls and holds no conversation.
func seatBase(home, cache string) map[string]string {
	return map[string]string{
		"HOME":            home,
		"XDG_CONFIG_HOME": filepath.Join(home, ".config"),
		"XDG_DATA_HOME":   filepath.Join(home, ".local", "share"),
		"XDG_STATE_HOME":  filepath.Join(home, ".local", "state"),
		"XDG_CACHE_HOME":  cache,
		"TMPDIR":          filepath.Join(home, "tmp"),
	}
}

// SeatEnv is the ISOLATION, by name and value: every variable a CLI could keep
// state under, pointed inside one seat's home — the base layout every profile
// gets, plus the vendor's own relocation variables ([Profile.ConfigEnv]). It
// is what keeps seven seats on one subscription from reading each other's
// transcripts, so the backend sets it LAST in a child's environment, over
// every layer an operator or a profile can write.
//
// ONE FUNCTION FOR THE NAMES AND THE VALUES, because both are read: the
// backend sets these values, and [Profile.EngineEnv] — what config refuses any
// other layer to name — is these names. A variable added to the isolation is
// set and guarded by the same edit.
//
// A relocation variable naming one of the base variables is refused by
// [Profile.Validate]; were one to reach here, the base value would stand.
func (p *Profile) SeatEnv(home, cache string) map[string]string {
	env := seatBase(home, cache)
	for name, rel := range p.ConfigEnv {
		if _, base := env[name]; base {
			continue
		}
		env[name] = filepath.Join(home, rel)
	}
	return env
}

// EngineEnv names every variable the engine sets in a CLI child's environment
// itself: the isolation ([Profile.SeatEnv]) and the system-prompt channel
// ([Profile.SystemPromptEnv]), which carries a per-call path. Sorted.
//
// THE ENGINE'S VALUE OR NONE. The backend removes each of these from whatever
// the lower layers assembled — the profile's env, its passthrough_env, the
// operator's cli.env — before it sets its own, so no layer can move a seat's
// home: a cli.env HOME used to land AFTER the isolation and collapse every
// seat on the entry into one home, which is the shared-memory failure this
// backend exists to prevent. Config refuses a document that names one in any
// of those layers ([Profile.ValidateEnv], and cli.env beside it), so an
// operator is told rather than silently overruled.
func (p *Profile) EngineEnv() []string {
	return slices.Sorted(maps.Keys(p.engineEnv()))
}

// EngineSets says what the engine puts in an engine-owned variable — the
// clause a refusal to let any other layer name it ends with — or "" for a
// variable the engine leaves to the layers below.
func (p *Profile) EngineSets(env string) string {
	return p.engineEnv()[env]
}

// engineEnv maps each engine-owned variable to what the engine puts there.
func (p *Profile) engineEnv() map[string]string {
	owned := map[string]string{}
	for name := range p.SeatEnv("", "") {
		owned[name] = "a directory of the seat's own home (the isolation between seats)"
	}
	if p.SystemPromptEnv != "" {
		owned[p.SystemPromptEnv] = "the path of the seat's system prompt for that call"
	}
	return owned
}

// ValidateEnv reports every place this profile names a variable the engine
// sets itself ([Profile.EngineEnv]): an env or passthrough_env entry, and a
// token_env or api_key_env, whose credential the engine's own value would
// replace before the CLI ever read it.
//
// AN ADMISSION RULE, like [Profile.ValidateCredentials] and for the same
// reason: a profile that breaks one still runs — the engine's value wins, so
// the CLI is isolated whatever the profile says — and what it gets wrong is a
// setting that reaches nothing. Config refuses it on every write and warns
// about a revision being applied.
func (p *Profile) ValidateEnv(name string) error {
	var bad []string
	refuse := func(field, env, consequence string) {
		if sets := p.EngineSets(env); sets != "" {
			bad = append(bad, fmt.Sprintf("%s names %s, which the engine sets itself on every "+
				"call to %s, so %s — remove it", field, env, sets, consequence))
		}
	}
	for _, env := range slices.Sorted(maps.Keys(p.Env)) {
		refuse("env", env, "the value written here never reaches the CLI")
	}
	for _, env := range p.PassthroughEnv {
		refuse("passthrough_env", env, "the host's value it would forward never reaches the CLI")
	}
	refuse("token_env", p.TokenEnv, "a token placed there never reaches the CLI")
	refuse("api_key_env", p.APIKeyEnv, "a key placed there never reaches the CLI")
	if len(bad) == 0 {
		return nil
	}
	return fmt.Errorf("cli-agent profile %q: %s", name, strings.Join(bad, "; "))
}
