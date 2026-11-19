package config

import "errors"

// Advisor is the Advisor's one setting: the model it runs on (ADR-0164). Its
// detectors run whether or not one is assigned; the model only explains and
// adds what no detector names. Everything else is a constant beside its use
// (internal/advisor/duty and internal/advisor/modelpass).
type Advisor struct {
	Model string `yaml:"model,omitempty" json:"model,omitempty" desc:"The providers.llm entry the Advisor's model runs on: it explains findings and adds what no rule names, each checked against the engine's records before it is listed. Absent: the Advisor's rules still run, and its model does not."`
}

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
	if c.Advisor.Model == "" {
		return nil
	}
	return c.helperModelProblem(field("advisor.model"), c.Advisor.Model)
}
