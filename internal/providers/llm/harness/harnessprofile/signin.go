package harnessprofile

import (
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"strings"

	"github.com/crewlet/crewlet/internal/envref"
	"github.com/crewlet/crewlet/internal/providers/llm"
)

// LimitResetEvent reads WHEN a limit lifts and WHICH limit it is from an event
// the harness writes in fields, ahead of any sentence: Claude Code's
// `rate_limit_event`, whose `rate_limit_info` carries the reset as an epoch and
// the window as `five_hour`, `seven_day` or `seven_day_opus`.
//
// IT DOES NOT CLASSIFY. A run reports a limit by its [LimitEvent] or a
// [LimitMarker], and this only dates and names it: the LAST event of a run
// that matches is the reset of the limit the same run reported. Claude Code
// writes one on every run, an allowed one too (`status: allowed_warning`), so
// a match that does not name the refusal would date a limit by a window that
// is still open; the shipped profile matches `status: rejected` for that
// reason.
//
// Safe for the reason [LimitEvent] is: a model chooses the words inside an
// event's strings, never the keys or the types of the event, so the profile
// may declare these only with jsonl output and a discriminator, and every
// match names the discriminator and at least one field beside it.
type LimitResetEvent struct {
	// Match picks the event out; see [Match].
	Match []Match `yaml:"match,omitempty"`

	// ResetsAt is where the event carries the instant the limit lifts.
	ResetsAt EventInstant `yaml:"resets_at,omitempty"`

	// Window is where the event names which limit it is, and how the
	// vendor's names map onto [llm.LimitWindow]. Empty reads no window.
	Window EventWindow `yaml:"window,omitempty"`
}

// EventInstant is an instant at a path in an event, in a unit.
type EventInstant struct {
	Path Path        `yaml:"path,omitempty"`
	Unit InstantUnit `yaml:"unit,omitempty"`
}

// InstantUnit is how an [EventInstant] is written.
type InstantUnit string

const (
	// InstantEpoch is Unix seconds, as a number or a string of digits.
	InstantEpoch InstantUnit = "epoch"
	// InstantEpochMS is Unix milliseconds.
	InstantEpochMS InstantUnit = "epoch_ms"
	// InstantRFC3339 is an RFC 3339 timestamp.
	InstantRFC3339 InstantUnit = "rfc3339"
)

// Valid reports whether u is a unit an instant can be read in.
func (u InstantUnit) Valid() bool {
	switch u {
	case InstantEpoch, InstantEpochMS, InstantRFC3339:
		return true
	}
	return false
}

// EventWindow is where an event names its limit, and the map from the
// vendor's names to the engine's. A name the map does not hold reads as
// [llm.LimitUnknown], so a window the vendor adds is a limit of unknown window
// rather than a misread one.
type EventWindow struct {
	Path Path                       `yaml:"path,omitempty"`
	Map  map[string]llm.LimitWindow `yaml:"map,omitempty"`
}

// LimitWindowWording names the window of a limit from the vendor's sentence,
// for a report no [LimitResetEvent] named one for: "weekly limit" is
// [llm.LimitWeekly]. Compared case-sensitively, as a sentinel is, against the
// sentence the classification already took as the vendor's report, so it
// reads nothing the model wrote.
type LimitWindowWording struct {
	Contains string          `yaml:"contains,omitempty"`
	Window   llm.LimitWindow `yaml:"window,omitempty"`
}

// AuthEvent recognises a refused sign-in by the harness's own STRUCTURED report,
// as [LimitEvent] does a spent plan: Claude Code's result event with
// `is_error: true` and `api_error_status: 401`, or its `system` event of
// subtype `api_retry` with `error: authentication_failed`.
//
// READ FIRST, before every limit report and every sentinel: a status code
// survives the rewording that retires a sentence, and the harness that names a 401
// has said the sign-in is what failed whatever its prose says after.
type AuthEvent struct {
	// Match picks the report out; see [Match].
	Match []Match `yaml:"match,omitempty"`

	// SaidPath is where in the event the vendor's own sentence is. Empty
	// shows the whole line.
	SaidPath Path `yaml:"said_path,omitempty"`
}

