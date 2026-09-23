package kv

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/crewlet/crewlet/internal/coord"
)

// ---- the bridged-call log ---------------------------------------------- //
//
// Two key classes in one ageless bucket, both built by [coord.DocumentKey]
// because a turn id and a launch id are values from elsewhere and a subject
// token cannot carry every byte they can:
//
//	call.<turn>.<launch>.<seq>  one call's record, created once and never
//	                            written again
//	next.<turn>.<launch>        the launch's numbering: the last Seq handed
//	                            out, advanced by compare-and-set
//
// The CLASS LEADS, so each is a wildcard the broker can match: a launch's
// calls are `call.<turn>.<launch>.>`, which the counter beside them — one
// segment shorter — never matches.

const (
	bridgeCallClass = "call"
	bridgeNextClass = "next"

	// kvStreamPrefix is what the NATS key-value protocol names a bucket's
	// stream: KV_<bucket>. A purge by subject is a STREAM operation, so it
	// needs the stream the bucket is.
	kvStreamPrefix = "KV_"
)

func bridgeCallKey(turnID, launchID string, seq uint64) string {
	return coord.DocumentKey(bridgeCallClass, turnID, launchID, strconv.FormatUint(seq, 10))
}

func bridgeNextKey(turnID, launchID string) string {
	return coord.DocumentKey(bridgeNextClass, turnID, launchID)
}

func bridgeCallFilter(turnID, launchID string) string {
	return coord.DocumentFilter(bridgeCallClass, turnID, launchID)
}

// bridgeCallSeq recovers a call key's Seq, reporting false for a key this
// backend did not write under that launch.
func bridgeCallSeq(key string) (uint64, bool) {
	segs, ok := coord.DocumentSegments(key)
	if !ok || len(segs) != 4 || segs[0] != bridgeCallClass {
		return 0, false
	}
	seq, err := strconv.ParseUint(segs[3], 10, 64)
	if err != nil || seq == 0 {
		return 0, false
	}
	return seq, true
}

// AppendBridgeCall files one call under its launch.
//
// THE NUMBER FIRST, THEN THE RECORD, and neither is a guess. The number is
// taken by a compare-and-set on the launch's counter, so two appends that land
// together get two numbers; the record is a Create, so a call is written once.
// A Create that finds its key taken is a record a purge missed — one a late
// append left after the counter it was numbered by was purged — and the next
// number is taken rather than the stray overwritten.
func (f *FleetStore) AppendBridgeCall(ctx context.Context, turnID, launchID string, value []byte) (uint64, error) {
	if err := validBridgeLaunch(turnID, launchID); err != nil {
		return 0, err
	}
	if len(value) > coord.MaxBridgeCallBytes {
		// REFUSED HERE rather than by the client, so the refusal names the
		// contract's ceiling instead of the connection's, and so the
		// memory twin and this backend refuse at the same length.
		return 0, fmt.Errorf("coord/kv: a bridged call of %d bytes is past the %d-byte "+
			"ceiling one record may hold (coord.MaxBridgeCallBytes): %w",
			len(value), coord.MaxBridgeCallBytes, coord.ErrTooLarge)
	}
	for range fleetCASRetries {
		seq, err := f.nextBridgeSeq(ctx, turnID, launchID)
		if err != nil {
			return 0, err
		}
		_, err = f.calls.Create(ctx, bridgeCallKey(turnID, launchID, seq), value)
		switch {
		case err == nil:
			return seq, nil
		case errors.Is(err, jetstream.ErrKeyExists):
			continue
		default:
			return 0, createRefusal(err)
		}
	}
	return 0, contended("append", turnID+"/"+launchID)
}

// createRefusal classifies a call record the broker did not take.
//
// THE CLIENT'S SIZE REFUSAL IS PERMANENT. It measures a message against the
// max_payload the server announced, and [OpenFleet] refuses a server that
// announced less than the contract's ceiling — but a client re-reads that
// announcement on every reconnect, so a cluster member configured below it is
// met at run time, not at boot. Answered as [coord.ErrUnavailable] it would
// read as a blip worth retrying; the same bytes are refused the same way every
// time, so it is [coord.ErrTooLarge], naming the setting that caused it.
func createRefusal(err error) error {
	if errors.Is(err, nats.ErrMaxPayload) {
		return fmt.Errorf("coord/kv: record the bridged call: the server this node is connected "+
			"to accepts less than the %d bytes a record may hold, so its max_payload is below "+
			"queue.MaxPayloadBytes: %w: %w", coord.MaxBridgeCallBytes, coord.ErrTooLarge, err)
	}
	return unavailable("record the bridged call", err)
}

