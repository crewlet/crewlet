package jetstream

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/nats-io/nats.go/jetstream"
)

// A stream's byte ceiling is a RESERVATION, and this file is what the broker
// will actually let this node reserve.
//
// # Why the number is read rather than modelled
//
// The broker refuses to create a stream it could not honour, with `insufficient
// storage resources available` and nothing naming the number it compared
// against. A ceiling derived from the disk alone can be refused on a machine
// that has the space, because the broker compares against a limit of its own:
// three quarters of its volume for an embedded file store, three quarters of
// the host's memory for one that keeps its streams in memory, and the account's
// JetStream limit wherever an operator set one. Each is a number the broker can
// state, so each is read from the broker.
//
// # And why it is RESERVATIONS that count against it, not bytes stored
//
// The broker adds a new ceiling to the ceilings it has already granted and
// compares the sum against its limit; what those streams actually hold is not
// in the sum. So an account that has granted 12 GiB of ceilings and stores
// 40 MiB has a few gibibytes left rather than nearly all of it, and a figure
// read from what is stored double-grants the difference on every restart.

// StorageBudget is what the broker compares a new stream's byte ceiling
// against, in the units of a ceiling.
//
// IN CEILING UNITS, because that is the only number a caller has to hand: an
// untiered account limit counts every replica of a ceiling against itself, so
// a replicated account's limit is divided by the replica count here rather than
// multiplied into every comparison a caller makes.
type StorageBudget struct {
	// Limit is the most this node's streams may reserve between them, -1
	// when the broker states no limit this client can read.
	Limit int64

	// Committed is what already counts against Limit.
	Committed int64

	// Source is who sets Limit, which is what an operator changes to raise
	// it.
	Source BudgetSource
}

// Available is what a new reservation may take: zero when the limit is already
// spent, and -1 when there is no stated limit to take it from.
func (b StorageBudget) Available() int64 {
	if b.Limit < 0 {
		return -1
	}
	return max(b.Limit-b.Committed, 0)
}

// BudgetSource names who sets a [StorageBudget]'s limit.
type BudgetSource string

const (
	// BudgetServerStore is an embedded broker's file store. Its limit is
	// three quarters of the free space on the volume holding its store
	// directory, counting what its own streams already occupy there.
	BudgetServerStore BudgetSource = "server_store"

	// BudgetServerMemory is an embedded broker that keeps its streams in
	// memory, because it was given no store directory. Its limit is three
	// quarters of the host's memory.
	BudgetServerMemory BudgetSource = "server_memory"

	// BudgetAccount is the account's own JetStream limit, set by whoever
	// operates the broker.
	BudgetAccount BudgetSource = "account"

	// BudgetAccountNoTier is a TIERED account that declares no tier for the
	// replica class this node's streams are created at — `R1` and `R5`
	// declared while `stream.replicas` is 3.
	//
	// ITS OWN SOURCE rather than [BudgetAccount] with a zero, because the
	// zero is the same number an exhausted account reports and the two
	// send an operator to opposite places: one waits for room, the other
	// changes a setting. The server refuses every create on such an
	// account with `no JetStream default or applicable tiered limit
	// present` before it compares a single byte, so a refusal has two
	// levers to name and neither is capacity.
	BudgetAccountNoTier BudgetSource = "account_no_tier"

	// BudgetUnstated is an external broker whose account states no limit.
	// Every server still has a cap of its own, and a client cannot read it.
	BudgetUnstated BudgetSource = "unstated"
)

// BudgetSources is the closed set.
var BudgetSources = []BudgetSource{
	BudgetServerStore, BudgetServerMemory, BudgetAccount,
	BudgetAccountNoTier, BudgetUnstated,
}

// Valid reports whether a source is one this build knows.
func (s BudgetSource) Valid() bool { return slices.Contains(BudgetSources, s) }

// StreamBudget is what the broker will let this node reserve for a new stream
// of the storage class this queue creates, and how much of that is taken.
//
// # Both halves, and the tighter one wins
//
// The account's limit is what every broker states; an embedded broker's
// account states none, and its own cap is what refuses there. Where both are
// stated (an embedded member whose account an operator limited), the one with
// less room is the one a reservation meets first.
func (q *Queue) StreamBudget(ctx context.Context) (StorageBudget, error) {
	return q.budget(ctx, q.embedded != nil)
}

