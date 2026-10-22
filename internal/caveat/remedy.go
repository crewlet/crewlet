package caveat

import (
	"slices"

	"github.com/crewlet/crewlet/internal/iam"
)

// # Where a person changes the limit that refused them (ADR-0060)
//
// A refusal a CONFIGURABLE LIMIT caused (a token budget spent) says what
// happened, and the person reading it then has to find the setting that
// caused it. A [Remedy] names that setting as a value beside the refusal's
// words: which [Setting] it is and, for one kept per seat, which seat. A
// person's surface resolves it to the screen and the section that edit it,
// and decides with its own write gate whether the reader may change it.
//
// A VALUE, NOT A SENTENCE OR AN ADDRESS. The words stay the refusal's, so a
// remedy adds nothing a person reads that the catalog did not write; and the
// engine names no dashboard address, because the dashboard's link table owns
// where a screen lives (`dashboard/src/lib/remedy.ts`), and a screen that moves
// moves there alone. Nor does a remedy name the grants that may change the
// setting: the screen it resolves to draws its editor behind the dashboard's
// own write gate (Admin, a lead inside their subtree, and the writers of a
// managed document), and the remedy is drawn behind that same gate, so the
// link and the editor it opens cannot disagree about who may change it.

// Setting is a configurable limit a remedy names: a named string type, so a
// value off the wire this build does not know is a value rather than a panic.
type Setting string

const (
	// SettingCompanyTokenBudget is the company's own ceilings, the
	// top-level `token_budget:` of the company document.
	SettingCompanyTokenBudget Setting = "company_token_budget"
	// SettingSeatTokenBudget is one seat's own ceilings, its role's
	// `token_budget:`; the remedy names the seat.
	SettingSeatTokenBudget Setting = "seat_token_budget"
	// SettingAssistantTokenBudget is the ceiling every person's Dashboard
	// Assistant spend is held to, `assistant.token_budget:`.
	SettingAssistantTokenBudget Setting = "assistant_token_budget"
	// SettingModelProviders is the company's models, its `providers.llm`
	// entries: where a model at its usage limit is given a fallback, or a
	// plan changed (ADR-0064).
	SettingModelProviders Setting = "model_providers"
)

// settings are every [Setting], in declaration order.
var settings = []Setting{SettingCompanyTokenBudget, SettingSeatTokenBudget,
	SettingAssistantTokenBudget, SettingModelProviders}

// Settings are every setting a remedy may name, in declaration order: the
// vocabulary the dashboard's LIMIT_SETTINGS mirrors.
func Settings() []Setting { return slices.Clone(settings) }

// Valid reports whether s is a setting this build knows.
func (s Setting) Valid() bool { return slices.Contains(settings, s) }

// Remedy is where a person changes the limit a refusal was caused by. The
// zero value is no remedy at all: a refusal no configurable limit caused.
type Remedy struct {
	Setting Setting `json:"setting"`
	// Seat is the handle of the seat whose own ceilings a
	// [SettingSeatTokenBudget] remedy names, and empty for every other.
	Seat string `json:"seat,omitempty"`
}

// IsZero reports whether r names no setting.
func (r Remedy) IsZero() bool { return r == Remedy{} }

// CompanyTokenBudget is the remedy of a refusal the company's own ceilings
// caused.
func CompanyTokenBudget() Remedy { return Remedy{Setting: SettingCompanyTokenBudget} }

// NO SEAT CONSTRUCTOR. Nothing the engine refuses a person for is a seat's
// ceiling: a seat's budget stop reaches a person as the live overlay of the
// seat's row, which names a seat by its agent id alone so that a rename never
// leaves a stale handle on a pushed row (livestate's OverlayRows), and a remedy
// naming the handle copied there would be exactly that stale copy. The
// dashboard composes a seat's remedy from the row's own current handle, in this
// same vocabulary.

// A SEAT AT A MODEL'S LIMIT is the same: it reaches a person as the seat's
// live `provider_limit`, and the dashboard composes the remedy from it, in
// this same vocabulary. A REFUSAL a model's limit caused has nothing live
// beside it, so it names the company's models itself ([ModelProviders]).

// ModelProviders is the remedy of a refusal a model's usage limit caused: the
// company's models, where the model is given a fallback or its plan changed
// (ADR-0064).
func ModelProviders() Remedy { return Remedy{Setting: SettingModelProviders} }

// AssistantTokenBudget is the remedy of a refusal a person's own assistant
// ceilings caused.
func AssistantTokenBudget() Remedy { return Remedy{Setting: SettingAssistantTokenBudget} }

// valid reports whether r is a remedy this build would write: a known
// setting, carrying a seat exactly when the setting is a seat's, and that seat
// a handle the chart could hold ([iam.ValidSeatHandle]), since the dashboard
// puts it in an address.
func (r Remedy) valid() bool {
	if r.Setting == SettingSeatTokenBudget {
		return iam.ValidSeatHandle(r.Seat)
	}
	return r.Setting.Valid() && r.Seat == ""
}
