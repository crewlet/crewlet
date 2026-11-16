package harnessprofile

import (
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/words"
)

// Effort is how a harness takes a reasoning effort, and the levels it takes in
// its own spelling: {effort} stands in Args (after the model's), ModelSuffix
// (OpenCode's `provider/model#variant`) or Env. Nil: the harness has none.
type Effort struct {
	// Args carry the level after the model's.
	Args []string `yaml:"args,omitempty"`

	// ModelSuffix is added to the model where the model goes.
	ModelSuffix string `yaml:"model_suffix,omitempty"`

	// Env is set on every call.
	Env map[string]string `yaml:"env,omitempty"`

	// Levels are every level the harness accepts, lowest first.
	Levels []string `yaml:"levels,omitempty"`

	// Models narrow Levels for the models the harness restricts: the first
	// whose Match matches the model argument applies.
	Models []EffortModels `yaml:"models,omitempty"`
}

// EffortModels is the levels a harness takes on the models Match names.
type EffortModels struct {
	// Match is a regular expression over the model argument, matched whole.
	Match string `yaml:"match"`

	// Levels are those of [Effort.Levels] these models take. None: the harness
	// runs them with no effort setting at all.
	Levels []string `yaml:"levels,omitempty"`
}

// effortPlaceholder is what the effort's args, suffix and env carry the level as.
const effortPlaceholder = "{effort}"

// TakesEffort reports whether the harness has an effort setting the engine can
// pass: an arg, a model suffix or a variable carrying {effort}.
func (p *Profile) TakesEffort() bool {
	e := p.Effort
	if e == nil {
		return false
	}
	return hasPlaceholder(e.Args, effortPlaceholder) ||
		strings.Contains(e.ModelSuffix, effortPlaceholder) ||
		slices.ContainsFunc(slices.Collect(maps.Values(e.Env)), func(v string) bool {
			return strings.Contains(v, effortPlaceholder)
		})
}

// EffortLevels is the levels the harness takes on model, its model argument
// ([Profile.ModelArgument]), and whether a [EffortModels] rule narrowed them.
// Nil where the harness takes none.
func (p *Profile) EffortLevels(model string) (levels []string, narrowed bool) {
	if !p.TakesEffort() {
		return nil, false
	}
	for _, rule := range p.Effort.Models {
		re, err := regexp.Compile(`^(?:` + rule.Match + `)$`)
		if err == nil && re.MatchString(model) {
			return slices.Clone(rule.Levels), true
		}
	}
	return slices.Clone(p.Effort.Levels), false
}

// EffortArgs is the effort's argv for level, or nil for none.
func (p *Profile) EffortArgs(level string) []string {
	if level == "" || p.Effort == nil || len(p.Effort.Args) == 0 {
		return nil
	}
	out := make([]string, len(p.Effort.Args))
	for i, arg := range p.Effort.Args {
		out[i] = strings.ReplaceAll(arg, effortPlaceholder, level)
	}
	return out
}

// EffortModel is model as the harness is handed it with level: the model with
// the effort's suffix, where the harness takes the level there.
func (p *Profile) EffortModel(model, level string) string {
	if level == "" || model == "" || p.Effort == nil || p.Effort.ModelSuffix == "" {
		return model
	}
	return model + strings.ReplaceAll(p.Effort.ModelSuffix, effortPlaceholder, level)
}

// EffortEnv is the variables a call carries level in, or nil for none.
func (p *Profile) EffortEnv(level string) map[string]string {
	if level == "" || p.Effort == nil || len(p.Effort.Env) == 0 {
		return nil
	}
	out := make(map[string]string, len(p.Effort.Env))
	for name, value := range p.Effort.Env {
		out[name] = strings.ReplaceAll(value, effortPlaceholder, level)
	}
	return out
}

// clone deep-copies an effort.
func (e *Effort) clone() *Effort {
	if e == nil {
		return nil
	}
	out := &Effort{Args: slices.Clone(e.Args), ModelSuffix: e.ModelSuffix,
		Env: maps.Clone(e.Env), Levels: slices.Clone(e.Levels)}
	if e.Models != nil {
		out.Models = make([]EffortModels, len(e.Models))
		for i, rule := range e.Models {
			out.Models[i] = EffortModels{Match: rule.Match, Levels: slices.Clone(rule.Levels)}
		}
	}
	return out
}

// validateEffort reports an effort the engine could not pass as written.
func (p *Profile) validateEffort(add func(string, ...any)) {
	e := p.Effort
	if e == nil {
		return
	}
	if !p.TakesEffort() {
		add("effort carries no {effort} in args, model_suffix or env, so no level would " +
			`reach the harness: declare where it goes, e.g. args: ["--effort", "{effort}"]`)
	}
	if e.ModelSuffix != "" && !p.TakesModel() {
		add("effort.model_suffix is added to the model, and model_args carry no {model}")
	}
	for name := range e.Env {
		if p.IsCredentialOrIsolation(name) {
			add("effort.env names %s, which the engine sets itself", name)
		}
	}
	if len(e.Levels) == 0 {
		add("effort.levels is empty: list the levels the harness accepts")
	}
	seen := make(map[string]bool, len(e.Levels))
	for _, level := range e.Levels {
		switch {
		case strings.TrimSpace(level) == "" || strings.ContainsAny(level, " \t\r\n#/"):
			add("effort.levels holds %q: a level is one word", level)
		case seen[level]:
			add("effort.levels holds %q twice", level)
		}
		seen[level] = true
	}
	for i, rule := range e.Models {
		if _, err := regexp.Compile(rule.Match); err != nil || strings.TrimSpace(rule.Match) == "" {
			add("effort.models[%d].match %q is not a regular expression", i, rule.Match)
		}
		for _, level := range rule.Levels {
			if !seen[level] {
				add("effort.models[%d].levels holds %q, which effort.levels does not", i, level)
			}
		}
	}
}

// IsCredentialOrIsolation reports whether name is a variable the engine sets on
// a call itself: a sign-in's or one that keeps the harness in its home.
func (p *Profile) IsCredentialOrIsolation(name string) bool {
	if name == p.TokenEnv || name == p.APIKeyEnv || IsCredentialName(name) {
		return true
	}
	_, isolated := p.IsolationLayerOf(name)
	return isolated
}

// EffortProblem says why level is not one the harness takes on model, or ""
// where it is. harness names it for a person; an empty model is judged on the
// harness alone.
func (p *Profile) EffortProblem(harness, model, level string) string {
	if !p.TakesEffort() {
		said := fmt.Sprintf("%s has no reasoning effort setting the engine can pass, so the "+
			"level would reach nothing", harness)
		if p.EffortNote != "" {
			said += " (" + p.EffortNote + ")"
		}
		return said
	}
	levels, narrowed := p.EffortLevels(model)
	switch {
	case slices.Contains(levels, level):
		return ""
	case narrowed && len(levels) == 0:
		return fmt.Sprintf("%s runs %s with no reasoning effort setting, so the level would "+
			"reach nothing", harness, model)
	case narrowed:
		return fmt.Sprintf("%s takes %s on %s, not %q", harness, words.Or(levels), model, level)
	}
	return fmt.Sprintf("%s takes %s, not %q", harness, words.Or(levels), level)
}