// DeviceCodePatterns reads a device sign-in's verification address and its
// one-time code out of what the harness prints ([Profile.DeviceLoginArgs]), with
// ANSI colour already stripped. Each is a regular expression whose FIRST
// capture group is the value. Lifetime, when set, captures the code's
// lifetime in minutes ("expires in 15 minutes").
type DeviceCodePatterns struct {
	URL      string `yaml:"url,omitempty"`
	Code     string `yaml:"code,omitempty"`
	Lifetime string `yaml:"lifetime,omitempty"`
}

// StatusFields locates, in the JSON a harness prints for [Profile.StatusArgs], how
// it is signed in and as whom. Each is a list of paths, the first that holds a
// non-empty string winning: Claude Code prints `authMethod` beside `email` and
// `orgName`, of which an API-key login has neither.
type StatusFields struct {
	Method  []Path `yaml:"method,omitempty"`
	Account []Path `yaml:"account,omitempty"`
}

// ModelAlias is a model name the harness resolves itself ("sonnet" is the harness's
// latest Sonnet), which no catalogue of the vendor's models lists.
type ModelAlias struct {
	ID     string `yaml:"id,omitempty"`
	Name   string `yaml:"name,omitempty"`
	Detail string `yaml:"detail,omitempty"`
}

// ReadsLimitWindow reports the window a vendor's sentence names by the
// profile's [Profile.LimitWindows], or [llm.LimitUnknown].
func (p *Profile) ReadsLimitWindow(said string) llm.LimitWindow {
	for _, w := range p.LimitWindows {
		if strings.Contains(said, w.Contains) {
			return w.Window
		}
	}
	return llm.LimitUnknown
}

// SignsInWithEnv reports whether env, the operator's env of an entry on this
// harness, sets one of the names [Profile.SignInEnv] lists, and which: the names
// that make an entry's env its sign-in under auth.mode env.
func (p *Profile) SignsInWithEnv(env map[string]string) []string {
	var names []string
	for _, name := range p.SignInEnv {
		if strings.TrimSpace(env[name]) != "" {
			names = append(names, name)
		}
	}
	return names
}

// HasDeviceLogin reports whether the harness has a device sign-in the engine can
// run ([Profile.DeviceLoginArgs]).
func (p *Profile) HasDeviceLogin() bool { return len(p.DeviceLoginArgs) > 0 }

