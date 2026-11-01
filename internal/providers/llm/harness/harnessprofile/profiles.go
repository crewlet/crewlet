package harnessprofile

import (
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"maps"
	"runtime"
	"slices"
	"sync"

	"gopkg.in/yaml.v3"

	"github.com/crewlet/crewlet/internal/words"
)

//go:embed profiles.yaml
var profilesYAML []byte

// builtins is the parsed profile table for the platform this build runs on,
// decoded once.
//
// sync.OnceValue rather than an init() so a malformed table fails at the
// first Load with a message naming the file, instead of during package
// initialisation where the panic has no caller to blame.
var builtins = sync.OnceValue(func() map[string]Profile {
	table, err := decodeTable(profilesYAML, runtime.GOOS)
	if err != nil {
		// The file is embedded from this repository, so a decode failure
		// is a build that shipped a broken table — there is no operator
		// input to blame and no correct behaviour to fall back to.
		panic(fmt.Sprintf("harnessprofile: profiles.yaml: %v", err))
	}
	return table
})

// Platforms are the operating systems a profile's `platforms` section may
// name: the ones a release is built for, which the store's database engine
// bounds (internal/store/platform.go).
var Platforms = []string{"darwin", "linux"}

// builtinEntry is one profile as profiles.yaml writes it: the profile, and
// the fields that differ on one platform.
//
// A SECTION OF THE TABLE, NOT A PROFILE FIELD, because it is resolved before
// anything else reads the profile: an operator's `overrides` are merged into
// the profile of the platform the engine runs on, and an override of a field a
// platform section also set would otherwise be undone by the section.
type builtinEntry struct {
	Profile `yaml:",inline"`
	// Platforms holds, by GOOS, the fields that differ there, merged over
	// the profile the way an override is ([applyOverrides]): maps key by
	// key, everything else wholesale. Cursor Agent keeps its login at
	// ~/.cursor/auth.json on macOS and at $XDG_CONFIG_HOME/cursor/auth.json
	// on Linux (read from its bundle, and measured on Linux).
	Platforms map[string]map[string]any `yaml:"platforms,omitempty"`
}

// decodeTable decodes a profile table as it reads on goos.
func decodeTable(raw []byte, goos string) (map[string]Profile, error) {
	var entries map[string]builtinEntry
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	dec.KnownFields(true)
	if err := dec.Decode(&entries); err != nil {
		return nil, fmt.Errorf("does not decode: %w", err)
	}
	table := make(map[string]Profile, len(entries))
	for name, entry := range entries {
		for platform := range entry.Platforms {
			if !slices.Contains(Platforms, platform) {
				return nil, fmt.Errorf("%s: platforms names %q, which no release is built "+
					"for: use %s", name, platform, words.Or(Platforms))
			}
		}
		profile := entry.Profile
		if section := entry.Platforms[goos]; len(section) > 0 {
			merged, err := applyOverrides(profile, section)
			if err != nil {
				return nil, fmt.Errorf("%s: platforms.%s: %w", name, goos, err)
			}
			profile = merged
		}
		table[name] = profile
	}
	return table, nil
}

// BuiltinNames lists the profiles this build ships, sorted.
func BuiltinNames() []string {
	table := builtins()
	names := slices.Sorted(maps.Keys(table))
	return names
}

// AssistantEligible lists the built-in profiles the Dashboard Assistant can
// run on, sorted: those whose web-denied run is left with no tool of its own
// by construction ([Profile.ToolsOffProblem]). Derived from the table, as
// [WebDeniable] is, of which it is a subset.
func AssistantEligible() []string {
	var out []string
	for _, name := range WebDeniable() {
		p, err := Load(name, map[string]any{"web_tools": string(WebToolsDenied)})
		if err == nil && p.ToolsOffProblem() == "" {
			out = append(out, name)
		}
	}
	return out
}

// WebDeniable lists the built-in profiles that can run with their web tools
// off ([Profile.CanDenyWeb]), sorted: the harnesses an entry may point the
// Dashboard Assistant at. Derived from the table, so a profile that gains a
// measured web_denial is named everywhere this list is printed.
func WebDeniable() []string {
	var out []string
	for _, name := range BuiltinNames() {
		if p := builtins()[name]; p.CanDenyWeb() {
			out = append(out, name)
		}
	}
	return out
}

// Builtin returns a copy of one built-in profile.
//
// A copy because callers merge overrides into it, and a shared map value
// would let one provider's `overrides` rewrite the table every later
// provider reads.
func Builtin(name string) (Profile, bool) {
	p, ok := builtins()[name]
	if !ok {
		return Profile{}, false
	}
	return p.Clone(), true
}

