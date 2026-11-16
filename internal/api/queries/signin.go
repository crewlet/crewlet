// How the company's people sign in, as this node enforces it, for the
// administrators who answer for it.

package queries

import (
	"context"
	"time"

	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam"
)

// SignInPolicy is a sign-in policy in force: whether a second factor is
// required, the password floor, and how long a session and a proof of
// identity last, every duration in whole seconds.
type SignInPolicy struct {
	// SecondFactor is `required` or `optional`.
	SecondFactor iam.SecondFactor `json:"second_factor"`
	// MinPasswordLength is the floor every new password is held to.
	MinPasswordLength int `json:"min_password_length"`
	// SessionAbsoluteSeconds is the longest a session lives from sign-in.
	SessionAbsoluteSeconds int64 `json:"session_absolute_seconds"`
	// SessionIdleSeconds is how long a session lives unused.
	SessionIdleSeconds int64 `json:"session_idle_seconds"`
	// StepUpSeconds is how long a proof of identity authorises a sensitive
	// change before it is asked for again.
	StepUpSeconds int64 `json:"step_up_seconds"`
}

// SignInLimits are this node's operator limits on the company's policy
// (`api.auth.limits`), each with its default applied.
type SignInLimits struct {
	// SecondFactor is `required` (the company may not drop the second
	// factor) or `optional` (it may).
	SecondFactor iam.SecondFactor `json:"second_factor"`
	// MinPasswordLength is the shortest floor the company may set.
	MinPasswordLength int `json:"min_password_length"`
	// MaxSessionLifetimeSeconds, MaxIdleTimeoutSeconds and MaxStepUpSeconds
	// are the longest values the company may set.
	MaxSessionLifetimeSeconds int64 `json:"max_session_lifetime_seconds"`
	MaxIdleTimeoutSeconds     int64 `json:"max_idle_timeout_seconds"`
	MaxStepUpSeconds          int64 `json:"max_step_up_seconds"`
}

// SignInPolicyAnswer is what `sign_in_policy` answers (ADR-0152): the company's
// settings, each range narrowed to this node's Limits, and the Policy the node
// enforces, lower only where a limit holds it there.
type SignInPolicyAnswer struct {
	// Node is the node whose limits and enforcement these are.
	Node   string       `json:"node"`
	Policy SignInPolicy `json:"policy"`
	Limits SignInLimits `json:"limits"`
	// Settings are the numeric settings, in the order a screen draws them.
	Settings []LimitRow `json:"settings"`
	// SecondFactorSetting is `sign_in.require_second_factor`.
	SecondFactorSetting SettingRow `json:"second_factor_setting"`
}

// signInRows are the sign-in policy's numeric settings, read through the one
// accessor every enforcer of them reads ([config.SignIn.Policy]).
var signInRows = []limit{
	{key: config.SignInPasswordKey, group: LimitSignIn, unit: LimitCount,
		read: func(c *config.Company) float64 { return float64(c.SignIn.Policy().MinPasswordLength) }},
	{key: config.SignInSessionLifetimeKey, group: LimitSignIn, unit: LimitSeconds,
		read: func(c *config.Company) float64 { return c.SignIn.Policy().SessionLifetime.Seconds() }},
	{key: config.SignInIdleTimeoutKey, group: LimitSignIn, unit: LimitSeconds,
		read: func(c *config.Company) float64 { return c.SignIn.Policy().IdleTimeout.Seconds() }},
	{key: config.SignInStepUpKey, group: LimitSignIn, unit: LimitSeconds,
		read: func(c *config.Company) float64 { return c.SignIn.Policy().StepUp.Seconds() }},
}

// SignInPolicyOf is the answer for company c (nil before a node has one) on a
// node whose `api.auth` is auth.
func SignInPolicyOf(node string, auth *config.APIAuth, c *config.Company) SignInPolicyAnswer {
	company := config.Company{}
	if c != nil {
		company = *c
	}
	l := auth.Limits
	rows := limitRows(&company, signInRows)
	for i := range rows {
		if r, ok := l.Range(rows[i].Key); ok {
			rows[i].Range = &r
		}
	}
	enforced := auth.SignInPolicy(c)
	required := company.SignIn.Policy().SecondFactor == iam.SecondFactorRequired
	return SignInPolicyAnswer{
		Node: node,
		Policy: SignInPolicy{
			SecondFactor:           enforced.SecondFactor,
			MinPasswordLength:      enforced.MinPasswordLength,
			SessionAbsoluteSeconds: seconds(enforced.SessionLifetime),
			SessionIdleSeconds:     seconds(enforced.IdleTimeout),
			StepUpSeconds:          seconds(enforced.StepUp),
		},
		Limits: SignInLimits{
			SecondFactor:              cmpSecondFactor(l.OptionalSecondFactor()),
			MinPasswordLength:         l.PasswordFloor(),
			MaxSessionLifetimeSeconds: seconds(l.MaxSessionLifetime()),
			MaxIdleTimeoutSeconds:     seconds(l.MaxIdleTimeout()),
			MaxStepUpSeconds:          seconds(l.MaxStepUp()),
		},
		Settings: rows,
		SecondFactorSetting: SettingRow{
			Key: config.SignInSecondFactorKey, Group: SettingSignIn, Kind: SettingSwitch,
			Value: required, Default: true, Set: company.SignIn.RequireSecondFactor != nil,
		},
	}
}

// cmpSecondFactor is the second-factor limit as the wire spells it.
func cmpSecondFactor(optional bool) iam.SecondFactor {
	if optional {
		return iam.SecondFactorOptional
	}
	return iam.SecondFactorRequired
}

func seconds(d time.Duration) int64 { return int64(d / time.Second) }

// signInPolicy answers the sign-in policy of the company this node runs.
func (s Sources) signInPolicy(context.Context, Params) (any, error) {
	var company *config.Company
	if s.Company != nil {
		company, _ = s.Company()
	}
	return SignInPolicyOf(s.NodeID, s.SignIn, company), nil
}
