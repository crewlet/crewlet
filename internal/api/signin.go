package api

import (
	"github.com/crewlet/crewlet/internal/config"
	"github.com/crewlet/crewlet/internal/iam/session"
	"github.com/crewlet/crewlet/internal/org"
)

// signInPolicyOf is the company's sign-in policy as this node holds it, read
// on every call (ADR-0152): the company company returns, or the engine's
// defaults before there is one, held within boot's `api.auth.limits`.
func signInPolicyOf(boot *config.Bootstrap, company func() *config.Company) func() config.SignInPolicy {
	if boot == nil {
		return config.DefaultSignInPolicy
	}
	auth := &boot.API.Auth
	return func() config.SignInPolicy { return auth.SignInPolicy(company()) }
}

// companyOf is the company of a read source, nil while there is none.
func companyOf(source func() (*config.Company, *org.Organization)) func() *config.Company {
	return func() *config.Company {
		if source == nil {
			return nil
		}
		c, _ := source()
		return c
	}
}

// sessionLimits are the lifetime and idle limits of a sign-in policy.
func sessionLimits(policy func() config.SignInPolicy) func() session.Limits {
	return func() session.Limits {
		p := policy()
		return session.Limits{Lifetime: p.SessionLifetime, Idle: p.IdleTimeout}
	}
}