// Clone deep-copies a profile. Every slice and map is copied because
// overrides replace them in place.
func (p Profile) Clone() Profile {
	out := p
	out.VersionArgs = append([]string(nil), p.VersionArgs...)
	out.CompleteArgs = append([]string(nil), p.CompleteArgs...)
	out.ModelArgs = append([]string(nil), p.ModelArgs...)
	out.SystemPrompt = p.SystemPrompt.clone()
	out.AppendSystemPrompt = p.AppendSystemPrompt.clone()
	out.PromptArgs = append([]string(nil), p.PromptArgs...)
	out.UsageFileArgs = append([]string(nil), p.UsageFileArgs...)
	out.SessionArgs = append([]string(nil), p.SessionArgs...)
	out.UsageArgs = append([]string(nil), p.UsageArgs...)
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
		Reasoning:  clonePaths(p.Usage.Reasoning),
		ToolPrompt: clonePaths(p.Usage.ToolPrompt),

		InputIncludesCache: p.Usage.InputIncludesCache,
	}
	out.EventTypePath = append(Path(nil), p.EventTypePath...)
	out.TextEvents = append([]string(nil), p.TextEvents...)
	if p.LimitEvents != nil {
		out.LimitEvents = make([]LimitEvent, len(p.LimitEvents))
		for i, e := range p.LimitEvents {
			out.LimitEvents[i] = LimitEvent{Match: cloneMatches(e.Match),
				SaidPath: slices.Clone(e.SaidPath), Reset: e.Reset}
		}
	}
	out.LimitMarkers = append([]LimitMarker(nil), p.LimitMarkers...)
	out.AuthMarkers = append([]AuthMarker(nil), p.AuthMarkers...)
	out.MarkerEvents = append([]string(nil), p.MarkerEvents...)
	out.MarkerPath = append(Path(nil), p.MarkerPath...)
	out.SeedFiles = append([]SeedFile(nil), p.SeedFiles...)
	out.KeySeedFiles = append([]SeedFile(nil), p.KeySeedFiles...)
	out.ConfigEnv = cloneMap(p.ConfigEnv)
	out.WorkConfigEnv = cloneMap(p.WorkConfigEnv)
	out.IsolationEnv = cloneMap(p.IsolationEnv)
	out.Env = cloneMap(p.Env)
	out.SubscriptionEnv = cloneMap(p.SubscriptionEnv)
	out.LoginKeys = cloneMap(p.LoginKeys)
	if p.StdinLogin != nil {
		login := *p.StdinLogin
		login.Args = append([]string(nil), p.StdinLogin.Args...)
		out.StdinLogin = &login
	}
	if p.WebDenial != nil {
		// slices.Clone keeps a nil list nil, and nil is meaningful here:
		// an unset field leaves its profile field alone.
		out.WebDenial = &WebDenial{
			CompleteArgs: slices.Clone(p.WebDenial.CompleteArgs),
			SeedFiles:    slices.Clone(p.WebDenial.SeedFiles),
		}
		if dm := p.WebDenial.MCP; dm != nil {
			out.WebDenial.MCP = &WebDenialMCP{CompleteArgs: slices.Clone(dm.CompleteArgs),
				Seed: slices.Clone(dm.Seed)}
		}
	}
	if p.ToolsOff != nil {
		args := make([][]string, len(p.ToolsOff.Args))
		for i, run := range p.ToolsOff.Args {
			args[i] = append([]string(nil), run...)
		}
		out.ToolsOff = &ToolsOff{Args: args, Refuses: append([]string(nil), p.ToolsOff.Refuses...),
			Note: p.ToolsOff.Note}
	}
	if p.CredentialStore != nil {
		out.CredentialStore = &CredentialStore{
			ExportArgs: append([]string(nil), p.CredentialStore.ExportArgs...),
			ImportArgs: append([]string(nil), p.CredentialStore.ImportArgs...),
		}
	}
	out.MCP = p.MCP.clone()
	out.Requests = p.Requests.clone()
	out.Cap = p.Cap.clone()
	out.FailureMarkers = append([]string(nil), p.FailureMarkers...)
	p.cloneSignIn(&out)
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

// ErrOverrides marks an `overrides` map that does not decode into a
// profile — a key no profile field has, or a value of the wrong shape — as
// opposed to a merged profile that decodes and breaks a rule. Config reports
// the two under different kinds, so an editor can be pointed at the typo.
var ErrOverrides = errors.New("overrides does not decode into a profile")

