package jetstream

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/jsprovision"
)

// A stream's byte ceiling is a RESERVATION; this file reads what the broker
// will let this node reserve.
//
// READ, NOT MODELLED: the broker refuses a create (`insufficient storage
// resources available`, naming no number) against a limit of its own — three
// quarters of its volume for a file store, of the host's memory for a memory
// store, or the account's JetStream limit where one is set — so a ceiling
// derived from the disk alone can be refused on a machine with the space.
//
// RESERVATIONS COUNT, NOT BYTES STORED: the broker sums the ceilings it has
// granted, so a figure read from what is stored double-grants the difference
// on every restart.

// StorageBudget is what the broker compares a new stream's byte ceiling
// against, in the units of a ceiling.
//
// IN CEILING UNITS, the only number a caller has to hand: an untiered account
// counts every replica of a ceiling, so its limit is divided by the replica
// count here rather than multiplied into every comparison a caller makes.
type StorageBudget struct {
	// Limit is the most this node's streams may reserve between them, -1
	// when the broker states no limit this client can read.
	Limit int64

	// Committed is what already counts against Limit: the ceilings the
	// broker has granted. Carried where Limit is unstated too: a node that
	// sizes from its own disk in place of the limit takes them off that.
	Committed int64

	// Source is who sets Limit: what an operator changes to raise it.
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
	// declared while `stream.replicas` is 3. The server refuses every create
	// on it (`no JetStream default or applicable tiered limit present`)
	// before comparing a byte.
	//
	// Not [BudgetAccount] with a zero: an exhausted account reports the same
	// zero, and the two send an operator to opposite places — wait for room,
	// or change a setting.
	BudgetAccountNoTier BudgetSource = "account_no_tier"

	// BudgetAccountTierNoLimit is a TIERED account that HAS this node's
	// replica class and declares no limit on it.
	//
	// `Account.JetStreamUsage` (server/jetstream.go) lists a tier for USAGE
	// as well as for limits, so a class the account holds objects in arrives
	// present with MaxStore at zero. The server treats it as a missing tier
	// (`jsa.selectLimits` answers not-ok for both), but for an operator
	// "declare a tier" and "declare a limit on the tier you have" differ.
	BudgetAccountTierNoLimit BudgetSource = "account_tier_no_limit"

	// BudgetUnstated is an external broker whose account states no limit.
	// Every server still has a cap of its own, and a client cannot read it.
	BudgetUnstated BudgetSource = "unstated"
)

// BudgetSources is the closed set.
var BudgetSources = []BudgetSource{
	BudgetServerStore, BudgetServerMemory, BudgetAccount,
	BudgetAccountNoTier, BudgetAccountTierNoLimit, BudgetUnstated,
}

// Valid reports whether a source is one this build knows.
func (s BudgetSource) Valid() bool { return slices.Contains(BudgetSources, s) }

// VolumeClaim is how much of its store directory's volume this node's EMBEDDED
// broker may hold: the limit its server enforces on what its streams STORE, and
// what they store there now.
//
// BYTES STORED, NEVER RESERVATIONS: [StorageBudget] answers what a new stream
// may reserve; a neighbour on the same disk asks what the broker can still
// write, and the server refuses a message that takes its stored bytes past
// this limit (nats-server's limitsExceeded) whatever the ceilings add up to,
// while a stream with no ceiling (the files bucket, a seat's mailbox) grows
// into it like any other.
type VolumeClaim struct {
	// Dir is the store directory the broker's files live under — the
	// volume a neighbour compares its own with.
	Dir string

	// Limit is the server's limit in force: stream.store_max_bytes where
	// declared, otherwise nats-server's derivation, three quarters of the
	// volume's free space when its JetStream came up plus what it already
	// stored there. -1 is a server holding to none: the whole volume.
	Limit int64

	// Stored is what the broker's streams occupy on the volume now.
	Stored int64
}

