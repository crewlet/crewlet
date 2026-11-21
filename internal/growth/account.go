package growth

import "slices"

// AccountState is whether a vendor serves an account, as its page and the
// growth sources show it. Spend tools refuse an account that is not serving.
type AccountState string

// The account states.
const (
	AccountServing    AccountState = "serving"
	AccountPaused     AccountState = "paused"
	AccountSuspended  AccountState = "suspended"
	AccountClosed     AccountState = "closed"
	AccountUnapproved AccountState = "unapproved"
	AccountUnknown    AccountState = "unknown"
)

// AccountStates is every account state.
func AccountStates() []AccountState {
	return []AccountState{AccountServing, AccountPaused, AccountSuspended, AccountClosed, AccountUnapproved,
		AccountUnknown}
}

// Valid reports whether s is an account state.
func (s AccountState) Valid() bool { return slices.Contains(AccountStates(), s) }

// Serves reports whether the vendor serves an account in state s: a paused
// account serves once a change enables a campaign in it.
func (s AccountState) Serves() bool { return s == AccountServing || s == AccountPaused }

// Account is one vendor account a connection reaches, as a pass reads it.
type Account struct {
	Kind string `json:"product"`
	// Connection is the block's connection that reaches it.
	Connection string `json:"connection,omitempty"`
	ID         string `json:"account"`
	// Name is the vendor's name for it, somebody's words.
	Name     string       `json:"account_name" untrusted:"name"`
	Currency string       `json:"currency,omitempty"`
	TimeZone string       `json:"time_zone,omitempty"`
	State    AccountState `json:"state"`
	// Roles are the vendor roles the connection holds on it.
	Roles []string `json:"roles,omitempty"`
}