// GrowthBudget is how far a running stream's ceiling may be raised before the
// broker refuses the update, stated only where this node can read the limit
// the broker holds the update to.
//
// # Why it is not [Queue.StreamBudget]
//
// A create and an update are refused by different rules. A create is PLACED,
// so a clustered member's own room is what the cluster weighs, and that is what
// StreamBudget reads. An update is checked by the server that leads the
// metadata group, against that server's own reservations, and a member cannot
// read another server's. So the server half is stated here only on a lone
// embedded server, where the leader is this process, and elsewhere only an
// account's limit, which every server shares, is held against a raise. A
// number this node cannot read exactly would refuse raises the broker grants.
func (q *Queue) GrowthBudget(ctx context.Context) (StorageBudget, error) {
	return q.budget(ctx, q.embedded != nil && !q.embedded.clustered)
}

// budget is the account's stated limit and, when withServer says the embedded
// server's own cap is this node's to hold a request to, that cap as well,
// whichever a reservation meets first.
//
// # The server half is read FIRST, and it is what survives a broker that will
// not answer
//
// That server is in this process, and its configuration is a struct field
// rather than a request — it cannot fail to answer because the broker is busy.
// The account half is a round trip, and on an embedded broker it states no
// limit at all in the ordinary case, because nothing here sets one. Failing
// the whole read on it therefore threw away the one number this node
// definitely had, for the one that is usually absent: [internal/engine] reads
// a failure here as "no limit" and sizes its state logs from FREE DISK
// instead, which is both looser than the cap that just went unread and the
// arithmetic the cap exists to replace.
//
// So the account half degrades to what it states when nobody can read it —
// nothing — and the SOURCE says which limit is bounding the answer, so a
// refusal names the one an operator can raise. Where no server half is stated
// (an external broker) the account's limit is the only thing readable, and a
// failure there is a failure: there is nothing left to hold a ceiling to.
func (q *Queue) budget(ctx context.Context, withServer bool) (StorageBudget, error) {
	memory := q.storage() == jetstream.MemoryStorage
	var server StorageBudget
	if withServer {
		var err error
		if server, err = q.embedded.budget(memory); err != nil {
			return StorageBudget{}, err
		}
	}
	info, err := q.js.AccountInfo(ctx)
	switch {
	case err != nil && !withServer:
		return StorageBudget{}, fmt.Errorf("jetstream: read the account's storage limits: %w", err)
	case err != nil:
		q.log.WarnContext(ctx, "jetstream_account_limits_unread", "error", err.Error(),
			"detail", "the embedded server's own cap bounds this node's "+
				"reservations instead; an account limit set on top of it is "+
				"not held against them")
		return server, nil
	case !withServer:
		return accountBudget(info, max(q.cfg.Replicas, 1), memory), nil
	}
	return tighter(accountBudget(info, max(q.cfg.Replicas, 1), memory), server), nil
}

// tighter is whichever of two budgets a reservation meets first: the one with
// less room, and a stated one over one that states nothing.
func tighter(a, b StorageBudget) StorageBudget {
	switch {
	case a.Limit < 0:
		return b
	case b.Limit < 0:
		return a
	case b.Available() < a.Available():
		return b
	}
	return a
}

// accountBudget is the account's stated limit for a stream of this storage
// class and replica count, in ceiling units.
//
// # The two account shapes
//
// An UNTIERED account has one limit, and the broker counts every replica of a
// ceiling against it: a 1 GiB ceiling on an R3 stream takes 3 GiB. A TIERED
// account has a limit per replica count (`R1`, `R3`), which already accounts
// for replication, and its top-level limits are zero: read there, a tiered
// account with room for terabytes reports none at all.
//
// A tiered account with no tier for this replica count grants nothing, and the
// broker refuses such a stream outright; zero says so rather than "unstated",
// which would read as a limit nobody set. It carries [BudgetAccountNoTier]
// rather than [BudgetAccount], because the same zero from an account that is
// merely full is a different instruction to whoever reads the refusal.
//
// THE DISCRIMINATOR IS WHETHER TIERS ARE PRESENT AT ALL, never whether the one
// named for this node is. The two are different questions, and the server
// answers the first: `Account.JetStreamUsage` fills `Tiers` OR the un-tiered
// `Limits` on the two arms of one either-or, and `JetStreamAccountStats`
// says so in the comment on the tier it embeds — "in case tiers are used,
// reflects totals with limits not set". Asked the second question, a tiered
// account missing this node's class fell through to the un-tiered branch and
// read a MaxStore of 0 as a limit somebody had set.
//
// The reservation the broker reports is the sum of ceilings, NOT multiplied by
// replicas, which is why only the limit is divided.
func accountBudget(info *jetstream.AccountInfo, replicas int, memory bool) StorageBudget {
	tier, perCeiling := info.Tier, int64(replicas)
	if len(info.Tiers) > 0 {
		held, found := info.Tiers[fmt.Sprintf("R%d", replicas)]
		if !found {
			return StorageBudget{Limit: 0, Source: BudgetAccountNoTier}
		}
		tier, perCeiling = held, 1
	}
	limit, reserved := tier.Limits.MaxStore, tier.ReservedStore
	if memory {
		limit, reserved = tier.Limits.MaxMemory, tier.ReservedMemory
	}
	if limit < 0 {
		return StorageBudget{Limit: -1, Source: BudgetUnstated}
	}
	return StorageBudget{
		Limit:     limit / perCeiling,
		Committed: int64(reserved),
		Source:    BudgetAccount,
	}
}