// Unclaimed is what the broker may still write to its volume: its limit less
// what it stores, never below zero, and -1 for a server holding to no limit —
// the whole volume, whatever that is.
func (c VolumeClaim) Unclaimed() int64 {
	if c.Limit < 0 {
		return -1
	}
	return max(c.Limit-c.Stored, 0)
}

// VolumeClaim is this node's embedded broker's claim on the volume its streams
// are stored on, and false where there is no such volume: a broker holding its
// streams in memory, a leaf (whose streams are its members'), and an external
// cluster (whose disks are its operator's).
//
// READ FROM THE SERVER IN THIS PROCESS (a struct field, never a round trip):
// the number is the server's own, declared or derived, so a reader cannot
// model it differently from the server that enforces it.
func (q *Queue) VolumeClaim() (VolumeClaim, bool, error) {
	if !q.embedded.holdsStreams() || q.storage() != jetstream.FileStorage {
		return VolumeClaim{}, false, nil
	}
	info, err := q.embedded.ns.Jsz(nil)
	if err != nil {
		return VolumeClaim{}, false, fmt.Errorf("jetstream: read the embedded server's store limit: %w", err)
	}
	if info.Disabled {
		return VolumeClaim{}, false, errors.New("jetstream: the embedded server reports JetStream disabled, so it states no store limit")
	}
	return VolumeClaim{Dir: q.cfg.StoreDir, Limit: info.Config.MaxStore, Stored: int64(info.Store)}, true, nil
}

// StreamBudget is what the broker will let this node reserve for a new stream
// of the storage class this queue creates, and how much of that is taken: the
// tighter of the account's limit and, on an embedded broker (whose account
// usually states none), the server's own cap.
func (q *Queue) StreamBudget(ctx context.Context) (StorageBudget, error) {
	return q.budget(ctx, q.embedded.holdsStreams())
}

// GrowthBudget is how far a running stream's ceiling may be raised before the
// broker refuses the update, stated only where this node can read the limit
// the broker holds the update to.
//
// Not [Queue.StreamBudget]: a create is PLACED, so a member's own room is what
// the cluster weighs, but an update is checked by the metadata leader against
// its own reservations, which a member cannot read. So the server half counts
// only on a lone embedded server (the leader is this process); elsewhere only
// the account's limit, shared by every server, is held against a raise — a
// number this node cannot read exactly would refuse raises the broker grants.
func (q *Queue) GrowthBudget(ctx context.Context) (StorageBudget, error) {
	return q.budget(ctx, q.embedded.holdsStreams() && !q.embedded.clustered)
}

