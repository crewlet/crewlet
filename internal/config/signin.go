package config

import (
	"fmt"
	"time"

	"github.com/crewlet/crewlet/internal/iam"
	"github.com/crewlet/crewlet/internal/words"
)

// SignIn is the company's sign-in policy (ADR-0152): the same on every node,
// changed without a restart, and held by each node within its operator's
// [SignInLimits]. Every field left out is the engine's default.
type SignIn struct {
	// RequireSecondFactor is whether a password sign-in must also give a
	// second factor. Unset is required.
	RequireSecondFactor *bool `yaml:"require_second_factor,omitempty" json:"require_second_factor,omitempty" desc:"Whether a person signing in with a password must also give a second factor. Empty is true. false needs api.auth.limits.second_factor: optional on the node."`

	// MinPasswordLength is the floor every new password is held to.
	MinPasswordLength *int `yaml:"min_password_length,omitempty" json:"min_password_length,omitempty" desc:"Shortest password a person may set: 12 to 256 characters. Empty is 12."`

	// SessionLifetimeSeconds is the longest a session lives from sign-in.
	SessionLifetimeSeconds *float64 `yaml:"session_lifetime_seconds,omitempty" json:"session_lifetime_seconds,omitempty" desc:"Longest a session lives from sign-in, however it is used: 3600 to 2592000 seconds (1 hour to 30 days). Empty is 604800 (7 days)."`

	// IdleTimeoutSeconds is how long a session lives unused.
	IdleTimeoutSeconds *float64 `yaml:"idle_timeout_seconds,omitempty" json:"idle_timeout_seconds,omitempty" desc:"How long a session lives unused before its person signs in again: 900 to 2592000 seconds (15 minutes to 30 days). Empty is 43200 (12 hours)."`

	// StepUpSeconds is how long a proof of identity authorises a sensitive
	// change before it is asked for again.
	StepUpSeconds *float64 `yaml:"step_up_seconds,omitempty" json:"step_up_seconds,omitempty" desc:"How long a proof of identity authorises a sensitive change before it is asked again: 300 to 86400 seconds (5 minutes to 1 day). Empty is 3600 (1 hour)."`
}

// The sign-in defaults and the engine's bounds around them. Each bound is the
// widest any operator may allow; [SignInLimits] narrows it per node.
const (
	// DefaultSessionLifetime is the working week.
	DefaultSessionLifetime = 168 * time.Hour
	// SessionLifetimeFloor is the default step-up window: below it a person is
	// signed out inside the window a proof grants.
	SessionLifetimeFloor = time.Hour
	// SessionLifetimeCeiling is a month, past which a stolen cookie works for
	// longer than anybody notices a lost laptop.
	SessionLifetimeCeiling = 720 * time.Hour

	// DefaultIdleTimeout is a working day and its evening: a laptop left open
	// overnight is signed out by morning.
	DefaultIdleTimeout = 12 * time.Hour
	// IdleTimeoutFloor is the shortest idle limit compliance regimes ask for
	// (PCI DSS 8.2.8); shorter signs out people who are reading.
	IdleTimeoutFloor = 15 * time.Minute
	// IdleTimeoutCeiling is the lifetime's: past it the idle limit never fires.
	IdleTimeoutCeiling = SessionLifetimeCeiling

	// DefaultStepUp keeps the stricter end of sudo-mode practice.
	DefaultStepUp = time.Hour
	// StepUpFloor: below five minutes a person proves again between screens.
	StepUpFloor = 5 * time.Minute
	// StepUpCeiling: past a day a step-up asks for nothing.
	StepUpCeiling = 24 * time.Hour
)

// SignInPolicy is a sign-in policy resolved: every default applied, and on a
// node, held within its limits ([APIAuth.SignInPolicy]).
type SignInPolicy struct {
	SecondFactor      iam.SecondFactor
	MinPasswordLength int
	SessionLifetime   time.Duration
	IdleTimeout       time.Duration
	StepUp            time.Duration
}

