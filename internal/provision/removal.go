package provision

import "slices"

// WHAT A TEARDOWN REMOVED, so the credentials it stranded can go with it.
// A teardown knows which seats it removed and, from each [Seat]'s `${VAR}`
// names, which secrets are now dead; only the engine can write the sealed
// store. Left sealed, a dead credential maps a reconnected seat to an account
// that no longer exists.
//
// An END STATE, not a delta: a retry after a failed secret deletion finds the
// accounts gone, so "already absent" counts as removed or the credentials stay.
// Only what is genuinely dead is named: a DISABLED account's token works again
// on re-enable. Slack's seats qualify though their apps cannot be deleted, as
// the owner can read back anything deleted here; a show-once credential like
// Datadog's key does not.

// Removal is one seat whose account a teardown removed.
type Removal struct {
	// Handle and Role name the seat, for the operator's report.
	Handle, Role string

	// Account is what the third-party app called it — a username, an
	// account id, a bot's @name — so a person can confirm at the app that
	// the right thing went.
	Account string

	// Secrets are the `${VAR}` names this seat's now-dead credentials live
	// in, which the engine deletes from the company's store.
	//
	// NAMES ONLY, and only this seat's OWN. A company-level credential —
	// an admin token, a webhook secret, an organization key — is never here:
	// those survive a disconnect by design, are reported to the operator
	// instead, and are the ones most likely to be shared with something
	// else.
	Secrets []string
}

// Removed is a teardown's whole end state.
type Removed struct{ Accounts []Removal }

// Secrets is every credential name this teardown stranded, sorted and
// deduplicated.
//
// TWO SEATS CAN NAME ONE VARIABLE — a company mid-migration, or one that has
// not given each agent its own account yet — and deleting it twice is a second
// request that answers "already gone". Worse, a list with a repeat in it reads
// to an operator as two credentials where there is one.
func (r Removed) Secrets() []string {
	var out []string
	for _, account := range r.Accounts {
		out = append(out, account.Secrets...)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Handles names the seats this teardown removed, sorted.
func (r Removed) Handles() []string {
	out := make([]string, 0, len(r.Accounts))
	for _, account := range r.Accounts {
		out = append(out, account.Handle)
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// Add records one removed account, ignoring an empty one so a caller can hand
// over whatever it found without guarding each call.
func (r *Removed) Add(account Removal) {
	if account.Handle == "" {
		return
	}
	account.Secrets = slices.Compact(slices.Sorted(slices.Values(account.Secrets)))
	r.Accounts = append(r.Accounts, account)
}