// validateSignIn holds the sign-in, credential-shape and catalogue fields to a
// shape the engine can use.
func (p *Profile) validateSignIn(add func(string, ...any)) {
	isolation := p.IsolationVariables()
	for i, name := range p.SignInEnv {
		switch {
		case !envref.ValidName(name):
			add("sign_in_env[%d] %q is not an environment variable name", i, name)
		case name == p.TokenEnv || name == p.APIKeyEnv:
			add("sign_in_env[%d] %q is the profile's own token_env or api_key_env, which "+
				"auth.mode env removes from the child: it cannot also be what signs it in", i, name)
		case slices.Contains(isolation, name):
			add("sign_in_env[%d] %q is a variable the engine sets to isolate the call", i, name)
		}
	}
	for i, name := range p.IdentityEnv {
		switch {
		case !envref.ValidName(name):
			add("identity_env[%d] %q is not an environment variable name", i, name)
		case slices.Contains(isolation, name):
			add("identity_env[%d] %q is a variable the engine sets to isolate the call", i, name)
		}
	}
	if p.EnvSignIn && len(p.SignInEnv) > 0 {
		// env_sign_in counts every credential-named variable in an entry's
		// env, and sign_in_env a fixed list: a profile with both would have
		// two answers to "does this env sign the harness in".
		add("sign_in_env is set on an env_sign_in profile, which already counts every " +
			"credential-named variable of an entry's env as its sign-in: set one or the other")
	}
	for _, prefix := range []struct{ field, value, variable string }{
		{"token_prefix", p.TokenPrefix, p.TokenEnv},
		{"api_key_prefix", p.APIKeyPrefix, p.APIKeyEnv},
	} {
		switch {
		case prefix.value == "":
		case prefix.variable == "":
			add("%s is set but the profile names no variable that carries it", prefix.field)
		case strings.TrimSpace(prefix.value) != prefix.value:
			add("%s %q has surrounding whitespace, which no credential starts with",
				prefix.field, prefix.value)
		}
	}
	if p.TokenPrefix != "" && p.APIKeyPrefix != "" &&
		(strings.HasPrefix(p.TokenPrefix, p.APIKeyPrefix) || strings.HasPrefix(p.APIKeyPrefix, p.TokenPrefix)) {
		add("token_prefix %q and api_key_prefix %q overlap, so neither tells a token from a key",
			p.TokenPrefix, p.APIKeyPrefix)
	}
	if p.TokenLifetimeDays < 0 {
		add("token_lifetime_days is %d; a lifetime is zero (unknown) or more", p.TokenLifetimeDays)
	}
	if p.TokenLifetimeDays > 0 && p.TokenEnv == "" {
		add("token_lifetime_days is set but the profile names no token_env, so there is no token to expire")
	}
	p.validateDeviceLogin(add)
	if p.StatusFields != nil && len(p.StatusArgs) == 0 {
		add("status_fields is set with no status_args: nothing prints the JSON they read")
	}
	if len(p.StatusTextArgs) > 0 && len(p.StatusArgs) == 0 {
		add("status_text_args is set with no status_args: the text form is the doctor's " +
			"reading of the same command")
	}
	seen := map[string]bool{}
	for i, alias := range p.ModelAliases {
		switch {
		case strings.TrimSpace(alias.ID) == "":
			add("model_aliases[%d].id is empty", i)
		case seen[alias.ID]:
			add("model_aliases[%d].id %q is listed twice", i, alias.ID)
		}
		seen[alias.ID] = true
	}
	if p.ModelsDocs != "" {
		if u, err := url.Parse(p.ModelsDocs); err != nil || u.Scheme != "https" || u.Host == "" {
			add("models_docs %q is not an https address", p.ModelsDocs)
		}
	}
}

// validateDeviceLogin holds a device sign-in to a command and the patterns
// that read its address and code.
func (p *Profile) validateDeviceLogin(add func(string, ...any)) {
	d := p.DeviceCodePatterns
	if len(p.DeviceLoginArgs) == 0 {
		if d != nil {
			add("device_code_patterns is set with no device_login_args: nothing prints a code")
		}
		return
	}
	if d == nil || d.URL == "" || d.Code == "" {
		add("device_login_args is set but device_code_patterns.url or .code is empty: " +
			"the engine relays only the address and the code, and has to find both")
		return
	}
	for _, pattern := range []struct{ field, expr string }{
		{"url", d.URL}, {"code", d.Code}, {"lifetime", d.Lifetime},
	} {
		if pattern.expr == "" {
			continue
		}
		re, err := regexp.Compile(pattern.expr)
		if err != nil {
			add("device_code_patterns.%s: %v", pattern.field, err)
			continue
		}
		if re.NumSubexp() < 1 {
			add("device_code_patterns.%s %q has no capture group: the first group is the value",
				pattern.field, pattern.expr)
		}
	}
}