// DefaultSignInPolicy is the engine's sign-in policy: a company that says
// nothing, on a node that limits nothing.
func DefaultSignInPolicy() SignInPolicy { return SignIn{}.Policy() }

// Policy is the company's choice with the engine's defaults applied.
func (s SignIn) Policy() SignInPolicy {
	p := SignInPolicy{
		SecondFactor:      iam.SecondFactorRequired,
		MinPasswordLength: iam.MinPasswordChars,
		SessionLifetime:   DefaultSessionLifetime,
		IdleTimeout:       DefaultIdleTimeout,
		StepUp:            DefaultStepUp,
	}
	if s.RequireSecondFactor != nil && !*s.RequireSecondFactor {
		p.SecondFactor = iam.SecondFactorOptional
	}
	if s.MinPasswordLength != nil {
		p.MinPasswordLength = *s.MinPasswordLength
	}
	if s.SessionLifetimeSeconds != nil {
		p.SessionLifetime = secondsOf(*s.SessionLifetimeSeconds)
	}
	if s.IdleTimeoutSeconds != nil {
		p.IdleTimeout = secondsOf(*s.IdleTimeoutSeconds)
	}
	if s.StepUpSeconds != nil {
		p.StepUp = secondsOf(*s.StepUpSeconds)
	}
	return p
}

func secondsOf(v float64) time.Duration { return time.Duration(v * float64(time.Second)) }

// The keys of the sign-in policy in the company document.
const (
	SignInSecondFactorKey    = "sign_in.require_second_factor"
	SignInPasswordKey        = "sign_in.min_password_length"
	SignInSessionLifetimeKey = "sign_in.session_lifetime_seconds"
	SignInIdleTimeoutKey     = "sign_in.idle_timeout_seconds"
	SignInStepUpKey          = "sign_in.step_up_seconds"
)

// Why each bound exists, for the operator reading a refusal.
const (
	signInPasswordWhy = "below 12 characters a password is guessable, and nobody could set one past 256"
	signInLifetimeWhy = "under an hour a person is signed out inside one step-up, and past a month a " +
		"stolen session outlives anybody noticing"
	signInIdleWhy = "under 15 minutes people are signed out while reading, and past 30 days the " +
		"lifetime ends a session first"
	signInStepUpWhy = "under five minutes people confirm it's them between screens, and past a day " +
		"a step-up protects nothing"
)

func (s SignIn) validate(path Path) error {
	var p problems
	if v := s.MinPasswordLength; v != nil {
		p.within(at(path, "min_password_length"), float64(*v), signInPasswordWhy)
	}
	if v := s.SessionLifetimeSeconds; v != nil {
		p.within(at(path, "session_lifetime_seconds"), *v, signInLifetimeWhy)
	}
	if v := s.IdleTimeoutSeconds; v != nil {
		p.within(at(path, "idle_timeout_seconds"), *v, signInIdleWhy)
	}
	if v := s.StepUpSeconds; v != nil {
		p.within(at(path, "step_up_seconds"), *v, signInStepUpWhy)
	}
	return p.err()
}

// SignInLimits are the operator's bounds on the company's sign-in policy,
// `api.auth.limits` in each node's own file (ADR-0152): a company value past
// one is refused by that node's writes and held to the limit by its sign-ins.
type SignInLimits struct {
	// SecondFactor is `required` (the default: the company may not drop the
	// second factor) or `optional` (it may). `optional` off loopback needs
	// [APIAuth.AcceptInsecure].
	SecondFactor iam.SecondFactor `yaml:"second_factor,omitempty" json:"second_factor,omitempty" js:"enum=required|optional" desc:"Whether the company may let people sign in with a password alone (optional) or must require a second factor (required, the default). optional off loopback needs accept_insecure."`

	// MinPasswordLength is the shortest password floor the company may set.
	MinPasswordLength int `yaml:"min_password_length,omitempty" json:"min_password_length,omitempty" js:"min=0;max=256" desc:"Shortest password floor the company may set (default 12, 12..256)."`

	// MaxSessionLifetimeRaw is the longest session lifetime the company may set.
	MaxSessionLifetimeRaw string `yaml:"max_session_lifetime,omitempty" json:"max_session_lifetime,omitempty" desc:"Longest session lifetime the company may set (default 720h, 1h..720h)."`

	// MaxIdleTimeoutRaw is the longest idle limit the company may set.
	MaxIdleTimeoutRaw string `yaml:"max_idle_timeout,omitempty" json:"max_idle_timeout,omitempty" desc:"Longest idle limit the company may set (default 720h, 15m..720h)."`

	// MaxStepUpRaw is the longest step-up window the company may set.
	MaxStepUpRaw string `yaml:"max_step_up,omitempty" json:"max_step_up,omitempty" desc:"Longest step-up window the company may set (default 24h, 5m..24h)."`
}

