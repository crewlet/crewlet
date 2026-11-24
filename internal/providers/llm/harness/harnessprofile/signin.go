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

// LimitReport reads where the vendor's limit windows stand from the harness's own
// event fields (Claude Code's `rate_limit_event`). It classifies nothing: the last
// report a run wrote whose window is reached dates that run's limit.
type LimitReport struct {
	// Match picks the event out; see [Match].
	Match []Match `yaml:"match,omitempty"`

	// State is where the event says its own window stands, and how the
	// vendor's words map onto [llm.LimitState].
	State EventState `yaml:"state,omitempty"`

	// Window is where the event names its own window, and how the vendor's
	// names map onto the engine's; an unmapped name dates a limit as unknown.
	Window EventWindow `yaml:"window,omitempty"`

	// Used is where the event carries the fraction of its window used.
	Used Path `yaml:"used,omitempty"`

	// ResetsAt is where the event carries the instant its window turns over.
	ResetsAt EventInstant `yaml:"resets_at,omitempty"`

	// Windows reads every window at once from an object keyed by the vendor's
	// names; optional.
	Windows *EventWindows `yaml:"windows,omitempty"`
}

// EventState is where an event names a state, and the map from the vendor's
// words to the engine's.
type EventState struct {
	Path Path                      `yaml:"path,omitempty"`
	Map  map[string]llm.LimitState `yaml:"map,omitempty"`
}

// EventWindows is an object of windows keyed by the vendor's names, each read
// at the same paths within its own entry.
type EventWindows struct {
	Path     Path         `yaml:"path,omitempty"`
	State    Path         `yaml:"state,omitempty"`
	Used     Path         `yaml:"used,omitempty"`
	ResetsAt EventInstant `yaml:"resets_at,omitempty"`
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

// EventWindow is where an event names its window, and the map from the
// vendor's names to the engine's.
type EventWindow struct {
	Path Path                  `yaml:"path,omitempty"`
	Map  map[string]WindowName `yaml:"map,omitempty"`
}

// WindowName is one of the vendor's windows in the engine's words: its kind,
// its length in hours where the vendor's name says it, and the model family a
// [llm.LimitModel] window is of.
type WindowName struct {
	Window llm.LimitWindow `yaml:"window,omitempty"`
	Hours  int             `yaml:"hours,omitempty"`
	Model  string          `yaml:"model,omitempty"`
}

// LimitWindowWording names the window of a limit from the vendor's sentence,
// for a report no [LimitReport] named one for: "weekly limit" is
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

	// In is where the report is looked for ([EventSource]): stdout by default.
	In EventSource `yaml:"in,omitempty"`
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
	// Model and Version are what the alias resolves to on the vendor's own
	// service, for the builds WrittenFor names; both empty where not known.
	Model   string `yaml:"model,omitempty"`
	Version string `yaml:"version,omitempty"`
}

