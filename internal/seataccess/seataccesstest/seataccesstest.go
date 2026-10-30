// Package seataccesstest is the test half of internal/seataccess: an
// in-memory [seataccess.Ledger], an in-memory [provision.TokenSink] that can
// be told to fail, and a scriptable fake [seataccess.Vendor] whose accounts
// live in memory, so the shared plan and sync, and every vendor's own tests,
// can drive a sync end to end with no network.
package seataccesstest

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/crewlet/crewlet/internal/integration"
	"github.com/crewlet/crewlet/internal/seataccess"
)

// ---- the ledger --------------------------------------------------------- //

// Ledger is an in-memory [seataccess.Ledger].
type Ledger struct {
	mu      sync.Mutex
	records map[integration.Kind]map[string]seataccess.Record

	// Fail, when set, is returned by every call.
	Fail error
}

var _ seataccess.Ledger = (*Ledger)(nil)

// NewLedger is an empty ledger.
func NewLedger() *Ledger {
	return &Ledger{records: map[integration.Kind]map[string]seataccess.Record{}}
}

// Records implements [seataccess.Ledger].
func (l *Ledger) Records(_ context.Context, kind integration.Kind) (map[string]seataccess.Record, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.Fail != nil {
		return nil, l.Fail
	}
	return maps.Clone(l.records[kind]), nil
}

// Put implements [seataccess.Ledger].
func (l *Ledger) Put(_ context.Context, rec seataccess.Record) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.Fail != nil {
		return l.Fail
	}
	if l.records[rec.Kind] == nil {
		l.records[rec.Kind] = map[string]seataccess.Record{}
	}
	l.records[rec.Kind][rec.Handle] = rec
	return nil
}

// Drop implements [seataccess.Ledger].
func (l *Ledger) Drop(_ context.Context, kind integration.Kind, handle string) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.Fail != nil {
		return l.Fail
	}
	delete(l.records[kind], handle)
	return nil
}

// Record is one seat's record, or false.
func (l *Ledger) Record(kind integration.Kind, handle string) (seataccess.Record, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	rec, ok := l.records[kind][handle]
	return rec, ok
}

// ---- the sink ----------------------------------------------------------- //

// Sink is an in-memory token sink.
type Sink struct {
	mu     sync.Mutex
	values map[string]string

	// FailRecord, when set, refuses every Record of a name it holds.
	FailRecord map[string]error

	// Forgotten is every name Forget was asked to clear, in order.
	Forgotten []string

	// FailForget, when set, refuses every Forget and clears nothing.
	FailForget error
}

// NewSink is an empty sink.
func NewSink() *Sink { return &Sink{values: map[string]string{}} }

// Record implements provision.TokenSink.
func (s *Sink) Record(_ context.Context, name, value string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.FailRecord[name]; err != nil {
		return err
	}
	s.values[name] = value
	return nil
}

// Discard implements provision.TokenSink.
func (s *Sink) Discard(context.Context) error { return nil }

// Forget implements provision.TokenSink.
func (s *Sink) Forget(_ context.Context, names ...string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.FailForget != nil {
		return s.FailForget
	}
	for _, name := range names {
		delete(s.values, name)
		s.Forgotten = append(s.Forgotten, name)
	}
	return nil
}

// Flush implements provision.TokenSink.
func (s *Sink) Flush(context.Context) error { return nil }

// Value implements provision.TokenSink.
func (s *Sink) Value(_ context.Context, name string) (string, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.values[name]
	return v, ok, nil
}

// Describe implements provision.TokenSink.
func (s *Sink) Describe() string { return "the test sink" }

// NextStep implements provision.TokenSink.
func (s *Sink) NextStep() string { return "nothing" }

// Held is the value sealed under name, or false.
func (s *Sink) Held(name string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.values[name]
	return v, ok
}

// ---- the vendor --------------------------------------------------------- //