// budget is this embedded member's own cap and what counts against it.
//
// # Why the member's committed figure depends on whether it is clustered
//
// A solo server compares a new ceiling against the ceilings it has granted and
// nothing else. A clustered one PLACES the stream, and placement counts the
// larger of a peer's reservations and what it stores, so a member holding an
// unbounded stream of events has less room than its reservations say. Each is
// the rule the server itself applies, so neither over-promises nor refuses
// what the broker would grant.
func (e *embeddedServer) budget(memory bool) (StorageBudget, error) {
	info, err := e.ns.Jsz(nil)
	if err != nil {
		return StorageBudget{}, fmt.Errorf("jetstream: read the embedded server's storage limits: %w", err)
	}
	if info.Disabled {
		return StorageBudget{}, errors.New("jetstream: the embedded server reports JetStream disabled, so it will reserve nothing")
	}
	limit, reserved, stored := info.Config.MaxStore, info.ReservedStore, info.Store
	source := BudgetServerStore
	if memory {
		limit, reserved, stored = info.Config.MaxMemory, info.ReservedMemory, info.Memory
		source = BudgetServerMemory
	}
	committed := reserved
	if e.clustered {
		committed = max(reserved, stored)
	}
	return StorageBudget{
		Limit:     limit,
		Committed: int64(committed),
		Source:    source,
	}, nil
}

// ErrInsufficientStorage reports a reservation the broker would not make —
// a stream it would not create, or a ceiling it would not raise.
//
// A SENTINEL rather than the broker's own error, because the broker says it in
// two codes, one per storage class, and neither names the number it compared
// against: a caller that wants to say what was needed, what was available and
// what to change has to recognise the refusal first, and must not have to know
// this backend's vocabulary to do it.
//
// BOTH WRITES, because both callers act on it the same way and for the same
// reason: a refusal is an ANSWER. The broker compared the ceiling against its
// limit and declined, so nothing was written and nothing is in flight — which
// is the one create failure that is not read back, and the one capacity-apply
// failure that is not an unknown the fleet has to seal to retire.
var ErrInsufficientStorage = errors.New("jetstream: the broker cannot reserve the stream's byte ceiling")

// The broker's codes for a reservation it refused for want of room.
//
// Spelled here for the reason [jsprovision.Unplaceable]'s code is: nats.go
// names neither. Placement is deliberately NOT one of them. A clustered create
// that no member can place reports `no suitable peers for placement`, whose
// code is shared by every placement failure and whose storage clause is prose,
// and what it would be compared against is somebody else's disk, which this
// node cannot read. Nor is `insufficient resources` (10023): the server
// answers a publish, a catch-up or a consumer's placement with it, never a
// stream's create, so naming it here would read some other failure as a
// ceiling nobody reserved.
const (
	jsErrCodeStorageExceeded jetstream.ErrorCode = 10047
	jsErrCodeMemoryExceeded  jetstream.ErrorCode = 10028
)

// refusedStorage reports whether err is the broker refusing a reservation.
func refusedStorage(err error) bool {
	var apiErr *jetstream.APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	switch apiErr.ErrorCode {
	case jsErrCodeStorageExceeded, jsErrCodeMemoryExceeded:
		return true
	}
	return false
}

// DomainStreamCeiling is the byte ceiling a domain's stream holds, and whether
// the stream exists at all.
//
// THREE ANSWERS, because the caller does something different with each: a
// ceiling it already holds is part of what the state logs have reserved, an
// absent stream is one this boot will create, and a broker that could not say
// is neither.
func (q *Queue) DomainStreamCeiling(ctx context.Context, stream string) (int64, bool, error) {
	s, err := q.js.Stream(ctx, stream)
	switch {
	case errors.Is(err, jetstream.ErrStreamNotFound):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("jetstream: read %q's ceiling: %w", stream, err)
	}
	// A STREAM WITH NO CEILING RESERVES NOTHING, which the broker spells
	// -1.
	return max(s.CachedInfo().Config.MaxBytes, 0), true, nil
}