// validateEventReports holds [Profile.LimitResetEvents], [Profile.AuthEvents]
// and [Profile.LimitWindows] to the shape [Profile.validateLimitEvents] holds
// a limit event to: matched on fields the harness writes, never on the model's
// words.
func (p *Profile) validateEventReports(add func(string, ...any)) {
	if (len(p.LimitResetEvents) > 0 || len(p.AuthEvents) > 0) &&
		(p.EffectiveOutput() != OutputJSONL || len(p.EventTypePath) == 0) {
		add("limit_reset_events and auth_events need output: %s and an event_type_path: on "+
			"any other output a line of the model's answer is a line of stdout", OutputJSONL)
	}
	discriminated := func(what string, match []Match) {
		named := slices.ContainsFunc(match, func(m Match) bool {
			return slices.Equal(m.Path, p.EventTypePath)
		})
		if !named || len(match) < 2 {
			add("%s.match must name the event type at event_type_path and at least one "+
				"field the harness writes beside it", what)
		}
	}
	for i, e := range p.LimitResetEvents {
		what := fmt.Sprintf("limit_reset_events[%d]", i)
		discriminated(what, e.Match)
		if len(e.ResetsAt.Path) == 0 && len(e.Window.Path) == 0 {
			add("%s reads neither resets_at nor window, so it says nothing", what)
		}
		if len(e.ResetsAt.Path) > 0 && !e.ResetsAt.Unit.Valid() {
			add("%s.resets_at.unit %q (want %s, %s or %s)", what, e.ResetsAt.Unit,
				InstantEpoch, InstantEpochMS, InstantRFC3339)
		}
		if len(e.Window.Path) == 0 && len(e.Window.Map) > 0 {
			add("%s.window.map is set with no window.path", what)
		}
		for _, name := range slices.Sorted(maps.Keys(e.Window.Map)) {
			if w := e.Window.Map[name]; !w.Valid() {
				add("%s.window.map[%q] is %q (want %s, %s, %s or %s)", what, name, w,
					llm.LimitSession, llm.LimitWeekly, llm.LimitModel, llm.LimitUnknown)
			}
		}
	}
	for i, e := range p.AuthEvents {
		discriminated(fmt.Sprintf("auth_events[%d]", i), e.Match)
	}
	for i, w := range p.LimitWindows {
		if problem := sentinelProblem(w.Contains); problem != "" {
			add("limit_windows[%d].contains %s", i, problem)
		}
		if !w.Window.Valid() {
			add("limit_windows[%d].window %q (want %s, %s, %s or %s)", i, w.Window,
				llm.LimitSession, llm.LimitWeekly, llm.LimitModel, llm.LimitUnknown)
		}
	}
}

// cloneSignIn copies the fields of this file a clone must not share.
func (p Profile) cloneSignIn(out *Profile) {
	if p.LimitResetEvents != nil {
		out.LimitResetEvents = make([]LimitResetEvent, len(p.LimitResetEvents))
		for i, e := range p.LimitResetEvents {
			window := EventWindow{Path: slices.Clone(e.Window.Path)}
			if e.Window.Map != nil {
				window.Map = maps.Clone(e.Window.Map)
			}
			out.LimitResetEvents[i] = LimitResetEvent{Match: cloneMatches(e.Match),
				ResetsAt: EventInstant{Path: slices.Clone(e.ResetsAt.Path), Unit: e.ResetsAt.Unit},
				Window:   window}
		}
	}
	out.LimitWindows = slices.Clone(p.LimitWindows)
	if p.AuthEvents != nil {
		out.AuthEvents = make([]AuthEvent, len(p.AuthEvents))
		for i, e := range p.AuthEvents {
			out.AuthEvents[i] = AuthEvent{Match: cloneMatches(e.Match), SaidPath: slices.Clone(e.SaidPath)}
		}
	}
	out.SignInEnv = slices.Clone(p.SignInEnv)
	out.IdentityEnv = slices.Clone(p.IdentityEnv)
	out.DeviceLoginArgs = slices.Clone(p.DeviceLoginArgs)
	if p.DeviceCodePatterns != nil {
		d := *p.DeviceCodePatterns
		out.DeviceCodePatterns = &d
	}
	out.StatusTextArgs = slices.Clone(p.StatusTextArgs)
	if p.StatusFields != nil {
		out.StatusFields = &StatusFields{Method: clonePaths(p.StatusFields.Method),
			Account: clonePaths(p.StatusFields.Account)}
	}
	out.ModelAliases = slices.Clone(p.ModelAliases)
}
