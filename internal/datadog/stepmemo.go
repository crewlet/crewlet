package datadog

import (
	"sync"

	"github.com/crewlet/crewlet/internal/seataccess"
)

// stepMemo is what one session remembers between the steps of one op.
//
// AN OP CAN TAKE SEVERAL CALLS, and a later one needs what an earlier one
// made: the mint after a create needs the account it created, and a
// rotation's retirement needs the key it minted so it deletes every other
// one and never that one. The session is one pass, so the memory lives
// exactly as long as the op it serves; a step that finds nothing remembered
// falls back to the ledger, or refuses rather than guesses.
type stepMemo struct {
	mu       sync.Mutex
	accounts map[string]string
	mints    map[string]string
}

func newStepMemo() *stepMemo {
	return &stepMemo{accounts: map[string]string{}, mints: map[string]string{}}
}

// account records the account a create made for a seat.
func (m *stepMemo) account(handle, id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.accounts[handle] = id
}

// mint records the credential a step minted for a seat.
func (m *stepMemo) mint(handle, id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.mints[handle] = id
}

// minted is the credential this session minted for a seat, or "".
func (m *stepMemo) minted(handle string) string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.mints[handle]
}

// accountOf is the account a step acts on: the one this session created, the
// ledger's, or the one the pass observed, in that order.
func (m *stepMemo) accountOf(in seataccess.ApplyInput) string {
	m.mu.Lock()
	created := m.accounts[in.Seat.Handle]
	m.mu.Unlock()
	switch {
	case created != "":
		return created
	case in.Record != nil && in.Record.Held():
		return in.Record.Account.ID
	default:
		return in.Have.AccountID
	}
}