// nextBridgeSeq takes the launch's next number.
func (f *FleetStore) nextBridgeSeq(ctx context.Context, turnID, launchID string) (uint64, error) {
	key := bridgeNextKey(turnID, launchID)
	for range fleetCASRetries {
		entry, err := f.calls.Get(ctx, key)
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			_, err := f.calls.Create(ctx, key, []byte("1"))
			switch {
			case err == nil:
				return 1, nil
			case errors.Is(err, jetstream.ErrKeyExists):
				continue
			default:
				return 0, unavailable("number the bridged call", err)
			}
		}
		if err != nil {
			return 0, unavailable("read the bridged-call numbering", err)
		}
		last, err := strconv.ParseUint(string(entry.Value()), 10, 64)
		if err != nil {
			return 0, unavailable("decode the bridged-call numbering", err)
		}
		next := last + 1
		_, err = f.calls.Update(ctx, key, []byte(strconv.FormatUint(next, 10)), entry.Revision())
		switch {
		case err == nil:
			return next, nil
		case errors.Is(err, jetstream.ErrKeyRevisionMismatch), errors.Is(err, jetstream.ErrKeyNotFound):
			// Another append took this number, or a purge took the
			// counter: re-read rather than guess.
			continue
		default:
			return 0, unavailable("number the bridged call", err)
		}
	}
	return 0, contended("number", turnID+"/"+launchID)
}

// BridgeCalls returns every call of one launch, in Seq order.
func (f *FleetStore) BridgeCalls(ctx context.Context, turnID, launchID string) ([]coord.BridgeCallRecord, error) {
	if err := validBridgeLaunch(turnID, launchID); err != nil {
		return nil, err
	}
	out := []coord.BridgeCallRecord{}
	err := f.eachUnder(ctx, f.calls, bridgeCallFilter(turnID, launchID), "the bridged calls",
		func(kve jetstream.KeyValueEntry) error {
			seq, ok := bridgeCallSeq(kve.Key())
			if !ok {
				return nil
			}
			out = append(out, coord.BridgeCallRecord{
				TurnID: turnID, LaunchID: launchID, Seq: seq,
				// COPIED: the entry's buffer is the client's.
				Value: slices.Clone(kve.Value()),
			})
			return nil
		})
	if err != nil {
		return nil, err
	}
	// The walk delivers in stream order, which is the order the records
	// were COMMITTED; two appends that took consecutive numbers can commit
	// the other way round.
	slices.SortFunc(out, func(a, b coord.BridgeCallRecord) int { return cmp.Compare(a.Seq, b.Seq) })
	return out, nil
}

// BridgeCallPage returns one page of one launch's calls.
//
// THE KEYS ARE LISTED AND ONLY THE PAGE IS READ. A page is a question about a
// window of the log, and the log's values can each be megabytes, so the
// listing leaves them at the broker and a Get fetches each call the page
// carries — which is what makes the byte budget bound what is READ as well
// as what is returned. A [coord.BridgeCallQuery.Last] page is the same walk
// taken from the newest end, which the listing makes as cheap as the oldest.
func (f *FleetStore) BridgeCallPage(ctx context.Context, q coord.BridgeCallQuery) (coord.BridgeCallPage, error) {
	if err := validBridgeQuery(q); err != nil {
		return coord.BridgeCallPage{}, err
	}
	var seqs []uint64
	err := eachKeyUnder(ctx, f.calls, bridgeCallFilter(q.TurnID, q.LaunchID), "the bridged calls",
		func(key string) error {
			if seq, ok := bridgeCallSeq(key); ok {
				seqs = append(seqs, seq)
			}
			return nil
		})
	if err != nil {
		return coord.BridgeCallPage{}, err
	}
	slices.Sort(seqs)
	page := coord.BridgeCallPage{Calls: []coord.BridgeCallRecord{}, Total: len(seqs)}
	// What the cursor admits, in the order the page takes it.
	admitted := seqs[sort.Search(len(seqs), func(i int) bool { return seqs[i] > q.After }):]
	if q.Last {
		admitted = slices.Clone(admitted)
		slices.Reverse(admitted)
	}
	weight := 0
	for _, seq := range admitted {
		if len(page.Calls) == q.Limit || (len(page.Calls) > 0 && weight > q.MaxBytes) {
			// Full, or a first call that alone outweighed the budget:
			// what is left is more, without spending a read to say so.
			page.More = true
			break
		}
		entry, err := f.calls.Get(ctx, bridgeCallKey(q.TurnID, q.LaunchID, seq))
		if errors.Is(err, jetstream.ErrKeyNotFound) {
			// Purged between the listing and the read: the run ended,
			// and a call of an ended run is not one to show.
			continue
		}
		if err != nil {
			return coord.BridgeCallPage{}, unavailable("read a bridged call", err)
		}
		if len(page.Calls) > 0 && weight+len(entry.Value()) > q.MaxBytes {
			page.More = true
			break
		}
		weight += len(entry.Value())
		page.Calls = append(page.Calls, coord.BridgeCallRecord{
			TurnID: q.TurnID, LaunchID: q.LaunchID, Seq: seq, Value: slices.Clone(entry.Value()),
		})
	}
	if q.Last {
		// Taken newest first; handed back in the order the run made them.
		slices.Reverse(page.Calls)
	}
	return page, nil
}

