package config

import (
	"errors"
	"fmt"
)

// Advisor is the Advisor's one setting: the model it runs on (ADR-0164). Its
// detectors run whether or not one is assigned; the model only explains and
// adds what no detector names. Everything else is a constant beside its use
// (internal/advisor/duty and internal/advisor/modelpass).
type Advisor struct {
	Model string `yaml:"model,omitempty" json:"model,omitempty" desc:"The providers.llm entry the Advisor's model runs on: it explains findings and adds what no rule names, each checked against the engine's records before it is listed. Absent: the Advisor's rules still run, and its model does not."`
	// Deep bounds the Advisor's investigation on its model (ADR-0166).
	Deep AdvisorDeep `yaml:"deep,omitempty" json:"deep,omitzero" desc:"What one investigation by the Advisor's model may spend. It runs nightly, early on Monday before the weekly report, and when an administrator asks."`
}

// AdvisorDeep is what one deep run may spend; zero takes the default.
type AdvisorDeep struct {
	Ceiling int `yaml:"tokens,omitempty" json:"tokens,omitempty" desc:"The most tokens one investigation may spend, charged to the company and no seat: 100000 to 20000000. Absent: 2000000."`
	Minutes int `yaml:"minutes,omitempty" json:"minutes,omitempty" desc:"The most minutes one investigation may run: 1 to 120. Absent: 20."`
}

// The bounds an investigation's settings are held to.
const (
	MinAdvisorDeepTokens  = 100_000
	MaxAdvisorDeepTokens  = 20_000_000
	MaxAdvisorDeepMinutes = 120
)

// ErrAdvisorModelOff is what [Company.AdvisorModel] answers for a company that
// assigns the Advisor no model.
var ErrAdvisorModelOff = errors.New("advisor.model names no model, so the Advisor's model is off: " +
	"assign a providers.llm entry, e.g. model: assistant")

// AdvisorModel is the one place `advisor.model` is handed out: the entry, held
// to the rule both helpers' models are held to, or why there is none.
func (c *Company) AdvisorModel() (string, error) {
	key := c.Advisor.Model
	if key == "" {
		return "", ErrAdvisorModelOff
	}
	if err := c.validateAdvisor(); err != nil {
		return "", err
	}
	return key, nil
}

// validateAdvisor holds `advisor.model` to the one rule both helpers' models
// are held to ([Company.HelperModelProblem]): an admission rule, since no seat
// depends on it and the entry is judged again where it is handed out.
func (c *Company) validateAdvisor() error {
	if t := c.Advisor.Deep.Ceiling; t != 0 && (t < MinAdvisorDeepTokens || t > MaxAdvisorDeepTokens) {
		return fmt.Errorf("advisor.deep.tokens: %d is outside %d to %d", t, MinAdvisorDeepTokens, MaxAdvisorDeepTokens)
	}
	if m := c.Advisor.Deep.Minutes; m < 0 || m > MaxAdvisorDeepMinutes {
		return fmt.Errorf("advisor.deep.minutes: %d is outside 1 to %d", m, MaxAdvisorDeepMinutes)
	}
	if c.Advisor.Model == "" {
		return nil
	}
	return c.helperModelProblem(field("advisor.model"), c.Advisor.Model)
}