// budget is the account's stated limit and, when withServer holds the request
// to the embedded server's own cap, that cap too, whichever is met first.
//
// THE SERVER HALF IS READ FIRST AND SURVIVES AN UNANSWERED ACCOUNT READ: it is
// a struct field in this process, while the account half is a round trip that
// on an embedded broker usually states no limit at all. Failing on it would
// discard the one number this node has, and [internal/engine] reads a failure
// as "no limit" and sizes its state logs from free disk, the arithmetic the
// cap replaces. So the account half degrades to nothing and the SOURCE names
// the limit bounding the answer. With no server half (an external broker) an
// account failure is a failure: nothing is left to hold a ceiling to.
func (q *Queue) budget(ctx context.Context, withServer bool) (StorageBudget, error) {
	memory := q.storage() == jetstream.MemoryStorage
	var server StorageBudget
	if withServer {
		var err error
		if server, err = q.embedded.budget(memory); err != nil {
			return StorageBudget{}, err
		}
	}
	// A METADATA READ ([jsprovision.ReadBoundedWith]): only the metadata
	// leader answers account info, and a member still believing in a lost
	// leader drops it silently, so it is asked again. A boot with no
	// deadline keeps the client's five-second default (the bound a data
	// pod's startup window counts this read at), not [askRead]'s lookup
	// ceiling.
	info, err := jsprovision.ReadBoundedWith(ctx, q.provisioning(), q.js.Options().DefaultTimeout,
		func(ctx context.Context) (*jetstream.AccountInfo, error) {
			return q.js.AccountInfo(ctx)
		}, nil)
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
// An UNTIERED account has one limit and counts every replica of a ceiling
// against it (1 GiB on R3 takes 3 GiB), so the limit is divided by replicas;
// the reported reservation is the sum of ceilings, unmultiplied, so it is not.
// A TIERED account has a limit per replica count (`R1`, `R3`) and zero
// top-level limits; with no tier for this count it grants nothing
// ([BudgetAccountNoTier], not a full [BudgetAccount]). Tiered is decided by
// whether ANY tier is present, never this node's: `Account.JetStreamUsage`
// fills `Tiers` or the untiered `Limits`, never both, so a tiered account
// missing this class must not read its top-level MaxStore of 0 as a limit.
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
		// RESERVATIONS CARRIED WITH NO LIMIT: a node standing its free
		// space in for the limit ([internal/engine]'s state-log sizing)
		// spends them from it, as the broker's own cap does.
		return StorageBudget{Limit: -1, Committed: int64(reserved), Source: BudgetUnstated}
	}
	// A TIER PRESENT WITH A ZERO LIMIT is the missing tier's other shape.
	// Zero, never `<= 0`: a tier declared unlimited reports a negative
	// limit and the broker creates against it, hence after the negative arm.
	if limit == 0 && perCeiling == 1 {
		// The usage is carried although nothing can be reserved: it is
		// why the tier is listed at all, the evidence for the diagnosis.
		return StorageBudget{
			Limit: 0, Committed: int64(reserved), Source: BudgetAccountTierNoLimit,
		}
	}
	return StorageBudget{
		Limit:     limit / perCeiling,
		Committed: int64(reserved),
		Source:    BudgetAccount,
	}
}

// budget is this embedded member's own cap and what counts against it.
//
// Committed follows the server's own rule: a solo server compares against the
// ceilings it has granted; a clustered one PLACES the stream, counting the
// larger of a peer's reservations and what it stores, so a member holding an
// unbounded stream has less room than its reservations say.
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
// A SENTINEL because the broker says it in two codes, one per storage class,
// neither naming the number compared, and a caller must recognise the refusal
// without knowing this backend's vocabulary. For both writes a refusal is an
// ANSWER: nothing was written and nothing is in flight, so a create is not
// read back and a capacity apply is not an unknown the fleet must seal.
var ErrInsufficientStorage = errors.New("jetstream: the broker cannot reserve the stream's byte ceiling")

// The broker's codes for a reservation refused for want of room are
// [jsprovision.OutOfCapacity]'s, for create and update alike, so the
// coordination store's buckets share one spelling; placement and `insufficient
// resources` (10023) are deliberately not among them (see that predicate).

// DomainStreamCeiling is the byte ceiling a domain's stream holds, and whether
// the stream exists: held, it is part of what the state logs have reserved;
// absent, this boot creates it; unknown (an error) is neither.
//
// A BOOT-PATH READ, sized and re-asked by [Queue.askRead]: it runs while peers
// booting together create these same streams, when a member drops reads of a
// stream in flight ([jsprovision.ReadTerm]), and the sizing would carry an
// unknown as absent and fix a ceiling without what the stream holds. An
// unanswered read costs the whole lookup ceiling, so [internal/engine] asks
// every log's at once.
func (q *Queue) DomainStreamCeiling(ctx context.Context, stream string) (int64, bool, error) {
	s, err := askRead(ctx, q, func(ctx context.Context) (jetstream.Stream, error) {
		return q.js.Stream(ctx, stream)
	})
	switch {
	case errors.Is(err, jetstream.ErrStreamNotFound):
		return 0, false, nil
	case err != nil:
		return 0, false, fmt.Errorf("jetstream: read %q's ceiling: %w", stream, err)
	}
	// A STREAM WITH NO CEILING (-1) RESERVES NOTHING.
	return max(s.CachedInfo().Config.MaxBytes, 0), true, nil
}