// IsZero lets an unset block drop out of a JSON round trip.
func (l SignInLimits) IsZero() bool { return l == SignInLimits{} }

// OptionalSecondFactor reports whether the company may let people sign in
// with a password alone. A value this build does not know reads as no.
func (l SignInLimits) OptionalSecondFactor() bool {
	return l.SecondFactor == iam.SecondFactorOptional
}

// PasswordFloor is the shortest floor the company may set, default applied.
func (l SignInLimits) PasswordFloor() int {
	return max(l.MinPasswordLength, iam.MinPasswordChars)
}

// MaxSessionLifetime is the longest lifetime the company may set.
func (l SignInLimits) MaxSessionLifetime() time.Duration {
	return durationOr(l.MaxSessionLifetimeRaw, SessionLifetimeCeiling)
}

// MaxIdleTimeout is the longest idle limit the company may set.
func (l SignInLimits) MaxIdleTimeout() time.Duration {
	return durationOr(l.MaxIdleTimeoutRaw, IdleTimeoutCeiling)
}

// MaxStepUp is the longest step-up window the company may set.
func (l SignInLimits) MaxStepUp() time.Duration {
	return durationOr(l.MaxStepUpRaw, StepUpCeiling)
}

// Hold is p held within the limits: what a node enforces when the company's
// choice lies past one (an operator who lowered a limit after it was set).
func (l SignInLimits) Hold(p SignInPolicy) SignInPolicy {
	if !l.OptionalSecondFactor() {
		p.SecondFactor = iam.SecondFactorRequired
	}
	p.MinPasswordLength = max(p.MinPasswordLength, l.PasswordFloor())
	p.SessionLifetime = min(p.SessionLifetime, l.MaxSessionLifetime())
	p.IdleTimeout = min(p.IdleTimeout, l.MaxIdleTimeout())
	p.StepUp = min(p.StepUp, l.MaxStepUp())
	return p
}

// Range is the values key admits on this node: the engine's range narrowed to
// the limit, marked where the limit is what bounds it.
func (l SignInLimits) Range(key string) (Range, bool) {
	r, ok := RangeOf(key)
	if !ok {
		return Range{}, false
	}
	narrow := func(most time.Duration) {
		if v := most.Seconds(); r.Max == nil || v < *r.Max {
			r.Max, r.OperatorMax = atMost(v), true
		}
	}
	switch key {
	case SignInPasswordKey:
		if v := float64(l.PasswordFloor()); v > r.Min {
			r.Min, r.OperatorMin = v, true
		}
	case SignInSessionLifetimeKey:
		narrow(l.MaxSessionLifetime())
	case SignInIdleTimeoutKey:
		narrow(l.MaxIdleTimeout())
	case SignInStepUpKey:
		narrow(l.MaxStepUp())
	}
	return r, true
}