// Account is one account the fake vendor holds for a seat.
type Account struct {
	ID      string
	Enabled map[seataccess.Lane]bool
	Level   map[seataccess.Lane]string
	Scope   map[seataccess.Lane][]string
	Perms   map[seataccess.Lane]seataccess.Permissions
	Keys    []string

	// Sealed is a credential sealed for the account outside the sync (a
	// manifest route, an install landing), which Observe reports.
	Sealed *seataccess.Credential
}

// Vendor is a scriptable fake vendor for one kind. Its accounts live in
// memory; every Apply is recorded; a refusal can be scripted per op.
type Vendor struct {
	KindValue integration.Kind
	Creds     []string
	Levels    map[seataccess.Lane][]seataccess.Level
	Scope     map[seataccess.Lane][]string

	mu       sync.Mutex
	accounts map[string]*Account
	next     int

	// Refuse makes Apply refuse every step of an op with this refusal.
	Refuse map[seataccess.Op]*seataccess.Refusal

	// Wait makes Apply answer Waiting for every step of an op.
	Wait map[seataccess.Op]*seataccess.Waiting

	// Applied is every step taken, as "handle/lane/op/verb".
	Applied []string

	// Revoked is every minted value revoked through Minted.Revoke.
	Revoked []string

	// NotConfigured makes Open answer integration.ErrNotConfigured.
	NotConfigured bool

	// OpenErr makes Open fail with it: a [*seataccess.Refusal] for a company
	// the vendor refuses as configured, any other error for a fault.
	OpenErr error

	// Opened counts the sessions opened.
	Opened int

	// StepsPerOp draws this many steps for an op instead of one.
	StepsPerOp map[seataccess.Op]int

	// Did is the sentence Apply reports doing for an op's steps.
	Did map[seataccess.Op]string

	// Priors is, for every step taken, how many of its op's earlier steps'
	// results it was handed.
	Priors []int

	// MintOnly, when set, limits what a mint makes to these fields.
	MintOnly []string
}

var _ seataccess.Vendor = (*Vendor)(nil)

// NewVendor is a fake for kind with every lane offering levels a, b and an
// administering c, and scope x, y and z.
func NewVendor(kind integration.Kind, creds ...string) *Vendor {
	if len(creds) == 0 {
		creds = []string{"TOKEN"}
	}
	v := &Vendor{
		KindValue: kind, Creds: creds,
		Levels:   map[seataccess.Lane][]seataccess.Level{},
		Scope:    map[seataccess.Lane][]string{},
		accounts: map[string]*Account{},
		Refuse:   map[seataccess.Op]*seataccess.Refusal{},
		Wait:     map[seataccess.Op]*seataccess.Waiting{},
	}
	for _, lane := range seataccess.LanesOf(kind) {
		v.Levels[lane] = []seataccess.Level{
			{ID: "a", Name: "A"}, {ID: "b", Name: "B"}, {ID: "c", Name: "C", Administers: true},
		}
		v.Scope[lane] = []string{"x", "y", "z"}
	}
	return v
}

// Kind implements [seataccess.Vendor].
func (v *Vendor) Kind() integration.Kind { return v.KindValue }

// Catalog implements [seataccess.Vendor].
func (v *Vendor) Catalog() seataccess.Catalog {
	cat := seataccess.Catalog{Kind: v.KindValue, Credentials: v.Creds}
	for _, lane := range seataccess.LanesOf(v.KindValue) {
		cat.Lanes = append(cat.Lanes, seataccess.LaneSpec{
			Lane: lane, Levels: len(v.Levels[lane]) > 0, Scoped: true,
			CreatedBy: seataccess.ByCrewlet, EnabledBy: seataccess.ByCrewlet, Rotatable: true,
		})
	}
	return cat
}