// namesVersion reports whether a model id carries a version label, as
// `5.5` or as `5-5`, so the label cannot drift from the id it is read for.
func namesVersion(model, version string) bool {
	return strings.Contains(model, version) || strings.Contains(model, strings.ReplaceAll(version, ".", "-"))
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

// CredentialEnvNames are the variables that sign this harness in for an entry
// whose env is env: the headless token's and the API key's, the names it signs
// in by from an entry's env ([Profile.SignInEnv]), a cloud provider's switch
// or a gateway's token, which authenticate a box as surely as a key does, and,
// for a harness that reads its model provider's key from its own environment
// ([Profile.EnvSignIn]), the credential-named variables env sets.
//
// ONE LIST, read by name and never by value, for the two that must agree on
// it: a launch judging whether anything in a run's environment signs the
// harness in, and config refusing a seat's own vendor credential in its box
// under a name that would sign the seat's executor in instead.
func (p *Profile) CredentialEnvNames(env map[string]string) []string {
	names := make([]string, 0, 2+len(p.SignInEnv))
	for _, name := range []string{p.TokenEnv, p.APIKeyEnv} {
		if name != "" {
			names = append(names, name)
		}
	}
	for _, name := range p.SignInEnv {
		if !slices.Contains(names, name) {
			names = append(names, name)
		}
	}
	if p.EnvSignIn {
		for _, name := range slices.Sorted(maps.Keys(env)) {
			if IsCredentialName(name) && !slices.Contains(names, name) {
				names = append(names, name)
			}
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
		case p.signsIn(name):
			add("identity_env[%d] %q is a credential the profile signs in by (token_env, api_key_env, "+
				"a credential of sign_in_env, or under env_sign_in any credential-named variable), and an "+
				"entry's identity takes no credential: remove it from identity_env", i, name)
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
		switch {
		case (alias.Model == "") != (alias.Version == ""):
			add("model_aliases[%d] (%s) sets one of model and version: the label is the model's own version, so both are set or neither", i, alias.ID)
		case alias.Version != "" && !namesVersion(alias.Model, alias.Version):
			add("model_aliases[%d] (%s) labels model %q as version %q, which its id does not name", i, alias.ID, alias.Model, alias.Version)
		}
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

// validateEventReports holds [Profile.LimitReports], [Profile.AuthEvents]
// and [Profile.LimitWindows] to the shape [Profile.validateLimitEvents] holds
// a limit event to: matched on fields the harness writes, never on the model's
// words.
func (p *Profile) validateEventReports(add func(string, ...any)) {
	authOnStdout := slices.ContainsFunc(p.AuthEvents, func(e AuthEvent) bool { return e.In == EventsInStdout })
	if (len(p.LimitReports) > 0 || authOnStdout) &&
		(p.EffectiveOutput() != OutputJSONL || len(p.EventTypePath) == 0) {
		add("limit_reports and auth_events need output: %s and an event_type_path: on "+
			"any other output a line of the model's answer is a line of stdout", OutputJSONL)
	}
	names := func(match []Match) bool {
		return slices.ContainsFunc(match, func(m Match) bool { return slices.Equal(m.Path, p.EventTypePath) })
	}
	discriminated := func(what string, match []Match) {
		if !names(match) || len(match) < 2 {
			add("%s.match must name the event type at event_type_path and at least one "+
				"field the harness writes beside it", what)
		}
	}
	for i, e := range p.LimitReports {
		e.validate(fmt.Sprintf("limit_reports[%d]", i), names(e.Match), add)
	}
	for i, e := range p.AuthEvents {
		what := fmt.Sprintf("auth_events[%d]", i)
		if e.In != EventsInStdout {
			p.validateRecordEvent(what, e.In, e.Match, add)
			continue
		}
		discriminated(what, e.Match)
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

// validate holds one report to a shape that reads the harness's fields alone.
// The state map is the field beside the discriminator: an event whose state
// is not a word it holds is no report.
func (e LimitReport) validate(what string, named bool, add func(string, ...any)) {
	if !named {
		add("%s.match must name the event type at event_type_path", what)
	}
	if len(e.State.Path) == 0 || len(e.State.Map) == 0 {
		add("%s.state needs a path and a map: it is what makes an event a report", what)
	}
	for _, word := range slices.Sorted(maps.Keys(e.State.Map)) {
		if s := e.State.Map[word]; !s.Valid() {
			add("%s.state.map[%q] is %q (want %s, %s or %s)", what, word, s,
				llm.LimitOK, llm.LimitWarning, llm.LimitReached)
		}
	}
	if len(e.Window.Path) == 0 {
		add("%s.window.path is empty, so no report names its window", what)
	}
	for _, name := range slices.Sorted(maps.Keys(e.Window.Map)) {
		w := e.Window.Map[name]
		if !w.Window.Valid() {
			add("%s.window.map[%q].window is %q (want %s, %s, %s or %s)", what, name, w.Window,
				llm.LimitSession, llm.LimitWeekly, llm.LimitModel, llm.LimitUnknown)
		}
		if w.Hours < 0 {
			add("%s.window.map[%q].hours is %d: a window's length is never negative", what, name, w.Hours)
		}
		if w.Model != "" && w.Window != llm.LimitModel {
			add("%s.window.map[%q].model is set on a %s window: only a %s window is one model's",
				what, name, w.Window, llm.LimitModel)
		}
	}
	instant := func(field string, at EventInstant) {
		if len(at.Path) > 0 && !at.Unit.Valid() {
			add("%s.%s.unit %q (want %s, %s or %s)", what, field, at.Unit,
				InstantEpoch, InstantEpochMS, InstantRFC3339)
		}
	}
	instant("resets_at", e.ResetsAt)
	if w := e.Windows; w != nil {
		if len(w.Path) == 0 {
			add("%s.windows.path is empty", what)
		}
		if len(w.State) == 0 && len(w.Used) == 0 && len(w.ResetsAt.Path) == 0 {
			add("%s.windows reads neither state, used nor resets_at, so it says nothing", what)
		}
		instant("windows.resets_at", w.ResetsAt)
	}
}

// cloneSignIn copies the fields of this file a clone must not share.
func (p Profile) cloneSignIn(out *Profile) {
	if p.LimitReports != nil {
		out.LimitReports = make([]LimitReport, len(p.LimitReports))
		for i, e := range p.LimitReports {
			out.LimitReports[i] = e.clone()
		}
	}
	out.LimitWindows = slices.Clone(p.LimitWindows)
	if p.AuthEvents != nil {
		out.AuthEvents = make([]AuthEvent, len(p.AuthEvents))
		for i, e := range p.AuthEvents {
			out.AuthEvents[i] = AuthEvent{Match: cloneMatches(e.Match), SaidPath: slices.Clone(e.SaidPath), In: e.In}
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

// clone copies a report so that no slice or map is shared.
func (e LimitReport) clone() LimitReport {
	out := LimitReport{Match: cloneMatches(e.Match),
		State:    EventState{Path: slices.Clone(e.State.Path), Map: maps.Clone(e.State.Map)},
		Window:   EventWindow{Path: slices.Clone(e.Window.Path), Map: maps.Clone(e.Window.Map)},
		Used:     slices.Clone(e.Used),
		ResetsAt: EventInstant{Path: slices.Clone(e.ResetsAt.Path), Unit: e.ResetsAt.Unit}}
	if w := e.Windows; w != nil {
		out.Windows = &EventWindows{Path: slices.Clone(w.Path), State: slices.Clone(w.State),
			Used:     slices.Clone(w.Used),
			ResetsAt: EventInstant{Path: slices.Clone(w.ResetsAt.Path), Unit: w.ResetsAt.Unit}}
	}
	return out
}

// signsIn reports whether name is a credential this profile signs in by: its
// token_env or api_key_env, a credential among its sign_in_env, where a switch
// is no credential ([Profile.RouteEnv]), or, under env_sign_in, any
// credential-named variable, each of which signs it in.
func (p *Profile) signsIn(name string) bool {
	return name == p.TokenEnv || name == p.APIKeyEnv ||
		((slices.Contains(p.SignInEnv, name) || p.EnvSignIn) && IsCredentialName(name))
}