// Load resolves the profile for one provider entry: the built-in table
// entry for name, with overrides merged in, validated.
//
// The merge round-trips through YAML rather than reflecting over the struct,
// which is what makes an override typo a VALIDATION failure at
// `crewlet validate` instead of a field silently ignored until an agent's
// first turn: the re-decode runs with KnownFields, so `complete_arg` is an
// error naming itself.
//
// Validated as [Profile.Validate] validates any profile, and against the
// built-in it was merged into besides: an override that moves a relocation
// directory must move what the built-in keeps under it ([Profile.strandedBy]),
// which only the pair can show.
//
// The profile it returns is the one the HARNESS RUNS: an entry whose merged stance
// is `web_tools: denied` comes back with its [WebDenial] already applied, so
// every caller (the backend that launches it, config validating it, the
// doctor probing it) reads the same argv and the same seeded files.
func Load(name string, overrides map[string]any) (Profile, error) {
	builtin, ok := Builtin(name)
	if !ok {
		return Profile{}, fmt.Errorf("harness: unknown agent %q (want %s)",
			name, words.Or(BuiltinNames()))
	}
	if len(overrides) == 0 {
		if err := builtin.Validate(name); err != nil {
			return Profile{}, err
		}
		return builtin, nil
	}
	merged, err := applyOverrides(builtin, overrides)
	if err != nil {
		return Profile{}, fmt.Errorf("cli-agent %q: %w: %w", name, ErrOverrides, err)
	}
	bad := merged.webOverrideConflicts(overrides)
	if merged.EffectiveWebTools() == WebToolsDenied && merged.WebDenial != nil {
		// A RUN WITH TOOLS IS DENIED THE WEB TOO, or the entry is refused:
		// see [WebDenial]. Asked of the entry that denies, never of a
		// profile that only carries a denial, so an operator's own mcp
		// route on a seat that keeps its web is not refused for a stance
		// it does not take.
		if uncovered := merged.WebDenial.mcpUncovered(merged.MCP); len(uncovered) > 0 {
			bad = append(bad, "web_tools is denied, but web_denial leaves "+
				words.And(uncovered)+" in place, which a run with tools uses "+
				"instead of what the denial replaces, so the web would stay on where a seat "+
				"works: set web_denial.mcp to what that run uses with web denied")
		}
		merged = merged.withWebDenied()
	}
	bad = append(bad, merged.problems()...)
	if err := profileError(name, append(bad, merged.strandedBy(&builtin)...)); err != nil {
		return Profile{}, err
	}
	return merged, nil
}

// webOverrideConflicts refuses an override the web denial would silently
// discard: a complete_args or seed_files written beside `web_tools: denied`
// when the profile's web_denial replaces that same field. Applied as written,
// the denial wins and the operator's edit never runs; applied the other way,
// the edit wins and the denial never runs. Neither is what was written, so
// the operator is told which field to edit instead.
func (p *Profile) webOverrideConflicts(overrides map[string]any) []string {
	if p.EffectiveWebTools() != WebToolsDenied || p.WebDenial == nil {
		return nil
	}
	var bad []string
	if _, set := overrides["complete_args"]; set && p.WebDenial.CompleteArgs != nil {
		bad = append(bad, "complete_args is overridden, but web_tools is denied and "+
			"web_denial.complete_args replaces it; override web_denial.complete_args instead")
	}
	if _, set := overrides["seed_files"]; set && p.WebDenial.SeedFiles != nil {
		bad = append(bad, "seed_files is overridden, but web_tools is denied and "+
			"web_denial.seed_files replaces it; override web_denial.seed_files instead")
	}
	return bad
}

// applyOverrides merges an operator's override map into a profile.
//
// Maps merge key-wise so `env: {A: 1}` adds rather than replaces. EVERYTHING
// ELSE REPLACES WHOLESALE — in particular lists, because position matters in
// an argv and an element-wise merge of two command lines produces one neither
// side wrote. Documented in llm-backends.md, and pinned by
// TestOverridesReplaceListsWholesale.
func applyOverrides(base Profile, overrides map[string]any) (Profile, error) {
	raw, err := yaml.Marshal(base)
	if err != nil {
		return Profile{}, fmt.Errorf("encoding the built-in profile: %w", err)
	}
	var doc map[string]any
	//nolint:govet // shadow: scoped to this block; see .golangci.yml
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
	dec := yaml.NewDecoder(bytes.NewReader(out))
	dec.KnownFields(true)
	if err := dec.Decode(&merged); err != nil {
		return Profile{}, fmt.Errorf("%w — the fields are listed under "+
			"\"Profile fields\" in docs/concepts/llm-backends.md", err)
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