// Steps implements [seataccess.Vendor]: one step per op, named after it.
func (v *Vendor) Steps(seat seataccess.SeatRef, lane seataccess.Lane, op seataccess.Op,
	_ seataccess.Desired, _ seataccess.Observed,
) []seataccess.Step {
	verb := map[seataccess.Op]seataccess.StepVerb{
		seataccess.OpCreate: seataccess.VerbCreate, seataccess.OpEnable: seataccess.VerbEnable,
		seataccess.OpLevel: seataccess.VerbRerole, seataccess.OpScope: seataccess.VerbScope,
		seataccess.OpMint: seataccess.VerbMint, seataccess.OpDisable: seataccess.VerbDisable,
		seataccess.OpRevoke: seataccess.VerbRevoke, seataccess.OpRemove: seataccess.VerbRemove,
		seataccess.OpRotate: seataccess.VerbRotate,
	}[op]
	n := max(v.StepsPerOp[op], 1)
	out := make([]seataccess.Step, 0, n)
	for i := range n {
		out = append(out, seataccess.Step{
			Verb: verb, By: seataccess.ByCrewlet,
			Text: fmt.Sprintf("%s %s on %s (%d)", op, seat.Handle, lane, i+1),
		})
	}
	return out
}

// Open implements [seataccess.Vendor].
func (v *Vendor) Open(context.Context, seataccess.OpenInput) (seataccess.Session, error) {
	v.mu.Lock()
	v.Opened++
	v.mu.Unlock()
	if v.NotConfigured {
		return nil, integration.ErrNotConfigured
	}
	if v.OpenErr != nil {
		return nil, v.OpenErr
	}
	return v, nil
}

// Options implements [seataccess.Session].
func (v *Vendor) Options(_ context.Context, lane seataccess.Lane) (seataccess.Options, error) {
	return seataccess.Options{Lane: lane, Levels: v.Levels[lane], Live: true, Scope: v.Scope[lane]}, nil
}

// Observe implements [seataccess.Session].
func (v *Vendor) Observe(_ context.Context, in seataccess.ObserveInput) (seataccess.Observation, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	out := seataccess.Observation{}
	for _, seat := range in.Seats {
		acct := v.accounts[seat.Handle]
		lanes := map[seataccess.Lane]seataccess.Observed{}
		for _, lane := range seataccess.LanesOf(v.KindValue) {
			if acct == nil {
				lanes[lane] = seataccess.Observed{}
				continue
			}
			lanes[lane] = seataccess.Observed{
				Exists: true, Enabled: acct.Enabled[lane], AccountID: acct.ID,
				Account: seat.Handle + "@vendor.invalid",
				Level:   acct.Level[lane], Scope: slices.Clone(acct.Scope[lane]),
				Permissions: acct.Perms[lane], Credential: acct.Sealed,
			}
		}
		out[seat.Handle] = lanes
	}
	return out, nil
}

