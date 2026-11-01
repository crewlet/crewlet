package engine

import (
	"fmt"

	"github.com/crewlet/crewlet/internal/config"
)

// BrokerFloor is the least a broker must be able to reserve for a FRESH node of
// one Tier A document to create everything it reserves: every state log at
// the smallest ceiling sizing gives it, and the nats object bucket beside
// them.
//
// # Why it is computed and never written down
//
// The figure is a sum over [register] and over [derivedObjectsReservation],
// and both move: the artifact logs joined the register and raised it by two
// gibibytes, and nothing said so until a host that used to boot refused to. So
// the refusal a boot gives ([stateLog.storageRefused]), the advisory `crewlet
// validate` gives ([BrokerFloorFor]'s caller in cmd/crewlet) and the figures
// the deployment guide quotes are all read from this one computation, and the
// guide's are held to it by a test.
//
// # What it does not count
//
// Mailboxes, coordination buckets and a joining node's snapshot reserve
// nothing, so a broker grants them from whatever is left. The floor is the
// least that BOOTS, not the least that runs a company for long.
type BrokerFloor struct {
	// Logs is what the state logs reserve between them at their floors: an
	// explicit ceiling as written, and an unset one at the smaller of what
	// it asks for and where its scaling stops ([domainCeiling.scaleFloor]).
	Logs int64

	// Bucket is the nats object bucket's reservation at Limit, zero where
	// the objects live in S3 and no bucket is reserved.
	Bucket int64

	// BucketSet reports that store.objects.nats_max_bytes sets Bucket, rather
	// than the derivation from the broker's limit.
	BucketSet bool

	// Limit is the smallest broker limit that holds Logs and Bucket together.
	Limit int64
}

// embeddedShareNum and embeddedShareDen are the share of the stream volume's
// free space an embedded broker takes as its limit where stream.store_max_bytes
// is unset: three quarters, nats-server's own default sizing.
const (
	embeddedShareNum = 3
	embeddedShareDen = 4
)

// FreeAtDefault is the free space the stream volume needs for an embedded
// broker whose limit is derived (stream.store_max_bytes unset) to reach Limit:
// the smallest whole number of quarters of which three hold it.
func (f BrokerFloor) FreeAtDefault() int64 {
	return (f.Limit + embeddedShareNum - 1) / embeddedShareNum * embeddedShareDen
}

// Sentence is the floor in the words a refusal and an advisory share: what a
// fresh node reserves, split by what reserves it, and the Tier A settings that
// move each part.
func (f BrokerFloor) Sentence() string {
	s := fmt.Sprintf("a fresh node of this configuration reserves at least %d "+
		"bytes of the broker: %d for the state logs at their floors", f.Limit, f.Logs)
	switch {
	case f.Bucket == 0:
		return s
	case f.BucketSet:
		return s + fmt.Sprintf(" and %d for the nats object bucket, which "+
			"store.objects.nats_max_bytes sets", f.Bucket)
	}
	return s + fmt.Sprintf(" and %d for the nats object bucket, a quarter of the "+
		"broker's limit unless store.objects.nats_max_bytes sets it", f.Bucket)
}

// BrokerFloorFor is the [BrokerFloor] of boot.
//
// Pure: it reads Tier A and the register, never a disk or a broker, so a
// document can be judged on a host other than the one it will run on.
func BrokerFloorFor(boot *config.Bootstrap) BrokerFloor {
	var f BrokerFloor
	for _, entry := range register() {
		ceiling := entry.Ceiling(boot.Stream, 0)
		if ceiling.Explicit {
			f.Logs += ceiling.Bytes
			continue
		}
		f.Logs += min(ceiling.Bytes, ceiling.scaleFloor())
	}
	switch {
	case boot.Store.Objects.BackendOrDefault() != config.ObjectBackendNATS:
		f.Limit = f.Logs
	case boot.Store.Objects.NatsMaxBytes > 0:
		f.Bucket, f.BucketSet = boot.Store.Objects.NatsMaxBytes, true
		f.Limit = f.Logs + f.Bucket
	default:
		f.Limit = leastLimitHolding(f.Logs, boot.Stream.DeclaredLogCeilings())
		f.Bucket = derivedObjectsReservation(f.Limit, boot.Stream.DeclaredLogCeilings())
	}
	return f
}

// leastLimitHolding is the smallest broker limit whose derived bucket
// reservation leaves logs bytes for the state logs.
//
// SEARCHED rather than solved, because [derivedObjectsReservation] is three
// regimes (a quarter of the limit, what the logs' share and the coordination
// floor leave, and the field's own bounds), and an inverse written beside it
// would be a second statement of that arithmetic. What it leaves the logs
// never shrinks as the limit grows, which is all a bisection needs.
func leastLimitHolding(logs, declared int64) int64 {
	holds := func(limit int64) bool {
		return limit-derivedObjectsReservation(limit, declared) >= logs
	}
	lo, hi := logs, logs+config.NatsObjectsMaxBytesFloor
	for !holds(hi) {
		lo, hi = hi, hi*2
	}
	for lo < hi {
		mid := lo + (hi-lo)/2
		if holds(mid) {
			hi = mid
		} else {
			lo = mid + 1
		}
	}
	return hi
}

// BootstrapWarnings is every advisory about a Tier A document: its own
// ([config.Bootstrap.Warnings]) and the ones only the engine can judge.
//
// ONE LIST FOR BOTH READERS, `crewlet validate` and the advisories a node
// logs at every start, so an advisory said in one place is said in the other.
//
// # A broker limit set below the floor
//
// Judged only where it is exact: a node holding the estate (the one that
// creates the state logs) on its own embedded broker, whose limit an
// operator wrote down. A derived limit is three quarters of a volume this
// command may not be running beside, and an external broker's is an account
// it cannot read, so neither is guessed at; the boot's own refusal quotes the
// same floor ([stateLog.storageRefused]) where it is the disk that falls short.
func BootstrapWarnings(boot *config.Bootstrap) []config.Warning {
	out := boot.Warnings()
	limit := boot.Stream.StoreMaxBytes
	if !HoldsEstate(boot) || boot.Stream.Type != config.StreamEmbedded || limit <= 0 {
		return out
	}
	if floor := BrokerFloorFor(boot); limit < floor.Limit {
		out = append(out, config.AdvisoryAt("stream.store_max_bytes", fmt.Sprintf(
			"the embedded broker may reserve %d bytes, and %s, so a node starting "+
				"on an empty stream directory refuses to boot. Raise "+
				"stream.store_max_bytes to at least %d, or make the reservations "+
				"smaller", limit, floor.Sentence(), floor.Limit)))
	}
	return out
}