// Admit refuses each sign-in value next changes from prior that lies past
// these limits. A value prior already held is left alone, so an operator
// lowering a limit refuses no unrelated write.
func (l SignInLimits) Admit(next, prior *Company) error {
	if next == nil {
		return nil
	}
	var was SignIn
	if prior != nil {
		was = prior.SignIn
	}
	now := next.SignIn
	var p problems
	if changed(now.RequireSecondFactor, was.RequireSecondFactor) &&
		now.Policy().SecondFactor == iam.SecondFactorOptional && !l.OptionalSecondFactor() {
		p.addSaying(field(SignInSecondFactorKey), ErrConflict,
			"This node's operator requires two-step verification for every password sign-in.",
			"false is past this node's limit: api.auth.limits.second_factor is required. Set it "+
				"to optional in the node's file to let the company drop the second factor")
	}
	if v := now.MinPasswordLength; v != nil && changed(v, was.MinPasswordLength) {
		l.admit(&p, SignInPasswordKey, float64(*v), "api.auth.limits.min_password_length",
			fmt.Sprintf("This node's operator asks for passwords of at least %s.",
				words.Count(l.PasswordFloor(), "character", "characters")))
	}
	limits := []struct {
		key, limit, what string
		now, was         *float64
		most             time.Duration
	}{
		{SignInSessionLifetimeKey, "api.auth.limits.max_session_lifetime", "sessions",
			now.SessionLifetimeSeconds, was.SessionLifetimeSeconds, l.MaxSessionLifetime()},
		{SignInIdleTimeoutKey, "api.auth.limits.max_idle_timeout", "an idle session",
			now.IdleTimeoutSeconds, was.IdleTimeoutSeconds, l.MaxIdleTimeout()},
		{SignInStepUpKey, "api.auth.limits.max_step_up", "a proof of identity",
			now.StepUpSeconds, was.StepUpSeconds, l.MaxStepUp()},
	}
	for _, c := range limits {
		if c.now != nil && changed(c.now, c.was) {
			l.admit(&p, c.key, *c.now, c.limit, fmt.Sprintf(
				"This node's operator allows %s to last at most %s.", c.what, DurationWords(c.most)))
		}
	}
	return p.err()
}

// admit records a fault for a value the engine's range admits and this node's
// limit does not; a value outside the engine's range is the validator's.
func (l SignInLimits) admit(p *problems, key string, v float64, limit, said string) {
	engine, _ := RangeOf(key)
	r, _ := l.Range(key)
	if !engine.Admits(v) || r.Admits(v) {
		return
	}
	*p = append(*p, &Fault{
		Path: field(key), Kind: ErrOutOfRange, Range: &r, Said: said,
		Detail: r.refusal(v) + ": past this node's " + limit,
	})
}

// changed reports whether a pointer setting differs between two documents.
func changed[T comparable](now, was *T) bool {
	if now == nil || was == nil {
		return now != was
	}
	return *now != *was
}

// DurationWords is a duration as a person reads it: whole days, hours or
// minutes, the largest unit that divides it.
func DurationWords(d time.Duration) string {
	switch {
	case d >= 24*time.Hour && d%(24*time.Hour) == 0:
		return words.Count(int(d/(24*time.Hour)), "day", "days")
	case d >= time.Hour && d%time.Hour == 0:
		return words.Count(int(d/time.Hour), "hour", "hours")
	case d >= time.Minute && d%time.Minute == 0:
		return words.Count(int(d/time.Minute), "minute", "minutes")
	}
	return d.String()
}