// Apply implements [seataccess.Session].
func (v *Vendor) Apply(_ context.Context, in seataccess.ApplyInput) (seataccess.StepResult, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.Applied = append(v.Applied, strings.Join([]string{
		in.Seat.Handle, string(in.Lane), string(in.Step.Op), string(in.Step.Verb),
	}, "/"))
	v.Priors = append(v.Priors, len(in.Prior))
	if r := v.Refuse[in.Step.Op]; r != nil {
		return seataccess.StepResult{}, r
	}
	if w := v.Wait[in.Step.Op]; w != nil {
		return seataccess.StepResult{Waiting: w}, nil
	}
	acct := v.accounts[in.Seat.Handle]
	res := seataccess.StepResult{Did: v.Did[in.Step.Op]}
	if in.Step.Index > 0 {
		// AN OP'S LATER STEPS have done their part in its first.
		return res, nil
	}
	switch in.Step.Op {
	case seataccess.OpCreate:
		if acct == nil {
			v.next++
			acct = &Account{
				ID:      fmt.Sprintf("acct-%d", v.next),
				Enabled: map[seataccess.Lane]bool{}, Level: map[seataccess.Lane]string{},
				Scope: map[seataccess.Lane][]string{}, Perms: map[seataccess.Lane]seataccess.Permissions{},
			}
			v.accounts[in.Seat.Handle] = acct
			res.AccountID, res.Account = acct.ID, in.Seat.Handle+"@vendor.invalid"
			res.Minted = v.mint(in.Seat.Handle, acct)
		}
		v.grant(acct, in)
	case seataccess.OpEnable:
		v.grant(acct, in)
		if len(acct.Keys) == 0 {
			res.Minted = v.mint(in.Seat.Handle, acct)
		}
	case seataccess.OpLevel:
		acct.Level[in.Lane], acct.Perms[in.Lane] = in.Want.Level, in.Want.Permissions
	case seataccess.OpScope:
		acct.Scope[in.Lane] = slices.Clone(in.Want.Scope)
	case seataccess.OpMint, seataccess.OpRotate:
		old := slices.Clone(acct.Keys)
		res.Minted = v.mint(in.Seat.Handle, acct)
		acct.Keys = slices.DeleteFunc(acct.Keys, func(k string) bool { return slices.Contains(old, k) })
	case seataccess.OpDisable:
		acct.Enabled[in.Lane] = false
		res.Disabled = !slices.ContainsFunc(seataccess.LanesOf(v.KindValue), func(l seataccess.Lane) bool {
			return acct.Enabled[l]
		})
	case seataccess.OpRevoke:
		acct.Keys = nil
		res.Revoked = slices.Clone(v.Creds)
	case seataccess.OpRemove:
		delete(v.accounts, in.Seat.Handle)
		res.Gone = true
	}
	return res, nil
}

func (v *Vendor) grant(acct *Account, in seataccess.ApplyInput) {
	acct.Enabled[in.Lane] = true
	acct.Level[in.Lane] = in.Want.Level
	acct.Scope[in.Lane] = slices.Clone(in.Want.Scope)
	acct.Perms[in.Lane] = in.Want.Permissions
}

// mint makes one value per credential field; the caller holds v.mu.
func (v *Vendor) mint(handle string, acct *Account) []seataccess.Minted {
	var out []seataccess.Minted
	for _, field := range v.Creds {
		if v.MintOnly != nil && !slices.Contains(v.MintOnly, field) {
			continue
		}
		v.next++
		value := fmt.Sprintf("fake-%s-%s-%d", strings.ToLower(field), handle, v.next)
		acct.Keys = append(acct.Keys, value)
		out = append(out, seataccess.Minted{Field: field, Value: value, Revoke: func(context.Context) error {
			v.mu.Lock()
			defer v.mu.Unlock()
			v.Revoked = append(v.Revoked, value)
			acct.Keys = slices.DeleteFunc(acct.Keys, func(k string) bool { return k == value })
			return nil
		}})
	}
	return out
}

// Seed gives handle an account the vendor already holds, enabled on lanes at
// level with scope.
func (v *Vendor) Seed(handle, level string, scope []string, lanes ...seataccess.Lane) *Account {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.next++
	acct := &Account{
		ID:      fmt.Sprintf("acct-%d", v.next),
		Enabled: map[seataccess.Lane]bool{}, Level: map[seataccess.Lane]string{},
		Scope: map[seataccess.Lane][]string{}, Perms: map[seataccess.Lane]seataccess.Permissions{},
		Keys: []string{"seeded"},
	}
	for _, lane := range lanes {
		acct.Enabled[lane], acct.Level[lane], acct.Scope[lane] = true, level, slices.Clone(scope)
	}
	v.accounts[handle] = acct
	return acct
}

// Account is handle's account, or nil.
func (v *Vendor) Account(handle string) *Account {
	v.mu.Lock()
	defer v.mu.Unlock()
	return v.accounts[handle]
}

// Mutate changes handle's account as somebody at the vendor would.
func (v *Vendor) Mutate(handle string, change func(*Account)) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	acct := v.accounts[handle]
	if acct == nil {
		return errors.New("seataccesstest: no account for " + handle)
	}
	change(acct)
	return nil
}

// Delete removes handle's account as somebody at the vendor would.
func (v *Vendor) Delete(handle string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	delete(v.accounts, handle)
}
