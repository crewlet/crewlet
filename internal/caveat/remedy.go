package caveat

import "slices"

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
)

// settings are every [Setting], in declaration order.
var settings = []Setting{SettingCompanyTokenBudget, SettingSeatTokenBudget,
	SettingAssistantTokenBudget}

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

// SeatTokenBudget is the remedy of a refusal the ceilings of the seat handle
// caused.
func SeatTokenBudget(handle string) Remedy {
	return Remedy{Setting: SettingSeatTokenBudget, Seat: handle}
}

// AssistantTokenBudget is the remedy of a refusal a person's own assistant
// ceilings caused.
func AssistantTokenBudget() Remedy { return Remedy{Setting: SettingAssistantTokenBudget} }

// valid reports whether r is a remedy this build would write: a known
// setting, carrying a seat exactly when the setting is a seat's.
func (r Remedy) valid() bool {
	return r.Setting.Valid() && (r.Seat != "") == (r.Setting == SettingSeatTokenBudget)
}
