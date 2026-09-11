package provision

import "slices"

// WHAT A TEARDOWN REMOVED, so the credentials it stranded can go with it.
//
// # A teardown knows, and used to have nowhere to say it
//
// Each vendor teardown walks its own [Plan] and deletes the accounts it
// created, so it knows exactly which seats it removed and — because every
// [Seat] carries the `${VAR}` names its credentials live in — exactly which
// values in the company's secret store are now dead. That knowledge was
// destroyed at an `error`-only return boundary before it reached the engine,
// which is the one layer holding a write path onto the sealed store.
//
// The consequence was measured: disconnecting Atlassian with "remove accounts"
// deleted every agent's service account, and left SRE_ATLASSIAN_TOKEN and
// SRE_ATLASSIAN_EMAIL sealed and resolving. On reconnect the tracker mapped the
// seat to the ACCOUNT THAT NO LONGER EXISTED — the identity cache is keyed on
// the credential, and the credential had not changed — and reported a 401 as
// the operator's problem, at a third-party app, for about thirty-five seconds.
//
// # The result is an END STATE, not a delta
//
// A teardown is retried on failure and every step is already "remove this if it
// is there". If a [Removal] named only what THIS call did, a retry after a
// failed secret deletion would find the accounts already gone, report nothing
// removed, and let the block drop with the credentials still in the store —
// the original defect, reached through the recovery path. So "already absent"
// and "already deleted by an earlier attempt" both count as removed.
//
// # And only what is genuinely dead
//
// A seat is named here only where the vendor can say its account is gone AND
// its credential with it. A merely DISABLED account does not qualify: a token
// on one is a credential that works again the moment anybody re-enables it, so
// reporting it would delete the company's only record of a live secret.

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