// BridgeLaunches returns every launch that holds any key in the log.
//
// BOTH CLASSES, because either can outlive the other: a launch whose counter a
// purge took can still hold a record a late append left, and one whose every
// record-create failed holds only its counter. A sweep that listed one class
// would never find the other's strays.
func (f *FleetStore) BridgeLaunches(ctx context.Context) ([]coord.BridgeLaunch, error) {
	seen := map[coord.BridgeLaunch]struct{}{}
	err := eachKeyUnder(ctx, f.calls, jetstream.AllKeys, "the bridged-call launches",
		func(key string) error {
			segs, ok := coord.DocumentSegments(key)
			if !ok || len(segs) < 3 {
				return nil
			}
			switch {
			case segs[0] == bridgeCallClass && len(segs) == 4,
				segs[0] == bridgeNextClass && len(segs) == 3:
				seen[coord.BridgeLaunch{TurnID: segs[1], LaunchID: segs[2]}] = struct{}{}
			}
			return nil
		})
	if err != nil {
		return nil, err
	}
	out := slices.Collect(maps.Keys(seen))
	slices.SortFunc(out, func(a, b coord.BridgeLaunch) int {
		return cmp.Or(cmp.Compare(a.TurnID, b.TurnID), cmp.Compare(a.LaunchID, b.LaunchID))
	})
	return out, nil
}

// PurgeBridgeCalls removes every record of one launch, and its numbering.
//
// A STREAM PURGE BY SUBJECT rather than a key purge per record. A key purge
// leaves a marker message behind for every key, and in a bucket with no age
// each marker is kept for the life of the deployment — one per call, for every
// run the company ever made. A purge by subject removes the messages and
// leaves nothing.
//
// RECORDS FIRST, THEN THE COUNTER. A failure between the two leaves the
// counter, which the sweep lists and purges again; the other order would leave
// records with nothing numbering them, which the sweep still finds (see
// [FleetStore.BridgeLaunches]) but a reader of the counter alone would not.
func (f *FleetStore) PurgeBridgeCalls(ctx context.Context, turnID, launchID string) error {
	if err := validBridgeLaunch(turnID, launchID); err != nil {
		return err
	}
	stream, err := f.js.Stream(ctx, kvStreamPrefix+f.calls.Bucket())
	if err != nil {
		return unavailable("find the bridged-call stream", err)
	}
	subjects := stream.CachedInfo().Config.Subjects
	if len(subjects) != 1 || !strings.HasSuffix(subjects[0], ">") {
		return fmt.Errorf("coord/kv: the bridged-call stream carries subjects %q, not the one "+
			"wildcard a key-value bucket's stream has", subjects)
	}
	prefix := strings.TrimSuffix(subjects[0], ">")
	for _, key := range []string{bridgeCallFilter(turnID, launchID), bridgeNextKey(turnID, launchID)} {
		if err := stream.Purge(ctx, jetstream.WithPurgeSubject(prefix+key)); err != nil {
			return unavailable("purge the bridged calls", err)
		}
	}
	return nil
}

func validBridgeLaunch(turnID, launchID string) error {
	if turnID == "" || launchID == "" {
		return errors.New("coord/kv: a bridged call needs a turn id and a launch id")
	}
	return nil
}

func validBridgeQuery(q coord.BridgeCallQuery) error {
	if err := validBridgeLaunch(q.TurnID, q.LaunchID); err != nil {
		return err
	}
	if q.Limit < 1 || q.MaxBytes < 1 {
		return fmt.Errorf("coord/kv: a page of bridged calls needs a limit and a byte "+
			"budget of at least one, got %d and %d", q.Limit, q.MaxBytes)
	}
	return nil
}
