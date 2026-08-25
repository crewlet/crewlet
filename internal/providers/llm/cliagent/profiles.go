package cliagent

import (
	_ "embed"
	"fmt"
	"sort"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

//go:embed profiles.yaml
var profilesYAML []byte

// builtins is the parsed profile table, decoded once.
//
// sync.OnceValue rather than an init() so a malformed table fails at the
// first Load with a message naming the file, instead of during package
// initialisation where the panic has no caller to blame.
var builtins = sync.OnceValue(func() map[string]Profile {
	var table map[string]Profile
	dec := yaml.NewDecoder(strings.NewReader(string(profilesYAML)))
	dec.KnownFields(true)
	if err := dec.Decode(&table); err != nil {
		// The file is embedded from this repository, so a decode failure
		// is a build that shipped a broken table — there is no operator
		// input to blame and no correct behaviour to fall back to.
		panic(fmt.Sprintf("cliagent: profiles.yaml does not decode: %v", err))
	}
	return table
})

// BuiltinNames lists the profiles this build ships, sorted.
func BuiltinNames() []string {
	table := builtins()
	names := make([]string, 0, len(table))
	for name := range table {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Builtin returns a copy of one built-in profile.
//
// A copy because callers merge overrides into it, and a shared map value
// would let one provider's `cli.overrides` rewrite the table every later
// provider reads.
func Builtin(name string) (Profile, bool) {
	p, ok := builtins()[name]
	if !ok {
		return Profile{}, false
	}
	return p.clone(), true
}

// clone deep-copies a profile. Every slice and map is copied because
// overrides replace them in place.
func (p Profile) clone() Profile {
	out := p
	out.VersionArgs = append([]string(nil), p.VersionArgs...)
	out.CompleteArgs = append([]string(nil), p.CompleteArgs...)
	out.ModelArgs = append([]string(nil), p.ModelArgs...)
	out.LoginArgs = append([]string(nil), p.LoginArgs...)
	out.CaptureTokenArgs = append([]string(nil), p.CaptureTokenArgs...)
	out.StatusArgs = append([]string(nil), p.StatusArgs...)
	out.LogoutArgs = append([]string(nil), p.LogoutArgs...)
	out.PassthroughEnv = append([]string(nil), p.PassthroughEnv...)
	out.CredentialPaths = append([]string(nil), p.CredentialPaths...)
	out.VolatilePaths = append([]string(nil), p.VolatilePaths...)
	out.HostCredentialPaths = append([]string(nil), p.HostCredentialPaths...)
	out.TextPaths = clonePaths(p.TextPaths)
	out.ErrorPaths = clonePaths(p.ErrorPaths)
	out.Usage = UsagePaths{
		Input:      clonePaths(p.Usage.Input),
		Output:     clonePaths(p.Usage.Output),
		CacheRead:  clonePaths(p.Usage.CacheRead),
		CacheWrite: clonePaths(p.Usage.CacheWrite),
	}
	out.LimitMarkers = append([]LimitMarker(nil), p.LimitMarkers...)
	out.AuthMarkers = append([]AuthMarker(nil), p.AuthMarkers...)
	out.ConfigEnv = cloneMap(p.ConfigEnv)
	out.Env = cloneMap(p.Env)
	if p.StdinLogin != nil {
		login := *p.StdinLogin
		login.Args = append([]string(nil), p.StdinLogin.Args...)
		out.StdinLogin = &login
	}
	return out
}

func clonePaths(in []Path) []Path {
	if in == nil {
		return nil
	}
	out := make([]Path, len(in))
	for i, p := range in {
		out[i] = append(Path(nil), p...)
	}
	return out
}

func cloneMap(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// Load resolves the profile for one provider entry: the built-in table
// entry for name, with overrides merged in, validated.
//
// The merge round-trips through YAML rather than reflecting over the struct,
// which is what makes an override typo a VALIDATION failure at
// `crewlet validate` instead of a field silently ignored until an agent's
// first turn: the re-decode runs with KnownFields, so `complete_arg` is an
// error naming itself.
func Load(name string, overrides map[string]any) (Profile, error) {
	base, ok := Builtin(name)
	if !ok {
		return Profile{}, fmt.Errorf("cli-agent: unknown agent %q (want %s)",
			name, strings.Join(BuiltinNames(), ", "))
	}
	if len(overrides) > 0 {
		merged, err := applyOverrides(base, overrides)
		if err != nil {
			return Profile{}, fmt.Errorf("cli-agent %q: cli.overrides: %w", name, err)
		}
		base = merged
	}
	if err := base.validate(name); err != nil {
		return Profile{}, err
	}
	return base, nil
}

// applyOverrides merges an operator's override map into a profile.
//
// Maps merge key-wise so `env: {A: 1}` adds rather than replaces. EVERYTHING
// ELSE REPLACES WHOLESALE — in particular lists, because position matters in
// an argv and an element-wise merge of two command lines produces one neither
// side wrote. Documented in subscription-llm-backends.md, and pinned by
// TestOverridesReplaceListsWholesale.
func applyOverrides(base Profile, overrides map[string]any) (Profile, error) {
	raw, err := yaml.Marshal(base)
	if err != nil {
		return Profile{}, fmt.Errorf("encoding the built-in profile: %w", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return Profile{}, fmt.Errorf("re-reading the built-in profile: %w", err)
	}
	if doc == nil {
		doc = map[string]any{}
	}
	mergeInto(doc, overrides)

	out, err := yaml.Marshal(doc)
	if err != nil {
		return Profile{}, fmt.Errorf("encoding the merged profile: %w", err)
	}
	var merged Profile
	dec := yaml.NewDecoder(strings.NewReader(string(out)))
	dec.KnownFields(true)
	if err := dec.Decode(&merged); err != nil {
		return Profile{}, fmt.Errorf("%w — see the field list in "+
			"docs/concepts/subscription-llm-backends.md", err)
	}
	return merged, nil
}

// mergeInto merges src into dst, recursing only into maps.
func mergeInto(dst, src map[string]any) {
	for k, v := range src {
		sub, isMap := v.(map[string]any)
		if !isMap {
			dst[k] = v
			continue
		}
		existing, ok := dst[k].(map[string]any)
		if !ok {
			dst[k] = v
			continue
		}
		mergeInto(existing, sub)
	}
}