// SignInWeakens warns of each sign-in value a write weakens from prior's: what
// the change allows, for the person confirming it. A transition, raised by the
// write that makes it, as [ClockMoves] is.
func SignInWeakens(next, prior *Company) []Warning {
	if next == nil || prior == nil {
		return nil
	}
	was, now := prior.SignIn.Policy(), next.SignIn.Policy()
	var out []Warning
	if was.SecondFactor == iam.SecondFactorRequired && now.SecondFactor == iam.SecondFactorOptional {
		out = append(out, passwordAloneAdvice.raise(field(SignInSecondFactorKey), "This lets people sign in with "+
			"a password alone. Anyone who learns a password can then act as that person."))
	}
	if now.MinPasswordLength < was.MinPasswordLength {
		from := words.Count(was.MinPasswordLength, "character", "characters")
		to := words.Count(now.MinPasswordLength, "character", "characters")
		w := shorterPasswordsAdvice.raise(field(SignInPasswordKey), fmt.Sprintf("This lets people set "+
			"passwords of %s instead of %s, which are easier to guess.", to, from))
		w.Item = from + " to " + to
		out = append(out, w)
	}
	longer := func(a advice, key string, was, now time.Duration, what string) {
		if now > was {
			w := a.raise(field(key), fmt.Sprintf(what, DurationWords(now), DurationWords(was)))
			w.Item = DurationWords(was) + " to " + DurationWords(now)
			out = append(out, w)
		}
	}
	longer(longerSessionAdvice, SignInSessionLifetimeKey, was.SessionLifetime, now.SessionLifetime,
		"This lets a session last %s instead of %s, so a stolen session works for longer. "+
			"Sessions open now keep their shorter lifetime.")
	longer(longerIdleAdvice, SignInIdleTimeoutKey, was.IdleTimeout, now.IdleTimeout,
		"This keeps an unused session signed in for %s instead of %s, so a computer left "+
			"open stays signed in for longer. Sessions open now keep their shorter limit.")
	longer(longerStepUpAdvice, SignInStepUpKey, was.StepUp, now.StepUp,
		"This lets one proof of identity authorise sensitive changes for %s instead of %s, "+
			"so a session left open can make them for longer.")
	return out
}

// The words of [SignInWeakens], one per value it weakens; each warning's
// Item is the value it moves from and the one it moves to.
var (
	passwordAloneAdvice = says("A password alone is enough to sign in",
		"People can sign in with a password alone, so anyone who learns a password can act as "+
			"that person.")
	shorterPasswordsAdvice = says("Shorter passwords are allowed",
		"People can choose shorter passwords, which are easier to guess.")
	longerSessionAdvice = says("Sessions last longer",
		"A stolen session works for longer. Sessions open now keep their shorter lifetime.")
	longerIdleAdvice = says("Idle sessions stay signed in longer",
		"A computer left open stays signed in for longer. Sessions open now keep their shorter limit.")
	longerStepUpAdvice = says("Sensitive changes need proof less often",
		"One proof of identity covers sensitive changes for longer, so a session left open can "+
			"make them for longer.")
)

// validateLimits checks the operator's limits against the engine's bounds.
func (l SignInLimits) validate(path Path) error {
	var p problems
	if l.SecondFactor != "" && !l.SecondFactor.Valid() {
		p.add(at(path, "second_factor"), ErrUnknownValue,
			"%q (want %s or %s; unset is %s)", l.SecondFactor, iam.SecondFactorRequired,
			iam.SecondFactorOptional, iam.SecondFactorRequired)
	}
	if n := l.MinPasswordLength; n != 0 && (n < iam.MinPasswordChars || n > MaxPasswordLength) {
		p.add(at(path, "min_password_length"), ErrOutOfRange,
			"%d is outside %d..%d. Remove the line to use the engine's floor of %d",
			n, iam.MinPasswordChars, MaxPasswordLength, iam.MinPasswordChars)
	}
	checkDurationRange(&p, at(path, "max_session_lifetime"), l.MaxSessionLifetimeRaw,
		SessionLifetimeFloor, SessionLifetimeCeiling, signInLifetimeWhy)
	checkDurationRange(&p, at(path, "max_idle_timeout"), l.MaxIdleTimeoutRaw,
		IdleTimeoutFloor, IdleTimeoutCeiling, signInIdleWhy)
	checkDurationRange(&p, at(path, "max_step_up"), l.MaxStepUpRaw,
		StepUpFloor, StepUpCeiling, signInStepUpWhy)
	return p.err()
}

// SignInPolicy is the sign-in policy this node enforces for company c: its
// choice, or the engine's defaults with no company yet, held within
// `api.auth.limits`.
func (a *APIAuth) SignInPolicy(c *Company) SignInPolicy {
	var s SignIn
	if c != nil {
		s = c.SignIn
	}
	return a.Limits.Hold(s.Policy())
}
